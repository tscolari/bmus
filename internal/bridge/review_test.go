package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dbos-inc/dbos-transact-golang/dbos"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tscolari/bmus/internal/regs"
	"github.com/tscolari/bmus/internal/store"
	"github.com/tscolari/bmus/internal/tracker"
	"github.com/tscolari/bmus/internal/tracker/trackertest"
)

// reviewHail is order id's open tracker_review hail, read with t's
// context and failing c; the zero Hail for none.
func reviewHail(c require.TestingT, t *testing.T, d *Daemon, id string) store.Hail {
	hails, err := d.store.ListHails(t.Context(), store.HailOpen)
	require.NoError(c, err)
	for _, h := range hails {
		if h.Category == regs.CategoryTrackerReview && h.DeliverTo == findingsWorkflowID(id) {
			return h
		}
	}
	return store.Hail{}
}

// waitReview waits for order id's tracker_review hail to be open and
// returns it.
func waitReview(t *testing.T, d *Daemon, id string) store.Hail {
	t.Helper()
	var h store.Hail
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		h = reviewHail(c, t, d, id)
		require.NotEmpty(c, h.ID)
	}, 90*time.Second, 20*time.Millisecond)
	return h
}

// endFindings waits for order id's findings workflow to end, answering
// decision to its review once the review is open, and returns its result.
func endFindings(t *testing.T, d *Daemon, id, decision string) FindingsResult {
	t.Helper()
	answered := false
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		if !answered {
			if h := reviewHail(c, t, d, id); h.ID != "" {
				require.NoError(c, d.AnswerHail(t.Context(), h.ID, decision, nil))
				answered = true
			}
		}
		wfs := workflows(c, d, findingsWorkflowID(id))
		require.Len(c, wfs, 1)
		require.Equal(c, dbos.WorkflowStatusSuccess, wfs[0].Status)
	}, 90*time.Second, 20*time.Millisecond)
	h, err := dbos.RetrieveWorkflow[FindingsResult](d.dbos, findingsWorkflowID(id))
	require.NoError(t, err)
	res, err := h.GetResult(dbos.WithHandleTimeout(30 * time.Second))
	require.NoError(t, err)
	return res
}

// testReview is a review of three issues, a dropped finding and a failed
// one.
var testReview = ReviewContext{Order: "ORD-0001",
	Issues:  []ReviewIssue{{Ref: "#2", Key: "a", Title: "A"}, {Ref: "#3", Key: "b", Title: "B"}, {Ref: "#4", Key: "c", Title: "C"}},
	Dropped: []ReviewDropped{{Key: "noise", Title: "Noise", Why: "noise"}},
	Failed:  []ReviewFailed{{Key: "lost", Title: "Lost", Error: "tracker down"}}}

// The answer forms, in normal form, and what isn't one.
func TestParseReview(t *testing.T) {
	t.Parallel()
	canon := (&trackertest.Fake{}).Canon
	for _, tc := range []struct {
		answer, normal string
		option         int
	}{
		{"keep", "keep", 0},
		{" KEEP ", "keep", 0},
		{"discard", "discard", 1},
		{"discard #4 #2", "discard #2 #4", 1},
		{"discard #2, #2", "discard #2", 1},
		{"discard #2 #3 #4", "discard", 1},
		{"discard #2; restore noise", "discard #2; restore noise", 1},
		{"keep ; restore lost noise", "keep; restore noise lost", 0},
		{"restore lost", "keep; restore lost", 0},
		{"Discard; Restore NOISE", "discard; restore noise", 1},
	} {
		d, err := parseReview(tc.answer, testReview, canon)
		require.NoError(t, err, tc.answer)
		require.Equal(t, tc.normal, d.normal(len(testReview.Issues)), tc.answer)
		require.Equal(t, tc.option, d.option(), tc.answer)
		again, err := parseReview(tc.normal, testReview, nil)
		require.NoError(t, err, "the normal form parses without canon")
		require.Equal(t, d, again)
	}
	for _, tc := range []struct{ answer, err string }{
		{"", "empty answer"},
		{"maybe", `"maybe" is not keep, discard or restore`},
		{"keep #2", "keep takes no refs (got #2)"},
		{"discard #9", `"#9" is not a ref of the review`},
		{"discard a", `"a" is not a ref of the review`},
		{"restore", "restore names no key"},
		{"restore a", `"a" is not a dropped or failed finding's key`},
		{"keep; restore", "restore names no key"},
		{"keep; discard #2", "after ; comes restore <key>..."},
		{"keep; restore noise; restore lost", "more than one ;"},
		{"restore noise; restore lost", "restore comes last"},
		{"discard ,", "discard names no ref (discard alone discards every issue)"},
		{"discard,", "discard names no ref (discard alone discards every issue)"},
		{"discard , ; restore noise", "discard names no ref (discard alone discards every issue)"},
		{"restore ,", "restore names no key"},
		{"keep; restore ,", "restore names no key"},
	} {
		_, err := parseReview(tc.answer, testReview, canon)
		require.EqualError(t, err, tc.err, tc.answer)
	}
	// An empty review: discard discards nothing.
	d, err := parseReview("discard", ReviewContext{}, nil)
	require.NoError(t, err)
	require.Equal(t, "keep", d.normal(0))
}

// AnswerHail checks a tracker_review answer against its context before it
// decides anything: a bad one names the refs and keys, an option that
// contradicts the decision is refused, and Command's triage hail can't be
// answered.
func TestAnswerReviewRefused(t *testing.T) {
	t.Parallel()
	raw, err := json.Marshal(testReview)
	require.NoError(t, err)
	d, id := bridgeHailStore(t, nil, store.Hail{Category: regs.CategoryTrackerReview, Question: "Review?",
		Options: reviewOptions, Context: raw})
	ctx := t.Context()
	err = d.AnswerHail(ctx, id, "discard #9", nil)
	require.EqualError(t, err, `hail `+id+`: "#9" is not a ref of the review; answer keep, discard, discard <ref>..., `+
		`or restore <key>..., the first three optionally followed by "; restore <key>..." (refs: #2, #3, #4; keys: noise, lost)`)
	var re *rpcError
	require.True(t, errors.As(err, &re))
	require.Equal(t, CodeBadRequest, re.code)
	zero := 0
	require.EqualError(t, d.AnswerHail(ctx, id, "discard #2", &zero),
		`hail `+id+`: option 0 (keep) does not match the decision "discard #2"`)
	h, err := d.store.GetHail(ctx, id)
	require.NoError(t, err)
	require.Equal(t, store.HailOpen, h.Status)

	empty, eid := bridgeHailStore(t, nil, store.Hail{Category: regs.CategoryTrackerReview, Question: "Review?",
		Options: reviewOptions, Context: json.RawMessage(`{"order":"ORD-0001"}`)})
	require.ErrorContains(t, empty.AnswerHail(ctx, eid, "restore x", nil), "(refs: none; keys: none)")

	triage, tid := bridgeHailStore(t, nil, store.Hail{Category: categoryFindingsTriage, Question: "Triage?"})
	require.EqualError(t, triage.AnswerHail(ctx, tid, "{}", nil),
		"hail "+tid+" is Command's advisory triage; review the issues in its tracker_review hail")
}

// The review's batch lists only rows filing moved on, and none raises no
// review.
func TestReviewBatch(t *testing.T) {
	t.Parallel()
	fake := &trackertest.Fake{}
	d, id := findingsStore(t, fake, store.Finding{Key: "a", Title: "A"}, store.Finding{Key: "b", Title: "B"},
		store.Finding{Key: "c", Title: "C"}, store.Finding{Key: "d", Title: "D"})
	ctx := t.Context()
	b, err := d.reviewBatch(ctx, id)
	require.NoError(t, err)
	require.False(t, b.Raise, "nothing filed, dropped or failed")

	_, err = d.store.SettleFinding(ctx, id, "a", store.FindingFiled, "#1", "")
	require.NoError(t, err)
	_, err = d.store.SettleFinding(ctx, id, "b", store.FindingFailed, "", "tracker down")
	require.NoError(t, err)
	e := store.LogEntry{Kind: logOrder, OrderID: id, Text: "triaged"}
	require.NoError(t, d.store.TriageFindings(ctx, id, []store.FindingTriage{{Key: "c", Drop: true, Why: "dup", DuplicateOf: "#9"}}, e))
	b, err = d.reviewBatch(ctx, id)
	require.NoError(t, err)
	require.True(t, b.Raise)
	require.Equal(t, ReviewContext{Order: id, Issues: []ReviewIssue{{Ref: "#1", Key: "a", Title: "A"}},
		Dropped: []ReviewDropped{{Key: "c", Title: "C", Why: "dup", DuplicateOf: "#9"}},
		Failed:  []ReviewFailed{{Key: "b", Title: "B", Error: "tracker down"}}}, b.Context, "d is still new: left out")
	require.Equal(t, "Review 1 issue filed from "+id+"'s findings (1 dropped, 1 failed): keep, discard, "+
		"or answer with the refs to discard and the keys to restore.", reviewQuestion(b.Context))
	_, err = d.reviewBatch(ctx, id)
	require.NoError(t, err)
	var left []string
	for _, s := range orderLogTexts(t, d, id) {
		if strings.HasPrefix(s, "findings of "+id+" left out of the review") {
			left = append(left, s)
		}
	}
	require.Equal(t, []string{"findings of " + id + " left out of the review: 4 still new"}, left,
		"the rows still new are counted, once per order")
}

// review-keep, review-discard and review-restore are safe to repeat: the
// second run changes nothing and files nothing.
func TestReviewStepsRepeat(t *testing.T) {
	t.Parallel()
	fake := &trackertest.Fake{}
	d, id := findingsStore(t, fake,
		store.Finding{Key: "a", Title: "A"}, store.Finding{Key: "b", Title: "B"},
		store.Finding{Key: "c", Title: "C", Labels: []string{"bug"}, BlockedBy: []string{"a", "b", "#9", "zzz"}},
		store.Finding{Key: "d", Title: "D"})
	ctx := t.Context()
	begin := findingsBegin{Since: time.Now(), Nonce: d.store.Nonce()}
	for _, k := range []string{"a", "b"} {
		got, err := d.fileFinding(ctx, id, k, nil, begin)
		require.NoError(t, err)
		require.Equal(t, store.FindingFiled, got.State)
	}
	e := store.LogEntry{Kind: logOrder, OrderID: id, Text: "triaged"}
	require.NoError(t, d.store.TriageFindings(ctx, id, []store.FindingTriage{{Key: "c", Drop: true, Why: "noise"}}, e))

	for range 2 {
		out, err := d.reviewKeep(ctx, id, "a")
		require.NoError(t, err)
		require.Equal(t, reviewOutcome{State: store.FindingKept}, out)
		out, err = d.reviewDiscard(ctx, id, "b")
		require.NoError(t, err)
		require.Equal(t, reviewOutcome{State: store.FindingDiscarded}, out)
		out, err = d.reviewRestore(ctx, id, "c", begin)
		require.NoError(t, err)
		require.Equal(t, reviewOutcome{State: store.FindingRestored}, out)
	}
	a, err := fake.Get(ctx, "#1")
	require.NoError(t, err)
	require.Empty(t, a.Labels)
	require.Equal(t, tracker.ReasonNotPlanned, fake.CloseReason("#2"))
	c, err := fake.Get(ctx, "#3")
	require.NoError(t, err)
	require.Equal(t, "C", c.Title)
	require.Equal(t, []string{"bug"}, c.Labels, "restored without bmus:unreviewed")
	require.Equal(t, []string{"#1"}, c.BlockedBy, "the kept issue links; the discarded one doesn't")
	_, err = fake.Get(ctx, "#4")
	require.ErrorIs(t, err, tracker.ErrNotFound, "restored once")
	row, err := d.store.GetFinding(ctx, id, "c")
	require.NoError(t, err)
	require.Equal(t, "#3", row.Ref)
	require.Empty(t, row.Why)
	texts := orderLogTexts(t, d, id)
	require.Contains(t, texts, "finding c: blocker b dropped: it has no open issue (discarded)")
	require.Contains(t, texts, "finding c: blocker zzz dropped: it names no other finding of "+id)
	require.Contains(t, texts, "finding c restored as #3")
	n := 0
	for _, s := range texts {
		if strings.HasPrefix(s, "finding c restored") {
			n++
		}
	}
	require.Equal(t, 1, n)

	// A step for a row not filed (d is new) does nothing.
	out, err := d.reviewKeep(ctx, id, "d")
	require.NoError(t, err)
	require.Equal(t, store.FindingNew, out.State)
	out, err = d.reviewRestore(ctx, id, "d", begin)
	require.NoError(t, err)
	require.Equal(t, store.FindingNew, out.State)
}

// Restoring a failed finding whose issue a lost answer had already filed
// under its marker finds that issue and removes its bmus:unreviewed.
func TestReviewRestoreFoundByMarker(t *testing.T) {
	t.Parallel()
	fake := &trackertest.Fake{}
	d, id := findingsStore(t, fake, store.Finding{Key: "a", Title: "A"})
	ctx := t.Context()
	begin := findingsBegin{Since: time.Now(), Nonce: d.store.Nonce()}
	f, err := d.store.GetFinding(ctx, id, "a")
	require.NoError(t, err)
	_, err = d.createFindingIssue(ctx, id, f, nil, begin, true)
	require.NoError(t, err)
	_, err = d.store.SettleFinding(ctx, id, "a", store.FindingFailed, "", "timeout")
	require.NoError(t, err)

	out, err := d.reviewRestore(ctx, id, "a", begin)
	require.NoError(t, err)
	require.Equal(t, store.FindingRestored, out.State)
	items, err := fake.List(ctx, tracker.Query{})
	require.NoError(t, err)
	require.Len(t, items, 1)
	require.Empty(t, items[0].Labels)
}

// Plan Verification 5, the exit test: on holodeck with the fake tracker
// and the default regs.yaml, Command answers triage with {}. An order's
// script files five findings, two blocked by earlier keys and one by an
// existing issue. Once the order ends the fake holds five issues labelled
// bmus:unreviewed with those blockers, and a tracker_review hail is open;
// answering discard #2 #4 closes both as not planned and removes the
// label from the other three.
func TestFindingsReviewExit(t *testing.T) {
	t.Parallel()
	fake := &trackertest.Fake{}
	existing := fake.Add(tracker.Item{Title: "Compactor rework"})
	require.Equal(t, "#1", existing)
	steps := []map[string]any{
		findingStep(10, map[string]any{"key": "a-lock", "title": "Lock held across I/O", "body": "b"}),
		findingStep(20, map[string]any{"key": "b-leak", "title": "Handle leak", "body": "b", "blocked_by": []string{"a-lock"}}),
		findingStep(30, map[string]any{"key": "c-flake", "title": "Flaky snapshot test", "body": "b"}),
		findingStep(40, map[string]any{"key": "d-retry", "title": "No retry on EINTR", "body": "b", "blocked_by": []string{"c-flake"}}),
		findingStep(50, map[string]any{"key": "e-docs", "title": "Compactor docs stale", "body": "b", "blocked_by": []string{existing}}),
		{"at_pct": 100, "report": map[string]any{"summary": "done"}},
	}
	script, err := json.Marshal(map[string]any{"steps": steps,
		"consult": map[string]any{"decision": "answer", "answer": "{}", "rationale": "all new", "confidence": 0.9}})
	require.NoError(t, err)
	b := newHolodeckBridge(t, writeScript(t, string(script)))
	b.regs = defaultRegs(t)
	b.tracker = fake
	b.consultTimeout = time.Minute
	hd := b.start(t)
	hd.addCommand(t)
	hd.addCrew(t)
	id := hd.createOrder(t)
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		require.Len(c, workflows(c, hd.d, missionWorkflowID(id)), 1)
	}, 30*time.Second, 20*time.Millisecond)
	require.Equal(t, MissionDone, missionResult(t, hd.d, id).Status)
	h := waitReview(t, hd.d, id)

	want := map[string][]string{"#2": {}, "#3": {"#2"}, "#4": {}, "#5": {"#4"}, "#6": {existing}}
	for ref, blockers := range want {
		it, err := fake.Get(t.Context(), ref)
		require.NoError(t, err)
		require.Equal(t, []string{labelUnreviewed}, it.Labels, ref)
		require.Equal(t, blockers, it.BlockedBy, ref)
	}
	all, err := fake.List(t.Context(), tracker.Query{State: tracker.StateAll})
	require.NoError(t, err)
	require.Len(t, all, 6, "the existing issue and the five filed")
	require.Equal(t, store.HailOpen, h.Status)
	require.Equal(t, reviewOptions, h.Options)
	require.Equal(t, "Review 5 issues filed from "+id+"'s findings (0 dropped, 0 failed): keep, discard, "+
		"or answer with the refs to discard and the keys to restore.", h.Question)
	var rc ReviewContext
	require.NoError(t, json.Unmarshal(h.Context, &rc))
	require.Len(t, rc.Issues, 5)
	require.Equal(t, ReviewIssue{Ref: "#3", Key: "b-leak", Title: "Handle leak"}, rc.Issues[1])
	require.Empty(t, rc.Dropped)
	require.Empty(t, rc.Failed)
	r, err := hd.d.classifyHail(t.Context(), h.ID)
	require.NoError(t, err)
	require.Zero(t, r.RemindEvery, "no reminders")
	require.Zero(t, r.Timeout, "no captain timeout")
	requirePending(t, hd.d, findingsWorkflowID(id))

	require.NoError(t, hd.d.AnswerHail(t.Context(), h.ID, "discard #2 #4", nil))
	require.Equal(t, FindingsResult{Filed: 5, Kept: 3, Discarded: 2}, endFindings(t, hd.d, id, ""))
	all, err = fake.List(t.Context(), tracker.Query{State: tracker.StateAll})
	require.NoError(t, err)
	require.Len(t, all, 6, "the review filed nothing")
	decided := hd.hail(t, h.ID)
	require.Equal(t, store.HailDecided, decided.Status)
	require.Equal(t, "discard #2 #4", decided.Decision)
	for _, ref := range []string{"#2", "#4"} {
		require.Equal(t, tracker.ReasonNotPlanned, fake.CloseReason(ref), ref)
	}
	for _, ref := range []string{"#3", "#5", "#6"} {
		it, err := fake.Get(t.Context(), ref)
		require.NoError(t, err)
		require.Empty(t, it.Labels, ref)
		require.Equal(t, tracker.StateOpen, it.State, ref)
	}
	rows, err := hd.d.store.OrderFindings(t.Context(), id)
	require.NoError(t, err)
	states := map[string]string{}
	for _, f := range rows {
		states[f.Key] = f.State
	}
	require.Equal(t, map[string]string{"a-lock": store.FindingDiscarded, "b-leak": store.FindingKept,
		"c-flake": store.FindingDiscarded, "d-retry": store.FindingKept, "e-docs": store.FindingKept}, states)
	texts := orderTexts(hd.logEntries(t), id, "review of")
	require.Equal(t, []string{"review of " + id + "'s findings: kept 3, discarded 2, restored 0"}, texts)
	e := hd.waitLog(t, "hail "+h.ID+" decided")
	require.Contains(t, string(e.Payload), `"option":1`)
}

// Plan Verification 6, restore: Command drops one finding; the review
// lists it with Command's reason, and restoring it files it without
// bmus:unreviewed while the issue filed is kept.
func TestFindingsReviewRestore(t *testing.T) {
	t.Parallel()
	steps := []map[string]any{
		findingStep(10, map[string]any{"key": "a", "title": "Keep me", "body": "b"}),
		findingStep(20, map[string]any{"key": "b", "title": "Noise?", "body": "b", "labels": []string{"docs"},
			"blocked_by": []string{"a"}}),
		{"at_pct": 100, "report": map[string]any{"summary": "done"}},
	}
	script, err := json.Marshal(map[string]any{"steps": steps,
		"consult": map[string]any{"decision": "answer", "answer": `{"drop":[2]}`, "rationale": "2 is noise", "confidence": 0.9}})
	require.NoError(t, err)
	fake := &trackertest.Fake{}
	b := newHolodeckBridge(t, writeScript(t, string(script)))
	b.regs = defaultRegs(t)
	b.tracker = fake
	b.consultTimeout = time.Minute
	hd := b.start(t)
	hd.addCommand(t)
	hd.addCrew(t)
	id := hd.createOrder(t)
	h := waitReview(t, hd.d, id)
	var rc ReviewContext
	require.NoError(t, json.Unmarshal(h.Context, &rc))
	require.Equal(t, []ReviewDropped{{Key: "b", Title: "Noise?", Why: "2 is noise"}}, rc.Dropped)

	require.Equal(t, FindingsResult{Filed: 1, Dropped: 1, Kept: 1, Restored: 1}, endFindings(t, hd.d, id, "restore b"))
	require.Equal(t, "keep; restore b", hd.hail(t, h.ID).Decision)
	restored, err := fake.Get(t.Context(), "#2")
	require.NoError(t, err)
	require.Equal(t, "Noise?", restored.Title)
	require.Equal(t, []string{"docs"}, restored.Labels)
	require.Equal(t, []string{"#1"}, restored.BlockedBy)
	kept, err := fake.Get(t.Context(), "#1")
	require.NoError(t, err)
	require.Empty(t, kept.Labels)
	row, err := hd.d.store.GetFinding(t.Context(), id, "b")
	require.NoError(t, err)
	require.Equal(t, store.FindingRestored, row.State)
	require.Equal(t, "#2", row.Ref)
	texts := orderTexts(hd.logEntries(t), id, "")
	require.Contains(t, texts, "finding b restored as #2")
	require.Contains(t, texts, "review of "+id+"'s findings: kept 1, discarded 0, restored 1")
}

// Plan Verification 7: a bridge restarted while the review is open picks
// the wait up again, and the answer is acted on once.
func TestFindingsReviewSurvivesRestart(t *testing.T) {
	t.Parallel()
	script, err := json.Marshal([]map[string]any{
		findingStep(10, map[string]any{"key": "a", "title": "A", "body": "b"}),
		{"at_pct": 100, "report": map[string]any{"summary": "done"}},
	})
	require.NoError(t, err)
	fake := &trackertest.Fake{}
	b := newHolodeckBridge(t, writeScript(t, string(script)))
	b.tracker = fake
	hd := b.start(t)
	hd.addCrew(t)
	id := hd.createOrder(t)
	h := waitReview(t, hd.d, id)

	hd.stop(t)
	hd = b.start(t)
	requirePending(t, hd.d, findingsWorkflowID(id))
	one := 1
	require.NoError(t, hd.d.AnswerHail(t.Context(), h.ID, "", &one))
	require.Equal(t, FindingsResult{Filed: 1, Discarded: 1}, endFindings(t, hd.d, id, ""))
	require.Equal(t, "discard", hd.hail(t, h.ID).Decision)
	require.Equal(t, tracker.ReasonNotPlanned, fake.CloseReason("#1"))
	n := 0
	for _, s := range stepNames(t, hd.d, findingsWorkflowID(id)) {
		if s == "review-discard" {
			n++
		}
	}
	require.Equal(t, 1, n, "acted on once")
	require.Equal(t, []string{"review of " + id + "'s findings: kept 0, discarded 1, restored 0"},
		orderTexts(hd.logEntries(t), id, "review of"))
	require.Len(t, orderTexts(hd.logEntries(t), id, "the captain asked to review"), 1, "raised once")
}

// closeFails is a fake tracker whose Close fails.
type closeFails struct{ trackertest.Fake }

func (c *closeFails) Close(context.Context, string, string) error { return errors.New("close refused") }

// An action out of retries is logged on the order, and the rest go on.
func TestFindingsReviewActionFails(t *testing.T) {
	t.Parallel()
	fake := &closeFails{}
	d, id := findingsDaemon(t, fake, nil, store.Finding{Key: "a", Title: "A"}, store.Finding{Key: "b", Title: "B"})
	startFindingsWorkflow(t, d, id)
	require.Equal(t, FindingsResult{Filed: 2, Kept: 1, ReviewFailed: 1}, endFindings(t, d, id, "discard #1"))
	a, err := d.store.GetFinding(t.Context(), id, "a")
	require.NoError(t, err)
	require.Equal(t, store.FindingFiled, a.State, "not discarded")
	texts := orderLogTexts(t, d, id)
	require.Contains(t, texts, "finding a not discarded: close refused")
	require.Equal(t, "review of "+id+"'s findings: kept 1, discarded 0, restored 0, failed 1", texts[len(texts)-1])
	it, err := fake.Get(t.Context(), "#2")
	require.NoError(t, err)
	require.Empty(t, it.Labels)
}

// A findings workflow that filed nothing, dropped nothing and failed
// nothing raises no review: here, with no tracker.
func TestNoReviewWithoutFiling(t *testing.T) {
	t.Parallel()
	d, id := findingsDaemon(t, nil, nil, store.Finding{Key: "a", Title: "A"})
	require.Equal(t, FindingsResult{NotFiled: "no tracker configured"}, runFindings(t, d, id))
	hails, err := d.store.ListHails(t.Context(), "")
	require.NoError(t, err)
	require.Empty(t, hails)
	require.NotContains(t, stepNames(t, d, findingsWorkflowID(id)), "raise-review")
}

// An issue gone before the review: discarding it counts as done, with a
// note; keeping it fails for good at once.
func TestReviewIssueGone(t *testing.T) {
	t.Parallel()
	fake := &trackertest.Fake{}
	d, id := findingsStore(t, fake, store.Finding{Key: "a", Title: "A"}, store.Finding{Key: "b", Title: "B"})
	ctx := t.Context()
	for _, k := range []string{"a", "b"} {
		_, err := d.store.SettleFinding(ctx, id, k, store.FindingFiled, "#9", "")
		require.NoError(t, err)
	}
	out, err := d.reviewDiscard(ctx, id, "a")
	require.NoError(t, err)
	require.Equal(t, reviewOutcome{State: store.FindingDiscarded}, out)
	a, err := d.store.GetFinding(ctx, id, "a")
	require.NoError(t, err)
	require.Equal(t, "item #9: tracker: not found", a.Why)
	require.Contains(t, orderLogTexts(t, d, id), "finding a discarded: item #9: tracker: not found")

	out, err = d.reviewKeep(ctx, id, "b")
	require.NoError(t, err)
	require.Equal(t, reviewOutcome{Failed: "item #9: tracker: not found"}, out)
	b, err := d.store.GetFinding(ctx, id, "b")
	require.NoError(t, err)
	require.Equal(t, store.FindingFiled, b.State)
}

// unlabelGone is a fake tracker whose Unlabel finds no issue, counting
// its calls.
type unlabelGone struct {
	trackertest.Fake
	mu sync.Mutex
	n  int
}

func (u *unlabelGone) Unlabel(context.Context, string, string) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.n++
	return fmt.Errorf("item gone: %w", tracker.ErrNotFound)
}

// Through DBOS: a keep whose issue is gone runs once, with no retries,
// and is logged once and counted as failed.
func TestFindingsReviewKeepGone(t *testing.T) {
	t.Parallel()
	fake := &unlabelGone{}
	d, id := findingsDaemon(t, fake, nil, store.Finding{Key: "a", Title: "A"})
	startFindingsWorkflow(t, d, id)
	require.Equal(t, FindingsResult{Filed: 1, ReviewFailed: 1}, endFindings(t, d, id, "keep"))
	fake.mu.Lock()
	require.Equal(t, 1, fake.n, "no retries")
	fake.mu.Unlock()
	texts := orderLogTexts(t, d, id)
	require.Contains(t, texts, "finding a not kept: item gone: tracker: not found")
	require.Equal(t, "review of "+id+"'s findings: kept 0, discarded 0, restored 0, failed 1", texts[len(texts)-1])
}

// A restore whose link keeps failing: the issue is filed and found under
// its marker, losing bmus:unreviewed before the link error returns; its
// ref is stored, and review-failed restores the finding with the error
// noted.
func TestReviewRestoreLinkFails(t *testing.T) {
	t.Parallel()
	fake := &linkFails{}
	existing := fake.Add(tracker.Item{Title: "existing"})
	d, id := findingsStore(t, fake, store.Finding{Key: "b", Title: "B", BlockedBy: []string{existing}})
	ctx := t.Context()
	begin := findingsBegin{Since: time.Now(), Nonce: d.store.Nonce()}
	f, err := d.store.GetFinding(ctx, id, "b")
	require.NoError(t, err)
	got, err := d.createFindingIssue(ctx, id, f, []string{existing}, begin, true)
	require.Error(t, err, "an earlier attempt filed it with the label, short of the link")
	require.Equal(t, "#2", got.Ref)
	_, err = d.store.SettleFinding(ctx, id, "b", store.FindingFailed, "", "link failed")
	require.NoError(t, err)

	_, err = d.reviewRestore(ctx, id, "b", begin)
	require.ErrorContains(t, err, "linking #2 to #1 failed")
	it, err := fake.Get(ctx, "#2")
	require.NoError(t, err)
	require.Empty(t, it.Labels, "unlabelled before the link error")
	row, err := d.store.GetFinding(ctx, id, "b")
	require.NoError(t, err)
	require.Equal(t, store.FindingFailed, row.State)
	require.Equal(t, "#2", row.Ref, "the ref is stored")

	out, err := d.reviewFailed(ctx, id, "H-0002", "b", "review-restore", "linking #2 to #1 failed")
	require.NoError(t, err)
	require.Equal(t, reviewOutcome{State: store.FindingRestored}, out)
	row, err = d.store.GetFinding(ctx, id, "b")
	require.NoError(t, err)
	require.Equal(t, "linking #2 to #1 failed", row.Why)
	require.Contains(t, orderLogTexts(t, d, id), "finding b restored as #2 (linking #2 to #1 failed)")
	items, err := fake.List(ctx, tracker.Query{State: tracker.StateAll})
	require.NoError(t, err)
	require.Len(t, items, 2, "one issue")

	// Without a stored ref, review-failed logs the failure once.
	d2, id2 := findingsStore(t, &trackertest.Fake{}, store.Finding{Key: "c", Title: "C"})
	_, err = d2.store.SettleFinding(ctx, id2, "c", store.FindingFailed, "", "down")
	require.NoError(t, err)
	for range 2 {
		out, err = d2.reviewFailed(ctx, id2, "H-0002", "c", "review-restore", "down again")
		require.NoError(t, err)
		require.Empty(t, out.State)
	}
	n := 0
	for _, s := range orderLogTexts(t, d2, id2) {
		if s == "finding c not restored: down again" {
			n++
		}
	}
	require.Equal(t, 1, n)
}
