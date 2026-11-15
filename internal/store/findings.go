package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
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

// FindingLogKind is the log kind of a kept finding's entry.
const FindingLogKind = "finding"

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
// ignored.
//
// A kept finding logs "finding <key>: <title>" (FindingLogKind) on the
// order at ts, the message's time, with payload {tour, seq, key}. Those
// entries order a key's findings: the pump can record a beam's messages
// out of seq order after a reconnect, so a replacement older than the
// last one this crew member filed under the key (an entry with a later
// tour, or a later seq of the same tour) is dropped. Another crew
// member's entries don't count: tour numbers are per crew member, and a
// reassigned order's runs follow each other.
func (t InboxTx) RecordFinding(id string, f Finding, ts time.Time) error {
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
	default:
		var later int
		err := t.tx.QueryRowContext(t.ctx, `SELECT EXISTS (SELECT 1 FROM log
			WHERE order_id = ? AND kind = ? AND crew_id = ? AND json_valid(payload) AND json_extract(payload, '$.key') = ?
			AND (json_extract(payload, '$.tour') > ? OR (json_extract(payload, '$.tour') = ? AND json_extract(payload, '$.seq') > ?)))`,
			id, FindingLogKind, t.crewID, f.Key, t.tourNo, t.tourNo, int64(t.seq)).Scan(&later)
		if err != nil {
			return err
		}
		if later == 1 {
			return fmt.Errorf("%w: finding %s of order %s already has a later version than tour %d seq %d",
				ErrFindingDropped, f.Key, id, t.tourNo, t.seq)
		}
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
	if err != nil {
		return err
	}
	payload, err := json.Marshal(struct {
		Tour int    `json:"tour"`
		Seq  uint64 `json:"seq"`
		Key  string `json:"key"`
	}{t.tourNo, t.seq, f.Key})
	if err != nil {
		return err
	}
	_, err = t.AppendLog(LogEntry{TS: ts, Kind: FindingLogKind, CrewID: t.crewID, OrderID: id,
		Text: fmt.Sprintf("finding %s: %s", f.Key, f.Title), Payload: payload})
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

// findingCols are the columns scanFinding reads.
const findingCols = `order_id, key, crew_id, title, body, labels, severity, blocked_by,
	received_at, state, why, duplicate_of, ref`

func scanFinding(row interface{ Scan(...any) error }) (Finding, error) {
	var (
		f               Finding
		labels, blocked string
	)
	if err := row.Scan(&f.OrderID, &f.Key, &f.CrewID, &f.Title, &f.Body, &labels, &f.Severity, &blocked,
		&f.ReceivedAt, &f.State, &f.Why, &f.DuplicateOf, &f.Ref); err != nil {
		return Finding{}, err
	}
	if err := json.Unmarshal([]byte(labels), &f.Labels); err != nil {
		return Finding{}, fmt.Errorf("finding %s/%s labels: %w", f.OrderID, f.Key, err)
	}
	if err := json.Unmarshal([]byte(blocked), &f.BlockedBy); err != nil {
		return Finding{}, fmt.Errorf("finding %s/%s blocked_by: %w", f.OrderID, f.Key, err)
	}
	return f, nil
}

// OrderFindings returns order id's findings, by key.
func (s *Store) OrderFindings(ctx context.Context, id string) ([]Finding, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+findingCols+` FROM findings WHERE order_id = ? ORDER BY key`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Finding
	for rows.Next() {
		f, err := scanFinding(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// GetFinding returns finding key of order id, or sql.ErrNoRows.
func (s *Store) GetFinding(ctx context.Context, id, key string) (Finding, error) {
	return scanFinding(s.db.QueryRowContext(ctx, `SELECT `+findingCols+` FROM findings WHERE order_id = ? AND key = ?`, id, key))
}

// CountFindings is how many findings order id has, in any state.
func (s *Store) CountFindings(ctx context.Context, id string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM findings WHERE order_id = ?`, id).Scan(&n)
	return n, err
}

// FindingTriage is what Command's triage (#155) does to one finding:
// drop it (with DuplicateOf, as a duplicate of that issue), with Why its
// rationale, or add blockers to it (finding keys or tracker refs).
type FindingTriage struct {
	Key          string
	Drop         bool
	Why          string
	DuplicateOf  string
	AddBlockedBy []string
}

// TriageFindings applies Command's triage to order id's findings that are
// still new, and appends entry to the captain's log, in one transaction
// (#155). A dropped finding becomes FindingDropped with its Why and
// DuplicateOf; added blockers join its blocked_by, each once.
func (s *Store) TriageFindings(ctx context.Context, id string, triage []FindingTriage, entry LogEntry) error {
	return s.withTx(ctx, func(tx *sql.Tx) error {
		for _, t := range triage {
			f, err := scanFinding(tx.QueryRowContext(ctx, `SELECT `+findingCols+` FROM findings WHERE order_id = ? AND key = ?`, id, t.Key))
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			if err != nil {
				return err
			}
			if f.State != FindingNew {
				continue
			}
			blocked := f.BlockedBy
			for _, b := range t.AddBlockedBy {
				if !slices.Contains(blocked, b) {
					blocked = append(blocked, b)
				}
			}
			list, err := jsonList(blocked)
			if err != nil {
				return err
			}
			state := FindingNew
			if t.Drop {
				state = FindingDropped
			}
			if _, err := tx.ExecContext(ctx, `UPDATE findings SET state = ?, why = ?, duplicate_of = ?, blocked_by = ?
				WHERE order_id = ? AND key = ?`, state, t.Why, t.DuplicateOf, list, id, t.Key); err != nil {
				return err
			}
		}
		_, err := s.appendLog(ctx, tx, entry)
		return err
	})
}

// SetFindingRef stores ref as finding key's issue while the finding is
// still new, leaving its state alone: the issue exists, and the findings
// workflow is still adding its links (#155).
func (s *Store) SetFindingRef(ctx context.Context, id, key, ref string) error {
	return s.withTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE findings SET ref = ? WHERE order_id = ? AND key = ? AND state = ?`,
			ref, id, key, FindingNew)
		return err
	})
}

// SettleFinding moves finding key of order id from new to state (filed or
// failed), with why and, when not empty, ref, and appends entry to the
// captain's log, in one transaction (#155). It reports whether the
// finding was still new; one that wasn't is left alone and nothing is
// logged.
func (s *Store) SettleFinding(ctx context.Context, id, key, state, ref, why string, entry LogEntry) (bool, error) {
	var settled bool
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE findings SET state = ?, why = ?, ref = CASE WHEN ? = '' THEN ref ELSE ? END
			WHERE order_id = ? AND key = ? AND state = ?`, state, why, ref, ref, id, key, FindingNew)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil || n == 0 {
			return err
		}
		settled = true
		_, err = s.appendLog(ctx, tx, entry)
		return err
	})
	return settled, err
}
