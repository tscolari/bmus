package transporter

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// fakeTsh writes a shell script standing in for tsh. Every `tsh beams`
// invocation appends its argv (as one line, args separated by \x1f) to a
// log, then runs body, where "$2" is the beams subcommand. It returns the
// script path and a func reading back the logged invocations.
//
// `tsh status` isn't logged there, and doesn't run body: it appends a line
// to status.log beside the script and runs status.sh, which reports a login
// valid until 2999 unless setStatus replaces it.
func fakeTsh(t *testing.T, body string) (string, func() [][]string) {
	t.Helper()
	dir := t.TempDir()
	log := filepath.Join(dir, "argv.log")
	setStatus(t, filepath.Join(dir, "tsh"), validStatus)
	script := "#!/bin/sh\n" +
		"if [ \"$1\" = status ]; then echo \"$*\" >> '" + filepath.Join(dir, "status.log") + "'; . '" + filepath.Join(dir, "status.sh") + "'; exit; fi\n" +
		"{ for a in \"$@\"; do printf '%s\\037' \"$a\"; done; printf '\\n'; } >> '" + log + "'\n" +
		body + "\n"
	bin := filepath.Join(dir, "tsh")
	require.NoError(t, os.WriteFile(bin, []byte(script), 0o755))
	return bin, func() [][]string {
		data, err := os.ReadFile(log)
		if os.IsNotExist(err) {
			return nil
		}
		require.NoError(t, err)
		var calls [][]string
		for line := range strings.Lines(string(data)) {
			args := strings.Split(strings.TrimSuffix(line, "\n"), "\x1f")
			calls = append(calls, args[:len(args)-1])
		}
		return calls
	}
}

// validStatus is `tsh status --format=json` with a login valid until 2999.
const validStatus = `echo '{"active":{"profile_url":"https://example.teleport.sh:443","username":"captain","cluster":"example.teleport.sh","valid_until":"2999-01-01T00:00:00Z"},"profiles":[]}'`

// setStatus makes fake tsh bin answer `tsh status` by running the shell
// code body.
func setStatus(t *testing.T, bin, body string) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(filepath.Dir(bin), "status.sh"), []byte(body+"\n"), 0o644))
}

// statusCalls counts fake tsh bin's `tsh status` runs.
func statusCalls(t *testing.T, bin string) int {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(filepath.Dir(bin), "status.log"))
	if os.IsNotExist(err) {
		return 0
	}
	require.NoError(t, err)
	return strings.Count(string(data), "\n")
}

// execBody makes `tsh beams exec <alias> -- <cmd...>` run <cmd...> locally,
// joining it into one string for a shell to re-parse as the beam's would.
const execBody = `[ "$2" = exec ] && [ "$4" = -- ] && { shift 4; eval "exec $*"; }`

const addJSON = `{
  "id": "warm-orbit",
  "uuid": "0b4c3c1e-8a5e-4d4f-9a3e-2f7c1d2b3a4f",
  "owner": "captain",
  "expires": "2026-10-17T12:00:00Z",
  "region": "us-east-1"
}`

var orbit = Beam{Alias: "warm-orbit"}

func TestNewTshDefaultsBinary(t *testing.T) {
	require.Equal(t, "tsh", NewTsh("").bin)
	require.Equal(t, DefaultProbeTimeout, NewTsh("").ProbeTimeout)
}

func TestCreate(t *testing.T) {
	bin, calls := fakeTsh(t, "cat <<'EOF'\n"+addJSON+"\nEOF")
	b, err := NewTsh(bin).Create(t.Context())
	require.NoError(t, err)
	require.Equal(t, Beam{Alias: "warm-orbit", ExpiresAt: time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)}, b)
	require.Equal(t, [][]string{{"beams", "add", "--no-console", "--format=json"}}, calls())
}

func TestCreateErrors(t *testing.T) {
	for name, tc := range map[string]struct{ body, want string }{
		"tsh fails":  {`echo 'ERROR: not logged in' >&2; exit 1`, "tsh beams add: exit status 1: ERROR: not logged in"},
		"bad json":   {`echo 'Beam "warm-orbit" created.'`, "tsh beams add: parse output"},
		"missing id": {`echo '{"expires":"2026-10-17T12:00:00Z"}'`, "tsh beams add: output lacks id or expires"},
	} {
		t.Run(name, func(t *testing.T) {
			bin, _ := fakeTsh(t, tc.body)
			_, err := NewTsh(bin).Create(t.Context())
			require.ErrorContains(t, err, tc.want)
		})
	}
}

func TestExecEcho(t *testing.T) {
	bin, calls := fakeTsh(t, execBody)
	s, err := NewTsh(bin).Exec(t.Context(), orbit, "echo", "hi")
	require.NoError(t, err)
	out, err := io.ReadAll(s.Stdout())
	require.NoError(t, err)
	require.Equal(t, "hi\n", string(out))
	require.NoError(t, s.Wait())
	require.Equal(t, [][]string{{"beams", "exec", "warm-orbit", "--", "'echo'", "'hi'"}}, calls())
}

func TestExecStdinAndConcurrentWait(t *testing.T) {
	bin, calls := fakeTsh(t, "exec cat") // stands in for combadge attach
	s, err := NewTsh(bin).Exec(t.Context(), orbit, "combadge", "attach", "--since", "7")
	require.NoError(t, err)
	// Wait before reading: it must not return (or drop output) until the
	// reader has drained stdout.
	waited := make(chan error, 1)
	go func() { waited <- s.Wait() }()
	_, err = io.WriteString(s, "ping\n")
	require.NoError(t, err)
	require.NoError(t, s.Close())
	out, err := io.ReadAll(s.Stdout())
	require.NoError(t, err)
	require.Equal(t, "ping\n", string(out))
	require.NoError(t, <-waited)
	require.Equal(t, [][]string{{"beams", "exec", "warm-orbit", "--", "'combadge'", "'attach'", "'--since'", "'7'"}}, calls())
}

func TestExecQuotesForRemoteShell(t *testing.T) {
	bin, calls := fakeTsh(t, execBody)
	s, err := NewTsh(bin).Exec(t.Context(), orbit, "printf", `%s|`, "it's", "a  b", "$HOME", "~", "--debug", "--")
	require.NoError(t, err)
	out, err := io.ReadAll(s.Stdout())
	require.NoError(t, err)
	require.NoError(t, s.Wait())
	require.Equal(t, `it's|a  b|$HOME|~|--debug|--|`, string(out))
	require.Equal(t, [][]string{{"beams", "exec", "warm-orbit", "--",
		"'printf'", "'%s|'", `'it'\''s'`, "'a  b'", "'$HOME'", "'~'", "'--debug'", "'--'"}}, calls())
}

// Output must survive tsh exiting before anyone reads it (no WaitDelay
// truncation).
// Its 60000 bytes must stay under the 64KiB Linux pipe buffer, or the fake blocks.
func TestExecOutputReadableAfterExit(t *testing.T) {
	bin, _ := fakeTsh(t, `head -c 60000 /dev/zero | tr '\0' x`)
	tsh := NewTsh(bin)
	tsh.waitDelay = 50 * time.Millisecond
	s, err := tsh.Exec(t.Context(), orbit, "anything")
	require.NoError(t, err)
	require.NoError(t, s.Wait())
	time.Sleep(4 * tsh.waitDelay)
	out, err := io.ReadAll(s.Stdout())
	require.NoError(t, err)
	require.Len(t, out, 60000)
}

func TestExecReadsAfterEOFStayEOF(t *testing.T) {
	bin, _ := fakeTsh(t, execBody)
	s, err := NewTsh(bin).Exec(t.Context(), orbit, "echo", "hi")
	require.NoError(t, err)
	out, err := io.ReadAll(s.Stdout())
	require.NoError(t, err)
	require.Equal(t, "hi\n", string(out))
	for range 3 {
		n, err := s.Stdout().Read(make([]byte, 8))
		require.Zero(t, n)
		require.ErrorIs(t, err, io.EOF)
	}
	require.NoError(t, s.Wait())
}

// Sessions read to EOF must release their pipe and ctx hook even while the
// Exec ctx lives on.
func TestExecNoLeakOnLongLivedCtx(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("counts /proc/self/fd")
	}
	countFDs := func() int {
		ents, err := os.ReadDir("/proc/self/fd")
		require.NoError(t, err)
		return len(ents)
	}
	bin, _ := fakeTsh(t, execBody)
	tsh := NewTsh(bin)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	before := countFDs()
	for range 50 {
		s, err := tsh.Exec(ctx, orbit, "echo", "hi")
		require.NoError(t, err)
		_, err = io.ReadAll(s.Stdout())
		require.NoError(t, err)
		require.NoError(t, s.Wait())
	}
	require.LessOrEqual(t, countFDs(), before+2) // slack for runtime fds
	cancel()
}

func TestExecFailureIncludesStderrTail(t *testing.T) {
	bin, _ := fakeTsh(t, `head -c 20000 /dev/zero | tr '\0' x >&2; echo boom >&2; exit 3`)
	s, err := NewTsh(bin).Exec(t.Context(), orbit, "anything")
	require.NoError(t, err)
	_, err = io.ReadAll(s.Stdout())
	require.NoError(t, err)
	err = s.Wait()
	require.ErrorContains(t, err, "tsh beams exec: exit status 3: ")
	require.True(t, strings.HasSuffix(err.Error(), "xboom"), err.Error())
	require.Less(t, len(err.Error()), stderrTail+100)
}

func TestExecCancelKills(t *testing.T) {
	bin, _ := fakeTsh(t, execBody)
	ctx, cancel := context.WithCancel(t.Context())
	s, err := NewTsh(bin).Exec(ctx, orbit, "sleep", "30")
	require.NoError(t, err)
	start := time.Now()
	cancel()
	// Nobody reads Stdout; Wait must still return promptly.
	require.ErrorIs(t, s.Wait(), context.Canceled)
	require.Less(t, time.Since(start), 2*time.Second)
	_, err = s.Stdout().Read(make([]byte, 1))
	require.ErrorIs(t, err, context.Canceled)
}

func TestExecMissingBinary(t *testing.T) {
	_, err := NewTsh(filepath.Join(t.TempDir(), "nope")).Exec(t.Context(), orbit, "true")
	require.ErrorContains(t, err, "tsh beams exec")
}

func TestProbe(t *testing.T) {
	const listed = `[ "$2" = ls ] && { echo '[{"id":"cold-comet","expires":"2026-10-17T00:00:00Z"},{"id":"warm-orbit","expires":"2026-10-17T12:00:00Z"}]'; exit 0; }`
	// beamctl lists the beam, makes `beamctl list` print out, and runs any
	// other exec (`true`) locally.
	beamctl := func(out string) string {
		return listed + "\n" + `[ "$6" = "'list'" ] && { printf '` + out + `'; exit 0; }` + "\n" + execBody
	}
	lsArgv := []string{"beams", "ls", "--format=json"}
	execArgv := []string{"beams", "exec", "warm-orbit", "--", "true"}
	listArgv := []string{"beams", "exec", "warm-orbit", "--", "'beamctl'", "'list'"}
	all := [][]string{lsArgv, execArgv, listArgv}
	for name, tc := range map[string]struct {
		body    string
		timeout time.Duration // ProbeTimeout; default DefaultProbeTimeout
		healthy bool
		err     string
		argv    [][]string
	}{
		"healthy":             {body: beamctl(`  bootstrap (running PID=2)\n  combadge (running PID=5817)\n`), healthy: true, argv: all},
		"combadge no pid":     {body: beamctl(`  combadge (running)\n`), healthy: true, argv: all},
		"combadge stopped":    {body: beamctl(`  bootstrap (running PID=2)\n  combadge (stopped)\n`), argv: all},
		"combadge exited":     {body: beamctl(`  combadge (exited PID=7)\n`), argv: all},
		"combadge failed":     {body: beamctl(`  combadge (failed)\n`), argv: all},
		"combadge not listed": {body: beamctl(`  bootstrap (running PID=2)\n`), argv: all},
		"empty beamctl list":  {body: beamctl(``), argv: all},
		// Not running after 30s of stale heartbeats: down.
		"combadge starting":  {body: beamctl(`  combadge (starting PID=3)\n`), argv: all},
		"beamctl list fails": {body: listed + "\n" + `[ "$6" = "'list'" ] && { echo 'beamctl: not found' >&2; exit 127; }` + "\n" + execBody, err: "tsh beams exec: exit status 127: beamctl: not found", argv: all},
		"beamctl list hangs": {body: listed + "\n" + `[ "$6" = "'list'" ] && exec sleep 30` + "\n" + execBody, timeout: 200 * time.Millisecond, err: "deadline exceeded", argv: all},
		"gone":               {body: `echo '[{"id":"cold-comet","expires":"2026-10-17T00:00:00Z"}]'`, argv: [][]string{lsArgv}},
		"empty list":         {body: `echo '[]'`, argv: [][]string{lsArgv}},
		"exec fails":         {body: listed + "\nexit 255", argv: [][]string{lsArgv, execArgv}},
		"exec hangs":         {body: listed + "\nexec sleep 30", timeout: 200 * time.Millisecond, argv: [][]string{lsArgv, execArgv}},
		"ls fails":           {body: `echo 'ERROR: certificate expired' >&2; exit 1`, err: "tsh beams ls: exit status 1: ERROR: certificate expired", argv: [][]string{lsArgv}},
		"ls bad json":        {body: `echo 'ID URL'`, err: "tsh beams ls: parse output", argv: [][]string{lsArgv}},
	} {
		t.Run(name, func(t *testing.T) {
			bin, calls := fakeTsh(t, tc.body)
			tsh := NewTsh(bin)
			if tc.timeout != 0 {
				tsh.ProbeTimeout = tc.timeout
			}
			healthy, err := tsh.Probe(t.Context(), orbit)
			if tc.err != "" {
				require.ErrorContains(t, err, tc.err)
			} else {
				require.NoError(t, err)
			}
			require.Equal(t, tc.healthy, healthy)
			require.Equal(t, tc.argv, calls())
		})
	}
}

func TestExists(t *testing.T) {
	const listed = `echo '[{"id":"cold-comet"},{"id":"warm-orbit"}]'`
	for name, tc := range map[string]struct {
		body   string
		exists bool
		err    string
	}{
		"listed":   {body: listed, exists: true},
		"gone":     {body: `echo '[{"id":"cold-comet"}]'`},
		"ls fails": {body: `echo 'ERROR: certificate expired' >&2; exit 1`, err: "certificate expired"},
	} {
		t.Run(name, func(t *testing.T) {
			bin, calls := fakeTsh(t, tc.body)
			exists, err := NewTsh(bin).Exists(t.Context(), orbit)
			if tc.err != "" {
				require.ErrorContains(t, err, tc.err)
			} else {
				require.NoError(t, err)
			}
			require.Equal(t, tc.exists, exists)
			require.Equal(t, [][]string{{"beams", "ls", "--format=json"}}, calls(), "Exists never execs in the beam")
		})
	}
}

func TestProbeParentCancelled(t *testing.T) {
	bin, _ := fakeTsh(t, `[ "$2" = ls ] && { echo '[{"id":"warm-orbit"}]'; exit 0; }; exec sleep 30`)
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	healthy, err := NewTsh(bin).Probe(ctx, orbit)
	require.False(t, healthy)
	require.ErrorIs(t, err, context.DeadlineExceeded)
}

func TestCopyAndRemove(t *testing.T) {
	bin, calls := fakeTsh(t, "true")
	tsh := NewTsh(bin)
	ctx := t.Context()
	require.NoError(t, tsh.CopyTo(ctx, orbit, "dist/combadge.js", "/home/beams/combadge.js"))
	require.NoError(t, tsh.CopyFrom(ctx, orbit, "/tmp/h/work.bundle", "/cargo/ORD-0001/3/work.bundle"))
	require.NoError(t, tsh.Remove(ctx, orbit))
	require.Equal(t, [][]string{
		{"beams", "exec", "warm-orbit", "--", "'mkdir'", "'-p'", "'/home/beams'"},
		{"beams", "scp", "dist/combadge.js", "warm-orbit:/home/beams/combadge.js"},
		{"beams", "scp", "warm-orbit:/tmp/h/work.bundle", "/cargo/ORD-0001/3/work.bundle"},
		{"beams", "rm", "warm-orbit"},
	}, calls())
}

func TestCopyAndRemoveErrors(t *testing.T) {
	bin, _ := fakeTsh(t, `echo "ERROR: beam not found" >&2; exit 1`)
	tsh := NewTsh(bin)
	ctx := t.Context()
	require.ErrorContains(t, tsh.CopyTo(ctx, orbit, "a", "/b"), "tsh beams exec: exit status 1: ERROR: beam not found")
	require.ErrorContains(t, tsh.CopyFrom(ctx, orbit, "a", "b"), "tsh beams scp: exit status 1: ERROR: beam not found")
	require.ErrorContains(t, tsh.Remove(ctx, orbit), "tsh beams rm: exit status 1: ERROR: beam not found")
}

// #65: pull-work and pull-checkpoint check a bundle's size before copying
// it.
func TestSize(t *testing.T) {
	bin, calls := fakeTsh(t, `[ "$2" = exec ] && echo 12345`)
	n, err := NewTsh(bin).Size(t.Context(), orbit, "/work/app.bundle")
	require.NoError(t, err)
	require.Equal(t, int64(12345), n)
	require.Equal(t, [][]string{{"beams", "exec", "warm-orbit", "--", "'stat'", "'-L'", "'-c%s'", "'/work/app.bundle'"}}, calls())
}

func TestSizeErrors(t *testing.T) {
	bin, _ := fakeTsh(t, `echo "ERROR: beam not found" >&2; exit 1`)
	_, err := NewTsh(bin).Size(t.Context(), orbit, "/work/app.bundle")
	require.ErrorContains(t, err, "tsh beams exec: exit status 1: ERROR: beam not found")

	bin, _ = fakeTsh(t, `[ "$2" = exec ] && echo not-a-number`)
	_, err = NewTsh(bin).Size(t.Context(), orbit, "/work/app.bundle")
	require.ErrorContains(t, err, "tsh beams exec stat: parse output")
}

func TestStartCombadge(t *testing.T) {
	cmd := func(args ...string) []string {
		argv := []string{"beams", "exec", "warm-orbit", "--"}
		for _, a := range args {
			argv = append(argv, "'"+a+"'")
		}
		return argv
	}
	list := cmd("beamctl", "list")
	restart := cmd("beamctl", "restart", "combadge")
	prune := cmd("beamctl", "stop", "--prune", "combadge")
	mkdir := cmd("mkdir", "-p", "/tmp/bmus")
	scp := []string{"beams", "scp", "/build/combadge.mjs", "warm-orbit:/tmp/bmus/combadge.mjs"}
	env := cmd("sh", "-c", "umask 077 && env -0 > /tmp/bmus/beam.env")
	start := cmd("beamctl", "start", "--name=combadge", "--",
		"node", "/tmp/bmus/combadge.mjs", "serve",
		"--socket", "/tmp/bmus/serve.sock", "--crew", "crew-1", "--outbox", "/tmp/bmus/outbox.ndjson",
		"--work-dir", "/tmp/bmus/work", "--env-file", "/tmp/bmus/beam.env", "--harness", "echo", "--heartbeat-interval", "5000")
	// The fakes dispatch on $6, the quoted beamctl subcommand (or mkdir's
	// -p). listing makes `beamctl list` print out. A "<name>" file next to
	// the fake records a step having run, for fakes whose listing changes.
	listing := func(out string) string {
		return `[ "$6" = "'list'" ] && { printf '` + out + `'; exit 0; }`
	}
	const stateful = `d="$(dirname "$0")"
on() { [ -e "$d/$1" ]; }
mark() { touch "$d/$1"; }
`
	for name, tc := range map[string]struct {
		body string
		err  string
		argv [][]string
	}{
		"fresh beam": {
			body: listing(`  bootstrap (running PID=2)\n`),
			argv: [][]string{list, mkdir, scp, env, start},
		},
		"already running": {
			body: listing(`  bootstrap (running PID=2)\n  combadge (running PID=5817)\n`),
			argv: [][]string{list},
		},
		"running without pid": {
			body: listing(`  combadge (running)\n`),
			argv: [][]string{list},
		},
		"stopped: restarted": {
			body: listing(`  combadge (stopped)\n`),
			argv: [][]string{list, mkdir, scp, env, restart},
		},
		"exited, restart fails: pruned and started": {
			body: listing(`  combadge (exited PID=7)\n`) + "\n" + `[ "$6" = "'restart'" ] && exit 1`,
			argv: [][]string{list, mkdir, scp, env, restart, list, prune, start},
		},
		"restart fails but serve came up": {
			body: stateful + `[ "$6" = "'list'" ] && { on restarted && echo '  combadge (running PID=9)' || echo '  combadge (failed)'; exit 0; }
[ "$6" = "'restart'" ] && { mark restarted; exit 1; }`,
			argv: [][]string{list, mkdir, scp, env, restart, list},
		},
		// tsh re-ran the prune: the second run fails, but combadge is gone.
		"prune fails but unlisted": {
			body: stateful + `[ "$6" = "'list'" ] && { on pruned || echo '  combadge (stopped)'; exit 0; }
[ "$6" = "'restart'" ] && exit 1
[ "$6" = "'stop'" ] && { mark pruned; echo 'no such service' >&2; exit 1; }`,
			argv: [][]string{list, mkdir, scp, env, restart, list, prune, list, start},
		},
		"prune fails, still listed": {
			body: listing(`  combadge (stopped)\n`) + "\n" + `[ "$6" = "'restart'" ] && exit 1
[ "$6" = "'stop'" ] && { echo 'beamctl: stuck' >&2; exit 1; }`,
			err:  "tsh beams exec: exit status 1: beamctl: stuck",
			argv: [][]string{list, mkdir, scp, env, restart, list, prune, list},
		},
		"env capture fails": {
			body: listing(``) + "\n" + `[ "$6" = "'-c'" ] && { echo 'sh: read-only file system' >&2; exit 1; }`,
			err:  "tsh beams exec: exit status 1: sh: read-only file system",
			argv: [][]string{list, mkdir, scp, env},
		},
		"unknown state left alone": {
			body: listing(`  combadge (starting PID=3)\n`),
			err:  `combadge service in warm-orbit is "starting", neither running nor known dead`,
			argv: [][]string{list},
		},
		// tsh re-ran `beamctl start` after a connection drop: the second run
		// finds the name taken, but serve is up.
		"start fails, serve running": {
			body: stateful + `[ "$6" = "'list'" ] && { on started && echo '  combadge (running PID=9)'; exit 0; }
[ "$6" = "'start'" ] && { mark started; echo 'service combadge already exists' >&2; exit 1; }`,
			argv: [][]string{list, mkdir, scp, env, start, list},
		},
		"start fails": {
			body: listing(``) + "\n" + `[ "$6" = "'start'" ] && { echo 'beamctl: boom' >&2; exit 1; }`,
			err:  "tsh beams exec: exit status 1: beamctl: boom",
			argv: [][]string{list, mkdir, scp, env, start, list},
		},
		"list fails": {
			body: `echo 'ERROR: beam not found' >&2; exit 1`,
			err:  "tsh beams exec: exit status 1: ERROR: beam not found",
			argv: [][]string{list},
		},
		"scp fails": {
			body: listing(``) + "\n" + `[ "$2" = scp ] && { echo 'scp: no space' >&2; exit 1; }`,
			err:  "tsh beams scp: exit status 1: scp: no space",
			argv: [][]string{list, mkdir, scp},
		},
	} {
		t.Run(name, func(t *testing.T) {
			bin, calls := fakeTsh(t, tc.body+"\nexit 0") // unmatched commands succeed
			tsh := NewTsh(bin)
			tsh.Combadge = "/build/combadge.mjs"
			err := tsh.StartCombadge(t.Context(), orbit, CombadgeSpec{CrewID: "crew-1", Harness: "echo", Args: []string{"--heartbeat-interval", "5000"}})
			if tc.err != "" {
				require.ErrorContains(t, err, tc.err)
			} else {
				require.NoError(t, err)
			}
			require.Equal(t, tc.argv, calls())
		})
	}
}

func TestBeamctlStateParsing(t *testing.T) {
	for line, want := range map[string][]string{
		"  combadge (running PID=5817)": {"combadge", "running"},
		"combadge (running)":            {"combadge", "running"},
		"  combadge (stopped)":          {"combadge", "stopped"},
		"bootstrap (exited code=1)":     {"bootstrap", "exited"},
	} {
		m := beamctlLine.FindStringSubmatch(strings.TrimSpace(line))
		require.Equal(t, want, m[1:], line)
	}
}

func TestStartCombadgeNeedsConfig(t *testing.T) {
	tsh := NewTsh("/nonexistent/tsh")
	require.ErrorContains(t, tsh.StartCombadge(t.Context(), orbit, CombadgeSpec{CrewID: "c"}), "Combadge bundle path not set")
	tsh.Combadge = "/build/combadge.mjs"
	require.ErrorContains(t, tsh.StartCombadge(t.Context(), orbit, CombadgeSpec{}), "CrewID is required")
}

func TestAttachCombadge(t *testing.T) {
	bin, calls := fakeTsh(t, "exec cat") // stands in for the attach relay
	s, err := NewTsh(bin).AttachCombadge(t.Context(), orbit, 42)
	require.NoError(t, err)
	_, err = io.WriteString(s, "ping\n")
	require.NoError(t, err)
	require.NoError(t, s.Close())
	out, err := io.ReadAll(s.Stdout())
	require.NoError(t, err)
	require.Equal(t, "ping\n", string(out))
	require.NoError(t, s.Wait())
	require.Equal(t, [][]string{{"beams", "exec", "warm-orbit", "--", "'node'", "'/tmp/bmus/combadge.mjs'", "'attach'",
		"'--socket'", "'/tmp/bmus/serve.sock'", "'--since'", "'42'"}}, calls())
}
