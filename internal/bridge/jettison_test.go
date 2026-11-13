package bridge

import (
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
// and leaves every other ref: open and recent orders' branches, a branch
// an open order names as its base_ref, a branch of an order the store
// doesn't have, other branches, remote-tracking refs and tags. The
// jettisoned order's report then marks its repos jettisoned, with no
// error.
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
	asBase := insert(store.OrderWithdrawn, 30*day, app)
	insert(store.OrderPool, 0, store.OrderRepo{Name: "app", BaseRef: orderBranch(asBase)})

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
	for _, id := range []string{oldFailed, recent, open, asBase, "ORD-9999"} {
		gitT(t, clone, "update-ref", "refs/heads/"+orderBranch(id), base)
	}
	gitT(t, clone, "update-ref", "refs/heads/bmus/feature/x", base)
	gitT(t, clone, "update-ref", "refs/remotes/origin/"+orderBranch(oldFailed), base)
	require.NoError(t, os.MkdirAll(filepath.Join(home, "repos", "junk"), 0o700))
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
		require.Equal(t, 3, res.Kept, "recent, open and asBase")
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
}
