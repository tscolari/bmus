package bridge

import (
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/dbos-inc/dbos-transact-golang/dbos"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tscolari/bmus/internal/comms"
	"github.com/tscolari/bmus/internal/store"
	"github.com/tscolari/bmus/internal/tracker"
	"github.com/tscolari/bmus/internal/tracker/trackertest"
)

// #153: a holodeck script's finding steps reach the bridge's findings table
// and the order's log before its report settles the order, the second
// finding with the same key replacing the first. Nothing reaches the
// mission but the report.
func TestHolodeckFindingsRecorded(t *testing.T) {
	t.Parallel()
	b := newHolodeckBridge(t, fixture(t, "findings.json"))
	hd := b.start(t)
	hd.addCrew(t)
	id := hd.createOrder(t)
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		require.Len(c, workflows(c, hd.d, missionWorkflowID(id)), 1)
	}, 10*time.Second, 20*time.Millisecond)

	res := missionResult(t, hd.d, id)
	require.Equal(t, MissionDone, res.Status, "%+v", res)
	require.Equal(t, store.OrderDone, hd.order(t, id).State)

	got, err := hd.d.store.OrderFindings(t.Context(), id)
	require.NoError(t, err)
	require.Len(t, got, 2)
	for i := range got {
		require.False(t, got[i].ReceivedAt.IsZero())
		got[i].ReceivedAt = time.Time{}
	}
	require.Equal(t, []store.Finding{
		{OrderID: id, Key: "flaky-test", CrewID: "sorell", Title: "TestSnapshot races the compactor",
			Body: "The flake is a race with compaction.", Labels: []string{}, Severity: "high", BlockedBy: []string{}, State: store.FindingNew},
		{OrderID: id, Key: "snapshot-leak", CrewID: "sorell", Title: "Snapshot writer leaks a file handle on error",
			Body: "Short writes return without closing the temp file.", Labels: []string{"bug", "raft"}, Severity: "medium",
			BlockedBy: []string{"flaky-test", "#12"}, State: store.FindingNew},
	}, got)

	entries := hd.logEntries(t)
	var logged []string
	findingsBefore := -1
	for i, e := range entries {
		if e.Kind == comms.FindingLogKind {
			require.Equal(t, id, e.OrderID)
			require.Equal(t, "sorell", e.CrewID)
			logged = append(logged, e.Text)
			findingsBefore = i
		}
	}
	require.Equal(t, []string{
		"finding flaky-test: TestSnapshot is flaky",
		"finding snapshot-leak: Snapshot writer leaks a file handle on error",
		"finding flaky-test: TestSnapshot races the compactor",
	}, logged)
	for _, e := range entries[:findingsBefore] {
		require.NotEqual(t, logDone, e.Kind, "every finding is logged before the order is done")
	}
	requireNoBackstop(t, entries)

	// Without a tracker, nothing is filed and the rows stay new (#155).
	require.Equal(t, FindingsResult{NotFiled: "no tracker configured"}, findingsResult(t, hd.d, id))
	require.Contains(t, orderTexts(hd.logEntries(t), id, "findings of"), "findings of "+id+" not filed: no tracker configured")
}

// findingsResult waits for order id's findings workflow to end and
// returns its result.
func findingsResult(t *testing.T, d *Daemon, id string) FindingsResult {
	t.Helper()
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		require.Len(c, workflows(c, d, findingsWorkflowID(id)), 1)
	}, 60*time.Second, 20*time.Millisecond)
	h, err := dbos.RetrieveWorkflow[FindingsResult](d.dbos, findingsWorkflowID(id))
	require.NoError(t, err)
	res, err := h.GetResult(dbos.WithHandleTimeout(90 * time.Second))
	require.NoError(t, err)
	return res
}

// findingStep is a holodeck finding step at pct.
func findingStep(pct int, f map[string]any) map[string]any {
	return map[string]any{"at_pct": pct, "finding": f}
}

// Plan Verification 6, triage: an order's script files four findings and
// reports; once its mission ends, Command (scripted) drops one, marks one a
// duplicate of an existing issue and adds blockers to another. The two
// left are filed blockers first, with the duplicate's blocker linked to
// the issue it repeats, another repository's ref dropped and the body's
// mention neutralised.
func TestFindingsTriageByCommand(t *testing.T) {
	t.Parallel()
	steps := []map[string]any{
		findingStep(10, map[string]any{"key": "a-noise", "title": "Typo in a comment", "body": "teh"}),
		findingStep(20, map[string]any{"key": "b-dup", "title": "TestSnapshot is flaky", "body": "Again."}),
		findingStep(30, map[string]any{"key": "c-main", "title": "Snapshot writer leaks a handle", "body": "Ask @alice.",
			"severity": "high", "blocked_by": []string{"b-dup"}}),
		findingStep(40, map[string]any{"key": "d-base", "title": "Compactor holds the lock", "body": "",
			"blocked_by": []string{"other/repo#9"}}),
		{"at_pct": 100, "report": map[string]any{"summary": "done"}},
	}
	script, err := json.Marshal(map[string]any{"steps": steps, "consult": map[string]any{"decision": "answer",
		"answer": `{"drop":[1],"duplicates":{"2":"#1"},"blocked_by":{"3":["#2","f4"]}}`, "rationale": "1 is noise; 2 is #1",
		"confidence": 0.9}})
	require.NoError(t, err)
	fake := &trackertest.Fake{}
	flaky := fake.Add(tracker.Item{Title: "TestSnapshot flakes"})
	leakOld := fake.Add(tracker.Item{Title: "Snapshot temp files pile up", Labels: []string{"bug"}})
	b := newHolodeckBridge(t, writeScript(t, string(script)))
	b.regs = defaultRegs(t)
	b.tracker = fake
	hd := b.start(t)
	hd.addCommand(t)
	hd.addCrew(t)
	id := hd.createOrder(t)
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		require.Len(c, workflows(c, hd.d, missionWorkflowID(id)), 1)
	}, 30*time.Second, 20*time.Millisecond)
	require.Equal(t, MissionDone, missionResult(t, hd.d, id).Status)
	require.Equal(t, FindingsResult{Filed: 2, Dropped: 2}, findingsResult(t, hd.d, id))

	names := stepNames(t, hd.d, missionWorkflowID(id))
	require.Greater(t, slices.Index(names, "count-findings"), slices.Index(names, "current-tour"), names)
	require.Greater(t, slices.Index(names, "current-tour"), 0, names)

	cs := consults(t, b.stepLog)
	require.Len(t, cs, 1)
	var c triageHailContext
	require.NoError(t, json.Unmarshal(cs[0].Context, &c))
	require.Equal(t, id, c.Order)
	require.Equal(t, []string{"a-noise", "b-dup", "c-main", "d-base"}, []string{c.Findings[0].Key, c.Findings[1].Key,
		c.Findings[2].Key, c.Findings[3].Key})
	require.Equal(t, 3, c.Findings[2].N)
	require.Equal(t, []backlogItem{{Ref: leakOld, Title: "Snapshot temp files pile up", Labels: []string{"bug"}},
		{Ref: flaky, Title: "TestSnapshot flakes"}}, c.Backlog)
	require.Zero(t, c.BacklogOmitted)
	require.Equal(t, triageQuestion(id, 4), cs[0].Question)

	got, err := hd.d.store.OrderFindings(t.Context(), id)
	require.NoError(t, err)
	state := map[string]store.Finding{}
	for _, f := range got {
		state[f.Key] = f
	}
	require.Equal(t, store.FindingDropped, state["a-noise"].State)
	require.Equal(t, "1 is noise; 2 is #1", state["a-noise"].Why)
	require.Empty(t, state["a-noise"].DuplicateOf)
	require.Equal(t, store.FindingDropped, state["b-dup"].State)
	require.Equal(t, flaky, state["b-dup"].DuplicateOf)
	require.Equal(t, []string{"b-dup", leakOld, "d-base"}, state["c-main"].BlockedBy)
	require.Equal(t, store.FindingFiled, state["d-base"].State)
	require.Equal(t, "#3", state["d-base"].Ref, "the blocker is filed first")
	require.Equal(t, store.FindingFiled, state["c-main"].State)
	require.Equal(t, "#4", state["c-main"].Ref)

	main, err := fake.Get(t.Context(), "#4")
	require.NoError(t, err)
	require.Equal(t, []string{flaky, leakOld, "#3"}, main.BlockedBy)
	require.Equal(t, []string{labelUnreviewed}, main.Labels)
	require.Equal(t, "Ask `@alice`.\n\nFiled by sorell from "+id+" (severity high).", main.Body)
	base, err := fake.Get(t.Context(), "#3")
	require.NoError(t, err)
	require.Empty(t, base.BlockedBy)
	items, err := fake.List(t.Context(), tracker.Query{})
	require.NoError(t, err)
	require.Len(t, items, 4, "the dropped findings filed nothing")

	texts := orderTexts(hd.logEntries(t), id, "")
	require.Contains(t, texts, "findings of "+id+" triaged by Command: dropped 2 (1 as duplicates), 2 blockers added")
	require.Contains(t, texts, `finding d-base: blocker other/repo#9 dropped: tracker: bad ref: "other/repo#9" is not #<n>`)
	require.Contains(t, texts, "finding c-main filed as #4")
	require.Contains(t, texts, "findings of "+id+": filed 2, dropped 2, failed 0")
}

// Plan decision 3: a failed order's findings are filed. The order hails,
// a belay reaches its beam and the run ends with an error; with no
// regs.yaml triage is skipped, and the finding is filed.
func TestFindingsOfFailedOrder(t *testing.T) {
	t.Parallel()
	script, err := json.Marshal([]map[string]any{
		findingStep(10, map[string]any{"key": "leak", "title": "Leak", "body": "b"}),
		{"at_pct": 20, "hail": toolHail("make deploy")},
		{"at_pct": 100, "report": map[string]any{"summary": "done"}},
	})
	require.NoError(t, err)
	fake := &trackertest.Fake{}
	b := newHolodeckBridge(t, writeScript(t, string(script)))
	b.tracker = fake
	hd := b.start(t)
	hd.addCrew(t)
	id := hd.createOrder(t)
	hd.waitHail(t, "H-0001")
	belay, err := comms.NewMessage(comms.TypeBelay, comms.BelayBody{Reason: "captain changed course"})
	require.NoError(t, err)
	belay.Order = id
	_, err = hd.d.SendCommand("sorell", belay)
	require.NoError(t, err)
	require.Equal(t, MissionFailed, missionResult(t, hd.d, id).Status)

	require.Equal(t, FindingsResult{Filed: 1}, findingsResult(t, hd.d, id))
	it, err := fake.Get(t.Context(), "#1")
	require.NoError(t, err)
	require.Equal(t, "Leak", it.Title)
	texts := orderTexts(hd.logEntries(t), id, "")
	require.Contains(t, texts, "findings of "+id+" filed without triage: "+infoRegsNotConfigured)
	require.Contains(t, texts, "findings of "+id+": filed 1, dropped 0, failed 0")
}
