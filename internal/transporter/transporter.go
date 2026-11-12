// Package transporter provisions beams and talks to processes inside them.
// Provider has two implementations:
//
//   - Tsh: real Teleport Beams via the tsh CLI (spec D8). combadge serve runs
//     as a beam-init service under beamctl.
//   - Holodeck: local processes standing in for beams (spec D10), so the
//     bridge's workflows can be tested end to end without tsh or a network.
//     A "beam" is a directory; combadge serve is a detached local process
//     that outlives the bridge, as a real beam would.
//
// Workflow code drives both through Provider alone, combadge included
// (StartCombadge, AttachCombadge); it never checks which one it has.
// Retries and rate limiting are deliberately not done here: beam creation is
// throttled by a DBOS queue in Phase 2 (spec D8).
package transporter

import (
	"context"
	"errors"
	"io"
	"strings"
	"time"
)

// ErrLoginExpired is returned, wrapped, by every Tsh call made while the
// captain's tsh login is expired or absent (#102): the call didn't start
// tsh. It says nothing about the beam, so callers treat it as health
// unknown, never as a lost beam, and retry once the captain has logged in
// again.
var ErrLoginExpired = errors.New("tsh login expired: run tsh login")

// LoginExpiryMargin is how long before its valid_until a login counts as
// expired (#102): it covers clock skew between the bridge and the cluster,
// and calls admitted just before the expiry.
const LoginExpiryMargin = 2 * time.Minute

// Beam identifies a provisioned beam.
type Beam struct {
	// Alias is the beam's human-friendly ID (e.g. "warm-orbit"), accepted by
	// every tsh beams subcommand.
	Alias string
	// ExpiresAt is when the beam is automatically purged (~24h after creation).
	ExpiresAt time.Time
}

// CombadgeSpec says how to start combadge serve in a beam.
//
// serve's flags come in this order: the provider's own (--socket, --crew,
// --work-dir, ...), --harness Harness, --role Role and --hail-park-after
// HailParkAfter, the provider's configured harness flags
// (HolodeckConfig.Harness), then Args (serveArgs). serve takes the last value of a
// repeated flag, so a later --harness overrides an earlier one: the crew
// member's harness applies unless the provider's configuration or Args name
// another, which tests use (a holodeck with --harness holodeck --script).
type CombadgeSpec struct {
	// CrewID is the crew member whose agent the beam runs (serve --crew).
	CrewID string
	// Harness is the crew member's harness (serve --harness), e.g. sdk.
	// Empty passes no --harness, leaving serve's default (sdk).
	Harness string
	// Role is the crew member's role (serve --role), "command" for
	// Command's crew members (#155). Empty passes no --role.
	Role string
	// HailParkAfter is how long the sdk harness holds a hail before it
	// parks the session (serve --hail-park-after, #180). It is passed, as
	// time.Duration.String(), only when it is positive and the harness serve
	// ends up running is sdk (servedHarness): serve refuses the flag with
	// any other. Zero passes none, leaving serve's default (15m).
	HailParkAfter time.Duration
	// Args are extra serve flags, e.g. harness selection for holodeck tests.
	// They are appended verbatim, after every other flag.
	Args []string
}

// serveArgs returns serve's flags after the provider's own: --harness
// spec.Harness and --role spec.Role, each only when set, --hail-park-after
// spec.HailParkAfter when serve will run sdk, then the provider's
// configured harness flags, then spec.Args.
func (spec CombadgeSpec) serveArgs(configured []string) []string {
	var args []string
	if spec.Harness != "" {
		args = append(args, "--harness", spec.Harness)
	}
	if spec.Role != "" {
		args = append(args, "--role", spec.Role)
	}
	later := append(append([]string(nil), configured...), spec.Args...)
	if spec.HailParkAfter > 0 && servedHarness(spec.Harness, later) == "sdk" {
		args = append(args, "--hail-park-after", spec.HailParkAfter.String())
	}
	return append(args, later...)
}

// servedHarness is the harness serve runs: the last --harness in later
// (as "--harness <name>" or "--harness=<name>"), else harness, else
// serve's default, sdk.
func servedHarness(harness string, later []string) string {
	for i, a := range later {
		if a == "--harness" && i+1 < len(later) {
			harness = later[i+1]
		} else if name, ok := strings.CutPrefix(a, "--harness="); ok {
			harness = name
		}
	}
	if harness == "" {
		return "sdk"
	}
	return harness
}

// Provider creates, reaches and destroys beams, and runs combadge in them.
type Provider interface {
	// Create provisions a new beam.
	Create(ctx context.Context) (Beam, error)

	// Exec starts name with args inside beam and returns a Session wired to
	// its stdin and stdout. name and args reach the remote process literally
	// (no shell expansion), so remote paths must be absolute: no "~" or $VARs.
	// Cancelling ctx kills the process.
	Exec(ctx context.Context, beam Beam, name string, args ...string) (Session, error)

	// Probe reports whether beam is alive and reachable and, where the
	// provider can tell, whether combadge serve is still running in it
	// (Tsh checks beam-init's service list). The two results
	// mean different things and callers must keep them apart:
	//   - err != nil: health couldn't be determined (e.g. listing beams
	//     failed because the tsh login expired, or ctx was cancelled). The
	//     pump's sickbay check logs a warning and does NOT declare sickbay.
	//   - healthy == false, err == nil: the beam is definitely gone, or it is
	//     listed but a trivial command in it failed or timed out, or combadge
	//     serve is known not to be running in it.
	//   - healthy == true, err == nil: the beam answered and combadge serve
	//     is running in it (where the provider can tell).
	Probe(ctx context.Context, beam Beam) (healthy bool, err error)

	// Exists reports whether beam still exists, whatever is or isn't running
	// in it: (false, nil) means it is definitely gone, an error that it
	// couldn't be told. Unlike Probe it doesn't look at combadge, so
	// provisioning can tell a beam to restart serve in from one to replace.
	Exists(ctx context.Context, beam Beam) (bool, error)

	// CopyTo copies the local file localPath to remotePath inside beam,
	// creating remotePath's parent directories and replacing a file already
	// there (used to ship repo bundles in, #51).
	CopyTo(ctx context.Context, beam Beam, localPath, remotePath string) error

	// CopyFrom copies remotePath inside beam to the local file localPath
	// (used to pull checkpoint bundles, transcripts and handover notes).
	CopyFrom(ctx context.Context, beam Beam, remotePath, localPath string) error

	// Size reports path's size on beam, in bytes, cheaply (no full copy):
	// pull-work and pull-checkpoint check a bundle against
	// Config.MaxBundleBytes before copying it (#65).
	Size(ctx context.Context, beam Beam, path string) (int64, error)

	// WorkDir returns the absolute directory inside beam under which
	// combadge serve keeps per-order files: StartCombadge passes it as serve
	// --work-dir. The bridge copies an order's repo bundles to
	// <work-dir>/<order>/in/<name>.bundle, serve checks them out into
	// <work-dir>/<order>/<name>, and harnesses write checkpoints under
	// <work-dir>/<order>/.
	WorkDir(beam Beam) (string, error)

	// Remove destroys beam.
	Remove(ctx context.Context, beam Beam) error

	// StartCombadge installs combadge in beam if needed and starts
	// `combadge serve` there, detached from ctx and from the bridge: serve
	// keeps running after the bridge exits. It must be idempotent: if serve
	// is already running in beam it returns nil, since tsh may re-run
	// commands and CrewTour steps may replay.
	StartCombadge(ctx context.Context, beam Beam, spec CombadgeSpec) error

	// LoginValidUntil reports when the login the provider's calls run on
	// expires: Tsh reads the captain's tsh login from `tsh status`. The zero
	// time means it never expires (Holodeck). An error wrapping
	// ErrLoginExpired means there is no login at all (not logged in); any
	// other error that it couldn't be told.
	LoginValidUntil(ctx context.Context) (time.Time, error)

	// AttachCombadge starts `combadge attach --since <since>` in beam and
	// returns its Session: the relay to serve's outbox (entries above since,
	// then live) and command input. Like Exec, cancelling ctx kills it; serve
	// keeps running.
	AttachCombadge(ctx context.Context, beam Beam, since uint64) (Session, error)
}

// Session is a process running inside a beam.
type Session interface {
	// Write writes to the process's stdin; Close closes its stdin. Write is
	// not safe for concurrent callers: serialise writes (e.g. one line at a
	// time under a mutex) so NDJSON lines don't interleave.
	io.WriteCloser

	// Stdout returns the process's stdout. Output stays readable after the
	// process exits; the reader reaches EOF once it is drained. Once the Exec
	// ctx is done, reads return the ctx's error instead of data or EOF.
	// Callers must read it to EOF (or an error) or cancel the Exec ctx, or
	// its resources stay held until that ctx ends.
	Stdout() io.Reader

	// Wait blocks until the process has exited, then returns its exit error,
	// if any. It doesn't wait for Stdout to be read, may be called
	// concurrently with reads from Stdout, and may be called more than once.
	Wait() error
}
