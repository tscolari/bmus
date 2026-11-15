package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/tscolari/bmus/internal/bridge"
)

// `bmus bridge run --console-port 65536` fails before touching the
// home.
func TestConsolePortMustBeAPort(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("builds and runs the bmus binary")
	}
	bin, err := buildBinary()
	require.NoError(t, err)
	for _, port := range []string{"65536", "-1"} {
		t.Run(port, func(t *testing.T) {
			home := filepath.Join(t.TempDir(), "home")
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			out, err := (cli{bin: bin, home: home}).cmd(ctx, "bridge", "run", "--console-port="+port).CombinedOutput()
			var exitErr *exec.ExitError
			require.ErrorAs(t, err, &exitErr, "%s", out)
			require.Equal(t, exitError, exitErr.ExitCode(), "%s", out)
			require.Contains(t, string(out), "--console-port: console port "+port+": out of range 0-65535")
			require.NoDirExists(t, home)
		})
	}
}

// `bmus bridge run --hail-park-after` refuses a value combadge serve
// wouldn't take, naming the cap, before touching the home (#180).
func TestBridgeRunRefusesBadHailParkAfter(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("builds and runs the bmus binary")
	}
	bin, err := buildBinary()
	require.NoError(t, err)
	for value, want := range map[string]string{
		"600h": "--hail-park-after: 600h0m0s is over the cap of 596h31m23.647s (2^31-1 ms, combadge serve's limit)",
		"0":    "--hail-park-after: 0s is not positive",
		"-1m":  "--hail-park-after: -1m0s is not positive",
	} {
		t.Run(value, func(t *testing.T) {
			home := filepath.Join(t.TempDir(), "home")
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			out, err := (cli{bin: bin, home: home}).cmd(ctx, "bridge", "run", "--hail-park-after="+value).CombinedOutput()
			var exitErr *exec.ExitError
			require.ErrorAs(t, err, &exitErr, "%s", out)
			require.Equal(t, exitError, exitErr.ExitCode(), "%s", out)
			require.Contains(t, string(out), want)
			require.NoDirExists(t, home)
		})
	}
}

// `bmus bridge run --checkpoint-every` and `--checkpoint-turns` refuse
// values combadge serve wouldn't take, before touching the home (#145).
func TestBridgeRunRefusesBadCheckpointCadence(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("builds and runs the bmus binary")
	}
	bin, err := buildBinary()
	require.NoError(t, err)
	for flag, want := range map[string]string{
		"--checkpoint-every=600h": "--checkpoint-every: 600h0m0s is over the cap of 596h31m23.647s",
		"--checkpoint-every=-1m":  "--checkpoint-every: -1m0s is negative",
		"--checkpoint-turns=-3":   "--checkpoint-turns: -3 is negative",
	} {
		t.Run(flag, func(t *testing.T) {
			home := filepath.Join(t.TempDir(), "home")
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			out, err := (cli{bin: bin, home: home}).cmd(ctx, "bridge", "run", flag).CombinedOutput()
			var exitErr *exec.ExitError
			require.ErrorAs(t, err, &exitErr, "%s", out)
			require.Equal(t, exitError, exitErr.ExitCode(), "%s", out)
			require.Contains(t, string(out), want)
			require.NoDirExists(t, home)
		})
	}
}

// The full console flow through the real CLI and a real holodeck bridge:
// `bmus console` prints the token's URL, the token file backs it, and the
// printed token actually exchanges for a session over the wire. Then,
// with the bridge stopped, `bmus console` exits 3 like every other
// command.
func TestConsoleCommand(t *testing.T) {
	t.Parallel()
	e := newE2E(t)
	e.console = true
	br := e.startBridge(t)

	var st bridge.BridgeStatus
	e.runJSON(t, &st, "status")
	addr := st.Console
	host, _, err := net.SplitHostPort(addr)
	require.NoError(t, err)
	require.Equal(t, "127.0.0.1", host)

	out := e.run(t, "console", "--no-open")
	url := strings.TrimSpace(out)
	want := "http://" + addr + "/#t="
	require.True(t, strings.HasPrefix(url, want), url)
	token := strings.TrimPrefix(url, want)
	require.Len(t, token, 64)

	tokenFile := filepath.Join(e.home, "console.token")
	data, err := os.ReadFile(tokenFile)
	require.NoError(t, err)
	require.Equal(t, token, strings.TrimSpace(string(data)))

	body, err := json.Marshal(map[string]string{"token": token})
	require.NoError(t, err)
	req, err := http.NewRequest(http.MethodPost, "http://"+addr+"/api/session", bytes.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Origin", "http://"+addr)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.NotEmpty(t, resp.Cookies())

	br.sigterm(t)
	require.NoFileExists(t, tokenFile)
	_, err = e.cmd(t.Context(), "console", "--no-open").Output()
	var exitErr *exec.ExitError
	require.ErrorAs(t, err, &exitErr)
	require.Equal(t, exitNotRunning, exitErr.ExitCode())
}

// With --no-console, `bmus console` says the console is off.
func TestConsoleCommandConsoleOff(t *testing.T) {
	t.Parallel()
	e := newE2E(t)
	e.startBridge(t)
	out, err := e.cmd(t.Context(), "console", "--no-open").CombinedOutput()
	var exitErr *exec.ExitError
	require.ErrorAs(t, err, &exitErr, "%s", out)
	require.Equal(t, exitError, exitErr.ExitCode(), "%s", out)
	require.Contains(t, string(out), "the console is off")
}

// A crew member added through the console's POST /api/crew shows in
// `bmus crew list`.
func TestConsoleAddCrewInCrewList(t *testing.T) {
	t.Parallel()
	e := newE2E(t)
	e.console = true
	e.startBridge(t)
	var st bridge.BridgeStatus
	e.runJSON(t, &st, "status")
	addr := st.Console
	url := strings.TrimSpace(e.run(t, "console", "--no-open"))
	token := strings.TrimPrefix(url, "http://"+addr+"/#t=")

	body, err := json.Marshal(map[string]string{"token": token})
	require.NoError(t, err)
	req, err := http.NewRequest(http.MethodPost, "http://"+addr+"/api/session", bytes.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Origin", "http://"+addr)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var sess struct {
		CSRF string `json:"csrf"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&sess))

	req, err = http.NewRequest(http.MethodPost, "http://"+addr+"/api/crew",
		strings.NewReader(`{"id":"sorell","name":"Sorell","rank":"Lt"}`))
	require.NoError(t, err)
	req.Header.Set("Origin", "http://"+addr)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-BMUS-CSRF", sess.CSRF)
	for _, c := range resp.Cookies() {
		req.AddCookie(c)
	}
	added, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer added.Body.Close()
	require.Equal(t, http.StatusCreated, added.StatusCode)
	var cv bridge.CrewView
	require.NoError(t, json.NewDecoder(added.Body).Decode(&cv))

	require.Regexp(t, `(?m)^sorell\s+Sorell\s+Lt\s`, e.run(t, "crew", "list"))
	var crew bridge.CrewListResult
	e.runJSON(t, &crew, "crew", "list")
	require.Len(t, crew.Crew, 1)
	// The tour may have moved on (a beam, a status) since.
	got := crew.Crew[0]
	require.Equal(t, [4]string{cv.ID, cv.Name, cv.Rank, cv.HarnessSource}, [4]string{got.ID, got.Name, got.Rank, got.HarnessSource})
}
