package main

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/tscolari/bmus/internal/bridge"
	"github.com/tscolari/bmus/internal/regs"
	"github.com/tscolari/bmus/internal/store"
)

// reviewContext is a review of two issues, a finding dropped as a
// duplicate and one that failed, with control characters in the crew's
// and Command's text.
func reviewContext(t *testing.T) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(bridge.ReviewContext{Order: "ORD-0001",
		Issues: []bridge.ReviewIssue{{Ref: "#2", Key: "a-lock", Title: "Lock held\x1b[2J across I/O"},
			{Ref: "#3", Key: "b-leak", Title: "Handle leak"}},
		Dropped: []bridge.ReviewDropped{{Key: "c-dup", Title: "Flaky test", Why: "seen\nbefore", DuplicateOf: "#1"}},
		Failed:  []bridge.ReviewFailed{{Key: "d-lost", Title: "Lost", Error: "tracker down"}}})
	require.NoError(t, err)
	return raw
}

// hails review prints the batch, one row per issue, dropped and failed
// finding, with control characters stripped, and the decision once there
// is one.
func TestPrintReview(t *testing.T) {
	t.Parallel()
	var b bytes.Buffer
	h := bridge.HailView{ID: "H-0002", Status: store.HailOpen, Category: regs.CategoryTrackerReview,
		Question: "Review 2 issues filed from ORD-0001's findings (1 dropped, 1 failed): keep, discard, " +
			"or answer with the refs to discard and the keys to restore.", Context: reviewContext(t)}
	require.NoError(t, printReview(&b, h))
	require.Equal(t, `H-0002 open: Review 2 issues filed from ORD-0001's findings (1 dropped, 1 failed): keep, discard, or answer with the refs to discard and the keys to restore.
N  REF/KEY  STATE    TITLE                     WHY
1  #2       filed    Lock held [2J across I/O  -
2  #3       filed    Handle leak               -
3  c-dup    dropped  Flaky test                duplicate of #1; seen before
4  d-lost   failed   Lost                      tracker down
`, b.String())
	require.NotContains(t, b.String(), "\x1b")

	b.Reset()
	h.Status, h.Decision = store.HailDecided, "discard #2"
	require.NoError(t, printReview(&b, h))
	require.True(t, strings.HasSuffix(b.String(), "decision: discard #2\n"), b.String())
}

func TestReviewDecision(t *testing.T) {
	t.Parallel()
	require.Equal(t, "keep", reviewDecision(nil, nil))
	require.Equal(t, "discard #2 #3", reviewDecision([]string{"#2", "#3"}, nil))
	require.Equal(t, "keep; restore c-dup", reviewDecision(nil, []string{"c-dup"}))
	require.Equal(t, "discard #2; restore c-dup d-lost", reviewDecision([]string{"#2"}, []string{"c-dup", "d-lost"}))
}

// `bmus hails review` against a bridge holding three bridge-raised hails:
// two findings reviews and another category. Without flags it prints the
// batch; with them it answers through hails.answer, and the bridge's bad
// requests (a ref or key the review lacks) and the CLI's own (--keep with
// --discard, another category, an unknown hail) are refused.
func TestHailsReviewCLI(t *testing.T) {
	t.Parallel()
	e := newE2E(t)
	ctx := t.Context()
	st, err := store.Open(ctx, filepath.Join(e.home, store.DBFile))
	require.NoError(t, err)
	for _, h := range []store.Hail{
		{DeliverTo: "findings/ORD-0001", Category: regs.CategoryTrackerReview, Question: "Review 2 issues?",
			Options: []string{"keep", "discard"}, Context: reviewContext(t)},
		{DeliverTo: "findings/ORD-0002", Category: regs.CategoryTrackerReview, Question: "Review 2 issues?",
			Options: []string{"keep", "discard"}, Context: reviewContext(t)},
		{DeliverTo: "raiser/pick", Category: "campaign_pick", Question: "Which?", Options: []string{"a", "b"}},
	} {
		_, err := st.InsertBridgeHail(ctx, h, store.LogEntry{Kind: "hail", Text: "raised"})
		require.NoError(t, err)
	}
	require.NoError(t, st.Close())
	e.startBridge(t)

	out := e.run(t, "hails", "review", "H-0001")
	require.Contains(t, out, "H-0001 open: Review 2 issues?\n")
	require.Regexp(t, `(?m)^3\s+c-dup\s+dropped\s+Flaky test\s+duplicate of #1; seen before$`, out)

	fail := func(args ...string) string {
		t.Helper()
		cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		var stderr bytes.Buffer
		cmd := e.cmd(cctx, args...)
		cmd.Stderr = &stderr
		require.Error(t, cmd.Run(), "bmus %s", strings.Join(args, " "))
		return stderr.String()
	}
	require.Contains(t, fail("hails", "review", "H-0001", "--discard", "#9"),
		`"#9" is not a ref of the review; answer keep, discard, discard <ref>..., or restore <key>..., `+
			`the first three optionally followed by "; restore <key>..." (refs: #2, #3; keys: c-dup, d-lost)`)
	require.Contains(t, fail("hails", "review", "H-0001", "--restore", "a-lock"), `"a-lock" is not a dropped or failed finding's key`)
	require.Contains(t, fail("hails", "review", "H-0001", "--keep", "--discard", "#2"), "pass --keep or --discard, not both")
	require.Contains(t, fail("hails", "review", "H-0003", "--keep"), "hail H-0003 is campaign_pick, not a findings review (tracker_review)")
	require.Contains(t, fail("hails", "review", "H-0009"), "no hail H-0009")
	require.Contains(t, fail("hails", "answer", "H-0001", "--decision", "maybe"), `"maybe" is not keep, discard or restore`)

	require.Equal(t, "H-0001 decided: discard #3; restore c-dup\n",
		e.run(t, "hails", "review", "H-0001", "--discard", "#3", "--restore", "c-dup"))
	require.Contains(t, e.run(t, "hails", "review", "H-0001"), "decision: discard #3; restore c-dup\n")
	require.Contains(t, fail("hails", "review", "H-0001", "--keep"), "decided")
	require.Equal(t, "H-0002 decided: keep\n", e.run(t, "hails", "review", "H-0002", "--keep"))
}
