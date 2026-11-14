package bridge

// Jettison (#108): the bridge's clones keep one bmus/<order> branch per
// order per repo (repos.go), and each keeps its commits reachable. bmus
// jettison deletes the branches of orders that ended a while ago, and
// nothing else: no other branch, no remote-tracking ref or tag, nothing on
// a remote.

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/tscolari/bmus/internal/store"
)

// DefaultJettisonAge is how long ago an order must have ended for jettison
// to take its branches when no --older-than is given.
const DefaultJettisonAge = "7d"

// jettisonTimeout bounds bridge.jettison.
const jettisonTimeout = 10 * time.Minute

// jettisonLockWait bounds the wait for one repo's lock, which a fetch may
// hold for minutes: past it the repo is skipped, so one busy repo doesn't
// take the whole of jettisonTimeout.
const jettisonLockWait = 2 * time.Minute

// ParseAge reads an age for --older-than: a Go duration ("36h", "90m") or
// a whole number of days ("7d"). It must not be negative.
func ParseAge(s string) (time.Duration, error) {
	if days, ok := strings.CutSuffix(s, "d"); ok {
		n, err := strconv.ParseInt(days, 10, 64)
		if err != nil || n < 0 || n > math.MaxInt64/int64(24*time.Hour) {
			return 0, fmt.Errorf("age %q: want a duration such as 36h, or whole days such as 7d", s)
		}
		return time.Duration(n) * 24 * time.Hour, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("age %q: want a duration such as 36h, or whole days such as 7d", s)
	}
	if d < 0 {
		return 0, fmt.Errorf("age %q is negative", s)
	}
	return d, nil
}

// JettisonArgs are bridge.jettison's args. OlderThan is an age (ParseAge),
// DefaultJettisonAge when empty. Without Yes it is a dry run.
type JettisonArgs struct {
	OlderThan string `json:"older_than,omitempty"`
	Yes       bool   `json:"yes,omitempty"`
}

// JettisonBranch is one order's branch in one of the bridge's clones:
// the order's state and when it ended, and the commit the branch was at.
type JettisonBranch struct {
	Repo    string    `json:"repo"`
	Order   string    `json:"order"`
	Branch  string    `json:"branch"`
	State   string    `json:"state"`
	EndedAt time.Time `json:"ended_at"`
	Commit  string    `json:"commit"`
}

// JettisonSkip is a branch, or a whole repo (Branch empty), that jettison
// left alone for Reason.
type JettisonSkip struct {
	Repo   string `json:"repo"`
	Order  string `json:"order,omitempty"`
	Branch string `json:"branch,omitempty"`
	Reason string `json:"reason"`
}

// JettisonResult is bridge.jettison's result. Branches are the branches
// jettisoned, or that would be on a dry run (Deleted false), by repo then
// order. Kept counts the other branches of orders the store knows: open,
// ended too recently, or named by an open order's base_ref. Skipped are
// the branches and repos left alone for another reason: a branch that
// moved since it was listed, a repo whose clone git can't read or whose
// lock stayed busy.
type JettisonResult struct {
	OlderThan string           `json:"older_than"`
	Deleted   bool             `json:"deleted"`
	Branches  []JettisonBranch `json:"branches"`
	Kept      int              `json:"kept"`
	Skipped   []JettisonSkip   `json:"skipped"`
}

// Jettison finds the bmus/<order> branches in the bridge's clones
// (<home>/repos/<name>) of orders that ended (done, failed, belayed or
// withdrawn) at least olderThan ago, and with yes deletes them. A branch
// whose order the store doesn't have is left alone and not counted.
//
// When an order ended is the time of its latest captain's log entry
// (store.LatestOrderLogTS): every move to an ended state logs one, so it
// is never before the order ended. A branch an open order's base_ref
// resolves to in the clone stays (protectedBranches): a re-run of that
// order's ship-repos resolves it again. So does the branch of an order an
// open order's base_ref names as order:<id> (#143).
//
// With yes, each repo's branches are deleted under its lock
// (jettisonRepo). On an error the result holds the branches deleted
// before it, and the error names how many.
func (d *Daemon) Jettison(ctx context.Context, olderThan time.Duration, yes bool) (JettisonResult, error) {
	out := JettisonResult{Deleted: yes, Branches: []JettisonBranch{}, Skipped: []JettisonSkip{}}
	ents, err := os.ReadDir(filepath.Join(d.home, "repos"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return out, err
	}
	all, err := d.store.ListOrders(ctx)
	if err != nil {
		return out, err
	}
	orders := make(map[string]store.Order, len(all))
	for _, o := range all {
		orders[o.ID] = o
	}
	cutoff := time.Now().Add(-olderThan)
	var found []JettisonBranch
	for _, ent := range ents {
		name := ent.Name()
		dir := d.repoDir(name)
		if !ent.IsDir() || !repoNameRE.MatchString(name) {
			continue
		}
		if _, err := os.Stat(filepath.Join(dir, "HEAD")); err != nil {
			continue // not a clone
		}
		refs, err := runGit(ctx, dir, "for-each-ref", "--format=%(objectname) %(refname)", "refs/heads/bmus/")
		if ctx.Err() != nil {
			return out, ctx.Err()
		}
		if err != nil {
			out.Skipped = append(out.Skipped, JettisonSkip{Repo: name, Reason: firstLine(err.Error())})
			continue
		}
		protected, err := protectedBranches(ctx, dir, name, all)
		if err != nil {
			return out, fmt.Errorf("repo %s: %w", name, err)
		}
		for line := range strings.Lines(refs) {
			commit, ref, _ := strings.Cut(strings.TrimSpace(line), " ")
			branch := strings.TrimPrefix(ref, "refs/heads/")
			o, ok := orders[strings.TrimPrefix(branch, "bmus/")]
			if !ok || branch != orderBranch(o.ID) {
				continue
			}
			if !store.OrderEnded(o.State) || protected[ref] {
				out.Kept++
				continue
			}
			ended, err := d.store.LatestOrderLogTS(ctx, o.ID)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return out, err
			}
			if err != nil || ended.After(cutoff) {
				out.Kept++
				continue
			}
			found = append(found, JettisonBranch{Repo: name, Order: o.ID, Branch: branch, State: o.State, EndedAt: ended, Commit: commit})
		}
	}
	slices.SortFunc(found, func(a, b JettisonBranch) int {
		return cmp.Or(cmp.Compare(a.Repo, b.Repo), cmp.Compare(a.Order, b.Order))
	})
	if !yes {
		out.Branches = append(out.Branches, found...)
		return out, nil
	}
	for repo := range slices.Values(compactRepos(found)) {
		var batch []JettisonBranch
		for _, b := range found {
			if b.Repo == repo {
				batch = append(batch, b)
			}
		}
		deleted, skipped, kept, err := d.jettisonRepo(ctx, repo, batch, orders, jettisonLockWait)
		out.Branches = append(out.Branches, deleted...)
		out.Skipped = append(out.Skipped, skipped...)
		out.Kept += kept
		if err != nil {
			return out, fmt.Errorf("repo %s: %w (%s jettisoned before it)", repo, err, branchCount(len(out.Branches)))
		}
	}
	return out, nil
}

// compactRepos is the repos of bs, sorted by repo, each once.
func compactRepos(bs []JettisonBranch) []string {
	var repos []string
	for _, b := range bs {
		if len(repos) == 0 || repos[len(repos)-1] != b.Repo {
			repos = append(repos, b.Repo)
		}
	}
	return repos
}

// branchCount is "1 branch" or "n branches".
func branchCount(n int) string {
	if n == 1 {
		return "1 branch"
	}
	return fmt.Sprintf("%d branches", n)
}

// jettisonRepo deletes batch, branches of repo, under the repo's lock,
// waiting at most wait for it (past it the batch is skipped).
// Under the lock it reads the open orders' base_refs again and keeps the
// branches they name (kept), drops each branch that moved from its listed
// commit (skipped), then deletes the rest in one `git update-ref --stdin`
// transaction of "delete <ref> <old>". If the transaction fails, each
// branch is deleted on its own, and one whose old-value check fails is
// skipped as moved. Each deletion is logged on its order. deleted holds
// what was deleted, also on an error.
func (d *Daemon) jettisonRepo(ctx context.Context, repo string, batch []JettisonBranch, orders map[string]store.Order, wait time.Duration) (deleted []JettisonBranch, skipped []JettisonSkip, kept int, err error) {
	lctx, cancel := context.WithTimeout(ctx, wait)
	unlock, err := d.lockRepo(lctx, repo)
	cancel()
	if err != nil {
		if ctx.Err() != nil {
			return nil, nil, 0, ctx.Err()
		}
		for _, b := range batch {
			skipped = append(skipped, JettisonSkip{Repo: repo, Order: b.Order, Branch: b.Branch,
				Reason: fmt.Sprintf("the repo's clone stayed busy for %s", wait)})
		}
		return nil, skipped, 0, nil
	}
	defer unlock()
	dir := d.repoDir(repo)

	all, err := d.store.ListOrders(ctx)
	if err != nil {
		return nil, nil, 0, err
	}
	protected, err := protectedBranches(ctx, dir, repo, all)
	if err != nil {
		return nil, nil, 0, err
	}
	refs, err := runGit(ctx, dir, "for-each-ref", "--format=%(objectname) %(refname)", "refs/heads/bmus/")
	if err != nil {
		return nil, nil, 0, err
	}
	now := map[string]string{}
	for line := range strings.Lines(refs) {
		commit, ref, _ := strings.Cut(strings.TrimSpace(line), " ")
		now[ref] = commit
	}
	var todo []JettisonBranch
	for _, b := range batch {
		ref := "refs/heads/" + b.Branch
		switch {
		case protected[ref]:
			kept++
		case now[ref] != b.Commit:
			skipped = append(skipped, movedSkip(b))
		default:
			todo = append(todo, b)
		}
	}
	if len(todo) == 0 {
		return nil, skipped, kept, nil
	}

	var stdin strings.Builder
	for _, b := range todo {
		fmt.Fprintf(&stdin, "delete refs/heads/%s %s\n", b.Branch, b.Commit)
	}
	if _, err := runGitStdin(ctx, dir, strings.NewReader(stdin.String()), "update-ref", "--stdin"); err == nil {
		deleted = todo
	} else {
		if ctx.Err() != nil {
			return nil, skipped, kept, ctx.Err()
		}
		// One at a time: a branch that moved fails only its own delete.
		for _, b := range todo {
			ref := "refs/heads/" + b.Branch
			_, err := runGit(ctx, dir, "update-ref", "-d", ref, b.Commit)
			if err == nil {
				deleted = append(deleted, b)
				continue
			}
			if ctx.Err() != nil {
				return deleted, skipped, kept, d.logJettisoned(ctx, deleted, orders, ctx.Err())
			}
			cur, rerr := runGit(ctx, dir, "rev-parse", "--verify", "--quiet", "--end-of-options", ref)
			if (rerr != nil && refMissing(rerr)) || (rerr == nil && cur != b.Commit) {
				skipped = append(skipped, movedSkip(b))
				continue
			}
			return deleted, skipped, kept, d.logJettisoned(ctx, deleted, orders, err)
		}
	}
	deleted, restored, err := d.restoreBases(ctx, repo, dir, deleted, orders)
	kept += restored
	return deleted, skipped, kept, d.logJettisoned(ctx, deleted, orders, err)
}

// restoreBases reads the open orders' order:<id> bases again after
// jettisonRepo's delete (#143). An order created between the lock's
// re-read and the delete may name a branch just deleted: each such branch
// is created again at its old commit, whose objects are still in the
// clone, and logged on its order as "branch bmus/<id> restored in repo
// <name>: an open order names it as its base". It returns the branches
// still deleted and how many were restored. A restore that fails leaves
// its branch deleted. A logging error is returned with the result.
func (d *Daemon) restoreBases(ctx context.Context, repo, dir string, deleted []JettisonBranch, orders map[string]store.Order) ([]JettisonBranch, int, error) {
	if len(deleted) == 0 {
		return deleted, 0, nil
	}
	all, err := d.store.ListOrders(ctx)
	if err != nil {
		return deleted, 0, err
	}
	named := map[string]bool{}
	for _, o := range all {
		if store.OrderEnded(o.State) {
			continue
		}
		for _, r := range o.Repos {
			if id, ok := orderBase(r.BaseRef); ok && r.Name == repo {
				named[id] = true
			}
		}
	}
	var (
		gone     []JettisonBranch
		restored int
		logErr   error
	)
	for _, b := range deleted {
		if !named[b.Order] {
			gone = append(gone, b)
			continue
		}
		ref := "refs/heads/" + b.Branch
		if _, err := runGitStdin(ctx, dir, strings.NewReader("create "+ref+" "+b.Commit+"\n"), "update-ref", "--stdin"); err != nil {
			gone = append(gone, b)
			continue
		}
		restored++
		if err := d.appendLog(ctx, logOrder, orders[b.Order].Assignee, b.Order,
			fmt.Sprintf("branch %s restored in repo %s: an open order names it as its base", b.Branch, repo),
			map[string]string{"repo": repo, "branch": b.Branch, "commit": b.Commit}); err != nil && logErr == nil {
			logErr = err
		}
	}
	return gone, restored, logErr
}

// movedSkip is the skip of b, which moved from its listed commit.
func movedSkip(b JettisonBranch) JettisonSkip {
	return JettisonSkip{Repo: b.Repo, Order: b.Order, Branch: b.Branch, Reason: "moved from " + b.Commit[:min(len(b.Commit), 12)] + " since it was listed"}
}

// jettisonText is the captain's log text of order id's branch jettisoned
// from repo.
func jettisonText(id, repo string) string {
	return fmt.Sprintf("branch %s jettisoned from repo %s", orderBranch(id), repo)
}

// logJettisoned logs each of deleted on its order, and returns cause or
// the first logging error.
func (d *Daemon) logJettisoned(ctx context.Context, deleted []JettisonBranch, orders map[string]store.Order, cause error) error {
	for _, b := range deleted {
		if err := d.appendLog(ctx, logOrder, orders[b.Order].Assignee, b.Order, jettisonText(b.Order, b.Repo),
			map[string]string{"repo": b.Repo, "branch": b.Branch, "commit": b.Commit}); err != nil && cause == nil {
			cause = err
		}
	}
	return cause
}

// refMissing reports whether err is `git rev-parse --verify --quiet`
// finding no such ref in a readable repository: exit status 1. A missing
// or broken repository exits 128, or fails before git runs.
func refMissing(err error) bool {
	var ee *exec.ExitError
	return errors.As(err, &ee) && ee.ExitCode() == 1
}

// protectedBranches is the refs/heads/bmus/* branches of the clone dir of
// repo that an open order's base_ref names. A base_ref is resolved as git
// would in the clone, after its ~, ^ and @{...} suffixes are stripped,
// with `git rev-parse --symbolic-full-name`; one that resolves to nothing
// protects nothing. A base_ref of order:<id> (#143) protects bmus/<id>.
func protectedBranches(ctx context.Context, dir, repo string, orders []store.Order) (map[string]bool, error) {
	out := map[string]bool{}
	seen := map[string]bool{}
	for _, o := range orders {
		if store.OrderEnded(o.State) {
			continue
		}
		for _, r := range o.Repos {
			if r.Name != repo {
				continue
			}
			if id, ok := orderBase(r.BaseRef); ok {
				out["refs/heads/"+orderBranch(id)] = true
				continue
			}
			ref := r.BaseRef
			if i := strings.IndexAny(ref, "~^"); i >= 0 {
				ref = ref[:i]
			}
			if i := strings.Index(ref, "@{"); i >= 0 {
				ref = ref[:i]
			}
			if ref == "" || seen[ref] {
				continue
			}
			seen[ref] = true
			full, err := runGit(ctx, dir, "rev-parse", "--symbolic-full-name", "--end-of-options", ref)
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			for line := range strings.Lines(full) {
				if line = strings.TrimSpace(line); strings.HasPrefix(line, "refs/heads/bmus/") {
					out[line] = true
				}
			}
			_ = err // a base_ref the clone can't resolve names no branch
		}
	}
	return out, nil
}

// jettisonedAt reports when order id's branch was jettisoned from repo:
// the time of its latest "branch bmus/<id> jettisoned from repo <repo>"
// entry. ok is false when it has none.
func (d *Daemon) jettisonedAt(ctx context.Context, id, repo string) (at time.Time, ok bool, err error) {
	entries, err := d.store.ListOrderLogKind(ctx, id, logOrder)
	if err != nil {
		return time.Time{}, false, err
	}
	text := jettisonText(id, repo)
	for _, e := range slices.Backward(entries) {
		var p struct {
			Repo string `json:"repo"`
		}
		if e.Text == text && json.Unmarshal(e.Payload, &p) == nil && p.Repo == repo {
			return e.TS, true, nil
		}
	}
	return time.Time{}, false, nil
}

// branchJettisoned reports whether order id's branch is gone from repo's
// clone because jettison deleted it: rev-parse's err says the ref is
// missing from a readable clone (refMissing), and the order's log has
// the jettison entry. at is when.
func (d *Daemon) branchJettisoned(ctx context.Context, id, repo string, revParseErr error) (at time.Time, ok bool, err error) {
	if revParseErr == nil || !refMissing(revParseErr) {
		return time.Time{}, false, nil
	}
	return d.jettisonedAt(ctx, id, repo)
}
