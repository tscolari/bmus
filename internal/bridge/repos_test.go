package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/dbos-inc/dbos-transact-golang/dbos"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tscolari/bmus/internal/comms"
	"github.com/tscolari/bmus/internal/store"
)

// gitT runs git in dir with a fixed identity and returns its trimmed
// stdout.
func gitT(t require.TestingT, dir string, args ...string) string {
	cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@example.com",
		"-c", "commit.gpgsign=false", "-c", "tag.gpgsign=false"}, args...)...)
	cmd.Dir = dir
	out, err := cmd.Output()
	var stderr []byte
	if ee := (*exec.ExitError)(nil); errors.As(err, &ee) {
		stderr = ee.Stderr
	}
	require.NoError(t, err, "git %v: %s", args, stderr)
	return strings.TrimSpace(string(out))
}

// upstream is a test repo standing in for a registered repo's server: a
// plain repository whose path is its URL.
type upstream struct{ url string }

// testRepo makes an upstream with main at two commits.
func testRepo(t *testing.T) upstream {
	t.Helper()
	dir := t.TempDir()
	gitT(t, dir, "init", "-q", "-b", "main")
	u := upstream{url: dir}
	u.commit(t, "one")
	u.commit(t, "two")
	return u
}

// commit adds a commit to the checked-out branch and returns it.
func (u upstream) commit(t *testing.T, msg string) string {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(u.url, "file.txt"), []byte(msg+"\n"), 0o600))
	gitT(t, u.url, "add", "file.txt")
	gitT(t, u.url, "commit", "-q", "-m", msg)
	return gitT(t, u.url, "rev-parse", "HEAD")
}

func (u upstream) rev(t *testing.T, rev string) string {
	t.Helper()
	return gitT(t, u.url, "rev-parse", rev+"^{commit}")
}

// bundleHeads returns `git bundle list-heads` of bundle.
func bundleHeads(t *testing.T, bundle string) string {
	t.Helper()
	return gitT(t, t.TempDir(), "bundle", "list-heads", bundle)
}

func TestCheckRepos(t *testing.T) {
	t.Parallel()
	require.NoError(t, CheckRepos(nil))
	require.NoError(t, CheckRepos(map[string]string{"app": "/srv/app.git", "web.ui-2_x": "git@example.com:a/b.git"}))
	for name, repos := range map[string]map[string]string{
		"empty name":   {"": "u"},
		"dot":          {".": "u"},
		"dotdot":       {"..": "u"},
		"hidden":       {".app": "u"},
		"slash":        {"a/b": "u"},
		"space":        {"a b": "u"},
		"reserved in":  {"in": "u"},
		"reserved out": {"out": "u"},
		"numeric":      {"12": "u"},
		"empty url":    {"app": ""},
		"blank url":    {"app": "  "},
		"dash url":     {"app": "-oProxyCommand=evil"},
	} {
		t.Run(name, func(t *testing.T) { require.Error(t, CheckRepos(repos)) })
	}
}

func testDaemon(t *testing.T, repos map[string]string) *Daemon {
	return &Daemon{home: privateHome(t), repos: repos, repoLocks: map[string]chan struct{}{}}
}

// bundleRepo keeps a bare clone in <home>/repos/<name> whose only branches
// are the bridge's bmus/<order>, and bundles bmus/<order> at base_ref's
// commit with the full history. A re-run follows a base_ref that moved.
func TestBundleRepo(t *testing.T) {
	t.Parallel()
	up := testRepo(t)
	gitT(t, up.url, "tag", "v1", "HEAD~1")
	gitT(t, up.url, "branch", "feature", "HEAD~1")
	d := testDaemon(t, map[string]string{"app": up.url})
	ctx := t.Context()
	out := filepath.Join(d.home, "cargo", "ORD-0001", "in", "app.bundle")

	commit, err := d.bundleRepo(ctx, "app", up.url, "ORD-0001", "main", out)
	require.NoError(t, err)
	require.Equal(t, up.rev(t, "main"), commit)
	require.Equal(t, commit+" refs/heads/bmus/ORD-0001", bundleHeads(t, out))
	_, err = os.Stat(out + ".tmp")
	require.ErrorIs(t, err, os.ErrNotExist)
	clone := filepath.Join(t.TempDir(), "c")
	gitT(t, filepath.Dir(clone), "clone", "-q", "--branch", "bmus/ORD-0001", out, clone)
	require.Equal(t, commit, gitT(t, clone, "rev-parse", "HEAD"))
	require.Equal(t, "2", gitT(t, clone, "rev-list", "--count", "HEAD"), "full history")

	dir := filepath.Join(d.home, "repos", "app")
	require.Equal(t, "true", gitT(t, dir, "rev-parse", "--is-bare-repository"))
	require.Equal(t, "refs/heads/bmus/ORD-0001", gitT(t, dir, "for-each-ref", "--format=%(refname)", "refs/heads"))
	require.Equal(t, commit, gitT(t, dir, "rev-parse", "refs/remotes/origin/main"))

	// The upstream moves on; a re-run fetches and follows it.
	moved := up.commit(t, "three")
	commit, err = d.bundleRepo(ctx, "app", up.url, "ORD-0001", "main", out)
	require.NoError(t, err)
	require.Equal(t, moved, commit)
	require.Equal(t, moved+" refs/heads/bmus/ORD-0001", bundleHeads(t, out))

	// Other refs: a branch of origin, a tag, a commit id, origin/<branch>.
	for ref, want := range map[string]string{
		"feature":           up.rev(t, "feature"),
		"v1":                up.rev(t, "v1"),
		up.rev(t, "HEAD~2"): up.rev(t, "HEAD~2"),
		"origin/main":       moved,
	} {
		commit, err := d.bundleRepo(ctx, "app", up.url, "ORD-0002", ref, out)
		require.NoError(t, err, ref)
		require.Equal(t, want, commit, ref)
		require.Equal(t, want+" refs/heads/bmus/ORD-0002", bundleHeads(t, out), ref)
	}

	// A ref the repo doesn't have isn't worth retrying; an unreachable URL is.
	_, err = d.bundleRepo(ctx, "app", up.url, "ORD-0003", "nope", out)
	require.ErrorIs(t, err, errRepoPermanent)
	require.ErrorContains(t, err, `base_ref "nope" not found in `+up.url)
	_, err = d.bundleRepo(ctx, "gone", filepath.Join(t.TempDir(), "missing"), "ORD-0003", "main", out)
	require.Error(t, err)
	require.NotErrorIs(t, err, errRepoPermanent)
	require.ErrorContains(t, err, "git fetch")
}

// An upstream that moves a tag doesn't break later fetches, origin's branch
// wins over a tag of the same name, and base_ref may name another order's
// bmus/<order> branch in the bridge's clone.
func TestBundleRepoRefs(t *testing.T) {
	t.Parallel()
	up := testRepo(t)
	gitT(t, up.url, "tag", "v1", "HEAD~1")
	gitT(t, up.url, "tag", "same", "HEAD~1")
	gitT(t, up.url, "branch", "same", "HEAD")
	d := testDaemon(t, map[string]string{"app": up.url})
	ctx := t.Context()
	out := filepath.Join(d.home, "out.bundle")

	commit, err := d.bundleRepo(ctx, "app", up.url, "ORD-0001", "v1", out)
	require.NoError(t, err)
	require.Equal(t, up.rev(t, "HEAD~1"), commit)
	gitT(t, up.url, "tag", "-f", "v1", "HEAD")
	commit, err = d.bundleRepo(ctx, "app", up.url, "ORD-0002", "v1", out)
	require.NoError(t, err, "a moved tag is fetched over the old one")
	require.Equal(t, up.rev(t, "HEAD"), commit)

	commit, err = d.bundleRepo(ctx, "app", up.url, "ORD-0003", "same", out)
	require.NoError(t, err)
	require.Equal(t, up.rev(t, "refs/heads/same"), commit, "origin's branch, not the tag")

	commit, err = d.bundleRepo(ctx, "app", up.url, "ORD-0004", "bmus/ORD-0001", out)
	require.NoError(t, err)
	require.Equal(t, up.rev(t, "HEAD~1"), commit, "the earlier order's branch")
	require.Equal(t, commit+" refs/heads/bmus/ORD-0004", bundleHeads(t, out))
}

// git's repository-locating environment (GIT_DIR, GIT_INDEX_FILE, ...) is
// dropped: the bridge's git works on its clone, never on the repo it names.
func TestBundleRepoIgnoresGitEnv(t *testing.T) {
	up := testRepo(t)
	decoy := testRepo(t)
	state := func() string {
		return gitT(t, decoy.url, "for-each-ref") + "\n" + gitT(t, decoy.url, "config", "--list", "--local") +
			"\n" + gitT(t, decoy.url, "count-objects", "-v")
	}
	before := state()
	d := testDaemon(t, map[string]string{"app": up.url})
	out := filepath.Join(d.home, "out.bundle")
	for k, v := range map[string]string{
		"GIT_DIR": filepath.Join(decoy.url, ".git"), "GIT_WORK_TREE": decoy.url,
		"GIT_INDEX_FILE":       filepath.Join(decoy.url, ".git", "index"),
		"GIT_OBJECT_DIRECTORY": filepath.Join(decoy.url, ".git", "objects"),
		"GIT_COMMON_DIR":       filepath.Join(decoy.url, ".git"),
	} {
		t.Setenv(k, v)
	}
	commit, err := d.bundleRepo(t.Context(), "app", up.url, "ORD-0001", "main", out)
	for _, k := range []string{"GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_OBJECT_DIRECTORY", "GIT_COMMON_DIR"} {
		os.Unsetenv(k) // t.Setenv restores them after the test
	}
	require.NoError(t, err)
	require.Equal(t, up.rev(t, "main"), commit)
	require.Equal(t, commit+" refs/heads/bmus/ORD-0001", bundleHeads(t, out))
	require.Equal(t, before, state(), "the decoy repo is untouched")
}

// A fetch that never answers is cut off by gitFetchTimeout, and waiting for
// a repo's lock gives up when the context ends: nothing holds a clone for
// good.
func TestBundleRepoBounded(t *testing.T) {
	d := testDaemon(t, nil)
	out := filepath.Join(d.home, "out.bundle")
	pidFile := filepath.Join(t.TempDir(), "ssh.pid")
	// An ssh that never answers, recording its pid.
	t.Setenv("GIT_SSH_COMMAND", "echo $$ > "+pidFile+"; exec sleep 30; :")
	old := gitFetchTimeout
	gitFetchTimeout = 500 * time.Millisecond
	defer func() { gitFetchTimeout = old }()
	start := time.Now()
	_, err := d.bundleRepo(t.Context(), "app", "ssh://example.invalid/app.git", "ORD-0001", "main", out)
	require.ErrorContains(t, err, "took over 500ms")
	require.NotErrorIs(t, err, errRepoPermanent)
	require.Less(t, time.Since(start), 15*time.Second, "cut off, not waited out")
	data, err := os.ReadFile(pidFile)
	require.NoError(t, err)
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	require.NoError(t, err)
	require.Eventually(t, func() bool { return processGone(pid) }, 5*time.Second, 20*time.Millisecond,
		"git's ssh (pid %d) outlived the cancelled fetch", pid)

	unlock, err := d.lockRepo(t.Context(), "app")
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	_, err = d.bundleRepo(ctx, "app", "ssh://example.invalid/app.git", "ORD-0001", "main", out)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.ErrorContains(t, err, "waiting for repo app's clone")
	unlock()
	unlock, err = d.lockRepo(t.Context(), "app")
	require.NoError(t, err, "free again")
	unlock()
}

// processGone reports whether pid has exited: no such process, or a zombie
// (reparented to an init that doesn't reap).
func processGone(pid int) bool {
	if syscall.Kill(pid, 0) != nil {
		return true
	}
	stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return errors.Is(err, os.ErrNotExist) // no procfs: kill(pid, 0) is all there is
	}
	i := strings.LastIndexByte(string(stat), ')')
	return i >= 0 && i+2 < len(stat) && (stat[i+2] == 'Z' || stat[i+2] == 'X')
}

// The tour's end is sticky: a tour that ends while ship-repos waits between
// attempts (the first copy failed) is seen by the next attempt, which never
// subscribed before the end. The order fails with the tour's code.
func TestShipReposSeesTourEndBetweenAttempts(t *testing.T) {
	t.Parallel()
	home := privateHome(t)
	insertCrew(t, home, store.Crew{ID: "sorell", Name: "Sorell"})
	failed := make(chan struct{})
	// The first copy fails; any later one would block for good.
	p := &fakeProvider{hello: true, copyFailOnce: failed, copyGate: make(chan struct{})}
	r := startDaemonWith(t, home, p)
	defer r.stop(t)
	waitOnDeck(t, r.d, "sorell", "fake-1")
	id, err := r.d.CreateOrder(t.Context(), store.Order{Title: "t", Assignee: "sorell",
		Repos: []store.OrderRepo{{Name: "app", BaseRef: "main"}}})
	require.NoError(t, err)
	select {
	case <-failed:
	case <-time.After(20 * time.Second):
		t.Fatal("ship-repos never tried to copy")
	}
	dismiss(t, r.d, tourWorkflowID("sorell", 0))
	res := missionResult(t, r.d, id)
	require.Equal(t, MissionResult{Status: MissionFailed, Reason: "crew member dismissed", Code: CodeDismissed}, res)
	out, ok := stepOutput(t, r.d, missionWorkflowID(id), "ship-repos")
	require.True(t, ok)
	require.JSONEq(t, `{"Repos":null,"TourEnded":true}`, out)
	entries, err := r.d.store.ListLog(t.Context(), 0)
	require.NoError(t, err)
	requireNoBackstop(t, entries)
}

// A tour marked ended before ship-repos starts, or a crew member already
// dismissed when it starts (a tour that ended before a bridge restart),
// ships nothing; the queued synthetic beam_lost parks the mission (#100):
// it stays pending with its order away, nothing settled.
func TestShipReposAfterTourEnded(t *testing.T) {
	t.Parallel()
	home := privateHome(t)
	insertCrew(t, home, store.Crew{ID: "sorell", Name: "Sorell"})
	p := &fakeProvider{blockProvision: true}
	r := startDaemonWith(t, home, p)
	defer r.stop(t)
	ctx := t.Context()
	run := func(t *testing.T, tourID string) string {
		t.Helper()
		id, err := r.d.store.InsertOrder(ctx, store.Order{Title: "t", Assignee: "sorell", State: store.OrderAway,
			Repos: []store.OrderRepo{{Name: "app", BaseRef: "main"}}}, testEntry)
		require.NoError(t, err)
		_, err = dbos.RunWorkflow(r.d.dbos, missionWorkflow, MissionInput{OrderID: id, CrewID: "sorell", TourID: tourID},
			dbos.WithWorkflowID(missionWorkflowID(id)))
		require.NoError(t, err)
		// The tour's synthetic error, queued as endTour does.
		msg, err := comms.NewMessage(comms.TypeError, comms.ErrorBody{Reason: "beam lost", Code: CodeBeamLost})
		require.NoError(t, err)
		msg.Crew, msg.Order = "sorell", id
		require.NoError(t, dbos.Send(r.d.dbos, missionWorkflowID(id), msg, fromCrewTopic))
		require.EventuallyWithT(t, func(c *assert.CollectT) {
			_, ok := stepOutput(c, r.d, missionWorkflowID(id), "park")
			require.True(c, ok)
		}, 20*time.Second, 20*time.Millisecond)
		requirePending(t, r.d, missionWorkflowID(id))
		o := getOrder(t, r.d, id)
		require.Equal(t, store.OrderAway, o.State)
		require.NotNil(t, o.ParkedAt)
		out, ok := stepOutput(t, r.d, missionWorkflowID(id), "ship-repos")
		require.True(t, ok)
		require.JSONEq(t, `{"Repos":null,"TourEnded":true}`, out)
		_, sent := stepOutput(t, r.d, missionWorkflowID(id), "send-order")
		require.False(t, sent)
		return id
	}
	// The tour (still provisioning) only receives the missions' mission_done.
	tourID := tourWorkflowID("sorell", 0)
	// The tour's end first: dismissed is terminal.
	t.Run("tour marked ended", func(t *testing.T) {
		r.d.markTourEnded(tourID)
		run(t, tourID)
	})
	t.Run("crew dismissed", func(t *testing.T) {
		require.NoError(t, r.d.store.UpdateCrewStatus(ctx, "sorell", store.CrewDismissed, testEntry))
		run(t, "")
	})
	require.Empty(t, p.copied())
	entries, err := r.d.store.ListLog(ctx, 0)
	require.NoError(t, err)
	require.Zero(t, kinds(entries)[logFail])
}

// A tour that ends (here, a dismissal) while ship-repos is still copying
// cancels the copy; the mission sends no order and fails the order with the
// tour's code inside the settle wait, not through the tour's backstop.
func TestShipReposStopsWhenTourEnds(t *testing.T) {
	t.Parallel()
	home := privateHome(t)
	insertCrew(t, home, store.Crew{ID: "sorell", Name: "Sorell"})
	p := &fakeProvider{hello: true, copyGate: make(chan struct{})} // the copy never finishes
	r := startDaemonWith(t, home, p)
	defer r.stop(t)
	waitOnDeck(t, r.d, "sorell", "fake-1")
	id, err := r.d.CreateOrder(t.Context(), store.Order{Title: "t", Assignee: "sorell",
		Repos: []store.OrderRepo{{Name: "app", BaseRef: "main"}}})
	require.NoError(t, err)
	local := filepath.Join(home, "cargo", id, "in", "app.bundle")
	require.Eventually(t, func() bool { _, err := os.Stat(local); return err == nil }, 20*time.Second, 20*time.Millisecond,
		"ship-repos is copying")

	start := time.Now()
	dismiss(t, r.d, tourWorkflowID("sorell", 0))
	require.Less(t, time.Since(start), missionSettleTimeout, "the tour didn't wait out its settle timeout")
	res := missionResult(t, r.d, id)
	require.Equal(t, MissionResult{Status: MissionFailed, Reason: "crew member dismissed", Code: CodeDismissed}, res)
	out, ok := stepOutput(t, r.d, missionWorkflowID(id), "ship-repos")
	require.True(t, ok)
	require.JSONEq(t, `{"Repos":null,"TourEnded":true}`, out)
	_, sent := stepOutput(t, r.d, missionWorkflowID(id), "send-order")
	require.False(t, sent)
	require.Empty(t, p.copied())
	entries, err := r.d.store.ListLog(t.Context(), 0)
	require.NoError(t, err)
	requireNoBackstop(t, entries)
	var fails int
	for _, e := range entries {
		if e.Kind == logFail && e.OrderID == id {
			fails++
		}
	}
	require.Equal(t, 1, fails)
}

// order.create (CreateOrder) takes exactly one registered repo with a
// base_ref.
func TestCreateOrderRepos(t *testing.T) {
	t.Parallel()
	home := privateHome(t)
	insertCrew(t, home, store.Crew{ID: "sorell", Name: "Sorell"})
	r := startDaemonWith(t, home, &fakeProvider{blockProvision: true})
	defer r.stop(t)
	ctx := t.Context()
	order := func(repos ...store.OrderRepo) error {
		_, err := r.d.CreateOrder(ctx, store.Order{Title: "t", Assignee: "sorell", Repos: repos})
		return err
	}
	for _, tc := range []struct {
		repos []store.OrderRepo
		err   string
	}{
		{nil, "an order needs repos: one entry, {name, base_ref} (repos: app)"},
		{[]store.OrderRepo{{Name: "app", BaseRef: "main"}, {Name: "app", BaseRef: "dev"}}, "an order takes exactly one repo in Phase 1, got 2"},
		{[]store.OrderRepo{{Name: "web", BaseRef: "main"}}, `unknown repo "web" (repos: app)`},
		{[]store.OrderRepo{{Name: "app"}}, "repo app: base_ref is required"},
		{[]store.OrderRepo{{Name: "app", BaseRef: "--upload-pack=x"}}, "must not start with '-'"},
	} {
		err := order(tc.repos...)
		var re *rpcError
		require.ErrorAs(t, err, &re, "%v", tc.repos)
		require.Equal(t, CodeBadRequest, re.code)
		require.ErrorContains(t, err, tc.err)
	}
	orders, err := r.d.store.ListOrders(ctx)
	require.NoError(t, err)
	require.Empty(t, orders, "nothing inserted for a refused order")
	require.NoError(t, order(store.OrderRepo{Name: "app", BaseRef: "main"}))
}

// stepOutput returns the recorded output (serialized JSON) of the
// workflow's step named name, and whether it has one.
func stepOutput(t require.TestingT, d *Daemon, workflowID, name string) (string, bool) {
	steps, err := dbos.GetWorkflowSteps(d.dbos, workflowID)
	require.NoError(t, err)
	for _, s := range steps {
		if s.StepName == name && s.Error == nil {
			out, _ := s.Output.(string)
			return out, true
		}
	}
	return "", false
}

// The mission's ship-repos step copies a bundle of bmus/<order> at
// base_ref's commit to <work-dir>/<order>/in/<name>.bundle on the beam before
// send-order, logs it and leaves no bundle behind on the bridge. A bridge
// stopped mid-step re-runs it on restart, following a base_ref that moved
// meanwhile, and the order goes out with the recorded repos.
func TestMissionShipsRepos(t *testing.T) {
	t.Parallel()
	home := privateHome(t)
	insertCrew(t, home, store.Crew{ID: "sorell", Name: "Sorell"})
	up := testRepo(t)
	repos := map[string]string{"app": up.url}
	beams := t.TempDir()

	// First daemon: the copy never finishes.
	p1 := &fakeProvider{hello: true, copyRoot: beams, copyGate: make(chan struct{})}
	r := startDaemonRepos(t, home, p1, repos)
	waitOnDeck(t, r.d, "sorell", "fake-1")
	id, err := r.d.CreateOrder(t.Context(), store.Order{Title: "t", Assignee: "sorell",
		Repos: []store.OrderRepo{{Name: "app", BaseRef: "main"}}})
	require.NoError(t, err)
	local := filepath.Join(home, "cargo", id, "in", "app.bundle")
	require.Eventually(t, func() bool { _, err := os.Stat(local); return err == nil }, 20*time.Second, 20*time.Millisecond,
		"the bundle is built before the copy")
	_, ok := stepOutput(t, r.d, missionWorkflowID(id), "ship-repos")
	require.False(t, ok, "ship-repos is still copying")
	r.stop(t)
	require.Empty(t, p1.copied())

	// The base moves while the bridge is down; the re-run follows it.
	moved := up.commit(t, "three")
	p2 := &fakeProvider{hello: true, copyRoot: beams}
	r = startDaemonRepos(t, home, p2, repos)
	defer r.stop(t)
	var shipped string
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		var ok bool
		shipped, ok = stepOutput(c, r.d, missionWorkflowID(id), "ship-repos")
		require.True(c, ok)
		_, ok = stepOutput(c, r.d, missionWorkflowID(id), "send-order")
		require.True(c, ok)
	}, 20*time.Second, 50*time.Millisecond)
	remote := "/beams/fake-1/work/" + id + "/in/app.bundle"
	var got struct {
		Repos     []comms.OrderRepo
		TourEnded bool
	}
	require.NoError(t, json.Unmarshal([]byte(shipped), &got))
	require.Equal(t, []comms.OrderRepo{{Name: "app", BaseRef: "main", BundlePath: remote}}, got.Repos)
	require.False(t, got.TourEnded)
	require.Equal(t, []string{"fake-1:" + remote}, p2.copied())
	require.Equal(t, moved+" refs/heads/bmus/"+id, bundleHeads(t, filepath.Join(beams, remote)))
	_, err = os.Stat(local)
	require.ErrorIs(t, err, os.ErrNotExist, "the bridge's copy of the bundle is removed")
	entries, err := r.d.store.ListLog(t.Context(), 0)
	require.NoError(t, err)
	var shippedLog []string
	for _, e := range entries {
		if strings.HasPrefix(e.Text, "repo app shipped") {
			shippedLog = append(shippedLog, e.Text)
		}
	}
	require.Equal(t, []string{"repo app shipped to the beam at " + moved[:12] + " (main)"}, shippedLog)
	requirePending(t, r.d, missionWorkflowID(id))
}

// A base_ref the repo doesn't have fails the order at once, with
// repo_failed, before anything is sent.
func TestMissionFailsOnMissingBaseRef(t *testing.T) {
	t.Parallel()
	home := privateHome(t)
	insertCrew(t, home, store.Crew{ID: "sorell", Name: "Sorell"})
	p := &fakeProvider{hello: true}
	r := startDaemonWith(t, home, p)
	defer r.stop(t)
	waitOnDeck(t, r.d, "sorell", "fake-1")
	id, err := r.d.CreateOrder(t.Context(), store.Order{Title: "t", Assignee: "sorell",
		Repos: []store.OrderRepo{{Name: "app", BaseRef: "no-such-branch"}}})
	require.NoError(t, err)
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		require.Len(c, workflows(c, r.d, missionWorkflowID(id)), 1)
	}, 10*time.Second, 20*time.Millisecond)
	res := missionResult(t, r.d, id)
	require.Equal(t, MissionFailed, res.Status)
	require.Equal(t, CodeRepoFailed, res.Code)
	require.Contains(t, res.Reason, `could not ship the order's repos to the beam: repo app: base_ref "no-such-branch" not found`)
	require.Equal(t, store.OrderFailed, getOrder(t, r.d, id).State)
	_, sent := stepOutput(t, r.d, missionWorkflowID(id), "send-order")
	require.False(t, sent)
	require.Empty(t, p.copied())
}

// ship-repos starts a resumed order from its newest checkpoint whose bundle
// lands in the clone (#100): a missing or broken one is logged
// work_rejected and an older one tried, base_ref last.
func TestBundleRepoFromCheckpoints(t *testing.T) {
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
	good := filepath.Join(t.TempDir(), "good.bundle")
	_, commit := workBundle(t, shipped, in.OrderID, good)
	broken := filepath.Join(t.TempDir(), "broken.bundle")
	require.NoError(t, os.WriteFile(broken, []byte("not a bundle\n"), 0o600))
	cp := func(beam string, seq int, bundle, commit string) store.Checkpoint {
		return store.Checkpoint{OrderID: in.OrderID, Beam: beam, Seq: seq,
			Repos: []store.CheckpointRepo{{Name: "app", Commit: commit, BundlePath: bundle}}}
	}
	cps := []store.Checkpoint{
		cp("b2", 3, filepath.Join(t.TempDir(), "missing.bundle"), commit),
		cp("b2", 1, broken, commit),
		cp("b1", 4, good, commit),
	}

	out := filepath.Join(t.TempDir(), "out.bundle")
	got, from, err := d.bundleRepoFrom(ctx, in, "app", up.url, "main", out, cps)
	require.NoError(t, err)
	require.Equal(t, commit, got)
	require.NotNil(t, from)
	require.Equal(t, "b1-4", checkpointName(*from))
	require.Equal(t, commit+" refs/heads/bmus/ORD-0001", bundleHeads(t, out))
	clone := filepath.Join(t.TempDir(), "c")
	gitT(t, filepath.Dir(clone), "clone", "-q", "--branch", "bmus/ORD-0001", out, clone)
	require.Equal(t, "3", gitT(t, clone, "rev-list", "--count", "HEAD"), "full history")
	entries, err := st.ListLog(ctx, 0)
	require.NoError(t, err)
	require.Equal(t, map[string]int{CodeWorkRejected: 2}, codes(t, entries))

	got, from, err = d.bundleRepoFrom(ctx, in, "app", up.url, "main", out, cps[:2])
	require.NoError(t, err)
	require.Nil(t, from, "base_ref last")
	require.Equal(t, up.rev(t, "main"), got)
	require.Equal(t, got+" refs/heads/bmus/ORD-0001", bundleHeads(t, out))
}
