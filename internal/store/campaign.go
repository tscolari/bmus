package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Task states (campaign_tasks.state, #180).
const (
	TaskWorking   = "working"
	TaskReviewing = "reviewing"
	TaskTriage    = "triage"
	TaskFixing    = "fixing"
	TaskLanding   = "landing"
	TaskStalled   = "stalled"
	TaskLanded    = "landed"
	TaskDropped   = "dropped"
)

// LogKindCampaign is the captain's log kind of a task's entries (SetTask).
const LogKindCampaign = "campaign"

// TaskOrder is an order of a task that ended or split, as settleFamily
// hands it to the task hook: Task is the order's task, State the state it
// is in.
type TaskOrder struct {
	Task  string
	Order string
	State string
}

// TaskHook is told, in tx, of each order with a task that ended or split
// (#180): the bridge sends the order's task workflow a wake in the same
// transaction (dbos.WithSendTransaction). An error rolls back the change
// that ended the order. It runs holding the store's write slot: it must
// not call a Store write method (it would wait forever).
type TaskHook func(tx *sql.Tx, o TaskOrder) error

// SetTaskHook makes every transaction that ends or splits an order with a
// task call fn (settleFamily), nil for none. Set it before the store's
// writers start.
func (s *Store) SetTaskHook(fn TaskHook) {
	if fn == nil {
		s.taskHook.Store(nil)
		return
	}
	s.taskHook.Store(&fn)
}

// CampaignTask is a row of the campaign_tasks table (#180): one tracker
// item a campaign takes to a pushed branch.
type CampaignTask struct {
	CampaignID string
	Ref        string
	WorkflowID string
	State      string
	Round      int
	MaxRounds  int
	Branch     string
	// Implementer is round 1's assignee; Head the latest done work order
	// and HeadCommit its commit; ReviewBase the commit round 1 started
	// from.
	Implementer string
	Head        string
	HeadCommit  string
	ReviewBase  string
	OpenOrder   string
	OpenHail    string
	// Why is the stall's reason, or the land's error.
	Why      string
	LandedAt *time.Time
}

const taskCols = `campaign_id, ref, workflow_id, state, round, max_rounds, branch, implementer, head, head_commit, review_base,
	open_order, open_hail, why, landed_at`

func scanTask(row interface{ Scan(...any) error }) (CampaignTask, error) {
	var (
		t      CampaignTask
		landed sql.NullTime
	)
	err := row.Scan(&t.CampaignID, &t.Ref, &t.WorkflowID, &t.State, &t.Round, &t.MaxRounds, &t.Branch, &t.Implementer, &t.Head,
		&t.HeadCommit, &t.ReviewBase, &t.OpenOrder, &t.OpenHail, &t.Why, &landed)
	if landed.Valid {
		t.LandedAt = &landed.Time
	}
	return t, err
}

// GetTask returns campaign campaignID's task for ref, or sql.ErrNoRows.
func (s *Store) GetTask(ctx context.Context, campaignID, ref string) (CampaignTask, error) {
	return scanTask(s.db.QueryRowContext(ctx, `SELECT `+taskCols+` FROM campaign_tasks WHERE campaign_id = ? AND ref = ?`,
		campaignID, ref))
}

// sameTask reports whether a and b hold the same values in the columns
// SetTask writes.
func sameTask(a, b CampaignTask) bool {
	landed := (a.LandedAt == nil) == (b.LandedAt == nil) && (a.LandedAt == nil || a.LandedAt.Equal(*b.LandedAt))
	a.LandedAt, b.LandedAt = nil, nil
	a.WorkflowID, b.WorkflowID = "", ""
	return landed && a == b
}

// SetTask writes task t's row, found by its CampaignID and Ref, and
// appends entry, in one transaction (#180): every column but the
// workflow id takes t's value. entry is logged with kind LogKindCampaign
// and its payload gains campaign, ref and round, as written;
// entry.OrderID names the order concerned, if any. An entry with no text
// is not logged (#184: the stall's hail id joins the row after the stall
// was logged). A row that already
// holds t's values is left alone and nothing is logged, so a re-run of
// the step that moved the task writes nothing. It reports whether it
// wrote. A missing task returns sql.ErrNoRows. The task workflow must be
// the row's only writer: a second writer's changes would be overwritten.
func (s *Store) SetTask(ctx context.Context, t CampaignTask, entry LogEntry) (wrote bool, err error) {
	err = s.withTx(ctx, func(tx *sql.Tx) error {
		cur, err := scanTask(tx.QueryRowContext(ctx, `SELECT `+taskCols+` FROM campaign_tasks WHERE campaign_id = ? AND ref = ?`,
			t.CampaignID, t.Ref))
		if err != nil {
			return err
		}
		if sameTask(cur, t) {
			return nil
		}
		var landed any
		if t.LandedAt != nil {
			landed = t.LandedAt.UTC()
		}
		if _, err := tx.ExecContext(ctx, `UPDATE campaign_tasks SET state = ?, round = ?, max_rounds = ?, branch = ?, implementer = ?,
			head = ?, head_commit = ?, review_base = ?, open_order = ?, open_hail = ?, why = ?, landed_at = ?
			WHERE campaign_id = ? AND ref = ?`,
			t.State, t.Round, t.MaxRounds, t.Branch, t.Implementer, t.Head, t.HeadCommit, t.ReviewBase, t.OpenOrder, t.OpenHail,
			t.Why, landed, t.CampaignID, t.Ref); err != nil {
			return err
		}
		wrote = true
		if entry.Text == "" {
			return nil
		}
		entry.Kind = LogKindCampaign
		for _, f := range []struct {
			key   string
			value any
		}{{"campaign", t.CampaignID}, {"ref", t.Ref}, {"round", t.Round}} {
			if entry.Payload, err = withField(entry.Payload, f.key, f.value); err != nil {
				return err
			}
		}
		_, err = s.appendLog(ctx, tx, entry)
		return err
	})
	return wrote, err
}

// EligibleReviewers returns the crew members who may review task now, in
// id order (#180): those of division neither dismissed nor in sickbay
// who never claimed a work order in the task (task_workers). Being busy,
// or standing orders being off, doesn't count against one. An empty
// division has none.
func (s *Store) EligibleReviewers(ctx context.Context, task, division string) ([]string, error) {
	if division == "" {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM crew c WHERE division = ? AND status NOT IN (?, ?)
		AND NOT EXISTS (SELECT 1 FROM task_workers w WHERE w.task = ? AND w.crew_id = c.id) ORDER BY id`,
		division, CrewDismissed, CrewSickbay, task)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// TaskWorkflowID is the id of campaign campaignID's task workflow for
// tracker ref ref, campaign/<id>/task/<ref>: an order's Task and the
// task's workflow_id.
func TaskWorkflowID(campaignID, ref string) string {
	return "campaign/" + campaignID + "/task/" + ref
}

// ParseTaskID splits a task workflow id (TaskWorkflowID) into its
// campaign and ref. ok is false for an id of another shape.
func ParseTaskID(task string) (campaignID, ref string, ok bool) {
	rest, ok := strings.CutPrefix(task, "campaign/")
	if !ok {
		return "", "", false
	}
	campaignID, ref, ok = strings.Cut(rest, "/task/")
	if !ok || campaignID == "" || ref == "" {
		return "", "", false
	}
	return campaignID, ref, true
}

// CampaignOrderText names order o of a campaign task for a refusal (#182):
// "ORD-0005 is campaign C-0001's work on o/r#12", or "review of" for a
// review. A task id of another shape is named whole.
func CampaignOrderText(o Order) string {
	what := "work on"
	if o.Kind == OrderKindReview {
		what = "review of"
	}
	campaignID, ref, ok := ParseTaskID(o.Task)
	if !ok {
		return fmt.Sprintf("%s is the %s task %s", o.ID, what, o.Task)
	}
	return fmt.Sprintf("%s is campaign %s's %s %s", o.ID, campaignID, what, ref)
}

// ErrReviewBarred is returned, wrapped in a ReviewBarredError, by
// RouteOrder for a review routed to a crew member who worked on its task
// (#182).
var ErrReviewBarred = errors.New("review barred")

// ReviewBarredError is RouteOrder's refusal of review Order to Crew, who
// claimed a work order in its task (task_workers). Ref is the task's
// tracker ref.
type ReviewBarredError struct {
	Order, Ref, Crew string
}

func (e *ReviewBarredError) Error() string {
	return fmt.Sprintf("%s reviews task %s, which %s worked on", e.Order, e.Ref, e.Crew)
}

// Is makes errors.Is(err, ErrReviewBarred) true.
func (e *ReviewBarredError) Is(target error) bool { return target == ErrReviewBarred }

// reviewBarred returns a ReviewBarredError when order o is a review and
// crewID is in its task's task_workers, read in tx; nil otherwise. The
// ref is the task's row's, else the one its id names, else the id.
func reviewBarred(ctx context.Context, tx *sql.Tx, o Order, crewID string) error {
	if o.Kind != OrderKindReview || crewID == "" {
		return nil
	}
	var worked int
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM task_workers WHERE task = ? AND crew_id = ?)`,
		o.Task, crewID).Scan(&worked); err != nil || worked == 0 {
		return err
	}
	ref := o.Task
	if _, r, ok := ParseTaskID(o.Task); ok {
		ref = r
	}
	err := tx.QueryRowContext(ctx, `SELECT ref FROM campaign_tasks WHERE workflow_id = ?`, o.Task).Scan(&ref)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	return &ReviewBarredError{Order: o.ID, Ref: ref, Crew: crewID}
}

// TaskHead is the head of a task that hasn't landed or been dropped: the
// latest done work order, whose bmus/<Head> branch the land pushes.
type TaskHead struct {
	Ref  string
	Repo string
	Head string
}

// OpenTaskHeads returns the head of every task that has one and hasn't
// landed or been dropped (#182), by head: jettison keeps their branches.
// A task's open work order counts as a head too (#184): the task holds it
// as its open order from its creation until check-head makes it the
// head, so its branch is kept between the order's end and that step.
func (s *Store) OpenTaskHeads(ctx context.Context) ([]TaskHead, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT t.ref, c.repo, t.head FROM campaign_tasks t JOIN campaigns c ON c.id = t.campaign_id
		WHERE t.head <> '' AND t.state NOT IN (?, ?)
		UNION SELECT t.ref, c.repo, t.open_order FROM campaign_tasks t JOIN campaigns c ON c.id = t.campaign_id
		JOIN orders o ON o.id = t.open_order
		WHERE o.kind = ? AND t.state NOT IN (?, ?)
		ORDER BY 3, 1`, TaskLanded, TaskDropped, OrderKindWork, TaskLanded, TaskDropped)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TaskHead
	for rows.Next() {
		var h TaskHead
		if err := rows.Scan(&h.Ref, &h.Repo, &h.Head); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// Campaign states (campaigns.state, #180).
const (
	CampaignRunning  = "running"
	CampaignBelaying = "belaying"
	CampaignEnded    = "ended"
)

// Campaign is a row of the campaigns table (#180).
type Campaign struct {
	ID    string
	State string
	Land  string
	Repo  string
	// BaseRef is where round 1's work starts.
	BaseRef string
	// Division and Assignee are where round 1's work order goes: to
	// Assignee when set, else to Division's pool.
	Division       string
	Assignee       string
	ReviewDivision string
	MaxRounds      int
	CreatedAt      time.Time
	EndedAt        *time.Time
}

const campaignCols = `id, state, land, repo, base_ref, division, assignee, review_division, max_rounds, created_at, ended_at`

func scanCampaign(row interface{ Scan(...any) error }) (Campaign, error) {
	var (
		c     Campaign
		ended sql.NullTime
	)
	err := row.Scan(&c.ID, &c.State, &c.Land, &c.Repo, &c.BaseRef, &c.Division, &c.Assignee, &c.ReviewDivision, &c.MaxRounds,
		&c.CreatedAt, &ended)
	if ended.Valid {
		c.EndedAt = &ended.Time
	}
	return c, err
}

// GetCampaign returns campaign id, or sql.ErrNoRows.
func (s *Store) GetCampaign(ctx context.Context, id string) (Campaign, error) {
	return scanCampaign(s.db.QueryRowContext(ctx, `SELECT `+campaignCols+` FROM campaigns WHERE id = ?`, id))
}

// OpenCampaigns returns the campaigns that haven't ended (running or
// belaying), by id: the bridge starts the workflow of each at startup
// (#184).
func (s *Store) OpenCampaigns(ctx context.Context) ([]Campaign, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+campaignCols+` FROM campaigns WHERE state <> ? ORDER BY id`, CampaignEnded)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Campaign
	for rows.Next() {
		c, err := scanCampaign(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// CampaignTasks returns campaign id's tasks, by ref.
func (s *Store) CampaignTasks(ctx context.Context, id string) ([]CampaignTask, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+taskCols+` FROM campaign_tasks WHERE campaign_id = ? ORDER BY ref`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CampaignTask
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// EndCampaign sets campaign id ended, with ended_at now, and appends
// entry with kind LogKindCampaign and the campaign in its payload, in one
// transaction (#184). A campaign already ended is left alone and nothing
// is logged, so a re-run of the step writes nothing. It reports whether
// it wrote; a missing campaign returns sql.ErrNoRows.
func (s *Store) EndCampaign(ctx context.Context, id string, entry LogEntry) (bool, error) {
	wrote := false
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		var state string
		if err := tx.QueryRowContext(ctx, `SELECT state FROM campaigns WHERE id = ?`, id).Scan(&state); err != nil {
			return err
		}
		if state == CampaignEnded {
			return nil
		}
		if _, err := tx.ExecContext(ctx, `UPDATE campaigns SET state = ?, ended_at = ? WHERE id = ?`,
			CampaignEnded, time.Now().UTC(), id); err != nil {
			return err
		}
		wrote = true
		entry.Kind = LogKindCampaign
		var err error
		if entry.Payload, err = withField(entry.Payload, "campaign", id); err != nil {
			return err
		}
		_, err = s.appendLog(ctx, tx, entry)
		return err
	})
	return wrote, err
}

// TaskOrderIDs returns the orders of task (its workflow id) of kind,
// every kind when kind is "", in creation order.
func (s *Store) TaskOrderIDs(ctx context.Context, task, kind string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM orders WHERE task = ? AND (? = '' OR kind = ?) ORDER BY rowid`,
		task, kind, kind)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
