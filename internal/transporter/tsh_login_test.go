package transporter

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// statusAt is `tsh status --format=json` for a login valid until at, exiting
// 1 as tsh does once it has passed.
func statusAt(at time.Time) string {
	json := `{"active":{"profile_url":"https://example.teleport.sh:443","username":"captain","valid_until":"` +
		at.UTC().Format(time.RFC3339Nano) + `"},"profiles":[]}`
	s := "echo '" + json + "'"
	if !at.After(time.Now()) {
		s += "; echo 'ERROR: Active profile expired.' >&2; exit 1"
	}
	return s
}

func TestParseLoginStatus(t *testing.T) {
	until := time.Date(2026, 10, 23, 20, 0, 0, 0, time.UTC)
	exit1 := errors.New("exit status 1")
	for name, tc := range map[string]struct {
		stdout, stderr string
		runErr         error
		want           time.Time
		expired        bool   // ErrLoginExpired
		err            string // another error
	}{
		"valid": {stdout: `{
  "active": {
    "profile_url": "https://example.teleport.sh:443",
    "username": "captain",
    "cluster": "example.teleport.sh",
    "roles": ["access"],
    "kubernetes_enabled": false,
    "valid_until": "2026-10-23T20:00:00Z"
  },
  "profiles": []
}`, want: until},
		"expired: JSON then exit 1": {stdout: `{"active":{"valid_until":"2026-10-23T20:00:00Z"},"profiles":[]}`,
			stderr: "ERROR: Active profile expired.", runErr: exit1, want: until},
		"warning before, text after": {stdout: "WARNING: something\n{\"active\":{\"valid_until\":\"2026-10-23T20:00:00Z\"}}\ntrailing", want: until},
		"zero valid_until":           {stdout: `{"active":{"valid_until":"0001-01-01T00:00:00Z"},"profiles":[]}`},
		"no valid_until":             {stdout: `{"active":{"username":"captain"},"profiles":[]}`, err: "tsh status: parse output: active profile without valid_until"},
		"null valid_until":           {stdout: `{"active":{"valid_until":null},"profiles":[]}`, err: "tsh status: parse output: active profile without valid_until"},
		"not logged in":              {stderr: "ERROR: Not logged in.", runErr: exit1, expired: true},
		"no active profile":          {stdout: `{"profiles":[{"valid_until":"2026-10-23T20:00:00Z"}]}`, stderr: "ERROR: No active profile.", runErr: exit1, expired: true},
		"tsh fails":                  {stderr: "ERROR: something else", runErr: exit1, err: "tsh status: exit status 1: ERROR: something else"},
		"not JSON":                   {stdout: "> Profile URL: https://example", err: "tsh status: parse output"},
		"another shape":              {stdout: `{"user":"captain"}`, err: "tsh status: parse output"},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := parseLoginStatus([]byte(tc.stdout), tc.stderr, tc.runErr)
			switch {
			case tc.expired:
				require.ErrorIs(t, err, ErrLoginExpired)
			case tc.err != "":
				require.Error(t, err)
				require.NotErrorIs(t, err, ErrLoginExpired)
				require.ErrorContains(t, err, tc.err)
			default:
				require.NoError(t, err)
				require.True(t, tc.want.Equal(got), "got %v", got)
			}
		})
	}
	// An object with neither "active" nor "profiles" is unknown, not
	// "logged out": a tsh whose output changed mustn't stop beam work.
	_, err := parseLoginStatus([]byte(`{"user":"captain"}`), "", nil)
	require.EqualError(t, err, `tsh status: parse output: neither active nor profiles in {"user":"captain"}`)
}

func TestLoginValidUntil(t *testing.T) {
	bin, calls := fakeTsh(t, "exit 99")
	until := time.Now().Add(10 * time.Minute).Truncate(time.Second)
	setStatus(t, bin, statusAt(until))
	got, err := NewTsh(bin).LoginValidUntil(t.Context())
	require.NoError(t, err)
	require.True(t, until.Equal(got), "got %v", got)
	require.Empty(t, calls(), "no tsh beams call")
	require.Equal(t, 1, statusCalls(t, bin))
	data, err := os.ReadFile(filepath.Join(filepath.Dir(bin), "status.log"))
	require.NoError(t, err)
	require.Equal(t, "status --format=json\n", string(data))

	setStatus(t, bin, `echo 'ERROR: Not logged in.' >&2; exit 1`)
	_, err = NewTsh(bin).LoginValidUntil(t.Context())
	require.ErrorIs(t, err, ErrLoginExpired)

	_, err = (&Holodeck{}).LoginValidUntil(t.Context())
	require.NoError(t, err)
}

// tsh never gets a controlling terminal: it leads a session of its own,
// and stdin is /dev/null for every call but Exec.
func TestTshHasNoTerminal(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("reads /proc")
	}
	dir := t.TempDir()
	out := filepath.Join(dir, "out")
	// The session id is /proc/<pid>/stat's 6th field.
	report := `echo "$(cut -d' ' -f6 /proc/$$/stat) $$ $(cat | wc -c)" >> '` + out + `'`
	bin, _ := fakeTsh(t, report+"; echo '[]'")
	setStatus(t, bin, report+"; "+validStatus)
	_, err := NewTsh(bin).Exists(t.Context(), orbit)
	require.NoError(t, err)
	data, err := os.ReadFile(out)
	require.NoError(t, err)
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	require.Len(t, lines, 2, "status, then beams ls")
	for _, l := range lines {
		f := strings.Fields(l)
		require.Equal(t, f[1], f[0], "tsh leads its own session: %q", l)
		require.Equal(t, "0", f[2], "stdin is empty: %q", l)
	}
}

// #102's acceptance: with the login expired, no `tsh beams` subcommand
// runs, every call returns ErrLoginExpired, and calls go through again
// once the login is valid.
func TestLoginGate(t *testing.T) {
	bin, calls := fakeTsh(t, `echo '[{"id":"warm-orbit"}]'`)
	setStatus(t, bin, statusAt(time.Now().Add(-time.Minute)))
	tsh := NewTsh(bin)
	tsh.Combadge = "./combadge.mjs"
	tsh.loginCheckEvery = 50 * time.Millisecond
	ctx := t.Context()
	local := filepath.Join(t.TempDir(), "f")

	gated := map[string]func() error{
		"create": func() error { _, err := tsh.Create(ctx); return err },
		"exists": func() error { _, err := tsh.Exists(ctx, orbit); return err },
		"probe": func() error {
			healthy, err := tsh.Probe(ctx, orbit)
			require.False(t, healthy)
			return err
		},
		"exec":   func() error { _, err := tsh.Exec(ctx, orbit, "true"); return err },
		"attach": func() error { _, err := tsh.AttachCombadge(ctx, orbit, 0); return err },
		"copy to": func() error {
			return tsh.CopyTo(ctx, orbit, local, "/tmp/x")
		},
		"copy from": func() error { return tsh.CopyFrom(ctx, orbit, "/tmp/x", local) },
		"size":      func() error { _, err := tsh.Size(ctx, orbit, "/tmp/x"); return err },
		"remove":    func() error { return tsh.Remove(ctx, orbit) },
		"start":     func() error { return tsh.StartCombadge(ctx, orbit, CombadgeSpec{CrewID: "sorell"}) },
	}
	for name, call := range gated {
		require.ErrorIs(t, call(), ErrLoginExpired, name)
	}
	require.Empty(t, calls(), "no tsh beams call while the login is expired")

	// The captain logs in again: within loginCheckEvery calls run.
	setStatus(t, bin, statusAt(time.Now().Add(time.Hour)))
	require.Eventually(t, func() bool {
		ok, err := tsh.Exists(ctx, orbit)
		return err == nil && ok
	}, 5*time.Second, 20*time.Millisecond)
	require.Equal(t, [][]string{{"beams", "ls", "--format=json"}}, calls())
}

// The gate trusts its state for loginCheckEvery, except that an expiry it
// knows of takes effect when it passes (LoginExpiryMargin early), without
// another status.
func TestLoginGateCachesState(t *testing.T) {
	bin, calls := fakeTsh(t, `echo '[]'`)
	setStatus(t, bin, statusAt(time.Now().Add(LoginExpiryMargin+2*time.Second)))
	tsh := NewTsh(bin)
	ctx := t.Context()
	for range 3 {
		_, err := tsh.Exists(ctx, orbit)
		require.NoError(t, err)
	}
	require.Equal(t, 1, statusCalls(t, bin), "one status a minute")
	time.Sleep(2500 * time.Millisecond)
	_, err := tsh.Exists(ctx, orbit)
	require.ErrorIs(t, err, ErrLoginExpired)
	require.Equal(t, 1, statusCalls(t, bin))
	require.Len(t, calls(), 3)
}

// A failed call reads the login again: a login that expired since (here,
// behind a cached state that said valid) turns the failure into
// ErrLoginExpired, and Probe reports health unknown, not a dead beam.
func TestLoginReadAfterFailure(t *testing.T) {
	const listed = `[ "$2" = ls ] && { echo '[{"id":"warm-orbit"}]'; exit 0; }`
	bin, calls := fakeTsh(t, listed+"\n"+`echo 'ERROR: ssh: handshake failed' >&2; exit 1`)
	tsh := NewTsh(bin)
	ctx := t.Context()
	_, err := tsh.Exists(ctx, orbit) // caches a valid login
	require.NoError(t, err)

	// The exec fails while the login is still valid: unhealthy.
	healthy, err := tsh.Probe(ctx, orbit)
	require.NoError(t, err)
	require.False(t, healthy)
	require.Equal(t, 2, statusCalls(t, bin), "read again after the failed exec")

	// The same failure with the login now expired: health unknown.
	setStatus(t, bin, `echo 'ERROR: Not logged in.' >&2; exit 1`)
	healthy, err = tsh.Probe(ctx, orbit)
	require.ErrorIs(t, err, ErrLoginExpired)
	require.ErrorContains(t, err, "handshake failed")
	require.False(t, healthy)
	require.Equal(t, 3, statusCalls(t, bin))
	require.Len(t, calls(), 5, "ls, ls, exec true, ls, exec true")

	// Now gated without running tsh beams.
	_, err = tsh.Exists(ctx, orbit)
	require.ErrorIs(t, err, ErrLoginExpired)
	require.Len(t, calls(), 5)
}

// #102's acceptance: a tsh that prompts for a password, reading stdin,
// never receives a command the bridge writes to an attach. The cached
// state says the login is valid; attach reads status again first.
func TestAttachNeverFeedsAPrompt(t *testing.T) {
	got := filepath.Join(t.TempDir(), "password")
	prompt := `[ "$2" = ls ] && { echo '[{"id":"warm-orbit"}]'; exit 0; }
printf 'Enter password for Teleport user captain:\n' >&2
read -r pw; printf '%s\n' "$pw" >> '` + got + `'; exit 1`
	bin, calls := fakeTsh(t, prompt)
	tsh := NewTsh(bin)
	ctx := t.Context()
	_, err := tsh.Exists(ctx, orbit) // caches a valid login
	require.NoError(t, err)

	setStatus(t, bin, statusAt(time.Now().Add(-time.Second)))
	s, err := tsh.AttachCombadge(ctx, orbit, 7)
	if err == nil {
		_, _ = io.WriteString(s, `{"type":"order","id":1}`+"\n")
		_ = s.Close()
		_ = s.Wait()
	}
	require.ErrorIs(t, err, ErrLoginExpired)
	_, statErr := os.Stat(got)
	require.True(t, errors.Is(statErr, os.ErrNotExist), "the prompt read something")
	require.Equal(t, [][]string{{"beams", "ls", "--format=json"}}, calls(), "attach never started")

	// Calls other than attach give tsh /dev/null: a prompt reads nothing.
	setStatus(t, bin, validStatus)
	tsh.loginCheckEvery = 0
	_, err = tsh.Size(ctx, orbit, "/tmp/x")
	require.Error(t, err)
	data, err := os.ReadFile(got)
	require.NoError(t, err)
	require.Equal(t, "\n", string(data))
}

// A canceled call still reads the login after failing, on a context of
// its own.
func TestLoginReadAfterCanceledCall(t *testing.T) {
	bin, _ := fakeTsh(t, `exec sleep 30`)
	tsh := NewTsh(bin)
	_, err := tsh.LoginValidUntil(t.Context())
	require.NoError(t, err)
	setStatus(t, bin, statusAt(time.Now().Add(-time.Second)))
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	_, err = tsh.Exists(ctx, orbit)
	require.ErrorIs(t, err, ErrLoginExpired)
	require.ErrorIs(t, err, context.DeadlineExceeded)
}

// A login counts as expired LoginExpiryMargin before its valid_until.
func TestLoginGateMargin(t *testing.T) {
	bin, calls := fakeTsh(t, `echo '[]'`)
	setStatus(t, bin, statusAt(time.Now().Add(LoginExpiryMargin-10*time.Second)))
	_, err := NewTsh(bin).Exists(t.Context(), orbit)
	require.ErrorIs(t, err, ErrLoginExpired)
	require.Empty(t, calls())
}
