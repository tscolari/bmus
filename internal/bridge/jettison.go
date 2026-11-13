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
	"errors"
	"fmt"
	"math"
	"os"
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

// jettisonTimeout bounds bridge.jettison: deleting waits for each repo's
// lock, which a fetch may hold for up to gitFetchTimeout.
const jettisonTimeout = 10 * time.Minute

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

// JettisonResult is bridge.jettison's result. Branches are the branches
// jettisoned, or that would be on a dry run (Deleted false), by repo then
// order. Kept counts the other branches of orders the store knows: open,
// ended too recently, or named as an open order's base_ref.
type JettisonResult struct {
	OlderThan string           `json:"older_than"`
	Deleted   bool             `json:"deleted"`
	Branches  []JettisonBranch `json:"branches"`
	Kept      int              `json:"kept"`
}

// Jettison finds the bmus/<order> branches in the bridge's clones
// (<home>/repos/<name>) of orders that ended (done, failed, belayed or
// withdrawn) at least olderThan ago, and with yes deletes them. A branch
// whose order the store doesn't have is left alone and not counted.
//
// When an order ended is the time of its latest captain's log entry
// (store.LatestOrderLogTS): every move to an ended state logs one, so it
// is never before the order ended. A branch an open order names as its
// base_ref stays: a re-run of that order's ship-repos resolves it again.
//
// Each deletion is `git update-ref -d` of the branch at the commit it was
// listed at, under the repo's lock, and is logged on the order.
func (d *Daemon) Jettison(ctx context.Context, olderThan time.Duration, yes bool) (JettisonResult, error) {
	out := JettisonResult{Deleted: yes, Branches: []JettisonBranch{}}
	ents, err := os.ReadDir(filepath.Join(d.home, "repos"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return out, err
	}
	all, err := d.store.ListOrders(ctx)
	if err != nil {
		return out, err
	}
	orders := make(map[string]store.Order, len(all))
	baseOf := map[string]bool{} // branches open orders name as base_ref
	for _, o := range all {
		orders[o.ID] = o
		if store.OrderEnded(o.State) {
			continue
		}
		for _, r := range o.Repos {
			baseOf[r.Name+"\x00"+strings.TrimPrefix(r.BaseRef, "refs/heads/")] = true
		}
	}
	cutoff := time.Now().Add(-olderThan)
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
			if !store.OrderEnded(o.State) || baseOf[name+"\x00"+branch] {
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
			out.Branches = append(out.Branches, JettisonBranch{Repo: name, Order: o.ID, Branch: branch, State: o.State, EndedAt: ended, Commit: commit})
		}
	}
	slices.SortFunc(out.Branches, func(a, b JettisonBranch) int {
		return cmp.Or(cmp.Compare(a.Repo, b.Repo), cmp.Compare(a.Order, b.Order))
	})
	if !yes {
		return out, nil
	}
	for i, b := range out.Branches {
		if err := d.jettisonBranch(ctx, b, orders[b.Order].Assignee); err != nil {
			out.Branches = out.Branches[:i]
			return out, fmt.Errorf("repo %s: jettison %s: %w", b.Repo, b.Branch, err)
		}
	}
	return out, nil
}

// jettisonBranch deletes b's branch from its clone, if it is still at b's
// commit, and logs it on the order.
func (d *Daemon) jettisonBranch(ctx context.Context, b JettisonBranch, crewID string) error {
	unlock, err := d.lockRepo(ctx, b.Repo)
	if err != nil {
		return err
	}
	_, err = runGit(ctx, d.repoDir(b.Repo), "update-ref", "-d", "refs/heads/"+b.Branch, b.Commit)
	unlock()
	if err != nil {
		return err
	}
	return d.appendLog(ctx, logOrder, crewID, b.Order, fmt.Sprintf("branch %s jettisoned from repo %s", b.Branch, b.Repo),
		map[string]string{"repo": b.Repo, "branch": b.Branch, "commit": b.Commit})
}
