package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tscolari/bmus/internal/bridge"
	"github.com/tscolari/bmus/internal/store"
	"github.com/tscolari/bmus/internal/transporter"
)

// The CLI end to end, and #17's acceptance: a real `bmus bridge run
// --transporter holodeck` subprocess, driven by the bmus binary as an
// operator would. Like the bridge's holodeck tests it takes the combadge
// bundle from $BMUS_COMBADGE (then a missing bundle or node fails the test)
// or builds it with `npm run build`, skipping if neither works.

var buildBundle = sync.OnceValues(func() (string, error) {
	dir, err := filepath.Abs(filepath.Join("..", "..", "combadge"))
	if err != nil {
		return "", err
	}
	if _, err := exec.LookPath("npm"); err != nil {
		return "", fmt.Errorf("npm not found: %w", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "node_modules")); err != nil {
		return "", fmt.Errorf("combadge dependencies not installed (run npm ci in %s): %w", dir, err)
	}
	cmd := exec.Command("npm", "run", "build")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("npm run build: %w: %s", err, out)
	}
	return filepath.Join(dir, "dist", "combadge.mjs"), nil
})

func combadgeBundle(t *testing.T) string {
	t.Helper()
	if p := os.Getenv("BMUS_COMBADGE"); p != "" {
		abs, err := filepath.Abs(p)
		require.NoError(t, err)
		_, err = os.Stat(abs)
		require.NoError(t, err, "BMUS_COMBADGE")
		_, err = exec.LookPath("node")
		require.NoError(t, err, "BMUS_COMBADGE is set but node isn't on PATH")
		return abs
	}
	if _, err := exec.LookPath("node"); err != nil {
		t.Skipf("needs node: %v (or set BMUS_COMBADGE in an environment with node)", err)
	}
	p, err := buildBundle()
	if err != nil {
		t.Skipf("needs the combadge bundle: set BMUS_COMBADGE or make `npm run build` work: %v", err)
	}
	return p
}

// binDir is where buildBinary put the binary; TestMain removes it.
var binDir string

var buildBinary = sync.OnceValues(func() (string, error) {
	dir, err := os.MkdirTemp("", "bmus-bin")
	if err != nil {
		return "", err
	}
	binDir = dir
	bin := filepath.Join(dir, "bmus")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		return "", fmt.Errorf("go build: %w: %s", err, out)
	}
	return bin, nil
})

func TestMain(m *testing.M) {
	code := m.Run()
	if binDir != "" {
		os.RemoveAll(binDir)
	}
	os.Exit(code)
}

// cli runs bmus commands against one home.
type cli struct {
	bin, home string
}

func (c cli) cmd(ctx context.Context, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, c.bin, append([]string{"--home", c.home}, args...)...)
	cmd.Env = append(os.Environ(), "BMUS_HOME=") // --home only
	return cmd
}

// run runs a command to completion and returns its stdout, failing the
// test on a non-zero exit.
func (c cli) run(t require.TestingT, args ...string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var stdout, stderr bytes.Buffer
	cmd := c.cmd(ctx, args...)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	require.NoError(t, cmd.Run(), "bmus %s: %s", strings.Join(args, " "), stderr.String())
	return stdout.String()
}

func (c cli) runJSON(t require.TestingT, v any, args ...string) {
	require.NoError(t, json.Unmarshal([]byte(c.run(t, append(args, "--json")...)), v))
}

// syncBuffer is a bytes.Buffer safe to read while a command writes it.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

// e2e is a bridge home for a test, driven through the bmus binary, whose
// holodeck beams all run script (default.json unless the test changes it;
// with no script each beam runs its crew member's harness).
// The bridge registers one repo, app: repo, a local git repo with main at
// two commits.
type e2e struct {
	cli
	bundle, script, logFile string
	repo                    string
	// bridgeArgs are extra `bridge run` flags, e.g. --combadge-arg pairs.
	bridgeArgs []string
	// console runs the bridge with its console; it runs with --no-console
	// otherwise.
	console bool
}

// gitT runs git in dir with a fixed identity and returns its trimmed stdout.
func gitT(t require.TestingT, dir string, args ...string) string {
	cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@example.com",
		"-c", "commit.gpgsign=false"}, args...)...)
	cmd.Dir = dir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	require.NoError(t, err, "git %v: %s", args, stderr.String())
	return strings.TrimSpace(string(out))
}

// newRepo makes a git repo with main at two commits.
func newRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	gitT(t, dir, "init", "-q", "-b", "main")
	for _, msg := range []string{"one", "two"} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, "file.txt"), []byte(msg+"\n"), 0o600))
		gitT(t, dir, "add", "file.txt")
		gitT(t, dir, "commit", "-q", "-m", msg)
	}
	return dir
}

func newE2E(t *testing.T) *e2e {
	t.Helper()
	if testing.Short() {
		t.Skip("builds and runs the bmus binary")
	}
	bundle := combadgeBundle(t)
	bin, err := buildBinary()
	require.NoError(t, err)
	script, err := filepath.Abs(filepath.Join("..", "..", "combadge", "test", "fixtures", "default.json"))
	require.NoError(t, err)

	// A short home: beams' sockets live under <home>/holodeck.
	base := os.TempDir()
	if len(base) > 20 {
		base = "/tmp"
	}
	home, err := os.MkdirTemp(base, "bmus")
	require.NoError(t, err)
	e := &e2e{cli: cli{bin: bin, home: home}, bundle: bundle, script: script,
		logFile: filepath.Join(t.TempDir(), "bridge.log"), repo: newRepo(t)}
	// Runs after every bridge's cleanup (cleanups are last in, first
	// out): remove every holodeck beam, since serve outlives the bridge as
	// on a real beam, then the home.
	t.Cleanup(func() {
		root := filepath.Join(home, "holodeck")
		h, err := transporter.NewHolodeck(transporter.HolodeckConfig{Root: root, Combadge: bundle})
		if assert.NoError(t, err) {
			entries, _ := os.ReadDir(root)
			for _, ent := range entries {
				if ent.IsDir() {
					assert.NoError(t, h.Remove(context.Background(), transporter.Beam{Alias: ent.Name()}))
				}
			}
		}
		os.RemoveAll(home)
		if t.Failed() {
			b, _ := os.ReadFile(e.logFile)
			t.Logf("bridge log:\n%s", b)
		}
	})
	return e
}

// bridgeProc is a running `bmus bridge run`.
type bridgeProc struct {
	cmd    *exec.Cmd
	exited chan struct{}
	err    error // set when exited is closed
}

// startBridge starts the bridge and waits until it serves RPC. It is
// killed at the end of the test unless it exited already.
func (e *e2e) startBridge(t *testing.T) *bridgeProc {
	t.Helper()
	lf, err := os.OpenFile(e.logFile, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	require.NoError(t, err)
	t.Cleanup(func() { lf.Close() })
	b := &bridgeProc{exited: make(chan struct{})}
	args := []string{"bridge", "run", "--transporter", "holodeck", "--combadge", e.bundle, "--log-level", "debug",
		"--repo", "app=" + e.repo}
	if !e.console {
		args = append(args, "--no-console")
	}
	if e.script != "" {
		args = append(args, "--holodeck-script", e.script)
	}
	args = append(args, e.bridgeArgs...)
	b.cmd = e.cmd(context.Background(), args...)
	b.cmd.Stdout, b.cmd.Stderr = lf, lf
	require.NoError(t, b.cmd.Start())
	go func() {
		b.err = b.cmd.Wait()
		close(b.exited)
	}()
	t.Cleanup(func() {
		select {
		case <-b.exited:
		default:
			b.cmd.Process.Kill()
			<-b.exited
		}
	})
	require.EventuallyWithT(t, func(ct *assert.CollectT) {
		select {
		case <-b.exited:
			require.Fail(ct, "bridge exited", "%v", b.err)
		default:
			e.run(ct, "crew", "list")
		}
	}, 20*time.Second, 100*time.Millisecond, "bridge never served RPC")
	return b
}

// sigterm stops the bridge and requires a clean exit.
func (b *bridgeProc) sigterm(t *testing.T) {
	t.Helper()
	require.NoError(t, b.cmd.Process.Signal(syscall.SIGTERM))
	select {
	case <-b.exited:
		require.NoError(t, b.err, "bridge exit")
	case <-time.After(90 * time.Second):
		t.Fatal("bridge did not exit after SIGTERM")
	}
}

// follower is a running `bmus log tail --follow --json`.
type follower struct {
	cmd    *exec.Cmd
	lines  chan bridge.LogEntryView
	stderr *syncBuffer
	exited chan struct{}
	err    error // set when exited is closed
	lastID int64
	seen   []string
}

func (e *e2e) follow(t *testing.T) *follower {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	f := &follower{lines: make(chan bridge.LogEntryView, 1000), stderr: &syncBuffer{}, exited: make(chan struct{})}
	f.cmd = e.cmd(ctx, "log", "tail", "--follow", "--json")
	stdout, err := f.cmd.StdoutPipe()
	require.NoError(t, err)
	f.cmd.Stderr = f.stderr
	require.NoError(t, f.cmd.Start())
	go func() {
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			var le bridge.LogEntryView
			if json.Unmarshal(sc.Bytes(), &le) == nil {
				f.lines <- le
			}
		}
		close(f.lines)
		f.err = f.cmd.Wait()
		close(f.exited)
	}()
	return f
}

// until reads entries until match accepts one, requiring ids to rise
// strictly, and returns the entries read.
func (f *follower) until(t *testing.T, timeout time.Duration, match func(bridge.LogEntryView) bool) []bridge.LogEntryView {
	t.Helper()
	var got []bridge.LogEntryView
	deadline := time.After(timeout)
	for {
		select {
		case le, ok := <-f.lines:
			require.True(t, ok, "log tail --follow ended early: %s; saw %v", f.stderr.String(), f.seen)
			require.Greater(t, le.ID, f.lastID, "log ids rise strictly")
			f.lastID = le.ID
			f.seen = append(f.seen, le.Kind+": "+le.Text)
			got = append(got, le)
			if match(le) {
				return got
			}
		case <-deadline:
			t.Fatalf("no matching log entry within %s; saw %v", timeout, f.seen)
		}
	}
}

// running requires the follower not to have exited.
func (f *follower) running(t *testing.T) {
	t.Helper()
	select {
	case <-f.exited:
		t.Fatalf("log tail --follow exited: %v: %s", f.err, f.stderr.String())
	default:
	}
}

// stop sends SIGINT and requires a clean exit.
func (f *follower) stop(t *testing.T) {
	t.Helper()
	require.NoError(t, f.cmd.Process.Signal(syscall.SIGINT))
	for range f.lines {
	}
	<-f.exited
	require.NoError(t, f.err, f.stderr.String())
}

func texts(es []bridge.LogEntryView) []string {
	var out []string
	for _, le := range es {
		out = append(out, le.Kind+": "+le.Text)
	}
	return out
}

func textIs(kind, text string) func(bridge.LogEntryView) bool {
	return func(le bridge.LogEntryView) bool { return le.Kind == kind && le.Text == text }
}

const hailQuestion = "Run make deploy?"

// orderYAML writes an order YAML for sorell on the given repos lines and
// returns its path.
func orderYAML(t *testing.T, repos string) string {
	t.Helper()
	order := filepath.Join(t.TempDir(), "order.yaml")
	require.NoError(t, os.WriteFile(order, []byte(`title: holodeck run
division: science
assignee: sorell
repos:
`+repos+`budget_tokens: 5000
body: |
  Walk the script.
`), 0o600))
	return order
}

func (e *e2e) createOrder(t *testing.T) string {
	t.Helper()
	return strings.TrimSpace(e.run(t, "order", "create", orderYAML(t, "  - name: app\n    base_ref: main\n")))
}

// The CLI end to end, and #17's acceptance: progress shows live in
// `log tail --follow`, and `hails answer` unblocks the pending hail.
func TestCLIEndToEnd(t *testing.T) {
	t.Parallel()
	e := newE2E(t)

	// Not running yet: a clear error and exit code 3.
	out, err := e.cmd(t.Context(), "crew", "list").CombinedOutput()
	var exitErr *exec.ExitError
	require.ErrorAs(t, err, &exitErr)
	require.Equal(t, exitNotRunning, exitErr.ExitCode())
	require.Contains(t, string(out), "bridge not running (start it with `bmus bridge run`)")
	_, err = e.cmd(t.Context(), "status").Output()
	require.ErrorAs(t, err, &exitErr)
	require.Equal(t, exitNotRunning, exitErr.ExitCode())

	br := e.startBridge(t)
	f := e.follow(t)
	// A new crew member is beaming in until its beam's hello (#106).
	require.Regexp(t, `^crew sorell added \((beaming_in|on_deck)\); tour started, provisioning a beam\n$`,
		e.run(t, "crew", "add", "sorell", "--name", "Sorell", "--rank", "Lieutenant", "--division", "science"))
	// Green with sorell on deck, its beam up (#70). No regs.yaml is
	// information, never a reason (#149).
	require.EventuallyWithT(t, func(ct *assert.CollectT) {
		var st bridge.BridgeStatus
		e.runJSON(ct, &st, "status")
		require.Equal(ct, bridge.BridgeStatus{Condition: "green", Reasons: []string{}, Info: []string{"regs.yaml not configured"},
			Crew: bridge.CrewCounts{OnDeck: 1}, BeamsLive: 1, LastLogID: st.LastLogID}, st)
	}, 30*time.Second, 200*time.Millisecond)
	require.Regexp(t, `(?m)^condition\s+green\n(.*\n)*info\s+regs\.yaml not configured\n`, e.run(t, "status"))
	require.Regexp(t, `(?m)^crew\s+0 beaming in, 1 on deck, 0 away, 0 hailing, 0 in sickbay$`, e.run(t, "status"))
	orderID := e.createOrder(t)
	require.Equal(t, "ORD-0001", orderID)

	// Before any answer, the follower shows the mission's progress and its
	// hail, live, while the mission is blocked on it.
	before := f.until(t, 30*time.Second, textIs("hail", hailQuestion))
	require.Contains(t, texts(before), "order: order created: holodeck run")
	require.Contains(t, texts(before), "order: surveying the codebase")
	require.NotContains(t, texts(before), "order: tests passing")
	var hails bridge.HailsListResult
	e.runJSON(t, &hails, "hails", "list")
	require.Len(t, hails.Hails, 1)
	hail := hails.Hails[0]
	require.Equal(t, orderID, hail.OrderID)
	require.Equal(t, []string{"allow", "deny"}, hail.Options)
	// Yellow while the hail is open (#70).
	var st bridge.BridgeStatus
	e.runJSON(t, &st, "status")
	require.Equal(t, "yellow", st.Condition)
	require.Equal(t, []string{"sorell hailing"}, st.Reasons)
	require.Equal(t, bridge.CrewCounts{Hailing: 1}, st.Crew)
	require.Equal(t, bridge.OrderCounts{Away: 1}, st.Orders)
	require.Equal(t, 1, st.HailsOpen)
	require.Contains(t, e.run(t, "status"), "yellow (sorell hailing)")
	var orders bridge.OrderListResult
	e.runJSON(t, &orders, "order", "list")
	require.Equal(t, store.OrderHailing, orders.Orders[0].State, "blocked on the hail (#134)")
	// The order carries its latest progress (#141).
	require.NotNil(t, orders.Orders[0].ProgressPct)
	require.InDelta(t, 10, *orders.Orders[0].ProgressPct, 0)
	require.Equal(t, "surveying the codebase", orders.Orders[0].ProgressNote)
	require.Regexp(t, `(?m)^ID\s+STATE\s+PROGRESS\s+USAGE\s+ASSIGNEE\s+DIVISION\s+TITLE\n`+orderID+`\s+hailing\s+10%\s+0/5k\s+sorell\s`,
		e.run(t, "order", "list"))

	require.Equal(t, "H-0001 decided: allow\n", e.run(t, "hails", "answer", hail.ID, "--option", "0"))
	_, err = e.cmd(t.Context(), "hails", "answer", hail.ID, "--decision", "deny").Output()
	require.ErrorAs(t, err, &exitErr, "answering again fails")
	require.Equal(t, exitError, exitErr.ExitCode())

	// After it, the rest of the mission arrives, through to done.
	after := f.until(t, 30*time.Second, func(le bridge.LogEntryView) bool {
		return le.Kind == "done" && le.OrderID == orderID
	})
	require.Contains(t, texts(after), "hail: hail H-0001 for the captain: regs.yaml not configured")
	require.Contains(t, texts(after), "hail: hail H-0001 decided: allow")
	require.Contains(t, texts(after), "order: tests passing")
	main := gitT(t, e.repo, "rev-parse", "main")
	require.Equal(t, "done: holodeck mission complete (app: bmus/"+orderID+" at "+main[:12]+")", texts(after)[len(after)-1])

	f.stop(t)

	e.runJSON(t, &orders, "order", "list")
	require.Len(t, orders.Orders, 1)
	require.Equal(t, store.OrderDone, orders.Orders[0].State)
	// Settling cleared the progress: the JSON omits it, the table shows "-".
	require.NotContains(t, e.run(t, "order", "list", "--json"), "progress_")
	require.Regexp(t, `(?m)^`+orderID+`\s+done\s+-\s+0/5k\s+sorell\s`, e.run(t, "order", "list"))
	require.Contains(t, e.fail(t, "order", "budget", orderID, "--tokens", "1000"), "order "+orderID+" is done, too late to change its budget")
	require.EventuallyWithT(t, func(ct *assert.CollectT) {
		var crew bridge.CrewListResult
		e.runJSON(ct, &crew, "crew", "list")
		require.Len(ct, crew.Crew, 1)
		require.Equal(ct, store.CrewOnDeck, crew.Crew[0].Status)
	}, 10*time.Second, 200*time.Millisecond)

	// SIGTERM: a clean exit, and the socket is gone.
	br.sigterm(t)
	_, err = os.Stat(filepath.Join(e.home, bridge.SocketFile))
	require.ErrorIs(t, err, os.ErrNotExist)
}

// #58's acceptance: on a holodeck bridge without a script, `crew add
// --harness echo` starts serve with --harness echo, and its hello says so. An
// unknown harness is refused before reaching the bridge.
func TestCrewAddHarness(t *testing.T) {
	t.Parallel()
	e := newE2E(t)
	e.script = ""
	e.startBridge(t)

	out, err := e.cmd(t.Context(), "crew", "add", "kirk", "--name", "Kirk", "--harness", "codex").CombinedOutput()
	var exitErr *exec.ExitError
	require.ErrorAs(t, err, &exitErr)
	require.Equal(t, exitError, exitErr.ExitCode())
	require.Contains(t, string(out), `--harness: want one of sdk, holodeck, echo, got "codex"`)

	// Without --holodeck-script the bridge can't run holodeck (#64).
	out, err = e.cmd(t.Context(), "crew", "add", "kirk", "--name", "Kirk", "--harness", "holodeck").CombinedOutput()
	require.ErrorAs(t, err, &exitErr)
	require.Equal(t, exitError, exitErr.ExitCode())
	require.Contains(t, string(out), `this bridge can't run harness "holodeck"`)
	var none bridge.CrewListResult
	e.runJSON(t, &none, "crew", "list")
	require.Empty(t, none.Crew)

	var added bridge.CrewView
	e.runJSON(t, &added, "crew", "add", "sorell", "--name", "Sorell", "--harness", "echo", "--model", "model-crew")
	require.Equal(t, "echo", added.Harness)
	require.Equal(t, "model-crew", added.Model)
	var beam string
	require.EventuallyWithT(t, func(ct *assert.CollectT) {
		var crew bridge.CrewListResult
		e.runJSON(ct, &crew, "crew", "list")
		require.Len(ct, crew.Crew, 1)
		require.Equal(ct, store.CrewOnDeck, crew.Crew[0].Status)
		beam = crew.Crew[0].CurrentBeam
		require.NotEmpty(ct, beam)
	}, 30*time.Second, 200*time.Millisecond)

	dir := filepath.Join(e.home, "holodeck", beam)
	if pid, err := os.ReadFile(filepath.Join(dir, "serve.pid")); err == nil {
		if cmdline, err := os.ReadFile("/proc/" + strings.TrimSpace(string(pid)) + "/cmdline"); err == nil {
			require.Contains(t, string(cmdline), "\x00--harness\x00echo\x00")
		}
	}
	require.EventuallyWithT(t, func(ct *assert.CollectT) {
		data, err := os.ReadFile(filepath.Join(dir, "outbox.ndjson"))
		require.NoError(ct, err)
		var hello map[string]any
		for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
			var m map[string]any
			require.NoError(ct, json.Unmarshal([]byte(line), &m))
			if m["type"] == "hello" {
				hello = m
			}
		}
		require.NotNil(ct, hello, "no hello in the outbox yet")
		require.Equal(ct, "echo", hello["body"].(map[string]any)["harness"])
	}, 10*time.Second, 100*time.Millisecond)
}

// #97's acceptance: with max_beams 2 in the roster, a third `crew add` is
// refused with a cap error, and taken once a crew member is dismissed. The
// dismissal is written to the store directly until `bmus crew dismiss`
// (#100) exists.
func TestCrewAddBeamCap(t *testing.T) {
	t.Parallel()
	e := newE2E(t)
	require.NoError(t, os.WriteFile(filepath.Join(e.home, bridge.RosterFile), []byte("transporter:\n  max_beams: 2\n"), 0o600))
	e.startBridge(t)

	e.run(t, "crew", "add", "sorell", "--name", "Sorell")
	e.run(t, "crew", "add", "tuvok", "--name", "Tuvok")
	out, err := e.cmd(t.Context(), "crew", "add", "kirk", "--name", "Kirk").CombinedOutput()
	var exitErr *exec.ExitError
	require.ErrorAs(t, err, &exitErr, "%s", out)
	require.Equal(t, exitError, exitErr.ExitCode())
	require.Contains(t, string(out), "beam cap reached (transporter.max_beams 2)")

	st, err := store.Open(t.Context(), filepath.Join(e.home, store.DBFile))
	require.NoError(t, err)
	require.NoError(t, st.UpdateCrewStatus(t.Context(), "tuvok", store.CrewDismissed,
		store.LogEntry{Kind: "crew", CrewID: "tuvok", Text: "crew dismissed"}))
	require.NoError(t, st.Close())

	var added bridge.CrewView
	e.runJSON(t, &added, "crew", "add", "kirk", "--name", "Kirk")
	require.Equal(t, "kirk", added.ID)
}

// #64's acceptance: with --holodeck-script, crew add --harness holodeck is
// accepted.
func TestCrewAddHolodeckWithScript(t *testing.T) {
	t.Parallel()
	e := newE2E(t)
	e.startBridge(t)
	var added bridge.CrewView
	e.runJSON(t, &added, "crew", "add", "kirk", "--name", "Kirk", "--harness", "holodeck")
	require.Equal(t, "holodeck", added.Harness)
	require.EventuallyWithT(t, func(ct *assert.CollectT) {
		var crew bridge.CrewListResult
		e.runJSON(ct, &crew, "crew", "list")
		require.Len(ct, crew.Crew, 1)
		require.Equal(ct, store.CrewOnDeck, crew.Crew[0].Status)
	}, 30*time.Second, 200*time.Millisecond)
}

// #180: `bridge run --hail-park-after 2m` reaches serve's command line
// for an sdk crew member's beam (no --holodeck-script, so the crew
// member's harness runs).
func TestBridgeRunPassesHailParkAfter(t *testing.T) {
	t.Parallel()
	if _, err := os.Stat("/proc/self/cmdline"); err != nil {
		t.Skip("reads serve's command line from /proc")
	}
	e := newE2E(t)
	e.script = ""
	// sdk's startup check (#172) runs this claude, which exits 1: no model call.
	claude := filepath.Join(t.TempDir(), "claude")
	require.NoError(t, os.WriteFile(claude, []byte("#!/bin/sh\nexit 1\n"), 0o755))
	e.bridgeArgs = []string{"--hail-park-after", "2m", "--combadge-arg=--claude-path", "--combadge-arg=" + claude}
	e.startBridge(t)
	e.run(t, "crew", "add", "sorell", "--name", "Sorell", "--harness", "sdk")
	require.EventuallyWithT(t, func(ct *assert.CollectT) {
		c := e.crewMember(ct, "sorell")
		require.Equal(ct, store.CrewOnDeck, c.Status)
		require.NotEmpty(ct, c.CurrentBeam, "serve started")
	}, 30*time.Second, 200*time.Millisecond)
	_, pid := e.onlyBeam(t)
	cmdline, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	require.NoError(t, err)
	args := strings.Split(strings.TrimSuffix(string(cmdline), "\x00"), "\x00")
	i := slices.Index(args, "--hail-park-after")
	require.NotEqual(t, -1, i, "%q", args)
	require.Equal(t, "2m0s", args[i+1])
	require.Equal(t, "sdk", args[slices.Index(args, "--harness")+1])
}

// #59's acceptance: `crew list` shows each crew member's resolved harness
// and model with its source, and --json carries both sources.
func TestCrewListShowsSources(t *testing.T) {
	t.Parallel()
	e := newE2E(t)
	require.NoError(t, os.WriteFile(filepath.Join(e.home, bridge.RosterFile),
		[]byte("divisions:\n  ops: {harness: echo, model: m1}\n"), 0o600))
	e.startBridge(t)
	e.run(t, "crew", "add", "sorell", "--name", "Sorell", "--division", "ops")
	e.run(t, "crew", "add", "tuvok", "--name", "Tuvok", "--division", "ops", "--harness", "sdk", "--model", "m2")
	e.run(t, "crew", "add", "kirk", "--name", "Kirk")

	out := e.run(t, "crew", "list")
	require.Regexp(t, `(?m)^ID\s+NAME\s+RANK\s+DIVISION\s+HARNESS\s+MODEL\s+STATUS\s+BEAM\s+TOUR$`, out)
	require.Regexp(t, `(?m)^sorell\s+Sorell\s+-\s+ops\s+echo \(division\)\s+m1 \(division\)\s`, out)
	require.Regexp(t, `(?m)^tuvok\s+Tuvok\s+-\s+ops\s+sdk \(crew\)\s+m2 \(crew\)\s`, out)
	require.Regexp(t, `(?m)^kirk\s+Kirk\s+-\s+-\s+sdk \(default\)\s+- \(default\)\s`, out)

	var crew bridge.CrewListResult
	e.runJSON(t, &crew, "crew", "list")
	got := map[string][4]string{}
	for _, c := range crew.Crew {
		got[c.ID] = [4]string{c.Harness, c.HarnessSource, c.Model, c.ModelSource}
	}
	require.Equal(t, map[string][4]string{
		"sorell": {"echo", bridge.SourceDivision, "m1", bridge.SourceDivision},
		"tuvok":  {"sdk", bridge.SourceCrew, "m2", bridge.SourceCrew},
		"kirk":   {"", bridge.SourceDefault, "", bridge.SourceDefault},
	}, got)
}

// #51's acceptance: with a local repo registered, an order for main starts
// the harness inside <work-dir>/<order>/app, on branch bmus/<order> at
// main's commit. An order naming two repos fails with the Phase 1 error, and
// one naming an unregistered repo is refused too.
func TestOrderChecksOutRepo(t *testing.T) {
	t.Parallel()
	e := newE2E(t)
	stepLog := e.withStepLog(t)
	e.startBridge(t)
	f := e.follow(t)
	e.run(t, "crew", "add", "sorell", "--name", "Sorell")

	for repos, want := range map[string]string{
		"  - name: app\n    base_ref: main\n  - name: app\n    base_ref: main\n": "an order takes exactly one repo in Phase 1, got 2",
		"  - name: web\n    base_ref: main\n":                                    `unknown repo "web" (repos: app)`,
	} {
		out, err := e.cmd(t.Context(), "order", "create", orderYAML(t, repos)).CombinedOutput()
		var exitErr *exec.ExitError
		require.ErrorAs(t, err, &exitErr, "%s", out)
		require.Equal(t, exitError, exitErr.ExitCode(), "%s", out)
		require.Contains(t, string(out), want)
	}
	var orders bridge.OrderListResult
	e.runJSON(t, &orders, "order", "list")
	require.Empty(t, orders.Orders)

	main := gitT(t, e.repo, "rev-parse", "main")
	orderID := e.createOrder(t)
	f.until(t, 30*time.Second, textIs("order", "repo app shipped to the beam at "+main[:12]+" (main)"))
	f.until(t, 30*time.Second, textIs("hail", hailQuestion))
	f.stop(t)

	// The harness's first step ran with the checkout in place.
	alias, _ := e.onlyBeam(t)
	dir := filepath.Join(e.home, "holodeck", alias, "work", orderID, "app")
	var first struct {
		Order string            `json:"order"`
		Index int               `json:"index"`
		Repos map[string]string `json:"repos"`
	}
	data, err := os.ReadFile(stepLog)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal([]byte(strings.SplitN(string(data), "\n", 2)[0]), &first))
	require.Equal(t, orderID, first.Order)
	require.Equal(t, 0, first.Index)
	require.Equal(t, map[string]string{"app": dir}, first.Repos)
	require.Equal(t, "bmus/"+orderID, gitT(t, dir, "rev-parse", "--abbrev-ref", "HEAD"))
	require.Equal(t, main, gitT(t, dir, "rev-parse", "HEAD"))
	require.Equal(t, "2", gitT(t, dir, "rev-list", "--count", "HEAD"), "full history")
	require.Empty(t, gitT(t, dir, "status", "--porcelain"))
	// The bundle it came from is gone once cloned; the bridge's clone has the branch.
	_, err = os.Stat(filepath.Join(e.home, "holodeck", alias, "work", orderID, "in", "app.bundle"))
	require.ErrorIs(t, err, os.ErrNotExist)
	require.Equal(t, main, gitT(t, filepath.Join(e.home, "repos", "app"), "rev-parse", "bmus/"+orderID))

	e.runJSON(t, &orders, "order", "list")
	require.Len(t, orders.Orders, 1)
	require.Equal(t, []bridge.OrderRepo{{Name: "app", BaseRef: "main"}}, orders.Orders[0].Repos)
}

// #53's acceptance: the holodeck script commits on bmus/<order> and
// reports. The bridge pulls the report's bundle into the cargo bay as
// final/app.bundle and fetches it into its clone, where bmus/<order> is at
// the reported commit. The upstream, a local bare repo, is untouched until
// `bmus order push` (#99) pushes bmus/<order> to it; an order that isn't
// done is refused.
func TestWorkComesBackAsBundle(t *testing.T) {
	t.Parallel()
	e := newE2E(t)
	e.script = fixtureScript(t, "work.json")
	bare := filepath.Join(t.TempDir(), "app.git")
	gitT(t, filepath.Dir(bare), "clone", "-q", "--bare", e.repo, bare)
	e.repo = bare
	upRefs := gitT(t, bare, "for-each-ref", "--format=%(objectname) %(refname)")
	e.startBridge(t)
	f := e.follow(t)
	e.run(t, "crew", "add", "sorell", "--name", "Sorell")
	orderID := e.createOrder(t)

	got := f.until(t, 60*time.Second, func(le bridge.LogEntryView) bool { return le.Kind == "done" && le.OrderID == orderID })
	f.stop(t)
	done := got[len(got)-1]
	var payload struct {
		Summary string `json:"summary"`
		Repos   []struct {
			Name   string `json:"name"`
			Branch string `json:"branch"`
			Commit string `json:"commit"`
			Bundle string `json:"bundle_path"`
		} `json:"repos"`
	}
	require.NoError(t, json.Unmarshal(done.Payload, &payload))
	require.Equal(t, "holodeck work committed", payload.Summary)
	require.Len(t, payload.Repos, 1)
	r := payload.Repos[0]
	branch := "bmus/" + orderID
	require.Equal(t, "app", r.Name)
	require.Equal(t, branch, r.Branch)
	require.Equal(t, "done: holodeck work committed (app: "+branch+" at "+r.Commit[:12]+")", texts(got)[len(got)-1])

	// The commit is the beam's own: the checkout's bmus/<order>, two
	// commits past main.
	alias, _ := e.onlyBeam(t)
	checkout := filepath.Join(e.home, "holodeck", alias, "work", orderID, "app")
	require.Equal(t, r.Commit, gitT(t, checkout, "rev-parse", branch))
	require.Equal(t, gitT(t, bare, "rev-parse", "main"), gitT(t, checkout, "rev-parse", branch+"~2"))

	clone := filepath.Join(e.home, "repos", "app")
	require.Equal(t, r.Commit, gitT(t, e.home, "--git-dir", clone, "rev-parse", branch))
	cargo := filepath.Join(e.home, "cargo", orderID, "final", "app.bundle")
	require.Equal(t, cargo, r.Bundle)
	require.Equal(t, r.Commit+" refs/heads/"+branch, gitT(t, e.home, "bundle", "list-heads", cargo))
	require.Equal(t, upRefs, gitT(t, bare, "for-each-ref", "--format=%(objectname) %(refname)"), "no remote was touched")
	require.Equal(t, store.OrderDone, e.order(t, orderID).State)

	require.Equal(t, "order "+orderID+": repo app pushed to "+branch+" at "+r.Commit+"\n", e.run(t, "order", "push", orderID))
	require.Equal(t, r.Commit, gitT(t, bare, "rev-parse", branch))
	diverged := gitT(t, bare, "commit-tree", "-p", "main", "-m", "elsewhere", "main^{tree}")
	gitT(t, bare, "update-ref", "refs/heads/feature", diverged)
	stderr := e.fail(t, "order", "push", orderID, "--target-branch", "feature")
	require.Contains(t, stderr, "[rejected]")
	require.Contains(t, stderr, "--force overwrites it")
	require.Equal(t, "order "+orderID+": repo app pushed to feature at "+r.Commit+"\n",
		e.run(t, "order", "push", orderID, "--target-branch", "feature", "--force"))
	next := e.createOrder(t)
	require.Contains(t, e.fail(t, "order", "push", next), "only a done order's work can be pushed")
	require.Contains(t, e.fail(t, "order", "push", orderID, "--target-branch", "-x"), `target branch "-x" is not a valid branch name`)
}

// `log tail --follow` rides out a bridge restart: it keeps running while
// the bridge shuts down and is gone, and prints new entries once it is back.
func TestFollowSurvivesBridgeRestart(t *testing.T) {
	t.Parallel()
	e := newE2E(t)
	br := e.startBridge(t)
	f := e.follow(t)
	e.run(t, "crew", "add", "sorell", "--name", "Sorell")
	f.until(t, 30*time.Second, func(le bridge.LogEntryView) bool { return le.Kind == "beam" })

	br.sigterm(t)
	// Down for a few polls.
	time.Sleep(1500 * time.Millisecond)
	f.running(t)
	require.Contains(t, f.stderr.String(), "bridge not running; waiting for it")

	br = e.startBridge(t)
	f.running(t)
	require.Equal(t, "ORD-0001", e.createOrder(t))
	f.until(t, 30*time.Second, textIs("order", "order created: holodeck run"))
	f.stop(t)
	br.sigterm(t)
}

// `bridge run` refuses a home others can reach, with a hint, and leaves its
// mode alone (#40).
func TestBridgeRunRefusesOpenHome(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("builds and runs the bmus binary")
	}
	bin, err := buildBinary()
	require.NoError(t, err)
	// Not t.TempDir, whose path holds the test's name: Run checks the
	// socket path length before the home's mode, so keep the home short.
	home, err := os.MkdirTemp("", "bmus")
	require.NoError(t, err)
	t.Cleanup(func() { os.RemoveAll(home) })
	require.NoError(t, os.Chmod(home, 0o755))
	// Run checks the home before it touches the bundle.
	bundle := filepath.Join(t.TempDir(), "combadge.mjs")
	require.NoError(t, os.WriteFile(bundle, nil, 0o600))

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	out, err := cli{bin: bin, home: home}.cmd(ctx, "bridge", "run", "--combadge", bundle).CombinedOutput()
	var exitErr *exec.ExitError
	require.ErrorAs(t, err, &exitErr, "%s", out)
	require.Equal(t, exitError, exitErr.ExitCode(), "%s", out)
	require.Contains(t, string(out), "chmod 700 "+home)
	fi, err := os.Stat(home)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o755), fi.Mode().Perm())
}

// #96's acceptance: `bmus status` is red with "<order> red alert" while a
// red order is open, whatever its crew member is doing; a routine pool
// order doesn't count. With standing orders on, sorell, of ops, claims the
// red pool order first; once it is done the bridge is no longer red.
func TestStatusRedAlert(t *testing.T) {
	t.Parallel()
	e := newE2E(t)
	e.startBridge(t)
	dir := t.TempDir()
	for i, priority := range []string{"routine", "red"} {
		p := filepath.Join(dir, priority+".yaml")
		require.NoError(t, os.WriteFile(p, []byte(`title: `+priority+` run
division: ops
priority: `+priority+`
repos:
  - name: app
    base_ref: main
body: Walk the script.
`), 0o600))
		require.Equal(t, fmt.Sprintf("ORD-%04d\n", i+1), e.run(t, "order", "create", p))
	}
	out := e.run(t, "status")
	require.Contains(t, out, "red (ORD-0002 red alert)")
	require.Contains(t, out, "2 open, 0 away")

	e.run(t, "crew", "add", "sorell", "--name", "Sorell", "--division", "ops")
	require.Equal(t, "on\n", e.run(t, "standing-orders", "on"))
	var hail bridge.HailView
	require.EventuallyWithT(t, func(ct *assert.CollectT) {
		var hails bridge.HailsListResult
		e.runJSON(ct, &hails, "hails", "list", "--status", "open")
		require.Len(ct, hails.Hails, 1)
		hail = hails.Hails[0]
	}, 30*time.Second, 200*time.Millisecond)
	require.Equal(t, "ORD-0002", hail.OrderID, "the red order first")
	var st bridge.BridgeStatus
	e.runJSON(t, &st, "status")
	require.Equal(t, "red", st.Condition)
	require.Equal(t, []string{"ORD-0002 red alert"}, st.Reasons, "red outranks sorell hailing")

	e.run(t, "hails", "answer", hail.ID, "--option", "0")
	// sorell takes the routine order next, and hails on it.
	require.EventuallyWithT(t, func(ct *assert.CollectT) {
		var st bridge.BridgeStatus
		e.runJSON(ct, &st, "status")
		require.Equal(ct, "yellow", st.Condition)
		require.Equal(ct, []string{"sorell hailing"}, st.Reasons)
	}, 60*time.Second, 200*time.Millisecond)
	var orders bridge.OrderListResult
	e.runJSON(t, &orders, "order", "list")
	require.Equal(t, store.OrderDone, orders.Orders[1].State)
	require.Equal(t, "sorell", orders.Orders[0].Assignee)
}
