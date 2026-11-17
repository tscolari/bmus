package bridge

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/dbos-inc/dbos-transact-golang/dbos"

	"github.com/tscolari/bmus/internal/comms"
	"github.com/tscolari/bmus/internal/store"
	"github.com/tscolari/bmus/internal/tracker"
)

// The findings workflow (#155, spec/campaigns-stage-2-plan-2026-11-14.md
// section 4) files an order's findings as tracker issues once its mission
// has ended. Command triages them first, advisorily; then each finding it
// didn't drop is filed, blockers first.

// findingsWorkflowName is the findings workflow's registered name.
const findingsWorkflowName = "findings"

// findingsWorkflowID is the workflow id of order orderID's findings
// workflow, the deliver_to of the hails it raises.
func findingsWorkflowID(orderID string) string { return "findings/" + orderID }

const (
	// labelUnreviewed marks an issue filed from a finding until the
	// captain keeps it (#156).
	labelUnreviewed = "bmus:unreviewed"
	// maxBacklog caps the open issues triage reads (plan, Decisions).
	maxBacklog = 200
	// maxTriageBody caps each finding's body in the triage context, in
	// bytes; it shrinks when the findings alone outgrow the context.
	maxTriageBody = 2 << 10
	// maxFindingWhy caps a finding's why: Command's rationale or the
	// filing error.
	maxFindingWhy = 2 << 10
	// maxTriageTitle caps the order's title in the triage context.
	maxTriageTitle = 200
	// maxTriageTightTitle and maxTriageTightList cap each finding's
	// title, labels and blockers when the findings outgrow the triage
	// context with empty bodies.
	maxTriageTightTitle = 100
	maxTriageTightList  = 2
	// retriesBacklogDown is file-finding's retries once list-backlog
	// failed: the tracker is likely down, and 5 slow retries per finding
	// would hold filing up for long.
	retriesBacklogDown = 2
	// trackerCallTimeout bounds each tracker call the workflow makes.
	trackerCallTimeout = 2 * time.Minute
)

// trackerRetries are the retries of list-backlog and file-finding: 5, with
// backoff up to 2 minutes. Config.trackerRetries replaces them in tests.
var trackerRetries = []dbos.StepOption{dbos.WithStepMaxRetries(5),
	dbos.WithStepBaseInterval(2 * time.Second), dbos.WithStepMaxInterval(2 * time.Minute)}

// FindingsInput is the input of a findings workflow, id findings/<OrderID>.
type FindingsInput struct {
	OrderID string
}

// FindingsResult is how a findings workflow ended: how many findings it
// filed, how many triage dropped and how many failed to file, then how
// many issues the captain's review kept and discarded and how many
// findings it restored (#156). NotFiled says why nothing was filed, when
// nothing was tried.
type FindingsResult struct {
	Filed, Dropped, Failed    int
	Kept, Discarded, Restored int `json:",omitempty"`
	// ReviewFailed counts the review's actions out of retries (#156).
	ReviewFailed int    `json:",omitempty"`
	NotFiled     string `json:",omitempty"`
}

// findingsBegin is the begin step's recorded result.
type findingsBegin struct {
	// Since is when begin ran: every marker's Since (tracker.Mark).
	Since time.Time
	// Nonce is the database's (store.Nonce), in every marker's key.
	Nonce string
	// Title is the order's.
	Title string
	// NoTracker: the bridge has no tracker, and nothing is filed.
	NoTracker bool
	// Findings are the order's new findings, by key.
	Findings []store.Finding
}

// backlogItem is an open issue triage offers Command.
type backlogItem struct {
	Ref       string   `json:"ref"`
	Title     string   `json:"title"`
	Labels    []string `json:"labels,omitempty"`
	BlockedBy []string `json:"blocked_by,omitempty"`
}

// triageOutcome is the apply-triage step's recorded result: what triage
// does to each finding, or why it was skipped.
type triageOutcome struct {
	Triage  []store.FindingTriage `json:",omitempty"`
	Skipped string                `json:",omitempty"`
}

// filingPlan is the plan-filing step's recorded result.
type filingPlan struct {
	// Order lists the keys to file, blockers before the findings they
	// block.
	Order []string
	// Blockers are each key's blockers: canonical refs and keys of the
	// order's findings.
	Blockers map[string][]string
}

// filedFinding is the file-finding step's recorded result: the finding's
// state after the step, and its issue.
type filedFinding struct {
	State string
	Ref   string `json:",omitempty"`
}

// startFindings is the end of every mission (#155): a count-findings step,
// and, when the order has any findings, findings/<order> started as a
// child the mission never awaits. A replay, or a findings workflow already
// started, starts nothing new. A review order's count is 0 (#182): its
// task workflow starts findings/<order> once it has triaged the verdict,
// so the reviewer's own findings are filed with the verdict's.
func startFindings(ctx dbos.Context, orderID string) error {
	n, err := dbos.RunAsStep(ctx, func(c context.Context) (int, error) {
		d := daemonFrom(c)
		o, err := d.store.GetOrder(c, orderID)
		if err != nil {
			return 0, fmt.Errorf("order %s: %w", orderID, err)
		}
		if o.Kind == store.OrderKindReview {
			return 0, nil
		}
		return d.store.CountFindings(c, orderID)
	}, append([]dbos.StepOption{dbos.WithStepName("count-findings")}, storeRetries...)...)
	if err != nil || n == 0 {
		return err
	}
	_, err = dbos.RunWorkflow(ctx, findingsWorkflow, FindingsInput{OrderID: orderID},
		dbos.WithWorkflowID(findingsWorkflowID(orderID)))
	return err
}

// findingsWorkflow files order in.OrderID's findings (#155):
//
//  1. begin records the time, the marker's Since and the database's
//     nonce, and reads the new findings. Without a tracker it logs
//     "findings of ORD-0001 not filed: no tracker configured" and ends;
//     the rows stay new.
//  2. list-backlog reads up to maxBacklog open issues. A failure skips
//     triage with the error as its reason.
//  3. Triage (triageFindings): a findings_triage hail Command alone
//     answers, advisorily. A skipped or unusable triage files everything.
//  4. plan-filing makes blockers canonical and orders the findings
//     blockers first; then one file-finding step per finding not
//     dropped, and finding-failed for one out of retries.
//  5. findings-summary logs "findings of ORD-0001: filed 4, dropped 1,
//     failed 0".
//  6. The captain's review (reviewFindings, #156): a tracker_review hail
//     when anything was filed, dropped or failed, a wait with no
//     deadline, and the answer acted on.
//
// Its notes are written once per order (store.AppendLogOnce), so a step
// re-run after its transaction committed logs nothing twice.
//
// The rows hold the outcome: dropped, filed or failed, with why, ref and
// duplicate_of, then kept, discarded or restored once reviewed. The
// tracker_review hail is raised from them.
func findingsWorkflow(ctx dbos.Context, in FindingsInput) (FindingsResult, error) {
	b, err := dbos.RunAsStep(ctx, func(c context.Context) (findingsBegin, error) {
		return daemonFrom(c).beginFindings(c, in.OrderID)
	}, append([]dbos.StepOption{dbos.WithStepName("begin")}, storeRetries...)...)
	if err != nil {
		return FindingsResult{}, err
	}
	if b.NoTracker {
		return FindingsResult{NotFiled: "no tracker configured"}, nil
	}
	if len(b.Findings) == 0 {
		return FindingsResult{}, nil
	}
	fs, backlogDown, err := triageFindings(ctx, in.OrderID, b)
	if err != nil {
		return FindingsResult{}, err
	}
	res, err := fileFindings(ctx, in.OrderID, b, fs, backlogDown)
	if err != nil {
		return res, err
	}
	err = storeStep(ctx, "findings-summary", func(c context.Context, d *Daemon) error {
		return d.logOnce(c, in.OrderID, "summary", fmt.Sprintf("findings of %s: filed %d, dropped %d, failed %d",
			in.OrderID, res.Filed, res.Dropped, res.Failed), map[string]any{"filed": res.Filed, "dropped": res.Dropped, "failed": res.Failed})
	})
	if err != nil {
		return res, err
	}
	err = reviewFindings(ctx, in.OrderID, b, &res)
	return res, err
}

// beginFindings is the begin step.
func (d *Daemon) beginFindings(ctx context.Context, orderID string) (findingsBegin, error) {
	o, err := d.store.GetOrder(ctx, orderID)
	if err != nil {
		return findingsBegin{}, fmt.Errorf("order %s: %w", orderID, err)
	}
	all, err := d.store.OrderFindings(ctx, orderID)
	if err != nil {
		return findingsBegin{}, err
	}
	b := findingsBegin{Since: time.Now().UTC(), Nonce: d.store.Nonce(), Title: o.Title}
	for _, f := range all {
		if f.State == store.FindingNew {
			b.Findings = append(b.Findings, f)
		}
	}
	if d.tracker == nil {
		b.NoTracker = true
		err = d.logOnce(ctx, orderID, "not-filed", "findings of "+orderID+" not filed: no tracker configured", nil)
	}
	return b, err
}

// logOnce appends text to order orderID's log, kind order, unless a step
// re-run already did (store.AppendLogOnce under once).
func (d *Daemon) logOnce(ctx context.Context, orderID, once, text string, payload any) error {
	e, err := logEntry(logOrder, "", orderID, text, payload)
	if err != nil {
		return err
	}
	return d.store.AppendLogOnce(ctx, orderID, once, e)
}

// ---- triage ----

// triageFindings runs list-backlog, raises the findings_triage hail and
// waits for it (raiseBridgeHail, awaitBridgeHail), then applies Command's
// answer (apply-triage). It returns fs with the triage applied. Triage is
// advisory: a hail skip-advisory closed, or an answer that doesn't parse,
// files everything, and the decision has no countermand window to wait
// out (classify gives advisory hails none). It also reports whether the
// backlog couldn't be read, which shortens filing's retries.
func triageFindings(ctx dbos.Context, orderID string, b findingsBegin) ([]store.Finding, bool, error) {
	fs := slices.Clone(b.Findings)
	var (
		skip    string
		hail    store.Hail
		offered []string
	)
	backlog, err := dbos.RunAsStep(ctx, func(c context.Context) ([]backlogItem, error) {
		return daemonFrom(c).listBacklog(c)
	}, append([]dbos.StepOption{dbos.WithStepName("list-backlog")}, daemonFrom(ctx).trackerRetries...)...)
	backlogDown := err != nil
	if err != nil {
		if ctx.Err() != nil {
			return nil, false, err
		}
		skip = "the backlog could not be read: " + stepError(err)
	}
	var (
		hctx json.RawMessage
		n    int
	)
	if skip == "" {
		if hctx, n, err = triageContext(orderID, b.Title, fs, backlog); errors.Is(err, errTriageTooBig) {
			skip = err.Error()
		} else if err != nil {
			return nil, false, err
		}
	}
	if skip == "" {
		for _, it := range backlog[:n] {
			offered = append(offered, it.Ref)
		}
		id, err := raiseBridgeHail(ctx, store.Hail{Category: categoryFindingsTriage, Question: triageQuestion(orderID, len(fs)),
			Context: hctx}, store.LogEntry{Kind: logHail, OrderID: orderID,
			Text: fmt.Sprintf("Command asked to triage %s of %s", countFindings(len(fs)), orderID)})
		if err != nil {
			return nil, false, err
		}
		if hail, err = awaitBridgeHail(ctx, id); err != nil {
			return nil, false, err
		}
		if hail.Status != store.HailDecided {
			skip = cmp.Or(hail.Rationale, "hail "+id+" closed "+hail.Status)
		}
	}
	keys := make([]string, len(fs))
	for i, f := range fs {
		keys[i] = f.Key
	}
	out, err := dbos.RunAsStep(ctx, func(c context.Context) (triageOutcome, error) {
		return daemonFrom(c).applyTriage(c, orderID, keys, offered, hail, skip)
	}, append([]dbos.StepOption{dbos.WithStepName("apply-triage")}, storeRetries...)...)
	if err != nil {
		return nil, false, err
	}
	for _, t := range out.Triage {
		i := slices.IndexFunc(fs, func(f store.Finding) bool { return f.Key == t.Key })
		if i < 0 {
			continue
		}
		if t.Drop {
			fs[i].State, fs[i].Why, fs[i].DuplicateOf = store.FindingDropped, t.Why, t.DuplicateOf
		}
		for _, k := range t.AddBlockedBy {
			if !slices.Contains(fs[i].BlockedBy, k) {
				fs[i].BlockedBy = append(fs[i].BlockedBy, k)
			}
		}
	}
	return fs, backlogDown, nil
}

// listBacklog is the list-backlog step: the newest maxBacklog open issues.
func (d *Daemon) listBacklog(ctx context.Context) ([]backlogItem, error) {
	ctx, cancel := context.WithTimeout(ctx, trackerCallTimeout)
	defer cancel()
	items, err := d.tracker.List(ctx, tracker.Query{Limit: maxBacklog})
	if err != nil {
		return nil, err
	}
	out := make([]backlogItem, 0, len(items))
	for _, it := range items {
		out = append(out, backlogItem{Ref: it.Ref, Title: oneLine(it.Title), Labels: it.Labels, BlockedBy: it.BlockedBy})
	}
	return out, nil
}

// triageFinding is a finding as the triage context shows it.
type triageFinding struct {
	N         int      `json:"n"`
	Key       string   `json:"key"`
	Title     string   `json:"title"`
	Body      string   `json:"body"`
	Labels    []string `json:"labels,omitempty"`
	Severity  string   `json:"severity,omitempty"`
	BlockedBy []string `json:"blocked_by,omitempty"`
}

// triageHailContext is a findings_triage hail's context.
type triageHailContext struct {
	Order    string          `json:"order"`
	Title    string          `json:"title"`
	Findings []triageFinding `json:"findings"`
	Backlog  []backlogItem   `json:"backlog"`
	// BacklogOmitted counts the backlog entries left out to keep the
	// context within maxConsultContext.
	BacklogOmitted int `json:"backlog_omitted"`
}

// triageContext is the findings_triage hail's context for findings fs of
// order orderID, and how many of backlog's entries it holds: the order's
// id and title, the findings numbered from 1 (each body cut to
// maxTriageBody), then backlog entries while the encoded context stays
// within maxConsultContext, and how many were left out. When the findings
// alone outgrow it, their bodies are cut shorter, halving the cap until
// they fit or there is no body left; then titles are cut to
// maxTriageTightTitle and each finding keeps its first maxTriageTightList
// labels and blockers. Findings that still don't fit are errTriageTooBig.
func triageContext(orderID, title string, fs []store.Finding, backlog []backlogItem) (json.RawMessage, int, error) {
	c := triageHailContext{Order: orderID, Title: clip(oneLine(title), maxTriageTitle), Backlog: []backlogItem{},
		BacklogOmitted: len(backlog)}
	var base []byte
	tight := false
	for cut := maxTriageBody; ; cut /= 2 {
		c.Findings = c.Findings[:0]
		for i, f := range fs {
			tf := triageFinding{N: i + 1, Key: f.Key, Title: f.Title, Body: clip(f.Body, cut),
				Labels: f.Labels, Severity: f.Severity, BlockedBy: f.BlockedBy}
			if tight {
				tf.Title = clip(tf.Title, maxTriageTightTitle)
				tf.Labels = tf.Labels[:min(len(tf.Labels), maxTriageTightList)]
				tf.BlockedBy = tf.BlockedBy[:min(len(tf.BlockedBy), maxTriageTightList)]
			}
			c.Findings = append(c.Findings, tf)
		}
		var err error
		if base, err = json.Marshal(c); err != nil {
			return nil, 0, err
		}
		if len(base) <= maxConsultContext {
			break
		}
		if cut == 0 && tight {
			return nil, 0, fmt.Errorf("%w (%d bytes without the backlog)", errTriageTooBig, len(base))
		}
		if cut == 0 {
			tight, cut = true, 1 // one more pass at cut 0
		}
	}
	size := len(base)
	for _, it := range backlog {
		e, err := json.Marshal(it)
		if err != nil {
			return nil, 0, err
		}
		if size+len(e)+1 > maxConsultContext {
			break
		}
		size += len(e) + 1
		c.Backlog = append(c.Backlog, it)
	}
	c.BacklogOmitted = len(backlog) - len(c.Backlog)
	raw, err := json.Marshal(c)
	return raw, len(c.Backlog), err
}

// errTriageTooBig: the findings don't fit the triage context even cut
// down (triageContext), and triage is skipped.
var errTriageTooBig = fmt.Errorf("the findings don't fit the triage context's %d KiB", maxConsultContext>>10)

// triageQuestion is the findings_triage hail's question, written by the
// bridge.
func triageQuestion(orderID string, n int) string {
	return fmt.Sprintf("Triage %s from %s against the open backlog in the context. "+
		"Answer with a JSON object of finding numbers and refs: drop (numbers that are noise), "+
		"duplicates (a number to the ref it repeats), blocked_by (a number to refs or f<number>). {} files them all.",
		countFindings(n), orderID)
}

// countFindings is "1 finding" or "n findings".
func countFindings(n int) string {
	if n == 1 {
		return "1 finding"
	}
	return strconv.Itoa(n) + " findings"
}

// findingNumberRe is a finding named by number in a triage answer's
// blocked_by: f1.
var findingNumberRe = regexp.MustCompile(`^f[0-9]+$`)

// triageAnswer is Command's triage answer: finding numbers (from 1) and
// refs.
type triageAnswer struct {
	Drop       []int               `json:"drop"`
	Duplicates map[string]string   `json:"duplicates"`
	BlockedBy  map[string][]string `json:"blocked_by"`
}

// applyTriage is the apply-triage step. With skip set, or a hail that
// wasn't decided, triage is skipped: it logs "findings of ORD-0001 filed
// without triage: <why>". Otherwise it parses Command's decision
// (parseTriage) against the batch (keys, numbered from 1) and the offered
// backlog refs; one that doesn't parse is skipped the same way. A parsed
// one writes dropped, why (Command's rationale) and duplicate_of on the
// rows and adds blockers (store.TriageFindings), and logs what it did.
func (d *Daemon) applyTriage(ctx context.Context, orderID string, keys, offered []string, h store.Hail, skip string) (triageOutcome, error) {
	var triage []store.FindingTriage
	if skip == "" {
		var err error
		if triage, err = parseTriage(h.Decision, keys, offered, d.tracker.Canon); err != nil {
			skip = "Command's answer was not used: " + err.Error()
		}
	}
	if skip != "" {
		return triageOutcome{Skipped: skip}, d.logOnce(ctx, orderID, store.TriageOnce,
			"findings of "+orderID+" filed without triage: "+skip, map[string]any{"hail_id": h.ID, "why": skip})
	}
	why := clip(h.Rationale, maxFindingWhy)
	dropped, dups, blockers := 0, 0, 0
	for i := range triage {
		if triage[i].Drop {
			triage[i].Why = why
			dropped++
			if triage[i].DuplicateOf != "" {
				dups++
			}
		}
		blockers += len(triage[i].AddBlockedBy)
	}
	entry, err := logEntry(logOrder, "", orderID, fmt.Sprintf("findings of %s triaged by Command: dropped %d (%d as duplicates), %d blockers added",
		orderID, dropped, dups, blockers), map[string]any{"hail_id": h.ID})
	if err != nil {
		return triageOutcome{}, err
	}
	return triageOutcome{Triage: triage}, d.store.TriageFindings(ctx, orderID, triage, entry)
}

// parseTriage reads Command's triage answer strictly: one JSON object (a
// ```json fence around it is taken off) with only drop, duplicates and
// blocked_by. Every number names a finding of keys (from 1), every ref
// is one of offered once canon makes it canonical, and blocked_by's
// entries are such refs or f<number>. A finding can't be dropped and a
// duplicate, or block itself. It returns what to do to each finding
// named, in the batch's order.
func parseTriage(answer string, keys, offered []string, canon func(string) (string, error)) ([]store.FindingTriage, error) {
	s := strings.TrimSpace(answer)
	if rest, ok := strings.CutPrefix(s, "```"); ok {
		_, body, _ := strings.Cut(rest, "\n")
		s = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(body), "```"))
	}
	if !strings.HasPrefix(s, "{") {
		return nil, errors.New("not a JSON object")
	}
	var a triageAnswer
	if err := decodeStrict(json.RawMessage(s), &a); err != nil {
		return nil, err
	}
	number := func(k string) (int, error) {
		n, err := strconv.Atoi(k)
		if err != nil || strconv.Itoa(n) != k || n < 1 || n > len(keys) {
			return 0, fmt.Errorf("%q is not a finding number from 1 to %d", k, len(keys))
		}
		return n - 1, nil
	}
	ref := func(r string) (string, error) {
		c, err := canon(r)
		if err != nil || !slices.Contains(offered, c) {
			return "", fmt.Errorf("%q is not a ref in the backlog offered", r)
		}
		return c, nil
	}
	t := make([]store.FindingTriage, len(keys))
	for i, k := range keys {
		t[i].Key = k
	}
	for _, n := range a.Drop {
		i, err := number(strconv.Itoa(n))
		if err != nil {
			return nil, fmt.Errorf("drop: %w", err)
		}
		if t[i].Drop {
			return nil, fmt.Errorf("drop: %d named twice", n)
		}
		t[i].Drop = true
	}
	for k, r := range a.Duplicates {
		i, err := number(k)
		if err != nil {
			return nil, fmt.Errorf("duplicates: %w", err)
		}
		if t[i].Drop {
			return nil, fmt.Errorf("duplicates: %s is dropped already", k)
		}
		if t[i].DuplicateOf, err = ref(r); err != nil {
			return nil, fmt.Errorf("duplicates: %w", err)
		}
	}
	for i := range t {
		if t[i].DuplicateOf != "" {
			t[i].Drop = true
		}
	}
	for k, bs := range a.BlockedBy {
		i, err := number(k)
		if err != nil {
			return nil, fmt.Errorf("blocked_by: %w", err)
		}
		for _, b := range bs {
			var add string
			if findingNumberRe.MatchString(b) {
				j, err := number(b[1:])
				if err != nil {
					return nil, fmt.Errorf("blocked_by: %w", err)
				}
				if j == i {
					return nil, fmt.Errorf("blocked_by: f%s blocks itself", k)
				}
				add = keys[j]
			} else if add, err = ref(b); err != nil {
				return nil, fmt.Errorf("blocked_by: %w", err)
			}
			if !slices.Contains(t[i].AddBlockedBy, add) {
				t[i].AddBlockedBy = append(t[i].AddBlockedBy, add)
			}
		}
	}
	return slices.DeleteFunc(t, func(t store.FindingTriage) bool { return !t.Drop && len(t.AddBlockedBy) == 0 }), nil
}

// ---- filing ----

// fileFindings files fs's findings that triage didn't drop, in
// plan-filing's order: one file-finding step each, with trackerRetries
// (retriesBacklogDown of them when backlogDown), and finding-failed for
// one out of retries, after which filing goes on (resolveBlockers links
// the blockers).
func fileFindings(ctx dbos.Context, orderID string, b findingsBegin, fs []store.Finding, backlogDown bool) (FindingsResult, error) {
	var res FindingsResult
	byKey := map[string]store.Finding{}
	for _, f := range fs {
		byKey[f.Key] = f
		if f.State == store.FindingDropped {
			res.Dropped++
		}
	}
	plan, err := dbos.RunAsStep(ctx, func(c context.Context) (filingPlan, error) {
		return daemonFrom(c).planFiling(c, orderID, fs)
	}, append([]dbos.StepOption{dbos.WithStepName("plan-filing")}, storeRetries...)...)
	if err != nil {
		return res, err
	}
	retries := append([]dbos.StepOption{dbos.WithStepName("file-finding")}, daemonFrom(ctx).trackerRetries...)
	if backlogDown {
		retries = append(retries, dbos.WithStepMaxRetries(retriesBacklogDown))
	}
	refs := map[string]string{} // filed key to its issue
	for _, key := range plan.Order {
		blockers := resolveBlockers(plan.Blockers[key], refs, byKey)
		out, err := dbos.RunAsStep(ctx, func(c context.Context) (filedFinding, error) {
			return daemonFrom(c).fileFinding(c, orderID, key, blockers, b)
		}, retries...)
		if err != nil {
			if ctx.Err() != nil {
				return res, err
			}
			why := clip(stepError(err), maxFindingWhy)
			if out, err = dbos.RunAsStep(ctx, func(c context.Context) (filedFinding, error) {
				return daemonFrom(c).findingFailed(c, orderID, key, why)
			}, append([]dbos.StepOption{dbos.WithStepName("finding-failed")}, storeRetries...)...); err != nil {
				return res, err
			}
		}
		switch out.State {
		case store.FindingFiled:
			refs[key] = out.Ref
			res.Filed++
		case store.FindingFailed:
			res.Failed++
		}
	}
	return res, nil
}

// resolveBlockers is a finding's blockers as refs to link, each once: a
// ref as it is, and a finding of the order (a key) as its issue (refs, the
// filed ones), or the issue a duplicate repeats. A dropped or failed
// finding links nothing.
func resolveBlockers(blockers []string, refs map[string]string, byKey map[string]store.Finding) []string {
	var out []string
	for _, bl := range blockers {
		if comms.FindingKey(bl) {
			bl = cmp.Or(refs[bl], byKey[bl].DuplicateOf)
		}
		if bl != "" && !slices.Contains(out, bl) {
			out = append(out, bl)
		}
	}
	return out
}

// findingFailed is the finding-failed step, for a finding file-finding
// gave up on with error why. A finding with a ref stored has its issue,
// short of a link: it is filed, with why noted, so its dependents link to
// it. Any other is failed, with why.
func (d *Daemon) findingFailed(ctx context.Context, orderID, key, why string) (filedFinding, error) {
	f, err := d.store.GetFinding(ctx, orderID, key)
	if err != nil {
		return filedFinding{}, err
	}
	state, text := store.FindingFailed, "finding "+key+" not filed: "+why
	if f.Ref != "" {
		state, text = store.FindingFiled, "finding "+key+" filed as "+f.Ref+" ("+why+")"
	}
	entry, err := logEntry(logOrder, "", orderID, text, map[string]any{"key": key, "ref": f.Ref})
	if err != nil {
		return filedFinding{}, err
	}
	if _, err := d.store.SettleFinding(ctx, orderID, key, state, "", why, entry); err != nil {
		return filedFinding{}, err
	}
	if f, err = d.store.GetFinding(ctx, orderID, key); err != nil {
		return filedFinding{}, err
	}
	return filedFinding{State: f.State, Ref: f.Ref}, nil
}

// planFiling is the plan-filing step: each finding's blockers and the
// order to file them in, blockers first. A blocker naming a key is one of
// the order's findings; a key the order doesn't have, or the finding's
// own, is dropped. A ref is made canonical (tracker Canon), and one Canon
// refuses (another repository's, or one it can't read) is dropped. A
// topological sort over the keys of findings to file drops the edge that
// closes a cycle. Each dropped blocker is logged with why.
func (d *Daemon) planFiling(ctx context.Context, orderID string, fs []store.Finding) (filingPlan, error) {
	plan := filingPlan{Blockers: map[string][]string{}}
	known := map[string]store.Finding{}
	for _, f := range fs {
		known[f.Key] = f
	}
	var notes []string
	note := func(key, blocker, why string) {
		notes = append(notes, fmt.Sprintf("finding %s: blocker %s dropped: %s", key, blocker, why))
	}
	var keys []string
	for _, f := range fs {
		if f.State == store.FindingDropped {
			continue
		}
		keys = append(keys, f.Key)
		var bs []string
		for _, b := range f.BlockedBy {
			switch _, ok := known[b]; {
			case comms.FindingKey(b) && b == f.Key:
				note(f.Key, b, "a finding can't block itself")
				continue
			case comms.FindingKey(b) && !ok:
				note(f.Key, b, "it names no finding of "+orderID)
				continue
			case !comms.FindingKey(b):
				c, err := d.tracker.Canon(b)
				if err != nil {
					note(f.Key, b, err.Error())
					continue
				}
				b = c
			}
			if !slices.Contains(bs, b) {
				bs = append(bs, b)
			}
		}
		plan.Blockers[f.Key] = bs
	}
	slices.Sort(keys)
	const (
		visiting = 1
		done     = 2
	)
	state := map[string]int{}
	var visit func(k string)
	visit = func(k string) {
		state[k] = visiting
		bs := plan.Blockers[k][:0:0]
		for _, b := range plan.Blockers[k] {
			if _, ok := plan.Blockers[b]; ok && comms.FindingKey(b) {
				// A finding to file: it goes first.
				if state[b] == visiting {
					note(k, b, "it closes a cycle")
					continue
				}
				if state[b] == 0 {
					visit(b)
				}
			}
			bs = append(bs, b)
		}
		plan.Blockers[k] = bs
		state[k] = done
		plan.Order = append(plan.Order, k)
	}
	for _, k := range keys {
		if state[k] == 0 {
			visit(k)
		}
	}
	var entries []store.LogEntry
	for _, n := range notes {
		entries = append(entries, store.LogEntry{Kind: logOrder, OrderID: orderID, Text: n})
	}
	if len(entries) > 0 {
		if err := d.store.AppendLogOnce(ctx, orderID, "plan-filing", entries...); err != nil {
			return filingPlan{}, err
		}
	}
	return plan, nil
}

// fileFinding is the file-finding step: it files finding key of order
// orderID with blockers (createFindingIssue), under the marker "finding <nonce>
// <order>/<key>" since b.Since, so a re-run finds the issue a lost answer
// created and adds only its missing links. A finding no longer new (a
// re-run after it settled) returns its row as it is.
//
//   - Filed: the row gets the ref and state filed, and the log "finding
//     <key> filed as o/r#12".
//   - Filed without labels the tracker dropped (tracker.ErrLabels): the
//     same, with the error as a note on the row and in the entry. Retrying
//     can't add them.
//   - A ref with any other error (a link that failed): the ref is stored
//     and the step fails, so its retry adds the missing links.
//   - Not found and no ref, with blockers: a blocker that no longer
//     exists fails Create before it files anything. Each blocker is read
//     (Get), those not found are dropped with a note, and Create runs
//     again in the same attempt.
//   - No ref otherwise: the step fails.
func (d *Daemon) fileFinding(ctx context.Context, orderID, key string, blockers []string, b findingsBegin) (filedFinding, error) {
	f, err := d.store.GetFinding(ctx, orderID, key)
	if err != nil {
		return filedFinding{}, fmt.Errorf("finding %s/%s: %w", orderID, key, err)
	}
	if f.State != store.FindingNew {
		return filedFinding{State: f.State, Ref: f.Ref}, nil
	}
	got, err := d.createFindingIssue(ctx, orderID, f, blockers, b, true)
	switch {
	case err == nil:
	case got.Ref != "":
		// A link that failed: the retry adds it.
		if serr := d.store.SetFindingRef(ctx, orderID, key, got.Ref); serr != nil {
			return filedFinding{}, errors.Join(err, serr)
		}
		return filedFinding{}, err
	default:
		return filedFinding{}, err
	}
	text := "finding " + key + " filed as " + got.Ref
	if got.Note != "" {
		text += " (" + got.Note + ")"
	}
	entry, err := logEntry(logOrder, "", orderID, text, map[string]any{"key": key, "ref": got.Ref})
	if err != nil {
		return filedFinding{}, err
	}
	settled, err := d.store.SettleFinding(ctx, orderID, key, store.FindingFiled, got.Ref, got.Note, append(got.Notes, entry)...)
	if err != nil || settled {
		return filedFinding{State: store.FindingFiled, Ref: got.Ref}, err
	}
	f, err = d.store.GetFinding(ctx, orderID, key)
	return filedFinding{State: f.State, Ref: f.Ref}, err
}

// findingIssue is what createFindingIssue filed: the issue's ref, a note
// when the tracker dropped its labels, and the notes of blockers dropped
// because they no longer exist.
type findingIssue struct {
	Ref   string
	Note  string
	Notes []store.LogEntry
}

// createFindingIssue files finding f of order orderID as an issue with
// blockers, under the marker "finding <nonce> <order>/<key>" since
// b.Since, labelled with the finding's labels, plus bmus:unreviewed when
// unreviewed (#155's filing; #156's restore files without it). A re-run
// finds the issue a lost answer created and adds only its missing links.
//
//   - Filed without labels the tracker dropped (tracker.ErrLabels): no
//     error, with the error as Note. Retrying can't add them.
//   - A ref with any other error (a link that failed): the ref and the
//     error, so the caller's retry adds the missing links.
//   - Not found and no ref, with blockers: a blocker that no longer
//     exists fails Create before it files anything. Each blocker is read
//     (Get), those not found are dropped with a note, and Create runs
//     again.
//   - Without unreviewed, an issue with bmus:unreviewed (one an attempt
//     whose answer was lost filed under the marker) has it removed, before
//     a link error returns.
func (d *Daemon) createFindingIssue(ctx context.Context, orderID string, f store.Finding, blockers []string, b findingsBegin,
	unreviewed bool) (findingIssue, error) {
	sameRepo := func(ref string) bool {
		_, err := d.tracker.Canon(ref)
		return err == nil
	}
	labels := slices.Clone(f.Labels)
	if unreviewed {
		labels = append(labels, labelUnreviewed)
	}
	it := tracker.Item{Title: findingTitle(f.Title, sameRepo), Body: findingBody(f, orderID), Labels: labels, BlockedBy: blockers}
	m := findingMark(b, orderID, f.Key)
	got, err := d.createFinding(ctx, it, m)
	var out findingIssue
	if errors.Is(err, tracker.ErrNotFound) && got.Ref == "" && len(it.BlockedBy) > 0 {
		var kept []string
		for _, bl := range it.BlockedBy {
			gctx, cancel := context.WithTimeout(ctx, trackerCallTimeout)
			_, gerr := d.tracker.Get(gctx, bl)
			cancel()
			if errors.Is(gerr, tracker.ErrNotFound) {
				out.Notes = append(out.Notes, store.LogEntry{Kind: logOrder, OrderID: orderID,
					Text: "finding " + f.Key + ": blocker " + bl + " dropped: " + gerr.Error()})
				continue
			}
			kept = append(kept, bl)
		}
		if len(out.Notes) > 0 {
			it.BlockedBy = kept
			got, err = d.createFinding(ctx, it, m)
		}
	}
	out.Ref = got.Ref
	if !unreviewed && got.Ref != "" && slices.ContainsFunc(got.Labels, func(l string) bool { return strings.EqualFold(l, labelUnreviewed) }) {
		// Before any link error returns: an issue found under the marker
		// loses the label whatever else is left to do.
		uctx, cancel := context.WithTimeout(ctx, trackerCallTimeout)
		uerr := d.tracker.Unlabel(uctx, got.Ref, labelUnreviewed)
		cancel()
		if uerr != nil {
			return out, errors.Join(err, uerr)
		}
	}
	switch {
	case err == nil:
	case errors.Is(err, tracker.ErrLabels) && got.Ref != "":
		out.Note = clip(err.Error(), maxFindingWhy)
	default:
		return out, err
	}
	return out, nil
}

// findingMark is the marker finding key of order orderID is filed under:
// "finding <nonce> <order>/<key>" since b.Since. A restore (#156) files
// under the same one.
func findingMark(b findingsBegin, orderID, key string) tracker.Mark {
	return tracker.Mark{Key: "finding " + b.Nonce + " " + orderID + "/" + key, Since: b.Since}
}

// createFinding is one Create, bounded by trackerCallTimeout.
func (d *Daemon) createFinding(ctx context.Context, it tracker.Item, m tracker.Mark) (tracker.Item, error) {
	ctx, cancel := context.WithTimeout(ctx, trackerCallTimeout)
	defer cancel()
	return d.tracker.Create(ctx, it, m)
}

// stepError is the last attempt's error of a step that ran out of
// retries: DBOS joins every attempt's error after its own prefix.
func stepError(err error) string {
	var joined interface{ Unwrap() []error }
	if errors.As(err, &joined) {
		if errs := joined.Unwrap(); len(errs) > 0 {
			return errs[len(errs)-1].Error()
		}
	}
	msg := err.Error()
	if _, rest, ok := strings.Cut(msg, " retries: "); ok && strings.HasPrefix(msg, "Step ") {
		msg = rest
	}
	if i := strings.LastIndexByte(msg, '\n'); i >= 0 {
		msg = msg[i+1:]
	}
	return msg
}

// ---- the issue's text ----

// findingBody is the body of finding f's issue: the agent's body as a
// text code block, then "Filed by <crew> from <order> (severity high)."
// as the bridge's own text.
//
// GitHub links and notifies nothing inside a code block: no @-mention,
// issue reference, URL, HTML or entity in the body reaches anyone (#153's
// review). The fence is a backtick run longer than any in the body, and
// at least three, so no line of the body can close it, whatever fences,
// tildes or HTML the body holds. The tracker's marker follows the bridge's
// line, outside the block.
func findingBody(f store.Finding, orderID string) string {
	line := "Filed by " + f.CrewID + " from " + orderID
	if f.Severity != "" {
		line += " (severity " + f.Severity + ")"
	}
	line += "."
	body := strings.Trim(f.Body, "\n")
	if strings.TrimSpace(body) == "" {
		return line
	}
	fence := strings.Repeat("`", max(3, longestRun(body, '`')+1))
	return fence + "text\n" + body + "\n" + fence + "\n\n" + line
}

var (
	// mentionRe is a GitHub @-mention of a user, or of a team
	// (@org/team).
	mentionRe = regexp.MustCompile(`@[A-Za-z0-9](?:[A-Za-z0-9-]*[A-Za-z0-9])?(?:/[A-Za-z0-9](?:[A-Za-z0-9_.-]*[A-Za-z0-9_-])?)?`)
	// crossRefRe is a reference to an issue of a repository by name:
	// owner/name#12.
	crossRefRe = regexp.MustCompile(`[A-Za-z0-9][A-Za-z0-9-]*/[A-Za-z0-9._-]+#[0-9]+`)
)

// zeroWidthSpace breaks a mention or a reference in a title.
const zeroWidthSpace = "\u200b"

// findingTitle is finding title as filed: a zero-width space follows the
// @ of every @-mention, and precedes the # of every owner/name#N
// reference that sameRepo refuses, so the title notifies nobody and
// backlinks no other repository. The tracker's own refs stay.
func findingTitle(title string, sameRepo func(ref string) bool) string {
	title = crossRefRe.ReplaceAllStringFunc(title, func(ref string) string {
		if sameRepo(ref) {
			return ref
		}
		return strings.Replace(ref, "#", zeroWidthSpace+"#", 1)
	})
	return mentionRe.ReplaceAllStringFunc(title, func(m string) string { return "@" + zeroWidthSpace + m[1:] })
}
