package store

import (
	"context"
	"database/sql"
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
// and its payload gains campaign, task (the ref) and round, as written;
// entry.OrderID names the order concerned, if any. A row that already
// holds t's values is left alone and nothing is logged, so a re-run of
// the step that moved the task writes nothing. It reports whether it
// wrote. A missing task returns sql.ErrNoRows.
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
		entry.Kind = LogKindCampaign
		for _, f := range []struct {
			key   string
			value any
		}{{"campaign", t.CampaignID}, {"task", t.Ref}, {"round", t.Round}} {
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
