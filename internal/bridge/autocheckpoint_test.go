package bridge

import (
	"bytes"
	"database/sql"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tscolari/bmus/internal/comms"
	"github.com/tscolari/bmus/internal/store"
	"github.com/tscolari/bmus/internal/transporter"
)

// wipClone clones the shipped bundle's bmus/<order> as a beam would and
// returns it with its base commit.
func wipClone(t *testing.T, shipped, orderID string) (clone, base string) {
	t.Helper()
	clone = filepath.Join(t.TempDir(), "c")
	gitT(t, filepath.Dir(clone), "clone", "-q", "--branch", orderBranch(orderID), shipped, clone)
	return clone, gitT(t, clone, "rev-parse", "HEAD")
}

// commitFile commits name with content in clone and returns the commit.
func commitFile(t *testing.T, clone, name, content string) string {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(clone, name), []byte(content), 0o600))
	gitT(t, clone, "add", name)
	gitT(t, clone, "commit", "-q", "-m", name)
	return gitT(t, clone, "rev-parse", "HEAD")
}

// snapshotWip makes a WIP commit of name with content on top of the tip,
// as combadge's snapshotWip does, and points refs/bmus/wip/<order> at it.
func snapshotWip(t *testing.T, clone, orderID, name, content string) string {
	t.Helper()
	tip := gitT(t, clone, "rev-parse", "HEAD")
	require.NoError(t, os.WriteFile(filepath.Join(clone, name), []byte(content), 0o600))
	gitT(t, clone, "add", name)
	tree := gitT(t, clone, "write-tree")
	gitT(t, clone, "reset", "-q")
	require.NoError(t, os.Remove(filepath.Join(clone, name)))
	wip := gitT(t, clone, "commit-tree", tree, "-p", tip, "-m", "wip")
	gitT(t, clone, "update-ref", wipRef(orderID), wip)
	return wip
}

// #145: a checkpoint bundle holding only what came after the previous
// checkpoint lands in the bridge's clone when the previous one did, and
// is replaced by the full bundle from the beam when it doesn't land.
func TestLandCheckpointIncrementalAndFull(t *testing.T) {
	t.Parallel()
	up := testRepo(t)
	d := testDaemon(t, map[string]string{"app": up.url})
	d.logger = slog.New(slog.DiscardHandler)
	d.maxBundleBytes = DefaultMaxBundleBytes
	root := t.TempDir()
	d.transporter = &fakeProvider{copyRoot: root}
	beam := transporter.Beam{Alias: "beam-1"}
	ctx := t.Context()
	const order = "ORD-0001"
	shipped := filepath.Join(t.TempDir(), "in.bundle")
	_, err := d.bundleRepo(ctx, "app", up.url, order, "main", shipped)
	require.NoError(t, err)
	clone, base := wipClone(t, shipped, order)
	branch := "refs/heads/" + orderBranch(order)
	onBeam := func(name string, args ...string) string {
		gitT(t, clone, append([]string{"bundle", "create", "-q", filepath.Join(root, name), branch}, args...)...)
		return "/" + name
	}
	c1 := commitFile(t, clone, "one.txt", "1\n")
	first := onBeam("first.bundle", "^"+base)
	c2 := commitFile(t, clone, "two.txt", "2\n")
	full := onBeam("full.bundle", "^"+base)
	inc := onBeam("inc.bundle", "^"+c1)
	cargo := filepath.Join(t.TempDir(), "cargo")
	require.NoError(t, os.MkdirAll(cargo, 0o700))
	local := filepath.Join(cargo, "app.bundle")
	copyIn := func(remote string) {
		data, err := os.ReadFile(filepath.Join(root, remote))
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(local, data, 0o600))
	}
	repo := comms.WorkRepo{Name: "app", Branch: orderBranch(order), Commit: c2, BundlePath: inc, FullBundlePath: full}

	// The clone never landed c1: the incremental bundle lacks its
	// prerequisite, and the full one replaces it in the cargo bay.
	copyIn(inc)
	require.NoError(t, d.landCheckpoint(ctx, beam, order, repo, local))
	got, err := os.ReadFile(local)
	require.NoError(t, err)
	want, err := os.ReadFile(filepath.Join(root, full))
	require.NoError(t, err)
	require.True(t, bytes.Equal(want, got), "the full bundle is in the cargo bay")
	require.Equal(t, c2, gitT(t, d.repoDir("app"), "rev-parse", checkpointRef(order)))

	// Once c1 has landed, the incremental bundle lands as it is.
	d2 := testDaemon(t, map[string]string{"app": up.url})
	d2.logger, d2.maxBundleBytes, d2.transporter = d.logger, d.maxBundleBytes, d.transporter
	_, err = d2.bundleRepo(ctx, "app", up.url, order, "main", filepath.Join(t.TempDir(), "in.bundle"))
	require.NoError(t, err)
	copyIn(first)
	require.NoError(t, d2.landCheckpoint(ctx, beam, order, comms.WorkRepo{Name: "app", Branch: orderBranch(order), Commit: c1, BundlePath: first}, local))
	copyIn(inc)
	require.NoError(t, d2.landCheckpoint(ctx, beam, order, repo, local))
	got, err = os.ReadFile(local)
	require.NoError(t, err)
	want, err = os.ReadFile(filepath.Join(root, inc))
	require.NoError(t, err)
	require.True(t, bytes.Equal(want, got), "the incremental bundle stays")
	require.Equal(t, c2, gitT(t, d2.repoDir("app"), "rev-parse", checkpointRef(order)))
}

// #145: ship-repos sends the WIP ref of the checkpoint it ships from, and
// when it falls back to an older checkpoint or to base_ref, the newest
// checkpoint's WIP ref still goes along. bmus/<order> never takes it, and
// the clone keeps no WIP ref after the ship.
func TestBundleRepoFromShipsWip(t *testing.T) {
	t.Parallel()
	up := testRepo(t)
	d := testDaemon(t, map[string]string{"app": up.url})
	st, err := store.Open(t.Context(), filepath.Join(d.home, store.DBFile))
	require.NoError(t, err)
	t.Cleanup(func() { st.Close() })
	d.store = st
	ctx := t.Context()
	in := MissionInput{OrderID: "ORD-0001", CrewID: "sorell"}
	shipped := filepath.Join(t.TempDir(), "in.bundle")
	_, err = d.bundleRepo(ctx, "app", up.url, in.OrderID, "main", shipped)
	require.NoError(t, err)
	clone, base := wipClone(t, shipped, in.OrderID)
	branch := "refs/heads/" + orderBranch(in.OrderID)
	// A checkpoint as the beam writes it: bmus/<order> from the base, and
	// the WIP refs on their own from the tip (wipBundleOf).
	bundle := func(wips ...string) string {
		out := filepath.Join(t.TempDir(), "cp.bundle")
		gitT(t, clone, "bundle", "create", "-q", out, branch, "^"+base)
		gitT(t, clone, append(append([]string{"bundle", "create", "-q", wipBundleOf(out)}, wips...), "^"+branch)...)
		return out
	}
	older := commitFile(t, clone, "older.txt", "older\n")
	olderWip := snapshotWip(t, clone, in.OrderID, "draft.txt", "older draft\n")
	olderBundle := bundle(wipRef(in.OrderID))
	newer := commitFile(t, clone, "newer.txt", "newer\n")
	newerWip := snapshotWip(t, clone, in.OrderID, "draft.txt", "newer draft\n")
	gitT(t, clone, "update-ref", keptWipRef(in.OrderID), olderWip) // a WIP an earlier run couldn't restore
	newerBundle := bundle(wipRef(in.OrderID), keptWipRef(in.OrderID))
	kept := "\n" + olderWip + " " + keptWipRef(in.OrderID)
	cp := func(seq int, bundle, commit string) store.Checkpoint {
		return store.Checkpoint{OrderID: in.OrderID, Beam: "b1", Seq: seq,
			Repos: []store.CheckpointRepo{{Name: "app", Commit: commit, BundlePath: bundle}}}
	}
	heads := func(out string) string { return bundleHeads(t, out) }
	out := filepath.Join(t.TempDir(), "out.bundle")
	clean := func() {
		for _, ref := range []string{wipRef(in.OrderID), keptWipRef(in.OrderID), "refs/bmus/scratch/" + in.OrderID} {
			_, err := runGit(ctx, d.repoDir("app"), "rev-parse", "--verify", "--quiet", ref)
			require.Error(t, err, "the clone keeps no %s", ref)
		}
	}

	// From the newest: its WIP.
	commit, from, err := d.bundleRepoFrom(ctx, in, "app", up.url, "main", out, []store.Checkpoint{cp(2, newerBundle, newer), cp(1, olderBundle, older)})
	require.NoError(t, err)
	require.Equal(t, newer, commit)
	require.Equal(t, 2, from.Seq)
	require.Equal(t, newer+" "+branch+"\n"+newerWip+" "+wipRef(in.OrderID)+kept, heads(out))
	clean()

	// The newest is rejected (its row names a commit its bundle lacks): it
	// ships from the older one, with the newest's WIP.
	commit, from, err = d.bundleRepoFrom(ctx, in, "app", up.url, "main", out, []store.Checkpoint{cp(2, newerBundle, older), cp(1, olderBundle, older)})
	require.NoError(t, err)
	require.Equal(t, older, commit)
	require.Equal(t, 1, from.Seq)
	require.Equal(t, older+" "+branch+"\n"+newerWip+" "+wipRef(in.OrderID)+kept, heads(out))
	clean()

	// All rejected: base_ref, still with the newest's WIP.
	commit, from, err = d.bundleRepoFrom(ctx, in, "app", up.url, "main", out, []store.Checkpoint{cp(2, newerBundle, base), cp(1, olderBundle, base)})
	require.NoError(t, err)
	require.Nil(t, from)
	require.Equal(t, base, commit)
	require.Equal(t, base+" "+branch+"\n"+newerWip+" "+wipRef(in.OrderID)+kept, heads(out))
	clean()
	require.NotEqual(t, olderWip, newerWip)

	// A newest checkpoint missing from the cargo bay has no WIP to give:
	// the shipped one's goes.
	_, _, err = d.bundleRepoFrom(ctx, in, "app", up.url, "main", out,
		[]store.Checkpoint{cp(3, filepath.Join(t.TempDir(), "gone.bundle"), newer), cp(1, olderBundle, older)})
	require.NoError(t, err)
	require.Equal(t, older+" "+branch+"\n"+olderWip+" "+wipRef(in.OrderID), heads(out))

	// bmus/<order> in the clone never holds a WIP commit.
	for _, wip := range []string{olderWip, newerWip} {
		_, err := runGit(ctx, d.repoDir("app"), "merge-base", "--is-ancestor", wip, branch)
		require.Error(t, err)
	}
}

// parkScript writes a holodeck script that commits, leaves notes/wip.txt
// uncommitted and hails, parking 200ms later (#145). A run resumed from
// that park walks on from the progress step, sleeping 4s before it
// reports: room for an answer to reach it.
func parkScript(t *testing.T) string {
	t.Helper()
	script := filepath.Join(t.TempDir(), "park.json")
	require.NoError(t, os.WriteFile(script, []byte(`[{"at_pct": 10, "commit": "a change"},
		{"at_pct": 20, "write": "notes/wip.txt"},
		{"at_pct": 30, "hail": {"category": "clarifying_question", "question": "Which way?", "options": ["left", "right"]}, "park_after_ms": 200},
		{"at_pct": 40, "progress": "resumed"},
		{"at_pct": 50, "sleep_ms": 4000},
		{"at_pct": 100, "report": {"summary": "done after the park"}}]`), 0o600))
	return script
}

// resumedSteps are order id's step-log entries that carry a handover: its
// resumed run's.
func resumedSteps(t require.TestingT, stepLog, id string) []map[string]any {
	var out []map[string]any
	for _, e := range readSteps(t, stepLog) {
		if e["order"] == id && e["handover"] != nil && e["action"] != "correction" {
			out = append(out, e)
		}
	}
	return out
}

// #145's acceptance: an order parks on a hail and its park checkpoint, with
// the uncommitted file as work in progress, lands in the cargo bay. The
// beam is vanished during the wait: the hail stays open, the captain
// answers it with no beam (before) or once the order has resumed (after).
// The order resumes on a new beam from that checkpoint with the file back
// in its work tree, named in the handover note, and raises no second hail.
// Answered before the resume, the question and answer are in the note;
// after it, the answer reaches the new run as a correction naming the
// question. bmus/<order>, the report and the push hold no WIP commit.
func TestParkCheckpointOutlivesBeam(t *testing.T) {
	t.Parallel()
	for _, answer := range []string{"before", "after"} {
		t.Run(answer, func(t *testing.T) {
			t.Parallel()
			testParkCheckpointOutlivesBeam(t, answer == "before")
		})
	}
}

func testParkCheckpointOutlivesBeam(t *testing.T, before bool) {
	b := newHolodeckBridge(t, parkScript(t))
	hd := b.start(t)
	c := hd.addCrew(t)
	id := hd.createOrder(t)
	hd.waitHail(t, "H-0001")
	var cp store.Checkpoint
	require.EventuallyWithT(t, func(ct *assert.CollectT) {
		cps, err := hd.d.store.ListCheckpoints(t.Context(), id)
		require.NoError(ct, err)
		require.Len(ct, cps, 1)
		cp = cps[0]
	}, 30*time.Second, 20*time.Millisecond)
	note, err := os.ReadFile(cp.HandoverPath)
	require.NoError(t, err)
	require.Contains(t, string(note), "The holodeck parked its script at step 2.")
	first := commitOf(t, b.stepLog, id, 0)
	require.Equal(t, first, cp.Repos[0].Commit)
	require.Equal(t, first+" refs/heads/bmus/"+id, bundleHeads(t, cp.Repos[0].BundlePath))
	heads := bundleHeads(t, wipBundleOf(cp.Repos[0].BundlePath))
	wip, _, ok := strings.Cut(heads, " ")
	require.True(t, ok, heads)
	require.Equal(t, wip+" "+wipRef(id), heads)

	require.NoError(t, b.h.Kill(c.CurrentBeam))
	require.Equal(t, CrewTourResult{Ended: store.CrewSickbay}, tourResult(t, hd.d, tourWorkflowID("sorell", 0)))
	require.EventuallyWithT(t, func(ct *assert.CollectT) {
		require.NotNil(ct, hd.order(ct, id).ParkedAt)
	}, 30*time.Second, 20*time.Millisecond)
	// The hail its park checkpoint held stays open while the order is parked.
	h, err := hd.d.store.GetHail(t.Context(), "H-0001")
	require.NoError(t, err)
	require.Equal(t, store.HailOpen, h.Status)
	require.True(t, h.Carried)
	require.Equal(t, store.OrderAway, hd.order(t, id).State)
	require.Empty(t, orderTexts(hd.logEntries(t), id, "hail H-0001 closed"))

	if before {
		require.NoError(t, hd.d.AnswerHail(t.Context(), "H-0001", "left", nil))
		require.EventuallyWithT(t, func(ct *assert.CollectT) {
			require.Equal(ct, 1, countSteps(t, hd.d, missionWorkflowID(id), "carried-answer"))
		}, 30*time.Second, 20*time.Millisecond)
	}
	requireOutcome(t, OutcomeRequested)(hd.d.ReenergizeCrew(t.Context(), "sorell"))
	if !before {
		require.EventuallyWithT(t, func(ct *assert.CollectT) {
			require.NotEmpty(ct, resumedSteps(ct, b.stepLog, id))
		}, 60*time.Second, 20*time.Millisecond)
		h, err := hd.d.store.GetHail(t.Context(), "H-0001")
		require.NoError(t, err)
		require.Equal(t, store.HailOpen, h.Status, "open while the resumed run works")
		require.NoError(t, hd.d.AnswerHail(t.Context(), "H-0001", "left", nil))
	}
	res := missionResult(t, hd.d, id)
	require.Equal(t, MissionDone, res.Status, "%+v", res)
	require.Equal(t, "done after the park", res.Report.Summary)

	// The resumed run walked on from the park, on a new beam, and raised no
	// second hail.
	steps := resumedSteps(t, b.stepLog, id)
	var actions []string
	for _, e := range steps {
		actions = append(actions, e["action"].(string))
	}
	require.Equal(t, []string{"progress", "sleep_ms", "report"}, actions)
	_, err = hd.d.store.GetHail(t.Context(), "H-0002")
	require.ErrorIs(t, err, sql.ErrNoRows)
	require.NotEqual(t, c.CurrentBeam, hd.crew(t, "sorell").CurrentBeam)

	// The uncommitted file is back in the new beam's work tree, and the
	// note says so.
	handover := steps[0]["handover"].(string)
	require.Contains(t, handover, "The holodeck parked its script at step 2.")
	require.Contains(t, handover, "Uncommitted work was restored in ")
	require.Contains(t, handover, "from the shipped checkpoint), unstaged; files: notes/wip.txt.")
	dir := steps[0]["repos"].(map[string]any)["app"].(string)
	data, err := os.ReadFile(filepath.Join(dir, "notes", "wip.txt"))
	require.NoError(t, err)
	require.Equal(t, id+" step 1\n", string(data))
	require.Equal(t, "?? notes/", gitT(t, dir, "status", "--porcelain"))

	answered := `While the order was parked, the captain answered hail H-0001, which the earlier session asked: "Which way?". The answer: "left".`
	if before {
		require.Contains(t, handover, answered)
		require.Empty(t, corrections(t, b.stepLog))
	} else {
		require.NotContains(t, handover, "the captain answered")
		require.Equal(t, []comms.CorrectionBody{{HailID: "H-0001", Ref: id, Note: "left", Question: "Which way?"}}, corrections(t, b.stepLog))
	}
	h, err = hd.d.store.GetHail(t.Context(), "H-0001")
	require.NoError(t, err)
	require.Equal(t, store.HailDecided, h.Status)

	// bmus/<order> holds no WIP commit, nor do the report and the push.
	clone := filepath.Join(b.home, "repos", "app")
	require.Equal(t, first, res.Report.Repos[0].Commit)
	require.Equal(t, first, gitT(t, clone, "rev-parse", "bmus/"+id))
	_, err = runGit(t.Context(), clone, "merge-base", "--is-ancestor", wip, "bmus/"+id)
	require.Error(t, err)
	require.Equal(t, first+" refs/heads/bmus/"+id, bundleHeads(t, filepath.Join(b.home, "cargo", id, "final", "app.bundle")))
	_, err = hd.d.PushOrder(t.Context(), id, "pushed", false)
	require.NoError(t, err)
	require.Equal(t, first, gitT(t, b.repo.url, "rev-parse", "pushed"))
	require.NotContains(t, refs(t, b.repo.url), "refs/bmus/")
	_, err = runGit(t.Context(), b.repo.url, "cat-file", "-e", wip)
	require.Error(t, err, "the WIP commit never reached the upstream")
	requireNoBackstop(t, hd.logEntries(t))
}

// #145's acceptance: an order re-sent to the same beam after a stop finds
// its work in progress in its existing checkout, which serve set aside,
// and restores it.
func TestStoppedOrderRestoresWipFromCheckout(t *testing.T) {
	t.Parallel()
	script := filepath.Join(t.TempDir(), "stop.json")
	require.NoError(t, os.WriteFile(script, []byte(`[{"at_pct": 10, "commit": "a change"},
		{"at_pct": 20, "write": "notes/wip.txt"},
		{"at_pct": 50, "hail": {"category": "clarifying_question", "question": "Which way?", "options": ["left", "right"]}},
		{"at_pct": 100, "report": {"summary": "done after the recall"}}]`), 0o600))
	b := newHolodeckBridge(t, script)
	hd := b.start(t)
	c := hd.addCrewWith(t, store.Crew{ID: "sorell", Name: "Sorell", Division: "ops"})
	id := hd.createDivisionOrder(t, "sorell", "ops")
	hd.waitHail(t, "H-0001")
	require.NoError(t, hd.d.RecallOrder(t.Context(), id, "reprioritise"))
	require.EventuallyWithT(t, func(ct *assert.CollectT) {
		o := hd.order(ct, id)
		require.Equal(ct, store.OrderPool, o.State)
		require.NotNil(ct, o.ParkedAt)
	}, 30*time.Second, 50*time.Millisecond)
	require.NoError(t, hd.d.SetStandingOrders(t.Context(), true))
	hd.waitHail(t, "H-0002")
	require.Equal(t, c.CurrentBeam, hd.crew(t, "sorell").CurrentBeam, "the same beam")

	steps := resumedSteps(t, b.stepLog, id)
	require.NotEmpty(t, steps)
	handover := steps[0]["handover"].(string)
	require.Contains(t, handover, "Uncommitted work was restored in ")
	require.Contains(t, handover, "from the order's existing checkout on this beam), unstaged; files: notes/wip.txt.")
	dir := steps[0]["repos"].(map[string]any)["app"].(string)
	data, err := os.ReadFile(filepath.Join(dir, "notes", "wip.txt"))
	require.NoError(t, err)
	// The restored line, then the resumed run's own write.
	require.Equal(t, id+" step 1\n"+id+" step 1\n", string(data))
	require.NoError(t, hd.d.AnswerHail(t.Context(), "H-0002", "left", nil))
	res := missionResult(t, hd.d, id)
	require.Equal(t, MissionDone, res.Status, "%+v", res)
	clone := filepath.Join(b.home, "repos", "app")
	require.Equal(t, res.Report.Repos[0].Commit, gitT(t, clone, "rev-parse", "bmus/"+id))
	require.Equal(t, res.Report.Repos[0].Commit+" refs/heads/bmus/"+id, bundleHeads(t, filepath.Join(b.home, "cargo", id, "final", "app.bundle")))
}

// #145: a checkpoint's WIP bundle over the size cap stays on the beam and
// is logged; the checkpoint's commits are pulled without it. One under
// the cap lands beside the checkpoint's bundle.
func TestPullWipOverCap(t *testing.T) {
	t.Parallel()
	d := testDaemon(t, nil)
	st, err := store.Open(t.Context(), filepath.Join(d.home, store.DBFile))
	require.NoError(t, err)
	t.Cleanup(func() { st.Close() })
	d.store = st
	root := t.TempDir()
	d.transporter = &fakeProvider{copyRoot: root}
	d.maxBundleBytes = 10
	require.NoError(t, os.WriteFile(filepath.Join(root, "app.wip.bundle"), []byte("0123456789abc"), 0o600))
	local := filepath.Join(t.TempDir(), "app.bundle")
	in := MissionInput{OrderID: "ORD-0001", CrewID: "sorell"}
	r := comms.WorkRepo{Name: "app", WipBundlePath: "/app.wip.bundle"}
	beam := transporter.Beam{Alias: "beam-1"}
	require.NoError(t, d.pullWip(t.Context(), beam, in, 3, r, local))
	require.NoFileExists(t, wipBundleOf(local))
	entries, err := st.ListLog(t.Context(), 0)
	require.NoError(t, err)
	require.Equal(t, map[string]int{CodeBundleTooLarge: 1}, codes(t, entries))
	require.Equal(t, "checkpoint 3: repo app's uncommitted work is 13 bytes, over the 10 byte cap; its commits are pulled without it", entries[len(entries)-1].Text)

	d.maxBundleBytes = 100
	require.NoError(t, d.pullWip(t.Context(), beam, in, 4, r, local))
	require.FileExists(t, wipBundleOf(local))
	// A later checkpoint without WIP leaves no stale copy.
	require.NoError(t, d.pullWip(t.Context(), beam, in, 4, comms.WorkRepo{Name: "app"}, local))
	require.NoFileExists(t, wipBundleOf(local))
}

// parkedWithCarried starts a holodeck bridge on script, waits for H-0001
// and the park checkpoint holding it, then kills sorell's beam: the order
// parks with H-0001 carried (#145).
func parkedWithCarried(t *testing.T, script string) (*holodeckBridge, *hdaemon, string) {
	t.Helper()
	b := newHolodeckBridge(t, script)
	hd := b.start(t)
	c := hd.addCrew(t)
	id := hd.createOrder(t)
	hd.waitHail(t, "H-0001")
	require.EventuallyWithT(t, func(ct *assert.CollectT) {
		cps, err := hd.d.store.ListCheckpoints(t.Context(), id)
		require.NoError(ct, err)
		require.Len(ct, cps, 1)
	}, 30*time.Second, 20*time.Millisecond)
	require.NoError(t, b.h.Kill(c.CurrentBeam))
	require.Equal(t, CrewTourResult{Ended: store.CrewSickbay}, tourResult(t, hd.d, tourWorkflowID("sorell", 0)))
	require.EventuallyWithT(t, func(ct *assert.CollectT) {
		require.NotNil(ct, hd.order(ct, id).ParkedAt)
	}, 30*time.Second, 20*time.Millisecond)
	h, err := hd.d.store.GetHail(t.Context(), "H-0001")
	require.NoError(t, err)
	require.Equal(t, store.HailOpen, h.Status)
	require.True(t, h.Carried)
	// Nobody hails on a carried hail's account.
	require.Equal(t, store.OrderAway, hd.order(t, id).State)
	return b, hd, id
}

// #145: the resumed run asks the carried hail's question again: the new
// hail is raised at once, not queued behind the carried one, which closes
// as superseded, so the captain answers once.
func TestCarriedHailSupersededByResumedRun(t *testing.T) {
	t.Parallel()
	script := filepath.Join(t.TempDir(), "again.json")
	require.NoError(t, os.WriteFile(script, []byte(`[{"at_pct": 10, "commit": "a change"},
		{"at_pct": 20, "write": "notes/wip.txt"},
		{"at_pct": 30, "hail": {"category": "clarifying_question", "question": "Which way?", "options": ["left", "right"]}, "park_after_ms": 200},
		{"at_pct": 40, "hail": {"category": "clarifying_question", "question": "Which way?", "options": ["left", "right"]}},
		{"at_pct": 100, "report": {"summary": "asked again"}}]`), 0o600))
	_, hd, id := parkedWithCarried(t, script)
	requireOutcome(t, OutcomeRequested)(hd.d.ReenergizeCrew(t.Context(), "sorell"))
	hd.waitHail(t, "H-0002")
	e := hd.waitLog(t, "hail H-0001 closed unanswered")
	require.Equal(t, "hail H-0001 closed unanswered: superseded by hail H-0002", e.Text)
	h, err := hd.d.store.GetHail(t.Context(), "H-0001")
	require.NoError(t, err)
	require.Equal(t, store.HailTimedOut, h.Status)
	require.NoError(t, hd.d.AnswerHail(t.Context(), "H-0002", "left", nil))
	res := missionResult(t, hd.d, id)
	require.Equal(t, MissionDone, res.Status, "%+v", res)
}

// #145: a carried hail still open when the mission ends is closed with it.
func TestCarriedHailClosedWhenOrderEnds(t *testing.T) {
	t.Parallel()
	_, hd, id := parkedWithCarried(t, parkScript(t))
	require.NoError(t, hd.d.BelayOrder(t.Context(), id, "scrapped"))
	require.Equal(t, MissionBelayed, missionResult(t, hd.d, id).Status)
	e := hd.waitLog(t, "hail H-0001 closed unanswered")
	require.Equal(t, "hail H-0001 closed unanswered: order belayed", e.Text)
}

// #145: jettison's sweep drops the landed checkpoint ref of every ended
// order and keeps an open order's.
func TestSweepCheckpointRefs(t *testing.T) {
	t.Parallel()
	up := testRepo(t)
	head := up.rev(t, "main")
	dir := t.TempDir()
	gitT(t, dir, "init", "-q", "--bare")
	gitT(t, dir, "fetch", "-q", up.url, "main:refs/heads/main")
	for _, id := range []string{"ORD-0001", "ORD-0002", "ORD-0003"} {
		gitT(t, dir, "update-ref", checkpointRef(id), head)
	}
	sweepCheckpointRefs(t.Context(), dir, []store.Order{
		{ID: "ORD-0001", State: store.OrderFailed}, {ID: "ORD-0002", State: store.OrderAway}, {ID: "ORD-0003", State: store.OrderDone},
	})
	require.Equal(t, checkpointRef("ORD-0002"), gitT(t, dir, "for-each-ref", "--format=%(refname)", "refs/bmus/"))
}

// #145: a carried tool_approval the captain allows while the order has no
// run becomes a grant in the resumed run's order, with the call its
// context names.
func TestCarriedToolApprovalBecomesGrant(t *testing.T) {
	t.Parallel()
	script := filepath.Join(t.TempDir(), "grant.json")
	require.NoError(t, os.WriteFile(script, []byte(`[{"at_pct": 10, "commit": "a change"},
		{"at_pct": 30, "hail": {"category": "tool_approval", "question": "Run `+"`make deploy`"+`?", "options": ["allow", "deny"],
			"context": {"tool": "Bash", "input": {"command": "make deploy"}}}, "park_after_ms": 200},
		{"at_pct": 40, "progress": "resumed"},
		{"at_pct": 100, "report": {"summary": "deployed"}}]`), 0o600))
	b, hd, id := parkedWithCarried(t, script)
	allow := 0
	require.NoError(t, hd.d.AnswerHail(t.Context(), "H-0001", "", &allow))
	requireOutcome(t, OutcomeRequested)(hd.d.ReenergizeCrew(t.Context(), "sorell"))
	require.Equal(t, MissionDone, missionResult(t, hd.d, id).Status)
	steps := resumedSteps(t, b.stepLog, id)
	require.NotEmpty(t, steps)
	require.Equal(t, []any{map[string]any{"tool": "Bash", "input": map[string]any{"command": "make deploy"}}}, steps[0]["grants"])
	require.Contains(t, steps[0]["handover"], `the captain answered hail H-0001, which the earlier session asked: "Run `+"`make deploy`"+`?". The answer: "allow".`)
}
