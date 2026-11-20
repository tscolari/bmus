package bridge

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dbos-inc/dbos-transact-golang/dbos"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tscolari/bmus/internal/comms"
	"github.com/tscolari/bmus/internal/regs"
	"github.com/tscolari/bmus/internal/store"
)

// defaultRegs is the regs.yaml `bmus regs init` writes, loaded.
func defaultRegs(t *testing.T) *regs.Config {
	t.Helper()
	path := filepath.Join(t.TempDir(), regs.File)
	require.NoError(t, os.WriteFile(path, regs.Default, 0o600))
	cfg, found, err := regs.LoadRegs(path)
	require.NoError(t, err)
	require.True(t, found)
	return &cfg
}

// autoGoTest is the default regs.yaml's rule that auto-allows go test.
const autoGoTest = `auto_allow[0]: tool "Bash", match "go test \\./\\.\\.\\."`

// toolHail is a holodeck hail step body: a Bash tool_approval hail for
// command, with the sdk harness's {tool, input} context.
func toolHail(command string) map[string]any {
	return map[string]any{"category": comms.CategoryToolApproval, "question": "Run " + command + "?",
		"options": []string{comms.OptionAllow, comms.OptionDeny},
		"context": map[string]any{"tool": "Bash", "input": map[string]any{"command": command}}}
}

// hailsScript writes a holodeck script that raises each hail in turn, then
// reports.
func hailsScript(t *testing.T, hails ...map[string]any) string {
	t.Helper()
	var steps []map[string]any
	for i, h := range hails {
		steps = append(steps, map[string]any{"at_pct": 10 + i, "hail": h})
	}
	steps = append(steps, map[string]any{"at_pct": 100, "report": map[string]any{"summary": "done after the hails"}})
	data, err := json.Marshal(steps)
	require.NoError(t, err)
	script := filepath.Join(t.TempDir(), "hails.json")
	require.NoError(t, os.WriteFile(script, data, 0o600))
	return script
}

// hailResult waits for hail id's workflow to end and returns its result.
func hailResult(t *testing.T, d *Daemon, id string) HailResult {
	t.Helper()
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		require.Len(c, workflows(c, d, hailWorkflowID(id)), 1)
	}, 30*time.Second, 20*time.Millisecond)
	h, err := dbos.RetrieveWorkflow[HailResult](d.dbos, hailWorkflowID(id))
	require.NoError(t, err)
	res, err := h.GetResult(dbos.WithHandleTimeout(30 * time.Second))
	require.NoError(t, err)
	return res
}

// waitLog waits for a ship's log entry whose text starts with prefix
// and returns it.
func (hd *hdaemon) waitLog(t *testing.T, prefix string) store.LogEntry {
	t.Helper()
	var found store.LogEntry
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		for _, e := range hd.logEntries(c) {
			if strings.HasPrefix(e.Text, prefix) {
				found = e
				return
			}
		}
		require.Fail(c, "no entry", prefix)
	}, 30*time.Second, 20*time.Millisecond)
	return found
}

func (hd *hdaemon) hail(t require.TestingT, id string) store.Hail {
	h, err := hd.d.store.GetHail(context.Background(), id)
	require.NoError(t, err)
	return h
}

// hailSteps are the holodeck step log's hail steps, in order.
func hailSteps(t *testing.T, stepLog string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, e := range stepLogEntries(t, stepLog) {
		if e["action"] == "hail" {
			out = append(out, e)
		}
	}
	return out
}

// #149's acceptance, auto-allow: with `bmus regs init`'s regs.yaml, a Bash
// `go test ./...` hail is decided by policy in one step, with the rule as
// rationale and no countermand window; the mission relays decided_by
// policy to the beam.
func TestHailAutoAllow(t *testing.T) {
	t.Parallel()
	b := newHolodeckBridge(t, hailsScript(t, toolHail("go test ./...")))
	b.regs = defaultRegs(t)
	hd := b.start(t)
	hd.addCrew(t)
	id := hd.createOrder(t)

	require.Equal(t, HailResult{Decided: true, By: comms.DecidedByPolicy}, hailResult(t, hd.d, "H-0001"))
	require.Equal(t, []string{"classify", "decide-policy"}, hailStepNames(t, hd.d, "H-0001"))
	require.Equal(t, MissionDone, missionResult(t, hd.d, id).Status)

	h := hd.hail(t, "H-0001")
	require.Equal(t, store.HailDecided, h.Status)
	require.Equal(t, comms.DecidedByPolicy, h.DecidedBy)
	require.Equal(t, comms.DecidedByPolicy, h.OriginalDecidedBy)
	require.Equal(t, comms.OptionAllow, h.Decision)
	require.Equal(t, regs.RouteAuto, h.Route)
	require.Equal(t, autoGoTest, h.Rationale)
	require.Nil(t, h.CountermandUntil)

	steps := hailSteps(t, b.stepLog)
	require.Len(t, steps, 1)
	require.Equal(t, comms.OptionAllow, steps[0]["decision"])
	require.Equal(t, comms.DecidedByPolicy, steps[0]["decided_by"])

	var texts []string
	for _, e := range hd.logEntries(t) {
		if e.Kind == logHail {
			texts = append(texts, e.Text)
		}
	}
	require.Equal(t, []string{"Run go test ./...?", "hail H-0001 decided: allow"}, texts, "never routed to the captain")
}

// #149's acceptance, captain route: a `git push` hail goes to the captain,
// logged with the captain_only rule, and AnswerHail decides it as before.
func TestHailCaptainRoute(t *testing.T) {
	t.Parallel()
	b := newHolodeckBridge(t, hailsScript(t, toolHail("git push origin main")))
	b.regs = defaultRegs(t)
	hd := b.start(t)
	hd.addCrew(t)
	id := hd.createOrder(t)

	hd.waitHail(t, "H-0001")
	e := hd.waitLog(t, "hail H-0001 for the captain")
	require.Equal(t, `hail H-0001 for the captain: captain_only[1]: tag "external_effect"`, e.Text)
	requirePending(t, hd.d, hailWorkflowID("H-0001"))
	require.Equal(t, regs.RouteCaptain, hd.hail(t, "H-0001").Route)

	deny := 1
	require.NoError(t, hd.d.AnswerHail(t.Context(), "H-0001", "", &deny))
	require.Equal(t, HailResult{Decided: true, By: comms.DecidedByCaptain}, hailResult(t, hd.d, "H-0001"))
	require.Equal(t, MissionDone, missionResult(t, hd.d, id).Status)
	h := hd.hail(t, "H-0001")
	require.Equal(t, comms.DecidedByCaptain, h.DecidedBy)
	require.Equal(t, comms.OptionDeny, h.Decision)
	steps := hailSteps(t, b.stepLog)
	require.Len(t, steps, 1)
	require.Equal(t, comms.DecidedByCaptain, steps[0]["decided_by"])
}

// #149's acceptance, unconfigured bridge: without regs.yaml even a hail
// auto_allow would take goes to the captain, logged "regs.yaml not
// configured", and status shows it as information, never as a reason.
func TestHailUnconfigured(t *testing.T) {
	t.Parallel()
	b := newHolodeckBridge(t, hailsScript(t, toolHail("go test ./...")))
	hd := b.start(t)
	hd.addCrew(t)
	s, err := hd.d.Status(t.Context())
	require.NoError(t, err)
	require.Equal(t, ConditionGreen, s.Condition)
	require.Equal(t, []string{"regs.yaml not configured"}, s.Info)
	id := hd.createOrder(t)

	hd.waitHail(t, "H-0001")
	hd.waitLog(t, "hail H-0001 for the captain: regs.yaml not configured")
	s, err = hd.d.Status(t.Context())
	require.NoError(t, err)
	require.Equal(t, ConditionYellow, s.Condition)
	require.Equal(t, []string{"sorell hailing"}, s.Reasons, "the hail, not regs.yaml")

	allow := 0
	require.NoError(t, hd.d.AnswerHail(t.Context(), "H-0001", "", &allow))
	require.Equal(t, HailResult{Decided: true, By: comms.DecidedByCaptain}, hailResult(t, hd.d, "H-0001"))
	require.Equal(t, MissionDone, missionResult(t, hd.d, id).Status)
}

// #149's acceptance, timeout with default_option: a routine
// captain-routed hail takes the default once captain_timeout passes, with
// countermand_until set, while a `git push` hail holds for the captain.
func TestHailTimeoutDefaultOption(t *testing.T) {
	t.Parallel()
	b := newHolodeckBridge(t, hailsScript(t, toolHail("make deploy"), toolHail("git push")))
	b.regs = defaultRegs(t)
	deny := 1
	b.regs.Escalation = regs.Escalation{CaptainTimeout: time.Second, OnTimeout: regs.OnTimeoutDefaultOption,
		DefaultOption: &deny}
	hd := b.start(t)
	hd.addCrew(t)
	id := hd.createOrder(t)

	require.Equal(t, HailResult{Decided: true, By: comms.DecidedByPolicy}, hailResult(t, hd.d, "H-0001"))
	h := hd.hail(t, "H-0001")
	require.Equal(t, store.HailDecided, h.Status)
	require.Equal(t, comms.DecidedByPolicy, h.DecidedBy)
	require.Equal(t, comms.OptionDeny, h.Decision)
	require.Equal(t, regs.RouteCaptain, h.Route)
	require.Contains(t, h.Rationale, "captain timeout: unanswered after")
	require.NotNil(t, h.DecidedAt)
	require.NotNil(t, h.CountermandUntil)
	require.WithinDuration(t, h.DecidedAt.Add(regs.DefaultCountermandWindow), *h.CountermandUntil, time.Millisecond)
	hd.waitLog(t, "hail H-0001 decided on the captain timeout: deny")

	// The git push hail is captain-only: it holds, and stays open.
	hd.waitHail(t, "H-0002")
	e := hd.waitLog(t, "hail H-0002 unanswered after")
	require.Contains(t, e.Text, "holding for the captain (default_option does not apply to a captain-only hail)")
	require.Equal(t, store.HailOpen, hd.hail(t, "H-0002").Status)
	requirePending(t, hd.d, hailWorkflowID("H-0002"))

	allow := 0
	require.NoError(t, hd.d.AnswerHail(t.Context(), "H-0002", "", &allow))
	require.Equal(t, HailResult{Decided: true, By: comms.DecidedByCaptain}, hailResult(t, hd.d, "H-0002"))
	require.Equal(t, MissionDone, missionResult(t, hd.d, id).Status)

	steps := hailSteps(t, b.stepLog)
	require.Len(t, steps, 2)
	require.Equal(t, map[string]any{"decision": comms.OptionDeny, "decided_by": comms.DecidedByPolicy},
		map[string]any{"decision": steps[0]["decision"], "decided_by": steps[0]["decided_by"]})
	require.Equal(t, comms.DecidedByCaptain, steps[1]["decided_by"])
}

// #149's acceptance, timeout with belay: the order ends belayed, its hail
// closed unanswered, and the hail's workflow ends with it.
func TestHailTimeoutBelay(t *testing.T) {
	t.Parallel()
	b := newHolodeckBridge(t, hailsScript(t, toolHail("make deploy")))
	b.regs = defaultRegs(t)
	b.regs.Escalation = regs.Escalation{CaptainTimeout: time.Second, OnTimeout: regs.OnTimeoutBelay}
	hd := b.start(t)
	hd.addCrew(t)
	id := hd.createOrder(t)

	require.Equal(t, HailResult{}, hailResult(t, hd.d, "H-0001"))
	require.Equal(t, MissionBelayed, missionResult(t, hd.d, id).Status)
	require.Equal(t, store.OrderBelayed, hd.order(t, id).State)
	require.Equal(t, store.HailTimedOut, hd.hail(t, "H-0001").Status)
	e := hd.waitLog(t, "belay requested: hail H-0001 unanswered after")
	require.Equal(t, id, e.OrderID)
	hd.waitLog(t, "hail H-0001 closed unanswered: order belayed")
}

// #149's acceptance, reminders: with a short captain_reminder_every, the
// open hail is reminded of at least twice before the captain answers, and
// none after.
func TestHailReminders(t *testing.T) {
	t.Parallel()
	b := newHolodeckBridge(t, hailsScript(t, toolHail("make deploy")))
	b.regs = defaultRegs(t)
	b.regs.Escalation = regs.Escalation{CaptainReminderEvery: time.Second}
	hd := b.start(t)
	hd.addCrew(t)
	id := hd.createOrder(t)

	hd.waitHail(t, "H-0001")
	reminders := func(t require.TestingT) int {
		n := 0
		for _, e := range hd.logEntries(t) {
			if strings.HasPrefix(e.Text, "hail H-0001 still open after ") {
				n++
			}
		}
		return n
	}
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		require.GreaterOrEqual(c, reminders(c), 2)
	}, 30*time.Second, 20*time.Millisecond)

	allow := 0
	require.NoError(t, hd.d.AnswerHail(t.Context(), "H-0001", "", &allow))
	require.Equal(t, HailResult{Decided: true, By: comms.DecidedByCaptain}, hailResult(t, hd.d, "H-0001"))
	n := reminders(t)
	require.Equal(t, MissionDone, missionResult(t, hd.d, id).Status)
	require.Equal(t, n, reminders(t), "the ended workflow reminds no more")
}

// A hail closed by another path (here a belay) ends its workflow: the close
// wakes it, and it finds the hail no longer open.
func TestHailClosedByBelay(t *testing.T) {
	t.Parallel()
	b := newHolodeckBridge(t, hailsScript(t, toolHail("make deploy")))
	hd := b.start(t)
	hd.addCrew(t)
	id := hd.createOrder(t)

	hd.waitHail(t, "H-0001")
	requirePending(t, hd.d, hailWorkflowID("H-0001"))
	require.NoError(t, hd.d.BelayOrder(t.Context(), id, "changed my mind"))
	require.Equal(t, HailResult{}, hailResult(t, hd.d, "H-0001"))
	require.Equal(t, MissionBelayed, missionResult(t, hd.d, id).Status)
}

// hailStore is a store with sorell, an order away with it at priority and
// an open hail on it, and a Daemon on it with cfg: enough for the steps
// that only read and write the store.
func hailStore(t *testing.T, cfg *regs.Config, division string, priority int, hail store.Hail) (*Daemon, string) {
	t.Helper()
	ctx := t.Context()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), store.DBFile))
	require.NoError(t, err)
	t.Cleanup(func() { st.Close() })
	e := store.LogEntry{Kind: "test"}
	require.NoError(t, st.InsertCrew(ctx, store.Crew{ID: "sorell", Name: "Sorell", Division: division}, e))
	_, err = st.InsertOrder(ctx, store.Order{Title: "t", Assignee: "sorell", Priority: priority}, e)
	require.NoError(t, err)
	order, found, err := st.ClaimOldestDirectedOrder(ctx, "sorell", e)
	require.NoError(t, err)
	require.True(t, found)
	hail.OrderID, hail.CrewID = order, "sorell"
	id, err := st.InsertHail(ctx, hail, e)
	require.NoError(t, err)
	d := &Daemon{store: st, logger: slog.New(slog.DiscardHandler)}
	if cfg != nil {
		d.regs, d.regsConfigured = *cfg, true
	}
	return d, id
}

func storeHail(command string) store.Hail {
	ctx, _ := json.Marshal(map[string]any{"tool": "Bash", "input": map[string]any{"command": command}})
	return store.Hail{Category: comms.CategoryToolApproval, Question: "Run " + command + "?",
		Options: []string{comms.OptionAllow, comms.OptionDeny}, Context: ctx}
}

// classify routes each hail and records why, and the escalation settings
// the workflow then runs on.
func TestClassifyHail(t *testing.T) {
	t.Parallel()
	def := defaultRegs(t)
	two := 1
	def.Escalation = regs.Escalation{CaptainReminderEvery: time.Minute, CaptainTimeout: time.Hour,
		OnTimeout: regs.OnTimeoutDefaultOption, DefaultOption: &two}
	noAllow := storeHail("go test ./...")
	noAllow.Options = []string{comms.OptionDeny, comms.OptionAllow}
	for _, tc := range []struct {
		name     string
		cfg      *regs.Config
		division string
		hail     store.Hail
		route    string
		why      string
		captain  bool // CaptainOnly
	}{
		{"unconfigured", nil, "", storeHail("go test ./..."), regs.RouteCaptain, "regs.yaml not configured", false},
		{"auto", def, "", storeHail("go test ./..."), regs.RouteAuto, autoGoTest, false},
		{"auto from Command", def, DivisionCommand, storeHail("go test ./..."), regs.RouteAuto,
			autoGoTest, false},
		{"auto without allow first", def, "", noAllow, regs.RouteCaptain,
			autoGoTest + ", but option 0 is not allow", true},
		{"captain only", def, "", storeHail("git push"), regs.RouteCaptain, `captain_only[1]: tag "external_effect"`, true},
		{"captain only from Command", def, DivisionCommand, storeHail("git push"), regs.RouteCaptain,
			`captain_only[1]: tag "external_effect"`, true},
		{"from Command", def, DivisionCommand, storeHail("make deploy"), regs.RouteCaptain, "from Command", false},
		{"Command route", def, "", storeHail("make deploy"), regs.RouteCommand, "", false},
		{"malformed", def, "", store.Hail{Category: comms.CategoryToolApproval, Question: "?",
			Options: []string{comms.OptionAllow, comms.OptionDeny}}, regs.RouteCaptain, "malformed tool_approval context", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d, id := hailStore(t, tc.cfg, tc.division, store.PriorityRoutine, tc.hail)
			r, err := d.classifyHail(t.Context(), id)
			require.NoError(t, err)
			require.True(t, r.Open)
			require.Equal(t, tc.route, r.Route)
			require.Equal(t, tc.why, r.Why)
			require.Equal(t, tc.captain, r.CaptainOnly)
			if tc.cfg == nil {
				require.Zero(t, r.RemindEvery)
				require.Zero(t, r.Timeout)
				return
			}
			require.Equal(t, time.Minute, r.RemindEvery)
			require.Equal(t, time.Hour, r.Timeout)
			require.Equal(t, regs.OnTimeoutDefaultOption, r.OnTimeout)
			require.Equal(t, &two, r.DefaultOption)
			require.Equal(t, regs.DefaultCountermandWindow, r.CountermandWindow)
		})
	}

	// Classify skips option rules: a default option one fits is blocked
	// at classify, for on-timeout to hold.
	d, id := hailStore(t, def, "", store.PriorityRoutine, forceHail())
	r, err := d.classifyHail(t.Context(), id)
	require.NoError(t, err)
	require.Equal(t, regs.RouteCommand, r.Route)
	require.Empty(t, r.Why)
	require.False(t, r.CaptainOnly)
	require.Equal(t, `captain-only "force push over it": captain_only[4]: option "(?i)delete|destroy|force"`, r.DefaultBlocked)
	zero, rebase := 0, *def
	rebase.Escalation.DefaultOption = &zero
	d, id = hailStore(t, &rebase, "", store.PriorityRoutine, forceHail())
	r, err = d.classifyHail(t.Context(), id)
	require.NoError(t, err)
	require.Empty(t, r.DefaultBlocked, "rebasing is fine")

	// A closed hail is left alone.
	d, id = hailStore(t, def, "", store.PriorityRoutine, storeHail("go test ./..."))
	_, err = d.store.TimeOutOpenHail(t.Context(), id, store.LogEntry{Kind: "test"})
	require.NoError(t, err)
	r, err = d.classifyHail(t.Context(), id)
	require.NoError(t, err)
	require.False(t, r.Open)
}

// forceHail is a clarifying question whose option 1 a captain_only option
// rule fits: Classify sends it to Command, not the captain. forceHailStep
// is it as a holodeck hail step body.
func forceHail() store.Hail {
	return store.Hail{Category: comms.CategoryClarifyingQuestion, Question: "The branch diverged. How do I proceed?",
		Options: []string{"rebase and open a new PR", "force push over it"}}
}

func forceHailStep() map[string]any {
	h := forceHail()
	return map[string]any{"category": h.Category, "question": h.Question, "options": h.Options}
}

// on_timeout default_option holds, logging why once, for a captain-only
// hail, for a default option CheckAnswer rejects, for a missing
// default_option and for one the hail has no option for; hold logs too.
// None of them closes the hail, and on a closed hail the step logs nothing
// and reports it closed.
func TestApplyTimeoutHolds(t *testing.T) {
	t.Parallel()
	five, zero, one := 5, 0, 1
	for _, tc := range []struct {
		name string
		hail store.Hail
		esc  regs.Escalation
		want string
	}{
		{"hold", storeHail("make deploy"), regs.Escalation{OnTimeout: regs.OnTimeoutHold},
			"hail H-0001 unanswered after 1m: holding for the captain"},
		{"captain only", storeHail("git push"), regs.Escalation{OnTimeout: regs.OnTimeoutDefaultOption, DefaultOption: &zero},
			"hail H-0001 unanswered after 1m: holding for the captain (default_option does not apply to a captain-only hail)"},
		{"captain-only option", forceHail(), regs.Escalation{OnTimeout: regs.OnTimeoutDefaultOption, DefaultOption: &one},
			`hail H-0001 unanswered after 1m: holding for the captain (default_option 1: captain-only "force push over it": ` +
				`captain_only[4]: option "(?i)delete|destroy|force")`},
		{"no default_option", storeHail("make deploy"), regs.Escalation{OnTimeout: regs.OnTimeoutDefaultOption},
			"hail H-0001 unanswered after 1m: holding for the captain (no default_option)"},
		{"out of range", storeHail("make deploy"), regs.Escalation{OnTimeout: regs.OnTimeoutDefaultOption, DefaultOption: &five},
			"hail H-0001 unanswered after 1m: holding for the captain (default_option 5 is not one of the hail's 2 options)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := defaultRegs(t)
			cfg.Escalation = tc.esc
			d, id := hailStore(t, cfg, "", store.PriorityRoutine, tc.hail)
			r, err := d.classifyHail(t.Context(), id)
			require.NoError(t, err)
			out, err := d.applyTimeout(t.Context(), id, r, time.Minute)
			require.NoError(t, err)
			require.Equal(t, timeoutOutcome{}, out)
			h, err := d.store.GetHail(t.Context(), id)
			require.NoError(t, err)
			require.Equal(t, store.HailOpen, h.Status)
			entries, err := d.store.ListLog(t.Context(), 0)
			require.NoError(t, err)
			last := entries[len(entries)-1]
			require.Equal(t, tc.want, last.Text)
			require.JSONEq(t, `{"hail_id":"H-0001","on_timeout":"`+tc.esc.OnTimeout+`"}`, string(last.Payload))

			_, err = d.store.TimeOutOpenHail(t.Context(), id, store.LogEntry{Kind: "test"})
			require.NoError(t, err)
			out, err = d.applyTimeout(t.Context(), id, r, time.Minute)
			require.NoError(t, err)
			require.Equal(t, timeoutOutcome{Closed: true}, out)
			after, err := d.store.ListLog(t.Context(), 0)
			require.NoError(t, err)
			require.Len(t, after, len(entries)+1, "only the close")
		})
	}
}

// on_timeout belay of an order that can no longer be belayed (settled
// while its hail stayed open) is logged and dropped: nothing is decided or
// closed, and the step doesn't fail.
func TestApplyTimeoutBelayRefused(t *testing.T) {
	t.Parallel()
	cfg := defaultRegs(t)
	cfg.Escalation = regs.Escalation{OnTimeout: regs.OnTimeoutBelay}
	d, id := hailStore(t, cfg, "", store.PriorityRoutine, storeHail("make deploy"))
	h, err := d.store.GetHail(t.Context(), id)
	require.NoError(t, err)
	_, _, err = d.store.SettleOrder(t.Context(), h.OrderID, store.OrderDone, store.LogEntry{Kind: "test"}, nil)
	require.NoError(t, err)
	r, err := d.classifyHail(t.Context(), id)
	require.NoError(t, err)
	before, err := d.store.ListLog(t.Context(), 0)
	require.NoError(t, err)
	out, err := d.applyTimeout(t.Context(), id, r, time.Minute)
	require.NoError(t, err)
	require.Equal(t, timeoutOutcome{}, out)
	after, err := d.store.ListLog(t.Context(), 0)
	require.NoError(t, err)
	require.Len(t, after, len(before), "no belay requested")
	h, err = d.store.GetHail(t.Context(), id)
	require.NoError(t, err)
	require.Equal(t, store.HailOpen, h.Status)
}

func TestShortDuration(t *testing.T) {
	t.Parallel()
	for d, want := range map[time.Duration]string{
		0: "0s", 400 * time.Millisecond: "0s", 1500 * time.Millisecond: "2s", 30 * time.Minute: "30m",
		time.Hour: "1h", 90 * time.Minute: "1h30m", 61 * time.Second: "1m1s", time.Hour + time.Second: "1h0m1s",
	} {
		require.Equal(t, want, shortDuration(d), d.String())
	}
}
