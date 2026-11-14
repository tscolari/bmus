package bridge

import (
	"bytes"
	"cmp"
	"encoding/json"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/dbos-inc/dbos-transact-golang/dbos"
	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tscolari/bmus/internal/comms"
	"github.com/tscolari/bmus/internal/regs"
	"github.com/tscolari/bmus/internal/store"
)

// raiserInput is what testRaiser raises.
type raiserInput struct {
	Category string
	Question string
	Options  []string        `json:",omitempty"`
	Context  json.RawMessage `json:",omitempty"`
}

// testRaiser stands in for a bridge workflow that raises a hail (#152):
// it raises in (raiseBridgeHail) and returns the hail's row once it closed
// (awaitBridgeHail). Registered by registerRaiser.
func testRaiser(ctx dbos.Context, in raiserInput) (store.Hail, error) {
	id, err := raiseBridgeHail(ctx, store.Hail{Category: in.Category, Question: in.Question, Options: in.Options,
		Context: in.Context}, store.LogEntry{Kind: logHail, Text: "raised " + in.Category})
	if err != nil {
		return store.Hail{}, err
	}
	return awaitBridgeHail(ctx, id)
}

// recvClosed stands in for a raiser waiting on hailClosedTopic: it returns
// the first message it reads there.
func recvClosed(ctx dbos.Context, _ string) (string, error) {
	return dbos.Recv[string](ctx, hailClosedTopic, time.Minute)
}

// registerRaiser is a Config.registerExtra for the stand-in raisers.
func registerRaiser(ctx dbos.Context) {
	dbos.RegisterWorkflow(ctx, testRaiser, dbos.WithWorkflowName("test-raiser"))
	dbos.RegisterWorkflow(ctx, recvClosed, dbos.WithWorkflowName("test-recv-closed"))
}

// raise starts testRaiser as raiser/<name> with in.
func raise(t *testing.T, d *Daemon, name string, in raiserInput) dbos.WorkflowHandle[store.Hail] {
	t.Helper()
	h, err := dbos.RunWorkflow(d.dbos, testRaiser, in, dbos.WithWorkflowID("raiser/"+name))
	require.NoError(t, err)
	return h
}

func raiserResult(t *testing.T, h dbos.WorkflowHandle[store.Hail]) store.Hail {
	t.Helper()
	res, err := h.GetResult(dbos.WithHandleTimeout(60 * time.Second))
	require.NoError(t, err)
	return res
}

// noMissions requires that d ran no mission: a bridge hail sends no
// mission anything, and none exists to send to.
func noMissions(t *testing.T, d *Daemon) {
	t.Helper()
	wfs, err := dbos.ListWorkflows(d.dbos, dbos.WithFilterName(missionWorkflowName), dbos.WithFilterLoadInput(false),
		dbos.WithFilterLoadOutput(false))
	require.NoError(t, err)
	require.Empty(t, wfs)
}

// bridgeHailStore is a store with no crew member or order and one bridge
// hail h raised by findings/ORD-0001, and a Daemon on it with cfg: enough
// for the steps that only read and write the store.
func bridgeHailStore(t *testing.T, cfg *regs.Config, h store.Hail) (*Daemon, string) {
	t.Helper()
	ctx := t.Context()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), store.DBFile))
	require.NoError(t, err)
	t.Cleanup(func() { st.Close() })
	h.DeliverTo = "findings/ORD-0001"
	id, err := st.InsertBridgeHail(ctx, h, store.LogEntry{Kind: logHail, Text: "raised"})
	require.NoError(t, err)
	d := &Daemon{store: st, logger: slog.New(slog.DiscardHandler)}
	if cfg != nil {
		d.regs, d.regsConfigured = *cfg, true
	}
	return d, id
}

// loadRegsText loads text as regs.yaml.
func loadRegsText(t *testing.T, text string) *regs.Config {
	t.Helper()
	path := filepath.Join(t.TempDir(), regs.File)
	require.NoError(t, os.WriteFile(path, []byte(text), 0o600))
	cfg, found, err := regs.LoadRegs(path)
	require.NoError(t, err)
	require.True(t, found)
	return &cfg
}

// triageOnlyCaptain is the default regs.yaml with findings_triage listed
// first under captain_only, which turns triage off.
func triageOnlyCaptain(t *testing.T) *regs.Config {
	t.Helper()
	text := strings.Replace(string(regs.Default), "captain_only:\n", "captain_only:\n  - category: findings_triage\n", 1)
	return loadRegsText(t, text)
}

// Plan Verification 4, classify: a bridge hail's classify reads no order
// or crew member (the store holds neither), sees priority 0 (an alert rule
// doesn't fit) and never routes it "from Command". tracker_review is the
// captain's; findings_triage is advisory with no countermand window.
func TestClassifyBridgeHail(t *testing.T) {
	t.Parallel()
	def := defaultRegs(t)
	alert := loadRegsText(t, "captain_only:\n  - alert: yellow\n")
	pick := store.Hail{Category: "campaign_pick", Question: "Which task next?", Options: []string{"a", "b"}}
	triage := store.Hail{Category: categoryFindingsTriage, Question: "Triage 3 findings from ORD-0001."}
	review := store.Hail{Category: regs.CategoryTrackerReview, Question: "Review 3 issues?", Options: []string{"keep", "discard"}}
	for _, tc := range []struct {
		name     string
		cfg      *regs.Config
		hail     store.Hail
		route    string
		why      string
		captain  bool // CaptainOnly
		advisory bool
		window   time.Duration
	}{
		{"unconfigured", nil, pick, regs.RouteCaptain, "regs.yaml not configured", false, false, 0},
		{"Command", def, pick, regs.RouteCommand, "", false, false, regs.DefaultCountermandWindow},
		{"priority 0", alert, pick, regs.RouteCommand, "", false, false, regs.DefaultCountermandWindow},
		{"tracker_review", def, review, regs.RouteCaptain, "category tracker_review is fixed to the captain", true, false,
			regs.DefaultCountermandWindow},
		{"tracker_review unconfigured", nil, review, regs.RouteCaptain, "regs.yaml not configured", false, false, 0},
		{"triage", def, triage, regs.RouteCommand, "", false, true, 0},
		{"triage unconfigured", nil, triage, regs.RouteCaptain, "regs.yaml not configured", false, true, 0},
		{"triage captain_only", triageOnlyCaptain(t), triage, regs.RouteCaptain, `captain_only[0]: category "findings_triage"`,
			true, true, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d, id := bridgeHailStore(t, tc.cfg, tc.hail)
			r, err := d.classifyHail(t.Context(), id)
			require.NoError(t, err)
			require.True(t, r.Open)
			require.Equal(t, tc.route, r.Route)
			require.Equal(t, tc.why, r.Why)
			require.Equal(t, tc.captain, r.CaptainOnly)
			require.Equal(t, tc.advisory, r.Advisory)
			require.Equal(t, tc.window, r.CountermandWindow)
		})
	}
}

// on_timeout belay holds a bridge hail, which has no order to belay.
func TestApplyTimeoutBelayBridgeHail(t *testing.T) {
	t.Parallel()
	cfg := defaultRegs(t)
	cfg.Escalation = regs.Escalation{OnTimeout: regs.OnTimeoutBelay}
	d, id := bridgeHailStore(t, cfg, store.Hail{Category: regs.CategoryTrackerReview, Question: "Review?",
		Options: []string{"keep", "discard"}})
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
	require.Equal(t, "hail "+id+" unanswered after 1m: holding for the captain (no order to belay)", last.Text)
	require.Empty(t, last.OrderID)
	require.Empty(t, last.CrewID)
}

// Plan Verification 4, consult: a bridge hail's consult has no crew, order,
// order spec or recent log, and has the shape of consult.orderless.json:
// the same body fields (model aside, which transmit-consult adds), valid
// against the schema.
func TestConsultOrderless(t *testing.T) {
	t.Parallel()
	d, id := bridgeHailStore(t, defaultRegs(t), store.Hail{Category: categoryFindingsTriage,
		Question: "Triage 3 findings from ORD-0001.", Context: json.RawMessage(`{"findings":[{"key":"k1","title":"t"}]}`)})
	ctx := t.Context()
	_, err := d.SetCommandGuidance(ctx, "Keep orders small.")
	require.NoError(t, err)
	r, err := d.classifyHail(ctx, id)
	require.NoError(t, err)
	body, open, err := d.consultBody(ctx, id, r)
	require.NoError(t, err)
	require.True(t, open)
	require.Empty(t, body.Crew)
	require.Empty(t, body.Order)
	require.Empty(t, body.OrderSpec)
	require.Empty(t, body.RecentLog)
	body.Model = "claude-x"

	msg, err := comms.NewMessage(comms.TypeConsult, body)
	require.NoError(t, err)
	msg.Seq, msg.Crew = 1, "vance"
	raw, err := json.Marshal(msg)
	require.NoError(t, err)

	golden, err := os.ReadFile(filepath.Join("..", "..", "protocol", "golden", "consult.orderless.json"))
	require.NoError(t, err)
	keys := func(raw []byte) []string {
		var m struct{ Body map[string]any }
		require.NoError(t, json.Unmarshal(raw, &m))
		return slices.Sorted(maps.Keys(m.Body))
	}
	require.Equal(t, keys(golden), keys(raw))

	path := filepath.Join("..", "..", "protocol", "messages.schema.json")
	f, err := os.Open(path)
	require.NoError(t, err)
	defer f.Close()
	doc, err := jsonschema.UnmarshalJSON(f)
	require.NoError(t, err)
	c := jsonschema.NewCompiler()
	require.NoError(t, c.AddResource(path, doc))
	sch, err := c.Compile(path)
	require.NoError(t, err)
	for _, b := range [][]byte{golden, raw} {
		inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(b))
		require.NoError(t, err)
		require.NoError(t, sch.Validate(inst), string(b))
	}
}

// Plan Verification 4, skip-advisory: wherever routing would take a
// findings_triage hail to the captain, skip-advisory closes it timed_out
// with the reason, logs it and wakes its raiser. No mission is involved.
func TestSkipAdvisory(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		regs    func(t *testing.T) *regs.Config
		command bool // Command on deck
		consult any  // Command's scripted reply
		timeout time.Duration
		why     string
	}{
		{name: "no regs.yaml", why: "regs.yaml not configured"},
		{name: "captain_only", regs: triageOnlyCaptain, why: `captain_only[0]: category "findings_triage"`},
		{name: "Command not on deck", regs: defaultRegs, why: "Command not on deck"},
		{name: "consult timeout", regs: defaultRegs, command: true, consult: "silence", timeout: time.Second,
			why: "Command did not reply within 1s"},
		{name: "escalated", regs: defaultRegs, command: true,
			consult: map[string]any{"decision": "escalate", "rationale": "too many to judge", "confidence": 0.2},
			why:     "Command escalated: too many to judge"},
		{name: "rejected", regs: defaultRegs, command: true,
			consult: map[string]any{"decision": "answer", "answer": "{}", "confidence": 0.5},
			why:     "Regulations rejected Command's answer: confidence 0.5 is below 0.75 or out of range"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			consult := tc.consult
			if consult == nil {
				consult = "silence"
			}
			b := newHolodeckBridge(t, commandScript(t, consult))
			b.registerExtra = registerRaiser
			if tc.regs != nil {
				b.regs = tc.regs(t)
			}
			b.consultTimeout = cmp.Or(tc.timeout, time.Minute)
			hd := b.start(t)
			if tc.command {
				hd.addCommand(t)
			}
			h := raiserResult(t, raise(t, hd.d, "triage", raiserInput{Category: categoryFindingsTriage,
				Question: "Triage 3 findings from ORD-0001.", Context: json.RawMessage(`{"findings":[]}`)}))
			require.Equal(t, store.HailTimedOut, h.Status)
			require.Equal(t, tc.why, h.Rationale)
			require.Equal(t, "raiser/triage", h.DeliverTo)
			require.Empty(t, h.DecidedBy)
			require.Nil(t, h.CountermandUntil)

			require.Equal(t, HailResult{}, hailResult(t, hd.d, h.ID))
			names := hailStepNames(t, hd.d, h.ID)
			require.Equal(t, "skip-advisory", names[len(names)-2], names)
			require.Equal(t, "check-hail", names[len(names)-1], names)
			require.NotContains(t, names, "route-captain")
			e := hd.waitLog(t, "hail "+h.ID+" skipped")
			require.Equal(t, "hail "+h.ID+" skipped, Command's alone to answer: "+tc.why, e.Text)
			for _, e := range hd.logEntries(t) {
				require.False(t, strings.HasPrefix(e.Text, "hail "+h.ID+" for the captain"), e.Text)
			}
			if tc.command {
				require.Len(t, consults(t, b.stepLog), 1)
			}
			noMissions(t, hd.d)
		})
	}
}

// Plan Verification 4, decide: Command's answer to a findings_triage hail,
// and the captain's AnswerHail of a tracker_review hail, each wake the
// raiser, which reads the decided row. Nothing goes to a mission, and
// triage's decision has no countermand window.
func TestBridgeHailDecided(t *testing.T) {
	t.Parallel()
	b := newHolodeckBridge(t, commandScript(t,
		map[string]any{"decision": "answer", "answer": `{"drop":[]}`, "rationale": "all fine", "confidence": 0.9}))
	b.registerExtra = registerRaiser
	b.regs = defaultRegs(t)
	b.consultTimeout = time.Minute
	hd := b.start(t)
	hd.addCommand(t)

	h := raiserResult(t, raise(t, hd.d, "triage", raiserInput{Category: categoryFindingsTriage,
		Question: "Triage 3 findings from ORD-0001."}))
	require.Equal(t, store.HailDecided, h.Status)
	require.Equal(t, comms.DecidedByCommand, h.DecidedBy)
	require.Equal(t, `{"drop":[]}`, h.Decision)
	require.Nil(t, h.CountermandUntil, "no countermand window")
	require.Equal(t, HailResult{Decided: true, By: comms.DecidedByCommand}, hailResult(t, hd.d, h.ID))
	cs := consults(t, b.stepLog)
	require.Len(t, cs, 1)
	require.Empty(t, cs[0].Crew)
	require.Empty(t, cs[0].Order)
	require.Empty(t, cs[0].OrderSpec)
	require.Empty(t, cs[0].RecentLog)

	review := raise(t, hd.d, "review", raiserInput{Category: regs.CategoryTrackerReview, Question: "Review 3 issues?",
		Options: []string{"keep", "discard"}})
	var id string
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		hails, err := hd.d.store.ListHails(t.Context(), store.HailOpen)
		require.NoError(c, err)
		require.Len(c, hails, 1)
		id = hails[0].ID
	}, 30*time.Second, 20*time.Millisecond)
	e := hd.waitLog(t, "hail "+id+" for the captain")
	require.Equal(t, "hail "+id+" for the captain: category tracker_review is fixed to the captain", e.Text)
	require.Empty(t, e.OrderID)
	requirePending(t, hd.d, "raiser/review")

	one := 1
	require.NoError(t, hd.d.AnswerHail(t.Context(), id, "", &one))
	h = raiserResult(t, review)
	require.Equal(t, id, h.ID)
	require.Equal(t, store.HailDecided, h.Status)
	require.Equal(t, comms.DecidedByCaptain, h.DecidedBy)
	require.Equal(t, "discard", h.Decision)
	require.Equal(t, HailResult{Decided: true, By: comms.DecidedByCaptain}, hailResult(t, hd.d, id))
	noMissions(t, hd.d)

	raw, err := json.Marshal(HailsListArgs{})
	require.NoError(t, err)
	res, err := rpcHailsList(t.Context(), hd.d, raw)
	require.NoError(t, err)
	views := res.(HailsListResult)
	require.Len(t, views.Hails, 2)
	for _, v := range views.Hails {
		require.Empty(t, v.OrderID)
		require.Empty(t, v.CrewID)
		require.True(t, strings.HasPrefix(v.DeliverTo, "raiser/"), v.DeliverTo)
	}
}

// Plan Verification 4, countermand: a decided bridge hail inside its
// window is countermanded through the RPC: the row and its log entry are
// written, the reply has no note, and nothing is sent, to a mission or to
// the raiser, whose first message is one the test sends after.
func TestCountermandBridgeHail(t *testing.T) {
	t.Parallel()
	b := newHolodeckBridge(t, hailsScript(t))
	b.registerExtra = registerRaiser
	hd := b.start(t)
	ctx := t.Context()
	const raiser = "raiser/countermand"
	wait, err := dbos.RunWorkflow(hd.d.dbos, recvClosed, "", dbos.WithWorkflowID(raiser))
	require.NoError(t, err)
	requirePending(t, hd.d, raiser)

	id, err := hd.d.store.InsertBridgeHail(ctx, store.Hail{DeliverTo: raiser, Category: "campaign_pick", Question: "Which next?",
		Options: []string{"a", "b"}}, store.LogEntry{Kind: logHail, Text: "raised"})
	require.NoError(t, err)
	until, conf := time.Now().Add(time.Hour), 0.9
	require.NoError(t, hd.d.store.DecideOpenHailAs(ctx, id, store.HailDecision{DecidedBy: comms.DecidedByCommand, Decision: "a",
		Route: regs.RouteCommand, Confidence: &conf, CountermandUntil: &until}, store.LogEntry{Kind: logHail, Text: "decided"}, nil))

	one := 1
	res, err := hd.d.countermandHail(ctx, HailsCountermandArgs{HailID: id, Option: &one, Reason: "b is due first"})
	require.NoError(t, err)
	require.Empty(t, res.Note)
	require.Equal(t, raiser, res.DeliverTo)
	require.Equal(t, comms.DecidedByCaptain, res.DecidedBy)
	require.Equal(t, "b", res.Decision)
	h := hd.hail(t, id)
	require.NotNil(t, h.CountermandedAt)
	require.Equal(t, comms.DecidedByCommand, h.OriginalDecidedBy)
	e := hd.waitLog(t, "hail "+id+" countermanded")
	require.Equal(t, "hail "+id+" countermanded: b is due first", e.Text)
	require.Empty(t, e.OrderID)

	require.NoError(t, dbos.Send(hd.d.dbos, raiser, "sentinel", hailClosedTopic))
	got, err := wait.GetResult(dbos.WithHandleTimeout(30 * time.Second))
	require.NoError(t, err)
	require.Equal(t, "sentinel", got, "the countermand sent the raiser nothing")
	noMissions(t, hd.d)
	for _, e := range hd.logEntries(t) {
		require.NotContains(t, e.Text, "correction", e.Text)
	}
}
