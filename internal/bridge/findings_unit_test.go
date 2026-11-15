package bridge

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dbos-inc/dbos-transact-golang/dbos"
	"github.com/stretchr/testify/require"

	"github.com/tscolari/bmus/internal/regs"
	"github.com/tscolari/bmus/internal/store"
	"github.com/tscolari/bmus/internal/tracker"
	"github.com/tscolari/bmus/internal/tracker/trackertest"
)

// #153's review and its probes: the agent's body is filed as a text code
// block, whatever it holds. Only the closing fence closes it, once, at the
// end, and the bridge's line follows outside it.
func TestFindingBody(t *testing.T) {
	t.Parallel()
	fenceLine := regexp.MustCompile("^ {0,3}(`{3,}|~{3,})[ \t]*$")
	for _, probe := range []string{
		"_@octocat_",
		"-@octocat",
		"<div>\n@octocat\n</div>",
		"<details>\n```\n</details>\n@octocat\n```\nother/repo#1",
		"# `x\n@octocat`",
		"- `a\n- @octocat`",
		"&#64;octocat",
		"other&#47;repo#1",
		"https://github.com/other/repo/issues/1",
		"[x](https://github.com/other/repo/issues/1)",
		`<a href="https://github.com/other/repo/issues/1">x</a>`,
		"~~~\n@octocat",
		"````\n@octocat",
		"``````\n@octocat\n``````",
		"\n\nlead and trail\n\n",
		"`@octocat` and o/r#1",
	} {
		got := findingBody(store.Finding{CrewID: "sorell", Body: probe, Severity: "high"}, "ORD-0001")
		fence := strings.Repeat("`", max(3, longestRun(probe, '`')+1))
		require.Equal(t, fence+"text\n"+strings.Trim(probe, "\n")+"\n"+fence+"\n\nFiled by sorell from ORD-0001 (severity high).", got, probe)
		lines := strings.Split(got, "\n")
		require.Equal(t, fence+"text", lines[0])
		closing := -1
		for i, l := range lines[1:] {
			if m := fenceLine.FindStringSubmatch(l); m != nil && m[1][0] == '`' && len(m[1]) >= len(fence) {
				require.Equal(t, -1, closing, "closed once: %q", probe)
				closing = i + 1
			}
		}
		require.Equal(t, len(lines)-3, closing, "closed at the end: %q", probe)
	}
	require.Equal(t, "Filed by sorell from ORD-0001.", findingBody(store.Finding{CrewID: "sorell", Body: " \n"}, "ORD-0001"))
}

// Titles: a zero-width space breaks every @-mention and every other
// repository's ref; the tracker's own refs stay.
func TestFindingTitle(t *testing.T) {
	t.Parallel()
	own := func(ref string) bool { return strings.HasPrefix(ref, "acme/app#") }
	require.Equal(t, "Ask @\u200balice and @\u200borg/team about acme/app#1, other/app\u200b#2 and #3",
		findingTitle("Ask @alice and @org/team about acme/app#1, other/app#2 and #3", own))
	require.Equal(t, "Plain title", findingTitle("Plain title", own))
}

// Command's answer is parsed strictly against the batch and the backlog
// offered; anything else is refused with why.
func TestParseTriage(t *testing.T) {
	t.Parallel()
	keys := []string{"a", "b", "c", "d"}
	offered := []string{"#1", "#2"}
	f := &trackertest.Fake{}
	got, err := parseTriage(`{"drop":[1],"duplicates":{"2":" #1"},"blocked_by":{"3":["#2","f4","f4"]}}`, keys, offered, f.Canon)
	require.NoError(t, err)
	require.Equal(t, []store.FindingTriage{
		{Key: "a", Drop: true},
		{Key: "b", Drop: true, DuplicateOf: "#1"},
		{Key: "c", AddBlockedBy: []string{"#2", "d"}},
	}, got)
	got, err = parseTriage("```json\n{}\n```", keys, offered, f.Canon)
	require.NoError(t, err)
	require.Empty(t, got)

	for _, tc := range []struct{ answer, err string }{
		{`file them all`, "not a JSON object"},
		{`null`, "not a JSON object"},
		{`{"drop":[5]}`, `drop: "5" is not a finding number from 1 to 4`},
		{`{"drop":[0]}`, `drop: "0" is not a finding number`},
		{`{"drop":[1,1]}`, "drop: 1 named twice"},
		{`{"drop":[1],"duplicates":{"1":"#1"}}`, "duplicates: 1 is dropped already"},
		{`{"duplicates":{"01":"#1"}}`, `duplicates: "01" is not a finding number`},
		{`{"duplicates":{"2":"#3"}}`, `duplicates: "#3" is not a ref in the backlog offered`},
		{`{"duplicates":{"2":"o/r#1"}}`, `duplicates: "o/r#1" is not a ref in the backlog offered`},
		{`{"blocked_by":{"3":["f3"]}}`, "blocked_by: f3 blocks itself"},
		{`{"blocked_by":{"3":["f9"]}}`, `blocked_by: "9" is not a finding number`},
		{`{"blocked_by":{"3":["d"]}}`, `blocked_by: "d" is not a ref in the backlog offered`},
		{`{"drop":[1],"keep":[2]}`, `unknown field "keep"`},
		{`{} {}`, "more than one JSON value"},
	} {
		_, err := parseTriage(tc.answer, keys, offered, f.Canon)
		require.ErrorContains(t, err, tc.err, tc.answer)
	}
}

// The triage context numbers the findings from 1 with bodies cut to 2 KiB,
// then holds backlog entries while it stays within 32 KiB, counting the
// rest; findings that alone outgrow it get shorter bodies.
func TestTriageContext(t *testing.T) {
	t.Parallel()
	fs := []store.Finding{
		{Key: "a", Title: "A", Body: strings.Repeat("x", 3000), Labels: []string{"bug"}, Severity: "high", BlockedBy: []string{"b"}},
		{Key: "b", Title: "B", Body: "short"},
	}
	var backlog []backlogItem
	for i := range maxBacklog {
		backlog = append(backlog, backlogItem{Ref: "#" + strings.Repeat("1", 1+i%3), Title: strings.Repeat("t", 200)})
	}
	raw, n, err := triageContext("ORD-0001", "Fix the flake", fs, backlog)
	require.NoError(t, err)
	require.LessOrEqual(t, len(raw), maxConsultContext)
	var c triageHailContext
	require.NoError(t, json.Unmarshal(raw, &c))
	require.Equal(t, "ORD-0001", c.Order)
	require.Equal(t, "Fix the flake", c.Title)
	require.Len(t, c.Findings, 2)
	require.Equal(t, 1, c.Findings[0].N)
	require.Equal(t, 2, c.Findings[1].N)
	require.Len(t, c.Findings[0].Body, maxTriageBody)
	require.True(t, strings.HasSuffix(c.Findings[0].Body, truncated))
	require.Equal(t, []string{"b"}, c.Findings[0].BlockedBy)
	require.Equal(t, n, len(c.Backlog))
	require.Greater(t, n, 100)
	require.Less(t, n, maxBacklog)
	require.Equal(t, maxBacklog-n, c.BacklogOmitted)

	// Twenty findings of 2 KiB each don't fit: their bodies shrink.
	var many []store.Finding
	for i := range store.MaxFindings {
		many = append(many, store.Finding{Key: "k" + strings.Repeat("x", i), Title: "t", Body: strings.Repeat("y", 4000)})
	}
	raw, n, err = triageContext("ORD-0001", "t", many, backlog)
	require.NoError(t, err)
	require.LessOrEqual(t, len(raw), maxConsultContext)
	require.NoError(t, json.Unmarshal(raw, &c))
	require.Len(t, c.Findings, store.MaxFindings)
	require.Len(t, c.Findings[0].Body, maxTriageBody/2)
	require.Equal(t, maxBacklog-n, c.BacklogOmitted)

	require.Equal(t, "Triage 1 finding from ORD-0001 against the open backlog in the context. Answer with a JSON object of "+
		"finding numbers and refs: drop (numbers that are noise), duplicates (a number to the ref it repeats), blocked_by "+
		"(a number to refs or f<number>). {} files them all.", triageQuestion("ORD-0001", 1))
}

// findingsStore is a store holding order ORD-0001 with findings fs, and a
// Daemon on it with tracker tr.
func findingsStore(t *testing.T, tr tracker.Tracker, fs ...store.Finding) (*Daemon, string) {
	d, id, _ := findingsStorePath(t, tr, fs...)
	return d, id
}

// findingsStorePath is findingsStore, with the database's path.
func findingsStorePath(t *testing.T, tr tracker.Tracker, fs ...store.Finding) (*Daemon, string, string) {
	t.Helper()
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), store.DBFile)
	st, err := store.Open(ctx, path)
	require.NoError(t, err)
	t.Cleanup(func() { st.Close() })
	id, err := st.InsertOrder(ctx, store.Order{Title: "Fix the flake"}, store.LogEntry{Kind: logOrder, Text: "created"})
	require.NoError(t, err)
	insertFindingRows(t, path, id, fs...)
	return &Daemon{store: st, tracker: tr, logger: slog.New(slog.DiscardHandler)}, id, path
}

// insertFindingRows writes findings fs of order id into the database at
// path, new, as the pump records them.
func insertFindingRows(t *testing.T, path, id string, fs ...store.Finding) {
	t.Helper()
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)")
	require.NoError(t, err)
	defer db.Close()
	for _, f := range fs {
		labels, err := json.Marshal(append([]string{}, f.Labels...))
		require.NoError(t, err)
		blocked, err := json.Marshal(append([]string{}, f.BlockedBy...))
		require.NoError(t, err)
		_, err = db.ExecContext(t.Context(), `INSERT INTO findings (order_id, key, crew_id, title, body, labels, severity, blocked_by, received_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, id, f.Key, "sorell", f.Title, f.Body, string(labels), f.Severity, string(blocked), time.Now().UTC())
		require.NoError(t, err)
	}
}

// orderLogTexts are the texts of order id's log entries.
func orderLogTexts(t *testing.T, d *Daemon, id string) []string {
	t.Helper()
	entries, err := d.store.ListLog(t.Context(), 0)
	require.NoError(t, err)
	var out []string
	for _, e := range entries {
		if e.OrderID == id {
			out = append(out, e.Text)
		}
	}
	return out
}

// plan-filing: a ref Canon refuses (another repository's) is dropped with
// a note, as are a key the order lacks and a finding's own key; the
// cycle's closing edge is dropped and logged; blockers come first.
func TestPlanFiling(t *testing.T) {
	t.Parallel()
	d, id := findingsStore(t, &trackertest.Fake{})
	fs := []store.Finding{
		{Key: "a", BlockedBy: []string{"c"}},
		{Key: "b", BlockedBy: []string{"a", "other/repo#9", " #4", "#4", "nope", "b"}},
		{Key: "c", BlockedBy: []string{"b"}},
		{Key: "d", BlockedBy: []string{"x"}, State: store.FindingDropped},
		{Key: "e", BlockedBy: []string{"d"}},
	}
	plan, err := d.planFiling(t.Context(), id, fs)
	require.NoError(t, err)
	require.Equal(t, filingPlan{
		Order: []string{"b", "c", "a", "e"},
		Blockers: map[string][]string{
			"a": {"c"},
			"b": {"#4"},
			"c": {"b"},
			"e": {"d"},
		},
	}, plan)
	require.Equal(t, []string{
		"created",
		`finding b: blocker other/repo#9 dropped: tracker: bad ref: "other/repo#9" is not #<n>`,
		"finding b: blocker nope dropped: it names no finding of " + id,
		"finding b: blocker b dropped: a finding can't block itself",
		"finding b: blocker a dropped: it closes a cycle",
	}, orderLogTexts(t, d, id))
}

// A file-finding re-run files one issue: after the step settled (the row
// is filed), and after a crash between the tracker's answer and the
// store (the marker finds the issue).
func TestFileFindingReplay(t *testing.T) {
	t.Parallel()
	fake := &trackertest.Fake{}
	blocker := fake.Add(tracker.Item{Title: "existing"})
	d, id, path := findingsStorePath(t, fake, store.Finding{Key: "leak", Title: "Leak", Body: "Ask @alice.", Labels: []string{"bug"},
		Severity: "high"})
	b := findingsBegin{Since: time.Now(), Nonce: d.store.Nonce()}
	ctx := t.Context()
	want := filedFinding{State: store.FindingFiled, Ref: "#2"}
	for range 2 {
		got, err := d.fileFinding(ctx, id, "leak", []string{blocker}, b)
		require.NoError(t, err)
		require.Equal(t, want, got)
	}
	it, err := fake.Get(ctx, "#2")
	require.NoError(t, err)
	require.Equal(t, tracker.Item{Ref: "#2", Title: "Leak", Body: "```text\nAsk @alice.\n```\n\nFiled by sorell from " + id + " (severity high).",
		Labels: []string{"bug", labelUnreviewed}, State: tracker.StateOpen, BlockedBy: []string{blocker}}, it)

	// The crash: the row is still new, and the issue exists.
	resetFinding(t, path, id, "leak")
	got, err := d.fileFinding(ctx, id, "leak", []string{blocker}, b)
	require.NoError(t, err)
	require.Equal(t, want, got)
	items, err := fake.List(ctx, tracker.Query{State: tracker.StateAll})
	require.NoError(t, err)
	require.Len(t, items, 2, "one issue filed")
	f, err := d.store.GetFinding(ctx, id, "leak")
	require.NoError(t, err)
	require.Equal(t, store.FindingFiled, f.State)
	require.Equal(t, "#2", f.Ref)
	texts := orderLogTexts(t, d, id)
	require.Equal(t, "finding leak filed as #2", texts[len(texts)-1])
}

// resetFinding makes finding key of order id new again, with no ref: a
// crash left the step unrecorded and the store unwritten.
func resetFinding(t *testing.T, path, id, key string) {
	t.Helper()
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)")
	require.NoError(t, err)
	defer db.Close()
	_, err = db.ExecContext(t.Context(), `UPDATE findings SET state = 'new', ref = '' WHERE order_id = ? AND key = ?`, id, key)
	require.NoError(t, err)
}

// A ref Create returns with a link error is stored and the step fails; its
// retry adds only the missing link, and files no second issue.
func TestFileFindingLinkError(t *testing.T) {
	t.Parallel()
	fake := &trackertest.Fake{}
	a, b := fake.Add(tracker.Item{Title: "a"}), fake.Add(tracker.Item{Title: "b"})
	fake.FailLinks = 1
	d, id := findingsStore(t, fake, store.Finding{Key: "leak", Title: "Leak"})
	begin := findingsBegin{Since: time.Now(), Nonce: d.store.Nonce()}
	ctx := t.Context()
	_, err := d.fileFinding(ctx, id, "leak", []string{a, b}, begin)
	require.ErrorContains(t, err, "linking blocker #2 failed")
	f, err := d.store.GetFinding(ctx, id, "leak")
	require.NoError(t, err)
	require.Equal(t, store.FindingNew, f.State)
	require.Equal(t, "#3", f.Ref)

	got, err := d.fileFinding(ctx, id, "leak", []string{a, b}, begin)
	require.NoError(t, err)
	require.Equal(t, filedFinding{State: store.FindingFiled, Ref: "#3"}, got)
	it, err := fake.Get(ctx, "#3")
	require.NoError(t, err)
	require.Equal(t, []string{a, b}, it.BlockedBy, "each link once")
	items, err := fake.List(ctx, tracker.Query{})
	require.NoError(t, err)
	require.Len(t, items, 3)
}

// A tracker that drops the labels (ErrLabels) leaves the issue filed, with
// a note and no retry.
func TestFileFindingLabelsDropped(t *testing.T) {
	t.Parallel()
	fake := &trackertest.Fake{DropLabels: true}
	d, id := findingsStore(t, fake, store.Finding{Key: "leak", Title: "Leak"})
	got, err := d.fileFinding(t.Context(), id, "leak", nil, findingsBegin{Since: time.Now(), Nonce: d.store.Nonce()})
	require.NoError(t, err)
	require.Equal(t, filedFinding{State: store.FindingFiled, Ref: "#1"}, got)
	f, err := d.store.GetFinding(t.Context(), id, "leak")
	require.NoError(t, err)
	require.Equal(t, "#1 created without the labels bmus:unreviewed: tracker: labels missing", f.Why)
	texts := orderLogTexts(t, d, id)
	require.Equal(t, "finding leak filed as #1 (#1 created without the labels bmus:unreviewed: tracker: labels missing)", texts[len(texts)-1])
}

// stepError reads the last attempt's error out of DBOS's.
func TestStepError(t *testing.T) {
	t.Parallel()
	require.Equal(t, "boom 2", stepError(errors.Join(errors.New("boom 1"), errors.New("boom 2"))))
	require.Equal(t, "boom 2", stepError(errors.New("Step file-finding has exceeded its maximum of 5 retries: boom 1\nboom 2")))
	require.Equal(t, "plain", stepError(errors.New("plain")))
}

// findingsDaemon runs a daemon on fake beams with tracker tr and regs
// cfg (nil: none), holding order ORD-0001 with findings fs. Tracker calls
// retry fast.
func findingsDaemon(t *testing.T, tr tracker.Tracker, cfg *regs.Config, fs ...store.Finding) (*Daemon, string) {
	t.Helper()
	home := privateHome(t)
	id := insertOrder(t, home, store.Order{Title: "Fix the flake"})
	insertFindingRows(t, filepath.Join(home, store.DBFile), id, fs...)
	c := testConfig(home)
	c.Tracker = tr
	if cfg != nil {
		c.Regs, c.RegsConfigured = *cfg, true
	}
	c.trackerRetries = []dbos.StepOption{dbos.WithStepMaxRetries(5), dbos.WithStepBaseInterval(10 * time.Millisecond),
		dbos.WithStepMaxInterval(50 * time.Millisecond)}
	r := startDaemonConfig(t, c)
	t.Cleanup(func() { r.stop(t) })
	return r.d, id
}

// runFindings runs order id's findings workflow and returns its result,
// answering keep to its review once the review is open.
func runFindings(t *testing.T, d *Daemon, id string) FindingsResult {
	t.Helper()
	_, err := dbos.RunWorkflow(d.dbos, findingsWorkflow, FindingsInput{OrderID: id}, dbos.WithWorkflowID(findingsWorkflowID(id)))
	require.NoError(t, err)
	return endFindings(t, d, id, reviewKeep)
}

// startFindingsWorkflow runs order id's findings workflow without waiting
// for it.
func startFindingsWorkflow(t *testing.T, d *Daemon, id string) {
	t.Helper()
	_, err := dbos.RunWorkflow(d.dbos, findingsWorkflow, FindingsInput{OrderID: id}, dbos.WithWorkflowID(findingsWorkflowID(id)))
	require.NoError(t, err)
}

// Plan Verification 6, skipped: with Command not on deck, and with no
// regs.yaml, skip-advisory closes the triage hail and every finding is
// filed, blockers first, labelled bmus:unreviewed, with the reason noted.
func TestFindingsTriageSkipped(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		regs func(*testing.T) *regs.Config
		why  string
	}{
		{"Command not on deck", defaultRegs, whyCommandNotOnDeck},
		{"no regs.yaml", nil, infoRegsNotConfigured},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fake := &trackertest.Fake{}
			existing := fake.Add(tracker.Item{Title: "existing"})
			var cfg *regs.Config
			if tc.regs != nil {
				cfg = tc.regs(t)
			}
			d, id := findingsDaemon(t, fake, cfg,
				store.Finding{Key: "a-leak", Title: "Leak", Body: "Seen by @alice in other/repo#4.", BlockedBy: []string{"b-flake", existing}},
				store.Finding{Key: "b-flake", Title: "Flake", Labels: []string{"test"}, Severity: "low"},
				store.Finding{Key: "c-foreign", Title: "Foreign", BlockedBy: []string{"other/repo#9"}})
			startFindingsWorkflow(t, d, id)
			waitReview(t, d, id)

			items, err := fake.List(t.Context(), tracker.Query{})
			require.NoError(t, err)
			require.Len(t, items, 4)
			byTitle := map[string]tracker.Item{}
			for _, it := range items {
				byTitle[it.Title] = it
			}
			flake, leak, foreign := byTitle["Flake"], byTitle["Leak"], byTitle["Foreign"]
			require.Equal(t, "#2", flake.Ref, "the blocker is filed first")
			require.Equal(t, []string{"test", labelUnreviewed}, flake.Labels)
			require.Equal(t, "Filed by sorell from "+id+" (severity low).", flake.Body)
			require.Equal(t, []string{flake.Ref, existing}, leak.BlockedBy)
			require.Equal(t, "```text\nSeen by @alice in other/repo#4.\n```\n\nFiled by sorell from "+id+".", leak.Body)
			require.Empty(t, foreign.BlockedBy, "another repository's ref is dropped")

			got, err := d.store.OrderFindings(t.Context(), id)
			require.NoError(t, err)
			for _, f := range got {
				require.Equal(t, store.FindingFiled, f.State, f.Key)
				require.Equal(t, byTitle[f.Title].Ref, f.Ref)
			}
			h, err := d.store.GetHail(t.Context(), "H-0001")
			require.NoError(t, err)
			require.Equal(t, store.HailTimedOut, h.Status)
			require.Equal(t, findingsWorkflowID(id), h.DeliverTo)
			require.Equal(t, tc.why, h.Rationale)

			texts := orderLogTexts(t, d, id)
			require.Contains(t, texts, "Command asked to triage 3 findings of "+id)
			require.Contains(t, texts, "findings of "+id+" filed without triage: "+tc.why)
			require.Contains(t, texts, `finding c-foreign: blocker other/repo#9 dropped: tracker: bad ref: "other/repo#9" is not #<n>`)
			require.Contains(t, texts, "finding b-flake filed as #2")
			require.Equal(t, "findings of "+id+": filed 3, dropped 0, failed 0", texts[len(texts)-2])
			require.Equal(t, "the captain asked to review 3 issues filed from "+id+"'s findings", texts[len(texts)-1])
			require.Equal(t, FindingsResult{Filed: 3, Kept: 3}, endFindings(t, d, id, reviewKeep))
		})
	}
}

// A tracker that fails every call: the backlog can't be read, so triage is
// skipped with that reason, and each finding fails after its retries with
// the error, filing going on to the next.
func TestFindingsTrackerDown(t *testing.T) {
	t.Parallel()
	fake := &creates{Fake: trackertest.Fake{Err: errors.New("tracker down")}}
	d, id := findingsDaemon(t, fake, defaultRegs(t),
		store.Finding{Key: "a", Title: "A"}, store.Finding{Key: "b", Title: "B", BlockedBy: []string{"a"}})
	startFindingsWorkflow(t, d, id)
	waitReview(t, d, id)
	require.Equal(t, 2*(1+retriesBacklogDown), fake.count(), "fewer retries once the backlog was unreadable")
	got, err := d.store.OrderFindings(t.Context(), id)
	require.NoError(t, err)
	for _, f := range got {
		require.Equal(t, store.FindingFailed, f.State)
		require.Equal(t, "tracker down", f.Why)
	}
	texts := orderLogTexts(t, d, id)
	require.Contains(t, texts, "findings of "+id+" filed without triage: the backlog could not be read: tracker down")
	require.Contains(t, texts, "finding a not filed: tracker down")
	require.Equal(t, "findings of "+id+": filed 0, dropped 0, failed 2", texts[len(texts)-2])
	h, err := d.store.GetHail(t.Context(), "H-0001")
	require.NoError(t, err)
	require.Equal(t, regs.CategoryTrackerReview, h.Category, "no triage hail; the review lists the failed")
	require.Equal(t, FindingsResult{Failed: 2}, endFindings(t, d, id, reviewKeep))
}

// Without a tracker nothing is filed: the rows stay new, and one entry
// says why.
func TestFindingsNoTracker(t *testing.T) {
	t.Parallel()
	d, id := findingsDaemon(t, nil, nil, store.Finding{Key: "a", Title: "A"})
	require.Equal(t, FindingsResult{NotFiled: "no tracker configured"}, runFindings(t, d, id))
	got, err := d.store.OrderFindings(t.Context(), id)
	require.NoError(t, err)
	require.Equal(t, store.FindingNew, got[0].State)
	texts := orderLogTexts(t, d, id)
	require.Equal(t, "findings of "+id+" not filed: no tracker configured", texts[len(texts)-1])
}

// Plan Verification 7, through DBOS: a Create that fails a link fails the
// file-finding step, whose retry adds only that link; ErrLabels files
// with no retry.
func TestFindingsWorkflowRetries(t *testing.T) {
	t.Parallel()
	fake := &trackertest.Fake{}
	a, b := fake.Add(tracker.Item{Title: "a"}), fake.Add(tracker.Item{Title: "b"})
	fake.FailLinks = 1
	d, id := findingsDaemon(t, fake, nil, store.Finding{Key: "leak", Title: "Leak", BlockedBy: []string{a, b}})
	require.Equal(t, FindingsResult{Filed: 1, Kept: 1}, runFindings(t, d, id))
	it, err := fake.Get(t.Context(), "#3")
	require.NoError(t, err)
	require.Equal(t, []string{a, b}, it.BlockedBy)
	items, err := fake.List(t.Context(), tracker.Query{})
	require.NoError(t, err)
	require.Len(t, items, 3)

	dropping := &trackertest.Fake{DropLabels: true}
	d, id = findingsDaemon(t, dropping, nil, store.Finding{Key: "leak", Title: "Leak"})
	require.Equal(t, FindingsResult{Filed: 1, Kept: 1}, runFindings(t, d, id))
	n := 0
	for _, s := range stepNames(t, d, findingsWorkflowID(id)) {
		if s == "file-finding" {
			n++
		}
	}
	require.Equal(t, 1, n)
	f, err := d.store.GetFinding(t.Context(), id, "leak")
	require.NoError(t, err)
	require.Equal(t, store.FindingKept, f.State, "filed, then kept by the review")
	require.Contains(t, f.Why, "created without the labels bmus:unreviewed")
}

// creates is a fake tracker that counts Create calls, and fails those for
// the titles in fail without filing anything.
type creates struct {
	trackertest.Fake
	fail map[string]bool
	mu   sync.Mutex
	n    int
}

func (c *creates) Create(ctx context.Context, it tracker.Item, m tracker.Mark) (tracker.Item, error) {
	c.mu.Lock()
	c.n++
	c.mu.Unlock()
	if c.fail[it.Title] {
		return tracker.Item{}, errors.New("refused " + it.Title)
	}
	return c.Fake.Create(ctx, it, m)
}

func (c *creates) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}

// A blocker that was dropped links nothing, one dropped as a duplicate
// links the issue it repeats, a filed one its issue, and one that failed
// nothing; each link once.
func TestResolveBlockers(t *testing.T) {
	t.Parallel()
	byKey := map[string]store.Finding{
		"noise": {Key: "noise", State: store.FindingDropped},
		"dup":   {Key: "dup", State: store.FindingDropped, DuplicateOf: "#1"},
		"filed": {Key: "filed", State: store.FindingNew},
		"fail":  {Key: "fail", State: store.FindingNew},
	}
	refs := map[string]string{"filed": "#7"}
	require.Equal(t, []string{"#1", "#7", "#2"},
		resolveBlockers([]string{"noise", "dup", "filed", "fail", "#2", "#1", "#7"}, refs, byKey))
	require.Empty(t, resolveBlockers([]string{"noise", "fail"}, refs, byKey))
}

// A finding that failed to file leaves its dependent filed without the
// link.
func TestFindingsFailedBlocker(t *testing.T) {
	t.Parallel()
	fake := &creates{fail: map[string]bool{"A": true}}
	d, id := findingsDaemon(t, fake, nil,
		store.Finding{Key: "a", Title: "A"}, store.Finding{Key: "b", Title: "B", BlockedBy: []string{"a"}})
	require.Equal(t, FindingsResult{Filed: 1, Failed: 1, Kept: 1}, runFindings(t, d, id))
	it, err := fake.Get(t.Context(), "#1")
	require.NoError(t, err)
	require.Equal(t, "B", it.Title)
	require.Empty(t, it.BlockedBy)
	a, err := d.store.GetFinding(t.Context(), id, "a")
	require.NoError(t, err)
	require.Equal(t, store.FindingFailed, a.State)
	require.Equal(t, "refused A", a.Why)
}

// N1: a finding whose link keeps failing has its issue: out of retries it
// is filed with the link error noted, and its dependent links to it.
func TestFindingsLinkOutOfRetries(t *testing.T) {
	t.Parallel()
	fake := &linkFails{}
	existing := fake.Add(tracker.Item{Title: "existing"})
	d, id := findingsDaemon(t, fake, nil,
		store.Finding{Key: "a", Title: "A", BlockedBy: []string{existing}},
		store.Finding{Key: "b", Title: "B", BlockedBy: []string{"a"}})
	require.Equal(t, FindingsResult{Filed: 2, Kept: 2}, runFindings(t, d, id))
	a, err := d.store.GetFinding(t.Context(), id, "a")
	require.NoError(t, err)
	require.Equal(t, store.FindingKept, a.State, "filed, then kept by the review")
	require.Equal(t, "#2", a.Ref)
	require.Equal(t, "linking #2 to #1 failed", a.Why)
	b, err := d.store.GetFinding(t.Context(), id, "b")
	require.NoError(t, err)
	require.Equal(t, "linking #3 to #2 failed", b.Why, "its dependent links to it")
	items, err := fake.List(t.Context(), tracker.Query{})
	require.NoError(t, err)
	require.Len(t, items, 3, "one issue each")
	texts := orderLogTexts(t, d, id)
	require.Contains(t, texts, "finding a filed as #2 (linking #2 to #1 failed)")
	require.Contains(t, texts, "findings of "+id+": filed 2, dropped 0, failed 0")
}

// linkFails is a fake tracker whose every link fails: Create files the
// item without blockers and returns it with an error.
type linkFails struct{ trackertest.Fake }

func (l *linkFails) Create(ctx context.Context, it tracker.Item, m tracker.Mark) (tracker.Item, error) {
	blockers := it.BlockedBy
	it.BlockedBy = nil
	got, err := l.Fake.Create(ctx, it, m)
	if err != nil || len(blockers) == 0 {
		return got, err
	}
	return got, errors.New("linking " + got.Ref + " to " + strings.Join(blockers, ", ") + " failed")
}

// N2: a blocker that no longer exists is dropped with a note, and the
// finding is filed in the same attempt.
func TestFileFindingMissingBlocker(t *testing.T) {
	t.Parallel()
	fake := &trackertest.Fake{}
	existing := fake.Add(tracker.Item{Title: "existing"})
	d, id := findingsStore(t, fake, store.Finding{Key: "leak", Title: "Leak"})
	got, err := d.fileFinding(t.Context(), id, "leak", []string{"#9", existing}, findingsBegin{Since: time.Now(), Nonce: d.store.Nonce()})
	require.NoError(t, err)
	require.Equal(t, filedFinding{State: store.FindingFiled, Ref: "#2"}, got)
	it, err := fake.Get(t.Context(), "#2")
	require.NoError(t, err)
	require.Equal(t, []string{existing}, it.BlockedBy)
	require.Equal(t, []string{"created", "finding leak: blocker #9 dropped: item #9: tracker: not found", "finding leak filed as #2"},
		orderLogTexts(t, d, id))
}

// N3: findings that outgrow the context with empty bodies get shorter
// titles and fewer labels and blockers; past that, triage is skipped.
func TestTriageContextTight(t *testing.T) {
	t.Parallel()
	var labels, blockers []string
	for i := range 5 {
		labels = append(labels, strings.Repeat(string(rune('a'+i)), 50))
	}
	for i := range 10 {
		blockers = append(blockers, "o/"+strings.Repeat("r", 190)+"#"+strings.Repeat("1", 1+i%5))
	}
	finding := func(i int) store.Finding {
		return store.Finding{Key: "k" + strings.Repeat("x", i%39), Title: strings.Repeat("t", 200), Body: "b",
			Labels: labels, BlockedBy: blockers}
	}
	var fs []store.Finding
	for i := range store.MaxFindings {
		fs = append(fs, finding(i))
	}
	raw, _, err := triageContext("ORD-0001", "t", fs, nil)
	require.NoError(t, err)
	require.LessOrEqual(t, len(raw), maxConsultContext)
	var c triageHailContext
	require.NoError(t, json.Unmarshal(raw, &c))
	require.Len(t, c.Findings[0].Title, maxTriageTightTitle)
	require.Len(t, c.Findings[0].Labels, maxTriageTightList)
	require.Len(t, c.Findings[0].BlockedBy, maxTriageTightList)
	require.Empty(t, c.Findings[0].Body)

	for i := range 3 * store.MaxFindings {
		fs = append(fs, finding(i))
	}
	_, _, err = triageContext("ORD-0001", "t", fs, nil)
	require.ErrorIs(t, err, errTriageTooBig)
	require.ErrorContains(t, err, "the findings don't fit the triage context's 32 KiB")
}
