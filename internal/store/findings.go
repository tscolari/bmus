package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// MaxFindings is the most findings an order keeps, counted by distinct
// key (#153; campaigns stage 2 plan, Decisions). A finding replacing one
// with the same key doesn't count again.
const MaxFindings = 20

// Finding states (findings.state). RecordFinding writes FindingNew; the
// findings workflow (#155) moves them on.
const (
	FindingNew       = "new"
	FindingDropped   = "dropped"
	FindingFiled     = "filed"
	FindingFailed    = "failed"
	FindingKept      = "kept"
	FindingDiscarded = "discarded"
	FindingRestored  = "restored"
)

// Finding is a row of findings: something a crew member found while
// working on an order that deserves its own tracker issue (#153).
type Finding struct {
	OrderID string
	Key     string
	// CrewID is who filed it.
	CrewID     string
	Title      string
	Body       string
	Labels     []string
	Severity   string
	BlockedBy  []string
	ReceivedAt time.Time
	State      string
	// Why is Command's reason for dropping it, or the filing error.
	Why         string
	DuplicateOf string
	// Ref is its issue, once filed.
	Ref string
}

// ErrFindingDropped is returned (wrapped, with the reason) by
// RecordFinding for a finding it kept nothing of. The transaction is
// untouched and may commit.
var ErrFindingDropped = errors.New("finding dropped")

// RecordFinding records f, from the crew member whose message is being
// recorded, on order id (#153), as a new finding received now. It is kept
// only while the order exists, hasn't ended (done, failed, belayed,
// withdrawn, or split: its mission is over) and has that crew member as
// its assignee. A finding with a key already recorded on the order
// replaces that row while it is still new; once the findings workflow has
// moved it on, the new one is dropped. A key the order doesn't have yet is
// dropped once the order holds MaxFindings. A finding it doesn't keep
// returns ErrFindingDropped, wrapped with the reason, having written
// nothing. f.OrderID, CrewID, ReceivedAt and the workflow's fields are
// ignored. The caller logs a kept one.
func (t InboxTx) RecordFinding(id string, f Finding) error {
	o, err := t.Order(id)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: no order %s", ErrFindingDropped, id)
	}
	if err != nil {
		return err
	}
	if OrderEnded(o.State) || o.State == OrderSplit {
		return fmt.Errorf("%w: order %s is %s", ErrFindingDropped, id, o.State)
	}
	if o.Assignee != t.crewID {
		return fmt.Errorf("%w: order %s is not assigned to %s", ErrFindingDropped, id, t.crewID)
	}
	var state string
	err = t.tx.QueryRowContext(t.ctx, `SELECT state FROM findings WHERE order_id = ? AND key = ?`, id, f.Key).Scan(&state)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		var n int
		if err := t.tx.QueryRowContext(t.ctx, `SELECT COUNT(*) FROM findings WHERE order_id = ?`, id).Scan(&n); err != nil {
			return err
		}
		if n >= MaxFindings {
			return fmt.Errorf("%w: order %s already has %d findings", ErrFindingDropped, id, MaxFindings)
		}
	case err != nil:
		return err
	case state != FindingNew:
		return fmt.Errorf("%w: finding %s of order %s is already %s", ErrFindingDropped, f.Key, id, state)
	}
	labels, err := jsonList(f.Labels)
	if err != nil {
		return err
	}
	blocked, err := jsonList(f.BlockedBy)
	if err != nil {
		return err
	}
	_, err = t.tx.ExecContext(t.ctx, `INSERT INTO findings (order_id, key, crew_id, title, body, labels, severity, blocked_by, received_at, state)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (order_id, key) DO UPDATE SET crew_id = excluded.crew_id, title = excluded.title, body = excluded.body,
			labels = excluded.labels, severity = excluded.severity, blocked_by = excluded.blocked_by, received_at = excluded.received_at`,
		id, f.Key, t.crewID, f.Title, f.Body, labels, f.Severity, blocked, time.Now().UTC(), FindingNew)
	return err
}

// jsonList is l as a JSON array, [] for none.
func jsonList(l []string) (string, error) {
	if l == nil {
		l = []string{}
	}
	b, err := json.Marshal(l)
	return string(b), err
}

// OrderFindings returns order id's findings, by key.
func (s *Store) OrderFindings(ctx context.Context, id string) ([]Finding, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT order_id, key, crew_id, title, body, labels, severity, blocked_by,
		received_at, state, why, duplicate_of, ref FROM findings WHERE order_id = ? ORDER BY key`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Finding
	for rows.Next() {
		var (
			f               Finding
			labels, blocked string
		)
		if err := rows.Scan(&f.OrderID, &f.Key, &f.CrewID, &f.Title, &f.Body, &labels, &f.Severity, &blocked,
			&f.ReceivedAt, &f.State, &f.Why, &f.DuplicateOf, &f.Ref); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(labels), &f.Labels); err != nil {
			return nil, fmt.Errorf("finding %s/%s labels: %w", id, f.Key, err)
		}
		if err := json.Unmarshal([]byte(blocked), &f.BlockedBy); err != nil {
			return nil, fmt.Errorf("finding %s/%s blocked_by: %w", id, f.Key, err)
		}
		out = append(out, f)
	}
	return out, rows.Err()
}
