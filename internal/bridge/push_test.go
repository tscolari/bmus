package bridge

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/tscolari/bmus/internal/comms"
	"github.com/tscolari/bmus/internal/store"
)

// bareOrigin makes a bare clone of up to stand in for a git server, and
// returns its path.
func bareOrigin(t *testing.T, up upstream) string {
	t.Helper()
	bare := filepath.Join(t.TempDir(), "app.git")
	gitT(t, filepath.Dir(bare), "clone", "-q", "--bare", up.url, bare)
	return bare
}

// pushLog returns the texts of orderID's push entries.
func pushLog(t *testing.T, d *Daemon, orderID string) []string {
	t.Helper()
	var out []string
	for _, e := range orderLog(t, d, orderID) {
		if e.Kind == logOrder && (strings.Contains(e.Text, " pushed to ") || strings.Contains(e.Text, ": push to ")) {
			out = append(out, e.Text)
		}
	}
	return out
}

// #99: PushOrder pushes a done order's bmus/<order> to the registered
// file:// origin, to bmus/<order> or --target-branch; --force overwrites a
// diverged target, which without it git refuses. An order that isn't done,
// a clone branch that moved, and a bad target name are refused. Only the
// work's commit is sent: no tags, wherever the captain's config points.
func TestPushOrder(t *testing.T) {
	t.Parallel()
	home := privateHome(t)
	insertCrew(t, home, store.Crew{ID: "sorell", Name: "Sorell"})
	up := testRepo(t)
	bare := bareOrigin(t, up)
	beams := t.TempDir()
	r := startDaemonRepos(t, home, &fakeProvider{hello: true, copyRoot: beams}, map[string]string{"app": "file://" + bare})
	defer r.stop(t)
	waitOnDeck(t, r.d, "sorell", "fake-1")
	ctx := t.Context()
	id := startMission(t, r.d)
	branch := orderBranch(id)

	_, err := r.d.PushOrder(ctx, id, "", false)
	require.ErrorContains(t, err, "order "+id+" is away; only a done order's work can be pushed")
	require.Equal(t, CodeBadRequest, errorCode(ctx, err))

	work := "/beams/fake-1/work/" + id
	_, commit := workBundle(t, filepath.Join(beams, work, "in", "app.bundle"), id, filepath.Join(beams, work, "out", "app.bundle"))
	sendReport(t, r.d, id, 2, comms.ReportBody{Summary: "did it",
		Repos: []comms.WorkRepo{{Name: "app", BundlePath: work + "/out/app.bundle", Commit: commit, Branch: branch}}})
	require.Equal(t, MissionDone, missionResult(t, r.d, id).Status)

	// The default target: bmus/<order>. The captain's push.followTags
	// sends no tag along, though the work carries an annotated one, and
	// remote.origin.pushurl doesn't redirect the push.
	clone := filepath.Join(home, "repos", "app")
	gitT(t, clone, "config", "push.followTags", "true")
	gitT(t, clone, "config", "remote.origin.pushurl", filepath.Join(t.TempDir(), "nowhere.git"))
	gitT(t, clone, "tag", "-a", "-m", "evil", "evil", commit)
	pushed, err := r.d.PushOrder(ctx, id, "", false)
	require.NoError(t, err)
	require.Equal(t, []PushedRepo{{Name: "app", URL: "file://" + bare, Branch: branch, Commit: commit}}, pushed)
	require.Equal(t, commit, gitT(t, bare, "rev-parse", branch))
	require.Empty(t, gitT(t, bare, "for-each-ref", "refs/tags"), "no tag pushed")

	// A diverged target: refused without --force, with git's error.
	gitT(t, up.url, "checkout", "-q", "-b", "feature", "main~1")
	diverged := up.commit(t, "elsewhere")
	gitT(t, up.url, "push", "-q", bare, "feature")
	_, err = r.d.PushOrder(ctx, id, "feature", false)
	require.ErrorContains(t, err, "repo app: push rejected: git push")
	require.ErrorContains(t, err, "[rejected]")
	require.ErrorIs(t, err, ErrPushRejected)
	require.Equal(t, CodeConflict, errorCode(ctx, err))
	require.Equal(t, diverged, gitT(t, bare, "rev-parse", "feature"))
	pushed, err = r.d.PushOrder(ctx, id, "feature", true)
	require.NoError(t, err)
	require.Equal(t, []PushedRepo{{Name: "app", URL: "file://" + bare, Branch: "feature", Commit: commit}}, pushed)
	require.Equal(t, commit, gitT(t, bare, "rev-parse", "feature"))

	short := commit[:12]
	require.Equal(t, []string{
		"repo app pushed to " + branch + " at " + short,
		"repo app: push to feature at " + short + " failed",
		"repo app pushed to feature at " + short + " (forced)",
	}, pushLog(t, r.d, id))

	// bmus/<order> moved in the bridge's clone: refused, nothing logged.
	base := gitT(t, clone, "rev-parse", branch+"~1")
	gitT(t, clone, "update-ref", "refs/heads/"+branch, base)
	_, err = r.d.PushOrder(ctx, id, "other", false)
	require.ErrorContains(t, err, "repo app: "+branch+" in the bridge's clone is at "+base+", not at the reported "+commit+"; refusing to push")
	require.Equal(t, CodeBadRequest, errorCode(ctx, err))
	gitT(t, clone, "update-ref", "-d", "refs/heads/"+branch)
	_, err = r.d.PushOrder(ctx, id, "other", false)
	require.ErrorContains(t, err, "in the bridge's clone is missing")
	require.Len(t, pushLog(t, r.d, id), 3)
	require.Empty(t, gitT(t, bare, "for-each-ref", "refs/heads/other"))

	for _, name := range []string{"-f", "a..b", "a b", "x.lock", "HEAD", "@", "@{-1}", "a:b"} {
		_, err := r.d.PushOrder(ctx, id, name, false)
		require.ErrorContains(t, err, "is not a valid branch name", name)
		require.Equal(t, CodeBadRequest, errorCode(ctx, err), name)
	}
	_, err = r.d.PushOrder(ctx, "ORD-9999", "", false)
	require.ErrorIs(t, err, ErrNotFound)
}

// A done order whose work couldn't be fetched into the bridge's clone has
// nothing to push: refused, naming the bundle, and nothing reaches origin.
func TestPushOrderFetchFailed(t *testing.T) {
	t.Parallel()
	home := privateHome(t)
	insertCrew(t, home, store.Crew{ID: "sorell", Name: "Sorell"})
	bare := bareOrigin(t, testRepo(t))
	beams := t.TempDir()
	r := startDaemonRepos(t, home, &fakeProvider{hello: true, copyRoot: beams}, map[string]string{"app": "file://" + bare})
	defer r.stop(t)
	waitOnDeck(t, r.d, "sorell", "fake-1")
	id := startMission(t, r.d)
	work := "/beams/fake-1/work/" + id
	base, _ := workBundle(t, filepath.Join(beams, work, "in", "app.bundle"), id, filepath.Join(beams, work, "out", "app.bundle"))
	sendReport(t, r.d, id, 2, comms.ReportBody{Summary: "did it",
		Repos: []comms.WorkRepo{{Name: "app", BundlePath: work + "/out/app.bundle", Commit: base, Branch: orderBranch(id)}}})
	require.Equal(t, MissionDone, missionResult(t, r.d, id).Status)

	_, err := r.d.PushOrder(context.Background(), id, "", false)
	require.ErrorContains(t, err, "order "+id+": repo app's work could not be fetched into the bridge's clone (")
	require.ErrorContains(t, err, "its bundle is at "+filepath.Join(home, "cargo", id, "final", "app.bundle"))
	require.Empty(t, gitT(t, bare, "for-each-ref", "refs/heads/bmus"))
	require.Empty(t, pushLog(t, r.d, id))
}

// A pushed repo's URL is shown without credentials: a URL with a scheme
// loses its userinfo, query and fragment, cut at the last "@" so a
// password with "/", "?" or "#" in it doesn't survive; an scp-like address
// and a local path are kept.
func TestRedactURL(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		"https://user:secret@example.com/o/r.git":   "https://example.com/o/r.git",
		"https://<token>@example.com/o/r.git":       "https://example.com/o/r.git",
		"https://user:p@ss@example.com:8443/o/r":    "https://example.com:8443/o/r",
		"https://example.com/o/r.git":               "https://example.com/o/r.git",
		"ssh://git@example.com/o/r.git":             "ssh://example.com/o/r.git",
		"ssh://git:secret@example.com:2222/o/r.git": "ssh://example.com:2222/o/r.git",
		"git@example.com:o/r.git":                   "git@example.com:o/r.git",
		"file:///srv/git/app.git":                   "file:///srv/git/app.git",
		"/srv/git/app.git":                          "/srv/git/app.git",
		"https://user:secret@exa mple.com/o/r":      "https://exa mple.com/o/r",
		// A password with an unencoded "/", "?" or "#".
		"https://user:se/cret@host/x":   "https://host/x",
		"https://user:se?cret@host/x":   "https://host/x",
		"https://user:se#cret@host/x":   "https://host/x",
		"https://user:12/secret@host/x": "https://host/x",
		"https://user:pa/ss@host/x":     "https://host/x",
		"https://user:pa?ss@host/x":     "https://host/x",
		"https://user:pa#ss@host/x":     "https://host/x",
		// A token in the query or fragment.
		"https://h/x?access_token=<token>":     "https://h/x",
		"https://h/x?secret#frag":              "https://h/x",
		"https://u:secret@host:bad/x?secret=1": "https://host:bad/x",
		// The review's probes.
		"https://us%40er:pa%40secret@host/x": "https://host/x",
		"ssh://user:secret@host/x":           "ssh://host/x",
		"git+ssh://user:secret@host/x":       "git+ssh://host/x",
		"https://secret@host/x":              "https://host/x",
		"HTTPS://u:secret@host":              "https://host",
		"file://u:secret@/srv/x":             "file:///srv/x",
		"https://u:secret@[::1]:8080/x":      "https://[::1]:8080/x",
		"https://u:secret@host:bad/x":        "https://host:bad/x",
	} {
		got := redactURL(in)
		require.Equal(t, want, got, in)
		require.NotContains(t, got, "secret", in)
		require.NotContains(t, got, "token", in)
	}
}
