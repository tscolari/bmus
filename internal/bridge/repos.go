package bridge

// Repos go into a beam as bundles built on the bridge (#51, D6). Beams hold
// no git credentials: only the bridge talks to a git server, through its own
// clone of each registered repo and the captain's usual git setup.
//
// The bridge's clone of repo <name> is the bare repository
// <home>/repos/<name>. Its remote origin is the registered URL, fetched into
// refs/remotes/origin/* (and tags into refs/tags/*), so the clone's own
// branches are only the bridge's: bmus/<order>, one per order, created at the
// order's base commit when the order is shipped, and moved to the finished
// work when the order reports (fetch-work, work.go). The captain can add the clone as a remote of a checkout.

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/tscolari/bmus/internal/comms"
	"github.com/tscolari/bmus/internal/store"
	"github.com/tscolari/bmus/internal/transporter"
)

// repoNameRE is what a registered repo name must match: it names a
// directory on the bridge (<home>/repos/<name>) and on the beam
// (<work-dir>/<order>/<name>). The protocol schema's order_repo.name has the
// same pattern.
var repoNameRE = regexp.MustCompile(`^[A-Za-z0-9_-][A-Za-z0-9._-]*$`)

// reservedRepoNameRE matches names taken by other files under the beam's
// <work-dir>/<order>/: in/ holds the bundles going in, out/ the bundles
// coming back (#53), numbered dirs the checkpoints. combadge refuses them
// too.
var reservedRepoNameRE = regexp.MustCompile(`^(in|out|[0-9]+)$`)

// maxOrderRepos is how many repos an order may name. Orders carry a list
// from the start, but Phase 1 takes exactly one.
const maxOrderRepos = 1

// CodeRepoFailed: the mission couldn't fetch, bundle or copy an order's
// repo onto the beam.
const CodeRepoFailed = "repo_failed"

// gitWaitDelay bounds how long a git command may take to exit after SIGTERM
// (sent when its step's context ends) before it is killed.
const gitWaitDelay = 10 * time.Second

// gitFetchTimeout bounds each fetch into a clone, so a server that never
// answers can't hold the repo's lock for good. Generous: the first fetch of
// a large repo takes a while. A variable so tests can shorten it.
var gitFetchTimeout = 10 * time.Minute

// errRepoPermanent marks ship-repos failures retrying can't fix: an unknown
// repo name or a base_ref the fetched repo doesn't have.
var errRepoPermanent = errors.New("permanent")

// CheckRepos validates a repo registry: each name must match repoNameRE and
// not reservedRepoNameRE, and each URL be non-empty and not start with '-',
// which git would read as an option (#105).
func CheckRepos(repos map[string]string) error {
	for name, url := range repos {
		if !repoNameRE.MatchString(name) {
			return fmt.Errorf("repo name %q: use letters, digits, '.', '_' and '-', not starting with '.'", name)
		}
		if reservedRepoNameRE.MatchString(name) {
			return fmt.Errorf("repo name %q is reserved: \"in\", \"out\" and numbers name other files of an order", name)
		}
		if strings.TrimSpace(url) == "" {
			return fmt.Errorf("repo %s: the URL is empty", name)
		}
		if strings.HasPrefix(url, "-") {
			return fmt.Errorf("repo %s: the URL %q starts with '-'; git would read it as an option", name, url)
		}
	}
	return nil
}

// repoDir is the bridge's clone of repo name: <home>/repos/<name>.
func (d *Daemon) repoDir(name string) string {
	return filepath.Join(d.home, "repos", name)
}

// registeredRepos is the registry's names, sorted; empty, never nil, when
// there are none, so the console's snapshot sends [] (#211).
func (d *Daemon) registeredRepos() []string {
	names := make([]string, 0, len(d.repos))
	for n := range d.repos {
		names = append(names, n)
	}
	slices.Sort(names)
	return names
}

// repoNames is the registry's names, sorted, for error messages.
func (d *Daemon) repoNames() string {
	names := d.registeredRepos()
	if len(names) == 0 {
		return "none; register one with `bmus bridge run --repo <name>=<url>`"
	}
	return strings.Join(names, ", ")
}

// checkOrderRepos validates an order's repos against the registry. Phase 1
// takes exactly one.
func (d *Daemon) checkOrderRepos(repos []store.OrderRepo) error {
	if len(repos) == 0 {
		return badRequest("an order needs repos: one entry, {name, base_ref} (repos: %s)", d.repoNames())
	}
	if len(repos) > maxOrderRepos {
		return badRequest("an order takes exactly one repo in Phase 1, got %d", len(repos))
	}
	seen := map[string]bool{}
	for _, r := range repos {
		if _, ok := d.repos[r.Name]; !ok {
			return badRequest("unknown repo %q (repos: %s)", r.Name, d.repoNames())
		}
		if seen[r.Name] {
			return badRequest("repo %s is listed twice", r.Name)
		}
		seen[r.Name] = true
		if strings.TrimSpace(r.BaseRef) == "" {
			return badRequest("repo %s: base_ref is required", r.Name)
		}
		if strings.HasPrefix(r.BaseRef, "-") {
			return badRequest("repo %s: base_ref %q must not start with '-'", r.Name, r.BaseRef)
		}
		if id, ok := orderBase(r.BaseRef); ok && !orderIDRE.MatchString(id) {
			return badRequest("repo %s: base_ref %q: %s takes an order's id, such as %sORD-0001", r.Name, r.BaseRef, orderBasePrefix, orderBasePrefix)
		}
	}
	return nil
}

// orderBasePrefix starts a base_ref naming another order's work (#143):
// order:<id> starts the work from the commit order <id>'s done entry
// records for the same repo.
const orderBasePrefix = store.OrderBasePrefix

// orderIDRE matches an order id as the store makes them (ORD-%04d).
var orderIDRE = regexp.MustCompile(`^ORD-[0-9]{4,}$`)

// orderBase returns the order id a base_ref of the form order:<id> names,
// and whether it has that form. git refs can't hold ':', so no git ref
// has it.
func orderBase(baseRef string) (string, bool) {
	return strings.CutPrefix(baseRef, orderBasePrefix)
}

// checkOrderBases checks the repos whose base_ref is order:<id> (#143)
// against the store and the bridge's clone, and returns those orders' ids,
// each once, for the new order's after. The named order must exist and
// must not have ended failed, belayed or withdrawn, nor have been split
// (splitWhy): a split order brings back no work of its own. A done order's
// done entry must name the repo with its work fetched into the clone
// (orderWork), its branch must not have been jettisoned, and the commit
// must be in the clone. An open order must list the repo among its own
// repos; its work is checked when the new order is shipped. Each refusal
// is a bad request.
func (d *Daemon) checkOrderBases(ctx context.Context, repos []store.OrderRepo) ([]string, error) {
	var ids []string
	for _, r := range repos {
		id, ok := orderBase(r.BaseRef)
		if !ok {
			continue
		}
		refuse := func(why string) error { return badRequest("repo %s: base_ref %s: %s", r.Name, r.BaseRef, why) }
		o, err := d.store.GetOrder(ctx, id)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, refuse("no such order")
		}
		if err != nil {
			return nil, err
		}
		if o.State == store.OrderFailed || o.State == store.OrderBelayed || o.State == store.OrderWithdrawn {
			return nil, refuse("it ended " + o.State + " and will never be done")
		}
		split, err := d.splitWhy(ctx, id)
		if err != nil {
			return nil, err
		}
		switch {
		case split != "":
			return nil, refuse(split)
		case o.State == store.OrderSplit:
			return nil, refuse("it was split, so its work is in its child orders; name one of them")
		case o.State == store.OrderDone:
			why, err := d.doneBaseWhy(ctx, id, r.Name)
			if err != nil {
				return nil, err
			}
			if why != "" {
				return nil, refuse(why)
			}
		case !slices.ContainsFunc(o.Repos, func(or store.OrderRepo) bool { return or.Name == r.Name }):
			return nil, refuse(fmt.Sprintf("%s doesn't work on repo %s", id, r.Name))
		}
		if !slices.Contains(ids, id) {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

// splitWhy says, for an order with child orders, that it was split and
// into which (store.SplitBaseText); "" for an order with none.
func (d *Daemon) splitWhy(ctx context.Context, id string) (string, error) {
	children, err := d.store.ChildIDs(ctx, id)
	if err != nil || len(children) == 0 {
		return "", err
	}
	return store.SplitBaseText(id, children), nil
}

// doneBaseWhy says why done order id's work on repo can't be a new
// order's base at create: orderWork's reasons, its branch jettisoned
// (branchJettisoned), or its commit missing from the clone. "" when it
// can.
func (d *Daemon) doneBaseWhy(ctx context.Context, id, repo string) (string, error) {
	commit, why, err := d.orderWork(ctx, id, repo)
	if err != nil || why != "" {
		return why, err
	}
	dir := d.repoDir(repo)
	_, rerr := runGit(ctx, dir, "rev-parse", "--verify", "--quiet", "--end-of-options", "refs/heads/"+orderBranch(id)+"^{commit}")
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	at, jettisoned, err := d.branchJettisoned(ctx, id, repo, rerr)
	if err != nil {
		return "", err
	}
	if jettisoned {
		return fmt.Sprintf("%s's branch was jettisoned on %s; its work may be gone", id, at.UTC().Format(time.DateOnly)), nil
	}
	if _, err := runGit(ctx, dir, "rev-parse", "--verify", "--quiet", "--end-of-options", commit+"^{commit}"); err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return fmt.Sprintf("%s's commit %s is not in the bridge's clone of %s", id, commit[:12], repo), nil
	}
	return "", nil
}

// orderWork returns the commit done order id's done entry records for
// repo: its work as fetched into the bridge's clone. why says, when it
// isn't there, what is missing: no done entry (an order settled done
// without a report of its own), no work on repo, work that was rejected
// or never fetched, or no full commit id. err is for a store that can't
// be read.
func (d *Daemon) orderWork(ctx context.Context, id, repo string) (commit, why string, err error) {
	e, err := d.store.LastOrderLog(ctx, id, logDone)
	if errors.Is(err, sql.ErrNoRows) {
		return "", id + " brought back no work of its own", nil
	}
	if err != nil {
		return "", "", fmt.Errorf("order %s's done entry: %w", id, err)
	}
	var p donePayload
	if err := json.Unmarshal(e.Payload, &p); err != nil {
		return "", "", fmt.Errorf("order %s's done entry: %w", id, err)
	}
	i := slices.IndexFunc(p.Repos, func(l landedRepo) bool { return l.Name == repo })
	if i < 0 {
		return "", fmt.Sprintf("%s brought back no work on repo %s", id, repo), nil
	}
	l := p.Repos[i]
	switch {
	case l.RejectError != "":
		return "", fmt.Sprintf("%s's work on repo %s was rejected and never fetched into the bridge's clone", id, repo), nil
	case l.FetchError != "":
		return "", fmt.Sprintf("%s's work on repo %s could not be fetched into the bridge's clone", id, repo), nil
	case !fullCommitRE.MatchString(l.Commit):
		return "", fmt.Sprintf("%s's done entry has no commit for repo %s", id, repo), nil
	}
	return l.Commit, "", nil
}

// orderBaseCommit resolves base_ref order:<id> for repo name, whose clone
// is dir, to the commit order <id>'s done entry records for the repo
// (#143). The order must be done and not split (splitWhy), its done entry
// must name the repo (orderWork), and the commit must be in the clone
// with every object it reaches (`git rev-list --objects`), so the bundle
// can be made. Each of those failing is errRepoPermanent: the mission
// fails with repo_failed instead of retrying.
func (d *Daemon) orderBaseCommit(ctx context.Context, dir, name, baseRef string) (string, error) {
	id, _ := orderBase(baseRef)
	permanent := func(why string) error { return fmt.Errorf("base_ref %s: %s: %w", baseRef, why, errRepoPermanent) }
	o, err := d.store.GetOrder(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", permanent("no such order")
	}
	if err != nil {
		return "", err
	}
	split, err := d.splitWhy(ctx, id)
	if err != nil {
		return "", err
	}
	if split != "" {
		return "", permanent(split)
	}
	if o.State != store.OrderDone {
		return "", permanent(fmt.Sprintf("%s is %s, not done", id, o.State))
	}
	commit, why, err := d.orderWork(ctx, id, name)
	if err != nil {
		return "", err
	}
	if why != "" {
		return "", permanent(why)
	}
	if _, err := runGit(ctx, dir, "rev-parse", "--verify", "--quiet", "--end-of-options", commit+"^{commit}"); err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		if refMissing(err) {
			return "", permanent(fmt.Sprintf("%s's commit %s is not in the bridge's clone of %s", id, commit[:12], name))
		}
		return "", err
	}
	if _, err := runGit(ctx, dir, "rev-list", "--objects", "--quiet", "--end-of-options", commit); err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", permanent(fmt.Sprintf("%s's work at %s is incomplete in the bridge's clone of %s: %s", id, commit[:12], name, firstLine(err.Error())))
	}
	return commit, nil
}

// orderBranch is the branch an order works on, in the bridge's clone and on
// the beam.
func orderBranch(orderID string) string { return "bmus/" + orderID }

// shipped is the ship-repos step's recorded outcome.
type shipped struct {
	// Repos are the order's repos as the order command carries them.
	Repos []comms.OrderRepo
	// Handover is the note of the checkpoint the repos were shipped from,
	// for the order command; empty when they were shipped from base_ref.
	Handover string `json:",omitempty"`
	// Beam is the beam the repos were shipped to, and the order goes to:
	// its checkpoints and work are pulled from it.
	Beam string `json:",omitempty"`
	// TourEnded: the crew member's tour ended (sickbay, dismissal) before
	// the repos were shipped. The mission then sends no order and reads
	// the tour's synthetic error instead.
	TourEnded bool `json:",omitempty"`
}

// errTourEnded is the cause ship-repos' work is cancelled with when the
// crew member's tour ends.
var errTourEnded = errors.New("the crew member's tour ended")

// shipRepos is the ship-repos step: for each of the order's repos it points
// bmus/<order> in the bridge's clone at where the work starts, bundles that
// branch with its full history and copies the bundle onto the crew member's
// beam at <work-dir>/<order>/in/<name>.bundle.
//
// The work starts from the order's latest checkpoint (#100): a resumed
// order picks up on its new beam where the old one stopped. When the order
// has no checkpoint, or the check of every checkpoint's bundle rejects it
// (see bundleRepoFrom), it starts from base_ref, or from another order's
// work for a base_ref of order:<id> (#143). The checkpoint's note goes
// into the order as its handover. Orders take one repo, so the first
// repo's checkpoint is the order's.
//
// A re-run (a retry, or the bridge restarting mid-step) does it all again:
// the fetch may move base_ref, and then bmus/<order> and the bundle follow
// it, replacing the copy on the beam. Nothing reads that copy before
// send-order, which runs only after this step is recorded, and a replay
// reuses the recorded repos.
//
// The work can take long (a first fetch, a large copy). If the mission's
// tour ends meanwhile (endTour marks it, markTourEnded, after queueing the
// mission its synthetic error) the work is cancelled and the step returns
// TourEnded, so the mission settles or parks the order with the tour's code
// inside the tour's settle wait instead of being backstopped. A tour
// already marked, on this attempt's start or a later retry's, returns
// TourEnded at once, before anything else; so does a crew row that shows
// the tour ended (tourOver: sickbay, dismissed or a later tour, for a tour
// that ended before a bridge restart). A shutdown that races the tour's
// end returns ctx's error, not TourEnded: nothing is recorded and the
// re-run after the restart decides.
func shipRepos(ctx context.Context, d *Daemon, in MissionInput) (shipped, error) {
	ended := d.tourEnded(in.TourID)
	select {
	case <-ended:
		return shipped{TourEnded: true}, nil
	default:
	}
	o, err := d.store.GetOrder(ctx, in.OrderID)
	if err != nil {
		return shipped{}, fmt.Errorf("order %s: %w", in.OrderID, err)
	}
	out := shipped{Repos: make([]comms.OrderRepo, 0, len(o.Repos))}
	if len(o.Repos) == 0 {
		return out, nil
	}
	c, err := d.store.GetCrew(ctx, in.CrewID)
	if err != nil {
		return shipped{}, err
	}
	if tourOver(c, in) {
		return shipped{TourEnded: true}, nil
	}
	if c.CurrentBeam == "" {
		return shipped{}, fmt.Errorf("crew %s has no beam", in.CrewID)
	}
	beam := transporter.Beam{Alias: c.CurrentBeam}
	out.Beam = beam.Alias
	workDir, err := d.transporter.WorkDir(beam)
	if err != nil {
		return shipped{}, err
	}
	cps, err := d.store.CheckpointsNewestFirst(ctx, in.OrderID)
	if err != nil {
		return shipped{}, err
	}

	wctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	go func() {
		select {
		case <-ended:
			cancel(errTourEnded)
		case <-wctx.Done():
		}
	}()
	tourEnded := func() bool { return ctx.Err() == nil && errors.Is(context.Cause(wctx), errTourEnded) }

	for i, r := range o.Repos {
		url, ok := d.repos[r.Name]
		if !ok {
			return shipped{}, fmt.Errorf("repo %s is not registered on this bridge (repos: %s): %w", r.Name, d.repoNames(), errRepoPermanent)
		}
		local := filepath.Join(d.home, "cargo", in.OrderID, "in", r.Name+".bundle")
		commit, from, err := d.bundleRepoFrom(wctx, in, r.Name, url, r.BaseRef, local, cps)
		if tourEnded() {
			os.Remove(local)
			return shipped{TourEnded: true}, nil
		}
		if err != nil {
			return shipped{}, fmt.Errorf("repo %s: %w", r.Name, err)
		}
		start := r.BaseRef
		if from != nil {
			start = "checkpoint " + checkpointName(*from)
			if i == 0 {
				note, err := os.ReadFile(from.HandoverPath)
				if err != nil {
					return shipped{}, fmt.Errorf("checkpoint %s's handover note: %w", checkpointName(*from), err)
				}
				out.Handover = strings.TrimSpace(string(note))
			}
		}
		remote := path.Join(workDir, in.OrderID, "in", r.Name+".bundle")
		err = d.transporter.CopyTo(wctx, beam, local, remote)
		os.Remove(local) // rebuilt on a re-run; a bundle holds the full history
		if tourEnded() {
			return shipped{TourEnded: true}, nil
		}
		if err != nil {
			return shipped{}, fmt.Errorf("copy repo %s's bundle to %s: %w", r.Name, beam.Alias, err)
		}
		out.Repos = append(out.Repos, comms.OrderRepo{Name: r.Name, BaseRef: r.BaseRef, BundlePath: remote})
		if err := d.appendLog(ctx, logOrder, in.CrewID, in.OrderID,
			fmt.Sprintf("repo %s shipped to the beam at %s (%s)", r.Name, commit[:min(len(commit), 12)], start),
			map[string]string{"repo": r.Name, "base_ref": r.BaseRef, "commit": commit, "bundle_path": remote, "from": start, "beam": beam.Alias}); err != nil {
			return shipped{}, err
		}
	}
	return out, nil
}

// checkpointName names a checkpoint as its cargo bay dir does:
// <beam>-<seq>.
func checkpointName(cp store.Checkpoint) string {
	return cp.Beam + "-" + strconv.Itoa(cp.Seq)
}

// bundleRepo fetches repo name's clone from url (creating it if needed),
// points refs/heads/bmus/<order> at baseRef's commit and writes a bundle of
// that branch, with its full history, to out. It returns the commit.
//
// baseRef is looked up as origin's branch first (refs/remotes/origin/<ref>),
// then as git resolves it in the clone: a tag, a commit id,
// origin/<branch>, or another order's bmus/<order> branch.
func (d *Daemon) bundleRepo(ctx context.Context, name, url, orderID, baseRef, out string) (string, error) {
	commit, _, err := d.bundleRepoFrom(ctx, MissionInput{OrderID: orderID}, name, url, baseRef, out, nil)
	return commit, err
}

// bundleRepoFrom is bundleRepo starting from the order's checkpoints, cps,
// newest first (#100). Under the repo's lock it fetches the newest
// checkpoint's bundle of repo name from the cargo bay into the clone as
// bmus/<order> (fetchBundleLocked: the object check, and the branch at the
// checkpoint's commit) and bundles that branch. A checkpoint the fetch
// can't land for good (errWorkPermanent: a rejected or broken bundle, or
// one missing from the cargo bay) is logged as work_rejected and the next
// older one is tried, and base_ref last, as bundleRepo does. It returns the
// commit and the checkpoint shipped from, nil for base_ref. A checkpoint
// without a bundle for name is skipped.
//
// A base_ref of order:<id> (#143) starts from the commit order <id>'s
// done entry records for the repo (orderBaseCommit), with no fetch: that
// work is in the clone already. It is read only when no checkpoint is
// shipped, so an order that resumes from its own checkpoint never looks
// at it again.
func (d *Daemon) bundleRepoFrom(ctx context.Context, in MissionInput, name, url, baseRef, out string, cps []store.Checkpoint) (string, *store.Checkpoint, error) {
	unlock, err := d.lockRepo(ctx, name)
	if err != nil {
		return "", nil, err
	}
	defer unlock()

	dir := d.repoDir(name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", nil, err
	}
	// Each command is idempotent, so a clone left half set up by a killed
	// step is finished by the next run.
	if _, err := runGit(ctx, "", "init", "--quiet", "--bare", dir); err != nil {
		return "", nil, err
	}
	for _, args := range [][]string{
		{"config", "remote.origin.url", url},
		{"config", "--replace-all", "remote.origin.fetch", "+refs/heads/*:refs/remotes/origin/*"},
	} {
		if _, err := runGit(ctx, dir, args...); err != nil {
			return "", nil, err
		}
	}
	branch := "refs/heads/" + orderBranch(in.OrderID)

	var (
		commit string
		from   *store.Checkpoint
	)
	for i := range cps {
		cp := &cps[i]
		j := slices.IndexFunc(cp.Repos, func(r store.CheckpointRepo) bool { return r.Name == name })
		if j < 0 {
			continue
		}
		r := cp.Repos[j]
		_, err := os.Stat(r.BundlePath)
		if errors.Is(err, os.ErrNotExist) {
			err = fmt.Errorf("%w: %w", err, errWorkPermanent)
		} else if err == nil {
			err = d.fetchBundleLocked(ctx, name, in.OrderID, r.BundlePath, r.Commit)
		}
		if err == nil {
			commit, from = r.Commit, cp
			break
		}
		if !errors.Is(err, errWorkPermanent) {
			return "", nil, err
		}
		text := fmt.Sprintf("checkpoint %s of repo %s not shipped, trying an older start: %s", checkpointName(*cp), name, firstLine(err.Error()))
		if err := d.appendLog(ctx, logOrder, in.CrewID, in.OrderID, text,
			map[string]any{"reason": text, "code": CodeWorkRejected, "beam": cp.Beam, "seq": cp.Seq}); err != nil {
			return "", nil, err
		}
	}

	if _, ok := orderBase(baseRef); ok && from == nil {
		// Another order's work (#143) is already in the clone: no fetch.
		if commit, err = d.orderBaseCommit(ctx, dir, name, baseRef); err != nil {
			return "", nil, err
		}
		if _, err := runGit(ctx, dir, "update-ref", branch, commit); err != nil {
			return "", nil, err
		}
	} else if from == nil {
		// Tags are forced too: an upstream that moves a tag must not make every
		// later fetch fail with "would clobber existing tag".
		fctx, cancel := context.WithTimeout(ctx, gitFetchTimeout)
		_, err = runGit(fctx, dir, "fetch", "--quiet", "--prune", "--no-tags", "origin",
			"+refs/heads/*:refs/remotes/origin/*", "+refs/tags/*:refs/tags/*")
		timedOut := ctx.Err() == nil && fctx.Err() != nil
		cancel()
		if err != nil {
			if timedOut {
				return "", nil, fmt.Errorf("git fetch from %s took over %s: %w", url, gitFetchTimeout, err)
			}
			return "", nil, err
		}
		for _, rev := range []string{"refs/remotes/origin/" + baseRef, baseRef} {
			if c, err := runGit(ctx, dir, "rev-parse", "--verify", "--quiet", "--end-of-options", rev+"^{commit}"); err == nil {
				commit = c
				break
			}
			if ctx.Err() != nil {
				return "", nil, ctx.Err()
			}
		}
		if commit == "" {
			return "", nil, fmt.Errorf("base_ref %q not found in %s: %w", baseRef, url, errRepoPermanent)
		}
		if _, err := runGit(ctx, dir, "update-ref", branch, commit); err != nil {
			return "", nil, err
		}
	}
	if err := os.MkdirAll(filepath.Dir(out), 0o700); err != nil {
		return "", nil, err
	}
	tmp := out + ".tmp"
	if _, err := runGit(ctx, dir, "bundle", "create", "--quiet", tmp, branch); err != nil {
		os.Remove(tmp)
		return "", nil, err
	}
	if err := os.Rename(tmp, out); err != nil {
		return "", nil, err
	}
	return commit, from, nil
}

// lockRepo takes the lock serialising git work on repo name's clone, so two
// missions shipping the same repo don't fetch into it at once. It gives up
// when ctx ends. The returned func releases it.
func (d *Daemon) lockRepo(ctx context.Context, name string) (func(), error) {
	d.repoLocksMu.Lock()
	sem, ok := d.repoLocks[name]
	if !ok {
		sem = make(chan struct{}, 1)
		d.repoLocks[name] = sem
	}
	d.repoLocksMu.Unlock()
	select {
	case sem <- struct{}{}:
		return func() { <-sem }, nil
	case <-ctx.Done():
		return nil, fmt.Errorf("waiting for repo %s's clone: %w", name, context.Cause(ctx))
	}
}

// gitLocalEnvVars are the environment variables that point git at a
// repository other than the one it runs in (`git rev-parse
// --local-env-vars`). runGit drops them, so a bridge started from inside a
// git hook or with GIT_DIR set can't touch another repo.
var gitLocalEnvVars = sync.OnceValue(func() map[string]bool {
	vars := map[string]bool{}
	// The list as of git 2.43, in case the command fails.
	for _, v := range strings.Fields(`GIT_ALTERNATE_OBJECT_DIRECTORIES GIT_CONFIG GIT_CONFIG_PARAMETERS
		GIT_CONFIG_COUNT GIT_OBJECT_DIRECTORY GIT_DIR GIT_WORK_TREE GIT_IMPLICIT_WORK_TREE GIT_GRAFT_FILE
		GIT_INDEX_FILE GIT_NO_REPLACE_OBJECTS GIT_REPLACE_REF_BASE GIT_PREFIX GIT_SHALLOW_FILE GIT_COMMON_DIR`) {
		vars[v] = true
	}
	if out, err := exec.Command("git", "rev-parse", "--local-env-vars").Output(); err == nil {
		for _, v := range strings.Fields(string(out)) {
			vars[v] = true
		}
	}
	return vars
})

// gitEnv is the environment runGit gives git: the bridge's own, minus
// gitLocalEnvVars and GIT_CONFIG_KEY_<n>/GIT_CONFIG_VALUE_<n>, plus
// GIT_TERMINAL_PROMPT=0 and LC_ALL=C. LC_ALL=C keeps git's messages in
// English for the matches in classifyBundleErr, verifyBundleObjects and
// pushRepo (#105). It is appended last and overrides an inherited LC_ALL.
func gitEnv() []string {
	local := gitLocalEnvVars()
	env := []string{"GIT_TERMINAL_PROMPT=0"}
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if local[k] || k == "GIT_TERMINAL_PROMPT" ||
			strings.HasPrefix(k, "GIT_CONFIG_KEY_") || strings.HasPrefix(k, "GIT_CONFIG_VALUE_") {
			continue
		}
		env = append(env, kv)
	}
	return append(env, "LC_ALL=C")
}

// runGit runs git on the repository gitDir (with --git-dir; none when
// empty) and returns its trimmed stdout. It uses the captain's own git and
// configuration, credential helpers and ssh config included, in gitEnv.
//
// Nothing can wait on a prompt: stdin is /dev/null, GIT_TERMINAL_PROMPT=0
// stops git's own prompts, and git runs in a new session with no
// controlling terminal, so ssh can't ask on /dev/tty (an unknown host key, a
// passphrase) and fails instead. When ctx ends git's process group gets
// SIGTERM, which lets git remove its lock files; git is killed gitWaitDelay
// later, and then the rest of the group.
//
// Automatic gc and maintenance run in the foreground: detached, they would
// outlive this call, the lock and the timeout, and still be writing to the
// clone after the step returned.
func runGit(ctx context.Context, gitDir string, args ...string) (string, error) {
	return runGitStdin(ctx, gitDir, nil, args...)
}

// runGitStdin is runGit, piping stdin to git from a Reader instead of
// /dev/null (git index-pack --stdin, #65's object check). A nil stdin
// keeps runGit's own behavior (os/exec gives git /dev/null).
func runGitStdin(ctx context.Context, gitDir string, stdin io.Reader, args ...string) (string, error) {
	cmd := gitCommand(ctx, gitDir, args...)
	cmd.Stdin = stdin
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := runGitCmd(ctx, cmd, &stderr, args); err != nil {
		return "", err
	}
	return strings.TrimSpace(stdout.String()), nil
}

// gitCommand is the git command runGit runs, args on the repository
// gitDir, before its stdin and outputs are set: see runGit.
func gitCommand(ctx context.Context, gitDir string, args ...string) *exec.Cmd {
	full := []string{"-c", "gc.autoDetach=false", "-c", "maintenance.autoDetach=false"}
	if gitDir != "" {
		full = append(full, "--git-dir="+gitDir)
	}
	full = append(full, args...)
	cmd := exec.CommandContext(ctx, "git", full...)
	if gitDir != "" {
		cmd.Dir = gitDir
	}
	cmd.Env = gitEnv()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	// git leads its own process group (Setsid): signal the group, so the
	// ssh or helper it started goes too.
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM) }
	cmd.WaitDelay = gitWaitDelay
	return cmd
}

// runGitCmd runs cmd, from gitCommand with args, and returns its error
// with git's stderr, which cmd writes to stderr. The error names git's
// command, after any leading -c options.
func runGitCmd(ctx context.Context, cmd *exec.Cmd, stderr fmt.Stringer, args []string) error {
	err := cmd.Run()
	if ctx.Err() != nil && cmd.Process != nil {
		// git is gone (it exited, or WaitDelay killed it); kill whatever of
		// its group outlived the SIGTERM.
		syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	if err == nil {
		return nil
	}
	return gitError(ctx, err, stderr, args)
}

// gitError is the error of git run with args that failed with err: with
// the context's cause when ctx ended, and git's stderr. It names git's
// command, after any leading -c options.
func gitError(ctx context.Context, err error, stderr fmt.Stringer, args []string) error {
	if ctx.Err() != nil {
		err = fmt.Errorf("%w (%w)", context.Cause(ctx), err)
	}
	i := 0
	for i+2 < len(args) && args[i] == "-c" {
		i += 2
	}
	name := args[i]
	if msg := strings.TrimSpace(stderr.String()); msg != "" {
		return fmt.Errorf("git %s: %w: %s", name, err, msg)
	}
	return fmt.Errorf("git %s: %w", name, err)
}

// gitStderrCap caps how much of git's stderr runGitCapped keeps.
const gitStderrCap = 64 << 10

// runGitCapped is runGit for output that may be large, such as a diff
// (#104): see cappedOutput.
func runGitCapped(ctx context.Context, gitDir string, limit int, args ...string) (out []byte, truncated bool, err error) {
	return cappedOutput(ctx, gitCommand(ctx, gitDir, args...), limit, args)
}

// cappedOutput runs cmd, from gitCommand with args, and reads at most
// limit+1 bytes of its stdout as git writes it, returning them
// untrimmed. With more than limit, it keeps limit, kills git's process
// group and reports truncated: git is never left to write the rest, and
// nothing past limit+1 bytes is held.
//
// git's stdout is a pipe read here, before Wait: git can't be reaped while
// it is read, so its pid, which leads its group, is still its own when
// the group is killed. A kill takes the lock and checks that Wait hasn't
// started. When ctx ends, cmd.Cancel sends the group SIGTERM, which ends
// the read; whatever holds the pipe open gitWaitDelay later is killed.
func cappedOutput(ctx context.Context, cmd *exec.Cmd, limit int, args []string) (out []byte, truncated bool, err error) {
	pr, pw, err := os.Pipe()
	if err != nil {
		return nil, false, err
	}
	defer pr.Close()
	stderr := &cappedWriter{limit: gitStderrCap}
	cmd.Stdout, cmd.Stderr = pw, stderr
	err = cmd.Start()
	pw.Close()
	if err != nil {
		return nil, false, gitError(ctx, err, stderr, args)
	}
	var (
		mu      sync.Mutex
		waiting bool
	)
	kill := func() {
		mu.Lock()
		defer mu.Unlock()
		if !waiting {
			syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
	}
	var late *time.Timer
	stop := context.AfterFunc(ctx, func() {
		mu.Lock()
		defer mu.Unlock()
		late = time.AfterFunc(gitWaitDelay, kill)
	})
	out, rerr := io.ReadAll(io.LimitReader(pr, int64(limit)+1))
	if len(out) > limit {
		out, truncated = out[:limit], true
		kill()
	}
	pr.Close()
	stop()
	mu.Lock()
	waiting = true
	if late != nil {
		late.Stop()
	}
	mu.Unlock()
	werr := cmd.Wait()
	if truncated {
		// The kill is the error, if any; the output is what was asked for.
		return out, true, nil
	}
	if werr == nil {
		werr = rerr
	}
	if werr != nil {
		return nil, false, gitError(ctx, werr, stderr, args)
	}
	return out, false, nil
}

// cappedWriter keeps the first limit bytes written to it and drops the
// rest.
type cappedWriter struct {
	buf   bytes.Buffer
	limit int
}

func (w *cappedWriter) Write(p []byte) (int, error) {
	w.buf.Write(p[:min(len(p), max(w.limit-w.buf.Len(), 0))])
	return len(p), nil
}

func (w *cappedWriter) String() string { return w.buf.String() }
