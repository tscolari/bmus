package bridge

import (
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dbos-inc/dbos-transact-golang/dbos"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tscolari/bmus/internal/comms"
	"github.com/tscolari/bmus/internal/store"
	"github.com/tscolari/bmus/internal/transporter"
)

// loginTsh is a fake tsh for a bridge running on transporter.Tsh (#102).
// `tsh status` runs status.sh (setLogin). Every `tsh beams` call is
// recorded in beams.log and acts as one healthy beam, warm-orbit: add
// creates it, ls lists it, `beamctl list` shows combadge running, and an
// attach sends one heartbeat (seq 1, with the ts of when the fake was
// made) and stays open, silent, until its stdin closes.
type loginTsh struct {
	dir, bin string
}

func newLoginTsh(t *testing.T) *loginTsh {
	t.Helper()
	f := &loginTsh{dir: t.TempDir()}
	f.bin = filepath.Join(f.dir, "tsh")
	hb, err := comms.NewMessage(comms.TypeHeartbeat, comms.HeartbeatBody{})
	require.NoError(t, err)
	hb.Seq = 1
	line, err := json.Marshal(hb)
	require.NoError(t, err)
	beam := `{"id":"warm-orbit","expires":"` + time.Now().Add(24*time.Hour).UTC().Format(time.RFC3339) + `"}`
	script := `#!/bin/sh
dir='` + f.dir + `'
if [ "$1" = status ]; then . "$dir/status.sh"; exit; fi
echo "$*" >> "$dir/beams.log"
case "$2" in
add) echo '` + beam + `' ;;
ls) echo '[` + beam + `]' ;;
exec)
	case "$*" in
	*"'beamctl' 'list'"*) echo 'combadge (running PID=42)' ;;
	*"'attach'"*) echo '` + string(line) + `'; cat >/dev/null ;;
	esac ;;
esac
`
	require.NoError(t, os.WriteFile(f.bin, []byte(script), 0o755))
	f.setLogin(t, time.Now().Add(time.Hour))
	return f
}

// setLogin makes `tsh status` report a login valid until at, exiting 1
// once that has passed, as tsh does.
func (f *loginTsh) setLogin(t *testing.T, at time.Time) {
	t.Helper()
	s := `echo '{"active":{"username":"captain","valid_until":"` + at.UTC().Format(time.RFC3339Nano) + `"},"profiles":[]}'`
	if !at.After(time.Now()) {
		s += "; echo 'ERROR: Active profile expired.' >&2; exit 1"
	}
	require.NoError(t, os.WriteFile(filepath.Join(f.dir, "status.sh"), []byte(s+"\n"), 0o644))
}

// beamsCalls returns the `tsh beams` calls made so far.
func (f *loginTsh) beamsCalls(t require.TestingT) []string {
	data, err := os.ReadFile(filepath.Join(f.dir, "beams.log"))
	if os.IsNotExist(err) {
		return nil
	}
	require.NoError(t, err)
	return strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
}

func requireReasons(t *testing.T, d *Daemon, condition string, reasons ...string) {
	t.Helper()
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		s, err := d.Status(t.Context())
		require.NoError(c, err)
		require.Equal(c, condition, s.Condition)
		require.Equal(c, append([]string{}, reasons...), s.Reasons)
	}, 10*time.Second, 20*time.Millisecond)
}

// #102's acceptance, on Tsh with a fake tsh:
//   - status is yellow in the login's last 30 minutes, and once it expired;
//   - with the login expired no tsh beams call runs, a tour's provision
//     waits for a login (logged once) instead of going to sickbay, and the
//     pump keeps its crew member on deck;
//   - a new login lets everything go on.
func TestLoginExpiry(t *testing.T) {
	t.Parallel()
	f := newLoginTsh(t)
	f.setLogin(t, time.Now().Add(10*time.Minute))
	tsh := transporter.NewTsh(f.bin)
	tsh.Combadge = "./combadge.mjs" // never uploaded: combadge is running
	cfg := testConfig(privateHome(t))
	cfg.Transporter = tsh
	logf, err := os.Create(filepath.Join(t.TempDir(), "bridge.log"))
	require.NoError(t, err)
	cfg.Logger = slog.New(slog.NewTextHandler(logf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	t.Cleanup(func() {
		if t.Failed() {
			out, _ := os.ReadFile(logf.Name())
			t.Logf("bridge log:\n%s", tail(string(out), 150))
		}
	})
	cfg.loginCheckEvery = 20 * time.Millisecond
	cfg.loginWait = 20 * time.Millisecond
	cfg.provisionBackoff = 5 * time.Millisecond // a wait counted against the budget would end it at once
	// 300ms stays (#197): the fake attach sends one old heartbeat and then
	// nothing, so every connection is recycled once it is 300ms old, by
	// design. The test asserts no ack, probe count or connection count, only
	// that a connection comes up and that attach is called again after a new
	// login, so a slow store commit recycling one early changes nothing.
	cfg.tunePump = func(c *comms.PumpConfig) {
		c.HeartbeatStale = 300 * time.Millisecond
		c.SickbayCheckEvery = 50 * time.Millisecond
		c.BackoffMin, c.BackoffMax = 20*time.Millisecond, 100*time.Millisecond
	}
	r := startDaemonConfig(t, cfg)
	defer r.stop(t)
	d := r.d

	requireReasons(t, d, ConditionYellow, "tsh login expires in 10m")
	f.setLogin(t, time.Now().Add(-time.Minute))
	requireReasons(t, d, ConditionYellow, "tsh login expired")

	// Provisioning waits.
	require.NoError(t, d.AddCrew(t.Context(), store.Crew{ID: "sorell", Name: "Sorell"}))
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		require.Contains(c, logTexts(t, d, "sorell"), "provisioning waits for a tsh login")
	}, 10*time.Second, 20*time.Millisecond)
	// Nine attempts are eight login waits, 160ms: past the 150ms retry
	// budget (provisionBudget times provisionBackoff).
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		require.Greater(c, countPrefix(stepNames(t, d, beamWorkflowID("sorell", 0)), "provision"), 8)
	}, 10*time.Second, 20*time.Millisecond)
	require.Empty(t, f.beamsCalls(t), "no tsh beams call while the login is expired")
	texts := logTexts(t, d, "sorell")
	require.Equal(t, 1, countPrefix(texts, "provisioning waits for a tsh login"), "%q", texts)
	require.Zero(t, countPrefix(texts, "crew to sickbay"), "%q", texts)
	require.Zero(t, countPrefix(texts, "beam creation failed"), "%q", texts)
	requirePending(t, d, beamWorkflowID("sorell", 0))

	// A new login: provisioning goes on.
	f.setLogin(t, time.Now().Add(2*time.Hour))
	waitOnDeck(t, d, "sorell", "warm-orbit")
	requireReasons(t, d, ConditionGreen)
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		p, ok := d.Pump("sorell")
		require.True(c, ok)
		require.True(c, p.Connected())
	}, 10*time.Second, 20*time.Millisecond)

	// The login expires under a live crew member: its heartbeats go stale,
	// and the pump can neither probe its beam nor attach again.
	f.setLogin(t, time.Now().Add(-time.Second))
	requireReasons(t, d, ConditionYellow, "tsh login expired")
	time.Sleep(200 * time.Millisecond) // calls that passed the gate before it closed
	n := len(f.beamsCalls(t))
	time.Sleep(time.Second)
	require.Len(t, f.beamsCalls(t), n, "no tsh beams call while the login is expired")
	c := getCrew(t, d, "sorell")
	require.Equal(t, store.CrewOnDeck, c.Status)
	require.Equal(t, "warm-orbit", c.CurrentBeam)
	requirePending(t, d, tourWorkflowID("sorell", 0))

	// A new login: the pump probes the beam and attaches again.
	f.setLogin(t, time.Now().Add(2*time.Hour))
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		calls := f.beamsCalls(c)
		require.Greater(c, len(calls), n)
		require.True(c, strings.Contains(strings.Join(calls[n:], "\n"), "'attach'"), "%q", calls[n:])
	}, 10*time.Second, 20*time.Millisecond)
	requireReasons(t, d, ConditionGreen)
	require.Equal(t, store.CrewOnDeck, getCrew(t, d, "sorell").Status)
}

// expireLogin sets p's login expired or valid and waits for the daemon to
// read it.
func expireLogin(t *testing.T, d *Daemon, p *fakeProvider, expired bool) {
	t.Helper()
	p.loginExpired.Store(expired)
	require.Eventually(t, func() bool { return d.loginExpired() == expired }, 10*time.Second, 10*time.Millisecond)
}

// Review of #102: a report that arrives while the login is expired (an
// attach opened before the expiry keeps delivering) doesn't fail the order:
// pull-work waits for a login, logged once, and the order is done after it.
func TestPullWorkWaitsForLogin(t *testing.T) {
	t.Parallel()
	home := privateHome(t)
	insertCrew(t, home, store.Crew{ID: "sorell", Name: "Sorell"})
	beams := t.TempDir()
	p := &fakeProvider{hello: true, copyRoot: beams}
	cfg := testConfig(home)
	cfg.Transporter = p
	cfg.Repos = map[string]string{"app": testRepo(t).url}
	cfg.loginCheckEvery = 20 * time.Millisecond
	cfg.loginWait = 20 * time.Millisecond
	cfg.copyRetries = fastCopyRetries
	r := startDaemonConfig(t, cfg)
	defer r.stop(t)
	waitOnDeck(t, r.d, "sorell", "fake-1")
	id := startMission(t, r.d)
	work := "/beams/fake-1/work/" + id
	_, commit := workBundle(t, filepath.Join(beams, work, "in", "app.bundle"), id, filepath.Join(beams, work, "out", "app.bundle"))

	expireLogin(t, r.d, p, true)
	sendReport(t, r.d, id, 2, comms.ReportBody{Summary: "did it",
		Repos: []comms.WorkRepo{{Name: "app", BundlePath: work + "/out/app.bundle", Commit: commit, Branch: "bmus/" + id}}})
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		require.Contains(c, logTexts(t, r.d, "sorell"), "pull-work waits for a tsh login")
	}, 20*time.Second, 20*time.Millisecond)
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		require.GreaterOrEqual(c, countPrefix(stepNames(t, r.d, missionWorkflowID(id)), "login-expired"), 2)
	}, 20*time.Second, 20*time.Millisecond)
	require.Equal(t, store.OrderAway, getOrder(t, r.d, id).State)

	expireLogin(t, r.d, p, false)
	res := missionResult(t, r.d, id)
	require.Equal(t, MissionDone, res.Status, "%+v %q", res, stepNames(t, r.d, missionWorkflowID(id)))
	require.Equal(t, store.OrderDone, getOrder(t, r.d, id).State)
	texts := logTexts(t, r.d, "sorell")
	require.Equal(t, 1, countPrefix(texts, "pull-work waits for a tsh login"), "%q", texts)
	// Each round of the wait is pull-work's retries, login-expired, a sleep.
	steps := stepNames(t, r.d, missionWorkflowID(id))
	require.GreaterOrEqual(t, countPrefix(steps, "login-expired"), 2, "%q", steps)
	require.Equal(t, countPrefix(steps, "login-expired")+1, countPrefix(steps, "pull-work"), "%q", steps)
}

// Review of #102: while the login is expired an idle tour claims nothing,
// so the pool isn't drained into failed orders; a new login wakes it.
func TestNoClaimWhileLoginExpired(t *testing.T) {
	t.Parallel()
	home := privateHome(t)
	insertCrew(t, home, store.Crew{ID: "sorell", Name: "Sorell"})
	p := &fakeProvider{hello: true}
	cfg := testConfig(home)
	cfg.Transporter = p
	cfg.Repos = map[string]string{"app": testRepo(t).url}
	cfg.loginCheckEvery = 20 * time.Millisecond
	r := startDaemonConfig(t, cfg)
	defer r.stop(t)
	waitOnDeck(t, r.d, "sorell", "fake-1")
	// The tour's first claim checks the login before it claims: one still
	// running at expireLogin can take an order created meanwhile.
	tour := tourWorkflowID("sorell", 0)
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		require.Positive(c, countPrefix(stepNames(t, r.d, tour), "claim-order"))
	}, 10*time.Second, 20*time.Millisecond)

	expireLogin(t, r.d, p, true)
	claims := countPrefix(stepNames(t, r.d, tour), "claim-order")
	id, err := r.d.CreateOrder(t.Context(), store.Order{Title: "t", Assignee: "sorell",
		Repos: []store.OrderRepo{{Name: "app", BaseRef: "main"}}})
	require.NoError(t, err)
	// The order's signal wakes the tour, which claims again: nothing.
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		require.Greater(c, countPrefix(stepNames(t, r.d, tour), "claim-order"), claims)
	}, 20*time.Second, 20*time.Millisecond)
	require.Equal(t, store.OrderDirected, getOrder(t, r.d, id).State, "claimed while the login is expired")
	require.Equal(t, store.CrewOnDeck, getCrew(t, r.d, "sorell").Status)

	expireLogin(t, r.d, p, false)
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		require.Equal(c, store.OrderAway, getOrder(t, r.d, id).State)
	}, 10*time.Second, 20*time.Millisecond)
}

// fastCopyRetries shortens the retries of the steps that wait for a login.
var fastCopyRetries = []dbos.StepOption{dbos.WithStepMaxRetries(2),
	dbos.WithStepBaseInterval(5 * time.Millisecond), dbos.WithStepMaxInterval(10 * time.Millisecond)}

// loginWaitConfig is testConfig with p, the app repo, fast login checks and
// fastCopyRetries.
func loginWaitConfig(t *testing.T, home string, p *fakeProvider) Config {
	cfg := testConfig(home)
	cfg.Transporter = p
	cfg.Repos = map[string]string{"app": testRepo(t).url}
	cfg.loginCheckEvery = 20 * time.Millisecond
	cfg.loginWait = 20 * time.Millisecond
	cfg.copyRetries = fastCopyRetries
	return cfg
}

// Review of #102: a checkpoint that arrives while the login is expired
// waits for a login (logged once) instead of failing, and lands after it.
func TestPullCheckpointWaitsForLogin(t *testing.T) {
	t.Parallel()
	home := privateHome(t)
	insertCrew(t, home, store.Crew{ID: "sorell", Name: "Sorell"})
	beams := t.TempDir()
	p := &fakeProvider{hello: true, copyRoot: beams}
	r := startDaemonConfig(t, loginWaitConfig(t, home, p))
	defer r.stop(t)
	waitOnDeck(t, r.d, "sorell", "fake-1")
	id := startMission(t, r.d)
	work := "/beams/fake-1/work/" + id
	_, commit := workBundle(t, filepath.Join(beams, work, "in", "app.bundle"), id, filepath.Join(beams, work, "out", "app.bundle"))
	for _, f := range []string{"/t", "/h"} {
		require.NoError(t, os.WriteFile(filepath.Join(beams, f), []byte("x"), 0o600))
	}

	expireLogin(t, r.d, p, true)
	cp, err := comms.NewMessage(comms.TypeCheckpointReady, comms.CheckpointReadyBody{
		Repos:          []comms.WorkRepo{{Name: "app", BundlePath: work + "/out/app.bundle", Commit: commit, Branch: "bmus/" + id}},
		TranscriptPath: "/t", HandoverPath: "/h",
	})
	require.NoError(t, err)
	cp.Seq, cp.Crew, cp.Order = 2, "sorell", id
	require.NoError(t, dbos.Send(r.d.dbos, missionWorkflowID(id), cp, fromCrewTopic))
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		require.Contains(c, logTexts(t, r.d, "sorell"), "pull-checkpoint waits for a tsh login")
	}, 20*time.Second, 20*time.Millisecond)
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		require.GreaterOrEqual(c, countPrefix(stepNames(t, r.d, missionWorkflowID(id)), "login-expired"), 2)
	}, 20*time.Second, 20*time.Millisecond)

	expireLogin(t, r.d, p, false)
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		cps, err := r.d.store.ListCheckpoints(t.Context(), id)
		require.NoError(c, err)
		require.Len(c, cps, 1)
	}, 20*time.Second, 20*time.Millisecond)
	texts := logTexts(t, r.d, "sorell")
	require.Equal(t, 1, countPrefix(texts, "pull-checkpoint waits for a tsh login"), "%q", texts)
	require.Empty(t, codes(t, orderLog(t, r.d, id)), "no checkpoint_failed")
	require.Equal(t, store.OrderAway, getOrder(t, r.d, id).State)
}

// Review of #102: a login that expires after the claim, before the repos
// are shipped, makes ship-repos wait (logged once) instead of failing the
// order; the order is sent after a new login. The daemon's own check runs
// hourly here, so only login-expired's fresh read sees the login go and
// come back, as with a tsh logout before valid_until.
func TestShipReposWaitsForLogin(t *testing.T) {
	t.Parallel()
	home := privateHome(t)
	insertCrew(t, home, store.Crew{ID: "sorell", Name: "Sorell"})
	p := &fakeProvider{hello: true, copyRoot: t.TempDir()}
	cfg := loginWaitConfig(t, home, p)
	cfg.loginCheckEvery = time.Hour
	r := startDaemonConfig(t, cfg)
	defer r.stop(t)
	waitOnDeck(t, r.d, "sorell", "fake-1")

	p.expireOnCopy.Store(true)
	id, err := r.d.CreateOrder(t.Context(), store.Order{Title: "t", Assignee: "sorell",
		Repos: []store.OrderRepo{{Name: "app", BaseRef: "main"}}})
	require.NoError(t, err)
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		require.Contains(c, logTexts(t, r.d, "sorell"), "ship-repos waits for a tsh login")
	}, 20*time.Second, 20*time.Millisecond)
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		require.GreaterOrEqual(c, countPrefix(stepNames(t, r.d, missionWorkflowID(id)), "login-expired"), 2)
	}, 20*time.Second, 20*time.Millisecond)
	require.Equal(t, store.OrderAway, getOrder(t, r.d, id).State)
	_, sent := stepOutput(t, r.d, missionWorkflowID(id), "send-order")
	require.False(t, sent)

	p.loginExpired.Store(false)
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		_, ok := stepOutput(c, r.d, missionWorkflowID(id), "send-order")
		require.True(c, ok)
	}, 20*time.Second, 20*time.Millisecond)
	require.Equal(t, store.OrderAway, getOrder(t, r.d, id).State)
	texts := logTexts(t, r.d, "sorell")
	require.Equal(t, 1, countPrefix(texts, "ship-repos waits for a tsh login"), "%q", texts)
	require.Equal(t, 1, countPrefix(texts, "repo app shipped to the beam"), "%q", texts)
}

// The unknown reason carries the error's first line, at most 120 bytes.
func TestShortErr(t *testing.T) {
	t.Parallel()
	require.Equal(t, "tsh status: exit status 1", shortErr(errors.New("tsh status: exit status 1\nmore")))
	long := strings.Repeat("x", 300)
	require.Equal(t, long[:maxLoginErr]+"...", shortErr(errors.New(long)))
}
