package bridge

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dbos-inc/dbos-transact-golang/dbos"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tscolari/bmus/internal/comms"
	"github.com/tscolari/bmus/internal/store"
)

// workBundle plays the crew: it clones shipped (a bundle of bmus/<order>,
// as ship-repos builds it), commits on bmus/<order> and writes a bundle of
// the branch minus its base to out, as combadge does. It returns the base
// and the new commit.
func workBundle(t *testing.T, shipped, orderID, out string) (base, commit string) {
	t.Helper()
	return workBundleFiles(t, shipped, orderID, out, map[string]string{"work.txt": "work\n"})
}

// workBundleFiles is workBundle, committing files (name to content).
func workBundleFiles(t *testing.T, shipped, orderID, out string, files map[string]string) (base, commit string) {
	t.Helper()
	clone := filepath.Join(t.TempDir(), "c")
	gitT(t, filepath.Dir(clone), "clone", "-q", "--branch", orderBranch(orderID), shipped, clone)
	base = gitT(t, clone, "rev-parse", "HEAD")
	for name, content := range files {
		require.NoError(t, os.WriteFile(filepath.Join(clone, name), []byte(content), 0o600))
		gitT(t, clone, "add", name)
	}
	gitT(t, clone, "commit", "-q", "-m", "work")
	commit = gitT(t, clone, "rev-parse", "HEAD")
	require.NoError(t, os.MkdirAll(filepath.Dir(out), 0o700))
	gitT(t, clone, "bundle", "create", "-q", out, "refs/heads/"+orderBranch(orderID), "^"+base)
	return base, commit
}

// refs lists every ref of the repo at dir with its object.
func refs(t *testing.T, dir string) string {
	t.Helper()
	return gitT(t, dir, "for-each-ref", "--format=%(objectname) %(refname)")
}

func TestMatchWork(t *testing.T) {
	t.Parallel()
	repos := []store.OrderRepo{{Name: "app", BaseRef: "main"}, {Name: "lib", BaseRef: "main"}}
	w := func(name string) comms.WorkRepo {
		return comms.WorkRepo{Name: name, BundlePath: "/w/ORD-0001/out/" + name + ".bundle", Commit: "abc", Branch: "bmus/ORD-0001"}
	}
	got, err := matchWork("ORD-0001", repos, []comms.WorkRepo{w("lib"), w("app")})
	require.NoError(t, err)
	require.Equal(t, []comms.WorkRepo{w("app"), w("lib")}, got, "in the order's repo order")
	got, err = matchWork("ORD-0001", nil, nil)
	require.NoError(t, err)
	require.Empty(t, got)

	with := func(name string, fn func(*comms.WorkRepo)) comms.WorkRepo {
		r := w(name)
		fn(&r)
		return r
	}
	for name, work := range map[string][]comms.WorkRepo{
		"missing repo":       {w("app")},
		"none":               nil,
		"unknown repo":       {w("app"), w("lib"), w("web")},
		"twice":              {w("app"), w("app"), w("lib")},
		"another branch":     {w("app"), with("lib", func(r *comms.WorkRepo) { r.Branch = "main" })},
		"no commit":          {w("app"), with("lib", func(r *comms.WorkRepo) { r.Commit = "" })},
		"option commit":      {w("app"), with("lib", func(r *comms.WorkRepo) { r.Commit = "--all" })},
		"relative path":      {w("app"), with("lib", func(r *comms.WorkRepo) { r.BundlePath = "out/lib.bundle" })},
		"path escaping name": {w("app"), with("../lib", func(*comms.WorkRepo) {})},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := matchWork("ORD-0001", repos, work)
			require.ErrorIs(t, err, errWorkPermanent)
		})
	}
}

// fetchWork lands a bundle on bmus/<order> in the bridge's clone, touching
// no remote; a re-run is a no-op. A bundle that doesn't hold the reported
// commit is a FetchError, not a retry; a missing bundle is retried.
func TestFetchWork(t *testing.T) {
	t.Parallel()
	up := testRepo(t)
	upRefs := refs(t, up.url)
	d := testDaemon(t, map[string]string{"app": up.url})
	ctx := t.Context()
	shipped := filepath.Join(t.TempDir(), "in.bundle")
	_, err := d.bundleRepo(ctx, "app", up.url, "ORD-0001", "main", shipped)
	require.NoError(t, err)
	bundle := filepath.Join(d.home, "cargo", "ORD-0001", "final", "app.bundle")
	base, commit := workBundle(t, shipped, "ORD-0001", bundle)
	require.Equal(t, up.rev(t, "main"), base)
	clone := d.repoDir("app")
	require.Equal(t, base, gitT(t, clone, "rev-parse", "bmus/ORD-0001"))

	landed := []landedRepo{{Name: "app", Branch: "bmus/ORD-0001", Commit: commit, Bundle: bundle}}
	for range 2 {
		got, err := fetchWork(ctx, d, "ORD-0001", landed)
		require.NoError(t, err)
		require.Equal(t, landed, got)
		require.Equal(t, commit, gitT(t, clone, "rev-parse", "bmus/ORD-0001"))
	}
	require.Equal(t, upRefs, refs(t, up.url), "the upstream is untouched")

	// A diverging commit replaces the branch (non-fast-forward: the forced
	// refspec), and a tag in the bundle doesn't come along (--no-tags).
	other := filepath.Join(t.TempDir(), "o")
	gitT(t, filepath.Dir(other), "clone", "-q", "--branch", "bmus/ORD-0001", shipped, other)
	require.NoError(t, os.WriteFile(filepath.Join(other, "other.txt"), []byte("other\n"), 0o600))
	gitT(t, other, "add", "other.txt")
	gitT(t, other, "commit", "-q", "-m", "other")
	gitT(t, other, "tag", "beam-tag")
	diverged := gitT(t, other, "rev-parse", "HEAD")
	otherBundle := filepath.Join(t.TempDir(), "other.bundle")
	gitT(t, other, "bundle", "create", "-q", otherBundle, "refs/heads/bmus/ORD-0001", "refs/tags/beam-tag", "^"+base)
	got, err := fetchWork(ctx, d, "ORD-0001", []landedRepo{{Name: "app", Branch: "bmus/ORD-0001", Commit: diverged, Bundle: otherBundle}})
	require.NoError(t, err)
	require.Empty(t, got[0].FetchError)
	require.Equal(t, diverged, gitT(t, clone, "rev-parse", "bmus/ORD-0001"))
	require.Empty(t, gitT(t, clone, "tag", "-l", "beam-tag"))
	require.Equal(t, upRefs, refs(t, up.url))
}

// A bundle that doesn't hold the reported commit, isn't a bundle, or needs
// commits the clone lacks is a FetchError, not a retry, and leaves the
// branch where it was; a missing bundle is retried.
func TestFetchWorkRefuses(t *testing.T) {
	t.Parallel()
	up := testRepo(t)
	d := testDaemon(t, map[string]string{"app": up.url})
	ctx := t.Context()
	shipped := filepath.Join(t.TempDir(), "in.bundle")
	_, err := d.bundleRepo(ctx, "app", up.url, "ORD-0002", "main", shipped)
	require.NoError(t, err)
	bundle := filepath.Join(t.TempDir(), "app.bundle")
	base, commit := workBundle(t, shipped, "ORD-0002", bundle)
	clone := d.repoDir("app")
	require.Equal(t, base, gitT(t, clone, "rev-parse", "bmus/ORD-0002"))

	// A bundle the clone can't use: its prerequisite is a commit the clone
	// has never seen.
	stranger := filepath.Join(t.TempDir(), "s")
	gitT(t, filepath.Dir(stranger), "init", "-q", "-b", "bmus/ORD-0002", stranger)
	gitT(t, stranger, "commit", "-q", "--allow-empty", "-m", "a")
	strangerBase := gitT(t, stranger, "rev-parse", "HEAD")
	gitT(t, stranger, "commit", "-q", "--allow-empty", "-m", "b")
	strangerTip := gitT(t, stranger, "rev-parse", "HEAD")
	lacking := filepath.Join(t.TempDir(), "lacking.bundle")
	gitT(t, stranger, "bundle", "create", "-q", lacking, "refs/heads/bmus/ORD-0002", "^"+strangerBase)
	junk := filepath.Join(t.TempDir(), "junk.bundle")
	require.NoError(t, os.WriteFile(junk, []byte("not a bundle\n"), 0o600))

	for name, tc := range map[string]struct {
		bundle, commit, want string
		rejected             bool
	}{
		"another commit": {bundle, base, "not the reported " + base, false},
		"not a bundle":   {junk, commit, "does not look like a v2 or v3 bundle file", false},
		// #65's object check runs before the real fetch, but only its
		// fsck-specific failures are rejected; index-pack --fix-thin
		// failing to resolve the stranger repo's missing base commit is
		// an ordinary "bundle lacks a prerequisite" condition, classified
		// the same as if the real fetch's own "lacks these prerequisite
		// commits" check had caught it.
		"missing prerequisites": {lacking, strangerTip, "did not receive expected object", false},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := fetchWork(ctx, d, "ORD-0002", []landedRepo{{Name: "app", Branch: "bmus/ORD-0002", Commit: tc.commit, Bundle: tc.bundle}})
			require.NoError(t, err)
			require.Len(t, got, 1)
			if tc.rejected {
				require.Contains(t, got[0].RejectError, tc.want)
				require.Empty(t, got[0].FetchError)
				require.Contains(t, workRejections(got), "repo app: ")
			} else {
				require.Contains(t, got[0].FetchError, tc.want)
				require.NotContains(t, got[0].FetchError, "\n")
				require.Contains(t, fetchFailures(got), "repo app: ")
			}
			require.Equal(t, base, gitT(t, clone, "rev-parse", "bmus/ORD-0002"), "left at the base")
		})
	}

	gone := filepath.Join(t.TempDir(), "gone.bundle")
	_, err = fetchWork(ctx, d, "ORD-0002", []landedRepo{{Name: "app", Branch: "bmus/ORD-0002", Commit: commit, Bundle: gone}})
	require.Error(t, err)
	require.False(t, errors.Is(err, errWorkPermanent))
	require.Equal(t, base, gitT(t, clone, "rev-parse", "bmus/ORD-0002"))
}

// gitTStdin is gitT, feeding stdin to git (git mktree, commit-tree).
func gitTStdin(t require.TestingT, dir, stdin string, args ...string) string {
	cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@example.com",
		"-c", "commit.gpgsign=false", "-c", "tag.gpgsign=false"}, args...)...)
	cmd.Dir = dir
	cmd.Stdin = strings.NewReader(stdin)
	out, err := cmd.Output()
	var stderr []byte
	if ee := (*exec.ExitError)(nil); errors.As(err, &ee) {
		stderr = ee.Stderr
	}
	require.NoError(t, err, "git %v: %s", args, stderr)
	return strings.TrimSpace(string(out))
}

// #65: a tree with a .GIT entry below the tip, or a malformed commit,
// fails fetch-work's object check (verifyBundleObjects, work.go) and is
// rejected: RejectError is set (not FetchError), and bmus/<order> in the
// bridge's clone never moves. Both bundles are built with raw plumbing,
// bypassing the checks git's own write path (hash-object, commit) would
// otherwise apply.
func TestFetchWorkRejectsBadObjects(t *testing.T) {
	t.Parallel()
	up := testRepo(t)
	d := testDaemon(t, map[string]string{"app": up.url})
	ctx := t.Context()
	shipped := filepath.Join(t.TempDir(), "in.bundle")
	_, err := d.bundleRepo(ctx, "app", up.url, "ORD-0003", "main", shipped)
	require.NoError(t, err)
	clone := d.repoDir("app")
	shipBase := gitT(t, clone, "rev-parse", "bmus/ORD-0003")

	t.Run("dotgit tree entry", func(t *testing.T) {
		work := filepath.Join(t.TempDir(), "w")
		gitT(t, filepath.Dir(work), "clone", "-q", "--branch", orderBranch("ORD-0003"), shipped, work)
		base := gitT(t, work, "rev-parse", "HEAD")
		evilFile := filepath.Join(t.TempDir(), "evil.txt")
		require.NoError(t, os.WriteFile(evilFile, []byte("evil\n"), 0o600))
		blob := gitT(t, work, "hash-object", "-w", evilFile)
		headTree := gitT(t, work, "rev-parse", "HEAD^{tree}")
		tree := gitTStdin(t, work, fmt.Sprintf("100644 blob %s\tfile.txt\n040000 tree %s\t.GIT\n", blob, headTree), "mktree")
		commit := gitTStdin(t, work, "evil\n", "commit-tree", "-p", base, tree)
		gitT(t, work, "update-ref", "refs/heads/"+orderBranch("ORD-0003"), commit)
		out := filepath.Join(t.TempDir(), "dotgit.bundle")
		gitT(t, work, "bundle", "create", "-q", out, "refs/heads/"+orderBranch("ORD-0003"), "^"+base)

		got, err := fetchWork(ctx, d, "ORD-0003", []landedRepo{{Name: "app", Branch: orderBranch("ORD-0003"), Commit: commit, Bundle: out}})
		require.NoError(t, err)
		require.Len(t, got, 1)
		require.Contains(t, got[0].RejectError, "hasDotgit")
		require.Empty(t, got[0].FetchError)
		require.Contains(t, workRejections(got), "repo app: ")
		require.Equal(t, shipBase, gitT(t, clone, "rev-parse", "bmus/ORD-0003"), "left at the base")
	})

	t.Run("malformed commit", func(t *testing.T) {
		work := filepath.Join(t.TempDir(), "w")
		gitT(t, filepath.Dir(work), "clone", "-q", "--branch", orderBranch("ORD-0003"), shipped, work)
		base := gitT(t, work, "rev-parse", "HEAD")
		tree := gitT(t, work, "rev-parse", "HEAD^{tree}")
		body := fmt.Sprintf("tree %s\nparent %s\nnotarealheader garbage\n\nevil\n", tree, base)
		content := fmt.Sprintf("commit %d\x00%s", len(body), body)
		sha := fmt.Sprintf("%x", sha1.Sum([]byte(content)))
		var zbuf bytes.Buffer
		zw := zlib.NewWriter(&zbuf)
		_, err := zw.Write([]byte(content))
		require.NoError(t, err)
		require.NoError(t, zw.Close())
		objDir := filepath.Join(work, ".git", "objects", sha[:2])
		require.NoError(t, os.MkdirAll(objDir, 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(objDir, sha[2:]), zbuf.Bytes(), 0o600))
		gitT(t, work, "update-ref", "refs/heads/"+orderBranch("ORD-0003"), sha)
		out := filepath.Join(t.TempDir(), "badcommit.bundle")
		gitT(t, work, "bundle", "create", "-q", out, "refs/heads/"+orderBranch("ORD-0003"), "^"+base)

		got, err := fetchWork(ctx, d, "ORD-0003", []landedRepo{{Name: "app", Branch: orderBranch("ORD-0003"), Commit: sha, Bundle: out}})
		require.NoError(t, err)
		require.Len(t, got, 1)
		require.Contains(t, got[0].RejectError, "missingAuthor")
		require.Empty(t, got[0].FetchError)
		require.Contains(t, workRejections(got), "repo app: ")
		require.Equal(t, shipBase, gitT(t, clone, "rev-parse", "bmus/ORD-0003"), "left at the base")
	})
}

// gitSpeaksGerman reports whether this host's git translates its fsck
// message under LANG=de_DE.UTF-8, by running index-pack on pack outside
// the bridge's own gitEnv (which #105 makes locale-independent). A
// minimal install may carry no German catalog, in which case the bug
// this guards against never reproduces here.
func gitSpeaksGerman(t *testing.T, pack []byte) bool {
	t.Helper()
	tmp := t.TempDir()
	gitT(t, tmp, "init", "-q", "--bare")
	cmd := exec.Command("git", "index-pack", "--strict", "--fix-thin", "--stdin")
	cmd.Dir = tmp
	cmd.Stdin = bytes.NewReader(pack)
	env := make([]string, 0, len(os.Environ())+1)
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "LC_ALL=") {
			env = append(env, kv)
		}
	}
	cmd.Env = append(env, "LANG=de_DE.UTF-8")
	out, _ := cmd.CombinedOutput()
	return strings.Contains(string(out), "Fehler") && !strings.Contains(string(out), "fsck error in packed object")
}

// #105: gitEnv (repos.go) sets LC_ALL=C, so fetch-work's object check
// (#65) keeps classifying a tampered bundle as RejectError/work_rejected
// under the captain's own locale, instead of git's inherited LANG
// translating "fsck error in packed object" and the failure falling
// through to the ordinary FetchError/fetch_failed path, which retries as
// if it were transient. Skipped on a host whose git has no German
// catalog to translate against.
func TestFetchWorkRejectsBadObjectsUnderCaptainsLocale(t *testing.T) {
	up := testRepo(t)
	d := testDaemon(t, map[string]string{"app": up.url})
	ctx := t.Context()
	shipped := filepath.Join(t.TempDir(), "in.bundle")
	_, err := d.bundleRepo(ctx, "app", up.url, "ORD-0004", "main", shipped)
	require.NoError(t, err)
	clone := d.repoDir("app")
	shipBase := gitT(t, clone, "rev-parse", "bmus/ORD-0004")

	work := filepath.Join(t.TempDir(), "w")
	gitT(t, filepath.Dir(work), "clone", "-q", "--branch", orderBranch("ORD-0004"), shipped, work)
	base := gitT(t, work, "rev-parse", "HEAD")
	evilFile := filepath.Join(t.TempDir(), "evil.txt")
	require.NoError(t, os.WriteFile(evilFile, []byte("evil\n"), 0o600))
	blob := gitT(t, work, "hash-object", "-w", evilFile)
	headTree := gitT(t, work, "rev-parse", "HEAD^{tree}")
	tree := gitTStdin(t, work, fmt.Sprintf("100644 blob %s\tfile.txt\n040000 tree %s\t.GIT\n", blob, headTree), "mktree")
	commit := gitTStdin(t, work, "evil\n", "commit-tree", "-p", base, tree)
	gitT(t, work, "update-ref", "refs/heads/"+orderBranch("ORD-0004"), commit)
	out := filepath.Join(t.TempDir(), "dotgit.bundle")
	gitT(t, work, "bundle", "create", "-q", out, "refs/heads/"+orderBranch("ORD-0004"), "^"+base)

	f, err := os.Open(out)
	require.NoError(t, err)
	defer f.Close()
	pack, err := bundlePackReader(f)
	require.NoError(t, err)
	packBytes, err := io.ReadAll(pack)
	require.NoError(t, err)
	if !gitSpeaksGerman(t, packBytes) {
		t.Skip("git on this host has no German translations")
	}

	t.Setenv("LC_ALL", "") // an inherited LC_ALL would hide the bug
	t.Setenv("LANG", "de_DE.UTF-8")
	got, err := fetchWork(ctx, d, "ORD-0004", []landedRepo{{Name: "app", Branch: orderBranch("ORD-0004"), Commit: commit, Bundle: out}})
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Contains(t, got[0].RejectError, "hasDotgit")
	require.Empty(t, got[0].FetchError)
	require.Contains(t, workRejections(got), "repo app: ")
	require.Equal(t, shipBase, gitT(t, clone, "rev-parse", "bmus/ORD-0004"), "left at the base")
}

func TestDoneText(t *testing.T) {
	t.Parallel()
	require.Equal(t, "did it", doneText("did it", nil))
	require.Equal(t, "did it (app: bmus/ORD-0001 at 0123456789ab; lib: not fetched, bundle at /c/lib.bundle)",
		doneText("did it", []landedRepo{
			{Name: "app", Branch: "bmus/ORD-0001", Commit: "0123456789abcdef"},
			{Name: "lib", Branch: "bmus/ORD-0001", Commit: "fedc", Bundle: "/c/lib.bundle", FetchError: "x"},
		}))
	require.Equal(t, "did it (app: rejected, bundle at /c/app.bundle)",
		doneText("did it", []landedRepo{
			{Name: "app", Branch: "bmus/ORD-0001", Bundle: "/c/app.bundle", RejectError: "hasDotgit"},
		}))
}

// sendReport delivers a report for orderID to its mission, as the comms
// pump would.
func sendReport(t *testing.T, d *Daemon, orderID string, seq uint64, body comms.ReportBody) {
	t.Helper()
	msg, err := comms.NewMessage(comms.TypeReport, body)
	require.NoError(t, err)
	msg.Seq, msg.Crew, msg.Order = seq, "sorell", orderID
	require.NoError(t, dbos.Send(d.dbos, missionWorkflowID(orderID), msg, fromCrewTopic))
}

// startMission adds an order for app at main and waits for its send-order.
func startMission(t *testing.T, d *Daemon) string {
	t.Helper()
	id, err := d.CreateOrder(t.Context(), store.Order{Title: "t", Assignee: "sorell",
		Repos: []store.OrderRepo{{Name: "app", BaseRef: "main"}}})
	require.NoError(t, err)
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		_, ok := stepOutput(c, d, missionWorkflowID(id), "send-order")
		require.True(c, ok)
	}, 20*time.Second, 50*time.Millisecond)
	return id
}

// #53: a report's bundle is pulled into <home>/cargo/<order>/final/ before
// the order is done, and fetched into the bridge's clone as bmus/<order>. A
// bridge stopped mid-pull re-runs the step on restart; nothing is pushed.
func TestMissionWorkComesBackAcrossRestart(t *testing.T) {
	t.Parallel()
	home := privateHome(t)
	insertCrew(t, home, store.Crew{ID: "sorell", Name: "Sorell"})
	up := testRepo(t)
	upRefs := refs(t, up.url)
	repos := map[string]string{"app": up.url}
	beams := t.TempDir()

	// First daemon: the pull never finishes.
	p1 := &fakeProvider{hello: true, copyRoot: beams, copyFromGate: make(chan struct{})}
	r := startDaemonRepos(t, home, p1, repos)
	waitOnDeck(t, r.d, "sorell", "fake-1")
	id := startMission(t, r.d)
	work := "/beams/fake-1/work/" + id
	_, commit := workBundle(t, filepath.Join(beams, work, "in", "app.bundle"), id, filepath.Join(beams, work, "out", "app.bundle"))
	report := comms.ReportBody{Summary: "did it", Usage: comms.Usage{InputTokens: 3, OutputTokens: 4},
		Repos: []comms.WorkRepo{{Name: "app", BundlePath: work + "/out/app.bundle", Commit: commit, Branch: "bmus/" + id}}}
	sendReport(t, r.d, id, 2, report)
	final := filepath.Join(home, "cargo", id, "final")
	require.Eventually(t, func() bool { _, err := os.Stat(final); return err == nil }, 20*time.Second, 20*time.Millisecond,
		"pull-work never started")
	require.Equal(t, store.OrderAway, getOrder(t, r.d, id).State, "not done before the pull")
	_, pulled := stepOutput(t, r.d, missionWorkflowID(id), "pull-work")
	require.False(t, pulled)
	r.stop(t)

	p2 := &fakeProvider{hello: true, copyRoot: beams}
	r = startDaemonRepos(t, home, p2, repos)
	defer r.stop(t)
	res := missionResult(t, r.d, id)
	require.Equal(t, MissionDone, res.Status)
	require.Equal(t, store.OrderDone, getOrder(t, r.d, id).State)

	bundle := filepath.Join(final, "app.bundle")
	require.Equal(t, commit+" refs/heads/bmus/"+id, bundleHeads(t, bundle))
	require.Equal(t, commit, gitT(t, filepath.Join(home, "repos", "app"), "rev-parse", "bmus/"+id))
	require.Equal(t, upRefs, refs(t, up.url), "nothing pushed")

	entries, err := r.d.store.ListLog(t.Context(), 0)
	require.NoError(t, err)
	var done []store.LogEntry
	for _, e := range entries {
		if e.Kind == logDone {
			done = append(done, e)
		}
	}
	require.Len(t, done, 1)
	require.Equal(t, "did it (app: bmus/"+id+" at "+commit[:12]+")", done[0].Text)
	var p donePayload
	require.NoError(t, json.Unmarshal(done[0].Payload, &p))
	require.Equal(t, donePayload{Summary: "did it", Usage: report.Usage,
		Repos: []landedRepo{{Name: "app", Branch: "bmus/" + id, Commit: commit, Bundle: bundle}}}, p)
}

// A report or checkpoint that doesn't match the order's repos can't be
// pulled: the checkpoint is logged as failed and the work goes on; the
// report fails the order with cargo_lost at once.
func TestMissionCargoLost(t *testing.T) {
	t.Parallel()
	home := privateHome(t)
	insertCrew(t, home, store.Crew{ID: "sorell", Name: "Sorell"})
	r := startDaemonWith(t, home, &fakeProvider{hello: true, copyRoot: t.TempDir()})
	defer r.stop(t)
	waitOnDeck(t, r.d, "sorell", "fake-1")
	id := startMission(t, r.d)

	cp, err := comms.NewMessage(comms.TypeCheckpointReady, comms.CheckpointReadyBody{TranscriptPath: "/t", HandoverPath: "/h"})
	require.NoError(t, err)
	cp.Seq, cp.Crew, cp.Order = 2, "sorell", id
	require.NoError(t, dbos.Send(r.d.dbos, missionWorkflowID(id), cp, fromCrewTopic))
	sendReport(t, r.d, id, 3, comms.ReportBody{Summary: "did it"})

	res := missionResult(t, r.d, id)
	require.Equal(t, MissionFailed, res.Status)
	require.Equal(t, CodeCargoLost, res.Code)
	require.Contains(t, res.Reason, "the order's work could not be pulled into the cargo bay: no bundle for repo app")
	require.Equal(t, store.OrderFailed, getOrder(t, r.d, id).State)
	cps, err := r.d.store.ListCheckpoints(t.Context(), id)
	require.NoError(t, err)
	require.Empty(t, cps)
	entries, err := r.d.store.ListLog(t.Context(), 0)
	require.NoError(t, err)
	codes := map[string]int{}
	for _, e := range entries {
		var p map[string]any
		require.NoError(t, json.Unmarshal(e.Payload, &p))
		if c, ok := p["code"].(string); ok {
			codes[c]++
		}
	}
	require.Equal(t, map[string]int{CodeCheckpointFailed: 1, CodeCargoLost: 1}, codes)
	_, pulled := stepOutput(t, r.d, missionWorkflowID(id), "fetch-work")
	require.False(t, pulled)
}

// orderLog returns the log entries of orderID.
func orderLog(t *testing.T, d *Daemon, orderID string) []store.LogEntry {
	t.Helper()
	entries, err := d.store.ListLog(t.Context(), 0)
	require.NoError(t, err)
	var out []store.LogEntry
	for _, e := range entries {
		if e.OrderID == orderID {
			out = append(out, e)
		}
	}
	return out
}

// codes counts entries by their payload's code.
func codes(t *testing.T, entries []store.LogEntry) map[string]int {
	t.Helper()
	out := map[string]int{}
	for _, e := range entries {
		var p map[string]any
		require.NoError(t, json.Unmarshal(e.Payload, &p))
		if c, ok := p["code"].(string); ok {
			out[c]++
		}
	}
	return out
}

// The tour's backstop fails the order while its work is still being
// pulled. When the pull finishes the order stays failed with its one
// terminal entry; the work is kept (cargo bay and clone) and logged once as
// late.
func TestMissionLateWorkKeepsBackstop(t *testing.T) {
	t.Parallel()
	home := privateHome(t)
	insertCrew(t, home, store.Crew{ID: "sorell", Name: "Sorell"})
	beams := t.TempDir()
	gate := make(chan struct{})
	r := startDaemonWith(t, home, &fakeProvider{hello: true, copyRoot: beams, copyFromGate: gate})
	defer r.stop(t)
	waitOnDeck(t, r.d, "sorell", "fake-1")
	id := startMission(t, r.d)
	work := "/beams/fake-1/work/" + id
	_, commit := workBundle(t, filepath.Join(beams, work, "in", "app.bundle"), id, filepath.Join(beams, work, "out", "app.bundle"))
	sendReport(t, r.d, id, 2, comms.ReportBody{Summary: "did it",
		Repos: []comms.WorkRepo{{Name: "app", BundlePath: work + "/out/app.bundle", Commit: commit, Branch: "bmus/" + id}}})
	final := filepath.Join(home, "cargo", id, "final")
	require.Eventually(t, func() bool { _, err := os.Stat(final); return err == nil }, 20*time.Second, 20*time.Millisecond,
		"pull-work never started")

	require.NoError(t, r.d.backstop(t.Context(), "sorell", id, false))
	require.Equal(t, store.OrderFailed, getOrder(t, r.d, id).State)
	close(gate)
	missionResult(t, r.d, id)

	require.Equal(t, store.OrderFailed, getOrder(t, r.d, id).State, "a late order-done doesn't overwrite the backstop")
	require.Equal(t, commit, gitT(t, filepath.Join(home, "repos", "app"), "rev-parse", "bmus/"+id), "the work is kept")
	entries := orderLog(t, r.d, id)
	terminal, late := 0, 0
	for _, e := range entries {
		switch {
		case e.Kind == logDone || e.Kind == logFail:
			terminal++
		case e.Kind == logOrder && e.Text == "the order's work arrived after the order had ended (app: bmus/"+id+" at "+commit[:12]+")":
			late++
		}
	}
	require.Equal(t, 1, terminal, "%+v", entries)
	require.Equal(t, 1, late, "%+v", entries)
	require.Equal(t, map[string]int{CodeUnsettled: 1}, codes(t, entries))
}

// A bundle that doesn't hold the reported commit is pulled but not
// fetched: the order is still done, one fetch_failed entry says why, and
// the done entry points at the bundle in the cargo bay.
func TestMissionFetchFailedStillDone(t *testing.T) {
	t.Parallel()
	home := privateHome(t)
	insertCrew(t, home, store.Crew{ID: "sorell", Name: "Sorell"})
	beams := t.TempDir()
	r := startDaemonWith(t, home, &fakeProvider{hello: true, copyRoot: beams})
	defer r.stop(t)
	waitOnDeck(t, r.d, "sorell", "fake-1")
	id := startMission(t, r.d)
	work := "/beams/fake-1/work/" + id
	base, _ := workBundle(t, filepath.Join(beams, work, "in", "app.bundle"), id, filepath.Join(beams, work, "out", "app.bundle"))
	sendReport(t, r.d, id, 2, comms.ReportBody{Summary: "did it",
		Repos: []comms.WorkRepo{{Name: "app", BundlePath: work + "/out/app.bundle", Commit: base, Branch: "bmus/" + id}}})

	res := missionResult(t, r.d, id)
	require.Equal(t, MissionDone, res.Status)
	require.Equal(t, store.OrderDone, getOrder(t, r.d, id).State)
	require.Equal(t, base, gitT(t, filepath.Join(home, "repos", "app"), "rev-parse", "bmus/"+id), "not fetched")

	entries := orderLog(t, r.d, id)
	require.Equal(t, map[string]int{CodeFetchFailed: 1}, codes(t, entries))
	bundle := filepath.Join(home, "cargo", id, "final", "app.bundle")
	var done []store.LogEntry
	for _, e := range entries {
		if e.Kind == logDone {
			done = append(done, e)
		}
		if e.Kind == logFail {
			t.Fatalf("order failed: %+v", e)
		}
	}
	require.Len(t, done, 1)
	require.Equal(t, "did it (app: not fetched, bundle at "+bundle+")", done[0].Text)
	var p donePayload
	require.NoError(t, json.Unmarshal(done[0].Payload, &p))
	require.Len(t, p.Repos, 1)
	require.Equal(t, bundle, p.Repos[0].Bundle)
	require.Contains(t, p.Repos[0].FetchError, "not the reported "+base)
}

// A fetch that keeps failing (the clone is gone) runs out of retries; the
// order is still done, with one short fetch_failed entry.
func TestMissionFetchExhaustedStillDone(t *testing.T) {
	t.Parallel()
	home := privateHome(t)
	insertCrew(t, home, store.Crew{ID: "sorell", Name: "Sorell"})
	beams := t.TempDir()
	cfg := testConfig(home)
	cfg.Transporter = &fakeProvider{hello: true, copyRoot: beams}
	cfg.Repos = map[string]string{"app": testRepo(t).url}
	cfg.copyRetries = []dbos.StepOption{dbos.WithStepMaxRetries(2),
		dbos.WithStepBaseInterval(10 * time.Millisecond), dbos.WithStepMaxInterval(20 * time.Millisecond)}
	r := startDaemonConfig(t, cfg)
	defer r.stop(t)
	waitOnDeck(t, r.d, "sorell", "fake-1")
	id := startMission(t, r.d)
	work := "/beams/fake-1/work/" + id
	_, commit := workBundle(t, filepath.Join(beams, work, "in", "app.bundle"), id, filepath.Join(beams, work, "out", "app.bundle"))
	require.NoError(t, os.RemoveAll(filepath.Join(home, "repos", "app")))
	sendReport(t, r.d, id, 2, comms.ReportBody{Summary: "did it",
		Repos: []comms.WorkRepo{{Name: "app", BundlePath: work + "/out/app.bundle", Commit: commit, Branch: "bmus/" + id}}})

	require.Equal(t, MissionDone, missionResult(t, r.d, id).Status)
	require.Equal(t, store.OrderDone, getOrder(t, r.d, id).State)
	entries := orderLog(t, r.d, id)
	require.Equal(t, map[string]int{CodeFetchFailed: 1}, codes(t, entries))
	for _, e := range entries {
		if e.Kind == logOrder && strings.Contains(e.Text, "could not be fetched") {
			require.NotContains(t, e.Text, "\n")
		}
		if e.Kind == logDone {
			require.Equal(t, "did it (app: not fetched, bundle at "+filepath.Join(home, "cargo", id, "final", "app.bundle")+")", e.Text)
		}
	}
}

// #65: a report's bundle over Config.MaxBundleBytes is refused before it
// is copied off the beam, so the order fails with bundle_too_large (not
// cargo_lost) and nothing lands in the cargo bay.
func TestMissionReportBundleTooLarge(t *testing.T) {
	t.Parallel()
	home := privateHome(t)
	insertCrew(t, home, store.Crew{ID: "sorell", Name: "Sorell"})
	beams := t.TempDir()
	r := startDaemonWith(t, home, &fakeProvider{hello: true, copyRoot: beams})
	defer r.stop(t)
	waitOnDeck(t, r.d, "sorell", "fake-1")
	id := startMission(t, r.d)
	work := "/beams/fake-1/work/" + id
	_, commit := workBundle(t, filepath.Join(beams, work, "in", "app.bundle"), id, filepath.Join(beams, work, "out", "app.bundle"))
	r.d.maxBundleBytes = 1
	sendReport(t, r.d, id, 2, comms.ReportBody{Summary: "did it",
		Repos: []comms.WorkRepo{{Name: "app", BundlePath: work + "/out/app.bundle", Commit: commit, Branch: "bmus/" + id}}})

	res := missionResult(t, r.d, id)
	require.Equal(t, MissionFailed, res.Status)
	require.Equal(t, CodeBundleTooLarge, res.Code)
	require.Contains(t, res.Reason, "over the")
	require.Contains(t, res.Reason, "app")
	require.Equal(t, store.OrderFailed, getOrder(t, r.d, id).State)
	_, err := os.Stat(filepath.Join(home, "cargo", id, "final", "app.bundle"))
	require.ErrorIs(t, err, fs.ErrNotExist)
}

// #65: a checkpoint's bundle over Config.MaxBundleBytes is refused before
// any of the checkpoint's files (transcript, handover note included) are
// copied: the checkpoint is logged as bundle_too_large and the mission
// goes on, the same contract as an ordinary checkpoint_failed.
func TestCheckpointBundleTooLarge(t *testing.T) {
	t.Parallel()
	home := privateHome(t)
	insertCrew(t, home, store.Crew{ID: "sorell", Name: "Sorell"})
	beams := t.TempDir()
	r := startDaemonWith(t, home, &fakeProvider{hello: true, copyRoot: beams})
	defer r.stop(t)
	waitOnDeck(t, r.d, "sorell", "fake-1")
	id := startMission(t, r.d)
	work := "/beams/fake-1/work/" + id
	_, commit := workBundle(t, filepath.Join(beams, work, "in", "app.bundle"), id, filepath.Join(beams, work, "out", "app.bundle"))
	r.d.maxBundleBytes = 1

	cp, err := comms.NewMessage(comms.TypeCheckpointReady, comms.CheckpointReadyBody{
		Repos:          []comms.WorkRepo{{Name: "app", BundlePath: work + "/out/app.bundle", Commit: commit, Branch: "bmus/" + id}},
		TranscriptPath: "/t", HandoverPath: "/h",
	})
	require.NoError(t, err)
	cp.Seq, cp.Crew, cp.Order = 2, "sorell", id
	require.NoError(t, dbos.Send(r.d.dbos, missionWorkflowID(id), cp, fromCrewTopic))

	require.EventuallyWithT(t, func(c *assert.CollectT) {
		_, ok := stepOutput(c, r.d, missionWorkflowID(id), "log-checkpoint-failed")
		require.True(c, ok)
	}, 20*time.Second, 50*time.Millisecond)

	entries := orderLog(t, r.d, id)
	require.Equal(t, map[string]int{CodeBundleTooLarge: 1}, codes(t, entries))
	cps, err := r.d.store.ListCheckpoints(t.Context(), id)
	require.NoError(t, err)
	require.Empty(t, cps)
	require.Equal(t, store.OrderAway, getOrder(t, r.d, id).State, "a checkpoint failure doesn't fail the order")
}

// #65: a beam whose Size answer is too small (a symlink, a file that grows
// after the check) still can't land an oversized bundle: the copy is
// checked again in the cargo bay and removed, for a report's bundle and a
// checkpoint's.
func TestBundleTooLargeAfterCopy(t *testing.T) {
	t.Parallel()
	for _, checkpoint := range []bool{false, true} {
		t.Run(map[bool]string{false: "report", true: "checkpoint"}[checkpoint], func(t *testing.T) {
			home := privateHome(t)
			insertCrew(t, home, store.Crew{ID: "sorell", Name: "Sorell"})
			beams := t.TempDir()
			r := startDaemonWith(t, home, &fakeProvider{hello: true, copyRoot: beams, sizeLie: 1})
			defer r.stop(t)
			waitOnDeck(t, r.d, "sorell", "fake-1")
			id := startMission(t, r.d)
			work := "/beams/fake-1/work/" + id
			_, commit := workBundle(t, filepath.Join(beams, work, "in", "app.bundle"), id, filepath.Join(beams, work, "out", "app.bundle"))
			r.d.maxBundleBytes = 10
			repos := []comms.WorkRepo{{Name: "app", BundlePath: work + "/out/app.bundle", Commit: commit, Branch: "bmus/" + id}}

			if !checkpoint {
				sendReport(t, r.d, id, 2, comms.ReportBody{Summary: "did it", Repos: repos})
				res := missionResult(t, r.d, id)
				require.Equal(t, MissionFailed, res.Status)
				require.Equal(t, CodeBundleTooLarge, res.Code)
				require.Contains(t, res.Reason, "the copied bundle is")
				_, err := os.Stat(filepath.Join(home, "cargo", id, "final", "app.bundle"))
				require.ErrorIs(t, err, fs.ErrNotExist)
				return
			}
			for _, p := range []string{"/t", "/h"} {
				require.NoError(t, os.WriteFile(filepath.Join(beams, p), []byte("x"), 0o600))
			}
			cp, err := comms.NewMessage(comms.TypeCheckpointReady, comms.CheckpointReadyBody{Repos: repos, TranscriptPath: "/t", HandoverPath: "/h"})
			require.NoError(t, err)
			cp.Seq, cp.Crew, cp.Order = 2, "sorell", id
			require.NoError(t, dbos.Send(r.d.dbos, missionWorkflowID(id), cp, fromCrewTopic))
			require.EventuallyWithT(t, func(c *assert.CollectT) {
				_, ok := stepOutput(c, r.d, missionWorkflowID(id), "log-checkpoint-failed")
				require.True(c, ok)
			}, 20*time.Second, 50*time.Millisecond)
			require.Equal(t, map[string]int{CodeBundleTooLarge: 1}, codes(t, orderLog(t, r.d, id)))
			cps, err := r.d.store.ListCheckpoints(t.Context(), id)
			require.NoError(t, err)
			require.Empty(t, cps)
			_, err = os.Stat(filepath.Join(r.d.cargoDir(id, "fake-1", 2), "app.bundle"))
			require.ErrorIs(t, err, fs.ErrNotExist)
		})
	}
}
