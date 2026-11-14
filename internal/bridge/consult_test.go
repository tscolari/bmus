package bridge

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/dbos-inc/dbos-transact-golang/dbos"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tscolari/bmus/internal/comms"
	"github.com/tscolari/bmus/internal/regs"
	"github.com/tscolari/bmus/internal/store"
)

// commandScript writes a holodeck script whose orders raise each hail in
// turn, then report, and whose consults all get consult: a consult_reply
// body without hail_id, or "silence".
func commandScript(t *testing.T, consult any, hails ...map[string]any) string {
	t.Helper()
	data, err := os.ReadFile(hailsScript(t, hails...))
	require.NoError(t, err)
	data, err = json.Marshal(map[string]any{"steps": json.RawMessage(data), "consult": consult})
	require.NoError(t, err)
	script := filepath.Join(t.TempDir(), "command.json")
	require.NoError(t, os.WriteFile(script, data, 0o600))
	return script
}

// addCommand adds vance, of the command division, and waits for it to be
// on deck with its pump connected.
func (hd *hdaemon) addCommand(t *testing.T) store.Crew {
	t.Helper()
	return hd.addCrewWith(t, store.Crew{ID: "vance", Name: "Vance", Rank: "Admiral", Division: DivisionCommand})
}

// consults are the holodeck step log's consults, in order.
func consults(t *testing.T, stepLog string) []comms.ConsultBody {
	t.Helper()
	var out []comms.ConsultBody
	if _, err := os.Stat(stepLog); os.IsNotExist(err) {
		return nil
	}
	for _, e := range stepLogEntries(t, stepLog) {
		if e["action"] != "consult" {
			continue
		}
		raw, err := json.Marshal(e["consult"])
		require.NoError(t, err)
		var c comms.ConsultBody
		require.NoError(t, json.Unmarshal(raw, &c))
		out = append(out, c)
	}
	return out
}

// hailStepNames are hail id's workflow's step names without DBOS.sleep.
// dbos-transact-golang v1.7.0's Recv records a DBOS.sleep step only when
// no message is waiting yet, so whether one shows up depends on how fast
// Command replied.
func hailStepNames(t *testing.T, d *Daemon, id string) []string {
	t.Helper()
	var names []string
	for _, n := range stepNames(t, d, hailWorkflowID(id)) {
		if n != "DBOS.sleep" {
			names = append(names, n)
		}
	}
	return names
}

// requireStepsFrom requires hail id's workflow's first steps to be want
// (hailStepNames).
func requireStepsFrom(t *testing.T, d *Daemon, id string, want ...string) {
	t.Helper()
	var names []string
	require.Eventually(t, func() bool {
		names = hailStepNames(t, d, id)
		return len(names) >= len(want)
	}, 30*time.Second, 20*time.Millisecond)
	require.Equal(t, want, names[:len(want)])
}

// #150's acceptance, answer: Command, scripted to answer, decides a
// routine hail as decided_by command, with its confidence and a
// countermand window; the mission relays decided_by command. The consult
// carries the hail, the order, its log, the guidance and the regulations.
func TestHailCommandAnswers(t *testing.T) {
	t.Parallel()
	b := newHolodeckBridge(t, commandScript(t,
		map[string]any{"decision": "answer", "option": 0, "rationale": "deploys are routine", "confidence": 0.9},
		toolHail("make deploy")))
	b.regs = defaultRegs(t)
	hd := b.start(t)
	hd.addCommand(t)
	hd.addCrew(t)
	_, err := hd.d.SetCommandGuidance(t.Context(), "Staging deploys are fine.")
	require.NoError(t, err)
	id := hd.createOrder(t)

	require.Equal(t, HailResult{Decided: true, By: comms.DecidedByCommand}, hailResult(t, hd.d, "H-0001"))
	require.Equal(t, []string{"classify", "find-command", "transmit-consult", "DBOS.recv", "decide-command"},
		hailStepNames(t, hd.d, "H-0001"))
	require.Equal(t, MissionDone, missionResult(t, hd.d, id).Status)

	h := hd.hail(t, "H-0001")
	require.Equal(t, store.HailDecided, h.Status)
	require.Equal(t, comms.DecidedByCommand, h.DecidedBy)
	require.Equal(t, comms.DecidedByCommand, h.OriginalDecidedBy)
	require.Equal(t, comms.OptionAllow, h.Decision)
	require.Equal(t, regs.RouteCommand, h.Route)
	require.Equal(t, "deploys are routine", h.Rationale)
	require.NotNil(t, h.Confidence)
	require.InDelta(t, 0.9, *h.Confidence, 1e-9)
	require.NotNil(t, h.DecidedAt)
	require.NotNil(t, h.CountermandUntil)
	require.WithinDuration(t, h.DecidedAt.Add(regs.DefaultCountermandWindow), *h.CountermandUntil, time.Millisecond)
	hd.waitLog(t, "hail H-0001 for Command: vance")
	hd.waitLog(t, "hail H-0001 decided by Command: allow")

	steps := hailSteps(t, b.stepLog)
	require.Len(t, steps, 1)
	require.Equal(t, comms.OptionAllow, steps[0]["decision"])
	require.Equal(t, comms.DecidedByCommand, steps[0]["decided_by"])

	cs := consults(t, b.stepLog)
	require.Len(t, cs, 1, "one consult, to vance only")
	c := cs[0]
	require.Equal(t, "H-0001", c.HailID)
	require.Equal(t, "sorell", c.Crew)
	require.Equal(t, id, c.Order)
	require.Equal(t, comms.CategoryToolApproval, c.Category)
	require.Equal(t, "Run make deploy?", c.Question)
	require.Equal(t, []string{comms.OptionAllow, comms.OptionDeny}, c.Options)
	require.JSONEq(t, `{"tool":"Bash","input":{"command":"make deploy"}}`, string(c.Context))
	require.Equal(t, "holodeck run\n\nwalk the script", c.OrderSpec)
	require.Equal(t, "Staging deploys are fine.", c.Guidance)
	require.Contains(t, c.Regulations, "min_confidence: 0.75")
	require.Contains(t, c.Regulations, `option "(?i)delete|destroy|force"`)
	require.NotEmpty(t, c.RecentLog)
	require.LessOrEqual(t, len(c.RecentLog), consultRecentLog)
	require.True(t, strings.HasSuffix(c.RecentLog[len(c.RecentLog)-1], "hail: hail H-0001 for Command: vance"),
		c.RecentLog[len(c.RecentLog)-1])
	require.Empty(t, c.Model, "no crew or division model: the harness's default")
}

// #162's acceptance: a consult carries Command's model, its crew member's,
// else its division's default (resolveModel).
func TestHailConsultModel(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, crew, division, want string
	}{
		{"crew model", "claude-x", "m1", "claude-x"},
		{"division default", "", "m1", "m1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			b := newHolodeckBridge(t, commandScript(t,
				map[string]any{"decision": "answer", "option": 0, "rationale": "fine", "confidence": 0.9},
				toolHail("make deploy")))
			b.regs = defaultRegs(t)
			b.divisions = map[string]DivisionConfig{DivisionCommand: {Model: tc.division}, "ops": {}}
			hd := b.start(t)
			hd.addCrewWith(t, store.Crew{ID: "vance", Name: "Vance", Rank: "Admiral", Division: DivisionCommand, Model: tc.crew})
			hd.addCrewWith(t, store.Crew{ID: "sorell", Name: "Sorell", Rank: "Lieutenant", Division: "ops"})
			hd.createOrder(t)

			require.Equal(t, HailResult{Decided: true, By: comms.DecidedByCommand}, hailResult(t, hd.d, "H-0001"))
			cs := consults(t, b.stepLog)
			require.Len(t, cs, 1)
			require.Equal(t, tc.want, cs[0].Model)
		})
	}
}

// #150's acceptance, fall back to the captain: Command escalates, answers
// below min_confidence, picks an option a captain_only option rule fits,
// or stays silent past the consult timeout. Each time the hail goes to the
// captain, logged why, and AnswerHail decides it.
func TestHailCommandFallsBack(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		hail    map[string]any
		consult any
		why     string
	}{
		{"escalate", toolHail("make deploy"),
			map[string]any{"decision": "escalate", "rationale": "prod looks busy", "confidence": 0.2},
			"Command escalated: prod looks busy"},
		{"below min_confidence", toolHail("make deploy"),
			map[string]any{"decision": "answer", "option": 0, "confidence": 0.5},
			"Regulations rejected Command's answer: confidence 0.5 is below 0.75 or out of range"},
		{"captain-only option", forceHailStep(),
			map[string]any{"decision": "answer", "option": 1, "confidence": 0.95},
			`Regulations rejected Command's answer: captain-only "force push over it": captain_only[4]: option "(?i)delete|destroy|force"`},
		{"silence", toolHail("make deploy"), "silence", "Command did not reply within 1s"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			b := newHolodeckBridge(t, commandScript(t, tc.consult, tc.hail))
			b.regs = defaultRegs(t)
			b.consultTimeout = time.Minute // a reply comes at once
			if tc.consult == "silence" {
				b.consultTimeout = time.Second
			}
			hd := b.start(t)
			hd.addCommand(t)
			hd.addCrew(t)
			id := hd.createOrder(t)

			hd.waitHail(t, "H-0001")
			e := hd.waitLog(t, "hail H-0001 for the captain")
			require.Equal(t, "hail H-0001 for the captain: "+tc.why, e.Text)
			requirePending(t, hd.d, hailWorkflowID("H-0001"))
			h := hd.hail(t, "H-0001")
			require.Equal(t, store.HailOpen, h.Status)
			require.Equal(t, regs.RouteCaptain, h.Route)

			zero := 0
			require.NoError(t, hd.d.AnswerHail(t.Context(), "H-0001", "", &zero))
			require.Equal(t, HailResult{Decided: true, By: comms.DecidedByCaptain}, hailResult(t, hd.d, "H-0001"))
			require.Equal(t, MissionDone, missionResult(t, hd.d, id).Status)
			h = hd.hail(t, "H-0001")
			require.Equal(t, comms.DecidedByCaptain, h.DecidedBy)
			require.Nil(t, h.Confidence)
			require.Nil(t, h.CountermandUntil)
			steps := hailSteps(t, b.stepLog)
			require.Len(t, steps, 1)
			require.Equal(t, comms.DecidedByCaptain, steps[0]["decided_by"])
			require.Len(t, consults(t, b.stepLog), 1, "consulted once")
		})
	}
}

// The captain may answer while Command thinks: the close wakes the
// workflow's consult wait, which ends without deciding or routing the hail.
func TestHailAnsweredDuringConsult(t *testing.T) {
	t.Parallel()
	b := newHolodeckBridge(t, commandScript(t, "silence", toolHail("make deploy")))
	b.regs = defaultRegs(t)
	b.consultTimeout = time.Hour
	hd := b.start(t)
	hd.addCommand(t)
	hd.addCrew(t)
	id := hd.createOrder(t)

	hd.waitHail(t, "H-0001")
	hd.waitLog(t, "hail H-0001 for Command: vance")
	require.Eventually(t, func() bool { return len(consults(t, b.stepLog)) == 1 }, 30*time.Second, 20*time.Millisecond)
	allow := 0
	require.NoError(t, hd.d.AnswerHail(t.Context(), "H-0001", "", &allow))
	require.Equal(t, HailResult{Decided: true, By: comms.DecidedByCaptain}, hailResult(t, hd.d, "H-0001"))
	require.Equal(t, MissionDone, missionResult(t, hd.d, id).Status)
	h := hd.hail(t, "H-0001")
	require.Equal(t, comms.DecidedByCaptain, h.DecidedBy)
	require.Equal(t, regs.RouteCommand, h.Route, "never routed to the captain")
	require.NotContains(t, stepNames(t, hd.d, hailWorkflowID("H-0001")), "route-captain")
}

// When Command objects (escalates), on_timeout default_option holds for
// the captain rather than decide over it.
func TestHailCommandObjectionHolds(t *testing.T) {
	t.Parallel()
	b := newHolodeckBridge(t, commandScript(t,
		map[string]any{"decision": "escalate", "rationale": "ask the captain", "confidence": 0}, toolHail("make deploy")))
	b.regs = defaultRegs(t)
	allow := 0
	b.regs.Escalation = regs.Escalation{CaptainTimeout: time.Second, OnTimeout: regs.OnTimeoutDefaultOption, DefaultOption: &allow}
	hd := b.start(t)
	hd.addCommand(t)
	hd.addCrew(t)
	id := hd.createOrder(t)

	hd.waitHail(t, "H-0001")
	hd.waitLog(t, "hail H-0001 for the captain: Command escalated: ask the captain")
	e := hd.waitLog(t, "hail H-0001 unanswered after")
	require.Contains(t, e.Text, "holding for the captain (default_option 0: Command escalated: ask the captain)")
	require.Equal(t, store.HailOpen, hd.hail(t, "H-0001").Status)

	deny := 1
	require.NoError(t, hd.d.AnswerHail(t.Context(), "H-0001", "", &deny))
	require.Equal(t, HailResult{Decided: true, By: comms.DecidedByCaptain}, hailResult(t, hd.d, "H-0001"))
	require.Equal(t, MissionDone, missionResult(t, hd.d, id).Status)
}

// A bridge restarted while the hail waits on Command picks the wait up
// again: the close of the hail still ends it.
func TestHailConsultSurvivesRestart(t *testing.T) {
	t.Parallel()
	b := newHolodeckBridge(t, commandScript(t, "silence", toolHail("make deploy")))
	b.regs = defaultRegs(t)
	b.consultTimeout = time.Hour
	hd := b.start(t)
	hd.addCommand(t)
	hd.addCrew(t)
	id := hd.createOrder(t)
	hd.waitHail(t, "H-0001")
	require.Eventually(t, func() bool { return len(consults(t, b.stepLog)) == 1 }, 30*time.Second, 20*time.Millisecond)

	hd.stop(t)
	hd = b.start(t)
	requirePending(t, hd.d, hailWorkflowID("H-0001"))
	require.Eventually(t, func() bool {
		p, ok := hd.d.Pump("sorell")
		return ok && p.Connected()
	}, 30*time.Second, 20*time.Millisecond)
	allow := 0
	require.NoError(t, hd.d.AnswerHail(t.Context(), "H-0001", "", &allow))
	require.Equal(t, HailResult{Decided: true, By: comms.DecidedByCaptain}, hailResult(t, hd.d, "H-0001"))
	require.Equal(t, MissionDone, missionResult(t, hd.d, id).Status)
	require.Len(t, consults(t, b.stepLog), 1, "not consulted again")
	n := 0
	for _, name := range stepNames(t, hd.d, hailWorkflowID("H-0001")) {
		if name == "find-command" {
			n++
		}
	}
	require.Equal(t, 1, n, "find-command ran once")
	require.Equal(t, regs.RouteCommand, hd.hail(t, "H-0001").Route)
}

// #150's acceptance, Command not on deck: with Command in sickbay the hail
// goes to the captain, logged "Command not on deck", and status lists it.
// With nobody in the command division there is no such reason.
func TestHailCommandNotOnDeck(t *testing.T) {
	t.Parallel()
	b := newHolodeckBridge(t, commandScript(t, map[string]any{"decision": "answer", "option": 0, "confidence": 1},
		toolHail("make deploy")))
	b.regs = defaultRegs(t)
	hd := b.start(t)
	hd.addCrew(t)
	s, err := hd.d.Status(t.Context())
	require.NoError(t, err)
	require.Equal(t, ConditionGreen, s.Condition, "nobody in the command division")
	require.Empty(t, s.Reasons)

	v := hd.addCommand(t)
	require.NoError(t, b.h.Kill(v.CurrentBeam))
	require.Equal(t, CrewTourResult{Ended: store.CrewSickbay}, tourResult(t, hd.d, tourWorkflowID("vance", 0)))
	s, err = hd.d.Status(t.Context())
	require.NoError(t, err)
	require.Equal(t, ConditionRed, s.Condition)
	require.Equal(t, []string{"vance in sickbay", "Command not on deck"}, s.Reasons)

	id := hd.createOrder(t)
	hd.waitHail(t, "H-0001")
	hd.waitLog(t, "hail H-0001 for the captain: Command not on deck")
	requireStepsFrom(t, hd.d, "H-0001", "classify", "find-command", "route-captain")
	s, err = hd.d.Status(t.Context())
	require.NoError(t, err)
	require.Equal(t, []string{"vance in sickbay", "Command not on deck"}, s.Reasons, "hailing is yellow: not listed")
	require.Empty(t, consults(t, b.stepLog))

	allow := 0
	require.NoError(t, hd.d.AnswerHail(t.Context(), "H-0001", "", &allow))
	require.Equal(t, HailResult{Decided: true, By: comms.DecidedByCaptain}, hailResult(t, hd.d, "H-0001"))
	require.Equal(t, MissionDone, missionResult(t, hd.d, id).Status)
}

// #150's acceptance, hail from Command: a hail raised by a command-division
// crew member goes to the captain, not to a consult, even with another
// Command crew member on deck.
func TestHailFromCommand(t *testing.T) {
	t.Parallel()
	b := newHolodeckBridge(t, commandScript(t, map[string]any{"decision": "answer", "option": 0, "confidence": 1},
		toolHail("make deploy")))
	b.regs = defaultRegs(t)
	hd := b.start(t)
	hd.addCommand(t)
	hd.addCrewWith(t, store.Crew{ID: "sorell", Name: "Sorell", Division: DivisionCommand})
	id := hd.createOrder(t)

	hd.waitHail(t, "H-0001")
	hd.waitLog(t, "hail H-0001 for the captain: from Command")
	requireStepsFrom(t, hd.d, "H-0001", "classify", "route-captain")
	allow := 0
	require.NoError(t, hd.d.AnswerHail(t.Context(), "H-0001", "", &allow))
	require.Equal(t, HailResult{Decided: true, By: comms.DecidedByCaptain}, hailResult(t, hd.d, "H-0001"))
	require.Equal(t, MissionDone, missionResult(t, hd.d, id).Status)
	require.Empty(t, consults(t, b.stepLog))
}

// commandCrew picks the first command-division crew member on deck, by
// id.
func TestCommandCrew(t *testing.T) {
	t.Parallel()
	d, _ := hailStore(t, defaultRegs(t), "", store.PriorityRoutine, storeHail("make deploy"))
	ctx := t.Context()
	got, err := d.commandCrew(ctx)
	require.NoError(t, err)
	require.Empty(t, got, "sorell is not Command")
	e := store.LogEntry{Kind: "test"}
	for _, c := range []store.Crew{
		{ID: "ross", Name: "Ross", Division: DivisionCommand, Status: store.CrewSickbay},
		{ID: "vance", Name: "Vance", Division: DivisionCommand, Status: store.CrewAway},
		{ID: "zed", Name: "Zed", Division: DivisionCommand},
	} {
		require.NoError(t, d.store.InsertCrew(ctx, c, e))
	}
	got, err = d.commandCrew(ctx)
	require.NoError(t, err)
	require.Equal(t, "vance", got)
}

// #106: Command beaming in with no beam yet (provisioning, perhaps
// waiting on a tsh login) isn't on deck: find-command finds nobody, so the
// hail goes to the captain, and the status says "Command not on deck".
// With its beam up and its hello pending it is Command again.
func TestCommandBeamingIn(t *testing.T) {
	t.Parallel()
	d, id := hailStore(t, defaultRegs(t), "", store.PriorityRoutine, storeHail("make deploy"))
	ctx := t.Context()
	e := store.LogEntry{Kind: "test"}
	require.NoError(t, d.store.InsertCrew(ctx, store.Crew{ID: "vance", Name: "Vance", Division: DivisionCommand,
		Status: store.CrewBeamingIn}, e))
	found, err := d.findCommand(ctx, id)
	require.NoError(t, err)
	require.Equal(t, commandFound{Open: true}, found, "nobody: the captain's route")
	h, err := d.store.GetHail(ctx, id)
	require.NoError(t, err)
	require.NotEqual(t, regs.RouteCommand, h.Route)
	s, err := d.Status(ctx)
	require.NoError(t, err)
	require.Equal(t, []string{"sorell hailing", "Command not on deck"}, s.Reasons)

	require.NoError(t, d.store.SetCrewStatus(ctx, "vance", store.CrewBeamingIn, "b1", e))
	found, err = d.findCommand(ctx, id)
	require.NoError(t, err)
	require.Equal(t, commandFound{Crew: "vance", Open: true}, found)
	s, err = d.Status(ctx)
	require.NoError(t, err)
	require.Equal(t, []string{"sorell hailing"}, s.Reasons)
}

// A roster with divisions takes crew members into command only when it
// lists it.
func TestCommandDivisionInRoster(t *testing.T) {
	t.Parallel()
	d := &Daemon{divisions: map[string]DivisionConfig{"ops": {}}}
	var re *rpcError
	require.ErrorAs(t, d.checkDivision(DivisionCommand), &re)
	require.Contains(t, re.Error(), `unknown division "command"`)
	d.divisions[DivisionCommand] = DivisionConfig{}
	require.NoError(t, d.checkDivision(DivisionCommand))
}

// decide-command refuses a reply from anyone but the consulted crew
// member, for another hail, or one CheckAnswer rejects, with why, deciding
// nothing. An escalation or risk flags are Command's objection; the
// Regulations' captain-only reason outranks an escalation's. A closed hail
// is reported closed.
func TestDecideCommandRejects(t *testing.T) {
	t.Parallel()
	one := 1
	long := strings.Repeat("é", maxCommandRationale)
	for _, tc := range []struct {
		name string
		hail store.Hail
		msg  comms.Message
		want commandOutcome
	}{
		{"not a reply", storeHail("make deploy"), comms.Message{Type: comms.TypeReport, Crew: "vance"},
			commandOutcome{Why: "Command's reply is not a consult_reply (report)"}},
		{"another crew member", storeHail("make deploy"), func() comms.Message {
			m := consultReply(t, comms.ConsultReplyBody{HailID: "H-0001", Decision: regs.DecisionAnswer, Option: &one, Confidence: 1})
			m.Crew = "sorell"
			return m
		}(), commandOutcome{Why: "Command's reply came from sorell, not vance"}},
		{"another hail", storeHail("make deploy"),
			consultReply(t, comms.ConsultReplyBody{HailID: "H-0002", Decision: regs.DecisionAnswer, Option: &one, Confidence: 1}),
			commandOutcome{Why: "Command's reply is for hail H-0002"}},
		{"low confidence", storeHail("make deploy"),
			consultReply(t, comms.ConsultReplyBody{HailID: "H-0001", Decision: regs.DecisionAnswer, Option: &one, Confidence: 0.1}),
			commandOutcome{Why: "Regulations rejected Command's answer: confidence 0.1 is below 0.75 or out of range"}},
		{"risk flags", storeHail("make deploy"),
			consultReply(t, comms.ConsultReplyBody{HailID: "H-0001", Decision: regs.DecisionAnswer, Option: &one,
				Confidence: 1, RiskFlags: []string{"prod"}}),
			commandOutcome{Why: "Regulations rejected Command's answer: Command flagged risks: prod", Objected: true}},
		{"escalate without rationale", storeHail("make deploy"),
			consultReply(t, comms.ConsultReplyBody{HailID: "H-0001", Decision: regs.DecisionEscalate}),
			commandOutcome{Why: "Command escalated", Objected: true}},
		{"escalate with a long rationale", storeHail("make deploy"),
			consultReply(t, comms.ConsultReplyBody{HailID: "H-0001", Decision: regs.DecisionEscalate, Rationale: long}),
			commandOutcome{Why: "Command escalated: " + clip(long, maxCommandRationale), Objected: true}},
		{"escalate on a captain-only hail", storeHail("git push"),
			consultReply(t, comms.ConsultReplyBody{HailID: "H-0001", Decision: regs.DecisionEscalate, Rationale: "no"}),
			commandOutcome{Why: `Regulations rejected Command's answer: captain-only hail: captain_only[1]: tag "external_effect"`,
				Objected: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d, id := hailStore(t, defaultRegs(t), "", store.PriorityRoutine, tc.hail)
			r, err := d.classifyHail(t.Context(), id)
			require.NoError(t, err)
			out, err := d.decideCommand(t.Context(), id, "vance", r, tc.msg)
			require.NoError(t, err)
			require.Equal(t, tc.want, out)
			h, err := d.store.GetHail(t.Context(), id)
			require.NoError(t, err)
			require.Equal(t, store.HailOpen, h.Status)
		})
	}

	d, id := hailStore(t, defaultRegs(t), "", store.PriorityRoutine, storeHail("make deploy"))
	r, err := d.classifyHail(t.Context(), id)
	require.NoError(t, err)
	_, err = d.store.TimeOutOpenHail(t.Context(), id, store.LogEntry{Kind: "test"})
	require.NoError(t, err)
	out, err := d.decideCommand(t.Context(), id, "vance", r,
		consultReply(t, comms.ConsultReplyBody{HailID: id, Decision: regs.DecisionAnswer, Option: &one, Confidence: 1}))
	require.NoError(t, err)
	require.Equal(t, commandOutcome{Closed: true}, out)
}

// consultReply is a consult_reply from vance.
func consultReply(t *testing.T, body comms.ConsultReplyBody) comms.Message {
	t.Helper()
	m, err := comms.NewMessage(comms.TypeConsultReply, body)
	require.NoError(t, err)
	m.Crew = "vance"
	return m
}

func TestClip(t *testing.T) {
	t.Parallel()
	alpha, accents := "abcdefghijklmnopqrstuvwxyz", strings.Repeat("é", 20) // é is 2 bytes
	for _, tc := range []struct {
		s    string
		n    int
		want string
	}{
		{"short", 5, "short"},
		{alpha, 4 + len(truncated), "abcd" + truncated},
		{accents, 5 + len(truncated), "éé" + truncated},
		{alpha, len(truncated), alpha[:len(truncated)]},
		{alpha, 3, "abc"},
		{accents, 3, "é"},
		{alpha, 0, ""},
		{alpha, -1, ""},
	} {
		got := clip(tc.s, tc.n)
		require.Equal(t, tc.want, got, "clip(%q, %d)", tc.s, tc.n)
		require.True(t, utf8.ValidString(got))
	}
}

// The consult of a hail without context or guidance leaves them out, and
// carries at most consultRecentLog of the order's latest log lines.
func TestConsultBody(t *testing.T) {
	t.Parallel()
	d, id := hailStore(t, defaultRegs(t), "", store.PriorityRoutine, forceHail())
	ctx := t.Context()
	h, err := d.store.GetHail(ctx, id)
	require.NoError(t, err)
	for i := range consultRecentLog + 5 {
		_, err := d.store.AppendLog(ctx, store.LogEntry{Kind: "progress", OrderID: h.OrderID, Text: "line " + string(rune('a'+i))})
		require.NoError(t, err)
	}
	_, err = d.store.AppendLog(ctx, store.LogEntry{Kind: "progress", OrderID: "ORD-0099", Text: "another order"})
	require.NoError(t, err)
	r, err := d.classifyHail(ctx, id)
	require.NoError(t, err)
	body, open, err := d.consultBody(ctx, id, r)
	require.NoError(t, err)
	require.True(t, open)
	require.Equal(t, "t", body.OrderSpec, "no body")
	require.Empty(t, body.Guidance)
	require.Nil(t, body.Context)
	require.Equal(t, forceHail().Options, body.Options)
	require.Len(t, body.RecentLog, consultRecentLog)
	require.True(t, strings.HasSuffix(body.RecentLog[consultRecentLog-1], "progress: line "+string(rune('a'+consultRecentLog+4))))
	require.Equal(t, "No captain_only or auto_allow rule fits this hail: Command may answer it.\n"+
		"min_confidence: 0.75 (a less confident answer goes to the captain)\n"+
		`captain_only[4]: option "(?i)delete|destroy|force" (an answer this fits goes to the captain)`, body.Regulations)

	_, err = d.store.TimeOutOpenHail(ctx, id, store.LogEntry{Kind: "test"})
	require.NoError(t, err)
	_, open, err = d.consultBody(ctx, id, r)
	require.NoError(t, err)
	require.False(t, open)
}

// A consult cuts long log lines and replaces an oversized context with its
// cut text, as a string in an object.
func TestConsultBodyCaps(t *testing.T) {
	t.Parallel()
	big := strings.Repeat("x", maxConsultContext)
	d, id := hailStore(t, defaultRegs(t), "", store.PriorityRoutine, storeHail(big))
	ctx := t.Context()
	h, err := d.store.GetHail(ctx, id)
	require.NoError(t, err)
	_, err = d.store.AppendLog(ctx, store.LogEntry{Kind: "progress", OrderID: h.OrderID, Text: big})
	require.NoError(t, err)
	body, open, err := d.consultBody(ctx, id, hailRouting{})
	require.NoError(t, err)
	require.True(t, open)
	last := body.RecentLog[len(body.RecentLog)-1]
	require.Len(t, last, maxConsultLogLine)
	require.True(t, strings.HasSuffix(last, truncated))
	var c map[string]string
	require.NoError(t, json.Unmarshal(body.Context, &c))
	require.Len(t, c["truncated"], maxConsultContext)
	require.True(t, strings.HasSuffix(c["truncated"], truncated))
}

// corrections are the holodeck step log's corrections, in order. It
// reports to t (an EventuallyWithT callback's c, too) and skips a line
// still being written.
func corrections(t require.TestingT, stepLog string) []comms.CorrectionBody {
	data, err := os.ReadFile(stepLog)
	if os.IsNotExist(err) {
		return nil
	}
	require.NoError(t, err)
	var out []comms.CorrectionBody
	for _, line := range strings.Split(string(data), "\n") {
		var e struct {
			Action     string               `json:"action"`
			Correction comms.CorrectionBody `json:"correction"`
		}
		if json.Unmarshal([]byte(line), &e) == nil && e.Action == "correction" {
			out = append(out, e.Correction)
		}
	}
	return out
}

// requireCorrections waits for the holodeck step log to hold n
// corrections and returns them.
func requireCorrections(t *testing.T, stepLog string, n int) []comms.CorrectionBody {
	t.Helper()
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		require.Len(c, corrections(c, stepLog), n)
	}, 30*time.Second, 20*time.Millisecond)
	return corrections(t, stepLog)
}

// #153's acceptance: the captain countermands Command's decision inside
// its window. decided_by turns captain, original_decided_by stays
// command, the log says so, and the hailing crew member's running order
// gets a correction through its mission (send-correction), which its
// holodeck log records. Two quick countermands reach the agent in commit
// order. A captain-only second hail keeps the order running meanwhile.
// The captain's own answer can't be countermanded.
func TestHailCountermandCommand(t *testing.T) {
	t.Parallel()
	b := newHolodeckBridge(t, commandScript(t,
		map[string]any{"decision": "answer", "option": 0, "rationale": "deploys are routine", "confidence": 0.9},
		toolHail("make deploy"), toolHail("git push origin main")))
	b.regs = defaultRegs(t)
	hd := b.start(t)
	hd.addCommand(t)
	hd.addCrew(t)
	id := hd.createOrder(t)

	require.Equal(t, HailResult{Decided: true, By: comms.DecidedByCommand}, hailResult(t, hd.d, "H-0001"))
	hd.waitHail(t, "H-0002")
	hd.waitLog(t, "hail H-0002 for the captain")

	badReq := func(err error) {
		t.Helper()
		require.Error(t, err)
		require.Equal(t, CodeBadRequest, errorCode(t.Context(), err), err.Error())
	}
	deny, allow := 1, 0
	badReq(countermand(t.Context(), hd, "H-0002", "", &deny, "")) // open: answer it
	require.ErrorIs(t, countermand(t.Context(), hd, "H-9999", "", &deny, ""), ErrNotFound)
	bad := 2
	badReq(countermand(t.Context(), hd, "H-0001", "", &bad, ""))
	badReq(countermand(t.Context(), hd, "H-0001", "", nil, ""))

	require.NoError(t, countermand(t.Context(), hd, "H-0001", "", &deny, "staging is frozen"))
	h := hd.hail(t, "H-0001")
	require.Equal(t, store.HailDecided, h.Status)
	require.Equal(t, comms.DecidedByCaptain, h.DecidedBy)
	require.Equal(t, comms.DecidedByCommand, h.OriginalDecidedBy)
	require.Equal(t, comms.OptionDeny, h.Decision)
	require.Equal(t, "staging is frozen", h.Rationale)
	require.NotNil(t, h.CountermandedAt)
	e := hd.waitLog(t, "hail H-0001 countermanded: ")
	require.Equal(t, "hail H-0001 countermanded: staging is frozen", e.Text)
	require.Equal(t, id, e.OrderID)
	// A second one at once: the agent hears both, in commit order.
	require.NoError(t, countermand(t.Context(), hd, "H-0001", "", &allow, ""))

	require.Equal(t, []comms.CorrectionBody{
		{HailID: "H-0001", Ref: id, Note: "deny (staging is frozen)", Question: "Run make deploy?"},
		{HailID: "H-0001", Ref: id, Note: "allow", Question: "Run make deploy?"},
	}, requireCorrections(t, b.stepLog, 2))
	for _, e := range stepLogEntries(t, b.stepLog) {
		if e["action"] == "correction" {
			require.Equal(t, id, e["order"])
		}
	}
	// DBOS records send-correction once its send returns, which may be
	// after the holodeck logged the correction.
	require.Eventually(t, func() bool { return countSteps(t, hd.d, missionWorkflowID(id), "send-correction") == 2 },
		30*time.Second, 20*time.Millisecond)

	require.NoError(t, hd.d.AnswerHail(t.Context(), "H-0002", "", &allow))
	require.Equal(t, MissionDone, missionResult(t, hd.d, id).Status)
	require.ErrorIs(t, countermand(t.Context(), hd, "H-0002", "", &deny, ""), store.ErrCountermandExpired)
	for _, e := range hd.logEntries(t) {
		require.NotContains(t, e.Text, "not delivered")
	}
}

// countermand is Daemon.CountermandHail without its ended result.
func countermand(ctx context.Context, hd *hdaemon, id, decision string, option *int, reason string) error {
	_, err := hd.d.CountermandHail(ctx, id, decision, option, reason)
	return err
}

// countSteps counts workflow id's steps named name.
func countSteps(t *testing.T, d *Daemon, id, name string) int {
	t.Helper()
	n := 0
	for _, s := range stepNames(t, d, id) {
		if s == name {
			n++
		}
	}
	return n
}

// #153, B2: a countermand that commits before the mission confirms the
// decision (its decide-hail) doesn't wedge the mission. The decision and
// the countermand are queued for the mission in one store transaction
// here, so the mission reads both only after both committed: the agent
// gets Command's answer, then the correction, and the order finishes.
func TestHailCountermandBeforeDecideHail(t *testing.T) {
	t.Parallel()
	b := newHolodeckBridge(t, hailsScript(t, toolHail("make deploy"), toolHail("git push origin main")))
	hd := b.start(t)
	hd.addCrew(t)
	id := hd.createOrder(t)
	h := hd.waitHail(t, "H-0001")
	hd.waitLog(t, "hail H-0001 for the captain")

	// What decideOpen does for Command, and what CountermandHail then
	// does, committed together.
	zero := 0
	answer := comms.AnswerBody{HailID: h.ID, DecidedBy: comms.DecidedByCommand, Decision: comms.OptionAllow, Option: &zero,
		Rationale: "deploys are routine"}
	ans, err := syntheticMessage(comms.TypeAnswer, answer, h.CrewID, id)
	require.NoError(t, err)
	corr, err := syntheticMessage(comms.TypeCorrection, comms.CorrectionBody{HailID: h.ID, Ref: id, Note: "deny (frozen)"}, h.CrewID, id)
	require.NoError(t, err)
	entry, err := hailDecidedEntry(h.CrewID, id, answer)
	require.NoError(t, err)
	until := time.Now().Add(time.Hour)
	require.NoError(t, hd.d.store.DecideOpenHailAs(t.Context(), h.ID, store.HailDecision{DecidedBy: answer.DecidedBy,
		Decision: answer.Decision, Rationale: answer.Rationale, CountermandUntil: &until}, entry, func(tx *sql.Tx) error {
		if err := dbos.Send(hd.d.dbos, missionWorkflowID(id), ans, fromCrewTopic, dbos.WithSendTransaction(tx)); err != nil {
			return err
		}
		if _, err := tx.ExecContext(t.Context(), `UPDATE hails SET decided_by = 'captain', decision = 'deny', countermanded_at = ? WHERE id = ?`,
			time.Now().UTC(), h.ID); err != nil {
			return err
		}
		if err := dbos.Send(hd.d.dbos, missionWorkflowID(id), corr, fromCrewTopic, dbos.WithSendTransaction(tx)); err != nil {
			return err
		}
		return hd.d.notifyHailClosed(tx, h.ID)
	}))

	require.Equal(t, []comms.CorrectionBody{{HailID: h.ID, Ref: id, Note: "deny (frozen)", Question: h.Question}}, requireCorrections(t, b.stepLog, 1))
	// The answer reached the agent first. The holodeck logs the correction
	// as it arrives but the hail step only once its walk takes the answer,
	// so when both come in one read the file has them the other way round
	// (#178); their "delivery" numbers, counted by the order's serve, have
	// the order they arrived in. The hail step may be logged after the
	// correction, so wait for it.
	var delivered []map[string]any
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		delivered = nil
		for _, e := range readSteps(c, b.stepLog) {
			if e["order"] == id && (e["action"] == "hail" || e["action"] == "correction") {
				require.Contains(c, e, "delivery", "step log entry %v", e)
				delivered = append(delivered, e)
			}
		}
		require.Len(c, delivered, 2)
	}, 30*time.Second, 20*time.Millisecond)
	stepLogEntries(t, b.stepLog) // no step ran twice
	slices.SortFunc(delivered, func(a, b map[string]any) int { return cmp.Compare(a["delivery"].(float64), b["delivery"].(float64)) })
	var order []string
	for _, e := range delivered {
		order = append(order, fmt.Sprint(e["action"], " ", e["decided_by"]))
	}
	require.Equal(t, []string{"hail command", "correction <nil>"}, order)
	got := hd.hail(t, h.ID)
	require.Equal(t, comms.DecidedByCaptain, got.DecidedBy, "decide-hail left the countermand alone")
	require.Equal(t, "deny", got.Decision)

	hd.waitHail(t, "H-0002")
	allow := 0
	require.NoError(t, hd.d.AnswerHail(t.Context(), "H-0002", "", &allow))
	require.Equal(t, MissionDone, missionResult(t, hd.d, id).Status)
}

// #153, #167: a correction that reaches a mission whose beam is stopping
// (a stand_down went out) isn't sent to a run that is ending: the mission
// holds it, parks at the stopping checkpoint, and sends it right after the
// resume's order, when the tour claims the parked order again. The
// decision, the stand_down and the countermand are queued in one store
// transaction, so the mission reads them in that order.
func TestHailCountermandWhileStopping(t *testing.T) {
	t.Parallel()
	b := newHolodeckBridge(t, hailsScript(t, toolHail("make deploy"), toolHail("git push origin main")))
	hd := b.start(t)
	hd.addCrew(t)
	id := hd.createOrder(t)
	h := hd.waitHail(t, "H-0001")
	hd.waitLog(t, "hail H-0001 for the captain")

	zero := 0
	answer := comms.AnswerBody{HailID: h.ID, DecidedBy: comms.DecidedByCommand, Decision: comms.OptionAllow, Option: &zero}
	ans, err := syntheticMessage(comms.TypeAnswer, answer, h.CrewID, id)
	require.NoError(t, err)
	stop, err := syntheticMessage(comms.TypeStandDown, comms.StandDownBody{Reason: "watch change"}, h.CrewID, id)
	require.NoError(t, err)
	corr, err := syntheticMessage(comms.TypeCorrection, comms.CorrectionBody{HailID: h.ID, Ref: id, Note: "deny"}, h.CrewID, id)
	require.NoError(t, err)
	entry, err := hailDecidedEntry(h.CrewID, id, answer)
	require.NoError(t, err)
	until := time.Now().Add(time.Hour)
	mission := missionWorkflowID(id)
	require.NoError(t, hd.d.store.DecideOpenHailAs(t.Context(), h.ID, store.HailDecision{DecidedBy: answer.DecidedBy,
		Decision: answer.Decision, CountermandUntil: &until}, entry, func(tx *sql.Tx) error {
		for _, m := range []comms.Message{ans, stop, corr} {
			if err := dbos.Send(hd.d.dbos, mission, m, fromCrewTopic, dbos.WithSendTransaction(tx)); err != nil {
				return err
			}
		}
		return hd.d.notifyHailClosed(tx, h.ID)
	}))

	require.Equal(t, []comms.CorrectionBody{{HailID: h.ID, Ref: id, Note: "deny", Question: h.Question}}, requireCorrections(t, b.stepLog, 1))
	require.Eventually(t, func() bool { return countSteps(t, hd.d, mission, "send-correction") == 1 },
		30*time.Second, 20*time.Millisecond)
	names := stepNames(t, hd.d, mission)
	at := slices.Index(names, "send-correction")
	require.Less(t, slices.Index(names, "send-stand-down"), slices.Index(names, "park"))
	require.Greater(t, at, slices.Index(names, "unpark"))
	require.Equal(t, "send-order", names[at-1])
	for _, e := range hd.logEntries(t) {
		require.NotContains(t, e.Text, "not delivered")
	}
	// The resumed run hails again; a belay ends it.
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		require.Equal(c, store.OrderHailing, hd.order(c, id).State)
	}, 30*time.Second, 20*time.Millisecond)
	require.NoError(t, hd.d.BelayOrder(t.Context(), id, ""))
	require.Equal(t, MissionBelayed, missionResult(t, hd.d, id).Status)
}
