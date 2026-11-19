package bridge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/dbos-inc/dbos-transact-golang/dbos"

	"github.com/tscolari/bmus/internal/comms"
	"github.com/tscolari/bmus/internal/regs"
	"github.com/tscolari/bmus/internal/store"
)

// Campaign hails (#183, spec/campaigns-stage-3-plan-2026-11-16.md section
// 4): a campaign's task workflow raises campaign_triage for a review's
// medium findings, campaign_stalled when the task stalls and campaign_land
// before it pushes. Their questions are the bridge's own and carry ids,
// refs, counts and option words only. Issue titles, finding text, commit
// subjects and summaries go in the context, out of reach of the option
// rules that read the question and the answer.
//
// The task workflow (#184) builds a hail (campaignTriageHail,
// campaignStalledHail, campaignLandHail), raises it under its key
// (campaignTriageKey, campaignStallKey, campaignLandKey) and waits for the
// decision it acts on (raiseCampaignHail). It reads the stored decision
// back with parseCampaignTriage or parseCampaignChoice.

// Campaign hail options.
const (
	campaignFile  = "file"
	campaignFix   = "fix"
	campaignDrop  = "drop"
	campaignLand  = "land"
	campaignRetry = "retry"
)

var (
	// campaignTriageOptions are a campaign_triage hail's options. Option
	// 0, file, is the default_option a captain timeout would pick.
	campaignTriageOptions = []string{campaignFile, campaignFix, campaignDrop}
	// campaignLandOptions are a campaign_land hail's options, and a
	// campaign_stalled hail's when the task has a head.
	campaignLandOptions = []string{campaignLand, campaignRetry, campaignDrop}
	// campaignHeadlessOptions are a campaign_stalled hail's options when
	// the task has no head to land.
	campaignHeadlessOptions = []string{campaignRetry, campaignDrop}
)

// Caps on what a campaign hail's context carries, in bytes.
const (
	// maxCampaignBody caps each finding's body; it shrinks when the
	// findings outgrow maxConsultContext (fitCampaignFindings).
	maxCampaignBody = 2 << 10
	// maxCampaignTitle caps the issue's title.
	maxCampaignTitle = 200
	// maxCampaignTightTitle and maxCampaignTightPath cap each finding's
	// title and path when the findings outgrow the context with empty
	// bodies.
	maxCampaignTightTitle = 100
	maxCampaignTightPath  = 100
	// maxCampaignCommits is how many commits a land context lists, and
	// maxCampaignSubject caps each subject.
	maxCampaignCommits = 50
	maxCampaignSubject = 200
	// maxCampaignSummary caps the passing review's summary.
	maxCampaignSummary = 8 << 10
	// maxCampaignReason caps a stall's reason, and maxCampaignTightReason
	// in fitCampaignFindings' tight pass.
	maxCampaignReason      = 2 << 10
	maxCampaignTightReason = 512
	// maxCampaignNote caps the note a stalled or land decision may carry.
	maxCampaignNote = 2 << 10
)

// campaignTriageKey, campaignStallKey and campaignLandKey are the raise
// keys of a task's triage in round round, its nth stall and its nth land
// hail (store.Hail.RaiseKey): a re-run of a raise with the same key
// returns the first hail.
func campaignTriageKey(round int) string { return "r" + strconv.Itoa(round) }
func campaignStallKey(n int) string      { return "stall-" + strconv.Itoa(n) }
func campaignLandKey(n int) string       { return "land-" + strconv.Itoa(n) }

// isCampaignHail reports whether h is a campaign hail a bridge workflow
// raised, whose decisions checkCampaignAnswer checks.
func isCampaignHail(h store.Hail) bool {
	if h.DeliverTo == "" {
		return false
	}
	switch h.Category {
	case regs.CategoryCampaignTriage, regs.CategoryCampaignStalled, regs.CategoryCampaignLand:
		return true
	}
	return false
}

// CampaignHailTask names the task a campaign hail is about: its campaign,
// its ref and the issue's title. The title is tracker text.
type CampaignHailTask struct {
	Campaign   string `json:"campaign"`
	Task       string `json:"task"`
	IssueTitle string `json:"issue_title"`
}

// CampaignFinding is a review's finding in a campaign hail's context,
// numbered from 1 (N). Title, Body and Path are the reviewer's text.
type CampaignFinding struct {
	N        int    `json:"n"`
	Severity string `json:"severity"`
	Title    string `json:"title"`
	Body     string `json:"body"`
	Path     string `json:"path,omitempty"`
	Line     int    `json:"line,omitempty"`
}

// CampaignTriageContext is a campaign_triage hail's context: the review
// order, its round and its medium findings, numbered from 1.
type CampaignTriageContext struct {
	CampaignHailTask
	Review   string            `json:"review"`
	Round    int               `json:"round"`
	Findings []CampaignFinding `json:"findings"`
}

// CampaignStalledContext is a campaign_stalled hail's context: the stall's
// code and reason, the round, the task's head (empty with none) and the
// order it stalled on (empty when it stalled on none), and the blocking
// findings of the last review.
type CampaignStalledContext struct {
	CampaignHailTask
	Code       string            `json:"code"`
	Reason     string            `json:"reason"`
	Round      int               `json:"round"`
	MaxRounds  int               `json:"max_rounds"`
	Head       string            `json:"head,omitempty"`
	HeadCommit string            `json:"head_commit,omitempty"`
	Order      string            `json:"order,omitempty"`
	Blocking   []CampaignFinding `json:"blocking"`
}

// CampaignCommit is a commit over a land's review base: its full id and
// subject, the subject cut to maxCampaignSubject.
type CampaignCommit struct {
	SHA     string `json:"sha"`
	Subject string `json:"subject"`
}

// CampaignLandContext is a campaign_land hail's context: where the head
// goes, the commits over the review base (at most maxCampaignCommits of
// CommitCount), the rounds the task took and the passing review.
type CampaignLandContext struct {
	CampaignHailTask
	Repo          string           `json:"repo"`
	Branch        string           `json:"branch"`
	Head          string           `json:"head"`
	HeadCommit    string           `json:"head_commit"`
	ReviewBase    string           `json:"review_base"`
	Commits       []CampaignCommit `json:"commits"`
	CommitCount   int              `json:"commit_count"`
	Rounds        int              `json:"rounds"`
	Review        string           `json:"review"`
	ReviewSummary string           `json:"review_summary"`
}

// commitIDRE is a full commit id.
var commitIDRE = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)

// stallCodeRE is a stall's code: a word of the bridge's, such as
// no_verdict, which the question carries.
var stallCodeRE = regexp.MustCompile(`^[a-z][a-z_]*$`)

// shortCommit is commit's first 12 characters, as questions show it.
func shortCommit(commit string) string { return commit[:min(len(commit), 12)] }

// check is what every campaign hail needs of t.
func (t CampaignHailTask) check() error {
	if t.Campaign == "" || t.Task == "" {
		return errors.New("a campaign hail needs its campaign and task")
	}
	return nil
}

// clean is t with its title made one line and cut to maxCampaignTitle.
func (t CampaignHailTask) clean() CampaignHailTask {
	t.IssueTitle = clip(oneLine(t.IssueTitle), maxCampaignTitle)
	return t
}

// campaignTriageHail builds the campaign_triage hail for c's review, with
// mediums, the review's medium findings, numbered from 1 in the context
// (c.Findings is replaced). Raise it under campaignTriageKey(c.Round).
//
// The question: "Triage 2 medium findings from ORD-0009's review in round
// 1: answer file, fix or drop for all of them, or a JSON object {"fix":
// [..], "file": [..], "drop": [..]} of finding numbers; unnamed findings
// are filed." It leaves out the task's ref, which is free text a
// captain_only rule could match (acme/salesforce#12 fits the
// delete|destroy|force option rule) and send every triage to the
// captain; the context names it.
func campaignTriageHail(c CampaignTriageContext, mediums []comms.VerdictFinding) (store.Hail, error) {
	if err := c.check(); err != nil {
		return store.Hail{}, err
	}
	if c.Review == "" || c.Round < 1 {
		return store.Hail{}, errors.New("a campaign_triage hail needs its review and round")
	}
	if len(mediums) == 0 {
		return store.Hail{}, errors.New("a campaign_triage hail needs a finding to triage")
	}
	c.CampaignHailTask = c.clean()
	raw, err := fitCampaignFindings(mediums, func(fs []CampaignFinding, _ bool) ([]byte, error) {
		c.Findings = fs
		return encodeContext(c)
	})
	if err != nil {
		return store.Hail{}, err
	}
	all := "answer file, fix or drop for all of them"
	if len(mediums) == 1 {
		all = "answer file, fix or drop"
	}
	q := fmt.Sprintf(`Triage %d medium %s from %s's review in round %d: %s, or a JSON object `+
		`{"fix": [..], "file": [..], "drop": [..]} of finding numbers; unnamed findings are filed.`,
		len(mediums), plural(len(mediums), "finding"), oneLine(c.Review), c.Round, all)
	return store.Hail{Category: regs.CategoryCampaignTriage, Question: q, Options: campaignTriageOptions, Context: raw}, nil
}

// campaignStalledHail builds the campaign_stalled hail for c, with
// blocking, the last review's blocking findings, numbered from 1 in the
// context (c.Blocking is replaced). Its options are land, retry and drop,
// or retry and drop when c has no head. Raise it under
// campaignStallKey(n) for the task's nth stall.
//
// The question: "A task of campaign C-0001 stalled in round 2 of 3 on
// ORD-0011 (no_verdict): land ORD-0010 (3f2a9c1e0b7d) as it is, retry or
// drop? An option may be followed by a note: retry: <note>." It leaves
// out the task's ref, as the triage question does, so a ref can't send a
// stall Command may answer to the captain; the context names it.
func campaignStalledHail(c CampaignStalledContext, blocking []comms.VerdictFinding) (store.Hail, error) {
	if err := c.check(); err != nil {
		return store.Hail{}, err
	}
	if !stallCodeRE.MatchString(c.Code) {
		return store.Hail{}, fmt.Errorf("stall code %q is not a word of lower-case letters and _", c.Code)
	}
	if c.Round < 1 || c.MaxRounds < 1 {
		return store.Hail{}, errors.New("a campaign_stalled hail needs its round and max_rounds")
	}
	if (c.Head == "") != (c.HeadCommit == "") || c.HeadCommit != "" && !commitIDRE.MatchString(c.HeadCommit) {
		return store.Hail{}, fmt.Errorf("head %q at %q is not an order and its full commit id", c.Head, c.HeadCommit)
	}
	c.CampaignHailTask = c.clean()
	reason := clip(oneLine(c.Reason), maxCampaignReason)
	raw, err := fitCampaignFindings(blocking, func(fs []CampaignFinding, tight bool) ([]byte, error) {
		c.Blocking, c.Reason = fs, reason
		if tight {
			c.Reason = clip(reason, maxCampaignTightReason)
		}
		return encodeContext(c)
	})
	if err != nil {
		return store.Hail{}, err
	}
	on := ""
	if c.Order != "" {
		on = " on " + oneLine(c.Order)
	}
	opts, ask := campaignHeadlessOptions, "retry or drop?"
	if c.Head != "" {
		opts = campaignLandOptions
		ask = fmt.Sprintf("land %s (%s) as it is, retry or drop?", oneLine(c.Head), shortCommit(c.HeadCommit))
	}
	q := fmt.Sprintf("A task of campaign %s stalled in round %d of %d%s (%s): %s An option may be followed by a note: retry: <note>.",
		oneLine(c.Campaign), c.Round, c.MaxRounds, on, c.Code, ask)
	return store.Hail{Category: regs.CategoryCampaignStalled, Question: q, Options: opts, Context: raw}, nil
}

// campaignLandHail builds the campaign_land hail for c. Commits keeps its
// first maxCampaignCommits entries, each subject made one line and cut to
// maxCampaignSubject; CommitCount, when lower, becomes the commits given.
// Raise it under campaignLandKey(n) for the task's nth land hail.
//
// The question: "Land o/r#12 of campaign C-0001: push ORD-0011
// (3f2a9c1e0b7d) to campaign/C-0001/o-r-12 on repo app, 4 commits over
// 8b1d0e2c4a55, passed review in round 2?" It names the ref and the
// branch, free text that could fit a captain_only rule, and that changes
// nothing: the category is fixed to the captain (regs.CaptainFixed).
func campaignLandHail(c CampaignLandContext) (store.Hail, error) {
	if err := c.check(); err != nil {
		return store.Hail{}, err
	}
	if c.Repo == "" || c.Branch == "" || c.Head == "" || c.Review == "" || c.Rounds < 1 {
		return store.Hail{}, errors.New("a campaign_land hail needs its repo, branch, head, review and rounds")
	}
	if !commitIDRE.MatchString(c.HeadCommit) || !commitIDRE.MatchString(c.ReviewBase) {
		return store.Hail{}, fmt.Errorf("head commit %q and review base %q must be full commit ids", c.HeadCommit, c.ReviewBase)
	}
	c.CampaignHailTask = c.clean()
	c.CommitCount = max(c.CommitCount, len(c.Commits))
	commits := make([]CampaignCommit, 0, min(len(c.Commits), maxCampaignCommits))
	for _, cm := range c.Commits[:min(len(c.Commits), maxCampaignCommits)] {
		commits = append(commits, CampaignCommit{SHA: cm.SHA, Subject: clip(oneLine(cm.Subject), maxCampaignSubject)})
	}
	c.Commits = commits
	c.ReviewSummary = clip(c.ReviewSummary, maxCampaignSummary)
	raw, err := encodeContext(c)
	if err != nil {
		return store.Hail{}, err
	}
	q := fmt.Sprintf("Land %s of campaign %s: push %s (%s) to %s on repo %s, %d %s over %s, passed review in round %d?",
		oneLine(c.Task), oneLine(c.Campaign), oneLine(c.Head), shortCommit(c.HeadCommit), oneLine(c.Branch), oneLine(c.Repo),
		c.CommitCount, plural(c.CommitCount, "commit"), shortCommit(c.ReviewBase), c.Rounds)
	return store.Hail{Category: regs.CategoryCampaignLand, Question: q, Options: campaignLandOptions, Context: raw}, nil
}

// plural is word, with an s unless n is 1.
func plural(n int, word string) string {
	if n == 1 {
		return word
	}
	return word + "s"
}

// encodeContext is v as JSON without HTML escaping, which would grow each
// <, > and & to 6 bytes.
func encodeContext(v any) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(b.Bytes(), []byte("\n")), nil
}

// fitCampaignFindings numbers fs from 1 and encodes the context build
// makes of them, within maxConsultContext so Command reads all of it:
// each body is cut to maxCampaignBody, halving the cap while the context
// is too long, and with empty bodies a tight pass (build's tight) cuts
// each title to maxCampaignTightTitle and each path to
// maxCampaignTightPath, and build cuts its own free text (a stall's
// reason). At most 20 findings of a verdict then always fit; what still
// doesn't is encoded as it is.
func fitCampaignFindings(fs []comms.VerdictFinding, build func(fs []CampaignFinding, tight bool) ([]byte, error)) ([]byte, error) {
	tight := false
	for cut := maxCampaignBody; ; cut /= 2 {
		out := make([]CampaignFinding, 0, len(fs))
		for i, f := range fs {
			cf := CampaignFinding{N: i + 1, Severity: f.Severity, Title: f.Title, Body: clip(f.Body, cut), Path: f.Path, Line: f.Line}
			if tight {
				cf.Title, cf.Path = clip(cf.Title, maxCampaignTightTitle), clip(cf.Path, maxCampaignTightPath)
			}
			out = append(out, cf)
		}
		raw, err := build(out, tight)
		if err != nil || len(raw) <= maxConsultContext || cut == 0 && tight {
			return raw, err
		}
		if cut == 0 {
			tight, cut = true, 1 // one more pass at cut 0
		}
	}
}

// raiseCampaignHail raises campaign hail h under key (raiseBridgeHail,
// entry its log entry) and returns its row once the decision is final
// (awaitCampaignDecision). A re-run with the same key finds the first
// hail.
func raiseCampaignHail(ctx dbos.Context, key string, h store.Hail, entry store.LogEntry) (store.Hail, error) {
	id, err := raiseBridgeHail(ctx, key, h, entry)
	if err != nil {
		return store.Hail{}, err
	}
	return awaitCampaignDecision(ctx, id)
}

// campaignHailRead is the check-campaign-hail step's recorded result: the
// hail's row and whether its countermand window had passed when it was
// read under the store's write lock.
type campaignHailRead struct {
	Hail  store.Hail
	Final bool
}

// awaitCampaignDecision waits for campaign hail id to close
// (awaitBridgeHail). A decision the captain may still countermand
// (Command's, or a captain timeout's default_option) is waited out in
// full (awaitCountermandWindow, uncapped: a campaign isn't bound to a
// tour), and the row is read again under the store's write lock with the
// time taken inside it (check-campaign-hail, store.GetHailLocked), so a
// countermand that took the lock before the deadline commits first. A
// read whose time isn't past countermand_until waits and reads again.
// The caller then acts on the final decision, a countermand included.
// The captain's own decision returns at once.
func awaitCampaignDecision(ctx dbos.Context, id string) (store.Hail, error) {
	h, err := awaitBridgeHail(ctx, id)
	if err != nil || h.Status != store.HailDecided || h.CountermandUntil == nil {
		return h, err
	}
	for {
		if err := awaitCountermandWindow(ctx, id, "campaign-window", true); err != nil {
			return store.Hail{}, err
		}
		r, err := dbos.RunAsStep(ctx, func(c context.Context) (campaignHailRead, error) {
			h, now, err := daemonFrom(c).store.GetHailLocked(c, id)
			return campaignHailRead{Hail: h, Final: h.CountermandUntil == nil || now.After(*h.CountermandUntil)}, err
		}, append([]dbos.StepOption{dbos.WithStepName("check-campaign-hail")}, storeRetries...)...)
		if err != nil || r.Final {
			return r.Hail, err
		}
	}
}

// campaignTriage is a campaign_triage decision: the finding numbers to
// fix, to file and to drop, each ascending. Every finding is in exactly
// one list.
type campaignTriage struct {
	Fix  []int `json:"fix"`
	File []int `json:"file"`
	Drop []int `json:"drop"`
}

// lists are t's lists in campaignTriageOptions' order.
func (t campaignTriage) lists() [][]int { return [][]int{t.File, t.Fix, t.Drop} }

// option is t's option: the index in campaignTriageOptions of its
// largest list. A tie goes to the option listed first: file, then fix,
// then drop.
func (t campaignTriage) option() int {
	best := 0
	for i, l := range t.lists() {
		if len(l) > len(t.lists()[best]) {
			best = i
		}
	}
	return best
}

// normal is t's normal form: the option's word alone when every finding
// is under it, else the JSON object with all three lists, fix, file and
// drop, each ascending.
func (t campaignTriage) normal() string {
	opt := t.option()
	alone := true
	for i, l := range t.lists() {
		if i != opt && len(l) > 0 {
			alone = false
		}
	}
	if alone {
		return campaignTriageOptions[opt]
	}
	raw, _ := json.Marshal(t) // lists of ints always encode
	return string(raw)
}

// unfence is s trimmed, with a ``` fence around it (```json, say) taken
// off.
func unfence(s string) string {
	s = strings.TrimSpace(s)
	if rest, ok := strings.CutPrefix(s, "```"); ok {
		_, body, _ := strings.Cut(rest, "\n")
		s = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(body), "```"))
	}
	return s
}

// findingNumberRE is a finding number as JSON writes it: no sign,
// fraction, exponent or leading zero.
var findingNumberRE = regexp.MustCompile(`^[1-9][0-9]*$`)

// parseCampaignTriage reads a campaign_triage decision for n findings,
// numbered from 1. An option word alone (file, fix or drop, in any case)
// puts every finding under it. Otherwise the decision is one JSON object
// (a ``` fence around it is taken off) with only the keys fix, file and
// drop, spelled so, each at most once, whose values are lists of finding
// numbers, each number named at most once in all. Unnamed findings are
// filed. The normal form it returns parses back to the same decision.
func parseCampaignTriage(decision string, n int) (campaignTriage, error) {
	s := unfence(decision)
	all := make([]int, n)
	for i := range all {
		all[i] = i + 1
	}
	switch strings.ToLower(s) {
	case campaignFile:
		return campaignTriage{Fix: []int{}, File: all, Drop: []int{}}, nil
	case campaignFix:
		return campaignTriage{Fix: all, File: []int{}, Drop: []int{}}, nil
	case campaignDrop:
		return campaignTriage{Fix: []int{}, File: []int{}, Drop: all}, nil
	}
	if !strings.HasPrefix(s, "{") {
		return campaignTriage{}, errors.New("not file, fix, drop or a JSON object")
	}
	lists, err := decodeTriageLists(s)
	if err != nil {
		return campaignTriage{}, err
	}
	where := map[int]string{}
	t := campaignTriage{Fix: []int{}, File: []int{}, Drop: []int{}}
	for _, key := range []string{campaignFix, campaignFile, campaignDrop} {
		for _, raw := range lists[key] {
			k := string(bytes.TrimSpace(raw))
			num, err := strconv.Atoi(k)
			if !findingNumberRE.MatchString(k) || err != nil || num > n {
				return campaignTriage{}, fmt.Errorf("%s: %s is not a finding number from 1 to %d", key, k, n)
			}
			if w, dup := where[num]; dup {
				return campaignTriage{}, fmt.Errorf("%d named twice (%s and %s)", num, w, key)
			}
			where[num] = key
			switch key {
			case campaignFix:
				t.Fix = append(t.Fix, num)
			case campaignFile:
				t.File = append(t.File, num)
			default:
				t.Drop = append(t.Drop, num)
			}
		}
	}
	for _, num := range all {
		if _, named := where[num]; !named {
			t.File = append(t.File, num)
		}
	}
	slices.Sort(t.Fix)
	slices.Sort(t.File)
	slices.Sort(t.Drop)
	return t, nil
}

// decodeTriageLists decodes s, one JSON object of the keys fix, file and
// drop, each at most once, spelled so, and each a list, into each key's
// raw elements. Go's own decoding would match keys in any case and let a
// repeated key replace the first.
func decodeTriageLists(s string) (map[string][]json.RawMessage, error) {
	dec := json.NewDecoder(strings.NewReader(s))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return nil, errors.New("not a JSON object")
	}
	lists := map[string][]json.RawMessage{}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		key, _ := tok.(string)
		if !slices.Contains(campaignTriageOptions, key) {
			return nil, fmt.Errorf("%q is not fix, file or drop", key)
		}
		if _, dup := lists[key]; dup {
			return nil, fmt.Errorf("%s named twice", key)
		}
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return nil, err
		}
		var elems []json.RawMessage
		if !bytes.HasPrefix(bytes.TrimSpace(raw), []byte("[")) || json.Unmarshal(raw, &elems) != nil {
			return nil, fmt.Errorf("%s is not a list of finding numbers", key)
		}
		lists[key] = append([]json.RawMessage{}, elems...)
	}
	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("more than one JSON value")
	}
	return lists, nil
}

// campaignChoice is a campaign_stalled or campaign_land decision: its
// option and the note that may follow it.
type campaignChoice struct {
	Option string
	Note   string
}

// normal is c's normal form: "retry", or "retry: <note>".
func (c campaignChoice) normal() string {
	if c.Note == "" {
		return c.Option
	}
	return c.Option + ": " + c.Note
}

// parseCampaignChoice reads a campaign_stalled or campaign_land decision
// against the hail's options: an option word, in any case, optionally
// followed by a colon and a note, "retry: the flaky test is real". The
// decision is one line: no control character, format character
// (unicode.Cf: bidi overrides, zero-width spaces), line or paragraph
// separator, in valid UTF-8. The note, trimmed, is at most
// maxCampaignNote bytes; an empty one is no note.
func parseCampaignChoice(decision string, options []string) (campaignChoice, error) {
	if !utf8.ValidString(decision) {
		return campaignChoice{}, errors.New("not valid UTF-8")
	}
	notLine := func(r rune) bool {
		return unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || r == '\u2028' || r == '\u2029'
	}
	if i := strings.IndexFunc(decision, notLine); i >= 0 {
		r, _ := utf8.DecodeRuneInString(decision[i:])
		return campaignChoice{}, fmt.Errorf("a decision is one line, with no control or format character (%q at byte %d)", r, i)
	}
	word, note, _ := strings.Cut(strings.TrimSpace(decision), ":")
	word, note = strings.ToLower(strings.TrimSpace(word)), strings.TrimSpace(note)
	if !slices.Contains(options, word) {
		return campaignChoice{}, fmt.Errorf("%.40q is not %s", word, orList(options))
	}
	if len(note) > maxCampaignNote {
		return campaignChoice{}, fmt.Errorf("the note is %d bytes, over %d", len(note), maxCampaignNote)
	}
	return campaignChoice{Option: word, Note: note}, nil
}

// orList is words as "a, b or c".
func orList(words []string) string {
	if len(words) < 2 {
		return strings.Join(words, "")
	}
	return strings.Join(words[:len(words)-1], ", ") + " or " + words[len(words)-1]
}

// checkCampaignAnswer checks a decision on campaign hail h against the
// hail's context and options (AnswerHail for the captain, CountermandHail,
// and decideCommand beside regs.CheckAnswer for Command): decision, or
// option's text when decision is empty. It returns the decision in normal
// form and its option.
//
//   - campaign_triage: parseCampaignTriage over the context's findings,
//     stored as campaignTriage.normal, the option of its largest list.
//   - campaign_stalled and campaign_land: parseCampaignChoice over the
//     hail's options, stored as campaignChoice.normal.
//
// An option given with a decision that doesn't match it, or a decision
// that doesn't parse, is a bad request saying what the hail takes.
func checkCampaignAnswer(h store.Hail, decision string, option *int) (string, *int, error) {
	if option != nil && (*option < 0 || *option >= len(h.Options)) {
		return "", nil, badRequest("hail %s: option %d out of range (%d options)", h.ID, *option, len(h.Options))
	}
	if decision == "" && option != nil {
		decision, option = h.Options[*option], nil
	}
	var (
		normal string
		opt    int
	)
	switch h.Category {
	case regs.CategoryCampaignTriage:
		var c CampaignTriageContext
		if err := json.Unmarshal(h.Context, &c); err != nil {
			return "", nil, fmt.Errorf("hail %s: campaign_triage context: %w", h.ID, err)
		}
		t, err := parseCampaignTriage(decision, len(c.Findings))
		if err != nil {
			return "", nil, badRequest(`hail %s: %s; answer file, fix or drop, or a JSON object {"fix": [..], "file": [..], "drop": [..]} `+
				"of finding numbers from 1 to %d, each named once", h.ID, err, len(c.Findings))
		}
		normal, opt = t.normal(), t.option()
	case regs.CategoryCampaignStalled, regs.CategoryCampaignLand:
		c, err := parseCampaignChoice(decision, h.Options)
		if err != nil {
			return "", nil, badRequest("hail %s: %s; answer %s, optionally followed by a one-line note (retry: <note>)",
				h.ID, err, orList(h.Options))
		}
		normal, opt = c.normal(), slices.Index(h.Options, c.Option)
	default:
		return "", nil, fmt.Errorf("hail %s: %s is not a campaign hail", h.ID, h.Category)
	}
	if option != nil && *option != opt {
		return "", nil, badRequest("hail %s: option %d (%s) does not match the decision %.80q", h.ID, *option,
			h.Options[*option], normal)
	}
	return normal, &opt, nil
}
