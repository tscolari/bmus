package transporter

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	// maxSocketPath bounds a beam's serve.sock path. sun_path holds 108 bytes
	// on Linux and 104 on macOS, NUL included; 100 leaves margin for both.
	maxSocketPath = 100

	// holodeckStartTimeout bounds how long StartCombadge waits for a new
	// serve to accept connections on its socket.
	holodeckStartTimeout = 10 * time.Second

	// holodeckStopTimeout is how long Remove waits after SIGTERM before
	// sending SIGKILL, and again after SIGKILL.
	holodeckStopTimeout = 3 * time.Second

	// holodeckLifetime is the default synthetic ExpiresAt offset
	// (HolodeckConfig.Lifetime), matching a real beam's ~24h lifetime.
	// Nothing enforces it.
	holodeckLifetime = 24 * time.Hour
)

// Files in a holodeck beam's directory, <Root>/<alias>/.
const (
	holodeckSocket = "serve.sock"
	holodeckOutbox = "outbox.ndjson" // serve keeps state.json and serve.lock beside it
	holodeckPID    = "serve.pid"
	holodeckLock   = "start.lock" // flock serialising StartCombadge, Remove and Kill
	holodeckLog    = "serve.log"  // serve's stdout and stderr
	holodeckWork   = "work"       // serve's --work-dir (WorkDir)
)

// HolodeckConfig configures a Holodeck.
type HolodeckConfig struct {
	// Root holds one directory per beam, e.g. <bmus home>/holodeck. It must
	// be absolute and short: each beam's socket, <Root>/<alias>/serve.sock,
	// must stay under 100 bytes.
	Root string
	// Combadge is the absolute path of the combadge bundle (combadge.mjs).
	Combadge string
	// Node is the node binary; "node" from $PATH when empty.
	Node string
	// Harness holds serve flags every beam gets, e.g. {"--harness",
	// "holodeck", "--script", "/abs/script.json"}. They come after
	// CombadgeSpec.Harness, so a --harness here overrides the crew member's
	// (a test override), and before CombadgeSpec.Args.
	Harness []string
	// Lifetime is how far out Create puts a beam's ExpiresAt; zero means
	// 24h. Nothing purges the beam then, but the bridge changes the watch
	// ahead of it, so tests shorten it to see a watch change (#100).
	Lifetime time.Duration
}

// Holodeck is a Provider whose beams are local (spec D10, plan §4). A beam is
// the directory <Root>/<alias>/, holding serve's socket (serve.sock), outbox
// (outbox.ndjson), pid file (serve.pid), log (serve.log) and work dir
// (work/).
//
// combadge serve runs as a detached local process in its own process group,
// so it outlives the bridge as a real beam would. Its pid is kept in the pid
// file, so Probe, Remove and Kill work from a Holodeck created after a bridge
// restart. While this process is serve's parent a goroutine reaps it, so a
// killed serve doesn't linger as a zombie.
//
// "Remote" paths are local paths: CopyTo and CopyFrom are plain file copies
// with paths taken as they are, since serve, being local, emits real absolute
// paths (e.g. in checkpoint_ready).
//
// StartCombadge, Remove and Kill on a beam hold an exclusive flock on its
// start.lock, so they are serialised across goroutines and across bridge
// processes sharing a Root.
//
// It works on Unix only.
type Holodeck struct {
	cfg HolodeckConfig
	// startTimeout bounds how long StartCombadge waits for serve's socket;
	// holodeckStartTimeout unless a test shortens it.
	startTimeout time.Duration

	mu  sync.Mutex
	seq int // last alias number minted
}

var _ Provider = (*Holodeck)(nil)

// NewHolodeck returns a Holodeck with cfg. Root and Combadge must be
// absolute.
func NewHolodeck(cfg HolodeckConfig) (*Holodeck, error) {
	if !filepath.IsAbs(cfg.Root) {
		return nil, fmt.Errorf("holodeck: Root must be an absolute path, got %q", cfg.Root)
	}
	if !filepath.IsAbs(cfg.Combadge) {
		return nil, fmt.Errorf("holodeck: Combadge must be an absolute path, got %q", cfg.Combadge)
	}
	if cfg.Node == "" {
		cfg.Node = "node"
	}
	// Probe matches serve by its socket path, so Root must read the same
	// across bridge restarts.
	cfg.Root = filepath.Clean(cfg.Root)
	cfg.Harness = append([]string(nil), cfg.Harness...)
	if cfg.Lifetime == 0 {
		cfg.Lifetime = holodeckLifetime
	}
	return &Holodeck{cfg: cfg, startTimeout: holodeckStartTimeout}, nil
}

// Create makes the beam directory <Root>/holodeck-<pid>-<n> and returns its
// alias, with ExpiresAt HolodeckConfig.Lifetime out. It starts nothing. It fails if the beam's
// socket path would be too long for a Unix socket.
func (h *Holodeck) Create(ctx context.Context) (Beam, error) {
	if err := ctx.Err(); err != nil {
		return Beam{}, err
	}
	if err := os.MkdirAll(h.cfg.Root, 0o700); err != nil {
		return Beam{}, fmt.Errorf("holodeck: create root: %w", err)
	}
	for {
		h.mu.Lock()
		h.seq++
		alias := fmt.Sprintf("holodeck-%d-%d", os.Getpid(), h.seq)
		h.mu.Unlock()
		dir := filepath.Join(h.cfg.Root, alias)
		if err := checkSocketPath(filepath.Join(dir, holodeckSocket)); err != nil {
			return Beam{}, err
		}
		// A pid reused across bridge restarts may meet an old beam's dir:
		// skip to the next number rather than share it.
		err := os.Mkdir(dir, 0o700)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		if err != nil {
			return Beam{}, fmt.Errorf("holodeck: create beam: %w", err)
		}
		return Beam{Alias: alias, ExpiresAt: time.Now().Add(h.cfg.Lifetime).UTC()}, nil
	}
}

func checkSocketPath(sock string) error {
	if len(sock) >= maxSocketPath {
		return fmt.Errorf("holodeck: socket path %s is %d bytes, too long for a Unix socket (limit %d): use a shorter Root",
			sock, len(sock), maxSocketPath-1)
	}
	return nil
}

// dir returns beam's directory. It rejects aliases that aren't a single path
// element, so a beam can't reach outside Root.
func (h *Holodeck) dir(beam Beam) (string, error) {
	a := beam.Alias
	if a == "" || a == "." || a == ".." || strings.ContainsAny(a, `/\`) {
		return "", fmt.Errorf("holodeck: invalid beam alias %q", a)
	}
	return filepath.Join(h.cfg.Root, a), nil
}

// existingDir is dir, failing if the beam doesn't exist.
func (h *Holodeck) existingDir(beam Beam) (string, error) {
	dir, err := h.dir(beam)
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(dir); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", fmt.Errorf("holodeck: unknown beam %s", beam.Alias)
		}
		return "", fmt.Errorf("holodeck: %w", err)
	}
	return dir, nil
}

// lockBeam takes an exclusive flock on dir's start.lock and returns its
// release. flock locks belong to the open file description, so this
// serialises goroutines of one process as well as separate processes. It
// fails with fs.ErrNotExist (wrapped) if the beam dir doesn't exist, or was
// removed while this waited for the lock.
func lockBeam(dir string) (func(), error) {
	f, err := os.OpenFile(filepath.Join(dir, holodeckLock), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	for {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
		if !errors.Is(err, syscall.EINTR) {
			break
		}
	}
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("flock %s: %w", f.Name(), err)
	}
	release := func() { f.Close() } // closing the last fd drops the lock
	// Remove may have deleted the beam while this waited.
	if _, err := os.Stat(dir); err != nil {
		release()
		return nil, err
	}
	return release, nil
}

// Exec runs name with args as a local child process, in beam's directory,
// with stdin and stdout piped to the returned Session; it shares that
// Session with Tsh.Exec. Cancelling ctx kills the process. Up to the last
// 4KiB of its stderr is included in Wait's error.
func (h *Holodeck) Exec(ctx context.Context, beam Beam, name string, args ...string) (Session, error) {
	dir, err := h.existingDir(beam)
	if err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Cancel = func() error { return cmd.Process.Kill() }
	cmd.WaitDelay = defaultWaitDelay
	return startSession(ctx, cmd, func(err error, stderr *tailBuffer) error {
		if ctxErr := ctx.Err(); ctxErr != nil {
			err = fmt.Errorf("%w (%w)", ctxErr, err)
		}
		if tail := stderr.String(); tail != "" {
			return fmt.Errorf("holodeck exec %s: %w: %s", name, err, tail)
		}
		return fmt.Errorf("holodeck exec %s: %w", name, err)
	})
}

// StartCombadge starts
//
//	<Node> <Combadge> serve --socket <dir>/serve.sock --crew <id> \
//	    --outbox <dir>/outbox.ndjson --work-dir <dir>/work [--harness <spec.Harness>] [--role <spec.Role>] \
//	    [--hail-park-after <spec.HailParkAfter>] <cfg.Harness...> <spec.Args...>
//
// in beam's directory, detached: it runs in its own process group with
// stdin from /dev/null and stdout and stderr appended to serve.log, and isn't
// tied to ctx, so it survives both ctx and the bridge. Its pid is written
// atomically to serve.pid. It returns once the socket accepts connections,
// or fails, with the log's tail, if serve exits first or doesn't accept
// within 10s (serve is then killed).
//
// It all happens under the beam's start.lock. If serve.pid names a live
// serve whose socket accepts, it returns nil without starting another. If
// that serve is alive but its socket doesn't accept, it waits for it as for
// a new one (it may still be starting). If the socket accepts but no live
// serve is recorded in serve.pid, it fails rather than report a serve it
// can't probe, kill or remove.
func (h *Holodeck) StartCombadge(ctx context.Context, beam Beam, spec CombadgeSpec) error {
	if spec.CrewID == "" {
		return errors.New("holodeck: StartCombadge: CrewID is required")
	}
	dir, err := h.existingDir(beam)
	if err != nil {
		return err
	}
	sock := filepath.Join(dir, holodeckSocket)
	if err := checkSocketPath(sock); err != nil {
		return err
	}
	unlock, err := lockBeam(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("holodeck: unknown beam %s", beam.Alias)
	}
	if err != nil {
		return fmt.Errorf("holodeck: %w", err)
	}
	defer unlock()

	pidFile := filepath.Join(dir, holodeckPID)
	logPath := filepath.Join(dir, holodeckLog)
	pid, err := readPID(pidFile)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err == nil && serveAlive(pid, sock) {
		// Running, or still starting (its starter may have died before the
		// socket came up). Not our child, so nothing here reaps it; once
		// dead it is reaped by whoever its parent is now.
		return h.awaitServe(ctx, pid, sock, logPath, nil, nil)
	}
	if socketAccepts(sock) {
		return fmt.Errorf("holodeck: %s accepts connections but %s names no live serve", sock, pidFile)
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	logFile, err := os.OpenFile(logPath, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("holodeck: open serve log: %w", err)
	}
	cmd := h.serveCommand(dir, spec)
	cmd.Stdout, cmd.Stderr = logFile, logFile
	err = cmd.Start()
	logFile.Close() // serve holds its own copy
	if err != nil {
		return fmt.Errorf("holodeck: start serve: %w", err)
	}
	pid = cmd.Process.Pid
	// Reap serve whenever it exits, so a killed serve isn't left a zombie
	// that kill(pid, 0) would still find.
	exited := make(chan struct{})
	var waitErr error
	go func() {
		waitErr = cmd.Wait()
		close(exited)
	}()
	if err := writeFileAtomic(pidFile, []byte(strconv.Itoa(pid)+"\n")); err != nil {
		select {
		case <-exited:
		default:
			killGroup(pid, syscall.SIGKILL)
		}
		return fmt.Errorf("holodeck: write serve pid file: %w", err)
	}
	return h.awaitServe(ctx, pid, sock, logPath, exited, &waitErr)
}

// awaitServe waits until serve (pid) accepts on sock. exited, if not nil, is
// closed once this process has reaped serve, with *waitErr its exit error;
// otherwise serve's liveness is polled with serveAlive. If serve dies first
// or startTimeout passes it fails with serve.log's tail, killing serve's
// group on timeout. If ctx ends first it returns ctx's error and leaves
// serve running: serve is meant to outlive its starter, and a replayed
// StartCombadge finds it through the pid file.
func (h *Holodeck) awaitServe(ctx context.Context, pid int, sock, logPath string, exited <-chan struct{}, waitErr *error) error {
	dead := func() bool {
		if exited == nil {
			return !serveAlive(pid, sock)
		}
		select {
		case <-exited:
			return true
		default:
			return false
		}
	}
	fail := func(format string, args ...any) error {
		msg := fmt.Sprintf(format, args...)
		if tail := logTail(logPath); tail != "" {
			msg += ": " + tail
		}
		return errors.New(msg)
	}
	exitedErr := func() error {
		if waitErr != nil {
			return fail("holodeck: combadge serve exited during startup (%v)", *waitErr)
		}
		return fail("holodeck: combadge serve (pid %d) exited during startup", pid)
	}
	deadline := time.NewTimer(h.startTimeout)
	defer deadline.Stop()
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	for {
		if dead() {
			return exitedErr()
		}
		if socketAccepts(sock) {
			return nil
		}
		select {
		case <-deadline.C:
			// dead() guards against signalling a reaped, possibly reused, pid.
			if !dead() {
				killGroup(pid, syscall.SIGKILL)
			}
			return fail("holodeck: combadge serve did not accept on %s within %s (killed)", sock, h.startTimeout)
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
		}
	}
}

// serveCommand builds the detached combadge serve command for beam dir.
func (h *Holodeck) serveCommand(dir string, spec CombadgeSpec) *exec.Cmd {
	args := []string{h.cfg.Combadge, "serve",
		"--socket", filepath.Join(dir, holodeckSocket),
		"--crew", spec.CrewID,
		"--outbox", filepath.Join(dir, holodeckOutbox),
		"--work-dir", filepath.Join(dir, holodeckWork),
	}
	args = append(args, spec.serveArgs(h.cfg.Harness)...)
	cmd := exec.Command(h.cfg.Node, args...)
	cmd.Dir = dir
	// Own process group: a signal to the bridge's group (Ctrl-C, a
	// supervisor stopping it) doesn't reach serve.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return cmd
}

// AttachCombadge runs `<Node> <Combadge> attach --socket <dir>/serve.sock
// --since <since>` through Exec: a plain child that dies with ctx (and with
// the bridge), unlike serve.
func (h *Holodeck) AttachCombadge(ctx context.Context, beam Beam, since uint64) (Session, error) {
	dir, err := h.dir(beam)
	if err != nil {
		return nil, err
	}
	return h.Exec(ctx, beam, h.cfg.Node, h.cfg.Combadge, "attach",
		"--socket", filepath.Join(dir, holodeckSocket), "--since", strconv.FormatUint(since, 10))
}

// Probe reports whether beam's serve is running, from its pid file:
// (false, nil) if the beam or its pid file doesn't exist or the process is
// gone. On Linux a zombie counts as gone, and so does a live process whose
// command line doesn't hold "serve" and this beam's socket path (the pid
// was reused); elsewhere it
// falls back to kill(pid, 0). An unreadable pid file is an error.
func (h *Holodeck) Probe(ctx context.Context, beam Beam) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	dir, err := h.dir(beam)
	if err != nil {
		return false, err
	}
	pid, err := readPID(filepath.Join(dir, holodeckPID))
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return serveAlive(pid, filepath.Join(dir, holodeckSocket)), nil
}

// Exists reports whether beam's directory exists.
func (h *Holodeck) Exists(ctx context.Context, beam Beam) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	dir, err := h.dir(beam)
	if err != nil {
		return false, err
	}
	_, err = os.Stat(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}

// CopyTo copies the local file localPath to remotePath, which is a local
// path too (see Holodeck), creating remotePath's parent directories. Both
// must be absolute.
func (h *Holodeck) CopyTo(ctx context.Context, beam Beam, localPath, remotePath string) error {
	return copyLocal(ctx, localPath, remotePath)
}

// LoginValidUntil returns the zero time: holodeck beams need no login.
func (h *Holodeck) LoginValidUntil(context.Context) (time.Time, error) { return time.Time{}, nil }

// WorkDir returns <Root>/<alias>/work, the --work-dir StartCombadge gives
// serve.
func (h *Holodeck) WorkDir(beam Beam) (string, error) {
	dir, err := h.dir(beam)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, holodeckWork), nil
}

// CopyFrom copies remotePath, a local path (see Holodeck), to localPath,
// creating localPath's parent directories. Both must be absolute.
func (h *Holodeck) CopyFrom(ctx context.Context, beam Beam, remotePath, localPath string) error {
	return copyLocal(ctx, remotePath, localPath)
}

// Size stats path, a local path (see Holodeck's doc comment: "remote"
// paths here are plain local paths, as CopyFrom takes them). beam is
// unused, like CopyFrom ignores it.
func (h *Holodeck) Size(ctx context.Context, beam Beam, path string) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if !filepath.IsAbs(path) {
		return 0, fmt.Errorf("holodeck: size %s: path must be absolute", path)
	}
	st, err := os.Stat(path)
	if err != nil {
		return 0, fmt.Errorf("holodeck: size %s: %w", path, err)
	}
	return st.Size(), nil
}

// Remove stops beam's serve, with SIGTERM to its process group and SIGKILL
// if it hasn't exited within 3s, then deletes the beam's directory
// (checkpoint files serve wrote under it included). Removing a beam that
// doesn't exist is not an error.
func (h *Holodeck) Remove(ctx context.Context, beam Beam) error {
	dir, err := h.dir(beam)
	if err != nil {
		return err
	}
	unlock, err := lockBeam(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil // already removed
	}
	if err != nil {
		return fmt.Errorf("holodeck: %w", err)
	}
	defer unlock()
	sock := filepath.Join(dir, holodeckSocket)
	pid, err := readPID(filepath.Join(dir, holodeckPID))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err == nil && serveAlive(pid, sock) {
		killGroup(pid, syscall.SIGTERM)
		if !waitGone(ctx, pid, sock, holodeckStopTimeout) {
			killGroup(pid, syscall.SIGKILL)
			if !waitGone(ctx, pid, sock, holodeckStopTimeout) {
				if err := ctx.Err(); err != nil {
					return err
				}
				return fmt.Errorf("holodeck: serve (pid %d) of %s survived SIGKILL", pid, beam.Alias)
			}
		}
		// serve is gone; take down anything left in its process group.
		_ = syscall.Kill(-pid, syscall.SIGKILL)
	}
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("holodeck: remove beam: %w", err)
	}
	return nil
}

// Kill SIGKILLs the serve process of the beam with alias, and its process
// group, to simulate the beam dying. It is not part of Provider: only the
// holodeck test harness calls it. It doesn't wait for the process to go;
// Probe reports false once it has. Killing a serve that is already gone is
// not an error.
func (h *Holodeck) Kill(alias string) error {
	dir, err := h.dir(Beam{Alias: alias})
	if err != nil {
		return err
	}
	unlock, err := lockBeam(dir)
	if err != nil {
		return fmt.Errorf("holodeck: kill %s: %w", alias, err)
	}
	defer unlock()
	pid, err := readPID(filepath.Join(dir, holodeckPID))
	if err != nil {
		return fmt.Errorf("holodeck: kill %s: %w", alias, err)
	}
	if serveAlive(pid, filepath.Join(dir, holodeckSocket)) {
		killGroup(pid, syscall.SIGKILL)
	}
	return nil
}

// killGroup sends sig to pid's process group (serve leads its own) and to
// pid itself, ignoring processes already gone.
func killGroup(pid int, sig syscall.Signal) {
	_ = syscall.Kill(-pid, sig)
	_ = syscall.Kill(pid, sig)
}

// waitGone polls until pid is no longer a live serve on sock, for up to
// timeout or until ctx ends. It reports whether the process went.
func waitGone(ctx context.Context, pid int, sock string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		if !serveAlive(pid, sock) {
			return true
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return false
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func readPID(file string) (int, error) {
	data, err := os.ReadFile(file)
	if err != nil {
		return 0, err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		return 0, fmt.Errorf("holodeck: bad pid file %s: %q", file, data)
	}
	return pid, nil
}

// procAvailable is whether /proc describes processes (Linux with procfs).
var procAvailable = sync.OnceValue(func() bool {
	if runtime.GOOS != "linux" {
		return false
	}
	_, err := os.Stat("/proc/self/stat")
	return err == nil
})

// serveAlive reports whether pid is a live combadge serve on sock. With
// /proc it treats a zombie (state Z, or X dead) as gone, since kill(pid, 0)
// succeeds on one until it is reaped, and requires the command line to hold
// both "serve" and sock as arguments, so a reused pid isn't mistaken for
// serve (the bundle's own path is configurable, so it isn't matched).
// Elsewhere it uses kill(pid, 0).
func serveAlive(pid int, sock string) bool {
	if pid <= 0 {
		return false
	}
	if !procAvailable() {
		err := syscall.Kill(pid, 0)
		return err == nil || errors.Is(err, syscall.EPERM)
	}
	switch procState(pid) {
	case 0, 'Z', 'X', 'x':
		return false
	}
	cmdline, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil {
		return false
	}
	args := strings.Split(string(cmdline), "\x00")
	return slices.Contains(args, "serve") && slices.Contains(args, sock)
}

// procState returns the state letter from /proc/<pid>/stat, or 0 if the
// process doesn't exist. The state follows the parenthesised command name,
// which may itself contain ") ", hence the last ')'.
func procState(pid int) byte {
	stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0
	}
	i := bytes.LastIndexByte(stat, ')')
	if i < 0 || i+2 >= len(stat) {
		return 0
	}
	return stat[i+2]
}

func socketAccepts(sock string) bool {
	c, err := net.DialTimeout("unix", sock, time.Second)
	if err != nil {
		return false
	}
	c.Close()
	return true
}

// logTail returns up to the last stderrTail bytes of file, trimmed.
func logTail(file string) string {
	f, err := os.Open(file)
	if err != nil {
		return ""
	}
	defer f.Close()
	if st, err := f.Stat(); err == nil && st.Size() > stderrTail {
		if _, err := f.Seek(-stderrTail, io.SeekEnd); err != nil {
			return ""
		}
	}
	data, _ := io.ReadAll(f)
	return strings.TrimSpace(string(data))
}

// writeFileAtomic writes data to a temp file beside file, then renames it
// over file, so readers see the old content or the new, never a torn write.
func writeFileAtomic(file string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(file), filepath.Base(file)+".*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // no-op once renamed
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), file)
}

// copyLocal copies src to dst through a temp file renamed into place,
// creating dst's directory and keeping src's permission bits.
func copyLocal(ctx context.Context, src, dst string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !filepath.IsAbs(src) || !filepath.IsAbs(dst) {
		return fmt.Errorf("holodeck: copy %s to %s: paths must be absolute", src, dst)
	}
	err := func() error {
		in, err := os.Open(src)
		if err != nil {
			return err
		}
		defer in.Close()
		st, err := in.Stat()
		if err != nil {
			return err
		}
		if !st.Mode().IsRegular() {
			return fmt.Errorf("%s is not a regular file", src)
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		tmp, err := os.CreateTemp(filepath.Dir(dst), filepath.Base(dst)+".*.tmp")
		if err != nil {
			return err
		}
		defer os.Remove(tmp.Name()) // no-op once renamed
		if _, err := io.Copy(tmp, in); err != nil {
			tmp.Close()
			return err
		}
		if err := tmp.Chmod(st.Mode().Perm()); err != nil {
			tmp.Close()
			return err
		}
		if err := tmp.Close(); err != nil {
			return err
		}
		return os.Rename(tmp.Name(), dst)
	}()
	if err != nil {
		return fmt.Errorf("holodeck: copy %s to %s: %w", src, dst, err)
	}
	return nil
}
