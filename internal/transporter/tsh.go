package transporter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	// DefaultProbeTimeout bounds each of Probe's `tsh beams exec <alias> -- true`
	// and `tsh beams exec <alias> -- beamctl list`.
	DefaultProbeTimeout = 5 * time.Second

	// defaultWaitDelay is how long os/exec waits, after tsh (or a holodeck
	// Exec process) exits or is killed, for the stderr (and run's stdout) copy
	// goroutines before closing their pipes, so a grandchild holding them
	// can't block Wait forever. Exec's stdout is an *os.File with no copy
	// goroutine, so it is never cut short.
	defaultWaitDelay = 5 * time.Second

	// stderrTail is how much of tsh's stderr is kept for error messages.
	stderrTail = 4 << 10

	// defaultLoginCheckEvery is how long Tsh trusts the login state it last
	// read from `tsh status` before reading it again (#102).
	defaultLoginCheckEvery = time.Minute

	// loginStatusTimeout bounds one `tsh status`.
	loginStatusTimeout = 10 * time.Second
)

// tshBeam is the JSON shape of a beam in `tsh beams add --format=json` (one
// object) and `tsh beams ls --format=json` (an array of them). It mirrors
// formattedBeam in gravitational/teleport tool/tsh/common/beams.go, which
// also carries uuid, owner, url and region fields we don't need. A real tsh
// matched them on #19.
type tshBeam struct {
	ID      string    `json:"id"`      // status.alias, e.g. "warm-orbit"
	Expires time.Time `json:"expires"` // spec.expires, RFC 3339
}

// Tsh is a Provider that shells out to the tsh CLI (spec D8). It relies on
// the captain's existing tsh login; it never logs in itself.
//
// Every `tsh beams` call passes a login gate first (#102). Tsh keeps the
// login's expiry as `tsh status` last reported it, read again once it is
// loginCheckEvery old and right after a call fails. While the login is
// expired (from LoginExpiryMargin before its valid_until), or the captain
// isn't logged in, a call returns an error wrapping
// ErrLoginExpired without starting tsh: an expired login makes tsh try to
// log in again, which can prompt or open a browser. A call that fails and
// then finds the login expired returns ErrLoginExpired too.
//
// tsh never gets the bridge's terminal: it runs in its own session, with no
// controlling terminal, and its stdin is /dev/null except for Exec's pipe.
// tsh reads a password or OTP only from a terminal on stdin; without one,
// its re-login gives up and the call fails with the original error.
type Tsh struct {
	bin string
	// Combadge is the local path of the combadge bundle (dist/combadge.mjs)
	// StartCombadge uploads; it must be set before StartCombadge is called.
	// Pass it absolute or "./"-prefixed (see CopyTo).
	Combadge string
	// ProbeTimeout bounds each of Probe's `exec true` and `beamctl list`
	// calls (separately, so a slow `true` doesn't shorten the list).
	ProbeTimeout time.Duration
	waitDelay    time.Duration // see defaultWaitDelay; shortened by tests

	// The login gate's state, from the last `tsh status`. loginMu is held
	// while that runs, so concurrent calls wait for one read.
	loginMu         sync.Mutex
	loginCheckEvery time.Duration // defaultLoginCheckEvery; shortened by tests
	loginChecked    time.Time     // when status was last run; zero: never
	loginUntil      time.Time     // the active profile's valid_until; zero: no expiry
	loggedOut       bool          // the last readable status found no login
}

var _ Provider = (*Tsh)(nil)

// NewTsh returns a Provider that runs the tsh binary at bin, or "tsh" from
// $PATH when bin is empty.
func NewTsh(bin string) *Tsh {
	if bin == "" {
		bin = "tsh"
	}
	return &Tsh{bin: bin, ProbeTimeout: DefaultProbeTimeout, waitDelay: defaultWaitDelay,
		loginCheckEvery: defaultLoginCheckEvery}
}

// Create runs `tsh beams add --no-console --format=json`.
func (t *Tsh) Create(ctx context.Context) (Beam, error) {
	out, err := t.run(ctx, "add", "--no-console", "--format=json")
	if err != nil {
		return Beam{}, err
	}
	var b tshBeam
	if err := json.Unmarshal(out, &b); err != nil {
		return Beam{}, fmt.Errorf("tsh beams add: parse output: %w", err)
	}
	if b.ID == "" || b.Expires.IsZero() {
		return Beam{}, fmt.Errorf("tsh beams add: output lacks id or expires: %s", bytes.TrimSpace(out))
	}
	return Beam{Alias: b.ID, ExpiresAt: b.Expires}, nil
}

// Exec runs `tsh beams exec <alias> -- <name> <args...>` with stdin and
// stdout piped to the returned Session.
//
// The `--` is required: tsh parses flags anywhere on its command line, so
// without it an arg like `--since` fails as an unknown tsh flag and one like
// `--debug` is silently taken by tsh. tsh consumes the first `--` and passes
// any later one through. tsh then joins the command with spaces into a single
// SSH exec string that the beam's shell parses again, so name and each arg
// are single-quoted here: they reach the remote process literally, with no
// "~" or $VAR expansion (callers must pass absolute remote paths). This
// matches the holodeck, which passes args to the process directly.
//
// tsh retries the whole remote command (up to 10 times) on connection
// problems, so a command may run more than once: anything with side effects,
// such as `beamctl start`, must tolerate running twice.
//
// Cancelling ctx kills tsh (closing the SSH session, which ends the remote
// process) and unblocks Stdout readers with ctx's error. Up to the last
// 4KiB of tsh's stderr is kept and included in Wait's error.
//
// The session's stdout pipe and its hook on ctx are released only when Stdout
// is read to EOF (or an error) or ctx ends, so callers must do one of the
// two. The pump should give each attach connection its own child ctx and
// cancel it when the connection ends.
//
// stdin is the one pipe the bridge writes into (attach's commands), so
// Exec doesn't trust the cached login state: it runs `tsh status` right
// before starting tsh, and with the login expired it starts nothing (#102).
func (t *Tsh) Exec(ctx context.Context, beam Beam, name string, args ...string) (Session, error) {
	if err := t.checkLogin(ctx, true); err != nil {
		return nil, fmt.Errorf("tsh beams exec: %w", err)
	}
	cmd := t.command(ctx, append([]string{"exec", beam.Alias, "--"}, quoteAll(name, args)...)...)
	return startSession(ctx, cmd, func(err error, stderr *tailBuffer) error {
		return wrapErr(ctx, "exec", err, stderr)
	})
}

// quoteAll shell-quotes name and each of args (see Exec).
func quoteAll(name string, args []string) []string {
	out := make([]string, 0, 1+len(args))
	out = append(out, shellQuote(name))
	for _, a := range args {
		out = append(out, shellQuote(a))
	}
	return out
}

// shellQuote single-quotes s for a POSIX shell.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// Probe lists beams with `tsh beams ls --format=json`; a listing failure is
// returned as err (health unknown), as is ErrLoginExpired from any of its
// calls: a login that expired mid-probe says nothing about the beam. If the alias isn't listed the beam is
// gone (false, nil). Otherwise it runs `tsh beams exec <alias> -- true` bounded
// by ProbeTimeout: failure or timeout means unhealthy (false, nil). If ctx
// itself ends, its error is returned since health is then unknown.
//
// A reachable beam isn't enough: combadge serve may have crashed in it, in
// which case heartbeats stop but the beam still answers. So Probe then runs
// `tsh beams exec <alias> -- beamctl list`, under its own ProbeTimeout, and
// reads the combadge service's state as StartCombadge does:
//
//   - running: healthy (true, nil).
//   - anything else, not listed included: unhealthy (false, nil). The pump
//     only probes after heartbeats have been stale for 30s, so a serve that
//     still isn't running by then (starting, restarting, a crash loop) is
//     down for the crew's purposes.
//   - beamctl list fails or times out: err (health unknown). `exec true`
//     just succeeded, so the beam is reachable and this failure says
//     nothing definite about combadge.
func (t *Tsh) Probe(ctx context.Context, beam Beam) (bool, error) {
	listed, err := t.Exists(ctx, beam)
	if err != nil || !listed {
		return false, err
	}
	pctx, cancel := context.WithTimeout(ctx, t.ProbeTimeout)
	defer cancel()
	if _, err := t.run(pctx, "exec", beam.Alias, "--", "true"); err != nil {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		if errors.Is(err, ErrLoginExpired) {
			return false, err
		}
		return false, nil
	}
	return t.probeCombadge(ctx, beam)
}

// Exists reports whether `tsh beams ls --format=json` lists beam's alias. A
// listing failure is returned as err.
func (t *Tsh) Exists(ctx context.Context, beam Beam) (bool, error) {
	out, err := t.run(ctx, "ls", "--format=json")
	if err != nil {
		return false, err
	}
	var beams []tshBeam
	if err := json.Unmarshal(out, &beams); err != nil {
		return false, fmt.Errorf("tsh beams ls: parse output: %w", err)
	}
	return slices.ContainsFunc(beams, func(b tshBeam) bool { return b.ID == beam.Alias }), nil
}

// probeCombadge is the `beamctl list` half of Probe.
func (t *Tsh) probeCombadge(ctx context.Context, beam Beam) (bool, error) {
	lctx, cancel := context.WithTimeout(ctx, t.ProbeTimeout)
	defer cancel()
	state, err := t.combadgeState(lctx, beam)
	if err != nil {
		return false, err
	}
	return state == "running", nil
}

// CopyTo runs `tsh beams exec <alias> -- mkdir -p <dir of remotePath>`, then
// `tsh beams scp <localPath> <alias>:<remotePath>`.
//
// Local paths are passed to tsh as-is: one containing ":" is taken as a
// remote path and one starting with "-" as a flag, so pass absolute or
// "./"-prefixed paths without colons. remotePath must be absolute.
func (t *Tsh) CopyTo(ctx context.Context, beam Beam, localPath, remotePath string) error {
	if err := t.execRun(ctx, beam, "mkdir", "-p", path.Dir(remotePath)); err != nil {
		return err
	}
	_, err := t.run(ctx, "scp", localPath, beam.Alias+":"+remotePath)
	return err
}

// WorkDir returns /tmp/bmus/work, the --work-dir StartCombadge gives serve.
func (t *Tsh) WorkDir(Beam) (string, error) { return remoteWorkDir, nil }

// CopyFrom runs `tsh beams scp <alias>:<remotePath> <localPath>`. The same
// local-path caveats as CopyTo apply, and the caller must create localPath's
// directory first.
func (t *Tsh) CopyFrom(ctx context.Context, beam Beam, remotePath, localPath string) error {
	_, err := t.run(ctx, "scp", beam.Alias+":"+remotePath, localPath)
	return err
}

// Remove runs `tsh beams rm <alias>`.
func (t *Tsh) Remove(ctx context.Context, beam Beam) error {
	_, err := t.run(ctx, "rm", beam.Alias)
	return err
}

// command builds `tsh beams <args...>` (tshCommand).
func (t *Tsh) command(ctx context.Context, args ...string) *exec.Cmd {
	return t.tshCommand(ctx, append([]string{"beams"}, args...)...)
}

// tshCommand builds `tsh <args...>`, killed when ctx ends. tsh runs in a
// session of its own (setsid), so it has no controlling terminal: /dev/tty
// can't be opened, and the bridge's terminal can't reach any prompt. Its
// stdin is /dev/null unless the caller sets one.
func (t *Tsh) tshCommand(ctx context.Context, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, t.bin, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Cancel = func() error { return cmd.Process.Kill() }
	cmd.WaitDelay = t.waitDelay
	return cmd
}

// run runs `tsh beams <args...>` to completion and returns its stdout. Stdin
// is /dev/null. It passes the login gate first (checkLogin). On #90 an
// expired login made tsh prompt for the password and then for MFA in a
// browser, so with the login expired run starts nothing. If tsh fails, the
// login is read again, and an expired one is added to the error.
func (t *Tsh) run(ctx context.Context, args ...string) ([]byte, error) {
	if err := t.checkLogin(ctx, false); err != nil {
		return nil, fmt.Errorf("tsh beams %s: %w", args[0], err)
	}
	var stdout bytes.Buffer
	var stderr tailBuffer
	cmd := t.command(ctx, args...)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	started := time.Now()
	if err := cmd.Run(); err != nil {
		err = wrapErr(ctx, args[0], err, &stderr)
		if t.expiredSince(ctx, started) {
			return nil, fmt.Errorf("%w (%w)", ErrLoginExpired, err)
		}
		return nil, err
	}
	return stdout.Bytes(), nil
}

// LoginValidUntil runs `tsh status --format=json` and returns the active
// profile's valid_until (see parseLoginStatus), updating the login gate.
func (t *Tsh) LoginValidUntil(ctx context.Context) (time.Time, error) {
	t.loginMu.Lock()
	defer t.loginMu.Unlock()
	return t.refreshLoginLocked(ctx)
}

// checkLogin is the login gate: it returns ErrLoginExpired if the login is
// expired or absent, reading `tsh status` first if fresh is set or the
// state is loginCheckEvery old. A status that can't be read leaves the
// state as it was, so a broken `tsh status` alone never stops beam work.
func (t *Tsh) checkLogin(ctx context.Context, fresh bool) error {
	t.loginMu.Lock()
	defer t.loginMu.Unlock()
	if fresh || time.Since(t.loginChecked) >= t.loginCheckEvery {
		_, _ = t.refreshLoginLocked(ctx)
	}
	if t.expiredLocked() {
		return ErrLoginExpired
	}
	return nil
}

// expiredSince reads the login state again after a call started at
// started failed (unless another call's read is already newer) and
// reports whether the login is expired. The read isn't cut short by ctx:
// a call killed by its timeout may have been waiting on a login.
func (t *Tsh) expiredSince(ctx context.Context, started time.Time) bool {
	t.loginMu.Lock()
	defer t.loginMu.Unlock()
	if t.loginChecked.Before(started) {
		_, _ = t.refreshLoginLocked(context.WithoutCancel(ctx))
	}
	return t.expiredLocked()
}

func (t *Tsh) expiredLocked() bool {
	return t.loggedOut || (!t.loginUntil.IsZero() && !time.Now().Before(t.loginUntil.Add(-LoginExpiryMargin)))
}

// refreshLoginLocked runs `tsh status` and records what it found. loginMu
// must be held.
func (t *Tsh) refreshLoginLocked(ctx context.Context) (time.Time, error) {
	t.loginChecked = time.Now()
	until, err := t.loginStatus(ctx)
	switch {
	case err == nil:
		t.loginUntil, t.loggedOut = until, false
	case errors.Is(err, ErrLoginExpired):
		t.loginUntil, t.loggedOut = time.Time{}, true
	}
	return until, err
}

// loginStatus runs `tsh status --format=json`, under loginStatusTimeout, and
// parses it with parseLoginStatus. Like the beams calls it has no terminal
// and stdin is /dev/null; status itself never logs in.
func (t *Tsh) loginStatus(ctx context.Context) (time.Time, error) {
	ctx, cancel := context.WithTimeout(ctx, loginStatusTimeout)
	defer cancel()
	var stdout bytes.Buffer
	var stderr tailBuffer
	cmd := t.tshCommand(ctx, "status", "--format=json")
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	runErr := cmd.Run()
	if runErr != nil && ctx.Err() != nil {
		return time.Time{}, fmt.Errorf("tsh status: %w (%w)", ctx.Err(), runErr)
	}
	return parseLoginStatus(stdout.Bytes(), stderr.String(), runErr)
}

// tshStatus is the part of `tsh status --format=json` LoginValidUntil reads:
//
//	{"active": {"profile_url": "...", "username": "...", "cluster": "...",
//	  "valid_until": "2026-10-23T20:00:00Z", ...}, "profiles": [...]}
//
// It mirrors serializeProfiles and profileInfo in gravitational/teleport
// tool/tsh/common/tsh.go (read at master, August 2026).
// TODO(#102): check the shape, and the not-logged-in and expired outputs,
// against a real tsh.
type tshStatus struct {
	Active *struct {
		ValidUntil *time.Time `json:"valid_until"`
	} `json:"active"`
	Profiles json.RawMessage `json:"profiles"`
}

// parseLoginStatus reads `tsh status --format=json`'s stdout, stderr and
// exit error. tsh's onStatus prints the profiles before it checks expiry, so
// an expired login exits 1 ("Active profile expired.") with the JSON on
// stdout: a valid_until in the past. Not logged in, it prints nothing and
// exits 1 ("Not logged in."), and with profiles but none active it prints
// them without "active" ("No active profile."): both are ErrLoginExpired.
// Text before the first "{" (a warning, say) and after the object is
// ignored. A zero valid_until never expires, as in tsh. Anything else is
// an error, the login state unknown: an object without "active" or
// "profiles", or an active profile without valid_until.
func parseLoginStatus(stdout []byte, stderr string, runErr error) (time.Time, error) {
	var st tshStatus
	var parseErr error
	if i := bytes.IndexByte(stdout, '{'); i >= 0 {
		parseErr = json.NewDecoder(bytes.NewReader(stdout[i:])).Decode(&st)
	} else {
		parseErr = errors.New("no JSON object")
	}
	switch {
	case parseErr == nil && st.Active != nil && st.Active.ValidUntil != nil:
		return *st.Active.ValidUntil, nil
	case parseErr == nil && st.Active == nil && st.Profiles != nil,
		strings.Contains(strings.ToLower(stderr), "not logged in"):
		return time.Time{}, fmt.Errorf("tsh status: no active login: %w", ErrLoginExpired)
	case runErr != nil:
		if stderr != "" {
			return time.Time{}, fmt.Errorf("tsh status: %w: %s", runErr, stderr)
		}
		return time.Time{}, fmt.Errorf("tsh status: %w", runErr)
	case parseErr != nil:
		return time.Time{}, fmt.Errorf("tsh status: parse output: %w", parseErr)
	case st.Active != nil:
		return time.Time{}, fmt.Errorf("tsh status: parse output: active profile without valid_until in %s", truncateOutput(stdout))
	default:
		return time.Time{}, fmt.Errorf("tsh status: parse output: neither active nor profiles in %s", truncateOutput(stdout))
	}
}

// wrapErr names the failed subcommand and appends tsh's stderr tail. If ctx
// ended, its error is wrapped too so callers can errors.Is it.
func wrapErr(ctx context.Context, sub string, err error, stderr *tailBuffer) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		err = fmt.Errorf("%w (%w)", ctxErr, err)
	}
	if tail := stderr.String(); tail != "" {
		return fmt.Errorf("tsh beams %s: %w: %s", sub, err, tail)
	}
	return fmt.Errorf("tsh beams %s: %w", sub, err)
}

// RemoteDir is where Tsh keeps combadge on a beam: the bundle
// (combadge.mjs), serve's socket (serve.sock) and its outbox (outbox.ndjson,
// with state.json and serve.lock beside it). It is absolute because Exec
// quotes every argument, so "~" would reach the beam literally.
//
// On #19 /tmp/bmus was writable and `node` was on the PATH of both
// `tsh beams exec` and beam-init services. The service got HOME, PATH and
// USER and nothing else; captureEnv fills in the rest.
//
// Beams can't restart (#90), so /tmp/bmus lasts as long as the beam.
const RemoteDir = "/tmp/bmus"

const (
	remoteBundle = RemoteDir + "/combadge.mjs"
	remoteSocket = RemoteDir + "/serve.sock"
	remoteOutbox = RemoteDir + "/outbox.ndjson"
	// remoteEnv is serve's --env-file (see captureEnv).
	remoteEnv = RemoteDir + "/beam.env"
	// remoteWorkDir is serve's --work-dir (Provider.WorkDir).
	remoteWorkDir = RemoteDir + "/work"

	// beamctlService is the beam-init service name serve runs under.
	beamctlService = "combadge"
)

// StartCombadge uploads the bundle and runs serve as a beam-init service:
//
//	tsh beams exec <alias> -- beamctl list
//	tsh beams exec <alias> -- mkdir -p /tmp/bmus       (CopyTo)
//	tsh beams scp <Combadge> <alias>:/tmp/bmus/combadge.mjs
//	tsh beams exec <alias> -- sh -c 'umask 077 && env -0 > /tmp/bmus/beam.env'   (captureEnv)
//	tsh beams exec <alias> -- beamctl start --name=combadge -- node /tmp/bmus/combadge.mjs \
//	    serve --socket /tmp/bmus/serve.sock --crew <id> --outbox /tmp/bmus/outbox.ndjson \
//	    --work-dir /tmp/bmus/work --env-file /tmp/bmus/beam.env [--harness <spec.Harness>] [--role <spec.Role>] \
//	    [--hail-park-after <spec.HailParkAfter>] <spec.Args...>
//
// beam-init refuses a service name that already exists, even a stopped one
// (https://goteleport.com/docs/beams/long-running-agents/), so it is made
// idempotent from the combadge service's state in `beamctl list`:
//
//   - running: return nil without uploading anything.
//   - not listed: upload, capture the environment and `beamctl start`. If start fails (e.g. tsh ran
//     it twice and the second run found the name taken), a second list
//     showing combadge running counts as success.
//   - stopped, exited or failed: upload, capture the environment, then `beamctl restart combadge`,
//     which reruns the service's original command (same paths; same crew,
//     since a beam serves one crew member) and keeps its beam-init history.
//     If restart fails and combadge still isn't running, it is pruned with
//     `beamctl stop --prune combadge` (a prune that fails but leaves it
//     unlisted counts as done) and started afresh as above.
//   - any other state: an error. It isn't known to be dead, so it is left
//     alone rather than killed.
//
// It doesn't wait for serve's socket: the pump's attach reconnect loop
// retries until serve is up.
//
// On #19 and #90 `beamctl list` showed `combadge (running PID=<n>)` and,
// after `beamctl stop`, `combadge (stopped)`; `beamctl restart` brought a
// stopped service back, and a taken name fails with "Service named
// `combadge` already exists" (exit 1).
func (t *Tsh) StartCombadge(ctx context.Context, beam Beam, spec CombadgeSpec) error {
	if t.Combadge == "" {
		return errors.New("tsh: StartCombadge: Combadge bundle path not set")
	}
	if spec.CrewID == "" {
		return errors.New("tsh: StartCombadge: CrewID is required")
	}
	state, err := t.combadgeState(ctx, beam)
	if err != nil {
		return err
	}
	if state == "running" {
		return nil
	}
	if state != "" && !deadServiceStates[state] {
		return fmt.Errorf("tsh: StartCombadge: combadge service in %s is %q, neither running nor known dead", beam.Alias, state)
	}
	if err := t.CopyTo(ctx, beam, t.Combadge, remoteBundle); err != nil {
		return err
	}
	if err := t.captureEnv(ctx, beam); err != nil {
		return err
	}
	if state != "" {
		restartErr := t.execRun(ctx, beam, "beamctl", "restart", beamctlService)
		if restartErr == nil {
			return nil
		}
		state, err := t.combadgeState(ctx, beam)
		switch {
		case err != nil:
			return restartErr
		case state == "running":
			return nil
		case state != "" && !deadServiceStates[state]:
			return fmt.Errorf("tsh: StartCombadge: restart failed (%w), and combadge is now %q", restartErr, state)
		case state != "":
			if err := t.pruneCombadge(ctx, beam); err != nil {
				return err
			}
		}
	}
	start := append([]string{"start", "--name=" + beamctlService, "--",
		"node", remoteBundle, "serve",
		"--socket", remoteSocket, "--crew", spec.CrewID, "--outbox", remoteOutbox,
		"--work-dir", remoteWorkDir, "--env-file", remoteEnv,
	}, spec.serveArgs(nil)...)
	startErr := t.execRun(ctx, beam, "beamctl", start...)
	if startErr == nil {
		return nil
	}
	if state, err := t.combadgeState(ctx, beam); err == nil && state == "running" {
		return nil
	}
	return startErr
}

// captureEnv writes the environment `tsh beams exec` gets to remoteEnv, as
// `env -0` output that serve loads with --env-file (#86). beam-init services
// start with little more than HOME, PATH and USER, and the sdk harness's
// claude needs the LLM proxy settings (ANTHROPIC_BASE_URL and friends) that
// exec sessions get. It runs before every start and restart, so a restarted
// serve picks up settings that changed since. serve skips the variables that
// describe this sh session (PWD, SHLVL, SSH_*, ...).
func (t *Tsh) captureEnv(ctx context.Context, beam Beam) error {
	return t.execRun(ctx, beam, "sh", "-c", "umask 077 && env -0 > "+remoteEnv)
}

// deadServiceStates are the `beamctl list` states in which the combadge
// service is known not to be running, so restarting or pruning it is safe.
var deadServiceStates = map[string]bool{"stopped": true, "exited": true, "failed": true}

// pruneCombadge runs `beamctl stop --prune combadge`. If that fails but
// combadge is no longer listed (e.g. tsh re-ran it), the prune is done.
func (t *Tsh) pruneCombadge(ctx context.Context, beam Beam) error {
	pruneErr := t.execRun(ctx, beam, "beamctl", "stop", "--prune", beamctlService)
	if pruneErr == nil {
		return nil
	}
	if state, err := t.combadgeState(ctx, beam); err == nil && state == "" {
		return nil
	}
	return pruneErr
}

// AttachCombadge runs `node /tmp/bmus/combadge.mjs attach --socket
// /tmp/bmus/serve.sock --since <since>` in beam through Exec.
func (t *Tsh) AttachCombadge(ctx context.Context, beam Beam, since uint64) (Session, error) {
	return t.Exec(ctx, beam, "node", remoteBundle, "attach",
		"--socket", remoteSocket, "--since", strconv.FormatUint(since, 10))
}

// beamctlLine matches a `beamctl list` line, e.g. "combadge (running PID=42)"
// or "combadge (stopped)", capturing the name and the state word.
var beamctlLine = regexp.MustCompile(`^(\S+)\s+\(([^\s)]+)`)

// combadgeState returns the combadge service's state in `beamctl list`
// ("running", or e.g. "stopped"), or "" if it isn't listed.
func (t *Tsh) combadgeState(ctx context.Context, beam Beam) (string, error) {
	out, err := t.execOutput(ctx, beam, "beamctl", "list")
	if err != nil {
		return "", err
	}
	for line := range strings.Lines(string(out)) {
		if m := beamctlLine.FindStringSubmatch(strings.TrimSpace(line)); m != nil && m[1] == beamctlService {
			return m[2], nil
		}
	}
	return "", nil
}

// execRun runs `tsh beams exec <alias> -- <name> <args...>` (quoted as in
// Exec) to completion, discarding its stdout.
func (t *Tsh) execRun(ctx context.Context, beam Beam, name string, args ...string) error {
	_, err := t.execOutput(ctx, beam, name, args...)
	return err
}

// execOutput is execRun returning the command's stdout.
func (t *Tsh) execOutput(ctx context.Context, beam Beam, name string, args ...string) ([]byte, error) {
	return t.run(ctx, append([]string{"exec", beam.Alias, "--"}, quoteAll(name, args)...)...)
}

// truncateOutput trims out and cuts it to 200 bytes, for an error message.
func truncateOutput(out []byte) string {
	out = bytes.TrimSpace(out)
	if len(out) > 200 {
		return string(out[:200]) + "..."
	}
	return string(out)
}

// Size runs `tsh beams exec <alias> -- stat -L -c%s <path>` and parses its
// stdout as the file's size in bytes. -L reports a symlink's target, the
// file CopyFrom would copy.
func (t *Tsh) Size(ctx context.Context, beam Beam, path string) (int64, error) {
	out, err := t.execOutput(ctx, beam, "stat", "-L", "-c%s", path)
	if err != nil {
		return 0, err
	}
	n, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("tsh beams exec stat: parse output: %w", err)
	}
	return n, nil
}
