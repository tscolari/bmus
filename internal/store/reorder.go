package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// ErrReorder is returned, wrapped, by ReorderOrder and OrderChange.Apply
// for a change they refuse (#148): an order a crew member has started or
// that has ended, a repo the order doesn't work on, an after it may not
// drop or one that would make a cycle. The text before it says why.
var ErrReorder = errors.New("cannot reorder")

// OrderChange is a change to the base and after of an order no crew member
// has started (#148). With Base set, repo Repo's base_ref becomes *Base
// (Repo may be empty for an order with one repo). AfterRemove drops orders
// from its after and AfterAdd adds orders to it.
type OrderChange struct {
	Repo        string
	Base        *string
	AfterAdd    []string
	AfterRemove []string
}

// baseOrders returns the orders repos' base_refs name (order:<id>, #143),
// each once, in repos' order.
func baseOrders(repos []OrderRepo) []string {
	var ids []string
	for _, r := range repos {
		if id, ok := strings.CutPrefix(r.BaseRef, OrderBasePrefix); ok && !slices.Contains(ids, id) {
			ids = append(ids, id)
		}
	}
	return ids
}

// Apply returns the repos and after order o would have with ch applied.
// A base of order:<id> replaces the order: entry the repo's old base put
// in after with <id>; a plain ref drops it. An entry another repo's base
// still names stays. After the base, AfterRemove drops each of its ids,
// refusing one o doesn't wait on or one a base implies, then AfterAdd adds
// each of its ids not there yet. The new base's order comes last. Refusals
// are ErrReorder, wrapped. Apply reads nothing: whether the orders exist,
// and whether the wait is a cycle, is ReorderOrder's to check.
func (ch OrderChange) Apply(o Order) (repos []OrderRepo, after []string, err error) {
	refuse := func(format string, args ...any) ([]OrderRepo, []string, error) {
		return nil, nil, fmt.Errorf(format+": %w", append(args, ErrReorder)...)
	}
	repos = slices.Clone(o.Repos)
	if ch.Base != nil {
		i := -1
		switch {
		case ch.Repo != "":
			i = slices.IndexFunc(repos, func(r OrderRepo) bool { return r.Name == ch.Repo })
			if i < 0 {
				return refuse("order %s doesn't work on repo %s", o.ID, ch.Repo)
			}
		case len(repos) == 1:
			i = 0
		default:
			return refuse("order %s works on %d repos: name the one to rebase", o.ID, len(repos))
		}
		repos[i].BaseRef = *ch.Base
	}
	oldBases, newBases := baseOrders(o.Repos), baseOrders(repos)
	for _, id := range o.After {
		if slices.Contains(oldBases, id) && !slices.Contains(newBases, id) {
			continue
		}
		after = append(after, id)
	}
	for _, id := range ch.AfterRemove {
		if slices.Contains(ch.AfterAdd, id) {
			return refuse("after %s: it is both added and removed", id)
		}
		if slices.Contains(newBases, id) {
			return refuse("after %s: order %s is based on order:%s, which waits on it; rebase the order to drop the wait", id, o.ID, id)
		}
		i := slices.Index(after, id)
		if i < 0 {
			return refuse("after %s: order %s doesn't wait on it", id, o.ID)
		}
		after = slices.Delete(after, i, i+1)
	}
	for _, id := range append(slices.Clone(ch.AfterAdd), newBases...) {
		if !slices.Contains(after, id) {
			after = append(after, id)
		}
	}
	return repos, after, nil
}

// startedWhy says why order o, read in tx, can't be reordered (#148): it
// ended, it was split, a crew member claimed it, or it was recalled and
// keeps a parked mission or a checkpoint, whose work is built on its
// current base. "" for an order no crew member has started.
func startedWhy(ctx context.Context, tx *sql.Tx, o Order) (string, error) {
	switch {
	case slices.Contains(endedStates, o.State):
		return fmt.Sprintf("order %s ended %s", o.ID, o.State), nil
	case o.State == OrderSplit:
		return fmt.Sprintf("order %s is split: its mission has ended, and its work is in its child orders", o.ID), nil
	case o.State != OrderPool && o.State != OrderDirected:
		return fmt.Sprintf("order %s is %s: a crew member has started it on its current base", o.ID, o.State), nil
	case o.ParkedAt != nil:
		return fmt.Sprintf("order %s was recalled and keeps its parked mission, whose work is built on its current base", o.ID), nil
	}
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM checkpoints WHERE order_id = ?)`, o.ID).Scan(&n); err != nil {
		return "", err
	}
	if n == 1 {
		return fmt.Sprintf("order %s has a checkpoint, whose work is built on its current base", o.ID), nil
	}
	return "", nil
}

// waitsOn reports whether order from waits on order id, directly or down
// its chain, in tx: through order_after, and through a split order's
// children, which it waits on to settle done.
func waitsOn(ctx context.Context, tx *sql.Tx, from, id string) (bool, error) {
	var n int
	err := tx.QueryRowContext(ctx, `WITH RECURSIVE up(id) AS (
			SELECT ?
			UNION
			SELECT a.after_id FROM order_after a JOIN up ON a.order_id = up.id
			UNION
			SELECT c.id FROM orders c JOIN up ON c.parent_id = up.id
		) SELECT EXISTS (SELECT 1 FROM up WHERE id = ?)`, from, id).Scan(&n)
	return n == 1, err
}

// ReorderOrder applies ch to order id (#148) and appends entry(before,
// after), the order as it was and as it is now, in one transaction. The
// order must be one no crew member has started: pool or directed, with no
// parked mission and no checkpoint. ch is applied as OrderChange.Apply
// says, and each order it adds to after is checked as InsertOrder checks
// an after (it exists, isn't the order itself, didn't end failed, belayed
// or withdrawn), as is each new base: it must not have been split. An
// added order that waits on this one, directly or down its chain (a split
// order waiting on its children included), would make a cycle. Each
// refusal is ErrReorder or ErrAfter, wrapped, and writes nothing. A
// change that leaves the order as it was writes nothing either. It returns
// the order afterwards and whether this call changed it; a missing order
// returns sql.ErrNoRows.
func (s *Store) ReorderOrder(ctx context.Context, id string, ch OrderChange, entry func(before, after Order) (LogEntry, error)) (o Order, changed bool, err error) {
	err = s.withTx(ctx, func(tx *sql.Tx) error {
		before, err := orderTx(ctx, tx, id)
		if err != nil {
			return err
		}
		o = before
		why, err := startedWhy(ctx, tx, before)
		if err != nil {
			return err
		}
		if why != "" {
			return fmt.Errorf("%s: %w", why, ErrReorder)
		}
		repos, after, err := ch.Apply(before)
		if err != nil {
			return err
		}
		var added, removed []string
		for _, a := range after {
			if !slices.Contains(before.After, a) {
				added = append(added, a)
			}
		}
		for _, a := range before.After {
			if !slices.Contains(after, a) {
				removed = append(removed, a)
			}
		}
		if slices.Equal(repos, before.Repos) && len(added) == 0 && len(removed) == 0 {
			return nil
		}
		oldBases := baseOrders(before.Repos)
		for _, a := range added {
			if a == id {
				return fmt.Errorf("after %s: an order can't wait on itself: %w", a, ErrAfter)
			}
			var state string
			err := tx.QueryRowContext(ctx, `SELECT state FROM orders WHERE id = ?`, a).Scan(&state)
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("after %s: no such order: %w", a, ErrAfter)
			}
			if err != nil {
				return err
			}
			if state == OrderFailed || state == OrderBelayed || state == OrderWithdrawn {
				return fmt.Errorf("after %s: it ended %s and will never be done: %w", a, state, ErrAfter)
			}
			if slices.Contains(baseOrders(repos), a) && !slices.Contains(oldBases, a) {
				children, err := childIDs(ctx, tx, a)
				if err != nil {
					return err
				}
				if len(children) > 0 {
					return fmt.Errorf("base order:%s: %s: %w", a, SplitBaseText(a, children), ErrReorder)
				}
			}
			cycle, err := waitsOn(ctx, tx, a, id)
			if err != nil {
				return err
			}
			if cycle {
				return fmt.Errorf("after %s: %s waits on %s, directly or down its chain, so the wait would be a cycle: %w", a, a, id, ErrReorder)
			}
		}
		for _, r := range repos {
			if _, err := tx.ExecContext(ctx, `UPDATE order_repos SET base_ref = ? WHERE order_id = ? AND name = ?`, r.BaseRef, id, r.Name); err != nil {
				return err
			}
		}
		for _, a := range removed {
			if _, err := tx.ExecContext(ctx, `DELETE FROM order_after WHERE order_id = ? AND after_id = ?`, id, a); err != nil {
				return err
			}
		}
		for _, a := range added {
			if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO order_after (order_id, after_id) VALUES (?, ?)`, id, a); err != nil {
				return err
			}
		}
		if o, err = orderTx(ctx, tx, id); err != nil {
			return err
		}
		e, err := entry(before, o)
		if err != nil {
			return err
		}
		e.OrderID = id
		changed = true
		_, err = s.appendLog(ctx, tx, e)
		return err
	})
	if err != nil {
		return Order{}, false, err
	}
	return o, changed, nil
}

// orderTx reads order id with its repos in tx; sql.ErrNoRows for none.
func orderTx(ctx context.Context, tx *sql.Tx, id string) (Order, error) {
	o, err := scanOrder(tx.QueryRowContext(ctx, `SELECT `+orderCols+` FROM orders WHERE id = ?`, id))
	if err != nil {
		return Order{}, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT name, base_ref FROM order_repos WHERE order_id = ? ORDER BY pos`, id)
	if err != nil {
		return Order{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var r OrderRepo
		if err := rows.Scan(&r.Name, &r.BaseRef); err != nil {
			return Order{}, err
		}
		o.Repos = append(o.Repos, r)
	}
	return o, rows.Err()
}

// childIDs is ChildIDs in tx.
func childIDs(ctx context.Context, tx *sql.Tx, parent string) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id FROM orders WHERE parent_id = ? ORDER BY rowid`, parent)
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

// OrderLogMarked reports whether order id's log has an entry of kind
// whose payload's field is value: a hail's change already applied (#148).
func (s *Store) OrderLogMarked(ctx context.Context, id, kind, field, value string) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM log WHERE order_id = ? AND kind = ?
		AND json_valid(payload) AND json_extract(payload, '$.' || ?) = ?)`, id, kind, field, value).Scan(&n)
	return n == 1, err
}
