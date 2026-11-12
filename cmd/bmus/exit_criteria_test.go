package main

import (
	"bufio"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tscolari/bmus/internal/bridge"
	"github.com/tscolari/bmus/internal/store"
	"github.com/tscolari/bmus/internal/transporter"
)

// The Phase 1 exit criteria (spec/initial-plan-2026-10-16.md §9), as #18's
// two tests, driving the real bmus binary like TestCLIEndToEnd:
//
//   - kill the bridge mid-mission and restart it, and the mission completes;
//   - kill the beam, and the crew member lands in sickbay with a checkpoint.

// fixtureScript returns the absolute path of a combadge holodeck fixture.
func fixtureScript(t *testing.T, name string) string {
	t.Helper()
	p, err := filepath.Abs(filepath.Join("..", "..", "combadge", "test", "fixtures", name))
	require.NoError(t, err)
	_, err = os.Stat(p)
	require.NoError(t, err)
	return p
}

// withStepLog makes every beam's serve beat every 100ms and record its
// executed steps in a step log, whose path it returns.
func (e *e2e) withStepLog(t *testing.T) string {
	t.Helper()
	stepLog := filepath.Join(t.TempDir(), "steps.ndjson")
	e.bridgeArgs = append(e.bridgeArgs,
		"--combadge-arg=--heartbeat-interval", "--combadge-arg=100",
		"--combadge-arg=--step-log", "--combadge-arg="+stepLog)
	return stepLog
}

// step is a line of the holodeck step log.
type step struct {
	Order  string    `json:"order"`
	Index  int       `json:"index"`
	Action string    `json:"action"`
	TS     time.Time `json:"ts"`
}

// readSteps reads the step log, requiring that no (order, index) step ran
// twice. A missing log is no steps.
func readSteps(t *testing.T, path string) []step {
	t.Helper()
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	require.NoError(t, err)
	var out []step
	seen := map[string]bool{}
	for line := range strings.SplitSeq(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		var s step
		require.NoError(t, json.Unmarshal([]byte(line), &s), "step log line %q", line)
		key := fmt.Sprintf("%s/%d", s.Order, s.Index)
		require.False(t, seen[key], "step %s ran twice:\n%s", key, data)
		seen[key] = true
		out = append(out, s)
	}
	return out
}

func stepIndexes(steps []step, order string) []int {
	var out []int
	for _, s := range steps {
		if s.Order == order {
			out = append(out, s.Index)
		}
	}
	return out
}

// logEntries returns the whole captain's log, through `log tail --json`.
func (e *e2e) logEntries(t require.TestingT) []bridge.LogEntryView {
	var out []bridge.LogEntryView
	sc := bufio.NewScanner(strings.NewReader(e.run(t, "log", "tail", "--json")))
	for sc.Scan() {
		var le bridge.LogEntryView
		require.NoError(t, json.Unmarshal(sc.Bytes(), &le))
		out = append(out, le)
	}
	return out
}

func (e *e2e) crewMember(t require.TestingT, id string) bridge.CrewView {
	var res bridge.CrewListResult
	e.runJSON(t, &res, "crew", "list")
	for _, c := range res.Crew {
		if c.ID == id {
			return c
		}
	}
	require.Fail(t, "crew member not listed", id)
	return bridge.CrewView{}
}

func (e *e2e) order(t require.TestingT, id string) bridge.OrderView {
	var res bridge.OrderListResult
	e.runJSON(t, &res, "order", "list")
	for _, o := range res.Orders {
		if o.ID == id {
			return o
		}
	}
	require.Fail(t, "order not listed", id)
	return bridge.OrderView{}
}

// checkpoints reads orderID's checkpoints rows, with their repos, straight
// from bridge.db, read-only, since no command lists them; the bridge may be
// running. The queries mirror store.ListCheckpoints.
func (e *e2e) checkpoints(t require.TestingT, orderID string) []store.Checkpoint {
	uri, err := store.FileURI(filepath.Join(e.home, store.DBFile))
	require.NoError(t, err)
	db, err := sql.Open("sqlite", uri+"?mode=ro&_pragma=busy_timeout(5000)")
	require.NoError(t, err)
	defer db.Close()
	rows, err := db.Query(`SELECT order_id, beam, seq, transcript_path, handover_path
		FROM checkpoints WHERE order_id = ? ORDER BY rowid`, orderID)
	require.NoError(t, err)
	var out []store.Checkpoint
	for rows.Next() {
		var c store.Checkpoint
		require.NoError(t, rows.Scan(&c.OrderID, &c.Beam, &c.Seq, &c.TranscriptPath, &c.HandoverPath))
		out = append(out, c)
	}
	require.NoError(t, rows.Err())
	require.NoError(t, rows.Close())
	for i, c := range out {
		rows, err := db.Query(`SELECT name, "commit", bundle_path FROM checkpoint_repos
			WHERE order_id = ? AND beam = ? AND seq = ? ORDER BY pos`, orderID, c.Beam, c.Seq)
		require.NoError(t, err)
		for rows.Next() {
			var r store.CheckpointRepo
			require.NoError(t, rows.Scan(&r.Name, &r.Commit, &r.BundlePath))
			out[i].Repos = append(out[i].Repos, r)
		}
		require.NoError(t, rows.Err())
		require.NoError(t, rows.Close())
	}
	return out
}

// requireCargo checks a checkpoints row's files are in the cargo bay at
// <home>/cargo/<order>/<seq>/: the transcript, the note and app's bundle,
// which carries bmus/<order> at commit.
func (e *e2e) requireCargo(t require.TestingT, cp store.Checkpoint, handover, commit string) {
	dir := filepath.Join(e.home, "cargo", cp.OrderID, cp.Beam+"-"+strconv.Itoa(cp.Seq))
	bundle := filepath.Join(dir, "app.bundle")
	require.Equal(t, []store.CheckpointRepo{{Name: "app", Commit: commit, BundlePath: bundle}}, cp.Repos)
	require.Equal(t, filepath.Join(dir, "transcript.jsonl"), cp.TranscriptPath)
	require.Equal(t, filepath.Join(dir, "handover.md"), cp.HandoverPath)
	require.Equal(t, commit+" refs/heads/bmus/"+cp.OrderID, gitT(t, dir, "bundle", "list-heads", bundle))
	transcript, err := os.ReadFile(cp.TranscriptPath)
	require.NoError(t, err)
	require.Contains(t, string(transcript), cp.OrderID)
	note, err := os.ReadFile(cp.HandoverPath)
	require.NoError(t, err)
	require.Equal(t, handover+"\n", string(note))
}

// onlyBeam returns the alias of the home's single holodeck beam and its
// serve's pid, from the beam directory, without asking the bridge.
func (e *e2e) onlyBeam(t *testing.T) (alias string, pid int) {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(e.home, "holodeck"))
	require.NoError(t, err)
	var dirs []string
	for _, ent := range entries {
		if ent.IsDir() {
			dirs = append(dirs, ent.Name())
		}
	}
	require.Len(t, dirs, 1, "one beam")
	data, err := os.ReadFile(filepath.Join(e.home, "holodeck", dirs[0], "serve.pid"))
	require.NoError(t, err)
	pid, err = strconv.Atoi(strings.TrimSpace(string(data)))
	require.NoError(t, err)
	return dirs[0], pid
}

// serveRunning reports whether pid is alive (not a zombie) and is a
// combadge serve.
func serveRunning(pid int) bool {
	if syscall.Kill(pid, 0) != nil {
		return false
	}
	stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return true // no procfs: kill(pid, 0) is all there is
	}
	if i := strings.LastIndexByte(string(stat), ')'); i >= 0 && i+2 < len(stat) && (stat[i+2] == 'Z' || stat[i+2] == 'X') {
		return false
	}
	cmdline, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	return err == nil && slices.Contains(strings.Split(string(cmdline), "\x00"), "serve")
}

// Exit criterion: SIGKILL the bridge while the beam's script pauses at 50%,
// start a new bridge on the same home, and the mission completes, with no
// script step run twice, one done entry and one report.
func TestMissionSurvivesBridgeRestart(t *testing.T) {
	t.Parallel()
	e := newE2E(t)
	// progress 10, progress 50, sleep_ms 10000, progress 90, report.
	e.script = fixtureScript(t, "pause.json")
	stepLog := e.withStepLog(t)

	br := e.startBridge(t)
	f := e.follow(t)
	e.run(t, "crew", "add", "sorell", "--name", "Sorell", "--rank", "Lieutenant", "--division", "science")
	orderID := e.createOrder(t)

	f.until(t, 30*time.Second, func(le bridge.LogEntryView) bool {
		return le.Kind == "order" && le.OrderID == orderID && le.Text == "halfway, pausing"
	})
	require.NoError(t, br.cmd.Process.Signal(syscall.SIGKILL))
	killed := time.Now()
	<-br.exited

	// Precondition: the kill landed inside the pause. A sleep step is
	// logged only once it completes, so step 2 must be missing; otherwise
	// the report may have got through before the kill, and nothing below
	// would exercise recovery.
	steps := readSteps(t, stepLog)
	require.Equal(t, []int{0, 1}, stepIndexes(steps, orderID),
		"the bridge must be killed during the pause (step 2, sleep_ms); step log: %+v", steps)
	t.Logf("bridge killed %s after step 1 ran, inside the 10s pause",
		killed.Sub(steps[len(steps)-1].TS).Round(time.Millisecond))

	alias, pid := e.onlyBeam(t)
	require.True(t, serveRunning(pid), "serve outlives the bridge")
	// The dead bridge's stale socket: clients report it not running.
	_, err := e.cmd(t.Context(), "crew", "list").CombinedOutput()
	var exitErr *exec.ExitError
	require.ErrorAs(t, err, &exitErr)
	require.Equal(t, exitNotRunning, exitErr.ExitCode())

	e.startBridge(t)
	f.until(t, 60*time.Second, func(le bridge.LogEntryView) bool {
		return le.Kind == "done" && le.OrderID == orderID
	})
	t.Logf("mission done %s after the kill", time.Since(killed).Round(time.Millisecond))
	f.stop(t)

	// The same serve ran the whole script, once: every step exactly once.
	alias2, pid2 := e.onlyBeam(t)
	require.Equal(t, alias, alias2)
	require.Equal(t, pid, pid2, "serve.pid unchanged across the bridge restart")
	require.True(t, serveRunning(pid), "serve still running")
	steps = readSteps(t, stepLog)
	require.Equal(t, []int{0, 1, 2, 3, 4}, stepIndexes(steps, orderID))
	require.Equal(t, "report", steps[len(steps)-1].Action)

	// The bridge side: one done entry carrying the one report, and each
	// progress line logged once.
	var dones []bridge.LogEntryView
	progress := map[string]int{}
	for _, le := range e.logEntries(t) {
		if le.OrderID != orderID {
			continue
		}
		switch {
		case le.Kind == "done":
			dones = append(dones, le)
		case le.Kind == "order" &&
			slices.Contains([]string{"surveying the codebase", "halfway, pausing", "resumed"}, le.Text):
			progress[le.Text]++
		}
	}
	require.Len(t, dones, 1, "one done entry")
	main := gitT(t, e.repo, "rev-parse", "main")
	require.Equal(t, "holodeck mission complete after pause (app: bmus/"+orderID+" at "+main[:12]+")", dones[0].Text)
	var report struct {
		Summary string `json:"summary"`
	}
	require.NoError(t, json.Unmarshal(dones[0].Payload, &report))
	require.Equal(t, "holodeck mission complete after pause", report.Summary)
	require.Equal(t, map[string]int{"surveying the codebase": 1, "halfway, pausing": 1, "resumed": 1}, progress)

	require.Equal(t, store.OrderDone, e.order(t, orderID).State)
	require.EventuallyWithT(t, func(ct *assert.CollectT) {
		c := e.crewMember(ct, "sorell")
		require.Equal(ct, store.CrewOnDeck, c.Status)
		require.Equal(ct, alias, c.CurrentBeam, "the beam is reused, not reprovisioned")
	}, 10*time.Second, 200*time.Millisecond)
}

// Exit criterion: the beam dies after a checkpoint (Kill SIGKILLs its
// serve), with the bridge up; within the pump's 30s staleness window plus
// its 5s check (45s allowed), the crew member is in sickbay, the checkpoint
// is in the cargo bay, and the order failed as beam_lost.
func TestCrewSickbayOnBeamLoss(t *testing.T) {
	t.Parallel()
	e := newE2E(t)
	// progress 10, request_handover at 50, sleep 60s, crash.
	e.script = fixtureScript(t, "crash.json")
	e.withStepLog(t)

	br := e.startBridge(t)
	e.run(t, "crew", "add", "sorell", "--name", "Sorell", "--rank", "Lieutenant", "--division", "science")
	orderID := e.createOrder(t)

	// Precondition: the checkpoint is pulled before the kill. A Kill that
	// beat the pump to the checkpoint_ready line would lose it in the dead
	// serve's outbox.
	var cps []store.Checkpoint
	require.EventuallyWithT(t, func(ct *assert.CollectT) {
		cps = e.checkpoints(ct, orderID)
		require.Len(ct, cps, 1)
	}, 30*time.Second, 100*time.Millisecond, "checkpoint never pulled")
	e.requireCargo(t, cps[0], "checkpoint before the beam is lost", gitT(t, e.repo, "rev-parse", "main"))
	require.Equal(t, store.OrderAway, e.order(t, orderID).State)

	c := e.crewMember(t, "sorell")
	require.Equal(t, store.CrewAway, c.Status)
	require.NotEmpty(t, c.CurrentBeam)
	// A fresh Holodeck on the same root: Kill works from the pid file.
	h, err := transporter.NewHolodeck(transporter.HolodeckConfig{
		Root:     filepath.Join(e.home, "holodeck"),
		Combadge: e.bundle,
	})
	require.NoError(t, err)
	_, pid := e.onlyBeam(t)
	require.True(t, serveRunning(pid))
	require.NoError(t, h.Kill(c.CurrentBeam))
	killed := time.Now()
	require.Eventually(t, func() bool { return !serveRunning(pid) }, 5*time.Second, 20*time.Millisecond, "serve killed")

	// The pump finds heartbeats stale 30s after the last one and checks
	// every 5s; the tour then settles the mission, each hop up to a second
	// late on DBOS's notification poll. About 32-38s in all, plus slack; the
	// 100ms heartbeats from withStepLog keep the last one at the kill.
	require.EventuallyWithT(t, func(ct *assert.CollectT) {
		require.Equal(ct, store.CrewSickbay, e.crewMember(ct, "sorell").Status)
	}, 45*time.Second, 250*time.Millisecond, "crew member not in sickbay")
	t.Logf("crew member in sickbay %s after the kill", time.Since(killed).Round(time.Millisecond))
	select {
	case <-br.exited:
		t.Fatalf("bridge exited: %v", br.err)
	default:
	}

	// The mission parks (#100): the order stays away, waiting for a
	// re-energize, a recall, a belay or a dismissal.
	require.EventuallyWithT(t, func(ct *assert.CollectT) {
		var parked bool
		for _, le := range e.logEntries(ct) {
			parked = parked || (le.OrderID == orderID && le.Text == "order parked: beam lost")
		}
		require.True(ct, parked)
	}, 10*time.Second, 200*time.Millisecond)
	require.Equal(t, store.OrderAway, e.order(t, orderID).State, "bmus order list shows it away")
	for _, le := range e.logEntries(t) {
		require.False(t, le.Kind == "fail" && le.OrderID == orderID, "the order failed: %+v", le)
	}

	cps = e.checkpoints(t, orderID)
	require.Len(t, cps, 1)
	e.requireCargo(t, cps[0], "checkpoint before the beam is lost", gitT(t, e.repo, "rev-parse", "main"))
	require.Equal(t, c.CurrentBeam, e.crewMember(t, "sorell").CurrentBeam)

	// Dismissing the crew member in sickbay fails its parked order (#100).
	require.Equal(t, "crew sorell dismissed\n", e.run(t, "crew", "dismiss", "sorell"))
	require.EventuallyWithT(t, func(ct *assert.CollectT) {
		require.Equal(ct, store.OrderFailed, e.order(ct, orderID).State)
	}, 10*time.Second, 200*time.Millisecond)
	require.Equal(t, store.CrewDismissed, e.crewMember(t, "sorell").Status)
	var fails []map[string]string
	for _, le := range e.logEntries(t) {
		if le.Kind == "fail" && le.OrderID == orderID {
			var p map[string]string
			require.NoError(t, json.Unmarshal(le.Payload, &p))
			fails = append(fails, p)
		}
	}
	require.Equal(t, []map[string]string{{"reason": "crew member dismissed", "code": "dismissed"}}, fails)
	err = e.cmd(t.Context(), "crew", "reenergize", "sorell").Run()
	require.Error(t, err, "a dismissed crew member can't be re-energized")
}
