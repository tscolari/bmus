package bridge

// Pushing an order's finished work (#99, D6) is the captain's own action,
// from the CLI or the console (#4). The bridge pushes bmus/<order> from its
// clone to the registered URL with the captain's git setup (runGit). It
// runs outside any workflow: a push is outward and not idempotent, so
// nothing replays it.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/tscolari/bmus/internal/store"
)

// gitPushTimeout bounds a whole PushOrder, so a server that never answers
// can't hold a repo's lock for good. Generous, as for a fetch: a push sends
// the work's objects. order.push's RPC timeout is the same.
const gitPushTimeout = 10 * time.Minute

// PushedRepo is a repo PushOrder pushed, at Commit, to Branch of URL. URL
// is the registered URL without its userinfo (redactURL): it reaches the
// console and the CLI's --json.
type PushedRepo struct {
	Name   string `json:"name"`
	URL    string `json:"url"`
	Branch string `json:"branch"`
	Commit string `json:"commit"`
}

// redactURL returns a registered repo URL fit to show. A URL with a
// scheme loses its userinfo, user and password alike, since a token may
// stand in the user part (https://<token>@example.com/o/r.git), and its
// query and fragment, which may carry one too. Everything up to the last
// "@" after the scheme goes, so a password holding an unencoded "/", "?"
// or "#" can't survive a parse that ends the authority early; an "@" in a
// path then cuts too much, which hides part of the URL and leaks nothing.
// An scp-like address (git@example.com:o/r.git) and a local path are kept,
// as their user part holds no password.
func redactURL(raw string) string {
	i := strings.Index(raw, "://")
	if i < 0 {
		return raw
	}
	scheme, rest := raw[:i], raw[i+3:]
	if at := strings.LastIndex(rest, "@"); at >= 0 {
		rest = rest[at+1:]
	}
	if u, err := url.Parse(scheme + "://" + rest); err == nil {
		u.User = nil
		u.RawQuery, u.ForceQuery = "", false
		u.Fragment, u.RawFragment = "", ""
		return u.String()
	}
	if j := strings.IndexAny(rest, "?#"); j >= 0 {
		rest = rest[:j]
	}
	return scheme + "://" + rest
}

// ErrPushRejected: the server refused a push, typically a target branch
// that has diverged (not a fast-forward). RPC code CodeConflict.
var ErrPushRejected = errors.New("push rejected")

// PushOrder pushes done order id's work to each repo's registered URL: the
// commit the done entry records for the repo, to refs/heads/<target>,
// forced only with force (unconditionally: not --force-with-lease). target
// defaults to bmus/<order>.
//
// The repos are the done entry's payload's, in order. Before anything is
// pushed, every repo must have been fetched into the clone (no FetchError
// or RejectError) and have bmus/<order> there at the recorded commit; each
// repo's push checks the branch again under its lock. The first failure
// stops the push; the returned list holds the repos pushed before it, which
// the error names too. Each push is logged, success or failure; a refusal
// logs nothing.
func (d *Daemon) PushOrder(ctx context.Context, id, target string, force bool) ([]PushedRepo, error) {
	if target == "" {
		target = orderBranch(id)
	}
	if err := checkBranchName(ctx, target); err != nil {
		return nil, err
	}
	o, err := d.store.GetOrder(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("order %s: %w", id, ErrNotFound)
	}
	if err != nil {
		return nil, err
	}
	if o.State != store.OrderDone {
		return nil, badRequest("order %s is %s; only a done order's work can be pushed", id, o.State)
	}
	e, err := d.store.LastOrderLog(ctx, id, logDone)
	if err != nil {
		return nil, fmt.Errorf("order %s's done entry: %w", id, err)
	}
	var p donePayload
	if err := json.Unmarshal(e.Payload, &p); err != nil {
		return nil, fmt.Errorf("order %s's done entry: %w", id, err)
	}
	if len(p.Repos) == 0 {
		return nil, badRequest("order %s brought back no work to push", id)
	}
	for _, r := range p.Repos {
		switch {
		case r.RejectError != "":
			return nil, badRequest("order %s: repo %s's work was rejected and never fetched into the bridge's clone (%s); its bundle is at %s",
				id, r.Name, r.RejectError, r.Bundle)
		case r.FetchError != "":
			return nil, badRequest("order %s: repo %s's work could not be fetched into the bridge's clone (%s); its bundle is at %s",
				id, r.Name, r.FetchError, r.Bundle)
		}
		if _, ok := d.repos[r.Name]; !ok {
			return nil, badRequest("repo %s is not registered on this bridge (repos: %s)", r.Name, d.repoNames())
		}
		if err := d.checkOrderBranch(ctx, id, r); err != nil {
			return nil, err
		}
	}

	ctx, cancel := context.WithTimeout(ctx, gitPushTimeout)
	defer cancel()
	var pushed []PushedRepo
	for _, r := range p.Repos {
		ok, err := d.pushRepo(ctx, o, r, target, force)
		if ok {
			pushed = append(pushed, PushedRepo{Name: r.Name, URL: redactURL(d.repos[r.Name]), Branch: target, Commit: r.Commit})
		}
		if err != nil {
			if len(pushed) > 0 && !ok {
				names := make([]string, len(pushed))
				for i, pr := range pushed {
					names[i] = pr.Name
				}
				err = fmt.Errorf("%w (pushed before it: %s)", err, strings.Join(names, ", "))
			}
			return pushed, err
		}
	}
	return pushed, nil
}

// checkOrderBranch refuses repo r when bmus/<order> in the bridge's clone
// is missing or not at r's recorded commit. A branch bmus jettison
// deleted (branchJettisoned) is refused saying so (#108).
func (d *Daemon) checkOrderBranch(ctx context.Context, orderID string, r landedRepo) error {
	branch := "refs/heads/" + orderBranch(orderID)
	got, err := runGit(ctx, d.repoDir(r.Name), "rev-parse", "--verify", "--quiet", "--end-of-options", branch+"^{commit}")
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err == nil && got == r.Commit {
		return nil
	}
	at, jettisoned, jerr := d.branchJettisoned(ctx, orderID, r.Name, err)
	if jerr != nil {
		return jerr
	}
	if jettisoned {
		return badRequest("%s was jettisoned from repo %s on %s; its work is no longer on the bridge",
			orderBranch(orderID), r.Name, at.UTC().Format(time.DateOnly))
	}
	state := "missing"
	if err == nil {
		state = "at " + got
	}
	return badRequest("repo %s: %s in the bridge's clone is %s, not at the reported %s; refusing to push",
		r.Name, orderBranch(orderID), state, r.Commit)
}

// pushRepo pushes order o's work on repo r, under the repo's lock: see
// PushOrder. pushed reports that the server took the push, even when
// logging it then failed.
//
// It pushes the verified commit itself, not the branch, so nothing can
// move between the check and the push, to the registered URL rather than
// origin, so no remote.origin.pushurl redirects it (the captain's
// url.*.pushInsteadOf still applies). --no-follow-tags and
// --no-recurse-submodules keep the captain's push.followTags and
// push.recurseSubmodules from sending anything else: the clone holds
// upstream's tags. The URL goes into no message: runGit's errors carry
// only git's output, where git hides credentials. PushedRepo carries it
// redacted.
func (d *Daemon) pushRepo(ctx context.Context, o store.Order, r landedRepo, target string, force bool) (pushed bool, err error) {
	unlock, err := d.lockRepo(ctx, r.Name)
	if err != nil {
		return false, err
	}
	defer unlock()
	if err := d.checkOrderBranch(ctx, o.ID, r); err != nil {
		return false, err
	}
	args := []string{"push", "--no-follow-tags", "--no-recurse-submodules"}
	if force {
		args = append(args, "--force")
	}
	args = append(args, "--end-of-options", d.repos[r.Name], r.Commit+":refs/heads/"+target)
	_, err = runGit(ctx, d.repoDir(r.Name), args...)

	// The push may have reached the server even when ctx ended: log it
	// regardless.
	lctx := context.WithoutCancel(ctx)
	payload := map[string]any{"repo": r.Name, "target": target, "commit": r.Commit, "force": force}
	short := r.Commit[:min(len(r.Commit), 12)]
	if err != nil {
		payload["reason"] = err.Error()
		text := fmt.Sprintf("repo %s: push to %s at %s failed", r.Name, target, short)
		if strings.Contains(err.Error(), "! [rejected]") {
			err = &rpcError{code: CodeConflict, err: fmt.Errorf("%w: %w", ErrPushRejected, err)}
		}
		err = fmt.Errorf("repo %s: %w", r.Name, err)
		if lerr := d.appendLog(lctx, logOrder, o.Assignee, o.ID, text, payload); lerr != nil {
			return false, errors.Join(err, lerr)
		}
		return false, err
	}
	text := fmt.Sprintf("repo %s pushed to %s at %s", r.Name, target, short)
	if force {
		text += " (forced)"
	}
	if err := d.appendLog(lctx, logOrder, o.Assignee, o.ID, text, payload); err != nil {
		return true, fmt.Errorf("repo %s was pushed to %s at %s, but logging it failed: %w", r.Name, target, r.Commit, err)
	}
	return true, nil
}

// checkBranchName refuses a target branch git wouldn't take as a branch
// name, one starting with '-', HEAD or @. It checks refs/heads/<name> with
// `git check-ref-format`, which needs no repository: --branch would expand
// "@{-1}" in whatever repository the bridge was started from.
func checkBranchName(ctx context.Context, name string) error {
	if strings.HasPrefix(name, "-") || name == "HEAD" || name == "@" {
		return badRequest("target branch %q is not a valid branch name", name)
	}
	if _, err := runGit(ctx, "", "check-ref-format", "refs/heads/"+name); err != nil {
		if ctx.Err() != nil {
			return err
		}
		return badRequest("target branch %q is not a valid branch name", name)
	}
	return nil
}
