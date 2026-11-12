package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/tscolari/bmus/internal/bridge"
)

// #95's acceptance: the roster turns standing orders on, the captain turns
// them off, and the bridge comes back with them off.
func TestStandingOrdersSurviveRestart(t *testing.T) {
	t.Parallel()
	e := newE2E(t)
	require.NoError(t, os.WriteFile(filepath.Join(e.home, bridge.RosterFile), []byte("standing_orders: true\n"), 0o600))
	br := e.startBridge(t)
	require.Equal(t, "on\n", e.run(t, "standing-orders"))
	var st bridge.BridgeStatus
	e.runJSON(t, &st, "status")
	require.True(t, st.StandingOrders)
	require.Equal(t, "off\n", e.run(t, "standing-orders", "off"))
	var res bridge.StandingOrdersResult
	e.runJSON(t, &res, "standing-orders")
	require.False(t, res.On)
	require.Regexp(t, `(?m)^standing orders +off$`, e.run(t, "status"))
	_, err := e.cmd(t.Context(), "standing-orders", "maybe").CombinedOutput()
	require.Error(t, err)
	br.sigterm(t)

	br = e.startBridge(t)
	require.Equal(t, "off\n", e.run(t, "standing-orders"))
	br.sigterm(t)
}

// #95's acceptance: `bridge run` refuses a roster with an unknown key,
// naming it, before it starts anything.
func TestBridgeRunRefusesBadRoster(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("builds and runs the bmus binary")
	}
	bin, err := buildBinary()
	require.NoError(t, err)
	dir := t.TempDir()
	roster := filepath.Join(dir, "bad.yaml")
	require.NoError(t, os.WriteFile(roster, []byte("standing_orders: true\nstandin_orders: false\n"), 0o600))

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	c := cli{bin: bin, home: filepath.Join(dir, "home")}
	for args, want := range map[[2]string]string{
		{"--roster", roster}:                             "field standin_orders not found",
		{"--roster", filepath.Join(dir, "missing.yaml")}: "--roster: stat " + filepath.Join(dir, "missing.yaml"),
	} {
		out, err := c.cmd(ctx, "bridge", "run", args[0], args[1]).CombinedOutput()
		var exitErr *exec.ExitError
		require.ErrorAs(t, err, &exitErr, "%s", out)
		require.Equal(t, exitError, exitErr.ExitCode(), "%s", out)
		require.Contains(t, string(out), want)
	}
	_, err = os.Stat(c.home)
	require.ErrorIs(t, err, os.ErrNotExist, "nothing started")
}
