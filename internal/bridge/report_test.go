package bridge

import (
	"context"
	"encoding/json"
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

// The console's mission report (#104) over the real daemon: GET
// /api/orders/{id}/report is a 401 without a session, a 404 for an
// unknown order and a 400 for one that isn't done. A done order's report
// carries the order, its crew member, the beam it ran on, its report and
// pushes, and its diff: its work from its base commit, as plain text. The work commits a
// .gitattributes naming a diff driver and a filter driver, which the
// clone's config defines as a command and a textconv, and a process,
// clean and smudge: none runs. git runs no filter for a diff of two
// commits, flags or not; the test keeps it that way.
func TestConsoleOrderReport(t *testing.T) {
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
	report := func(id string, v any) (int, string) {
		t.Helper()
		return consoleSend(t, d.consoleAddr, cookies, csrf, http.MethodGet, "/api/orders/"+id+"/report", "", v)
	}

	id := startMission(t, d)
	branch := orderBranch(id)
	status, code := report(id, nil)
	require.Equal(t, [2]any{http.StatusBadRequest, CodeBadRequest}, [2]any{status, code}, "away")
	status, code = report("ORD-9999", nil)
	require.Equal(t, [2]any{http.StatusNotFound, CodeNotFound}, [2]any{status, code})
	resp, err := http.Get("http://" + d.consoleAddr + "/api/orders/" + id + "/report")
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode)

	work := "/beams/fake-1/work/" + id
	base, commit := workBundleFiles(t, filepath.Join(beams, work, "in", "app.bundle"), id, filepath.Join(beams, work, "out", "app.bundle"),
		map[string]string{
			".gitattributes": "* diff=evil filter=evil\n",
			"work.txt":       "retries added\n<script>alert(1)</script>\n",
		})
	sendReport(t, d, id, 2, comms.ReportBody{Summary: "did it <script>", Artifacts: []string{"notes.md"},
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

	pushed, err := d.PushOrder(t.Context(), id, "", false)
	require.NoError(t, err)
	require.Len(t, pushed, 1)

	// A late usage report still counts: the report's order carries its
	// final usage against its limits (#120).
	_, err = d.store.RecordInbox(t.Context(), "sorell", 0, 101, func(tx store.InboxTx) error {
		_, err := tx.RecordUsage(id, store.Usage{Beam: "fake-1", InputTokens: 3000, OutputTokens: 500, Turns: 8})
		return err
	})
	require.NoError(t, err)

	var got OrderReport
	status, _ = report(id, &got)
	require.Equal(t, http.StatusOK, status)
	require.NoFileExists(t, ranCommand)
	require.NoFileExists(t, ranTextconv)
	require.NoFileExists(t, ranClean)
	require.NoFileExists(t, ranSmudge)
	require.NoFileExists(t, ranProcess)
	require.Equal(t, id, got.Order.ID)
	require.Equal(t, "t", got.Order.Title)
	require.Equal(t, store.OrderDone, got.Order.State)
	require.Equal(t, []OrderRepo{{Name: "app", BaseRef: "main"}}, got.Order.Repos)
	require.Equal(t, UsageView{InputTokens: 3000, OutputTokens: 500, Turns: 8}, got.Order.Usage)
	require.Equal(t, [3]int64{0, 0, 0}, [3]int64{got.Order.LimitTokens, got.Order.LimitTurns, got.Order.BudgetExtended})
	require.Equal(t, BudgetSource{Tokens: SourceDefault, Turns: SourceDefault}, got.Order.BudgetSource)
	require.NotNil(t, got.Order.CreatedAt)
	require.NotNil(t, got.Order.DoneAt)
	require.False(t, got.Order.DoneAt.Before(*got.Order.CreatedAt))
	require.NotNil(t, got.Crew)
	require.Equal(t, "sorell", got.Crew.ID)
	require.Equal(t, "Sorell", got.Crew.Name)
	require.Equal(t, []string{"fake-1"}, got.Beams)
	require.Equal(t, &ReportSummary{Summary: "did it <script>", Artifacts: []string{"notes.md"}}, got.Report)
	require.Len(t, got.Pushes, 1)
	require.Equal(t, ReportPush{Repo: "app", Branch: branch, Commit: commit, At: got.Pushes[0].At}, got.Pushes[0])
	require.False(t, got.Pushes[0].At.IsZero())
	require.Equal(t, consoleDiffCap, got.Cap)
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

// A done order's report reads whatever it has: no crew row (dismissed and
// gone, or never there), no beam in its entries (an order from before
// they were recorded), and no done entry give a nil crew, no beams and a
// nil report, not an error.
func TestOrderReportUnknowns(t *testing.T) {
	t.Parallel()
	r := startDaemonConfig(t, testConfig(privateHome(t)))
	defer r.stop(t)
	d := r.d
	ctx := t.Context()
	bare, err := d.store.InsertOrder(ctx, store.Order{Title: "old", Assignee: "ghost", State: store.OrderDone},
		store.LogEntry{Kind: logOrder, CrewID: "ghost", Text: "order created: old"})
	require.NoError(t, err)
	got, err := d.OrderReport(ctx, bare, consoleDiffCap)
	require.NoError(t, err)
	require.Nil(t, got.Crew)
	require.Nil(t, got.Report)
	require.Nil(t, got.Order.DoneAt)
	require.NotNil(t, got.Order.CreatedAt)
	require.Equal(t, []string{}, got.Beams)
	require.Equal(t, []ReportPush{}, got.Pushes)
	require.Equal(t, []RepoDiff{}, got.Repos)

	old, err := d.store.InsertOrder(ctx, store.Order{Title: "older", State: store.OrderDone}, store.LogEntry{Kind: logOrder, Text: "imported"})
	require.NoError(t, err)
	_, err = d.store.AppendLog(ctx, store.LogEntry{Kind: logDone, OrderID: old, Text: "did it", Payload: json.RawMessage(`{"summary":"did it","repos":[]}`)})
	require.NoError(t, err)
	got, err = d.OrderReport(ctx, old, consoleDiffCap)
	require.NoError(t, err)
	require.Nil(t, got.Crew)
	require.Nil(t, got.Order.CreatedAt)
	require.NotNil(t, got.Order.DoneAt)
	require.Equal(t, []string{}, got.Beams)
	require.Equal(t, &ReportSummary{Summary: "did it", Artifacts: []string{}}, got.Report)

	_, err = d.OrderReport(ctx, "ORD-9999", consoleDiffCap)
	require.ErrorIs(t, err, ErrNotFound)
}

// A review's report carries its verdict (#187), as the done entry holds
// it: findings in order, an empty list for a pass, and agent text as it
// came. A work order's entry with a verdict, and a review's entry with
// one Check refuses, show none.
func TestOrderReportVerdict(t *testing.T) {
	t.Parallel()
	r := startDaemonConfig(t, testConfig(privateHome(t)))
	defer r.stop(t)
	d, ctx := r.d, t.Context()
	const base = "8b1d0e2c4a55f00dfeedfacecafebeef01234567"
	done := func(o store.Order, payload string) string {
		t.Helper()
		o.State = store.OrderDone
		id, err := d.store.InsertOrder(ctx, o, store.LogEntry{Kind: logOrder, Text: "order created: " + o.Title})
		require.NoError(t, err)
		_, err = d.store.AppendLog(ctx, store.LogEntry{Kind: logDone, OrderID: id, Text: "done", Payload: json.RawMessage(payload)})
		require.NoError(t, err)
		return id
	}
	review := func(round int, payload string) string {
		return done(store.Order{Title: "Review o/r#12", Kind: store.OrderKindReview, Task: testTask, Round: round, ReviewBase: base}, payload)
	}
	verdictOf := func(id string) (*comms.Verdict, string) {
		t.Helper()
		got, err := d.OrderReport(ctx, id, consoleDiffCap)
		require.NoError(t, err)
		require.NotNil(t, got.Report)
		b, err := json.Marshal(got.Report)
		require.NoError(t, err)
		return got.Report.Verdict, string(b)
	}

	blocked := review(1, `{"summary":"one must go","repos":[],"verdict":{"findings":[`+
		`{"severity":"high","title":"<script>alert(1)</script> leaks","body":"**bold**","path":"x.go","line":3},`+
		`{"severity":"low","title":"Later","body":"follow up"}]}}`)
	v, raw := verdictOf(blocked)
	require.NotNil(t, v)
	require.Equal(t, []comms.VerdictFinding{
		{Severity: comms.SeverityHigh, Title: "<script>alert(1)</script> leaks", Body: "**bold**", Path: "x.go", Line: 3},
		{Severity: comms.SeverityLow, Title: "Later", Body: "follow up"},
	}, v.Findings)
	require.Contains(t, raw, `"summary":"one must go"`)

	passed := review(2, `{"summary":"all good","repos":[],"verdict":{"findings":[]}}`)
	v, raw = verdictOf(passed)
	require.NotNil(t, v)
	require.Empty(t, v.Findings)
	require.Contains(t, raw, `"verdict":{"findings":[]}`)

	bad := review(3, `{"summary":"odd","repos":[],"verdict":{"findings":[{"severity":"urgent","title":"x","body":""}]}}`)
	v, raw = verdictOf(bad)
	require.Nil(t, v)
	require.NotContains(t, raw, "verdict")

	none := review(4, `{"summary":"no verdict","repos":[]}`)
	v, _ = verdictOf(none)
	require.Nil(t, v)

	work := done(store.Order{Title: "Implement o/r#12", Task: testTask, Round: 1},
		`{"summary":"did it","repos":[],"verdict":{"findings":[]}}`)
	v, raw = verdictOf(work)
	require.Nil(t, v)
	require.NotContains(t, raw, "verdict")
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
