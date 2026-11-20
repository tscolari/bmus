package bridge

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/tscolari/bmus/internal/store"
	"github.com/tscolari/bmus/internal/tracker"
)

// The campaign RPCs (#186, spec/campaigns-stage-3-plan-2026-11-16.md
// section 9): campaign.run starts a campaign on one tracker issue,
// campaign.list and campaign.show read them, and campaign.belay stops
// one.

// Campaign run defaults and limits.
const (
	defaultCampaignBase           = "main"
	defaultCampaignReviewDivision = "science"
	defaultCampaignMaxRounds      = 3
	maxCampaignRounds             = 10
	// campaignLandPush is the only land mode of stage 3; pr and merge
	// come with stage 5.
	campaignLandPush = "push"
	// campaignBelayedWhy is the rationale of a hail campaign.belay closes.
	campaignBelayedWhy = "campaign belayed"
)

// Campaign args and results.
type (
	// CampaignRunArgs are campaign.run's args. Issue is a tracker ref and
	// Repo a registered repo, both required. Base defaults to main,
	// ReviewDivision to science, MaxRounds (absent) to 3 and Land to
	// push.
	// Division or Assignee, or both, says where round 1's work goes.
	// Branch, when set, is where the task lands, instead of
	// campaign/<id>/<slug>. It returns the campaign as a CampaignView.
	CampaignRunArgs struct {
		Issue          string `json:"issue"`
		Repo           string `json:"repo"`
		Base           string `json:"base,omitempty"`
		Division       string `json:"division,omitempty"`
		Assignee       string `json:"assignee,omitempty"`
		ReviewDivision string `json:"review_division,omitempty"`
		MaxRounds      *int   `json:"max_rounds,omitempty"`
		Land           string `json:"land,omitempty"`
		Branch         string `json:"branch,omitempty"`
	}
	// CampaignIDArgs are campaign.show's args.
	CampaignIDArgs struct {
		ID string `json:"id"`
	}
	// CampaignBelayArgs are campaign.belay's args: the campaign and an
	// optional reason for the captain's log.
	CampaignBelayArgs struct {
		ID     string `json:"id"`
		Reason string `json:"reason,omitempty"`
	}
	// CampaignView is a campaign and its tasks.
	CampaignView struct {
		ID             string             `json:"id"`
		State          string             `json:"state"`
		Land           string             `json:"land"`
		Repo           string             `json:"repo"`
		BaseRef        string             `json:"base_ref"`
		Division       string             `json:"division"`
		Assignee       string             `json:"assignee"`
		ReviewDivision string             `json:"review_division"`
		MaxRounds      int                `json:"max_rounds"`
		CreatedAt      time.Time          `json:"created_at"`
		EndedAt        *time.Time         `json:"ended_at,omitempty"`
		Tasks          []CampaignTaskView `json:"tasks"`
	}
	// CampaignTaskView is a campaign's task: its tracker ref, state,
	// round of MaxRounds, land branch, head and commit, open order and
	// hail, and why it stalled or dropped.
	CampaignTaskView struct {
		Ref         string     `json:"ref"`
		WorkflowID  string     `json:"workflow_id"`
		State       string     `json:"state"`
		Round       int        `json:"round"`
		MaxRounds   int        `json:"max_rounds"`
		Branch      string     `json:"branch"`
		Implementer string     `json:"implementer"`
		Head        string     `json:"head"`
		HeadCommit  string     `json:"head_commit"`
		ReviewBase  string     `json:"review_base"`
		OpenOrder   string     `json:"open_order"`
		OpenHail    string     `json:"open_hail"`
		Why         string     `json:"why"`
		LandedAt    *time.Time `json:"landed_at,omitempty"`
	}
	// CampaignListResult is campaign.list's result, by id.
	CampaignListResult struct {
		Campaigns []CampaignView `json:"campaigns"`
	}
	// CampaignShowResult is campaign.show's result: the campaign and its
	// tasks' orders, in creation order.
	CampaignShowResult struct {
		Campaign CampaignView `json:"campaign"`
		Orders   []OrderView  `json:"orders"`
	}
	// CampaignBelayResult is campaign.belay's result: the campaign
	// afterwards, the hails the call closed and the orders it asked to
	// end. Both lists are always present.
	CampaignBelayResult struct {
		Campaign CampaignView         `json:"campaign"`
		Hails    []string             `json:"hails"`
		Orders   []CampaignOrderEnded `json:"orders"`
	}
	// CampaignOrderEnded is an order campaign.belay asked to end, and how:
	// "withdrawn" or "belay requested" (its mission settles it).
	CampaignOrderEnded struct {
		ID     string `json:"id"`
		Action string `json:"action"`
	}
)

func rpcCampaignRun(ctx context.Context, d *Daemon, raw json.RawMessage) (any, error) {
	var a CampaignRunArgs
	if err := decodeArgs(raw, &a); err != nil {
		return nil, err
	}
	return d.runCampaign(ctx, a)
}

func rpcCampaignList(ctx context.Context, d *Daemon, raw json.RawMessage) (any, error) {
	if err := decodeArgs(raw, &struct{}{}); err != nil {
		return nil, err
	}
	cs, err := d.store.ListCampaigns(ctx)
	if err != nil {
		return nil, err
	}
	res := CampaignListResult{Campaigns: []CampaignView{}}
	for _, c := range cs {
		v, err := d.campaignView(ctx, c)
		if err != nil {
			return nil, err
		}
		res.Campaigns = append(res.Campaigns, v)
	}
	return res, nil
}

func rpcCampaignShow(ctx context.Context, d *Daemon, raw json.RawMessage) (any, error) {
	var a CampaignIDArgs
	if err := decodeArgs(raw, &a); err != nil {
		return nil, err
	}
	return d.showCampaign(ctx, a.ID)
}

func rpcCampaignBelay(ctx context.Context, d *Daemon, raw json.RawMessage) (any, error) {
	var a CampaignBelayArgs
	if err := decodeArgs(raw, &a); err != nil {
		return nil, err
	}
	return d.belayCampaign(ctx, a)
}

// runCampaign starts the campaign a describes (section 9). Every refusal
// comes before anything is written: no tracker; a ref the tracker's Canon
// refuses; an issue Get can't find, or one closed; an issue in another
// campaign's open task; a repo or base CreateOrder would refuse
// (checkOrderRepos, checkOrderBases); neither a division nor an assignee,
// an unknown division, or an assignee CreateOrder would refuse; a review
// division the roster lacks or with no crew member who isn't dismissed;
// max_rounds outside 1 to 10; land pr or merge, or any other mode; a
// branch git refuses (checkBranchName, or campaignBranch for the
// default); a branch that already exists on the repo's remote, or that
// another campaign's open task on the repo lands on (checked in the
// insert's transaction). Then the
// campaign and its task are inserted in one transaction
// (store.InsertCampaign) and campaign/<id> starts. A start that fails
// leaves the campaign running, and the bridge's next startup starts it
// (attachCampaigns).
func (d *Daemon) runCampaign(ctx context.Context, a CampaignRunArgs) (CampaignView, error) {
	issue := strings.TrimSpace(a.Issue)
	if issue == "" {
		return CampaignView{}, badRequest("an issue is required: a tracker ref, such as #12")
	}
	if a.Repo == "" {
		return CampaignView{}, badRequest("a repo is required (repos: %s)", d.repoNames())
	}
	if a.Division == "" && a.Assignee == "" {
		return CampaignView{}, badRequest("a division or an assignee is required: where round 1's work goes")
	}
	land := a.Land
	switch land {
	case "":
		land = campaignLandPush
	case campaignLandPush:
	case "pr", "merge":
		return CampaignView{}, badRequest("land pr and merge come with stage 5")
	default:
		return CampaignView{}, badRequest("unknown land %q: push is the only one", land)
	}
	maxRounds := defaultCampaignMaxRounds
	if a.MaxRounds != nil {
		maxRounds = *a.MaxRounds
	}
	if maxRounds < 1 || maxRounds > maxCampaignRounds {
		return CampaignView{}, badRequest("max_rounds %d out of range (1 to %d)", maxRounds, maxCampaignRounds)
	}
	base := a.Base
	if base == "" {
		base = defaultCampaignBase
	}
	reviewDivision := a.ReviewDivision
	if reviewDivision == "" {
		reviewDivision = defaultCampaignReviewDivision
	}

	// The issue.
	if d.tracker == nil {
		return CampaignView{}, badRequest("campaign run: %s", whyNoTracker)
	}
	ref, err := d.tracker.Canon(issue)
	if err != nil {
		return CampaignView{}, badRequest("issue %s: %v", issue, err)
	}
	tctx, cancel := context.WithTimeout(ctx, sourceTimeout)
	it, err := d.tracker.Get(tctx, ref)
	cancel()
	switch {
	case errors.Is(err, tracker.ErrBadRef):
		return CampaignView{}, badRequest("issue %s: %v", ref, err)
	case errors.Is(err, tracker.ErrNotFound):
		return CampaignView{}, fmt.Errorf("issue %s: %v: %w", ref, err, ErrNotFound)
	case err != nil:
		return CampaignView{}, fmt.Errorf("issue %s: %w", ref, err)
	}
	if it.Ref != "" {
		ref = it.Ref
	}
	if it.State == tracker.StateClosed {
		return CampaignView{}, badRequest("issue %s is closed", ref)
	}
	other, err := d.store.OpenTaskOf(ctx, ref)
	if err != nil {
		return CampaignView{}, err
	}
	if other != "" {
		return CampaignView{}, badRequest("%v", &store.OpenTaskError{Ref: ref, Campaign: other})
	}

	// Where the work goes and starts.
	repos := []store.OrderRepo{{Name: a.Repo, BaseRef: base}}
	if err := d.checkOrderRepos(repos); err != nil {
		return CampaignView{}, err
	}
	if _, err := d.checkOrderBases(ctx, repos); err != nil {
		return CampaignView{}, err
	}
	if err := d.checkDivision(a.Division); err != nil {
		return CampaignView{}, err
	}
	if a.Assignee != "" {
		c, err := d.store.GetCrew(ctx, a.Assignee)
		if errors.Is(err, sql.ErrNoRows) {
			return CampaignView{}, fmt.Errorf("assignee %s: %w", a.Assignee, ErrNotFound)
		}
		if err != nil {
			return CampaignView{}, err
		}
		if c.Status == store.CrewSickbay || c.Status == store.CrewDismissed {
			return CampaignView{}, fmt.Errorf("assignee %s is %s: %w", c.ID, c.Status, ErrUnavailable)
		}
	}
	if err := d.checkDivision(reviewDivision); err != nil {
		return CampaignView{}, err
	}
	crew, err := d.store.ListCrew(ctx)
	if err != nil {
		return CampaignView{}, err
	}
	reviewers := 0
	for _, c := range crew {
		if c.Division == reviewDivision && c.Status != store.CrewDismissed {
			reviewers++
		}
	}
	if reviewers == 0 {
		return CampaignView{}, badRequest("review division %s has no crew member to review the work", reviewDivision)
	}

	// The id, and the branch it names. One run at a time, so the id read
	// here is the one the insert takes.
	d.campaignMu.Lock()
	defer d.campaignMu.Unlock()
	id, err := d.store.NextCampaignID(ctx)
	if err != nil {
		return CampaignView{}, err
	}
	branch := a.Branch
	if branch == "" {
		if branch, err = campaignBranch(ctx, id, ref); err != nil {
			return CampaignView{}, err
		}
	} else if err := checkBranchName(ctx, branch); err != nil {
		return CampaignView{}, err
	}
	at, err := d.remoteBranch(ctx, a.Repo, branch)
	if err != nil {
		return CampaignView{}, fmt.Errorf("read branch %s on repo %s's remote: %w", branch, a.Repo, err)
	}
	if at != "" {
		return CampaignView{}, badRequest("branch %s already exists on repo %s's remote, at %s; pick another with --branch",
			branch, a.Repo, shortCommit(at))
	}

	c := store.Campaign{ID: id, Land: land, Repo: a.Repo, BaseRef: base, Division: a.Division, Assignee: a.Assignee,
		ReviewDivision: reviewDivision, MaxRounds: maxRounds}
	t := store.CampaignTask{Ref: ref, MaxRounds: maxRounds, Branch: branch}
	entry, err := logEntry(store.LogKindCampaign, "", "", "campaign "+id+" started: "+CampaignRunText(c, t),
		map[string]any{"repo": a.Repo, "base_ref": base, "division": a.Division, "assignee": a.Assignee,
			"review_division": reviewDivision, "max_rounds": maxRounds, "land": land, "branch": branch})
	if err != nil {
		return CampaignView{}, err
	}
	err = d.store.InsertCampaign(ctx, c, t, entry)
	var (
		ote *store.OpenTaskError
		bte *store.BranchTakenError
	)
	switch {
	case errors.As(err, &ote), errors.As(err, &bte):
		return CampaignView{}, badRequest("%v", err)
	case err != nil:
		return CampaignView{}, fmt.Errorf("insert campaign %s: %w", id, err)
	}
	if err := d.startCampaign(id); err != nil {
		return CampaignView{}, fmt.Errorf("campaign %s was created but its workflow didn't start; the bridge starts it "+
			"at its next startup: %w", id, err)
	}
	cp, err := d.store.GetCampaign(ctx, id)
	if err != nil {
		return CampaignView{}, err
	}
	return d.campaignView(ctx, cp)
}

// CampaignRunText describes campaign c with task t, as campaign.run's
// log entry and `bmus campaign run` print it: "o/r#12 on app from main:
// work for sorell, reviews in science, up to 3 rounds, landing by push to
// campaign/C-0001/o-r-12". Work goes "for" an assignee, else "in" a
// division's pool.
func CampaignRunText(c store.Campaign, t store.CampaignTask) string {
	work := "work for " + c.Assignee
	if c.Assignee == "" {
		work = "work in " + c.Division
	}
	rounds := fmt.Sprintf("up to %d rounds", c.MaxRounds)
	if c.MaxRounds == 1 {
		rounds = "1 round"
	}
	return fmt.Sprintf("%s on %s from %s: %s, reviews in %s, %s, landing by %s to %s", t.Ref, c.Repo, c.BaseRef, work,
		c.ReviewDivision, rounds, c.Land, t.Branch)
}

// campaignView is campaign c with its tasks.
func (d *Daemon) campaignView(ctx context.Context, c store.Campaign) (CampaignView, error) {
	ts, err := d.store.CampaignTasks(ctx, c.ID)
	if err != nil {
		return CampaignView{}, err
	}
	v := CampaignView{ID: c.ID, State: c.State, Land: c.Land, Repo: c.Repo, BaseRef: c.BaseRef, Division: c.Division,
		Assignee: c.Assignee, ReviewDivision: c.ReviewDivision, MaxRounds: c.MaxRounds, CreatedAt: c.CreatedAt.UTC(),
		EndedAt: utc(c.EndedAt), Tasks: []CampaignTaskView{}}
	for _, t := range ts {
		v.Tasks = append(v.Tasks, CampaignTaskView{Ref: t.Ref, WorkflowID: t.WorkflowID, State: t.State, Round: t.Round,
			MaxRounds: t.MaxRounds, Branch: t.Branch, Implementer: t.Implementer, Head: t.Head, HeadCommit: t.HeadCommit,
			ReviewBase: t.ReviewBase, OpenOrder: t.OpenOrder, OpenHail: t.OpenHail, Why: t.Why, LandedAt: utc(t.LandedAt)})
	}
	return v, nil
}

// getCampaign reads campaign id, ErrNotFound when there is none.
func (d *Daemon) getCampaign(ctx context.Context, id string) (store.Campaign, error) {
	if id == "" {
		return store.Campaign{}, badRequest("campaign id is required")
	}
	c, err := d.store.GetCampaign(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return store.Campaign{}, fmt.Errorf("campaign %s: %w", id, ErrNotFound)
	}
	return c, err
}

// showCampaign is campaign.show: campaign id, its tasks and their orders.
func (d *Daemon) showCampaign(ctx context.Context, id string) (CampaignShowResult, error) {
	c, err := d.getCampaign(ctx, id)
	if err != nil {
		return CampaignShowResult{}, err
	}
	v, err := d.campaignView(ctx, c)
	if err != nil {
		return CampaignShowResult{}, err
	}
	res := CampaignShowResult{Campaign: v, Orders: []OrderView{}}
	for _, t := range v.Tasks {
		ids, err := d.store.TaskOrderIDs(ctx, t.WorkflowID, "")
		if err != nil {
			return CampaignShowResult{}, err
		}
		for _, oid := range ids {
			o, err := d.store.GetOrder(ctx, oid)
			if err != nil {
				return CampaignShowResult{}, err
			}
			res.Orders = append(res.Orders, d.orderView(o))
		}
	}
	return res, nil
}

// belayCampaign is campaign.belay (section 9, decision 4). It sets the
// campaign belaying first (store.BelayCampaign), so that each task reads
// the close of a hail or the end of an order that follows as the belay's
// and drops itself with no stall hail. Then, for each task that hasn't
// landed or been dropped, each in a transaction of its own: every open
// hail the task raised is closed timed out with "campaign belayed" as its
// rationale (store.TimeOutOpenHailWhy, waking the task and the hail's
// workflow with notifyHailClosed), and every open order of the task is
// withdrawn when unclaimed or belayed otherwise (endTaskOrderHow). The
// task's row names one open hail and order; the call looks at all the
// task raised and created, which also covers one the task raised but had
// not yet written to its row. A campaign already belaying is swept again
// and logs nothing new for itself; one that ended is a bad request.
func (d *Daemon) belayCampaign(ctx context.Context, a CampaignBelayArgs) (CampaignBelayResult, error) {
	c, err := d.getCampaign(ctx, a.ID)
	if err != nil {
		return CampaignBelayResult{}, err
	}
	reason := strings.TrimSpace(oneLine(a.Reason))
	text := "campaign " + c.ID + " belayed by the captain"
	if reason != "" {
		text += ": " + reason
	}
	entry, err := logEntry(store.LogKindCampaign, "", "", text, map[string]string{"reason": reason})
	if err != nil {
		return CampaignBelayResult{}, err
	}
	if _, err := d.store.BelayCampaign(ctx, c.ID, entry); err != nil {
		if errors.Is(err, store.ErrCampaignEnded) {
			return CampaignBelayResult{}, badRequest("campaign %s has ended", c.ID)
		}
		return CampaignBelayResult{}, err
	}
	res := CampaignBelayResult{Hails: []string{}, Orders: []CampaignOrderEnded{}}
	ts, err := d.store.CampaignTasks(ctx, c.ID)
	if err != nil {
		return CampaignBelayResult{}, err
	}
	why := "campaign " + c.ID + " belayed"
	if reason != "" {
		why += ": " + reason
	}
	for _, t := range ts {
		if t.State == store.TaskLanded || t.State == store.TaskDropped {
			continue
		}
		hails, err := d.store.ListHails(ctx, store.HailOpen)
		if err != nil {
			return CampaignBelayResult{}, err
		}
		for _, h := range hails {
			if h.DeliverTo != t.WorkflowID {
				continue
			}
			he, err := logEntry(logHail, "", "", "hail "+h.ID+" closed: "+why, map[string]string{"hail_id": h.ID,
				"campaign": c.ID})
			if err != nil {
				return CampaignBelayResult{}, err
			}
			closed, err := d.store.TimeOutOpenHailWhy(ctx, h.ID, campaignBelayedWhy, he,
				func(tx *sql.Tx) error { return d.notifyHailClosed(tx, h.ID) })
			if err != nil {
				return CampaignBelayResult{}, fmt.Errorf("close hail %s: %w", h.ID, err)
			}
			if closed {
				res.Hails = append(res.Hails, h.ID)
			}
		}
		ids, err := d.store.TaskOrderIDs(ctx, t.WorkflowID, "")
		if err != nil {
			return CampaignBelayResult{}, err
		}
		for _, oid := range ids {
			how, err := d.endTaskOrderHow(ctx, oid, why)
			if err != nil {
				return CampaignBelayResult{}, fmt.Errorf("end order %s: %w", oid, err)
			}
			if how != "" {
				res.Orders = append(res.Orders, CampaignOrderEnded{ID: oid, Action: how})
			}
		}
	}
	if c, err = d.store.GetCampaign(ctx, c.ID); err != nil {
		return CampaignBelayResult{}, err
	}
	if res.Campaign, err = d.campaignView(ctx, c); err != nil {
		return CampaignBelayResult{}, err
	}
	return res, nil
}
