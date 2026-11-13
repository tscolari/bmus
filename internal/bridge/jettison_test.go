package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/tscolari/bmus/internal/store"
)

func TestParseAge(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]time.Duration{
		"7d": 7 * 24 * time.Hour, "0d": 0, "36h": 36 * time.Hour, "90m": 90 * time.Minute, "0s": 0, "1h30m": 90 * time.Minute,
	} {
		got, err := ParseAge(in)
		require.NoError(t, err, in)
		require.Equal(t, want, got, in)
	}
	for _, bad := range []string{"", "d", "-1d", "1.5d", "7days", "-3h", "week", "999999999999d"} {
		_, err := ParseAge(bad)
		require.Error(t, err, bad)
	}
}

// cloneRefs lists every ref of the clone gitDir with its commit.
func cloneRefs(t *testing.T, gitDir string) []string {
	t.Helper()
	return strings.Split(gitT(t, gitDir, "for-each-ref", "--format=%(objectname) %(refname)"), "\n")
}

// without is refs less the ones named.
func without(all []string, names ...string) []string {
	var out []string
	for _, r := range all {
		_, name, _ := strings.Cut(r, " ")
		keep := true
		for _, n := range names {
			if name == n {
				keep = false
			}
		}
		if keep {
			out = append(out, r)
		}
	}
	return out
}

// #108's acceptance: with ended, open and recently ended orders, the dry
// run lists only the branches of orders that ended long enough ago, in
// every repo they used, and deletes nothing; --yes deletes exactly those
// and leaves every other ref: open and recent orders' branches, branches
// open orders' base_refs name in any spelling, a branch of an order the
// store doesn't have, other branches, remote-tracking refs and tags. A
// clone git can't read is skipped and reported. The jettisoned order's
// report then marks its repos jettisoned with no error, its push says it
// was jettisoned, and a branch missing without a jettison, or a missing
// clone, is still the report's 400.
func TestJettison(t *testing.T) {
	t.Parallel()
	home := privateHome(t)
	up := testRepo(t)
	cfg := testConfig(home)
	cfg.Repos = map[string]string{"app": up.url, "lib": up.url}
	r := startDaemonConfig(t, cfg)
	defer r.stop(t)
	d := r.d
	ctx := t.Context()

	now := time.Now()
	day := 24 * time.Hour
	insert := func(state string, ended time.Duration, repos ...store.OrderRepo) string {
		t.Helper()
		id, err := d.store.InsertOrder(ctx, store.Order{Title: "t", Division: "ops", State: state, Repos: repos},
			store.LogEntry{Kind: logOrder, Text: "order created: t", TS: now.Add(-ended)})
		require.NoError(t, err)
		return id
	}
	app, lib := store.OrderRepo{Name: "app", BaseRef: "main"}, store.OrderRepo{Name: "lib", BaseRef: "main"}
	oldDone := insert(store.OrderDone, 30*day, app, lib)
	oldFailed := insert(store.OrderFailed, 10*day, app)
	recent := insert(store.OrderBelayed, day, app)
	open := insert(store.OrderPool, 30*day, app)
	var asBase []string
	for _, spell := range []string{"%s", "%s~1", "refs/heads/%s", "heads/%s^{commit}", "%s@{1}"} {
		id := insert(store.OrderWithdrawn, 30*day, app)
		asBase = append(asBase, id)
		insert(store.OrderPool, 0, store.OrderRepo{Name: "app", BaseRef: fmt.Sprintf(spell, orderBranch(id))})
	}
	_, err := d.store.AppendLog(ctx, store.LogEntry{Kind: logDone, OrderID: oldDone, Text: "done", TS: now.Add(-30 * day),
		Payload: json.RawMessage(fmt.Sprintf(`{"summary":"s","repos":[{"name":"app","branch":%q,"commit":%q}]}`, orderBranch(oldDone), up.rev(t, "main")))})
	require.NoError(t, err)
	never := insert(store.OrderDone, 30*day, app)

	// The clones, as ship-repos leaves them, plus refs jettison must not
	// touch, and a directory that isn't a clone.
	base := up.rev(t, "main")
	for _, name := range []string{"app", "lib"} {
		dir := d.repoDir(name)
		gitT(t, "", "init", "-q", "--bare", dir)
		gitT(t, dir, "fetch", "-q", up.url, "+refs/heads/*:refs/remotes/origin/*")
		gitT(t, dir, "update-ref", "refs/heads/"+orderBranch(oldDone), base)
		gitT(t, dir, "update-ref", "refs/heads/main", base)
		gitT(t, dir, "tag", "v1", base)
	}
	clone := d.repoDir("app")
	for _, id := range append([]string{oldFailed, recent, open, "ORD-9999"}, asBase...) {
		gitT(t, clone, "update-ref", "refs/heads/"+orderBranch(id), base)
	}
	gitT(t, clone, "update-ref", "refs/heads/bmus/feature/x", base)
	gitT(t, clone, "update-ref", "refs/remotes/origin/"+orderBranch(oldFailed), base)
	require.NoError(t, os.MkdirAll(filepath.Join(home, "repos", "junk"), 0o700))
	require.NoError(t, os.MkdirAll(filepath.Join(home, "repos", "broken"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(home, "repos", "broken", "HEAD"), []byte("not a ref\n"), 0o600))
	appRefs, libRefs := cloneRefs(t, clone), cloneRefs(t, d.repoDir("lib"))

	want := []JettisonBranch{
		{Repo: "app", Order: oldDone, Branch: orderBranch(oldDone), State: store.OrderDone, Commit: base},
		{Repo: "app", Order: oldFailed, Branch: orderBranch(oldFailed), State: store.OrderFailed, Commit: base},
		{Repo: "lib", Order: oldDone, Branch: orderBranch(oldDone), State: store.OrderDone, Commit: base},
	}
	check := func(res JettisonResult, deleted bool) {
		t.Helper()
		require.Equal(t, deleted, res.Deleted)
		require.Equal(t, DefaultJettisonAge, res.OlderThan)
		require.Equal(t, 2+len(asBase), res.Kept, "recent, open and asBase")
		require.Len(t, res.Skipped, 1)
		require.Equal(t, "broken", res.Skipped[0].Repo)
		require.Empty(t, res.Skipped[0].Branch)
		require.Len(t, res.Branches, len(want))
		for i, b := range res.Branches {
			require.False(t, b.EndedAt.After(now.Add(-7*day)), b.Order)
			b.EndedAt = time.Time{}
			require.Equal(t, want[i], b)
		}
	}

	var res JettisonResult
	require.NoError(t, call(t, home, "bridge.jettison", JettisonArgs{}, &res))
	check(res, false)
	require.Equal(t, appRefs, cloneRefs(t, clone), "a dry run deletes nothing")
	require.Equal(t, libRefs, cloneRefs(t, d.repoDir("lib")))

	requireRPCError(t, call(t, home, "bridge.jettison", JettisonArgs{OlderThan: "soon"}, nil), CodeBadRequest)

	require.NoError(t, call(t, home, "bridge.jettison", JettisonArgs{OlderThan: "7d", Yes: true}, &res))
	check(res, true)
	require.Equal(t, without(appRefs, "refs/heads/"+orderBranch(oldDone), "refs/heads/"+orderBranch(oldFailed)), cloneRefs(t, clone))
	require.Equal(t, without(libRefs, "refs/heads/"+orderBranch(oldDone)), cloneRefs(t, d.repoDir("lib")))
	entry, err := d.store.LastOrderLog(ctx, oldDone, logOrder)
	require.NoError(t, err)
	require.Equal(t, "branch "+orderBranch(oldDone)+" jettisoned from repo lib", entry.Text)

	require.NoError(t, call(t, home, "bridge.jettison", JettisonArgs{OlderThan: "0s"}, &res))
	require.Equal(t, []JettisonBranch{{Repo: "app", Order: recent, Branch: orderBranch(recent), State: store.OrderBelayed,
		EndedAt: res.Branches[0].EndedAt, Commit: base}}, res.Branches, "a shorter age takes the recent order's")

	got, err := d.OrderReport(ctx, oldDone, consoleDiffCap)
	require.NoError(t, err)
	require.Equal(t, []RepoDiff{{Name: "app", Jettisoned: true}, {Name: "lib", Jettisoned: true}}, got.Repos)

	_, err = d.PushOrder(ctx, oldDone, "", false)
	requireCode(t, err, CodeBadRequest)
	require.EqualError(t, err, fmt.Sprintf("%s was jettisoned from repo app on %s; its work is no longer on the bridge",
		orderBranch(oldDone), entry.TS.UTC().Format(time.DateOnly)))

	_, err = d.OrderReport(ctx, never, consoleDiffCap)
	requireCode(t, err, CodeBadRequest)
	require.EqualError(t, err, "repo app: "+orderBranch(never)+" is missing from the bridge's clone", "missing without a jettison")

	require.NoError(t, os.Rename(d.repoDir("lib"), d.repoDir("lib")+".moved"))
	_, err = d.OrderReport(ctx, oldDone, consoleDiffCap)
	requireCode(t, err, CodeBadRequest)
	require.EqualError(t, err, "repo lib: "+orderBranch(oldDone)+" is missing from the bridge's clone", "a missing clone is no jettison")
}

// requireCode requires err to carry RPC code.
func requireCode(t *testing.T, err error, code string) {
	t.Helper()
	require.Error(t, err)
	require.Equal(t, code, errorCode(t.Context(), err), "%v", err)
}

// A branch that moved after it was listed is skipped, the others in its
// repo are deleted in one transaction, and a branch an open order's
// base_ref names, created after the listing, is kept: jettisonRepo checks
// both again under the lock.
func TestJettisonRepoRechecks(t *testing.T) {
	t.Parallel()
	home := privateHome(t)
	up := testRepo(t)
	cfg := testConfig(home)
	cfg.Repos = map[string]string{"app": up.url}
	r := startDaemonConfig(t, cfg)
	defer r.stop(t)
	d := r.d
	ctx := t.Context()

	insert := func(state string, repos ...store.OrderRepo) string {
		t.Helper()
		id, err := d.store.InsertOrder(ctx, store.Order{Title: "t", Division: "ops", State: state, Repos: repos},
			store.LogEntry{Kind: logOrder, Text: "order created: t"})
		require.NoError(t, err)
		return id
	}
	gone, moved, based := insert(store.OrderDone), insert(store.OrderFailed), insert(store.OrderBelayed)
	dir := d.repoDir("app")
	gitT(t, "", "init", "-q", "--bare", dir)
	gitT(t, dir, "fetch", "-q", up.url, "+refs/heads/main:refs/heads/main")
	first := gitT(t, dir, "rev-parse", "main~1")
	head := gitT(t, dir, "rev-parse", "main")
	for _, id := range []string{gone, moved, based} {
		gitT(t, dir, "update-ref", "refs/heads/"+orderBranch(id), first)
	}
	batch := []JettisonBranch{
		{Repo: "app", Order: gone, Branch: orderBranch(gone), Commit: first},
		{Repo: "app", Order: moved, Branch: orderBranch(moved), Commit: first},
		{Repo: "app", Order: based, Branch: orderBranch(based), Commit: first},
	}
	// After the listing: one branch moves, and an open order names another.
	gitT(t, dir, "update-ref", "refs/heads/"+orderBranch(moved), head)
	insert(store.OrderPool, store.OrderRepo{Name: "app", BaseRef: orderBranch(based) + "^"})
	orders := map[string]store.Order{}
	all, err := d.store.ListOrders(ctx)
	require.NoError(t, err)
	for _, o := range all {
		orders[o.ID] = o
	}

	deleted, skipped, kept, err := d.jettisonRepo(ctx, "app", batch, orders, time.Minute)
	require.NoError(t, err)
	require.Equal(t, batch[:1], deleted)
	require.Equal(t, []JettisonSkip{{Repo: "app", Order: moved, Branch: orderBranch(moved), Reason: "moved from " + first[:12] + " since it was listed"}}, skipped)
	require.Equal(t, 1, kept)
	require.Equal(t, head, gitT(t, dir, "rev-parse", orderBranch(moved)))
	require.Equal(t, first, gitT(t, dir, "rev-parse", orderBranch(based)))
	_, err = runGit(ctx, dir, "rev-parse", "--verify", "--quiet", "refs/heads/"+orderBranch(gone))
	require.True(t, refMissing(err))
	_, ok, err := d.jettisonedAt(ctx, gone, "app")
	require.NoError(t, err)
	require.True(t, ok)
	_, ok, err = d.jettisonedAt(ctx, moved, "app")
	require.NoError(t, err)
	require.False(t, ok)
}

// A repo whose lock stays busy past the wait is skipped, its branches
// left as they are; the bridge stopping meanwhile is an error.
func TestJettisonRepoBusy(t *testing.T) {
	t.Parallel()
	r := startDaemonConfig(t, testConfig(privateHome(t)))
	defer r.stop(t)
	d := r.d
	unlock, err := d.lockRepo(t.Context(), "app")
	require.NoError(t, err)
	defer unlock()
	batch := []JettisonBranch{{Repo: "app", Order: "ORD-0001", Branch: "bmus/ORD-0001", Commit: "c"}}
	deleted, skipped, kept, err := d.jettisonRepo(t.Context(), "app", batch, nil, 50*time.Millisecond)
	require.NoError(t, err)
	require.Empty(t, deleted)
	require.Zero(t, kept)
	require.Equal(t, []JettisonSkip{{Repo: "app", Order: "ORD-0001", Branch: "bmus/ORD-0001", Reason: "the repo's clone stayed busy for 50ms"}}, skipped)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, _, _, err = d.jettisonRepo(ctx, "app", batch, nil, time.Minute)
	require.ErrorIs(t, err, context.Canceled)
}
