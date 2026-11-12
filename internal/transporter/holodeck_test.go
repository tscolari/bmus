package transporter

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/tscolari/bmus/internal/comms"
)

// The holodeck tests run the real combadge bundle. CI builds it and sets
// BMUS_COMBADGE, which makes a missing bundle or node a failure; locally the
// tests build it with `npm run build` and skip if they can't.

const (
	helperEnv     = "BMUS_HOLODECK_HELPER"
	helperRootEnv = "BMUS_HOLODECK_HELPER_ROOT"
	helperBinEnv  = "BMUS_HOLODECK_HELPER_COMBADGE"
	helperArgsEnv = "BMUS_HOLODECK_HELPER_HARNESS" // JSON array
)

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

// combadgeBundle returns the absolute path of the combadge bundle, from
// $BMUS_COMBADGE or by building it, skipping the test if neither works.
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
		t.Skipf("holodeck tests need node: %v (or set BMUS_COMBADGE in an environment with node)", err)
	}
	p, err := buildBundle()
	if err != nil {
		t.Skipf("holodeck tests need the combadge bundle: set BMUS_COMBADGE or make `npm run build` work: %v", err)
	}
	return p
}

// fixture returns the absolute path of a combadge holodeck script.
func fixture(t *testing.T, name string) string {
	t.Helper()
	p, err := filepath.Abs(filepath.Join("..", "..", "combadge", "test", "fixtures", name))
	require.NoError(t, err)
	return p
}

// testRoot makes a short Root (t.TempDir paths can push serve.sock past the
// Unix socket limit). Its cleanup SIGKILLs every serve whose pid file is
// under it, including ones started by the helper subprocess, and (with
// /proc) any other process serving a socket under it, then removes it.
func testRoot(t *testing.T) string {
	t.Helper()
	base := os.TempDir()
	if len(base) > 40 {
		base = "/tmp"
	}
	root, err := os.MkdirTemp(base, "hd")
	require.NoError(t, err)
	t.Cleanup(func() {
		dirs, _ := filepath.Glob(filepath.Join(root, "*"))
		for _, dir := range dirs {
			sock := filepath.Join(dir, holodeckSocket)
			pids := servesOn(sock)
			if pid, err := readPID(filepath.Join(dir, holodeckPID)); err == nil {
				pids = append(pids, pid)
			}
			for _, pid := range pids {
				if serveAlive(pid, sock) {
					killGroup(pid, syscall.SIGKILL)
					waitGone(context.Background(), pid, sock, 2*time.Second)
				}
			}
		}
		os.RemoveAll(root)
	})
	return root
}

func testConfig(t *testing.T, script string) HolodeckConfig {
	return HolodeckConfig{
		Root:     testRoot(t),
		Combadge: combadgeBundle(t),
		Harness:  []string{"--harness", "holodeck", "--script", fixture(t, script)},
	}
}

func newHolodeck(t *testing.T, cfg HolodeckConfig) *Holodeck {
	t.Helper()
	h, err := NewHolodeck(cfg)
	require.NoError(t, err)
	return h
}

// startBeam creates a beam and starts serve in it, returning serve's pid.
func startBeam(t *testing.T, h *Holodeck) (Beam, int) {
	t.Helper()
	beam, err := h.Create(t.Context())
	require.NoError(t, err)
	require.NoError(t, h.StartCombadge(t.Context(), beam, CombadgeSpec{CrewID: "crew-1"}))
	pid, err := readPID(filepath.Join(h.cfg.Root, beam.Alias, holodeckPID))
	require.NoError(t, err)
	return beam, pid
}

// TestHolodeckHelperProcess isn't a test: TestHolodeckServeOutlivesStarter
// re-runs the test binary with helperEnv set to make it create a beam, start
// serve and exit, printing "BEAM=<alias>".
func TestHolodeckHelperProcess(t *testing.T) {
	if os.Getenv(helperEnv) != "1" {
		return
	}
	var harness []string
	require.NoError(t, json.Unmarshal([]byte(os.Getenv(helperArgsEnv)), &harness))
	h := newHolodeck(t, HolodeckConfig{Root: os.Getenv(helperRootEnv), Combadge: os.Getenv(helperBinEnv), Harness: harness})
	beam, err := h.Create(t.Context())
	require.NoError(t, err)
	require.NoError(t, h.StartCombadge(t.Context(), beam, CombadgeSpec{CrewID: "crew-helper"}))
	fmt.Printf("BEAM=%s\n", beam.Alias)
}

// The issue's acceptance test: serve runs in its own process group and keeps
// running after the process that started it has exited.
func TestHolodeckServeOutlivesStarter(t *testing.T) {
	cfg := testConfig(t, "default.json")
	harness, err := json.Marshal(cfg.Harness)
	require.NoError(t, err)

	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestHolodeckHelperProcess$", "-test.count=1")
	cmd.Env = append(os.Environ(),
		helperEnv+"=1", helperRootEnv+"="+cfg.Root, helperBinEnv+"="+cfg.Combadge, helperArgsEnv+"="+string(harness))
	out, err := cmd.Output() // returns once the helper exits: serve doesn't hold its stdout
	require.NoError(t, err, "helper output: %s", out)
	var alias string
	for line := range strings.Lines(string(out)) {
		if a, ok := strings.CutPrefix(strings.TrimSpace(line), "BEAM="); ok {
			alias = a
		}
	}
	require.NotEmpty(t, alias, "helper output: %s", out)
	helperPID := cmd.ProcessState.Pid()

	pid, err := readPID(filepath.Join(cfg.Root, alias, holodeckPID))
	require.NoError(t, err)
	// The helper is gone; serve isn't.
	sock := filepath.Join(cfg.Root, alias, holodeckSocket)
	require.True(t, serveAlive(pid, sock), "serve (pid %d) died with its starter", pid)
	healthy, err := newHolodeck(t, cfg).Probe(t.Context(), Beam{Alias: alias})
	require.NoError(t, err)
	require.True(t, healthy)

	pgid, err := syscall.Getpgid(pid)
	require.NoError(t, err)
	require.Equal(t, pid, pgid, "serve should lead its own process group")
	require.NotEqual(t, syscall.Getpgrp(), pgid)
	if procAvailable() {
		ppid := procPPID(t, pid)
		require.NotEqual(t, os.Getpid(), ppid)
		require.NotEqual(t, helperPID, ppid)
	}

	// The command StartCombadge runs asks for that new process group.
	h := newHolodeck(t, cfg)
	c := h.serveCommand(filepath.Join(cfg.Root, alias), CombadgeSpec{CrewID: "c"})
	require.NotNil(t, c.SysProcAttr)
	require.True(t, c.SysProcAttr.Setpgid)

	require.NoError(t, h.Kill(alias))
	require.Eventually(t, func() bool { return !serveAlive(pid, sock) }, 2*time.Second, 20*time.Millisecond)
}

func procPPID(t *testing.T, pid int) int {
	t.Helper()
	stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	require.NoError(t, err)
	fields := strings.Fields(string(stat[strings.LastIndexByte(string(stat), ')')+1:]))
	ppid, err := strconv.Atoi(fields[1]) // state, ppid, ...
	require.NoError(t, err)
	return ppid
}

// beamReader decodes a Session's stdout in the background.
type beamReader struct {
	msgs chan comms.Message
	done chan error // the terminal read error
}

func readBeam(s Session) *beamReader {
	r := &beamReader{msgs: make(chan comms.Message, 64), done: make(chan error, 1)}
	go func() {
		defer close(r.msgs)
		sc := bufio.NewScanner(s.Stdout())
		for sc.Scan() {
			var m comms.Message
			if err := json.Unmarshal(sc.Bytes(), &m); err == nil {
				r.msgs <- m
			}
		}
		r.done <- sc.Err()
	}()
	return r
}

// next returns the next message of type typ, skipping others (heartbeats).
func (r *beamReader) next(t *testing.T, typ string) comms.Message {
	t.Helper()
	timeout := time.After(10 * time.Second)
	for {
		select {
		case m, ok := <-r.msgs:
			require.True(t, ok, "stdout ended waiting for %s", typ)
			if m.Type == typ {
				return m
			}
		case <-timeout:
			t.Fatalf("no %s within 10s", typ)
		}
	}
}

func send(t *testing.T, s Session, seq uint64, typ string, body any) {
	t.Helper()
	m, err := comms.NewMessage(typ, body)
	require.NoError(t, err)
	m.Seq, m.Crew, m.Order = seq, "crew-1", "ORD-0001"
	line, err := json.Marshal(m)
	require.NoError(t, err)
	_, err = s.Write(append(line, '\n'))
	require.NoError(t, err)
}

// Attach round trip through the real serve, running default.json to its
// report, with the checkpoint pulled by CopyFrom.
func TestHolodeckAttachRoundTrip(t *testing.T) {
	h := newHolodeck(t, testConfig(t, "default.json"))
	beam, _ := startBeam(t, h)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	s, err := h.AttachCombadge(ctx, beam, 0)
	require.NoError(t, err)
	r := readBeam(s)
	r.next(t, comms.TypeHello)

	send(t, s, 1, comms.TypeOrder, comms.OrderBody{Title: "t", Body: "b"})
	progress := r.next(t, comms.TypeProgress)
	require.Equal(t, "ORD-0001", progress.Order)

	hail, err := r.next(t, comms.TypeHail).DecodeBody()
	require.NoError(t, err)
	allow := 0 // index of comms.OptionAllow
	send(t, s, 2, comms.TypeAnswer, comms.AnswerBody{
		Ref: hail.(comms.HailBody).Ref, HailID: "H-1", DecidedBy: comms.DecidedByCaptain, Decision: comms.OptionAllow, Option: &allow,
	})

	body, err := r.next(t, comms.TypeCheckpointReady).DecodeBody()
	require.NoError(t, err)
	cp := body.(comms.CheckpointReadyBody)
	require.Empty(t, cp.Repos, "an order with no repos has no bundles")
	require.True(t, filepath.IsAbs(cp.HandoverPath), cp.HandoverPath)
	cargo := filepath.Join(t.TempDir(), "cargo", "ORD-0001", "1")
	for _, p := range []string{cp.TranscriptPath, cp.HandoverPath} {
		require.NoError(t, h.CopyFrom(t.Context(), beam, p, filepath.Join(cargo, filepath.Base(p))))
	}
	note, err := os.ReadFile(filepath.Join(cargo, filepath.Base(cp.HandoverPath)))
	require.NoError(t, err)
	require.Equal(t, "mid-mission checkpoint\n", string(note))

	r.next(t, comms.TypeReport)

	// Cancelling the ctx ends the attach relay, not serve.
	cancel()
	select {
	case <-r.done:
	case <-time.After(5 * time.Second):
		t.Fatal("stdout reader not unblocked by ctx cancel")
	}
	waited := make(chan error, 1)
	go func() { waited <- s.Wait() }()
	select {
	case err := <-waited:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(5 * time.Second):
		t.Fatal("Wait didn't return after ctx cancel")
	}
	_, err = s.Stdout().Read(make([]byte, 1))
	require.ErrorIs(t, err, context.Canceled)
	healthy, err := h.Probe(t.Context(), beam)
	require.NoError(t, err)
	require.True(t, healthy)
}

// serve runs CombadgeSpec.Harness, unless the holodeck's configured harness
// flags name another: they come later on serve's command line, and serve
// takes the last --harness.
func TestHolodeckHarnessPrecedence(t *testing.T) {
	for name, tc := range map[string]struct {
		cfg  []string
		spec string
		want string
	}{
		"crew harness":         {spec: "echo", want: "echo"},
		"configured flags win": {cfg: []string{"--harness", "holodeck", "--script", fixture(t, "default.json")}, spec: "sdk", want: "holodeck"},
		"no crew harness":      {cfg: []string{"--harness", "echo"}, want: "echo"},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := testConfig(t, "default.json")
			cfg.Harness = tc.cfg
			h := newHolodeck(t, cfg)
			beam, err := h.Create(t.Context())
			require.NoError(t, err)
			require.NoError(t, h.StartCombadge(t.Context(), beam, CombadgeSpec{CrewID: "crew-1", Harness: tc.spec}))
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			s, err := h.AttachCombadge(ctx, beam, 0)
			require.NoError(t, err)
			body, err := readBeam(s).next(t, comms.TypeHello).DecodeBody()
			require.NoError(t, err)
			require.Equal(t, tc.want, body.(comms.HelloBody).Harness)
		})
	}
}

func TestHolodeckStartCombadgeIdempotent(t *testing.T) {
	cfg := testConfig(t, "default.json")
	h := newHolodeck(t, cfg)
	beam, pid := startBeam(t, h)
	require.NoError(t, h.StartCombadge(t.Context(), beam, CombadgeSpec{CrewID: "crew-1"}))
	// As after a bridge restart: no in-memory state, only the pid file.
	require.NoError(t, newHolodeck(t, cfg).StartCombadge(t.Context(), beam, CombadgeSpec{CrewID: "crew-1"}))
	pid2, err := readPID(filepath.Join(cfg.Root, beam.Alias, holodeckPID))
	require.NoError(t, err)
	require.Equal(t, pid, pid2)
	if procAvailable() {
		require.Equal(t, []int{pid}, servesOn(filepath.Join(cfg.Root, beam.Alias, holodeckSocket)))
	}
}

// servesOn lists the pids of live serves on sock (none without /proc).
func servesOn(sock string) []int {
	if !procAvailable() {
		return nil
	}
	ents, _ := os.ReadDir("/proc")
	var pids []int
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		if serveAlive(pid, sock) {
			pids = append(pids, pid)
		}
	}
	return pids
}

func TestHolodeckKill(t *testing.T) {
	h := newHolodeck(t, testConfig(t, "default.json"))
	beam, pid := startBeam(t, h)
	healthy, err := h.Probe(t.Context(), beam)
	require.NoError(t, err)
	require.True(t, healthy)

	require.NoError(t, h.Kill(beam.Alias))
	require.Eventually(t, func() bool {
		healthy, err := h.Probe(t.Context(), beam)
		return err == nil && !healthy
	}, time.Second, 10*time.Millisecond)
	if procAvailable() {
		// Reaped, not left a zombie.
		require.Eventually(t, func() bool { return procState(pid) == 0 }, time.Second, 10*time.Millisecond)
	}
	require.NoError(t, h.Kill(beam.Alias), "killing a dead serve")
}

func TestHolodeckProbeAndRemoveAfterRestart(t *testing.T) {
	cfg := testConfig(t, "default.json")
	beam, pid := startBeam(t, newHolodeck(t, cfg))

	h := newHolodeck(t, cfg) // a restarted bridge
	healthy, err := h.Probe(t.Context(), beam)
	require.NoError(t, err)
	require.True(t, healthy)

	require.NoError(t, h.Remove(t.Context(), beam))
	require.False(t, serveAlive(pid, filepath.Join(cfg.Root, beam.Alias, holodeckSocket)))
	require.NoDirExists(t, filepath.Join(cfg.Root, beam.Alias))
	healthy, err = h.Probe(t.Context(), beam)
	require.NoError(t, err)
	require.False(t, healthy)
	require.NoError(t, h.Remove(t.Context(), beam), "removing a removed beam")
}

func TestHolodeckProbeUnknownBeam(t *testing.T) {
	h := newHolodeck(t, HolodeckConfig{Root: t.TempDir(), Combadge: "/x/combadge.mjs"})
	healthy, err := h.Probe(t.Context(), Beam{Alias: "holodeck-1-1"})
	require.NoError(t, err)
	require.False(t, healthy)

	exists, err := h.Exists(t.Context(), Beam{Alias: "holodeck-1-1"})
	require.NoError(t, err)
	require.False(t, exists)

	beam, err := h.Create(t.Context()) // created, serve never started
	require.NoError(t, err)
	healthy, err = h.Probe(t.Context(), beam)
	require.NoError(t, err)
	require.False(t, healthy)
	exists, err = h.Exists(t.Context(), beam)
	require.NoError(t, err)
	require.True(t, exists, "a beam without serve still exists")
	require.NoError(t, h.Remove(t.Context(), beam))
	exists, err = h.Exists(t.Context(), beam)
	require.NoError(t, err)
	require.False(t, exists)

	_, err = h.Probe(t.Context(), Beam{Alias: "../etc"})
	require.ErrorContains(t, err, "invalid beam alias")
}

func TestHolodeckStartFailureIncludesLog(t *testing.T) {
	cfg := testConfig(t, "default.json")
	cfg.Harness = []string{"--harness", "holodeck", "--script", filepath.Join(cfg.Root, "missing.json")}
	h := newHolodeck(t, cfg)
	beam, err := h.Create(t.Context())
	require.NoError(t, err)
	err = h.StartCombadge(t.Context(), beam, CombadgeSpec{CrewID: "crew-1"})
	require.ErrorContains(t, err, "combadge serve exited during startup")
	require.ErrorContains(t, err, "missing.json")
}

func TestHolodeckConfigAndCreate(t *testing.T) {
	_, err := NewHolodeck(HolodeckConfig{Root: "rel", Combadge: "/x"})
	require.ErrorContains(t, err, "Root must be an absolute path")
	_, err = NewHolodeck(HolodeckConfig{Root: "/x", Combadge: "rel"})
	require.ErrorContains(t, err, "Combadge must be an absolute path")

	root := t.TempDir()
	h := newHolodeck(t, HolodeckConfig{Root: root, Combadge: "/x/combadge.mjs"})
	require.Equal(t, "node", h.cfg.Node)
	a, err := h.Create(t.Context())
	require.NoError(t, err)
	b, err := h.Create(t.Context())
	require.NoError(t, err)
	require.NotEqual(t, a.Alias, b.Alias)
	require.DirExists(t, filepath.Join(root, a.Alias))
	require.WithinDuration(t, time.Now().Add(24*time.Hour), a.ExpiresAt, time.Minute)

	long := newHolodeck(t, HolodeckConfig{Root: filepath.Join(root, strings.Repeat("x", 100)), Combadge: "/x/combadge.mjs"})
	_, err = long.Create(t.Context())
	require.ErrorContains(t, err, "too long for a Unix socket")

	require.ErrorContains(t, h.StartCombadge(t.Context(), Beam{Alias: "holodeck-0-0"}, CombadgeSpec{CrewID: "c"}), "unknown beam")
}

func TestHolodeckExecRunsLocallyInBeamDir(t *testing.T) {
	root := t.TempDir()
	h := newHolodeck(t, HolodeckConfig{Root: root, Combadge: "/x/combadge.mjs"})
	beam, err := h.Create(t.Context())
	require.NoError(t, err)
	s, err := h.Exec(t.Context(), beam, "sh", "-c", `pwd; printf '%s|' "$0"`, "$HOME")
	require.NoError(t, err)
	out, err := io.ReadAll(s.Stdout())
	require.NoError(t, err)
	require.NoError(t, s.Wait())
	dir, err := filepath.EvalSymlinks(filepath.Join(root, beam.Alias))
	require.NoError(t, err)
	pwd, rest, ok := strings.Cut(string(out), "\n")
	require.True(t, ok, "missing pwd line: %q", out)
	pwd, err = filepath.EvalSymlinks(pwd)
	require.NoError(t, err)
	require.Equal(t, dir, pwd, "exec ran outside the beam dir")
	require.Equal(t, "$HOME|", rest)

	s, err = h.Exec(t.Context(), beam, "sh", "-c", "echo boom >&2; exit 3")
	require.NoError(t, err)
	require.ErrorContains(t, s.Wait(), "holodeck exec sh: exit status 3: boom")
}

func TestHolodeckCopy(t *testing.T) {
	h := newHolodeck(t, HolodeckConfig{Root: t.TempDir(), Combadge: "/x/combadge.mjs"})
	src := filepath.Join(t.TempDir(), "combadge.mjs")
	require.NoError(t, os.WriteFile(src, []byte("bundle"), 0o640))
	dst := filepath.Join(t.TempDir(), "a", "b", "combadge.mjs")
	require.NoError(t, h.CopyTo(t.Context(), orbit, src, dst))
	got, err := os.ReadFile(dst)
	require.NoError(t, err)
	require.Equal(t, "bundle", string(got))
	st, err := os.Stat(dst)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o640), st.Mode().Perm())

	back := filepath.Join(t.TempDir(), "x", "back")
	require.NoError(t, h.CopyFrom(t.Context(), orbit, dst, back))
	got, err = os.ReadFile(back)
	require.NoError(t, err)
	require.Equal(t, "bundle", string(got))

	require.ErrorContains(t, h.CopyFrom(t.Context(), orbit, "rel", back), "paths must be absolute")
	require.ErrorContains(t, h.CopyFrom(t.Context(), orbit, filepath.Join(t.TempDir(), "nope"), back), "no such file")
}

// #65: pull-work and pull-checkpoint check a bundle's size before copying
// it; Size stats the local path directly, no copy involved.
func TestHolodeckSize(t *testing.T) {
	h := newHolodeck(t, HolodeckConfig{Root: t.TempDir(), Combadge: "/x/combadge.mjs"})
	path := filepath.Join(t.TempDir(), "app.bundle")
	require.NoError(t, os.WriteFile(path, []byte("bundle contents"), 0o640))

	n, err := h.Size(t.Context(), orbit, path)
	require.NoError(t, err)
	require.Equal(t, int64(len("bundle contents")), n)

	_, err = h.Size(t.Context(), orbit, "rel")
	require.ErrorContains(t, err, "path must be absolute")

	_, err = h.Size(t.Context(), orbit, filepath.Join(t.TempDir(), "missing.bundle"))
	require.Error(t, err)
}

// Two bridges sharing a Root (here two Holodecks; start.lock is a flock, which
// excludes separate opens even within one process) racing to start serve on
// one beam end up with exactly one serve, recorded in serve.pid.
func TestHolodeckConcurrentStartAcrossHolodecks(t *testing.T) {
	cfg := testConfig(t, "default.json")
	hs := []*Holodeck{newHolodeck(t, cfg), newHolodeck(t, cfg)}
	for round := range 10 {
		beam, err := hs[0].Create(t.Context())
		require.NoError(t, err)
		errs := make(chan error, len(hs))
		for _, h := range hs {
			go func() { errs <- h.StartCombadge(t.Context(), beam, CombadgeSpec{CrewID: "crew-1"}) }()
		}
		for range hs {
			require.NoError(t, <-errs, "round %d", round)
		}
		assertOneServe(t, hs[1], beam)
		require.NoError(t, hs[1].Remove(t.Context(), beam))
	}
}

func TestHolodeckConcurrentStartOneHolodeck(t *testing.T) {
	h := newHolodeck(t, testConfig(t, "default.json"))
	beam, err := h.Create(t.Context())
	require.NoError(t, err)
	errs := make(chan error, 4)
	for range cap(errs) {
		go func() { errs <- h.StartCombadge(t.Context(), beam, CombadgeSpec{CrewID: "crew-1"}) }()
	}
	for range cap(errs) {
		require.NoError(t, <-errs)
	}
	assertOneServe(t, h, beam)
}

// assertOneServe checks beam is healthy and, with /proc, that the only serve
// on its socket is the one in serve.pid.
func assertOneServe(t *testing.T, h *Holodeck, beam Beam) {
	t.Helper()
	healthy, err := h.Probe(t.Context(), beam)
	require.NoError(t, err)
	require.True(t, healthy)
	if procAvailable() {
		pid, err := readPID(filepath.Join(h.cfg.Root, beam.Alias, holodeckPID))
		require.NoError(t, err)
		require.Equal(t, []int{pid}, servesOn(filepath.Join(h.cfg.Root, beam.Alias, holodeckSocket)))
	}
}

// fakeNode writes a stand-in for node that logs a line and then idles
// without ever opening the socket. Its command line keeps the serve args,
// so serveAlive recognises it.
func fakeNode(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "fake-node")
	require.NoError(t, os.WriteFile(bin, []byte("#!/bin/sh\necho 'fake serve: never listening' >&2\nwhile :; do sleep 1; done\n"), 0o755))
	return bin
}

func TestHolodeckStartTimeoutKillsServe(t *testing.T) {
	root := testRoot(t)
	h := newHolodeck(t, HolodeckConfig{Root: root, Combadge: "/x/combadge.mjs", Node: fakeNode(t)})
	h.startTimeout = 300 * time.Millisecond
	beam, err := h.Create(t.Context())
	require.NoError(t, err)
	err = h.StartCombadge(t.Context(), beam, CombadgeSpec{CrewID: "crew-1"})
	require.ErrorContains(t, err, "did not accept")
	require.ErrorContains(t, err, "fake serve: never listening")
	pid, err := readPID(filepath.Join(root, beam.Alias, holodeckPID))
	require.NoError(t, err)
	sock := filepath.Join(root, beam.Alias, holodeckSocket)
	require.Eventually(t, func() bool { return !serveAlive(pid, sock) }, 2*time.Second, 20*time.Millisecond)
	require.Eventually(t, func() bool { return syscall.Kill(-pid, 0) != nil }, 2*time.Second, 20*time.Millisecond,
		"serve's process group (the shell's sleep) should be gone too")
}

// A serve recorded in serve.pid that is alive but not accepting (its starter
// died mid-startup) is waited for, not reported healthy, and killed on
// timeout.
func TestHolodeckStartWaitsForRecordedServe(t *testing.T) {
	root := testRoot(t)
	h := newHolodeck(t, HolodeckConfig{Root: root, Combadge: "/x/combadge.mjs", Node: fakeNode(t)})
	h.startTimeout = 300 * time.Millisecond
	beam, err := h.Create(t.Context())
	require.NoError(t, err)
	dir := filepath.Join(root, beam.Alias)
	orphan := h.serveCommand(dir, CombadgeSpec{CrewID: "crew-1"})
	require.NoError(t, orphan.Start())
	go orphan.Wait()
	pid := orphan.Process.Pid
	require.NoError(t, writeFileAtomic(filepath.Join(dir, holodeckPID), []byte(strconv.Itoa(pid)+"\n")))
	sock := filepath.Join(dir, holodeckSocket)
	require.Eventually(t, func() bool { return serveAlive(pid, sock) }, 2*time.Second, 10*time.Millisecond)

	err = h.StartCombadge(t.Context(), beam, CombadgeSpec{CrewID: "crew-1"})
	require.ErrorContains(t, err, "did not accept")
	require.Eventually(t, func() bool { return !serveAlive(pid, sock) }, 2*time.Second, 20*time.Millisecond)
}

func TestHolodeckStartRefusesUnrecordedSocket(t *testing.T) {
	root := testRoot(t)
	h := newHolodeck(t, HolodeckConfig{Root: root, Combadge: "/x/combadge.mjs", Node: fakeNode(t)})
	beam, err := h.Create(t.Context())
	require.NoError(t, err)
	l, err := net.Listen("unix", filepath.Join(root, beam.Alias, holodeckSocket))
	require.NoError(t, err)
	defer l.Close()
	err = h.StartCombadge(t.Context(), beam, CombadgeSpec{CrewID: "crew-1"})
	require.ErrorContains(t, err, "names no live serve")
	require.NoFileExists(t, filepath.Join(root, beam.Alias, holodeckPID))
}

func TestHolodeckCreateSkipsExistingDir(t *testing.T) {
	root := t.TempDir()
	h := newHolodeck(t, HolodeckConfig{Root: root, Combadge: "/x/combadge.mjs"})
	taken := fmt.Sprintf("holodeck-%d-1", os.Getpid())
	require.NoError(t, os.Mkdir(filepath.Join(root, taken), 0o700))
	beam, err := h.Create(t.Context())
	require.NoError(t, err)
	require.Equal(t, fmt.Sprintf("holodeck-%d-2", os.Getpid()), beam.Alias)
}

// serve gets --role after --harness, before the configured harness flags
// and the spec's args (#155).
func TestHolodeckServeRole(t *testing.T) {
	cfg := testConfig(t, "default.json")
	cfg.Harness = []string{"--harness", "holodeck"}
	h := newHolodeck(t, cfg)
	c := h.serveCommand("/beam", CombadgeSpec{CrewID: "vance", Harness: "sdk", Role: "command", Args: []string{"--x"}})
	require.Equal(t, []string{"--harness", "sdk", "--role", "command", "--harness", "holodeck", "--x"}, c.Args[len(c.Args)-7:])
	c = h.serveCommand("/beam", CombadgeSpec{CrewID: "sorell"})
	require.NotContains(t, c.Args, "--role")
}

// --hail-park-after goes to serve only when it is set and serve ends up
// running sdk: serve refuses it with any other harness (#180).
func TestServeArgsHailParkAfter(t *testing.T) {
	park := []string{"--hail-park-after", "2m0s"}
	for name, tc := range map[string]struct {
		spec       CombadgeSpec
		configured []string
		want       []string
	}{
		"sdk crew": {spec: CombadgeSpec{Harness: "sdk", HailParkAfter: 2 * time.Minute},
			want: append([]string{"--harness", "sdk"}, park...)},
		"default harness": {spec: CombadgeSpec{HailParkAfter: 2 * time.Minute}, want: park},
		"unset":           {spec: CombadgeSpec{Harness: "sdk"}, want: []string{"--harness", "sdk"}},
		"echo crew": {spec: CombadgeSpec{Harness: "echo", HailParkAfter: 2 * time.Minute},
			want: []string{"--harness", "echo"}},
		"holodeck crew": {spec: CombadgeSpec{Harness: "holodeck", HailParkAfter: 2 * time.Minute},
			want: []string{"--harness", "holodeck"}},
		"configured harness overrides sdk": {spec: CombadgeSpec{Harness: "sdk", HailParkAfter: time.Minute},
			configured: []string{"--harness", "holodeck", "--script", "s.json"},
			want:       []string{"--harness", "sdk", "--harness", "holodeck", "--script", "s.json"}},
		"args override sdk": {spec: CombadgeSpec{HailParkAfter: time.Minute, Args: []string{"--harness=echo"}},
			want: []string{"--harness=echo"}},
		"args restore sdk": {spec: CombadgeSpec{Harness: "echo", HailParkAfter: 90 * time.Minute, Args: []string{"--harness", "sdk"}},
			want: []string{"--harness", "echo", "--hail-park-after", "1h30m0s", "--harness", "sdk"}},
		"role and sub-second": {spec: CombadgeSpec{Role: "command", HailParkAfter: 1500 * time.Millisecond, Args: []string{"--x"}},
			want: []string{"--role", "command", "--hail-park-after", "1.5s", "--x"}},
	} {
		t.Run(name, func(t *testing.T) {
			require.Equal(t, tc.want, tc.spec.serveArgs(tc.configured))
		})
	}
}

// A holodeck serve running sdk takes --hail-park-after 2m0s (serve refuses
// a duration it can't parse at startup); one running echo gets no flag.
// sdk's startup check (#172) runs the claude first on PATH: a fake one that
// exits 1, so the check is inconclusive and makes no model call.
func TestHolodeckHailParkAfter(t *testing.T) {
	bin := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(bin, "claude"), []byte("#!/bin/sh\nexit 1\n"), 0o755))
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	cfg := testConfig(t, "default.json")
	cfg.Harness = nil
	h := newHolodeck(t, cfg)
	for harness, want := range map[string]bool{"sdk": true, "echo": false} {
		beam, err := h.Create(t.Context())
		require.NoError(t, err)
		spec := CombadgeSpec{CrewID: "crew-1", Harness: harness, HailParkAfter: 2 * time.Minute}
		require.NoError(t, h.StartCombadge(t.Context(), beam, spec), harness)
		ctx, cancel := context.WithCancel(t.Context())
		s, err := h.AttachCombadge(ctx, beam, 0)
		require.NoError(t, err)
		body, err := readBeam(s).next(t, comms.TypeHello).DecodeBody()
		cancel()
		require.NoError(t, err)
		require.Equal(t, harness, body.(comms.HelloBody).Harness)
		if !procAvailable() {
			continue
		}
		pid, err := readPID(filepath.Join(cfg.Root, beam.Alias, holodeckPID))
		require.NoError(t, err)
		cmdline, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
		require.NoError(t, err)
		args := strings.Split(strings.TrimSuffix(string(cmdline), "\x00"), "\x00")
		if want {
			require.Equal(t, []string{"--harness", "sdk", "--hail-park-after", "2m0s"}, args[len(args)-4:])
		} else {
			require.NotContains(t, args, "--hail-park-after")
		}
	}
}
