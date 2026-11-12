package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/tscolari/bmus/internal/bridge"
	"github.com/tscolari/bmus/internal/regs"
	"github.com/tscolari/bmus/internal/store"
)

// #148's acceptance: `bmus regs init` writes the default, and a second run
// fails without changing the file.
func TestRegsInit(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("builds and runs the bmus binary")
	}
	bin, err := buildBinary()
	require.NoError(t, err)
	c := cli{bin: bin, home: filepath.Join(t.TempDir(), "home")}
	path := filepath.Join(c.home, regs.File)
	require.Equal(t, "wrote "+path+"\n", c.run(t, "regs", "init"))
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, regs.Default, data)
	_, found, err := regs.LoadRegs(path)
	require.NoError(t, err)
	require.True(t, found)

	require.NoError(t, os.WriteFile(path, []byte("classify: []\n"), 0o600))
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	out, err := c.cmd(ctx, "regs", "init").CombinedOutput()
	var exitErr *exec.ExitError
	require.ErrorAs(t, err, &exitErr, "%s", out)
	require.Contains(t, string(out), path+" already exists")
	data, err = os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, "classify: []\n", string(data))
}

// #148's acceptance: `bridge run` refuses a regs.yaml with a bad regex,
// naming the rule, before it starts anything.
func TestBridgeRunRefusesBadRegs(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("builds and runs the bmus binary")
	}
	bin, err := buildBinary()
	require.NoError(t, err)
	dir := t.TempDir()
	c := cli{bin: bin, home: filepath.Join(dir, "home")}
	require.NoError(t, os.MkdirAll(c.home, 0o700))
	bad := "classify:\n  - {tag: x, tool: Bash}\ncaptain_only:\n  - tag: x\n  - match: 'git (push'\n"
	flagged := filepath.Join(dir, "bad.yaml")
	require.NoError(t, os.WriteFile(flagged, []byte(bad), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(c.home, regs.File), []byte(bad), 0o600))

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	for _, tc := range []struct {
		args []string
		want string
	}{
		{nil, filepath.Join(c.home, regs.File) + `: captain_only[1]: match "git (push": error parsing regexp`},
		{[]string{"--regs", flagged}, flagged + `: captain_only[1]: match "git (push"`},
		{[]string{"--regs", filepath.Join(dir, "missing.yaml")}, "--regs: stat " + filepath.Join(dir, "missing.yaml")},
	} {
		out, err := c.cmd(ctx, append([]string{"bridge", "run"}, tc.args...)...).CombinedOutput()
		var exitErr *exec.ExitError
		require.ErrorAs(t, err, &exitErr, "%s", out)
		require.Equal(t, exitError, exitErr.ExitCode(), "%s", out)
		require.Contains(t, string(out), tc.want)
	}
	_, err = os.Stat(filepath.Join(c.home, store.DBFile))
	require.ErrorIs(t, err, os.ErrNotExist, "nothing started")
}

// #149's acceptance, auto-allow end to end: with `bmus regs init` run, a
// Bash `go test ./...` hail is decided by policy without the captain, the
// mission finishes, and `bmus hails list` shows the hail decided. Status
// has no "regs.yaml not configured" line.
func TestHailAutoAllowEndToEnd(t *testing.T) {
	t.Parallel()
	e := newE2E(t)
	e.script = filepath.Join(t.TempDir(), "go-test.json")
	require.NoError(t, os.WriteFile(e.script, []byte(`[
		{"at_pct": 40, "hail": {"category": "tool_approval", "question": "Run go test?", "options": ["allow", "deny"],
			"context": {"tool": "Bash", "input": {"command": "go test ./..."}}}},
		{"at_pct": 100, "report": {"summary": "tested"}}]`), 0o600))
	e.run(t, "regs", "init")
	e.startBridge(t)
	f := e.follow(t)
	e.run(t, "crew", "add", "sorell", "--name", "Sorell", "--rank", "Lieutenant", "--division", "science")
	orderID := e.createOrder(t)

	log := f.until(t, 60*time.Second, func(le bridge.LogEntryView) bool { return le.Kind == "done" && le.OrderID == orderID })
	require.Contains(t, texts(log), "hail: hail H-0001 decided: allow")
	require.NotContains(t, strings.Join(texts(log), "\n"), "for the captain")
	f.stop(t)

	var hails bridge.HailsListResult
	e.runJSON(t, &hails, "hails", "list", "--status", "all")
	require.Len(t, hails.Hails, 1)
	h := hails.Hails[0]
	require.Equal(t, store.HailDecided, h.Status)
	require.Equal(t, "policy", h.DecidedBy)
	require.Equal(t, "auto", h.Route)
	require.Equal(t, "allow", h.Decision)
	require.Regexp(t, `(?m)^H-0001\s+`+orderID+`\s+sorell\s+tool_approval\s+decided\s`, e.run(t, "hails", "list", "--status", "decided"))

	var st bridge.BridgeStatus
	e.runJSON(t, &st, "status")
	require.Equal(t, []string{}, st.Info)
	require.NotContains(t, e.run(t, "status"), "regs.yaml")
}

// #153: `bmus hails countermand` overrides the default option a captain
// timeout took, inside the countermand window: decided_by turns captain,
// original_decided_by stays policy, and hails list --json shows the
// window. The order is done, so the result notes the agent won't be told
// and the log says the correction wasn't delivered. A hail that doesn't
// exist is an error.
func TestHailCountermandEndToEnd(t *testing.T) {
	t.Parallel()
	e := newE2E(t)
	e.script = filepath.Join(t.TempDir(), "deploy.json")
	require.NoError(t, os.WriteFile(e.script, []byte(`[
		{"at_pct": 40, "hail": {"category": "tool_approval", "question": "Run make deploy?", "options": ["allow", "deny"],
			"context": {"tool": "Bash", "input": {"command": "make deploy"}}}},
		{"at_pct": 100, "report": {"summary": "deployed"}}]`), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(e.home, regs.File), []byte(`command_may_answer:
  countermand_window: 10m
escalation:
  on_timeout: default_option
  captain_timeout: 1s
  default_option: 0
`), 0o600))
	e.startBridge(t)
	f := e.follow(t)
	e.run(t, "crew", "add", "sorell", "--name", "Sorell", "--rank", "Lieutenant", "--division", "science")
	orderID := e.createOrder(t)

	log := f.until(t, 60*time.Second, func(le bridge.LogEntryView) bool { return le.Kind == "done" && le.OrderID == orderID })
	require.Contains(t, texts(log), "hail: hail H-0001 decided on the captain timeout: allow")
	f.stop(t)

	var h bridge.HailsCountermandResult
	e.runJSON(t, &h, "hails", "countermand", "H-0001", "--option", "1", "--reason", "staging is frozen", "--json")
	require.Equal(t, "captain", h.DecidedBy)
	require.Equal(t, "policy", h.OriginalDecidedBy)
	require.Equal(t, "deny", h.Decision)
	require.Equal(t, "staging is frozen", h.Rationale)
	require.NotNil(t, h.CountermandUntil)
	require.NotNil(t, h.CountermandedAt)
	// The order is done: its mission has ended and the agent isn't told.
	note := "order " + orderID + " is done: no live run, the agent won't be told"
	require.Equal(t, note, h.Note)
	require.Equal(t, "H-0001 countermanded: allow\nnote: "+note+"\n",
		e.run(t, "hails", "countermand", "H-0001", "--decision", "allow"))
	require.Contains(t, e.run(t, "log", "tail"), "correction for hail H-0001 not delivered: the order's mission has ended")

	var hails bridge.HailsListResult
	e.runJSON(t, &hails, "hails", "list", "--status", "decided")
	require.Len(t, hails.Hails, 1)
	require.Equal(t, "captain", hails.Hails[0].DecidedBy)
	require.Equal(t, "allow", hails.Hails[0].Decision)

	// #156: hails metrics --json prints hails.metrics's result as is. The
	// timeout's default_option is policy's, so auto-resolved, and the
	// captain overrode it.
	since := hails.Hails[0].CreatedAt.Add(-time.Hour).Format(time.RFC3339Nano)
	out := e.run(t, "hails", "metrics", "--since", since, "--json")
	from, err := time.Parse(time.RFC3339Nano, since)
	require.NoError(t, err)
	var raw json.RawMessage
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	require.NoError(t, bridge.Call(ctx, filepath.Join(e.home, bridge.SocketFile), "hails.metrics", bridge.HailsMetricsArgs{Since: &from}, &raw))
	var indented bytes.Buffer
	require.NoError(t, json.Indent(&indented, raw, "", "  "))
	require.Equal(t, indented.String()+"\n", out)
	var m bridge.HailsMetricsResult
	require.NoError(t, json.Unmarshal([]byte(out), &m))
	one := 1.0
	require.Equal(t, 1, m.Total)
	require.Equal(t, 1, m.Decided)
	require.Equal(t, 1, m.Countermanded)
	require.Equal(t, map[string]int{"policy": 1, "command": 0, "captain": 0}, m.ByOriginalDecider)
	require.Equal(t, &one, m.AutoResolvedShare)
	require.Equal(t, &one, m.CaptainOverrides)
	require.NotNil(t, m.MedianTimeToAnswerMS)
	require.GreaterOrEqual(t, *m.MedianTimeToAnswerMS, int64(1000), "the 1s captain_timeout")
	require.Equal(t, m.MedianTimeToAnswerMS, m.P90TimeToAnswerMS)
	require.Regexp(t, `(?m)^auto-resolved\s+100\.0% \(1 of 1\)$`, e.run(t, "hails", "metrics"))

	var exitErr *exec.ExitError
	_, err = e.cmd(t.Context(), "hails", "countermand", "H-0009", "--option", "0").Output()
	require.ErrorAs(t, err, &exitErr)
	_, err = e.cmd(t.Context(), "hails", "countermand", "H-0001").Output()
	require.ErrorAs(t, err, &exitErr, "needs --option or --decision")
}
