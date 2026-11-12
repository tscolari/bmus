package bridge

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dbos-inc/dbos-transact-golang/dbos"
	"github.com/stretchr/testify/require"

	"github.com/tscolari/bmus/internal/comms"
	"github.com/tscolari/bmus/internal/store"
	"github.com/tscolari/bmus/internal/transporter"
)

// fakeProvider is a transporter.Provider whose attaches give sessions that
// send a hello, as serve does on every attach (seq since+1, so each attach's
// hello is new to the pump and reaches the tour, #173), then stay open,
// silent, until their ctx ends. A session that has sent its hello counts as
// connected, so Send works (commands are discarded). Only a beam this
// provider started combadge on says hello: it knows its crew member. Create mints fake-<n> aliases,
// StartCombadge records the beam (waiting for startGate, if set, to close),
// Remove records the beam (failing with removeErr, if set), every beam
// probes healthy and exists unless listed in gone. Create fails while
// createFails is above 0, counting it down (#97). With blockProvision,
// Create, Exists and StartCombadge block until their ctx ends, so no tour
// gets past provisioning. With hello, every
// session also sends one heartbeat after its hello. WorkDir is /beams/<alias>/work,
// and CopyTo copies into copyRoot (that path below it), waiting for
// copyGate, if set, to close; without copyRoot it only records the copy.
// With copyFailOnce the first CopyTo fails instead. CopyFrom copies from
// copyRoot (the remote path below it), waiting for copyFromGate, if set, to
// close. Exec isn't implemented. With loginExpired set, LoginValidUntil
// reports a login that expired a minute ago, and Create, Exists,
// StartCombadge, AttachCombadge, Probe, CopyTo, CopyFrom and Size fail with
// transporter.ErrLoginExpired, as Tsh's login gate does (#102); otherwise
// the login never expires. With expireOnCopy set, the next CopyTo expires
// the login (and clears expireOnCopy) before it checks it.
type fakeProvider struct {
	transporter.Provider
	loginExpired   atomic.Bool
	expireOnCopy   atomic.Bool
	blockProvision bool
	hello          bool
	startGate      chan struct{}
	gone           map[string]bool
	copyRoot       string
	copyGate       chan struct{}
	// copyFailOnce, if set, makes the first CopyTo fail and then closes it.
	copyFailOnce chan struct{}
	copyFromGate chan struct{}
	// sizeLie, if set, is what Size reports for every file, as a beam
	// would that lies or whose file grows after the check (#65).
	sizeLie int64
	// removeErr, if set, is what Remove returns.
	removeErr error
	// lifetime, if set, is how far out Create puts a beam's ExpiresAt
	// (24h otherwise).
	lifetime time.Duration

	mu         sync.Mutex
	attaches   []string          // "<alias> since=<n>"
	crewOf     map[string]string // beam alias -> crew id, from StartCombadge
	created    int
	starts     []string                   // beams StartCombadge was called on
	specs      []transporter.CombadgeSpec // their specs, in the same order
	copies     []string                   // "<alias>:<remote path>" of each CopyTo done
	copyFailed bool

	createFails int
	createCalls int      // Create calls, failed ones included
	removes     []string // beams Remove was called on
}

func (f *fakeProvider) WorkDir(beam transporter.Beam) (string, error) {
	return "/beams/" + beam.Alias + "/work", nil
}

func (f *fakeProvider) CopyTo(ctx context.Context, beam transporter.Beam, localPath, remotePath string) error {
	if f.expireOnCopy.CompareAndSwap(true, false) {
		f.loginExpired.Store(true)
	}
	if f.loginExpired.Load() {
		return fmt.Errorf("tsh beams scp: %w", transporter.ErrLoginExpired)
	}
	if f.copyFailOnce != nil {
		f.mu.Lock()
		first := !f.copyFailed
		f.copyFailed = true
		f.mu.Unlock()
		if first {
			close(f.copyFailOnce)
			return errors.New("fake: copy failed")
		}
	}
	if f.copyGate != nil {
		select {
		case <-f.copyGate:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if f.copyRoot != "" {
		data, err := os.ReadFile(localPath)
		if err != nil {
			return err
		}
		dst := filepath.Join(f.copyRoot, remotePath)
		if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
			return err
		}
		if err := os.WriteFile(dst, data, 0o600); err != nil {
			return err
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.copies = append(f.copies, beam.Alias+":"+remotePath)
	return nil
}

func (f *fakeProvider) CopyFrom(ctx context.Context, beam transporter.Beam, remotePath, localPath string) error {
	if f.loginExpired.Load() {
		return fmt.Errorf("tsh beams scp: %w", transporter.ErrLoginExpired)
	}
	if f.copyFromGate != nil {
		select {
		case <-f.copyFromGate:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	data, err := os.ReadFile(filepath.Join(f.copyRoot, remotePath))
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(localPath), 0o700); err != nil {
		return err
	}
	return os.WriteFile(localPath, data, 0o600)
}

func (f *fakeProvider) Size(ctx context.Context, beam transporter.Beam, path string) (int64, error) {
	if f.loginExpired.Load() {
		return 0, fmt.Errorf("tsh beams exec: %w", transporter.ErrLoginExpired)
	}
	if f.sizeLie > 0 {
		return f.sizeLie, nil
	}
	st, err := os.Stat(filepath.Join(f.copyRoot, path))
	if err != nil {
		return 0, err
	}
	return st.Size(), nil
}

func (f *fakeProvider) copied() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.copies...)
}

func (f *fakeProvider) Create(ctx context.Context) (transporter.Beam, error) {
	if f.loginExpired.Load() {
		return transporter.Beam{}, fmt.Errorf("tsh beams add: %w", transporter.ErrLoginExpired)
	}
	if f.blockProvision {
		<-ctx.Done()
		return transporter.Beam{}, ctx.Err()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.createCalls++
	if f.createFails > 0 {
		f.createFails--
		return transporter.Beam{}, errors.New("fake: beams add failed")
	}
	f.created++
	return transporter.Beam{Alias: fmt.Sprintf("fake-%d", f.created), ExpiresAt: time.Now().Add(cmp.Or(f.lifetime, 24*time.Hour))}, nil
}

func (f *fakeProvider) Exists(ctx context.Context, beam transporter.Beam) (bool, error) {
	if f.loginExpired.Load() {
		return false, fmt.Errorf("tsh beams ls: %w", transporter.ErrLoginExpired)
	}
	if f.blockProvision {
		<-ctx.Done()
		return false, ctx.Err()
	}
	return !f.gone[beam.Alias], nil
}

func (f *fakeProvider) StartCombadge(ctx context.Context, beam transporter.Beam, spec transporter.CombadgeSpec) error {
	if f.loginExpired.Load() {
		return fmt.Errorf("tsh beams exec: %w", transporter.ErrLoginExpired)
	}
	f.mu.Lock()
	f.starts = append(f.starts, beam.Alias)
	f.specs = append(f.specs, spec)
	if f.crewOf == nil {
		f.crewOf = map[string]string{}
	}
	f.crewOf[beam.Alias] = spec.CrewID
	f.mu.Unlock()
	if f.blockProvision {
		<-ctx.Done()
		return ctx.Err()
	}
	if f.startGate != nil {
		select {
		case <-f.startGate:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

func (f *fakeProvider) Remove(ctx context.Context, beam transporter.Beam) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removes = append(f.removes, beam.Alias)
	return f.removeErr
}

func (f *fakeProvider) removed() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.removes...)
}

func (f *fakeProvider) createCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.createCalls
}

func (f *fakeProvider) started() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.starts...)
}

func (f *fakeProvider) startSpecs() []transporter.CombadgeSpec {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]transporter.CombadgeSpec(nil), f.specs...)
}

func (f *fakeProvider) createdCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.created
}

func (f *fakeProvider) AttachCombadge(ctx context.Context, beam transporter.Beam, since uint64) (transporter.Session, error) {
	if f.loginExpired.Load() {
		return nil, fmt.Errorf("tsh beams exec: %w", transporter.ErrLoginExpired)
	}
	f.mu.Lock()
	f.attaches = append(f.attaches, fmt.Sprintf("%s since=%d", beam.Alias, since))
	crew := f.crewOf[beam.Alias]
	f.mu.Unlock()
	pr, pw := io.Pipe()
	done := make(chan struct{})
	context.AfterFunc(ctx, func() { pw.CloseWithError(ctx.Err()); close(done) })
	var msgs []comms.Message
	if crew != "" {
		m, err := comms.NewMessage(comms.TypeHello, comms.HelloBody{Protocol: comms.Version, Harness: "fake", CombadgeVersion: "test"})
		if err != nil {
			return nil, err
		}
		msgs = append(msgs, m)
	}
	if f.hello {
		m, err := comms.NewMessage(comms.TypeHeartbeat, comms.HeartbeatBody{})
		if err != nil {
			return nil, err
		}
		msgs = append(msgs, m)
	}
	var out []byte
	for i, m := range msgs {
		m.Crew, m.Seq = crew, since+uint64(i)+1
		line, err := json.Marshal(m)
		if err != nil {
			return nil, err
		}
		out = append(append(out, line...), '\n')
	}
	// Unblocked by the reader or by the pipe closing with ctx.
	go pw.Write(out)
	return &silentSession{pr: pr, done: done}, nil
}

func (f *fakeProvider) Probe(ctx context.Context, beam transporter.Beam) (bool, error) {
	if f.loginExpired.Load() {
		return false, fmt.Errorf("tsh beams ls: %w", transporter.ErrLoginExpired)
	}
	return true, nil
}

func (f *fakeProvider) LoginValidUntil(context.Context) (time.Time, error) {
	if f.loginExpired.Load() {
		return time.Now().Add(-time.Minute), nil
	}
	return time.Time{}, nil
}

func (f *fakeProvider) attached() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.attaches...)
}

type silentSession struct {
	pr   *io.PipeReader
	done chan struct{}
}

func (s *silentSession) Write(p []byte) (int, error) { return len(p), nil }
func (s *silentSession) Close() error                { return nil }
func (s *silentSession) Stdout() io.Reader           { return s.pr }
func (s *silentSession) Wait() error                 { <-s.done; return nil }

// startDaemonWith runs a daemon with provider p and a registry of one repo,
// app, a fresh testRepo.
func startDaemonWith(t *testing.T, home string, p transporter.Provider) running {
	t.Helper()
	return startDaemonRepos(t, home, p, map[string]string{"app": testRepo(t).url})
}

func startDaemonRepos(t *testing.T, home string, p transporter.Provider, repos map[string]string) running {
	t.Helper()
	cfg := testConfig(home)
	cfg.Transporter = p
	cfg.Repos = repos
	return startDaemonConfig(t, cfg)
}

// startDaemonConfig runs the daemon with cfg in a goroutine and waits until
// it is up.
func startDaemonConfig(t *testing.T, cfg Config) running {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	ready := make(chan *Daemon, 1)
	cfg.onReady = func(d *Daemon) { ready <- d }
	done := make(chan error, 1)
	go func() { done <- Run(ctx, cfg) }()
	select {
	case d := <-ready:
		return running{d: d, cancel: cancel, done: done}
	case err := <-done:
		cancel()
		t.Fatalf("Run returned before ready: %v", err)
	case <-time.After(20 * time.Second):
		cancel()
		t.Fatal("daemon not ready")
	}
	return running{}
}

// After a restart the tour's provisioning step doesn't run again, so the
// daemon itself starts a pump for every crew member with a live beam.
func TestDaemonStartsPumpsForLiveCrew(t *testing.T) {
	t.Parallel()
	home := privateHome(t)
	for _, c := range []store.Crew{
		{ID: "away", Name: "A", Status: store.CrewAway, CurrentBeam: "beam-a", TourNo: 2},
		{ID: "deck", Name: "D", Status: store.CrewOnDeck, CurrentBeam: "beam-d"},
		{ID: "hail", Name: "H", Status: store.CrewHailing, CurrentBeam: "beam-h"},
		{ID: "new", Name: "N", Status: store.CrewOnDeck}, // not provisioned yet
		{ID: "sick", Name: "S", Status: store.CrewSickbay, CurrentBeam: "beam-s"},
		{ID: "gone", Name: "G", Status: store.CrewDismissed, CurrentBeam: "beam-g"},
	} {
		insertCrew(t, home, c)
	}
	// Some inbox history: the pump must resume after its own tour's, not
	// an earlier tour's (#22).
	st, err := store.Open(t.Context(), home+"/"+store.DBFile)
	require.NoError(t, err)
	for _, seq := range []uint64{1, 2, 3, 5} {
		_, err := st.RecordInbox(t.Context(), "away", 2, seq, nil)
		require.NoError(t, err)
	}
	for _, seq := range []uint64{1, 2, 3, 4, 5, 6, 7} {
		_, err := st.RecordInbox(t.Context(), "away", 1, seq, nil)
		require.NoError(t, err)
	}
	require.NoError(t, st.Close())

	// Every crew member that isn't dismissed gets a tour, and a tour that
	// got past provisioning would call EnsurePump itself (and re-add a pump
	// StopPump removed below). Here provisioning never finishes, so only the
	// daemon's own ensurePumps attaches.
	p := &fakeProvider{blockProvision: true}
	r := startDaemonWith(t, home, p)
	defer r.stop(t)

	require.Eventually(t, func() bool { return len(p.attached()) == 3 }, 10*time.Second, 10*time.Millisecond)
	require.ElementsMatch(t, []string{"beam-a since=3", "beam-d since=0", "beam-h since=0"}, p.attached())
	for _, id := range []string{"away", "deck", "hail"} {
		_, ok := r.d.Pump(id)
		require.True(t, ok, id)
	}
	for _, id := range []string{"new", "sick", "gone"} {
		_, ok := r.d.Pump(id)
		require.False(t, ok, id)
	}

	// EnsurePump is idempotent for the same beam and tour, and replaces a
	// pump for another.
	a, _ := r.d.Pump("away")
	same, err := r.d.EnsurePump("away", transporter.Beam{Alias: "beam-a"}, 2)
	require.NoError(t, err)
	require.Same(t, a, same)
	other, err := r.d.EnsurePump("away", transporter.Beam{Alias: "beam-a2"}, 3)
	require.NoError(t, err)
	require.NotSame(t, a, other)
	<-a.Done()

	// SendCommand: no session yet (the fake never sends a line), or no pump.
	_, err = r.d.SendCommand("away", comms.Message{Type: comms.TypeBelay})
	require.ErrorIs(t, err, comms.ErrNotConnected)
	r.d.StopPump("away")
	<-other.Done()
	_, ok := r.d.Pump("away")
	require.False(t, ok)
	_, err = r.d.SendCommand("away", comms.Message{Type: comms.TypeBelay})
	require.ErrorIs(t, err, comms.ErrNotConnected)
}

// The crash-safety mechanism: a DBOS send made with WithSendTransaction on
// the store's inbox transaction commits or rolls back with the inbox row,
// though the store and DBOS use separate connection pools on bridge.db.
func TestPumpSinkSendIsAtomicWithInbox(t *testing.T) {
	t.Parallel()
	home := privateHome(t)
	insertCrew(t, home, store.Crew{ID: "sorell", Name: "Sorell"})
	r := startDaemon(t, home)
	defer r.stop(t)
	tourID := tourWorkflowID("sorell", 0)
	requirePending(t, r.d, tourID)
	ctx := t.Context()
	sink := pumpSink{d: r.d, crewID: "sorell", tourNo: 0}
	// Seqs above the fake beam's own (its hello, #173).

	// Rolled back: neither the inbox row nor the message survive.
	boom := errors.New("crash before commit")
	_, err := r.d.store.RecordInbox(ctx, "sorell", 0, 101, func(tx store.InboxTx) error {
		require.NoError(t, dbos.Send(r.d.dbos, tourID, CrewSignal{Kind: SignalDismissed}, crewEventsTopic,
			dbos.WithSendTransaction(tx.SQL())))
		return boom
	})
	require.ErrorIs(t, err, boom)
	seen, err := r.d.store.SeenInbox(ctx, "sorell", 0, 101)
	require.NoError(t, err)
	require.False(t, seen)
	time.Sleep(2 * time.Second) // well past DBOS's notification polling
	require.Equal(t, dbos.WorkflowStatusPending, workflows(t, r.d, tourID)[0].Status, "rolled-back signal was delivered")

	// A crew-level error through the sink is delivered once committed.
	msg, err := comms.NewMessage(comms.TypeError, comms.ErrorBody{Reason: "disk full", Code: "enospc"})
	require.NoError(t, err)
	msg.Seq, msg.Crew = 101, "sorell"
	recorded, err := r.d.store.RecordInbox(ctx, "sorell", 0, 101, func(tx store.InboxTx) error {
		return sink.CrewError(ctx, tx.SQL(), msg)
	})
	require.NoError(t, err)
	require.True(t, recorded)
	require.Eventually(t, func() bool {
		steps, err := dbos.GetWorkflowSteps(r.d.dbos, tourID)
		return err == nil && containsCrewError(steps)
	}, 10*time.Second, 50*time.Millisecond, "crew_error never reached the tour")

	// A mission that doesn't exist can't be sent to: the inbox row rolls
	// back and the pump would retry the message later.
	hail, err := comms.NewMessage(comms.TypeHail, comms.HailBody{Ref: "r", Category: comms.CategoryClarifyingQuestion, Question: "?"})
	require.NoError(t, err)
	hail.Seq, hail.Crew, hail.Order = 102, "sorell", "ORD-9999"
	_, err = r.d.store.RecordInbox(ctx, "sorell", 0, 102, func(tx store.InboxTx) error {
		return sink.ToMission(ctx, tx.SQL(), hail)
	})
	require.ErrorIs(t, err, comms.ErrUndeliverable, "a missing mission is reported as undeliverable")
	seen, err = r.d.store.SeenInbox(ctx, "sorell", 0, 102)
	require.NoError(t, err)
	require.False(t, seen)

	// Committed: the dismissed signal arrives and ends the tour.
	_, err = r.d.store.RecordInbox(ctx, "sorell", 0, 102, func(tx store.InboxTx) error {
		return dbos.Send(r.d.dbos, tourID, CrewSignal{Kind: SignalDismissed}, crewEventsTopic,
			dbos.WithSendTransaction(tx.SQL()))
	})
	require.NoError(t, err)
	h, err := dbos.RetrieveWorkflow[CrewTourResult](r.d.dbos, tourID)
	require.NoError(t, err)
	res, err := h.GetResult(dbos.WithHandleTimeout(10 * time.Second))
	require.NoError(t, err)
	require.Equal(t, SignalDismissed, res.Ended)

	// #160: the tour has ended. A crew error to it is gone (the pump logs it
	// undeliverable) and its inbox row rolls back; a hello is dropped with
	// its inbox row recorded and nothing logged.
	msg.Seq = 103
	_, err = r.d.store.RecordInbox(ctx, "sorell", 0, 103, func(tx store.InboxTx) error {
		return sink.CrewError(ctx, tx.SQL(), msg)
	})
	require.ErrorIs(t, err, comms.ErrGone)
	seen, err = r.d.store.SeenInbox(ctx, "sorell", 0, 103)
	require.NoError(t, err)
	require.False(t, seen)
	hello, err := comms.NewMessage(comms.TypeHello, comms.HelloBody{Protocol: comms.Version, Harness: "fake", CombadgeVersion: "test"})
	require.NoError(t, err)
	hello.Seq, hello.Crew = 104, "sorell"
	_, err = r.d.store.RecordInbox(ctx, "sorell", 0, 104, func(tx store.InboxTx) error {
		return sink.CrewHello(ctx, tx.SQL(), hello)
	})
	require.NoError(t, err)
	seen, err = r.d.store.SeenInbox(ctx, "sorell", 0, 104)
	require.NoError(t, err)
	require.True(t, seen)
	entries, err := r.d.store.ListLog(ctx, 0)
	require.NoError(t, err)
	for _, e := range entries {
		require.NotEqual(t, comms.UndeliverableLogKind, e.Kind, "hello logged: %+v", e)
	}

	// heartbeat_missed is sent at most once per tour, even by a new pump.
	// (Tour 0 dismissed sorell, for good: another crew member's tour 1.)
	require.NoError(t, r.d.store.InsertCrew(ctx, store.Crew{ID: "tuvok", Name: "Tuvok"}, testEntry))
	tour1 := tourWorkflowID("tuvok", 1)
	_, err = dbos.RunWorkflow(r.d.dbos, crewTourWorkflow, CrewTourInput{CrewID: "tuvok", TourNo: 1}, dbos.WithWorkflowID(tour1))
	require.NoError(t, err)
	sink1 := pumpSink{d: r.d, crewID: "tuvok", tourNo: 1}
	require.NoError(t, sink1.HeartbeatMissed(ctx))
	require.NoError(t, sink1.HeartbeatMissed(ctx))
	require.NoError(t, dbos.Send(r.d.dbos, tour1, CrewSignal{Kind: SignalDismissed}, crewEventsTopic))
	h, err = dbos.RetrieveWorkflow[CrewTourResult](r.d.dbos, tour1)
	require.NoError(t, err)
	_, err = h.GetResult(dbos.WithHandleTimeout(10 * time.Second))
	require.NoError(t, err)
	steps, err := dbos.GetWorkflowSteps(r.d.dbos, tour1)
	require.NoError(t, err)
	require.Equal(t, 1, countSignal(steps, SignalHeartbeatMissed), "%+v", steps)
}

func containsCrewError(steps []dbos.StepInfo) bool { return countSignal(steps, SignalCrewError) > 0 }

// countSignal counts the tour's recorded Recv steps that got a signal of
// kind. GetWorkflowSteps returns a step's output as its serialized JSON.
func countSignal(steps []dbos.StepInfo, kind string) int {
	n := 0
	for _, s := range steps {
		out, ok := s.Output.(string)
		if !ok || s.StepName != "DBOS.recv" {
			continue
		}
		var sig CrewSignal
		if json.Unmarshal([]byte(out), &sig) == nil && sig.Kind == kind {
			n++
		}
	}
	return n
}

// pumpSink.ToHail delivers a consult_reply to a running hail/<id> workflow on
// consultReplyTopic, atomically with the inbox row, and reports a missing or
// finished one as undeliverable (#151). A reply from a crew member outside
// the command division, or unknown, is dropped at once with its inbox row
// kept (#150): it can't use up the workflow's one read. The workflow is a stand-in for #149's.
func TestPumpSinkToHail(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	recv := func(ctx dbos.Context, _ string) (comms.Message, error) {
		return dbos.Recv[comms.Message](ctx, consultReplyTopic, 20*time.Second)
	}
	st, dctx := newSinkDBOS(t, func(dctx dbos.Context) {
		dbos.RegisterWorkflow(dctx, recv, dbos.WithWorkflowName("consult-reply-recv"))
	})
	e := store.LogEntry{Kind: "test"}
	require.NoError(t, st.InsertCrew(ctx, store.Crew{ID: "command", Name: "C", Division: DivisionCommand}, e))
	require.NoError(t, st.InsertCrew(ctx, store.Crew{ID: "sorell", Name: "S", Division: "ops"}, e))

	replyFrom := func(crew string, seq uint64, hailID string) comms.Message {
		m, err := comms.NewMessage(comms.TypeConsultReply, comms.ConsultReplyBody{HailID: hailID, Decision: "answer", Confidence: 0.9})
		require.NoError(t, err)
		m.Seq, m.Crew = seq, crew
		return m
	}
	reply := func(seq uint64, hailID string) comms.Message { return replyFrom("command", seq, hailID) }
	toHail := func(m comms.Message) error {
		var b comms.ConsultReplyBody
		require.NoError(t, json.Unmarshal(m.Body, &b))
		sink := pumpSink{d: &Daemon{dbos: dctx, logger: slog.New(slog.DiscardHandler)}, crewID: m.Crew}
		_, err := st.RecordInbox(ctx, m.Crew, 0, m.Seq, func(tx store.InboxTx) error {
			return sink.ToHail(ctx, tx.SQL(), m, b.HailID)
		})
		return err
	}
	seenBy := func(crew string, seq uint64) bool {
		ok, err := st.SeenInbox(ctx, crew, 0, seq)
		require.NoError(t, err)
		return ok
	}
	seen := func(seq uint64) bool { return seenBy("command", seq) }

	// No such workflow: undeliverable, and the inbox row rolls back.
	require.ErrorIs(t, toHail(reply(1, "H-0091")), comms.ErrUndeliverable)
	require.False(t, seen(1))

	h, err := dbos.RunWorkflow(dctx, recv, "", dbos.WithWorkflowID(hailWorkflowID("H-0091")))
	require.NoError(t, err)
	// Not Command, or nobody: dropped, recorded, and the workflow still
	// waits.
	require.NoError(t, toHail(replyFrom("sorell", 1, "H-0091")))
	require.True(t, seenBy("sorell", 1))
	require.NoError(t, toHail(replyFrom("ghost", 1, "H-0091")))
	require.True(t, seenBy("ghost", 1))
	require.NoError(t, toHail(reply(1, "H-0091")))
	require.True(t, seen(1))
	got, err := h.GetResult(dbos.WithHandleTimeout(20 * time.Second))
	require.NoError(t, err)
	require.Equal(t, comms.TypeConsultReply, got.Type)
	require.Equal(t, uint64(1), got.Seq)

	// Finished: gone, for good (#160).
	require.ErrorIs(t, toHail(reply(2, "H-0091")), comms.ErrGone)
	require.False(t, seen(2))
}

// #160: a report to a mission whose workflow has finished (failed by the
// timeout, belayed) is logged undeliverable at once, with the report as
// payload, instead of being acked into a notification nobody reads or held
// for UndeliverableAfter; the next message on the stream reaches its live
// (PENDING) mission.
func TestPumpSkipsFinishedMission(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	done := func(dbos.Context, string) (string, error) { return "failed", nil }
	recv := func(ctx dbos.Context, _ string) (comms.Message, error) {
		return dbos.Recv[comms.Message](ctx, fromCrewTopic, 20*time.Second)
	}
	st, dctx := newSinkDBOS(t, func(dctx dbos.Context) {
		dbos.RegisterWorkflow(dctx, done, dbos.WithWorkflowName("finished-mission"))
		dbos.RegisterWorkflow(dctx, recv, dbos.WithWorkflowName("live-mission"))
	})
	var err error

	finished, err := dbos.RunWorkflow(dctx, done, "", dbos.WithWorkflowID(missionWorkflowID("ORD-0001")))
	require.NoError(t, err)
	_, err = finished.GetResult(dbos.WithHandleTimeout(10 * time.Second))
	require.NoError(t, err)
	live, err := dbos.RunWorkflow(dctx, recv, "", dbos.WithWorkflowID(missionWorkflowID("ORD-0002")))
	require.NoError(t, err)

	link := &scriptLink{sessions: make(chan *scriptSession, 4)}
	p, err := comms.StartPump(ctx, comms.PumpConfig{
		CrewID: "sorell", Beam: "beam-s", Link: link, Store: st,
		Sink:               pumpSink{d: &Daemon{dbos: dctx, logger: slog.New(slog.DiscardHandler)}, crewID: "sorell"},
		Logger:             slog.New(slog.DiscardHandler),
		UndeliverableAfter: time.Hour, // a retry would outlast the test
	})
	require.NoError(t, err)
	defer p.Stop()

	report := func(seq uint64, order string) []byte {
		m, err := comms.NewMessage(comms.TypeReport, comms.ReportBody{Summary: "late for " + order})
		require.NoError(t, err)
		m.Seq, m.Crew, m.Order = seq, "sorell", order
		line, err := json.Marshal(m)
		require.NoError(t, err)
		return append(line, '\n')
	}
	var sess *scriptSession
	select {
	case sess = <-link.sessions:
	case <-time.After(10 * time.Second):
		t.Fatal("pump never attached")
	}
	_, err = sess.pw.Write(append(report(1, "ORD-0001"), report(2, "ORD-0002")...))
	require.NoError(t, err)

	got, err := live.GetResult(dbos.WithHandleTimeout(20 * time.Second))
	require.NoError(t, err)
	require.Equal(t, uint64(2), got.Seq, "the next message is delivered")
	require.Eventually(t, func() bool { return p.Acked() == 2 }, 10*time.Second, 10*time.Millisecond)
	require.Equal(t, int32(1), link.attaches.Load(), "skipped without a reconnect")

	entries, err := st.ListLog(ctx, 0)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.Equal(t, comms.UndeliverableLogKind, entries[0].Kind)
	require.Equal(t, "ORD-0001", entries[0].OrderID)
	require.Contains(t, entries[0].Text, "comms: message destination has finished: order/ORD-0001")
	require.Equal(t, 1, strings.Count(entries[0].Text, "has finished"), entries[0].Text)
	var logged comms.Message
	require.NoError(t, json.Unmarshal(entries[0].Payload, &logged))
	require.Equal(t, comms.TypeReport, logged.Type)
	require.Equal(t, uint64(1), logged.Seq)
	var body comms.ReportBody
	require.NoError(t, json.Unmarshal(logged.Body, &body))
	require.Equal(t, "late for ORD-0001", body.Summary)
}

// newSinkDBOS opens a store and a launched DBOS on one fresh bridge.db, as
// the daemon shares it, for pumpSink tests that need no daemon. register
// registers the test's workflows before Launch. Both close with the test.
func newSinkDBOS(t *testing.T, register func(dbos.Context)) (*store.Store, dbos.Context) {
	t.Helper()
	path := filepath.Join(t.TempDir(), store.DBFile)
	st, err := store.Open(t.Context(), path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })
	uri, err := store.FileURI(path)
	require.NoError(t, err)
	dctx, err := dbos.NewContext(context.Background(), dbos.Config{
		AppName:            "sink-test",
		DatabaseURL:        "sqlite:" + uri,
		ApplicationVersion: "test",
		Logger:             slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	require.NoError(t, err)
	t.Cleanup(func() { dbos.Shutdown(dctx, 10*time.Second) })
	register(dctx)
	require.NoError(t, dbos.Launch(dctx))
	return st, dctx
}

// scriptLink is a comms.Link whose sessions the test writes beam lines to
// (scriptSession.pw); what the pump writes (acks) is discarded.
type scriptLink struct {
	sessions chan *scriptSession
	attaches atomic.Int32
}

func (l *scriptLink) Attach(ctx context.Context, since uint64) (comms.Session, error) {
	l.attaches.Add(1)
	pr, pw := io.Pipe()
	s := &scriptSession{silentSession: silentSession{pr: pr, done: make(chan struct{})}, pw: pw}
	context.AfterFunc(ctx, func() { pw.CloseWithError(ctx.Err()); close(s.done) })
	select {
	case l.sessions <- s:
		return s, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (l *scriptLink) Probe(context.Context) (bool, error) { return true, nil }

type scriptSession struct {
	silentSession
	pw *io.PipeWriter
}
