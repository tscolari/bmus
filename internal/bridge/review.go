package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/dbos-inc/dbos-transact-golang/dbos"

	"github.com/tscolari/bmus/internal/comms"
	"github.com/tscolari/bmus/internal/regs"
	"github.com/tscolari/bmus/internal/store"
	"github.com/tscolari/bmus/internal/tracker"
)

// The findings review (#156, spec/campaigns-stage-2-plan-2026-11-14.md
// section 5): once an order's findings are filed, the captain keeps or
// discards the issues and may restore findings triage dropped or filing
// failed, in one tracker_review hail per order.

// Review options: option 0 keeps every issue, option 1 discards every one.
const (
	reviewKeep    = "keep"
	reviewDiscard = "discard"
	reviewRestore = "restore"
)

// reviewOptions are a tracker_review hail's options.
var reviewOptions = []string{reviewKeep, reviewDiscard}

// ReviewContext is a tracker_review hail's context: the order whose
// findings it reviews, the issues filed from them, and the findings triage
// dropped or filing failed, which the captain may restore.
type ReviewContext struct {
	Order   string          `json:"order"`
	Issues  []ReviewIssue   `json:"issues"`
	Dropped []ReviewDropped `json:"dropped"`
	Failed  []ReviewFailed  `json:"failed"`
}

// ReviewIssue is an issue filed from a finding.
type ReviewIssue struct {
	Ref   string `json:"ref"`
	Key   string `json:"key"`
	Title string `json:"title"`
}

// ReviewDropped is a finding Command's triage dropped: Why is its
// rationale, and DuplicateOf the issue it repeats, if any.
type ReviewDropped struct {
	Key         string `json:"key"`
	Title       string `json:"title"`
	Why         string `json:"why"`
	DuplicateOf string `json:"duplicate_of,omitempty"`
}

// ReviewFailed is a finding filing gave up on, with the error.
type ReviewFailed struct {
	Key   string `json:"key"`
	Title string `json:"title"`
	Error string `json:"error"`
}

// Numbers maps each key of rc's issues, dropped and failed findings to
// its rank among them, from 1 in key order: the number Command's triage
// gave the finding (triageContext), so a rationale such as "3 is a typo"
// names the row numbered 3. An empty key gets no number.
func (rc ReviewContext) Numbers() map[string]int {
	var keys []string
	for _, is := range rc.Issues {
		keys = append(keys, is.Key)
	}
	for _, f := range rc.Dropped {
		keys = append(keys, f.Key)
	}
	for _, f := range rc.Failed {
		keys = append(keys, f.Key)
	}
	keys = slices.DeleteFunc(keys, func(k string) bool { return k == "" })
	slices.Sort(keys)
	keys = slices.Compact(keys)
	n := make(map[string]int, len(keys))
	for i, k := range keys {
		n[k] = i + 1
	}
	return n
}

// reviewBatch is the raise-review step's recorded result: the review's
// context, empty when there is nothing to review.
type reviewBatch struct {
	Context ReviewContext
	Raise   bool
}

// reviewFindings raises order orderID's tracker_review hail, once its
// findings are filed, and acts on the captain's answer (#156):
//
//  1. raise-review reads the rows filing moved on: filed (the issues),
//     dropped and failed. With none, no hail is raised. Rows still new
//     are left out, and their count logged.
//  2. raise-hail and the wait (raiseBridgeHail, awaitBridgeHail), with no
//     deadline: classify records no reminder and no captain timeout for
//     the hail.
//  3. One step per issue, review-keep (removes bmus:unreviewed) or
//     review-discard (closes it not planned), then review-restore for
//     each finding to restore (filed as filing does, without
//     bmus:unreviewed, under the same marker). Each retries as filing
//     does; one out of retries, or failed for good, is logged on the
//     order (review-failed) and the rest go on.
//  4. review-summary logs "review of ORD-0001's findings: kept 3,
//     discarded 2, restored 1", with ", failed 1" when any failed.
func reviewFindings(ctx dbos.Context, orderID string, b findingsBegin, res *FindingsResult) error {
	batch, err := dbos.RunAsStep(ctx, func(c context.Context) (reviewBatch, error) {
		return daemonFrom(c).reviewBatch(c, orderID)
	}, append([]dbos.StepOption{dbos.WithStepName("raise-review")}, storeRetries...)...)
	if err != nil || !batch.Raise {
		return err
	}
	rc := batch.Context
	raw, err := json.Marshal(rc)
	if err != nil {
		return err
	}
	id, err := raiseBridgeHail(ctx, store.Hail{Category: regs.CategoryTrackerReview, Question: reviewQuestion(rc),
		Options: reviewOptions, Context: raw}, store.LogEntry{Kind: logHail, OrderID: orderID,
		Text: fmt.Sprintf("the captain asked to review %s filed from %s's findings", countIssues(len(rc.Issues)), orderID)})
	if err != nil {
		return err
	}
	h, err := awaitBridgeHail(ctx, id)
	if err != nil {
		return err
	}
	if h.Status != store.HailDecided {
		return storeStep(ctx, "review-summary", func(c context.Context, d *Daemon) error {
			return d.logOnce(c, orderID, "review", fmt.Sprintf("review of %s's findings closed unanswered (hail %s %s)",
				orderID, id, h.Status), map[string]any{"hail_id": id})
		})
	}
	dec, err := parseReview(h.Decision, rc, nil)
	if err != nil {
		// AnswerHail stored it in normal form: this is a bug, or a hail
		// decided another way. Nothing is acted on.
		return storeStep(ctx, "review-summary", func(c context.Context, d *Daemon) error {
			return d.logOnce(c, orderID, "review", fmt.Sprintf("review of %s's findings not acted on: %v", orderID, err),
				map[string]any{"hail_id": id})
		})
	}
	retries := daemonFrom(ctx).trackerRetries
	act := func(step, key string, fn func(c context.Context, d *Daemon) (reviewOutcome, error)) (string, error) {
		out, err := dbos.RunAsStep(ctx, func(c context.Context) (reviewOutcome, error) {
			return fn(c, daemonFrom(c))
		}, append([]dbos.StepOption{dbos.WithStepName(step)}, retries...)...)
		if err != nil && ctx.Err() != nil {
			return "", err
		}
		if err == nil && out.Failed == "" {
			return out.State, nil
		}
		why := out.Failed
		if err != nil {
			why = clip(stepError(err), maxFindingWhy)
		}
		out, err = dbos.RunAsStep(ctx, func(c context.Context) (reviewOutcome, error) {
			return daemonFrom(c).reviewFailed(c, orderID, id, key, step, why)
		}, append([]dbos.StepOption{dbos.WithStepName("review-failed")}, storeRetries...)...)
		if err != nil {
			return "", err
		}
		if out.State == "" {
			res.ReviewFailed++
		}
		return out.State, nil
	}
	for _, is := range rc.Issues {
		step, fn := "review-keep", (*Daemon).reviewKeep
		if slices.Contains(dec.Discard, is.Ref) {
			step, fn = "review-discard", (*Daemon).reviewDiscard
		}
		state, err := act(step, is.Key, func(c context.Context, d *Daemon) (reviewOutcome, error) { return fn(d, c, orderID, is.Key) })
		if err != nil {
			return err
		}
		switch state {
		case store.FindingKept:
			res.Kept++
		case store.FindingDiscarded:
			res.Discarded++
		}
	}
	for _, key := range dec.Restore {
		state, err := act("review-restore", key, func(c context.Context, d *Daemon) (reviewOutcome, error) {
			return d.reviewRestore(c, orderID, key, b)
		})
		if err != nil {
			return err
		}
		if state == store.FindingRestored {
			res.Restored++
		}
	}
	return storeStep(ctx, "review-summary", func(c context.Context, d *Daemon) error {
		text := fmt.Sprintf("review of %s's findings: kept %d, discarded %d, restored %d", orderID, res.Kept, res.Discarded, res.Restored)
		if res.ReviewFailed > 0 {
			text += fmt.Sprintf(", failed %d", res.ReviewFailed)
		}
		return d.logOnce(c, orderID, "review", text, map[string]any{"hail_id": id, "kept": res.Kept,
			"discarded": res.Discarded, "restored": res.Restored, "failed": res.ReviewFailed})
	})
}

// reviewOutcome is a review step's recorded result: the finding's state
// after it, or why the action failed for good (Failed), which skips the
// step's retries.
type reviewOutcome struct {
	State  string
	Failed string `json:",omitempty"`
}

// reviewVerbs are each review step's past tense, for its failure entry.
var reviewVerbs = map[string]string{"review-keep": "kept", "review-discard": "discarded", "review-restore": "restored"}

// reviewFailed is the review-failed step, for review step step on finding
// key, which failed for good with why. A restore whose issue exists (its
// ref stored, short of a link) is restored, with why noted, as filing's
// finding-failed files one. Any other logs "finding <key> not <verb>:
// <why>" once and leaves the row as it was; its State is empty.
func (d *Daemon) reviewFailed(ctx context.Context, orderID, hailID, key, step, why string) (reviewOutcome, error) {
	if step == "review-restore" {
		f, err := d.store.GetFinding(ctx, orderID, key)
		if err != nil {
			return reviewOutcome{}, err
		}
		if f.Ref != "" && (f.State == store.FindingDropped || f.State == store.FindingFailed) {
			entry, err := logEntry(logOrder, "", orderID, "finding "+key+" restored as "+f.Ref+" ("+why+")",
				map[string]any{"key": key, "ref": f.Ref})
			if err != nil {
				return reviewOutcome{}, err
			}
			if _, err := d.store.ReviewFinding(ctx, orderID, key, f.State, store.FindingRestored, "", &why, entry); err != nil {
				return reviewOutcome{}, err
			}
		}
		if f, err = d.store.GetFinding(ctx, orderID, key); err != nil || f.State == store.FindingRestored {
			return reviewOutcome{State: f.State}, err
		}
	}
	return reviewOutcome{}, d.logOnce(ctx, orderID, "review-failed "+key, "finding "+key+" not "+reviewVerbs[step]+": "+why,
		map[string]any{"hail_id": hailID, "key": key})
}

// reviewBatch is the raise-review step: order orderID's findings filing
// moved on, as the review's context, by key. Raise is false when there is
// none: nothing was filed, dropped or failed.
func (d *Daemon) reviewBatch(ctx context.Context, orderID string) (reviewBatch, error) {
	fs, err := d.store.OrderFindings(ctx, orderID)
	if err != nil {
		return reviewBatch{}, err
	}
	rc := ReviewContext{Order: orderID, Issues: []ReviewIssue{}, Dropped: []ReviewDropped{}, Failed: []ReviewFailed{}}
	left := 0
	for _, f := range fs {
		switch f.State {
		case store.FindingNew:
			left++
		case store.FindingFiled:
			rc.Issues = append(rc.Issues, ReviewIssue{Ref: f.Ref, Key: f.Key, Title: f.Title})
		case store.FindingDropped:
			rc.Dropped = append(rc.Dropped, ReviewDropped{Key: f.Key, Title: f.Title, Why: f.Why, DuplicateOf: f.DuplicateOf})
		case store.FindingFailed:
			rc.Failed = append(rc.Failed, ReviewFailed{Key: f.Key, Title: f.Title, Error: f.Why})
		}
	}
	if left > 0 {
		if err := d.logOnce(ctx, orderID, "review-new", fmt.Sprintf("findings of %s left out of the review: %d still new",
			orderID, left), map[string]any{"new": left}); err != nil {
			return reviewBatch{}, err
		}
	}
	return reviewBatch{Context: rc, Raise: len(rc.Issues)+len(rc.Dropped)+len(rc.Failed) > 0}, nil
}

// reviewQuestion is the tracker_review hail's question, written by the
// bridge. It names no titles: those are the crew's text, and the context
// holds them.
func reviewQuestion(rc ReviewContext) string {
	return fmt.Sprintf("Review %s filed from %s's findings (%d dropped, %d failed): keep, discard, "+
		"or answer with the refs to discard and the keys to restore.", countIssues(len(rc.Issues)), rc.Order,
		len(rc.Dropped), len(rc.Failed))
}

// countIssues is "1 issue" or "n issues".
func countIssues(n int) string {
	if n == 1 {
		return "1 issue"
	}
	return strconv.Itoa(n) + " issues"
}

// reviewDecision is a tracker_review answer: the refs of the issues to
// discard (the others are kept) and the keys of the findings to restore,
// each in the context's order and spelling.
type reviewDecision struct {
	Discard []string
	Restore []string
}

// normal is d's normal form: keep, discard (every issue) or discard
// <ref>..., then "; restore <key>..." when there are any to restore.
// all is how many issues the review has.
func (d reviewDecision) normal(all int) string {
	s := reviewKeep
	switch {
	case len(d.Discard) > 0 && len(d.Discard) == all:
		s = reviewDiscard
	case len(d.Discard) > 0:
		s = reviewDiscard + " " + strings.Join(d.Discard, " ")
	}
	if len(d.Restore) > 0 {
		s += "; " + reviewRestore + " " + strings.Join(d.Restore, " ")
	}
	return s
}

// option is d's option: 1 (discard) when it discards anything, else 0.
func (d reviewDecision) option() int {
	if len(d.Discard) > 0 {
		return 1
	}
	return 0
}

// parseReview reads a tracker_review answer against the review's context
// rc:
//
//	keep
//	discard
//	discard <ref> [<ref>...]
//
// each optionally followed by "; restore <key> [<key>...]", or
// "restore <key> [<key>...]" alone, which keeps every issue. Words are
// separated by spaces or commas, and the verbs read in any case. Only the
// bare word discard discards every issue: "discard ," is refused. A ref
// is one of rc's issues, as spelled there or, with canon set, once canon
// makes both canonical; a key is one of rc's dropped or failed findings.
// Each is taken once.
func parseReview(answer string, rc ReviewContext, canon func(string) (string, error)) (reviewDecision, error) {
	var d reviewDecision
	parts := strings.Split(answer, ";")
	if len(parts) > 2 {
		return d, errors.New("more than one ;")
	}
	words := func(s string) []string { return strings.Fields(strings.ReplaceAll(s, ",", " ")) }
	head := words(parts[0])
	if len(head) == 0 {
		return d, errors.New("empty answer")
	}
	restore := []string(nil)
	switch verb := strings.ToLower(head[0]); {
	case verb == reviewRestore && len(parts) == 1:
		restore = head[1:]
		if len(restore) == 0 {
			return d, errors.New("restore names no key")
		}
	case verb == reviewRestore:
		return d, errors.New("restore comes last")
	case verb == reviewKeep:
		if len(head) > 1 {
			return d, fmt.Errorf("keep takes no refs (got %s)", strings.Join(head[1:], " "))
		}
	case verb == reviewDiscard && len(head) == 1 && !strings.EqualFold(strings.TrimSpace(parts[0]), reviewDiscard):
		// "discard ," names no ref: only the bare word discards every issue.
		return d, errors.New("discard names no ref (discard alone discards every issue)")
	case verb == reviewDiscard && len(head) == 1:
		for _, is := range rc.Issues {
			d.Discard = append(d.Discard, is.Ref)
		}
	case verb == reviewDiscard:
		for _, w := range head[1:] {
			i := slices.IndexFunc(rc.Issues, func(is ReviewIssue) bool { return sameRef(w, is.Ref, canon) })
			if i < 0 {
				return d, fmt.Errorf("%q is not a ref of the review", w)
			}
			if !slices.Contains(d.Discard, rc.Issues[i].Ref) {
				d.Discard = append(d.Discard, rc.Issues[i].Ref)
			}
		}
	default:
		return d, fmt.Errorf("%q is not keep, discard or restore", head[0])
	}
	if len(parts) == 2 {
		tail := words(parts[1])
		if len(tail) == 0 || strings.ToLower(tail[0]) != reviewRestore {
			return d, errors.New("after ; comes restore <key>...")
		}
		if restore = tail[1:]; len(restore) == 0 {
			return d, errors.New("restore names no key")
		}
	}
	keys := restorable(rc)
	for _, w := range restore {
		k := strings.ToLower(w)
		if !slices.Contains(keys, k) {
			return d, fmt.Errorf("%q is not a dropped or failed finding's key", w)
		}
		if !slices.Contains(d.Restore, k) {
			d.Restore = append(d.Restore, k)
		}
	}
	// In the context's order, whatever the answer's.
	slices.SortStableFunc(d.Discard, func(a, b string) int {
		return slices.IndexFunc(rc.Issues, func(is ReviewIssue) bool { return is.Ref == a }) -
			slices.IndexFunc(rc.Issues, func(is ReviewIssue) bool { return is.Ref == b })
	})
	slices.SortStableFunc(d.Restore, func(a, b string) int { return slices.Index(keys, a) - slices.Index(keys, b) })
	return d, nil
}

// restorable are the keys of rc's dropped findings, then its failed ones.
func restorable(rc ReviewContext) []string {
	var keys []string
	for _, f := range rc.Dropped {
		keys = append(keys, f.Key)
	}
	for _, f := range rc.Failed {
		keys = append(keys, f.Key)
	}
	return keys
}

// sameRef reports whether w names ref: the same text, or, with canon set,
// the same canonical ref.
func sameRef(w, ref string, canon func(string) (string, error)) bool {
	if w == ref {
		return true
	}
	if canon == nil {
		return false
	}
	a, err := canon(w)
	if err != nil {
		return false
	}
	b, err := canon(ref)
	return err == nil && a == b
}

// checkReviewAnswer checks the captain's answer to tracker_review hail h
// against its context (AnswerHail): decision, or option's text when
// decision is empty. It returns the decision in normal form and its
// option: 1 when anything is discarded, else 0. An option given with a
// decision that doesn't match it, or a decision that doesn't parse, is a
// bad request naming the refs and keys the hail takes.
func (d *Daemon) checkReviewAnswer(h store.Hail, decision string, option *int) (string, *int, error) {
	var rc ReviewContext
	if err := json.Unmarshal(h.Context, &rc); err != nil {
		return "", nil, fmt.Errorf("hail %s: review context: %w", h.ID, err)
	}
	var canon func(string) (string, error)
	if d.tracker != nil {
		canon = d.tracker.Canon
	}
	dec, err := parseReview(decision, rc, canon)
	if err != nil {
		var refs []string
		for _, is := range rc.Issues {
			refs = append(refs, is.Ref)
		}
		return "", nil, badRequest("hail %s: %s; answer keep, discard, discard <ref>..., or restore <key>..., "+
			"the first three optionally followed by \"; restore <key>...\" (refs: %s; keys: %s)",
			h.ID, err, listOrNone(refs), listOrNone(restorable(rc)))
	}
	opt := dec.option()
	if option != nil && *option != opt {
		return "", nil, badRequest("hail %s: option %d (%s) does not match the decision %q", h.ID, *option,
			reviewOptions[*option], dec.normal(len(rc.Issues)))
	}
	return dec.normal(len(rc.Issues)), &opt, nil
}

// listOrNone is l joined with commas, or "none".
func listOrNone(l []string) string {
	if len(l) == 0 {
		return "none"
	}
	return strings.Join(l, ", ")
}

// reviewKeep is the review-keep step: finding key's issue loses
// bmus:unreviewed, and the row moves from filed to kept. An issue that no
// longer exists fails for good (Failed), with no retries. A row already
// moved on is returned as it is.
func (d *Daemon) reviewKeep(ctx context.Context, orderID, key string) (reviewOutcome, error) {
	return d.reviewIssue(ctx, orderID, key, store.FindingKept, func(c context.Context, ref string) (string, error) {
		err := d.tracker.Unlabel(c, ref, labelUnreviewed)
		if errors.Is(err, tracker.ErrNotFound) {
			return "", permanent{err}
		}
		return "", err
	})
}

// reviewDiscard is the review-discard step: finding key's issue is closed
// as not planned, and the row moves from filed to discarded. An issue
// that no longer exists counts as discarded, noted on the row and in the
// log. A row already moved on is returned as it is.
func (d *Daemon) reviewDiscard(ctx context.Context, orderID, key string) (reviewOutcome, error) {
	return d.reviewIssue(ctx, orderID, key, store.FindingDiscarded, func(c context.Context, ref string) (string, error) {
		err := d.tracker.Close(c, ref, tracker.ReasonNotPlanned)
		if errors.Is(err, tracker.ErrNotFound) {
			return clip(err.Error(), maxFindingWhy), nil
		}
		return "", err
	})
}

// permanent is an error a review step gives up on at once (Failed).
type permanent struct{ error }

// reviewIssue runs fn on finding key's issue, bounded by
// trackerCallTimeout, and moves the row from filed to state, with fn's
// note, if any, as its why and logged as "finding <key> <state>: <note>".
// fn must be safe to repeat: a re-run after a crash calls it again. A
// permanent error is returned as Failed.
func (d *Daemon) reviewIssue(ctx context.Context, orderID, key, state string, fn func(context.Context, string) (string, error)) (reviewOutcome, error) {
	f, err := d.store.GetFinding(ctx, orderID, key)
	if err != nil {
		return reviewOutcome{}, fmt.Errorf("finding %s/%s: %w", orderID, key, err)
	}
	if f.State != store.FindingFiled {
		return reviewOutcome{State: f.State}, nil
	}
	if d.tracker == nil {
		return reviewOutcome{}, errors.New("no tracker configured")
	}
	tctx, cancel := context.WithTimeout(ctx, trackerCallTimeout)
	note, err := fn(tctx, f.Ref)
	cancel()
	if p := (permanent{}); errors.As(err, &p) {
		return reviewOutcome{Failed: clip(p.Error(), maxFindingWhy)}, nil
	}
	if err != nil {
		return reviewOutcome{}, err
	}
	var (
		why     *string
		entries []store.LogEntry
	)
	if note != "" {
		why = &note
		e, err := logEntry(logOrder, "", orderID, "finding "+key+" "+state+": "+note, map[string]any{"key": key, "ref": f.Ref})
		if err != nil {
			return reviewOutcome{}, err
		}
		entries = append(entries, e)
	}
	if _, err := d.store.ReviewFinding(ctx, orderID, key, store.FindingFiled, state, "", why, entries...); err != nil {
		return reviewOutcome{}, err
	}
	f, err = d.store.GetFinding(ctx, orderID, key)
	return reviewOutcome{State: f.State}, err
}

// reviewRestore is the review-restore step: finding key, dropped or
// failed, is filed as filing does (createFindingIssue), without
// bmus:unreviewed, under the same marker, and the row moves to restored
// with its ref, logged "finding <key> restored as o/r#12". Its blockers
// are its refs, made canonical, and the issues of the order's findings it
// names that are filed, kept or restored, or the issue a duplicate
// repeats; others are dropped with a note. A ref returned with a link
// error is stored on the row (store.SetFindingRefIn) and the step fails,
// so its retry adds the link. A row already moved on is returned as it
// is.
func (d *Daemon) reviewRestore(ctx context.Context, orderID, key string, b findingsBegin) (reviewOutcome, error) {
	f, err := d.store.GetFinding(ctx, orderID, key)
	if err != nil {
		return reviewOutcome{}, fmt.Errorf("finding %s/%s: %w", orderID, key, err)
	}
	if f.State != store.FindingDropped && f.State != store.FindingFailed {
		return reviewOutcome{State: f.State}, nil
	}
	if d.tracker == nil {
		return reviewOutcome{}, errors.New("no tracker configured")
	}
	all, err := d.store.OrderFindings(ctx, orderID)
	if err != nil {
		return reviewOutcome{}, err
	}
	var (
		blockers []string
		notes    []store.LogEntry
	)
	note := func(bl, why string) {
		notes = append(notes, store.LogEntry{Kind: logOrder, OrderID: orderID,
			Text: "finding " + key + ": blocker " + bl + " dropped: " + why})
	}
	for _, bl := range f.BlockedBy {
		ref := ""
		if comms.FindingKey(bl) {
			i := slices.IndexFunc(all, func(o store.Finding) bool { return o.Key == bl })
			switch {
			case bl == key || i < 0:
				note(bl, "it names no other finding of "+orderID)
				continue
			case all[i].State == store.FindingFiled || all[i].State == store.FindingKept || all[i].State == store.FindingRestored:
				ref = all[i].Ref
			case all[i].DuplicateOf != "":
				ref = all[i].DuplicateOf
			default:
				note(bl, "it has no open issue ("+all[i].State+")")
				continue
			}
		} else if ref, err = d.tracker.Canon(bl); err != nil {
			note(bl, err.Error())
			continue
		}
		if ref != "" && !slices.Contains(blockers, ref) {
			blockers = append(blockers, ref)
		}
	}
	got, err := d.createFindingIssue(ctx, orderID, f, blockers, b, false)
	if err != nil {
		if got.Ref != "" {
			// A link that failed: the retry adds it, and review-failed
			// restores the finding with its issue if none does.
			if serr := d.store.SetFindingRefIn(ctx, orderID, key, got.Ref, f.State); serr != nil {
				return reviewOutcome{}, errors.Join(err, serr)
			}
		}
		return reviewOutcome{}, err
	}
	text := "finding " + key + " restored as " + got.Ref
	if got.Note != "" {
		text += " (" + got.Note + ")"
	}
	entry, err := logEntry(logOrder, "", orderID, text, map[string]any{"key": key, "ref": got.Ref})
	if err != nil {
		return reviewOutcome{}, err
	}
	entries := append(append(notes, got.Notes...), entry)
	if _, err := d.store.ReviewFinding(ctx, orderID, key, f.State, store.FindingRestored, got.Ref, &got.Note, entries...); err != nil {
		return reviewOutcome{}, err
	}
	f, err = d.store.GetFinding(ctx, orderID, key)
	return reviewOutcome{State: f.State}, err
}
