package bridge

import (
	"encoding/json"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tscolari/bmus/internal/store"
)

// insertDoneOrder inserts a done order on repos with a done entry whose
// payload carries work, and returns its id.
func insertDoneOrder(t *testing.T, st *store.Store, repos []store.OrderRepo, work ...landedRepo) string {
	t.Helper()
	ctx := t.Context()
	id, err := st.InsertOrder(ctx, store.Order{Title: "done", Division: "ops", State: store.OrderDone, Repos: repos},
		store.LogEntry{Kind: logOrder, Text: "order created: done"})
	require.NoError(t, err)
	if work == nil {
		return id
	}
	payload, err := json.Marshal(donePayload{Summary: "s", Repos: work})
	require.NoError(t, err)
	_, err = st.AppendLog(ctx, store.LogEntry{Kind: logDone, OrderID: id, Text: "s", Payload: payload})
	require.NoError(t, err)
	return id
}

// order.create with a base_ref of order:<id> (#143): an unknown order, a
// malformed id, a repo the named order didn't use, an order that will
// never be done, a split one (named with its children when it has them),
// a done one without fetched work on the repo, one whose branch was
// jettisoned and one whose commit the clone lacks are bad requests, and
// nothing is inserted for them. An open order
// listing the repo and a done one with its work are taken, each adding
// itself to the order's after once.
func TestOrderCreateBase(t *testing.T) {
	t.Parallel()
	home := privateHome(t)
	up := testRepo(t)
	cfg := testConfig(home)
	cfg.Repos = map[string]string{"app": up.url, "lib": up.url}
	r := startDaemonConfig(t, cfg)
	defer r.stop(t)
	ctx := t.Context()
	st := r.d.store
	app := []store.OrderRepo{{Name: "app", BaseRef: "main"}}
	commit := up.rev(t, "main")

	done := insertDoneOrder(t, st, app, landedRepo{Name: "app", Branch: "bmus/ORD-0001", Commit: commit})
	open, err := st.InsertOrder(ctx, store.Order{Title: "open", Division: "ops", Repos: app}, testEntry)
	require.NoError(t, err)
	withdrawn, err := st.InsertOrder(ctx, store.Order{Title: "w", Division: "ops", State: store.OrderWithdrawn, Repos: app}, testEntry)
	require.NoError(t, err)
	split, err := st.InsertOrder(ctx, store.Order{Title: "s", Division: "ops", State: store.OrderSplit, Repos: app}, testEntry)
	require.NoError(t, err)
	noReport := insertDoneOrder(t, st, app)
	unfetched := insertDoneOrder(t, st, app, landedRepo{Name: "app", Branch: "bmus/x", Commit: commit, FetchError: "boom"})
	jettisoned := insertDoneOrder(t, st, app, landedRepo{Name: "app", Branch: "bmus/x", Commit: commit})
	_, err = st.AppendLog(ctx, store.LogEntry{Kind: logOrder, OrderID: jettisoned, Text: jettisonText(jettisoned, "app"),
		Payload: json.RawMessage(`{"repo":"app"}`)})
	require.NoError(t, err)
	jettisonEntry, err := st.LastOrderLog(ctx, jettisoned, logOrder)
	require.NoError(t, err)
	missing := insertDoneOrder(t, st, app, landedRepo{Name: "app", Branch: "bmus/x", Commit: strings.Repeat("ab", 20)})
	parent, err := st.InsertOrder(ctx, store.Order{Title: "p", Division: "ops", State: store.OrderAway, Repos: app}, testEntry)
	require.NoError(t, err)
	children, err := st.InsertChildOrders(ctx, parent, "split:test", []store.Order{{Title: "c1", Division: "ops", Repos: app},
		{Title: "c2", Division: "ops", Repos: app}}, func([]string) (store.LogEntry, error) { return testEntry, nil })
	require.NoError(t, err)
	clone := r.d.repoDir("app")
	gitT(t, "", "init", "-q", "--bare", clone)
	gitT(t, clone, "fetch", "-q", up.url, "+refs/heads/*:refs/remotes/origin/*")
	gitT(t, clone, "update-ref", "refs/heads/"+orderBranch(done), commit)
	before, err := st.ListOrders(ctx)
	require.NoError(t, err)

	create := func(repo, base string, after ...string) (string, error) {
		var res OrderCreateResult
		err := call(t, home, "order.create", OrderCreateArgs{Title: "t", Division: "ops",
			Repos: []OrderRepo{{Name: repo, BaseRef: base}}, After: after}, &res)
		return res.ID, err
	}
	for _, tc := range []struct{ repo, base, msg string }{
		{"app", "order:ORD-0404", "repo app: base_ref order:ORD-0404: no such order"},
		{"app", "order:", `repo app: base_ref "order:": order: takes an order's id, such as order:ORD-0001`},
		{"app", "order:ord-0001", `repo app: base_ref "order:ord-0001": order: takes an order's id, such as order:ORD-0001`},
		{"app", "order:ORD-0001~1", `repo app: base_ref "order:ORD-0001~1": order: takes an order's id, such as order:ORD-0001`},
		{"lib", "order:" + done, "repo lib: base_ref order:" + done + ": " + done + " brought back no work on repo lib"},
		{"lib", "order:" + open, "repo lib: base_ref order:" + open + ": " + open + " doesn't work on repo lib"},
		{"app", "order:" + withdrawn, "repo app: base_ref order:" + withdrawn + ": it ended withdrawn and will never be done"},
		{"app", "order:" + split, "repo app: base_ref order:" + split + ": it was split, so its work is in its child orders; name one of them"},
		{"app", "order:" + noReport, "repo app: base_ref order:" + noReport + ": " + noReport + " brought back no work of its own"},
		{"app", "order:" + unfetched, "repo app: base_ref order:" + unfetched + ": " + unfetched + "'s work on repo app could not be fetched into the bridge's clone"},
		{"app", "order:" + jettisoned, "repo app: base_ref order:" + jettisoned + ": " + jettisoned + "'s branch was jettisoned on " +
			jettisonEntry.TS.UTC().Format(time.DateOnly) + "; its work may be gone"},
		{"app", "order:" + missing, "repo app: base_ref order:" + missing + ": " + missing + "'s commit abababababab is not in the bridge's clone of app"},
		{"app", "order:" + parent, "repo app: base_ref order:" + parent + ": " + parent + " was split into " +
			strings.Join(children, ", ") + ": base it on one of them"},
	} {
		_, err := create(tc.repo, tc.base)
		re := requireRPCError(t, err, CodeBadRequest)
		require.Equal(t, tc.msg, re.Message, tc.base)
	}
	after, err := st.ListOrders(ctx)
	require.NoError(t, err)
	require.Len(t, after, len(before), "nothing inserted for a refused order")

	onDone, err := create("app", "order:"+done)
	require.NoError(t, err)
	onOpen, err := create("app", "order:"+open, open, done)
	require.NoError(t, err)
	o, err := st.GetOrder(ctx, onDone)
	require.NoError(t, err)
	require.Equal(t, []string{done}, o.After)
	require.Empty(t, o.WaitingOn, "a done order is satisfied")
	require.Equal(t, []store.OrderRepo{{Name: "app", BaseRef: "order:" + done}}, o.Repos)
	o, err = st.GetOrder(ctx, onOpen)
	require.NoError(t, err)
	require.Equal(t, []string{done, open}, o.After, "the base is in after once")
	require.Equal(t, []string{open}, o.WaitingOn)
}

// bundleRepoFrom with a base_ref of order:<id> (#143) starts bmus/<order>
// at the commit order <id>'s done entry records, with no fetch: upstream
// may have moved or be gone. A named order that isn't done, a done entry
// without the repo and a commit the clone lacks are errRepoPermanent. A
// checkpoint is shipped before the base is looked at.
func TestBundleRepoFromOrderBase(t *testing.T) {
	t.Parallel()
	up := testRepo(t)
	d := testDaemon(t, map[string]string{"app": up.url})
	st, err := store.Open(t.Context(), filepath.Join(d.home, store.DBFile))
	require.NoError(t, err)
	t.Cleanup(func() { st.Close() })
	d.store = st
	ctx := t.Context()
	app := []store.OrderRepo{{Name: "app", BaseRef: "main"}}

	// ORD-0001 started from main and its work is in the clone.
	shipped := filepath.Join(t.TempDir(), "in.bundle")
	_, err = d.bundleRepo(ctx, "app", up.url, "ORD-0001", "main", shipped)
	require.NoError(t, err)
	work := filepath.Join(t.TempDir(), "work.bundle")
	_, commit := workBundle(t, shipped, "ORD-0001", work)
	require.NoError(t, d.fetchBundleLocked(ctx, "app", "ORD-0001", work, commit))
	first := insertDoneOrder(t, st, app, landedRepo{Name: "app", Branch: "bmus/ORD-0001", Commit: commit})
	require.Equal(t, "ORD-0001", first)
	up.commit(t, "three") // upstream moves on; nothing fetches it

	out := filepath.Join(t.TempDir(), "out.bundle")
	in := MissionInput{OrderID: "ORD-0002", CrewID: "sorell"}
	got, from, err := d.bundleRepoFrom(ctx, in, "app", "/nonexistent/upstream", "order:ORD-0001", out, nil)
	require.NoError(t, err)
	require.Nil(t, from)
	require.Equal(t, commit, got)
	require.Equal(t, commit+" refs/heads/bmus/ORD-0002", bundleHeads(t, out))
	clone := d.repoDir("app")
	require.Equal(t, commit, gitT(t, clone, "rev-parse", "bmus/ORD-0002"))
	require.Equal(t, up.rev(t, "HEAD~1"), gitT(t, clone, "rev-parse", "refs/remotes/origin/main"), "no fetch")

	open, err := st.InsertOrder(ctx, store.Order{Title: "open", Division: "ops", Repos: app}, testEntry)
	require.NoError(t, err)
	other := insertDoneOrder(t, st, app, landedRepo{Name: "lib", Branch: "bmus/x", Commit: commit})
	missing := insertDoneOrder(t, st, app, landedRepo{Name: "app", Branch: "bmus/x", Commit: strings.Repeat("ab", 20)})
	for base, msg := range map[string]string{
		"order:ORD-0404":   "base_ref order:ORD-0404: no such order",
		"order:" + open:    "base_ref order:" + open + ": " + open + " is pool, not done",
		"order:" + other:   "base_ref order:" + other + ": " + other + " brought back no work on repo app",
		"order:" + missing: "base_ref order:" + missing + ": " + missing + "'s commit abababababab is not in the bridge's clone of app",
	} {
		_, _, err := d.bundleRepoFrom(ctx, in, "app", up.url, base, out, nil)
		require.ErrorIs(t, err, errRepoPermanent, base)
		require.ErrorContains(t, err, msg)
	}

	// A resumed order ships its own checkpoint and never reads the base.
	cpWork := filepath.Join(t.TempDir(), "cp.bundle")
	_, cpCommit := workBundleFiles(t, out, "ORD-0002", cpWork, map[string]string{"work.txt": "more work\n"})
	cps := []store.Checkpoint{{OrderID: "ORD-0002", Beam: "b1", Seq: 1,
		Repos: []store.CheckpointRepo{{Name: "app", Commit: cpCommit, BundlePath: cpWork}}}}
	got, from, err = d.bundleRepoFrom(ctx, in, "app", up.url, "order:"+open, out, cps)
	require.NoError(t, err)
	require.NotNil(t, from)
	require.Equal(t, cpCommit, got)
	require.Equal(t, commit, gitT(t, clone, "rev-parse", "bmus/ORD-0002^"))
}

// An open base that ends done without usable work on the repo fails its
// dependent at ship with repo_failed, before anything is sent (#143): a
// done entry without the repo, and a commit the clone lacks.
func TestMissionFailsOnOrderBaseMissing(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		work func(base string) landedRepo
		why  func(base string) string
	}{
		"no work on the repo": {
			func(string) landedRepo {
				return landedRepo{Name: "lib", Branch: "bmus/x", Commit: strings.Repeat("ef", 20)}
			},
			func(base string) string { return base + " brought back no work on repo app" },
		},
		"commit missing": {
			func(base string) landedRepo {
				return landedRepo{Name: "app", Branch: orderBranch(base), Commit: strings.Repeat("cd", 20)}
			},
			func(base string) string { return base + "'s commit cdcdcdcdcdcd is not in the bridge's clone of app" },
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			home := privateHome(t)
			insertCrew(t, home, store.Crew{ID: "sorell", Name: "Sorell"})
			p := &fakeProvider{hello: true}
			r := startDaemonWith(t, home, p)
			defer r.stop(t)
			ctx := t.Context()
			waitOnDeck(t, r.d, "sorell", "fake-1")
			base, err := r.d.store.InsertOrder(ctx, store.Order{Title: "base", Division: "ops", State: store.OrderAway,
				Repos: []store.OrderRepo{{Name: "app", BaseRef: "main"}}}, testEntry)
			require.NoError(t, err)
			id, err := r.d.CreateOrder(ctx, store.Order{Title: "t", Assignee: "sorell",
				Repos: []store.OrderRepo{{Name: "app", BaseRef: "order:" + base}}})
			require.NoError(t, err)
			payload, err := json.Marshal(donePayload{Summary: "s", Repos: []landedRepo{tc.work(base)}})
			require.NoError(t, err)
			_, _, err = r.d.store.SettleOrder(ctx, base, store.OrderDone,
				store.LogEntry{Kind: logDone, OrderID: base, Text: "s", Payload: payload}, nil)
			require.NoError(t, err)
			r.d.wakeOnDeck(ctx, "for the test")
			require.EventuallyWithT(t, func(c *assert.CollectT) {
				require.Len(c, workflows(c, r.d, missionWorkflowID(id)), 1)
			}, 30*time.Second, 20*time.Millisecond)
			res := missionResult(t, r.d, id)
			require.Equal(t, MissionFailed, res.Status)
			require.Equal(t, CodeRepoFailed, res.Code)
			require.Contains(t, res.Reason, "could not ship the order's repos to the beam: repo app: base_ref order:"+base+": "+tc.why(base))
			_, sent := stepOutput(t, r.d, missionWorkflowID(id), "send-order")
			require.False(t, sent)
			require.Empty(t, p.copied())
		})
	}
}

// A split accepted for an order named as a base (#143) withdraws, in the
// same transaction, the unclaimed orders based on it, naming the
// children, and down the chain the orders waiting on those. A new order
// naming it is refused the same way. A split child of an order based on
// order:<id> ships that order's commit.
func TestSplitOrderBase(t *testing.T) {
	t.Parallel()
	home := privateHome(t)
	insertCrew(t, home, store.Crew{ID: "sorell", Name: "Sorell"})
	up := testRepo(t)
	p := &fakeProvider{hello: true, copyRoot: t.TempDir()}
	r := startDaemonRepos(t, home, p, map[string]string{"app": up.url})
	defer r.stop(t)
	ctx := t.Context()
	st := r.d.store
	waitOnDeck(t, r.d, "sorell", "fake-1")

	commit, err := r.d.bundleRepo(ctx, "app", up.url, "ORD-0001", "main", filepath.Join(t.TempDir(), "b.bundle"))
	require.NoError(t, err)
	named := insertDoneOrder(t, st, []store.OrderRepo{{Name: "app", BaseRef: "main"}},
		landedRepo{Name: "app", Branch: "bmus/ORD-0001", Commit: commit})
	require.Equal(t, "ORD-0001", named)
	onNamed := []store.OrderRepo{{Name: "app", BaseRef: "order:" + named}}
	parent, err := st.InsertOrder(ctx, store.Order{Title: "parent", Division: "ops", State: store.OrderAway, Repos: onNamed}, testEntry)
	require.NoError(t, err)
	dep, err := r.d.CreateOrder(ctx, store.Order{Title: "dep", Division: "ops",
		Repos: []store.OrderRepo{{Name: "app", BaseRef: "order:" + parent}}})
	require.NoError(t, err)
	chained, err := r.d.CreateOrder(ctx, store.Order{Title: "chained", Division: "ops", After: []string{dep},
		Repos: []store.OrderRepo{{Name: "app", BaseRef: "main"}}})
	require.NoError(t, err)

	children, err := st.InsertChildOrders(ctx, parent, "split:test", []store.Order{{Title: "child", Assignee: "sorell", Repos: onNamed}},
		func([]string) (store.LogEntry, error) { return testEntry, nil })
	require.NoError(t, err)
	why := parent + " was split into " + children[0] + ": base it on one of them"
	require.Equal(t, store.OrderWithdrawn, getOrder(t, r.d, dep).State)
	require.Equal(t, store.OrderWithdrawn, getOrder(t, r.d, chained).State, "down the chain")
	entry, err := st.LastOrderLog(ctx, dep, logOrder)
	require.NoError(t, err)
	require.Equal(t, "order withdrawn: "+why, entry.Text)
	_, err = r.d.CreateOrder(ctx, store.Order{Title: "late", Division: "ops", Repos: []store.OrderRepo{{Name: "app", BaseRef: "order:" + parent}}})
	requireCode(t, err, CodeBadRequest)
	require.EqualError(t, err, "repo app: base_ref order:"+parent+": "+why)

	r.d.wakeOnDeck(ctx, "for the test")
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		_, ok := stepOutput(c, r.d, missionWorkflowID(children[0]), "ship-repos")
		require.True(c, ok)
	}, 30*time.Second, 20*time.Millisecond)
	shipped, err := st.LastOrderLog(ctx, children[0], logOrder)
	require.NoError(t, err)
	entries, err := st.ListOrderLogKind(ctx, children[0], logOrder)
	require.NoError(t, err)
	var texts []string
	for _, e := range entries {
		if strings.HasPrefix(e.Text, "repo app shipped") {
			texts = append(texts, e.Text)
		}
	}
	require.Equal(t, []string{"repo app shipped to the beam at " + commit[:12] + " (order:" + named + ")"}, texts, shipped.Text)
	require.Equal(t, commit, gitT(t, r.d.repoDir("app"), "rev-parse", orderBranch(children[0])))
}

// bmus jettison keeps the branch of a done order an open order names as
// order:<id> (#143), and takes it once that order has ended.
func TestJettisonKeepsOrderBase(t *testing.T) {
	t.Parallel()
	home := privateHome(t)
	up := testRepo(t)
	cfg := testConfig(home)
	cfg.Repos = map[string]string{"app": up.url}
	r := startDaemonConfig(t, cfg)
	defer r.stop(t)
	d := r.d
	commit := up.rev(t, "main")
	app := []store.OrderRepo{{Name: "app", BaseRef: "main"}}
	named := insertDoneOrder(t, d.store, app, landedRepo{Name: "app", Branch: "bmus/ORD-0001", Commit: commit})
	plain := insertDoneOrder(t, d.store, app, landedRepo{Name: "app", Branch: "bmus/ORD-0002", Commit: commit})
	clone := d.repoDir("app")
	gitT(t, "", "init", "-q", "--bare", clone)
	gitT(t, clone, "fetch", "-q", up.url, "+refs/heads/*:refs/remotes/origin/*")
	for _, id := range []string{named, plain} {
		gitT(t, clone, "update-ref", "refs/heads/"+orderBranch(id), commit)
	}
	var dep OrderCreateResult
	require.NoError(t, call(t, home, "order.create", OrderCreateArgs{Title: "dependent", Division: "ops",
		Repos: []OrderRepo{{Name: "app", BaseRef: "order:" + named}}}, &dep))

	branches := func() []string {
		return strings.Fields(gitT(t, clone, "for-each-ref", "--format=%(refname:short)", "refs/heads/"))
	}
	var res JettisonResult
	require.NoError(t, call(t, home, "bridge.jettison", JettisonArgs{OlderThan: "0s", Yes: true}, &res))
	require.Len(t, res.Branches, 1)
	require.Equal(t, orderBranch(plain), res.Branches[0].Branch)
	require.Equal(t, 1, res.Kept, "the named order's branch")
	require.Equal(t, []string{orderBranch(named)}, branches())

	// With the dependent withdrawn, an open child order of an ended
	// parent still holds the base.
	require.NoError(t, call(t, home, "order.withdraw", OrderActionArgs{ID: dep.ID}, nil))
	ctx := t.Context()
	parent := insertDoneOrder(t, d.store, app)
	child, err := d.store.InsertOrder(ctx, store.Order{Title: "child", Division: "ops", ParentID: parent,
		Repos: []store.OrderRepo{{Name: "app", BaseRef: "order:" + named}}}, testEntry)
	require.NoError(t, err)
	require.NoError(t, call(t, home, "bridge.jettison", JettisonArgs{OlderThan: "0s", Yes: true}, &res))
	require.Empty(t, res.Branches)
	require.Equal(t, []string{orderBranch(named)}, branches())

	require.NoError(t, call(t, home, "order.withdraw", OrderActionArgs{ID: child}, nil))
	require.NoError(t, call(t, home, "bridge.jettison", JettisonArgs{OlderThan: "0s", Yes: true}, &res))
	require.Len(t, res.Branches, 1)
	require.Equal(t, orderBranch(named), res.Branches[0].Branch)
	require.Empty(t, branches())
}

// After jettison's delete, a branch an open order now names as its
// order:<id> base (created in the race with the delete) is created again
// at its old commit and logged (#143); the others stay deleted.
func TestJettisonRestoresBase(t *testing.T) {
	t.Parallel()
	home := privateHome(t)
	up := testRepo(t)
	cfg := testConfig(home)
	cfg.Repos = map[string]string{"app": up.url}
	r := startDaemonConfig(t, cfg)
	defer r.stop(t)
	d := r.d
	ctx := t.Context()
	commit := up.rev(t, "main")
	app := []store.OrderRepo{{Name: "app", BaseRef: "main"}}
	named := insertDoneOrder(t, d.store, app, landedRepo{Name: "app", Branch: "bmus/ORD-0001", Commit: commit})
	plain := insertDoneOrder(t, d.store, app, landedRepo{Name: "app", Branch: "bmus/ORD-0002", Commit: commit})
	clone := d.repoDir("app")
	gitT(t, "", "init", "-q", "--bare", clone)
	gitT(t, clone, "fetch", "-q", up.url, "+refs/heads/*:refs/remotes/origin/*")
	_, err := d.store.InsertOrder(ctx, store.Order{Title: "dep", Division: "ops",
		Repos: []store.OrderRepo{{Name: "app", BaseRef: "order:" + named}}}, testEntry)
	require.NoError(t, err)

	deleted := []JettisonBranch{
		{Repo: "app", Order: named, Branch: orderBranch(named), Commit: commit},
		{Repo: "app", Order: plain, Branch: orderBranch(plain), Commit: commit},
	}
	orders := map[string]store.Order{named: getOrder(t, d, named), plain: getOrder(t, d, plain)}
	gone, restored, err := d.restoreBases(ctx, "app", clone, deleted, orders)
	require.NoError(t, err)
	require.Equal(t, 1, restored)
	require.Equal(t, deleted[1:], gone)
	require.Equal(t, commit, gitT(t, clone, "rev-parse", orderBranch(named)))
	entry, err := d.store.LastOrderLog(ctx, named, logOrder)
	require.NoError(t, err)
	require.Equal(t, "branch "+orderBranch(named)+" restored in repo app: an open order names it as its base", entry.Text)
}

// #143's acceptance on holodeck: an order based on order:ORD-0001 waits
// for ORD-0001, then its bundle starts from the commit ORD-0001's done
// entry recorded, and its bmus/<order> has that commit as the parent of
// its own work. Its mission report's base is that commit, so its diff
// holds only its own work.
func TestOrderBaseRunsOnWork(t *testing.T) {
	t.Parallel()
	b := newHolodeckBridge(t, fixture(t, "work.json"))
	hd := b.start(t)
	hd.addCrew(t)
	first := hd.createOrder(t)
	second, err := hd.d.CreateOrder(t.Context(), store.Order{Title: "follow up", Body: "build on it", Assignee: "sorell",
		Priority: store.PriorityRed, Repos: []store.OrderRepo{{Name: "app", BaseRef: "order:" + first}}})
	require.NoError(t, err)
	require.Equal(t, []string{first}, hd.order(t, second).After)

	missionDone(t, hd.d, first)
	missionDone(t, hd.d, second)
	firstDone, err := hd.d.store.LastOrderLog(t.Context(), first, logDone)
	require.NoError(t, err)
	var p donePayload
	require.NoError(t, json.Unmarshal(firstDone.Payload, &p))
	require.Len(t, p.Repos, 1)
	base := p.Repos[0].Commit

	entries := hd.logEntries(t)
	done, claimed := -1, -1
	var shipped []string
	for i, e := range entries {
		if e.OrderID == first && e.Kind == logDone && done < 0 {
			done = i
		}
		if e.OrderID == second && e.Text == "order away: mission started" && claimed < 0 {
			claimed = i
		}
		if e.OrderID == second && strings.HasPrefix(e.Text, "repo app shipped") {
			shipped = append(shipped, e.Text)
		}
	}
	require.Positive(t, done)
	require.Greater(t, claimed, done, "the dependent is claimed after the base order is done")
	require.Equal(t, []string{"repo app shipped to the beam at " + base[:12] + " (order:" + first + ")"}, shipped)

	clone := filepath.Join(b.home, "repos", "app")
	steps := stepLogEntries(t, b.stepLog)
	var commits []string
	for _, s := range steps {
		cs, _ := s["commits"].(map[string]any)
		if c, ok := cs["app"].(string); ok && s["order"] == second && !slices.Contains(commits, c) {
			commits = append(commits, c)
		}
	}
	require.NotEmpty(t, commits)
	require.Equal(t, base, gitT(t, clone, "rev-parse", commits[0]+"^"), "the dependent's first commit sits on the base's work")
	require.Equal(t, base, gitT(t, clone, "rev-parse", "bmus/"+second+"~2"))
	require.Equal(t, base, gitT(t, clone, "rev-parse", "bmus/"+first))

	rep, err := hd.d.OrderReport(t.Context(), second, consoleDiffCap)
	require.NoError(t, err)
	require.Len(t, rep.Repos, 1)
	require.Equal(t, base, rep.Repos[0].Base)
	require.Contains(t, rep.Repos[0].Diff, "+"+second+" step 1: first change")
	require.NotContains(t, rep.Repos[0].Diff, "+"+first+" step", "only the dependent's own work")
	requireNoBackstop(t, entries)
}
