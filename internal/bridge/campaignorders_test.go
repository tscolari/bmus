package bridge

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tscolari/bmus/internal/comms"
	"github.com/tscolari/bmus/internal/store"
)

// testTask is the task workflow id of the campaign orders in these tests.
var testTask = store.TaskWorkflowID("C-0001", "o/r#12")

// orderKind gives the order command no kind outside a campaign, work for
// a task's work order, and a review's base and round from its row (#182).
func TestOrderKind(t *testing.T) {
	t.Parallel()
	kind, review := orderKind(store.Order{Kind: store.OrderKindWork})
	require.Empty(t, kind)
	require.Nil(t, review)
	kind, review = orderKind(store.Order{Kind: store.OrderKindWork, Task: testTask, Round: 2})
	require.Equal(t, comms.OrderKindWork, kind)
	require.Nil(t, review)
	kind, review = orderKind(store.Order{Kind: store.OrderKindReview, Task: testTask, Round: 2, ReviewBase: "8b1d0e2c"})
	require.Equal(t, comms.OrderKindReview, kind)
	require.Equal(t, &comms.OrderReview{Base: "8b1d0e2c", Round: 2}, review)
}

// verdictProblem passes only a verdict Check takes, read back from JSON as
// a DBOS step's output would be: a missing verdict, one that didn't decode
// and one outside the limits each name why, on one line.
func TestVerdictProblem(t *testing.T) {
	t.Parallel()
	read := func(raw string) *comms.Verdict {
		t.Helper()
		var body comms.ReportBody
		require.NoError(t, json.Unmarshal([]byte(`{"summary":"s","repos":[],"usage":{"input_tokens":0,"output_tokens":0},"verdict":`+raw+`}`), &body))
		// Through JSON again, as a step's recorded output.
		data, err := json.Marshal(body)
		require.NoError(t, err)
		var again comms.ReportBody
		require.NoError(t, json.Unmarshal(data, &again))
		return again.Verdict
	}
	require.Equal(t, "the report has no verdict", verdictProblem(nil))
	require.Empty(t, verdictProblem(read(`{"findings":[]}`)))
	require.Empty(t, verdictProblem(read(`{"findings":[{"severity":"low","title":"t","body":"b"}]}`)))
	require.Equal(t, "the verdict has no findings list (an empty list is a pass)", verdictProblem(read(`{"Findings":[]}`)))
	require.Equal(t, "the verdict is not an object", verdictProblem(read(`"pass"`)))
	require.Equal(t, `finding 1: severity "critical" is not high, medium or low`,
		verdictProblem(read(`{"findings":[{"severity":"critical","title":"t","body":"b"}]}`)))
	long := verdictProblem(read(`{"findings":[{"severity":"x\nrm -rf /` + fmt.Sprintf("%0600d", 0) + `","title":"t","body":"b"}]}`))
	require.LessOrEqual(t, len(long), maxVerdictProblem)
	require.NotContains(t, long, "\n")
}

// campaignDB opens the bridge's database at home for the rows no store
// call writes yet (campaigns, campaign_tasks, task_workers).
func campaignDB(t *testing.T, home string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(home, store.DBFile)+"?_pragma=busy_timeout(5000)")
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	return db
}

// insertTaskRow inserts campaign id on repo app and its task for ref in
// state, with head.
func insertTaskRow(t *testing.T, db *sql.DB, id, ref, state, head string) {
	t.Helper()
	_, err := db.ExecContext(t.Context(), `INSERT INTO campaigns (id, repo, base_ref, created_at) VALUES (?, 'app', 'main', ?)`,
		id, time.Now().UTC())
	require.NoError(t, err)
	_, err = db.ExecContext(t.Context(), `INSERT INTO campaign_tasks (campaign_id, ref, workflow_id, state, max_rounds, branch, head)
		VALUES (?, ?, ?, ?, 3, ?, ?)`, id, ref, store.TaskWorkflowID(id, ref), state, "campaign/"+id+"/b", head)
	require.NoError(t, err)
}

// A campaign order stays whole (#182): route refuses a review to a crew
// member who worked on its task and takes it to another; order.rebase and
// order.after refuse an order with a task, before any base check; a split
// of one is refused (checkSplit). The refusals log nothing.
func TestCampaignOrdersStayWhole(t *testing.T) {
	t.Parallel()
	home := privateHome(t)
	up := testRepo(t)
	cfg := testConfig(home)
	cfg.Repos = map[string]string{"app": up.url}
	cfg.Divisions = map[string]DivisionConfig{"science": {}, DivisionCommand: {}}
	r := startDaemonConfig(t, cfg)
	defer r.stop(t)
	d, ctx := r.d, t.Context()
	for _, c := range []string{"sorell", "tuvok"} {
		require.NoError(t, d.store.InsertCrew(ctx, store.Crew{ID: c, Name: c, Division: "science", Status: store.CrewOnDeck}, testEntry))
	}
	db := campaignDB(t, home)
	insertTaskRow(t, db, "C-0001", "o/r#12", store.TaskReviewing, "")
	_, err := db.ExecContext(ctx, `INSERT INTO task_workers (task, crew_id) VALUES (?, 'sorell')`, testTask)
	require.NoError(t, err)

	app := []store.OrderRepo{{Name: "app", BaseRef: "main"}}
	work, err := d.store.InsertOrder(ctx, store.Order{Title: "Implement o/r#12", Assignee: "sorell", State: store.OrderDone,
		Task: testTask, Round: 1, Repos: app}, testEntry)
	require.NoError(t, err)
	review, err := d.store.InsertOrder(ctx, store.Order{Title: "Review o/r#12 (round 1)", Division: "science", Kind: store.OrderKindReview,
		Task: testTask, Round: 1, ReviewBase: up.rev(t, "main"), Repos: []store.OrderRepo{{Name: "app", BaseRef: "order:" + work}}}, testEntry)
	require.NoError(t, err)
	fix, err := d.store.InsertOrder(ctx, store.Order{Title: "Fix o/r#12 (round 2)", Division: "science", Task: testTask, Round: 2,
		Repos: []store.OrderRepo{{Name: "app", BaseRef: "order:" + work}}, After: []string{work}}, testEntry)
	require.NoError(t, err)
	last, err := d.store.LastLogID(ctx)
	require.NoError(t, err)

	str := func(s string) *string { return &s }
	err = d.RouteOrder(ctx, review, str("sorell"), nil)
	requireCode(t, err, CodeBadRequest)
	require.EqualError(t, err, review+" reviews task o/r#12, which sorell worked on")
	err = requireRPCError(t, call(t, home, "order.route", OrderRouteArgs{ID: review, Assignee: str("sorell")}, nil), CodeBadRequest)
	require.ErrorContains(t, err, review+" reviews task o/r#12, which sorell worked on")
	require.Equal(t, store.OrderPool, getOrder(t, d, review).State, "the refusal routed nothing")

	for _, tc := range []struct {
		id, cmd string
		args    any
		what    string
	}{
		{review, "order.rebase", OrderRebaseArgs{ID: review, Onto: "main"}, "review of"},
		{fix, "order.rebase", OrderRebaseArgs{ID: fix, Onto: "no-such-ref"}, "work on"},
		{fix, "order.after", OrderAfterArgs{ID: fix, Add: []string{review}}, "work on"},
	} {
		err := requireRPCError(t, call(t, home, tc.cmd, tc.args, nil), CodeBadRequest)
		require.ErrorContains(t, err, tc.id+" is campaign C-0001's "+tc.what+" o/r#12: a campaign order can't be reordered", tc.cmd)
	}
	require.Equal(t, []string{work}, getOrder(t, d, fix).After, "the refusals reordered nothing")

	vance := store.Crew{ID: "vance", Division: DivisionCommand}
	split := store.Hail{OrderID: fix, Category: comms.CategoryOrderSplit, Options: []string{comms.OptionAllow, comms.OptionDeny},
		Context: json.RawMessage(`{"children":[{"title":"t","body":"b"}]}`)}
	kids, why := d.checkSplit(split, getOrder(t, d, fix), vance)
	require.Nil(t, kids)
	require.Equal(t, fix+" is campaign C-0001's work on o/r#12: a campaign order can't be split", why)

	entries, err := d.store.ListLog(ctx, last)
	require.NoError(t, err)
	require.Empty(t, entries, "the refusals logged nothing")

	require.NoError(t, d.RouteOrder(ctx, review, str("tuvok"), nil))
	o := getOrder(t, d, review)
	require.Equal(t, []string{store.OrderDirected, "tuvok"}, []string{o.State, o.Assignee})

	// The views carry the kind, the task and the round.
	id, err := d.store.InsertOrder(ctx, store.Order{Title: "plain", Division: "science", Repos: app}, testEntry)
	require.NoError(t, err)
	var list OrderListResult
	require.NoError(t, call(t, home, "order.list", struct{}{}, &list))
	views := map[string]OrderView{}
	for _, v := range list.Orders {
		views[v.ID] = v
	}
	require.Equal(t, []any{store.OrderKindReview, testTask, 1}, []any{views[review].Kind, views[review].Task, views[review].Round})
	require.Equal(t, []any{store.OrderKindWork, testTask, 2}, []any{views[fix].Kind, views[fix].Task, views[fix].Round})
	require.Equal(t, []any{store.OrderKindWork, "", 0}, []any{views[id].Kind, views[id].Task, views[id].Round})
}

// Jettison keeps bmus/<head> of a task that hasn't landed or been dropped
// (#182), however long ago its order ended, and takes a landed task's head
// like any other ended order's branch.
func TestJettisonKeepsTaskHeads(t *testing.T) {
	t.Parallel()
	home := privateHome(t)
	up := testRepo(t)
	cfg := testConfig(home)
	cfg.Repos = map[string]string{"app": up.url}
	r := startDaemonConfig(t, cfg)
	defer r.stop(t)
	d, ctx := r.d, t.Context()

	old := time.Now().Add(-30 * 24 * time.Hour)
	insert := func(task string) string {
		t.Helper()
		id, err := d.store.InsertOrder(ctx, store.Order{Title: "t", Division: "ops", State: store.OrderDone, Task: task,
			Repos: []store.OrderRepo{{Name: "app", BaseRef: "main"}}}, store.LogEntry{Kind: logOrder, Text: "order created: t", TS: old})
		require.NoError(t, err)
		return id
	}
	head := insert(testTask)
	landed := insert(store.TaskWorkflowID("C-0002", "o/r#13"))
	plain := insert("")
	db := campaignDB(t, home)
	insertTaskRow(t, db, "C-0001", "o/r#12", store.TaskStalled, head)
	insertTaskRow(t, db, "C-0002", "o/r#13", store.TaskLanded, landed)

	base := up.rev(t, "main")
	clone := d.repoDir("app")
	gitT(t, "", "init", "-q", "--bare", clone)
	gitT(t, clone, "fetch", "-q", up.url, "+refs/heads/*:refs/remotes/origin/*")
	for _, id := range []string{head, landed, plain} {
		gitT(t, clone, "update-ref", "refs/heads/"+orderBranch(id), base)
	}

	var res JettisonResult
	require.NoError(t, call(t, home, "bridge.jettison", JettisonArgs{Yes: true}, &res))
	var gone []string
	for _, b := range res.Branches {
		gone = append(gone, b.Order)
	}
	require.Equal(t, []string{landed, plain}, gone)
	require.Equal(t, 1, res.Kept)
	require.Equal(t, base, gitT(t, clone, "rev-parse", orderBranch(head)), "the stalled task's head stays")
}

// reviewScript writes a holodeck script that files a finding, commits and
// reports with verdict, raw JSON, or none when verdict is empty.
func reviewScript(t *testing.T, verdict string) string {
	t.Helper()
	report := `{"summary": "reviewed"}`
	if verdict != "" {
		report = `{"summary": "reviewed", ` + verdict + `}`
	}
	script := filepath.Join(t.TempDir(), "review.json")
	require.NoError(t, os.WriteFile(script, []byte(`[
		{"at_pct": 20, "finding": {"key": "stale-docs", "title": "The docs are stale", "body": "Outside the task."}},
		{"at_pct": 40, "commit": "a reviewer's change"},
		{"at_pct": 100, "report": `+report+`}]`), 0o600))
	return script
}

// createReview creates review round of testTask, directed to sorell.
func (hd *hdaemon) createReview(t *testing.T, base string, round int) string {
	t.Helper()
	id, err := hd.d.CreateOrder(t.Context(), store.Order{Title: fmt.Sprintf("Review o/r#12 (round %d)", round), Body: "review it",
		Assignee: "sorell", Kind: store.OrderKindReview, Task: testTask, Round: round, ReviewBase: base,
		Repos: []store.OrderRepo{{Name: "app", BaseRef: "main"}}})
	require.NoError(t, err)
	return id
}

// entryOf is order id's only entry of kind.
func entryOf(t *testing.T, entries []store.LogEntry, id, kind string) store.LogEntry {
	t.Helper()
	var found []store.LogEntry
	for _, e := range entries {
		if e.OrderID == id && e.Kind == kind {
			found = append(found, e)
		}
	}
	require.Len(t, found, 1, "%s entries of %s", kind, id)
	return found[0]
}

// #182's acceptance, on holodeck: a review that files a finding, commits
// and reports a verdict is done, its verdict in the done entry and the
// mission's report, picked by the round the order command carried; its
// commit is pulled like any work, and no findings workflow starts for it.
// A work order's verdict is dropped, and its findings workflow starts.
func TestMissionReviewVerdict(t *testing.T) {
	t.Parallel()
	b := newHolodeckBridge(t, reviewScript(t, `"verdicts": [
		{"findings": [{"severity": "high", "title": "TestSnapshot races compaction", "body": "Fails once in 20 runs.", "path": "internal/raft/snapshot_test.go", "line": 88}]},
		{"findings": []}]`))
	hd := b.start(t)
	hd.addCrew(t)
	base := b.repo.rev(t, "main")
	clone := filepath.Join(b.home, "repos", "app")

	run := func(id string) MissionResult {
		t.Helper()
		require.EventuallyWithT(t, func(c *assert.CollectT) {
			require.Len(c, workflows(c, hd.d, missionWorkflowID(id)), 1)
		}, 30*time.Second, 20*time.Millisecond)
		res := missionResult(t, hd.d, id)
		require.Equal(t, MissionDone, res.Status, "%+v", res)
		require.Equal(t, store.OrderDone, hd.order(t, id).State)
		require.NotEmpty(t, gitT(t, clone, "rev-parse", orderBranch(id)), "the work was pulled")
		n, err := hd.d.store.CountFindings(t.Context(), id)
		require.NoError(t, err)
		require.Equal(t, 1, n)
		return res
	}
	donePayload := func(id string) map[string]json.RawMessage {
		t.Helper()
		var p map[string]json.RawMessage
		require.NoError(t, json.Unmarshal(entryOf(t, hd.logEntries(t), id, logDone).Payload, &p))
		return p
	}

	review1 := hd.createReview(t, base, 1)
	res := run(review1)
	high := []comms.VerdictFinding{{Severity: comms.SeverityHigh, Title: "TestSnapshot races compaction", Body: "Fails once in 20 runs.",
		Path: "internal/raft/snapshot_test.go", Line: 88}}
	require.NotNil(t, res.Report.Verdict)
	require.Equal(t, high, res.Report.Verdict.Findings)
	var v comms.Verdict
	require.NoError(t, json.Unmarshal(donePayload(review1)["verdict"], &v))
	require.NoError(t, v.Check())
	require.Equal(t, high, v.Findings)

	review2 := hd.createReview(t, base, 2)
	res = run(review2)
	require.NotNil(t, res.Report.Verdict)
	require.Empty(t, res.Report.Verdict.Findings, "round 2 takes verdicts[1]")
	require.JSONEq(t, `{"findings":[]}`, string(donePayload(review2)["verdict"]))

	work, err := hd.d.CreateOrder(t.Context(), store.Order{Title: "Implement o/r#12", Assignee: "sorell", Task: testTask, Round: 1,
		Repos: []store.OrderRepo{{Name: "app", BaseRef: "main"}}})
	require.NoError(t, err)
	res = run(work)
	require.Nil(t, res.Report.Verdict, "a work order's verdict is dropped")
	require.NotContains(t, donePayload(work), "verdict")

	require.EventuallyWithT(t, func(c *assert.CollectT) {
		require.Len(c, workflows(c, hd.d, findingsWorkflowID(work)), 1)
	}, 30*time.Second, 20*time.Millisecond)
	for _, id := range []string{review1, review2} {
		require.Empty(t, workflows(t, hd.d, findingsWorkflowID(id)), "no findings workflow for review %s", id)
	}
	requireNoBackstop(t, hd.logEntries(t))
}

// A review whose report has no verdict, or one Check refuses, fails
// no_verdict once its work is pulled (#182), and starts no findings
// workflow.
func TestMissionReviewNoVerdict(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, verdict, why string
	}{
		{"missing", "", "the report has no verdict"},
		{"invalid", `"verdict": {"findings": [{"severity": "critical", "title": "t", "body": "b"}]}`,
			`finding 1: severity "critical" is not high, medium or low`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			b := newHolodeckBridge(t, reviewScript(t, tc.verdict))
			hd := b.start(t)
			hd.addCrew(t)
			id := hd.createReview(t, b.repo.rev(t, "main"), 1)
			require.EventuallyWithT(t, func(c *assert.CollectT) {
				require.Len(c, workflows(c, hd.d, missionWorkflowID(id)), 1)
			}, 30*time.Second, 20*time.Millisecond)
			res := missionResult(t, hd.d, id)
			reason := "the review reported no valid verdict: " + tc.why
			require.Equal(t, MissionResult{Status: MissionFailed, Reason: reason, Code: CodeNoVerdict}, res)
			require.Equal(t, store.OrderFailed, hd.order(t, id).State)
			require.NotEmpty(t, gitT(t, filepath.Join(b.home, "repos", "app"), "rev-parse", orderBranch(id)), "the work was pulled")

			entries := hd.logEntries(t)
			fail := entryOf(t, entries, id, logFail)
			require.Equal(t, reason, fail.Text)
			require.JSONEq(t, fmt.Sprintf(`{"reason":%q,"code":"no_verdict"}`, reason), string(fail.Payload))
			for _, e := range entries {
				require.False(t, e.OrderID == id && e.Kind == logDone, "no done entry")
			}
			n, err := hd.d.store.CountFindings(t.Context(), id)
			require.NoError(t, err)
			require.Equal(t, 1, n)
			names := stepNames(t, hd.d, missionWorkflowID(id))
			require.Contains(t, names, "count-findings")
			require.Empty(t, workflows(t, hd.d, findingsWorkflowID(id)))
			requireNoBackstop(t, entries)
		})
	}
}
