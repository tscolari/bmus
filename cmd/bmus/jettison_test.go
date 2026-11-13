package main

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/tscolari/bmus/internal/bridge"
)

// #108's acceptance through the CLI: the dry run lists only an ended
// order's branch and deletes nothing, `bmus purge` says the same as `bmus
// jettison`, --older-than keeps a recently ended order's branch, and
// --yes deletes exactly the listed branch.
func TestJettisonCLI(t *testing.T) {
	t.Parallel()
	e := newE2E(t)
	e.startBridge(t)
	pool := filepath.Join(t.TempDir(), "pool.yaml")
	require.NoError(t, os.WriteFile(pool, []byte("title: pool\ndivision: ops\nrepos:\n  - name: app\n    base_ref: main\n"), 0o600))
	require.Equal(t, "ORD-0001\n", e.run(t, "order", "create", pool))
	require.Equal(t, "ORD-0002\n", e.run(t, "order", "create", pool))
	e.run(t, "order", "withdraw", "ORD-0001")

	clone := filepath.Join(e.home, "repos", "app")
	gitT(t, "", "init", "-q", "--bare", clone)
	gitT(t, clone, "fetch", "-q", e.repo, "+refs/heads/main:refs/heads/main")
	for _, b := range []string{"bmus/ORD-0001", "bmus/ORD-0002", "bmus/ORD-9999"} {
		gitT(t, clone, "branch", b, "main")
	}
	branches := func() string { return gitT(t, clone, "for-each-ref", "--format=%(refname:short)", "refs/heads/") }
	all := branches()

	dry := e.run(t, "jettison", "--older-than", "0s")
	require.Regexp(t, `^REPO +ORDER +BRANCH +ENDED\napp +ORD-0001 +bmus/ORD-0001 +\d{4}-\d\d-\d\dT[0-9:]+Z\n`+
		regexp.QuoteMeta("would jettison 1 branch of 1 order that ended over 0s ago; 1 branch kept; run with --yes to delete them")+"\n$", dry)
	require.Equal(t, dry, e.run(t, "purge", "--older-than", "0s"))
	require.Equal(t, all, branches(), "a dry run deletes nothing")
	require.Equal(t, "nothing to jettison: no order ended over 7d ago has a branch; 2 branches kept\n", e.run(t, "purge"))
	require.Contains(t, e.fail(t, "jettison", "--older-than", "soon"), `--older-than: age "soon"`)

	var res bridge.JettisonResult
	e.runJSON(t, &res, "purge", "--older-than", "0s", "--yes", "--json")
	require.True(t, res.Deleted)
	require.Len(t, res.Branches, 1)
	require.Equal(t, "bmus/ORD-0001", res.Branches[0].Branch)
	require.WithinDuration(t, time.Now(), res.Branches[0].EndedAt, time.Minute)
	require.Equal(t, "bmus/ORD-0002\nbmus/ORD-9999\nmain", branches())
	require.Equal(t, "nothing to jettison: no order ended over 0s ago has a branch; 1 branch kept\n", e.run(t, "jettison", "--older-than", "0s", "--yes"))
}

func TestJettisonSummary(t *testing.T) {
	t.Parallel()
	b := []bridge.JettisonBranch{{Repo: "app", Order: "ORD-0001"}, {Repo: "lib", Order: "ORD-0001"}, {Repo: "app", Order: "ORD-0002"}}
	require.Equal(t, "jettisoned 3 branches of 2 orders that ended over 7d ago; 1 branch kept",
		jettisonSummary(bridge.JettisonResult{OlderThan: "7d", Deleted: true, Branches: b, Kept: 1}))
	require.Equal(t, "would jettison 3 branches of 2 orders that ended over 36h ago; 0 branches kept; run with --yes to delete them",
		jettisonSummary(bridge.JettisonResult{OlderThan: "36h", Branches: b}))
}
