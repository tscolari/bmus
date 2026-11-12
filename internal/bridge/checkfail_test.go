package bridge

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dbos-inc/dbos-transact-golang/dbos"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tscolari/bmus/internal/comms"
	"github.com/tscolari/bmus/internal/store"
)

// checkFailReason is the reason combadge serve's failed harness check gives.
const checkFailReason = "claude 9.9.9: a PreToolUse ask did not reach canUseTool under bypassPermissions; hails would not reach the captain"

// checkFail makes a holodeck serve look like one whose harness check failed
// (#172): once armed, every hello the pump reads becomes the crew-level
// error with code harness_check_failed such a serve queues instead, under
// the hello's seq. It keeps the sessions it wrapped.
type checkFail struct {
	armed atomic.Bool

	mu       sync.Mutex
	sessions []comms.Session
}

// wrap is a holodeckBridge.tunePump.
func (f *checkFail) wrap(c *comms.PumpConfig) { c.Link = checkFailLink{Link: c.Link, f: f} }

func (f *checkFail) last() comms.Session {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sessions[len(f.sessions)-1]
}

// swap returns line, or the error in its place for an armed hello.
func (f *checkFail) swap(line []byte) []byte {
	var m comms.Message
	if !f.armed.Load() || json.Unmarshal(line, &m) != nil || m.Type != comms.TypeHello {
		return line
	}
	e, err := comms.NewMessage(comms.TypeError, comms.ErrorBody{Reason: checkFailReason, Code: CodeHarnessCheckFailed})
	if err != nil {
		panic(err)
	}
	e.V, e.Seq, e.Crew, e.TS = m.V, m.Seq, m.Crew, m.TS
	out, err := json.Marshal(e)
	if err != nil {
		panic(err)
	}
	return append(out, '\n')
}

type checkFailLink struct {
	comms.Link
	f *checkFail
}

func (l checkFailLink) Attach(ctx context.Context, since uint64) (comms.Session, error) {
	s, err := l.Link.Attach(ctx, since)
	if err != nil {
		return nil, err
	}
	pr, pw := io.Pipe()
	go func() {
		r := bufio.NewReader(s.Stdout())
		for {
			line, err := r.ReadBytes('\n')
			if len(line) > 0 {
				if _, werr := pw.Write(l.f.swap(line)); werr != nil {
					return
				}
			}
			if err != nil {
				if errors.Is(err, io.EOF) {
					err = nil
				}
				pw.CloseWithError(err)
				return
			}
		}
	}()
	ws := swappedSession{Session: s, out: pr}
	l.f.mu.Lock()
	l.f.sessions = append(l.f.sessions, ws)
	l.f.mu.Unlock()
	return ws, nil
}

type swappedSession struct {
	comms.Session
	out io.Reader
}

func (s swappedSession) Stdout() io.Reader { return s.out }

// requireCheckFailSickbay requires tourID to have ended in sickbay with the
// failed check's reason and code, after logging the crew error.
func requireCheckFailSickbay(t *testing.T, hd *hdaemon, tourID string) {
	t.Helper()
	require.Equal(t, CrewTourResult{Ended: store.CrewSickbay}, tourResult(t, hd.d, tourID))
	require.Equal(t, store.CrewSickbay, hd.crew(t, "sorell").Status)
	entries := hd.logEntries(t)
	crewErr := slices.IndexFunc(entries, func(e store.LogEntry) bool { return e.Text == "crew error: "+checkFailReason })
	sick := slices.IndexFunc(entries, func(e store.LogEntry) bool { return strings.HasPrefix(e.Text, "crew to sickbay:") })
	require.GreaterOrEqual(t, crewErr, 0, "the crew error is logged")
	require.Greater(t, sick, crewErr)
	require.Equal(t, "crew to sickbay: "+checkFailReason, entries[sick].Text)
	require.Contains(t, string(entries[sick].Payload), `"code":"`+CodeHarnessCheckFailed+`"`)
	_, ok := hd.d.Pump("sorell")
	require.False(t, ok, "pump stopped")
}

// #172: a serve whose check failed sends harness_check_failed instead of
// hello. The hello wait ends the tour in sickbay with its reason, and the
// directed order waiting for the crew member is never claimed.
func TestHarnessCheckFailedBeforeHello(t *testing.T) {
	t.Parallel()
	b := newHolodeckBridge(t, quickScript(t))
	f := &checkFail{}
	f.armed.Store(true)
	b.tunePump = f.wrap
	hd := b.start(t)
	addSorell(t, hd)
	id := hd.createOrder(t)
	tourID := tourWorkflowID("sorell", 0)

	requireCheckFailSickbay(t, hd, tourID)
	names, _ := tourSteps(t, hd.d, tourID)
	require.Contains(t, names, "hello-deadline")
	require.NotContains(t, names, "hello-logged")
	require.NotContains(t, names, "claim-order")
	require.Equal(t, store.OrderDirected, hd.order(t, id).State)
	require.Empty(t, workflows(t, hd.d, missionWorkflowID(id)))
	_, err := os.Stat(b.stepLog)
	require.ErrorIs(t, err, os.ErrNotExist, "the holodeck ran no step")
}

// #172: an idle tour past its hello whose serve comes back failing its
// check (a reconnect brings harness_check_failed) ends in sickbay with its
// reason. One naming another beam (a stale pump's) is only logged.
func TestHarnessCheckFailedIdle(t *testing.T) {
	t.Parallel()
	b := newHolodeckBridge(t, quickScript(t))
	f := &checkFail{}
	b.tunePump = f.wrap
	hd := b.start(t)
	addSorell(t, hd)
	tourID := tourWorkflowID("sorell", 0)
	require.EventuallyWithT(t, func(ct *assert.CollectT) {
		names, _ := tourSteps(ct, hd.d, tourID)
		require.Contains(ct, names, "claim-order")
	}, 30*time.Second, 50*time.Millisecond)

	require.NoError(t, dbos.Send(hd.d.dbos, tourID, CrewSignal{Kind: SignalCrewError, Reason: "stale", Code: CodeHarnessCheckFailed, Beam: "beam-stale"},
		crewEventsTopic))
	require.EventuallyWithT(t, func(ct *assert.CollectT) {
		_, steps := tourSteps(ct, hd.d, tourID)
		require.Positive(ct, countSignal(steps, SignalCrewError))
	}, 30*time.Second, 50*time.Millisecond)
	requirePending(t, hd.d, tourID)
	require.Equal(t, store.CrewOnDeck, hd.crew(t, "sorell").Status, "on deck since its hello")
	// The tour logs the line after its Recv step is recorded.
	require.EventuallyWithT(t, func(ct *assert.CollectT) {
		logs, err := os.ReadFile(b.logFile)
		require.NoError(ct, err)
		require.Contains(ct, string(logs), "ignoring harness_check_failed from another beam")
	}, 30*time.Second, 50*time.Millisecond)

	f.armed.Store(true)
	_ = f.last().Close() // attach exits, and the pump attaches again
	requireCheckFailSickbay(t, hd, tourID)
}

// #172: a harness_check_failed crew error while a mission runs is handled
// as a beam loss: the mission gets beam_lost and parks, its order away for
// another crew member to resume, and the crew member goes to sickbay with
// the check's reason.
func TestHarnessCheckFailedMidMission(t *testing.T) {
	t.Parallel()
	script := filepath.Join(t.TempDir(), "script.json")
	require.NoError(t, os.WriteFile(script, []byte(`[{"at_pct": 50, "sleep_ms": 120000},
		{"at_pct": 100, "report": {"summary": "never reached"}}]`), 0o600))
	b := newHolodeckBridge(t, script)
	f := &checkFail{}
	b.tunePump = f.wrap
	hd := b.start(t)
	hd.addCrew(t)
	id := hd.createOrder(t)
	tourID := tourWorkflowID("sorell", 0)
	// Armed once the mission runs, wherever it is: one still before its
	// send-order parks as well (#195).
	require.EventuallyWithT(t, func(ct *assert.CollectT) {
		require.Equal(ct, store.OrderAway, hd.order(ct, id).State)
		require.Len(ct, workflows(ct, hd.d, missionWorkflowID(id)), 1)
	}, 30*time.Second, 50*time.Millisecond)

	f.armed.Store(true)
	_ = f.last().Close()
	requireCheckFailSickbay(t, hd, tourID)
	o := hd.order(t, id)
	require.Equal(t, store.OrderAway, o.State)
	require.NotNil(t, o.ParkedAt, "the mission parked")
	requirePending(t, hd.d, missionWorkflowID(id))
	entries := hd.logEntries(t)
	require.Equal(t, []string{"order parked: harness check failed: " + checkFailReason}, orderTexts(entries, id, "order parked"))
	requireNoBackstop(t, entries)
}
