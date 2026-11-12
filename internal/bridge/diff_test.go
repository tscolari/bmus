package bridge

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/tscolari/bmus/internal/comms"
	"github.com/tscolari/bmus/internal/console"
	"github.com/tscolari/bmus/internal/store"
)

// The console's diff (#104) over the real daemon: GET
// /api/orders/{id}/diff is a 401 without a session, a 404 for an unknown
// order and a 400 for one that isn't done. A done order's diff is its
// work from its base commit, as plain text. The work commits a
// .gitattributes naming a diff driver and a filter driver, which the
// clone's config defines as a command and a textconv, and a process,
// clean and smudge: none runs. git runs no filter for a diff of two
// commits, flags or not; the test keeps it that way.
func TestConsoleOrderDiff(t *testing.T) {
	t.Parallel()
	home := privateHome(t)
	insertCrew(t, home, store.Crew{ID: "sorell", Name: "Sorell"})
	up := testRepo(t)
	beams := t.TempDir()
	cfg := testConfig(home)
	cfg.Transporter = &fakeProvider{hello: true, copyRoot: beams}
	cfg.Repos = map[string]string{"app": up.url}
	cfg.NoConsole = false
	r := startDaemonConfig(t, cfg)
	defer r.stop(t)
	d := r.d
	waitOnDeck(t, d, "sorell", "fake-1")
	cookies, csrf := consoleSession(t, d.consoleAddr, home)
	diff := func(id string, v any) (int, string) {
		t.Helper()
		return consoleSend(t, d.consoleAddr, cookies, csrf, http.MethodGet, "/api/orders/"+id+"/diff", "", v)
	}

	id := startMission(t, d)
	branch := orderBranch(id)
	status, code := diff(id, nil)
	require.Equal(t, [2]any{http.StatusBadRequest, CodeBadRequest}, [2]any{status, code}, "away")
	status, code = diff("ORD-9999", nil)
	require.Equal(t, [2]any{http.StatusNotFound, CodeNotFound}, [2]any{status, code})
	resp, err := http.Get("http://" + d.consoleAddr + "/api/orders/" + id + "/diff")
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode)

	work := "/beams/fake-1/work/" + id
	base, commit := workBundleFiles(t, filepath.Join(beams, work, "in", "app.bundle"), id, filepath.Join(beams, work, "out", "app.bundle"),
		map[string]string{
			".gitattributes": "* diff=evil filter=evil\n",
			"work.txt":       "retries added\n<script>alert(1)</script>\n",
		})
	sendReport(t, d, id, 2, comms.ReportBody{Summary: "did it",
		Repos: []comms.WorkRepo{{Name: "app", BundlePath: work + "/out/app.bundle", Commit: commit, Branch: branch}}})
	require.Equal(t, MissionDone, missionResult(t, d, id).Status)

	// The clone's config defines the driver, and reads attributes from the
	// work's tree: a plain git diff runs its command, and one with
	// --no-ext-diff its textconv.
	clone := d.repoDir("app")
	markers := t.TempDir()
	ranCommand, ranTextconv := filepath.Join(markers, "command-ran"), filepath.Join(markers, "textconv-ran")
	gitT(t, clone, "config", "attr.tree", "refs/heads/"+branch)
	gitT(t, clone, "config", "diff.evil.command", fmt.Sprintf("touch %s; true", ranCommand))
	gitT(t, clone, "config", "diff.evil.textconv", fmt.Sprintf("sh -c 'touch %s; cat \"$1\"' -", ranTextconv))
	gitT(t, clone, "diff", base+"..."+branch)
	require.FileExists(t, ranCommand)
	gitT(t, clone, "diff", "--no-ext-diff", base+"..."+branch)
	require.FileExists(t, ranTextconv)
	require.NoError(t, os.Remove(ranCommand))
	require.NoError(t, os.Remove(ranTextconv))

	// The filter driver: smudge, then process once it is set, run on a
	// read with filters.
	ranClean, ranSmudge, ranProcess := filepath.Join(markers, "clean-ran"), filepath.Join(markers, "smudge-ran"), filepath.Join(markers, "process-ran")
	gitT(t, clone, "config", "filter.evil.clean", fmt.Sprintf("sh -c 'touch %s; cat'", ranClean))
	gitT(t, clone, "config", "filter.evil.smudge", fmt.Sprintf("sh -c 'touch %s; cat'", ranSmudge))
	gitT(t, clone, "cat-file", "--filters", "refs/heads/"+branch+":work.txt")
	require.FileExists(t, ranSmudge)
	gitT(t, clone, "config", "filter.evil.process", fmt.Sprintf("sh -c 'touch %s; exit 1'", ranProcess))
	_ = exec.Command("git", "-C", clone, "cat-file", "--filters", "refs/heads/"+branch+":work.txt").Run() // the process fails
	require.FileExists(t, ranProcess)
	require.NoError(t, os.Remove(ranSmudge))
	require.NoError(t, os.Remove(ranProcess))

	var got OrderDiff
	status, _ = diff(id, &got)
	require.Equal(t, http.StatusOK, status)
	require.NoFileExists(t, ranCommand)
	require.NoFileExists(t, ranTextconv)
	require.NoFileExists(t, ranClean)
	require.NoFileExists(t, ranSmudge)
	require.NoFileExists(t, ranProcess)
	require.Equal(t, consoleDiffCap, got.Cap)
	require.Equal(t, id, got.ID)
	require.Equal(t, "t", got.Title)
	require.Equal(t, branch, got.Branch)
	require.Len(t, got.Repos, 1)
	repo := got.Repos[0]
	require.Equal(t, "app", repo.Name)
	require.Equal(t, base, repo.Base)
	require.False(t, repo.Truncated)
	require.Contains(t, repo.Diff, "diff --git a/.gitattributes b/.gitattributes\n")
	require.Contains(t, repo.Diff, "diff --git a/work.txt b/work.txt\n")
	require.Contains(t, repo.Diff, "\n+retries added\n+<script>alert(1)</script>\n")
	require.NotContains(t, repo.Diff, "\x1b[", "no color")
}

// diffRepo makes a repository with main at a base commit and bmus/<order>
// one commit on, adding big.txt with lines lines, and returns its git dir
// and the base.
func diffRepo(t *testing.T, order string, lines int) (gitDir, base string) {
	t.Helper()
	up := testRepo(t)
	base = up.rev(t, "main")
	gitT(t, up.url, "checkout", "-q", "-b", orderBranch(order))
	var b strings.Builder
	for i := range lines {
		fmt.Fprintf(&b, "line %d of the work\n", i)
	}
	require.NoError(t, os.WriteFile(filepath.Join(up.url, "big.txt"), []byte(b.String()), 0o600))
	gitT(t, up.url, "add", "big.txt")
	gitT(t, up.url, "commit", "-q", "-m", "big")
	gitT(t, up.url, "checkout", "-q", "main")
	return filepath.Join(up.url, ".git"), base
}

// A diff over the cap stops at its last whole line under the cap, and
// says so; one under it is whole.
func TestRepoDiffCap(t *testing.T) {
	t.Parallel()
	gitDir, base := diffRepo(t, "ORD-0001", 50000)
	full := gitT(t, gitDir, "diff", base+"...bmus/ORD-0001") + "\n"
	require.Greater(t, len(full), 1<<20)

	const limit = 64 << 10
	out, truncated, err := repoDiff(t.Context(), gitDir, base, "bmus/ORD-0001", limit)
	require.NoError(t, err)
	require.True(t, truncated)
	require.LessOrEqual(t, len(out), limit)
	require.Greater(t, len(out), limit-100)
	require.True(t, strings.HasSuffix(out, "\n"))
	require.True(t, strings.HasPrefix(full, out))

	out, truncated, err = repoDiff(t.Context(), gitDir, base, "bmus/ORD-0001", len(full))
	require.NoError(t, err)
	require.False(t, truncated)
	require.Equal(t, full, out)
}

// A diff cut with no newline under the cap keeps the cap's bytes.
func TestRepoDiffCapNoNewline(t *testing.T) {
	t.Parallel()
	gitDir, base := diffRepo(t, "ORD-0001", 10)
	out, truncated, err := repoDiff(t.Context(), gitDir, base, "bmus/ORD-0001", 16)
	require.NoError(t, err)
	require.True(t, truncated)
	require.Equal(t, "diff --git a/big", out)
}

// A file bigger than consoleDiffCap is binary to the diff: git writes no
// line diff of it.
func TestRepoDiffBigFile(t *testing.T) {
	t.Parallel()
	up := testRepo(t)
	base := up.rev(t, "main")
	gitT(t, up.url, "checkout", "-q", "-b", "bmus/ORD-0001")
	require.NoError(t, os.WriteFile(filepath.Join(up.url, "huge.txt"), []byte(strings.Repeat("x", consoleDiffCap+1)), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(up.url, "small.txt"), []byte("small\n"), 0o600))
	gitT(t, up.url, "add", "huge.txt", "small.txt")
	gitT(t, up.url, "commit", "-q", "-m", "huge")
	out, truncated, err := repoDiff(t.Context(), filepath.Join(up.url, ".git"), base, "bmus/ORD-0001", consoleDiffCap)
	require.NoError(t, err)
	require.False(t, truncated)
	require.Contains(t, out, "diff --git a/huge.txt b/huge.txt\n")
	require.Contains(t, out, "Binary files /dev/null and b/huge.txt differ\n")
	require.NotContains(t, out, "+xxx")
	require.Contains(t, out, "\n+small\n")
}

// repoDiff takes only a full commit id as the base, and git's failure is
// its error.
func TestRepoDiffBase(t *testing.T) {
	t.Parallel()
	gitDir, base := diffRepo(t, "ORD-0001", 1)
	for _, bad := range []string{"", "HEAD", "main", base[:12], "-" + base[1:], strings.ToUpper(base), base + "0"} {
		_, _, err := repoDiff(t.Context(), gitDir, bad, "bmus/ORD-0001", 1<<20)
		require.ErrorContains(t, err, "is not a full commit id", bad)
	}
	_, _, err := repoDiff(t.Context(), gitDir, strings.Repeat("0", 40), "bmus/ORD-0001", 1<<20)
	require.ErrorContains(t, err, "git diff: ")
}

// runGitCapped kills git once it writes past the cap: a diff far bigger
// than the cap returns the cap's worth, git's error is not one, and git
// and its process group are gone, killed rather than finished.
func TestRunGitCappedKillsGit(t *testing.T) {
	t.Parallel()
	gitDir, base := diffRepo(t, "ORD-0001", 200000)
	args := []string{"diff", base + "...bmus/ORD-0001"}
	cmd := gitCommand(t.Context(), gitDir, args...)
	out, truncated, err := cappedOutput(t.Context(), cmd, 4096, args)
	require.NoError(t, err)
	require.True(t, truncated)
	require.Len(t, out, 4096)
	ws, ok := cmd.ProcessState.Sys().(syscall.WaitStatus)
	require.True(t, ok)
	require.True(t, ws.Signaled())
	require.Equal(t, syscall.SIGKILL, ws.Signal())
	require.ErrorIs(t, syscall.Kill(-cmd.Process.Pid, 0), syscall.ESRCH)
	out, truncated, err = runGitCapped(t.Context(), gitDir, 4096, "rev-parse", base)
	require.NoError(t, err)
	require.False(t, truncated)
	require.Equal(t, base+"\n", string(out))
}

// A console read that runs past its time fails with timeout and a
// message that says so, without the error it cut short; the daemon
// stopping ends it, and a stopped daemon refuses it.
func TestConsoleReadTimeout(t *testing.T) {
	t.Parallel()
	src := consoleSource{ctx: t.Context(), actions: &actionGate{}}
	_, err := src.readFor(t.Context(), time.Millisecond, "diff", func(ctx context.Context) (any, error) {
		<-ctx.Done()
		return nil, errors.New("git diff: secret detail: " + ctx.Err().Error())
	})
	var re *rpcError
	require.ErrorAs(t, err, &re)
	require.Equal(t, console.CodeTimeout, re.Code())
	require.Equal(t, "diff timed out after 1ms", re.Error())

	daemon, stop := context.WithCancel(t.Context())
	src = consoleSource{ctx: daemon, actions: &actionGate{}}
	_, err = src.readFor(t.Context(), time.Hour, "diff", func(ctx context.Context) (any, error) {
		stop()
		<-ctx.Done()
		return nil, ctx.Err()
	})
	require.ErrorAs(t, err, &re)
	require.Equal(t, CodeShutdown, re.Code())
	_, err = src.readFor(t.Context(), time.Hour, "diff", func(context.Context) (any, error) { return nil, nil })
	require.ErrorAs(t, err, &re)
	require.Equal(t, CodeShutdown, re.Code())
}
