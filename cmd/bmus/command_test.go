package main

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/tscolari/bmus/internal/bridge"
)

// #147's acceptance: the captain sets Command's guidance and show prints
// it; show prints nothing once it is cleared.
func TestCommandGuidance(t *testing.T) {
	t.Parallel()
	e := newE2E(t)
	e.startBridge(t)
	require.Empty(t, e.run(t, "command", "guidance", "show"), "none set")
	require.Equal(t, "prefer small PRs\n", e.run(t, "command", "guidance", "set", "prefer small PRs"))
	require.Equal(t, "prefer small PRs\n", e.run(t, "command", "guidance", "show"))
	var res bridge.CommandGuidanceResult
	e.runJSON(t, &res, "command", "guidance", "show")
	require.Equal(t, "prefer small PRs", res.Guidance)

	out, err := e.cmd(t.Context(), "command", "guidance", "set", strings.Repeat("x", bridge.MaxCommandGuidance+1)).CombinedOutput()
	require.Error(t, err)
	require.Contains(t, string(out), "over the 4096 limit")
	require.Equal(t, "prefer small PRs\n", e.run(t, "command", "guidance", "show"), "a refused set changes nothing")

	require.Empty(t, e.run(t, "command", "guidance", "set", ""))
	require.Empty(t, e.run(t, "command", "guidance", "show"))
}
