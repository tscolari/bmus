package bridge

// Finished work comes back as bundles (#53, D6). A report carries one
// bundle per repo of the order. The mission pulls them into the cargo bay
// (pull-work) before the order can be done, then fetches each into the
// bridge's clone as branch bmus/<order> (fetch-work). Nothing is pushed:
// that stays the captain's own action.

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/tscolari/bmus/internal/comms"
	"github.com/tscolari/bmus/internal/store"
	"github.com/tscolari/bmus/internal/transporter"
)

const (
	// CodeCargoLost: a report's bundles couldn't be pulled into the cargo
	// bay, so the order's work never reached the bridge. The order fails.
	CodeCargoLost = "cargo_lost"
	// CodeFetchFailed: a pulled bundle couldn't be fetched into the
	// bridge's clone (logged; the order is still done, its work in the
	// cargo bay).
	CodeFetchFailed = "fetch_failed"
	// CodeWorkRejected: a pulled bundle's objects failed #65's check
	// (index-pack --strict) and were never fetched into the bridge's
	// clone (logged; the order is still done, the bundle left in the
	// cargo bay).
	CodeWorkRejected = "work_rejected"
	// CodeBundleTooLarge: a report's or checkpoint's bundle was over
	// Config.MaxBundleBytes (#65) and was never copied off the beam.
	CodeBundleTooLarge = "bundle_too_large"

	// DefaultMaxBundleBytes is Config.MaxBundleBytes's default: 2 GiB.
	DefaultMaxBundleBytes = 2 * 1024 * 1024 * 1024
)

// errWorkPermanent marks pull-work and fetch-work failures retrying can't
// fix: a report that doesn't match the order's repos, or a bundle that
// doesn't hold the commit it was reported with.
var errWorkPermanent = errors.New("permanent")

// errBundleRejected marks a fetchBundle failure as #65's object check
// (index-pack --strict), not an ordinary bad-bundle or commit-mismatch
// failure, so fetch-work logs it as work_rejected instead of
// fetch_failed. It wraps errWorkPermanent, so the existing retry
// predicates that check only errWorkPermanent still treat it as
// permanent.
var errBundleRejected = fmt.Errorf("the bundle's objects: %w", errWorkPermanent)

// errBundleTooLarge marks a pull-work/pull-checkpoint failure as #65's
// size cap, not an ordinary copy failure, so the mission logs it as
// bundle_too_large instead of cargo_lost/checkpoint_failed. It wraps
// errWorkPermanent: retrying can't shrink the bundle.
var errBundleTooLarge = fmt.Errorf("the bundle is over the size cap: %w", errWorkPermanent)

// cargoFinal is the cargo bay dir of an order's final bundles:
// <home>/cargo/<order>/final/.
func (d *Daemon) cargoFinal(orderID string) string {
	return filepath.Join(d.home, "cargo", orderID, "final")
}

// landedRepo is one repo's work in the cargo bay, as pull-work records it.
type landedRepo struct {
	Name   string `json:"name"`
	Branch string `json:"branch"`
	Commit string `json:"commit"`
	// Bundle is the bundle's path in the cargo bay.
	Bundle string `json:"bundle_path"`
	// FetchError is why fetch-work couldn't land the work on Branch in the
	// bridge's clone; the bundle is still in the cargo bay.
	FetchError string `json:"fetch_error,omitempty"`
	// RejectError is why fetch-work's object check (#65) refused the
	// bundle; the bundle is still in the cargo bay, and bmus/<order> in
	// the bridge's clone wasn't moved.
	RejectError string `json:"reject_error,omitempty"`
}

// matchWork pairs an order's repos with a report's or checkpoint's bundles:
// exactly one per order repo, on bmus/<order>, with a commit and an
// absolute path on the beam. The result follows the order's repo order.
// Anything else is errWorkPermanent.
func matchWork(orderID string, repos []store.OrderRepo, work []comms.WorkRepo) ([]comms.WorkRepo, error) {
	byName := map[string]comms.WorkRepo{}
	for _, w := range work {
		if _, dup := byName[w.Name]; dup {
			return nil, fmt.Errorf("repo %s has two bundles: %w", w.Name, errWorkPermanent)
		}
		byName[w.Name] = w
	}
	out := make([]comms.WorkRepo, 0, len(repos))
	for _, r := range repos {
		w, ok := byName[r.Name]
		if !ok {
			return nil, fmt.Errorf("no bundle for repo %s: %w", r.Name, errWorkPermanent)
		}
		delete(byName, r.Name)
		if w.Branch != orderBranch(orderID) {
			return nil, fmt.Errorf("repo %s: the bundle carries %q, not %s: %w", r.Name, w.Branch, orderBranch(orderID), errWorkPermanent)
		}
		if w.Commit == "" || strings.HasPrefix(w.Commit, "-") {
			return nil, fmt.Errorf("repo %s: bad commit %q: %w", r.Name, w.Commit, errWorkPermanent)
		}
		if !path.IsAbs(w.BundlePath) {
			return nil, fmt.Errorf("repo %s: bundle path %q is not absolute: %w", r.Name, w.BundlePath, errWorkPermanent)
		}
		out = append(out, w)
	}
	for name := range byName {
		return nil, fmt.Errorf("bundle for repo %s, which the order doesn't have: %w", name, errWorkPermanent)
	}
	return out, nil
}

// missionBeam is the beam a mission's checkpoints and work come from: the
// one it shipped the order to (alias), which a watch change's next-tour and
// the next provision take off crew.current_beam while a pull may still be
// running (#100). Without one (a ship-repos recorded with no repos) it is
// the crew member's current beam.
func (d *Daemon) missionBeam(ctx context.Context, crewID, alias string) (transporter.Beam, error) {
	if alias != "" {
		return transporter.Beam{Alias: alias}, nil
	}
	return d.crewBeam(ctx, crewID)
}

// crewBeam is the crew member's current beam.
func (d *Daemon) crewBeam(ctx context.Context, crewID string) (transporter.Beam, error) {
	c, err := d.store.GetCrew(ctx, crewID)
	if err != nil {
		return transporter.Beam{}, err
	}
	if c.CurrentBeam == "" {
		return transporter.Beam{}, fmt.Errorf("crew %s has no beam", crewID)
	}
	return transporter.Beam{Alias: c.CurrentBeam}, nil
}

// workPullTimeout bounds each copy of a bundle out of a beam, so a hung
// copy is retried rather than waited on for good. Generous: a bundle can be
// large. A variable so tests can shorten it.
var workPullTimeout = 15 * time.Minute

// pullWork is the pull-work step: it copies each of the report's bundles
// out of the crew member's beam into <home>/cargo/<order>/final/<name>.bundle
// and returns them as landed. A re-run copies again; the copies replace
// atomically. It doesn't watch the tour-end mark: a reported order's work
// is worth pulling for as long as the beam answers. A pull can outlast the
// tour's settle wait (each copy may take up to workPullTimeout), and the
// backstop may fail the order meanwhile; order-done then keeps the order
// failed and logs the late work instead (store.SettleOrder).
func pullWork(ctx context.Context, d *Daemon, in MissionInput, beamAlias string, work []comms.WorkRepo) ([]landedRepo, error) {
	o, err := d.store.GetOrder(ctx, in.OrderID)
	if err != nil {
		return nil, fmt.Errorf("order %s: %w", in.OrderID, err)
	}
	matched, err := matchWork(in.OrderID, o.Repos, work)
	if err != nil {
		return nil, err
	}
	landed := make([]landedRepo, 0, len(matched))
	if len(matched) == 0 {
		return landed, nil
	}
	beam, err := d.missionBeam(ctx, in.CrewID, beamAlias)
	if err != nil {
		return nil, err
	}
	dir := d.cargoFinal(in.OrderID)
	// tsh's CopyFrom needs the local directory to exist.
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	for _, w := range matched {
		size, err := d.transporter.Size(ctx, beam, w.BundlePath)
		if err != nil {
			return nil, fmt.Errorf("size repo %s's bundle %s on %s: %w", w.Name, w.BundlePath, beam.Alias, err)
		}
		if size > d.maxBundleBytes {
			return nil, fmt.Errorf("repo %s: bundle %s is %d bytes, over the %d byte cap: %w",
				w.Name, w.BundlePath, size, d.maxBundleBytes, errBundleTooLarge)
		}
		local := filepath.Join(dir, w.Name+".bundle")
		cctx, cancel := context.WithTimeout(ctx, workPullTimeout)
		err = d.transporter.CopyFrom(cctx, beam, w.BundlePath, local)
		cancel()
		if err != nil {
			return nil, fmt.Errorf("copy repo %s's bundle %s from %s: %w", w.Name, w.BundlePath, beam.Alias, err)
		}
		if err := d.checkCopiedBundle(w.Name, local); err != nil {
			return nil, err
		}
		landed = append(landed, landedRepo{Name: w.Name, Branch: w.Branch, Commit: w.Commit, Bundle: local})
	}
	return landed, nil
}

// checkCopiedBundle checks a bundle's size again once it is in the cargo
// bay, and removes it if it is over Config.MaxBundleBytes. The check before
// the copy reads the beam's own answer: the file may have grown since, and
// the beam controls the file. This one reads what actually arrived.
func (d *Daemon) checkCopiedBundle(repo, local string) error {
	st, err := os.Stat(local)
	if err != nil {
		return err
	}
	if st.Size() <= d.maxBundleBytes {
		return nil
	}
	if err := os.Remove(local); err != nil {
		return err
	}
	return fmt.Errorf("repo %s: the copied bundle is %d bytes, over the %d byte cap: %w",
		repo, st.Size(), d.maxBundleBytes, errBundleTooLarge)
}

// fetchWork is the fetch-work step: for each landed bundle it runs
// `git fetch <bundle> +bmus/<order>:bmus/<order>` in the bridge's clone of
// the repo, under the repo's lock, and checks the branch then points at the
// reported commit. It pushes nothing and touches no remote. A re-run fetches
// the same bundle again, a no-op. A repo that can't be landed for good (the
// bundle doesn't hold the reported commit) gets FetchError in the returned
// list; any other failure is returned, for the step to retry.
func fetchWork(ctx context.Context, d *Daemon, orderID string, landed []landedRepo) ([]landedRepo, error) {
	out := make([]landedRepo, len(landed))
	for i, l := range landed {
		out[i] = l
		if err := d.fetchBundle(ctx, l.Name, orderID, l.Bundle, l.Commit); err != nil {
			if !errors.Is(err, errWorkPermanent) {
				return nil, fmt.Errorf("repo %s: %w", l.Name, err)
			}
			if errors.Is(err, errBundleRejected) {
				out[i].RejectError = firstLine(err.Error())
			} else {
				out[i].FetchError = firstLine(err.Error())
			}
		}
	}
	return out, nil
}

// fetchFailures describes the repos fetch-work couldn't land, or is empty.
func fetchFailures(landed []landedRepo) string {
	var parts []string
	for _, l := range landed {
		if l.FetchError != "" {
			parts = append(parts, fmt.Sprintf("repo %s: %s", l.Name, l.FetchError))
		}
	}
	return strings.Join(parts, "; ")
}

// workRejections describes the repos fetch-work's object check (#65)
// rejected, or is empty.
func workRejections(landed []landedRepo) string {
	var parts []string
	for _, l := range landed {
		if l.RejectError != "" {
			parts = append(parts, fmt.Sprintf("repo %s: %s", l.Name, l.RejectError))
		}
	}
	return strings.Join(parts, "; ")
}

// fetchBundle fetches bundle's bmus/<order> into repo name's clone as
// refs/heads/bmus/<order>, forced, and checks it is at commit, under the
// repo's lock.
func (d *Daemon) fetchBundle(ctx context.Context, name, orderID, bundle, commit string) error {
	unlock, err := d.lockRepo(ctx, name)
	if err != nil {
		return err
	}
	defer unlock()
	return d.fetchBundleLocked(ctx, name, orderID, bundle, commit)
}

// fetchBundleLocked is fetchBundle for a caller already holding the repo's
// lock (ship-repos landing a checkpoint, #100).
func (d *Daemon) fetchBundleLocked(ctx context.Context, name, orderID, bundle, commit string) error {
	return d.fetchBundleAs(ctx, name, orderID, bundle, commit, "refs/heads/"+orderBranch(orderID))
}

// fetchBundleAs is fetchBundleLocked fetching the bundle's bmus/<order>
// into ref instead (pull-checkpoint's checkpointRef, #145).
func (d *Daemon) fetchBundleAs(ctx context.Context, name, orderID, bundle, commit, ref string) error {
	dir := d.repoDir(name)
	branch := "refs/heads/" + orderBranch(orderID)
	heads, err := runGit(ctx, dir, "bundle", "list-heads", bundle, branch)
	if err != nil {
		return classifyBundleErr(err)
	}
	if got, _, _ := strings.Cut(heads, " "); got != commit {
		return fmt.Errorf("the bundle has %s at %q, not the reported %s: %w", branch, got, commit, errWorkPermanent)
	}
	if err := verifyBundleObjects(ctx, dir, bundle); err != nil {
		return err
	}
	fctx, cancel := context.WithTimeout(ctx, gitFetchTimeout)
	defer cancel()
	if _, err := runGit(fctx, dir, "fetch", "--quiet", "--no-tags", "--no-write-fetch-head", bundle, "+"+branch+":"+ref); err != nil {
		return classifyBundleErr(err)
	}
	got, err := runGit(ctx, dir, "rev-parse", "--verify", "--end-of-options", ref+"^{commit}")
	if err != nil {
		return err
	}
	if got != commit {
		return fmt.Errorf("%s is at %s after the fetch, not the reported %s: %w", ref, got, commit, errWorkPermanent)
	}
	return nil
}

// wipRef is the ref an order's work in progress travels on (#145):
// refs/bmus/wip/<order>, never a branch, so the report, the push and
// jettison never take it.
func wipRef(orderID string) string { return "refs/bmus/wip/" + orderID }

// checkpointRef holds the latest checkpoint pull-checkpoint landed for an
// order in the bridge's clone (#145): a later incremental checkpoint's
// bundle needs its commit. Jettison removes it with bmus/<order>.
func checkpointRef(orderID string) string { return "refs/bmus/checkpoints/" + orderID }

// fetchWipLocked fetches bundle's refs/bmus/wip/<order>, if it carries
// one, into the clone of repo name as the same ref, after the object
// check, and returns its commit ("" when the bundle has none). The caller
// holds the repo's lock.
func (d *Daemon) fetchWipLocked(ctx context.Context, name, orderID, bundle string) (string, error) {
	dir := d.repoDir(name)
	ref := wipRef(orderID)
	heads, err := runGit(ctx, dir, "bundle", "list-heads", bundle, ref)
	if err != nil {
		return "", classifyBundleErr(err)
	}
	commit, _, _ := strings.Cut(heads, " ")
	if commit == "" {
		return "", nil
	}
	if err := verifyBundleObjects(ctx, dir, bundle); err != nil {
		return "", err
	}
	fctx, cancel := context.WithTimeout(ctx, gitFetchTimeout)
	defer cancel()
	if _, err := runGit(fctx, dir, "fetch", "--quiet", "--no-tags", "--no-write-fetch-head", bundle, "+"+ref+":"+ref); err != nil {
		return "", classifyBundleErr(err)
	}
	return commit, nil
}

// verifyBundleObjects checks every object bundle's pack would add to
// dir's clone before fetchBundle moves bmus/<order>: it pipes the pack
// (the bundle file, minus its header) through `git index-pack --strict
// --fix-thin --stdin` in a throwaway bare repository that borrows dir's
// objects through objects/info/alternates, so index-pack's own fsck
// checks (a malformed commit, a tree with a path git itself refuses,
// e.g. ".GIT") see the real object graph without writing anything into
// dir. `git fsck --no-full` checks only loose objects and would miss
// the pack fetchBundle is about to add, which is why this runs
// index-pack instead (#65).
//
// Only index-pack's fsck-specific failures (a bad object: a tree with a
// ".GIT" entry, a malformed commit, ...) are errBundleRejected. index-pack
// --strict wraps every one of those in the constant message "fatal: fsck
// error in packed object", so that text, not the whole failure, is what's
// classified as rejected. Anything else index-pack --fix-thin --stdin can
// fail on is an ordinary condition, not tampering -- notably, git bundle
// create routinely emits a thin pack (deltified against the excluded
// boundary commit) even for an unremarkable bundle, and if dir's clone
// lacks that base commit too (a reset clone, a diverged base_ref),
// index-pack reports "did not receive expected object <sha>" trying to
// resolve it. That's the same ordinary "bundle lacks a prerequisite the
// clone has" condition classifyBundleErr already treats as
// errWorkPermanent when it surfaces from the real fetch instead, so it's
// classified the same way here. Making the throwaway repo, reading the
// bundle, or ctx ending is returned as is, for fetch-work's step to retry.
func verifyBundleObjects(ctx context.Context, dir, bundle string) error {
	tmp, err := os.MkdirTemp(filepath.Dir(dir), ".verify-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	if _, err := runGit(ctx, "", "init", "--quiet", "--bare", tmp); err != nil {
		return err
	}
	alt := filepath.Join(dir, "objects") + "\n"
	if err := os.WriteFile(filepath.Join(tmp, "objects", "info", "alternates"), []byte(alt), 0o600); err != nil {
		return err
	}
	f, err := os.Open(bundle)
	if err != nil {
		return err
	}
	defer f.Close()
	pack, err := bundlePackReader(f)
	if err != nil {
		return err
	}
	fctx, cancel := context.WithTimeout(ctx, gitFetchTimeout)
	defer cancel()
	if _, err := runGitStdin(fctx, tmp, pack, "index-pack", "--strict", "--fix-thin", "--stdin"); err != nil {
		if ctx.Err() != nil {
			return err
		}
		if strings.Contains(err.Error(), "fsck error in packed object") {
			return fmt.Errorf("%w: %w", err, errBundleRejected)
		}
		return fmt.Errorf("%w: %w", err, errWorkPermanent)
	}
	return nil
}

// bundlePackReader positions f at the start of bundle's pack data and
// returns it as a Reader from there on, for index-pack --stdin.
// git bundle's header is a run of non-empty lines (signature,
// optional v3 capability lines, prerequisites, refs) terminated by one
// blank line; everything after that line is the pack.
func bundlePackReader(f *os.File) (io.Reader, error) {
	br := bufio.NewReader(f)
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return nil, fmt.Errorf("the bundle's header ended before its pack data: %w", err)
		}
		if line == "\n" {
			return br, nil
		}
	}
}

// permanentBundleErrs are git messages about a bundle no retry can fix: a
// file that isn't a bundle, or one whose prerequisites the clone lacks.
var permanentBundleErrs = []string{"does not look like a v2 or v3 bundle file", "is not a bundle",
	"lacks these prerequisite commits"}

// classifyBundleErr marks a git error about a bad bundle errWorkPermanent.
func classifyBundleErr(err error) error {
	for _, m := range permanentBundleErrs {
		if strings.Contains(err.Error(), m) {
			return fmt.Errorf("%w: %w", err, errWorkPermanent)
		}
	}
	return err
}

// firstLine is s up to its first newline.
func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}

// lateText is the text of the entry order-done writes instead of the done
// entry when the order had already ended.
func lateText(landed []landedRepo) string {
	return "the order's work arrived after the order had ended" + doneText("", landed)
}

// doneText is the done entry's text: the summary, then each repo's local
// branch and commit (or where its bundle waits when it couldn't be
// fetched).
func doneText(summary string, landed []landedRepo) string {
	parts := make([]string, 0, len(landed))
	for _, l := range landed {
		switch {
		case l.RejectError != "":
			parts = append(parts, fmt.Sprintf("%s: rejected, bundle at %s", l.Name, l.Bundle))
		case l.FetchError != "":
			parts = append(parts, fmt.Sprintf("%s: not fetched, bundle at %s", l.Name, l.Bundle))
		default:
			parts = append(parts, fmt.Sprintf("%s: %s at %s", l.Name, l.Branch, l.Commit[:min(len(l.Commit), 12)]))
		}
	}
	if len(parts) == 0 {
		return summary
	}
	return summary + " (" + strings.Join(parts, "; ") + ")"
}

// donePayload is the done entry's payload: the report, with each repo's
// work as it landed on the bridge instead of the beam's paths, and the
// beam it came from (#104; absent before).
type donePayload struct {
	Beam      string       `json:"beam,omitempty"`
	Summary   string       `json:"summary"`
	Repos     []landedRepo `json:"repos"`
	Artifacts []string     `json:"artifacts,omitempty"`
	Usage     comms.Usage  `json:"usage"`
}
