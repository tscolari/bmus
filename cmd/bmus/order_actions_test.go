package main

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tscolari/bmus/internal/bridge"
	"github.com/tscolari/bmus/internal/store"
)

// fail runs a command that must exit 1 and returns its stderr.
func (c cli) fail(t *testing.T, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	var stderr bytes.Buffer
	cmd := c.cmd(ctx, args...)
	cmd.Stderr = &stderr
	err := cmd.Run()
	var exitErr *exec.ExitError
	require.ErrorAs(t, err, &exitErr, "bmus %v", args)
	require.Equal(t, exitError, exitErr.ExitCode(), "%s", stderr.String())
	return stderr.String()
}

// #101's acceptance through the CLI: `bmus order recall` on an order
// without a division fails, pointing at `order route`; `order belay`
// settles it belayed; `order route` and `order withdraw` move an unclaimed
// order.
func TestOrderBelayRecallWithdrawRoute(t *testing.T) {
	t.Parallel()
	e := newE2E(t)
	e.startBridge(t)
	e.run(t, "crew", "add", "sorell", "--name", "Sorell")
	dir := t.TempDir()
	directed := filepath.Join(dir, "directed.yaml")
	require.NoError(t, os.WriteFile(directed, []byte(`title: no division
assignee: sorell
repos:
  - name: app
    base_ref: main
body: Walk the script.
`), 0o600))
	require.Equal(t, "ORD-0001\n", e.run(t, "order", "create", directed))
	require.EventuallyWithT(t, func(ct *assert.CollectT) {
		var hails bridge.HailsListResult
		e.runJSON(ct, &hails, "hails", "list", "--status", "open")
		require.Len(ct, hails.Hails, 1)
	}, 30*time.Second, 200*time.Millisecond)

	stderr := e.fail(t, "order", "recall", "ORD-0001")
	require.Contains(t, stderr, "order ORD-0001 has no division")
	require.Contains(t, stderr, "`bmus order route --division`")
	require.Equal(t, "order ORD-0001 belay requested\n", e.run(t, "order", "belay", "ORD-0001", "--reason", "scrapped"))
	require.EventuallyWithT(t, func(ct *assert.CollectT) {
		var orders bridge.OrderListResult
		e.runJSON(ct, &orders, "order", "list")
		require.Equal(ct, store.OrderBelayed, orders.Orders[0].State)
	}, 30*time.Second, 200*time.Millisecond)

	pool := filepath.Join(dir, "pool.yaml")
	require.NoError(t, os.WriteFile(pool, []byte(`title: pool
division: ops
repos:
  - name: app
    base_ref: main
body: Walk the script.
`), 0o600))
	require.Equal(t, "ORD-0002\n", e.run(t, "order", "create", pool))
	require.Contains(t, e.fail(t, "order", "route", "ORD-0002"), "--assignee or --division is required")
	require.Equal(t, "order ORD-0002 routed to the eng pool\n", e.run(t, "order", "route", "ORD-0002", "--division", "eng"))
	var view bridge.OrderView
	e.runJSON(t, &view, "order", "route", "ORD-0002", "--division", "ops")
	require.Equal(t, []string{store.OrderPool, "", "ops"}, []string{view.State, view.Assignee, view.Division})
	require.Equal(t, "order ORD-0002 withdrawn\n", e.run(t, "order", "withdraw", "ORD-0002", "--reason", "duplicate"))
	require.Contains(t, e.fail(t, "order", "withdraw", "ORD-0002"), "order ORD-0002 is withdrawn, too late to withdraw")
	require.Contains(t, e.fail(t, "order", "belay", "ORD-0001"), "order ORD-0001 is belayed, too late to belay")
}

// #120's acceptance through the CLI: `bmus order budget` changes an open
// order's budget, and order list's USAGE cell shows it; an ended order is
// refused.
func TestOrderBudget(t *testing.T) {
	t.Parallel()
	e := newE2E(t)
	e.startBridge(t)
	file := filepath.Join(t.TempDir(), "pool.yaml")
	require.NoError(t, os.WriteFile(file, []byte(`title: capped
division: ops
budget_tokens: 2000000
repos:
  - name: app
    base_ref: main
`), 0o600))
	require.Equal(t, "ORD-0001\n", e.run(t, "order", "create", file))
	require.Regexp(t, `(?m)^ID\s+STATE\s+PROGRESS\s+USAGE\s+ASSIGNEE\s+DIVISION\s+TITLE\nORD-0001\s+pool\s+-\s+0/2M\s+-\s+ops\s+capped\n`,
		e.run(t, "order", "list"))

	require.Contains(t, e.fail(t, "order", "budget", "ORD-0001"), "--tokens or --turns is required")
	require.Contains(t, e.fail(t, "order", "budget", "ORD-0001", "--turns", "-1"), "budget turns must not be negative")
	require.Equal(t, "order ORD-0001 limit now 2M tokens (order), 40 turns (order)\n",
		e.run(t, "order", "budget", "ORD-0001", "--turns", "40"))
	require.Regexp(t, `(?m)^ORD-0001\s+pool\s+-\s+0/2M 0/40t\s+-\s+ops\s+capped\n`, e.run(t, "order", "list"))
	var view bridge.OrderView
	e.runJSON(t, &view, "order", "budget", "ORD-0001", "--tokens", "0", "--json")
	require.Equal(t, []int64{0, 40, 0}, []int64{view.BudgetTokens, view.BudgetTurns, view.BudgetExtended})
	require.Equal(t, bridge.BudgetSource{Tokens: bridge.SourceDefault, Turns: bridge.SourceOrder}, view.BudgetSource)
	require.Equal(t, bridge.UsageView{}, view.Usage)
	require.Regexp(t, `(?m)^ORD-0001\s+pool\s+-\s+0/40t\s+-\s+ops\s+capped\n`, e.run(t, "order", "list"))

	e.run(t, "order", "withdraw", "ORD-0001")
	require.Contains(t, e.fail(t, "order", "budget", "ORD-0001", "--tokens", "1000"),
		"order ORD-0001 is withdrawn, too late to change its budget")
	require.Contains(t, e.fail(t, "order", "budget", "ORD-0404", "--tokens", "1000"), "ORD-0404")
}

// order list's USAGE cell: tokens and turns against their limits, each
// only with a limit, "-" with neither (#120).
func TestUsageCell(t *testing.T) {
	t.Parallel()
	used := bridge.UsageView{InputTokens: 400_000, OutputTokens: 20_000, Turns: 42}
	require.Equal(t, "420k/1M 42/150t", usageCell(bridge.OrderView{BudgetTokens: 1_000_000, BudgetTurns: 150, Usage: used}))
	require.Equal(t, "420k/1M", usageCell(bridge.OrderView{BudgetTokens: 1_000_000, Usage: used}))
	require.Equal(t, "42/150t", usageCell(bridge.OrderView{BudgetTurns: 150, Usage: used}))
	require.Equal(t, "-", usageCell(bridge.OrderView{Usage: used}))
}

// order budget's output: each field's limit and source, and the
// extensions, which multiply it (#120).
func TestBudgetLine(t *testing.T) {
	t.Parallel()
	v := bridge.OrderView{ID: "ORD-0001", BudgetTokens: 4_000_000, BudgetSource: bridge.BudgetSource{Tokens: "order", Turns: "default"}}
	require.Equal(t, "order ORD-0001 limit now 4M tokens (order), unlimited turns", budgetLine(v))
	v.BudgetExtended = 1
	require.Equal(t, "order ORD-0001 limit now 4M tokens (order), unlimited turns, with 1 extension", budgetLine(v))
	v.BudgetExtended = 3
	require.Equal(t, "order ORD-0001 limit now 4M tokens (order), unlimited turns, with 3 extensions", budgetLine(v))
}

// order list's STATE cell gives a split order's open children (#155).
func TestStateCell(t *testing.T) {
	t.Parallel()
	require.Equal(t, "split (2 open)", stateCell(bridge.OrderView{State: store.OrderSplit, OpenChildren: 2}))
	require.Equal(t, "done", stateCell(bridge.OrderView{State: store.OrderDone}))
}

// order belay's output says how many open child orders the belay
// reaches (#146): the CLI has no confirm prompt.
func TestBelayLine(t *testing.T) {
	t.Parallel()
	line := func(n int) string {
		return belayLine(bridge.OrderBelayResult{OrderView: bridge.OrderView{ID: "ORD-0001"}, Children: n})
	}
	require.Equal(t, "order ORD-0001 belay requested", line(0))
	require.Equal(t, "order ORD-0001 belay requested, and its 1 open child order", line(1))
	require.Equal(t, "order ORD-0001 belay requested, and its 2 open child orders", line(2))
	split := bridge.OrderBelayResult{OrderView: bridge.OrderView{ID: "ORD-0001", State: store.OrderBelayed}, Children: 2, Belayed: true}
	require.Equal(t, "order ORD-0001 belayed, and its 2 open child orders", belayLine(split))
}
