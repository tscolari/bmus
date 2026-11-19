package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// A database made with stage 2's schema (testdata/schema-before-180.sql,
// schema.sql as it was) is refused, naming the file to delete for a fresh
// database, before the schema runs: it gains no campaign table or index.
func TestOpenRejectsStage2Schema(t *testing.T) {
	t.Parallel()
	old, err := os.ReadFile(filepath.Join("testdata", "schema-before-180.sql"))
	require.NoError(t, err)
	path := filepath.Join(t.TempDir(), DBFile)
	db, err := sql.Open("sqlite", path)
	require.NoError(t, err)
	_, err = db.Exec(string(old))
	require.NoError(t, err)
	require.NoError(t, db.Close())

	_, err = Open(t.Context(), path)
	require.ErrorContains(t, err, "bridge.db predates the campaign orders (#180); delete "+path+
		" and start the bridge again, which makes a fresh database")

	db, err = sql.Open("sqlite", path)
	require.NoError(t, err)
	defer db.Close()
	var n int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM sqlite_master
		WHERE name IN ('campaigns', 'campaign_tasks', 'task_workers', 'hails_raise', 'orders_task', 'orders_created_by')`).Scan(&n))
	require.Zero(t, n)
}

// A new database has the #180 schema: the order and hail columns, the
// raise index in place of stage 2's, the campaign tables, one open task
// per ref and one task_workers row per task and crew member.
func TestStage3Schema(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s := openTest(t)
	orders := tableColumns(t, s.db, "orders")
	require.Equal(t, "TEXT notnull=1 default='work'", orders["kind"])
	require.Equal(t, "TEXT notnull=1 default=''", orders["task"])
	require.Equal(t, "TEXT notnull=1 default=''", tableColumns(t, s.db, "hails")["raise_key"])
	indexes := map[string]bool{}
	rows, err := s.db.QueryContext(ctx, `SELECT name FROM sqlite_master WHERE type = 'index'`)
	require.NoError(t, err)
	for rows.Next() {
		var name string
		require.NoError(t, rows.Scan(&name))
		indexes[name] = true
	}
	require.NoError(t, rows.Close())
	for _, name := range []string{"hails_raise", "orders_task", "orders_created_by", "campaign_tasks_open"} {
		require.True(t, indexes[name], name)
	}
	require.False(t, indexes["hails_deliver_to"], "hails_raise replaces it")
	require.Contains(t, tableColumns(t, s.db, "campaigns"), "review_division")
	require.Contains(t, tableColumns(t, s.db, "campaign_tasks"), "open_hail")

	insertCampaign(t, s, "C-0001", "o/r#12")
	insertCampaign(t, s, "C-0002", "")
	insertTask := `INSERT INTO campaign_tasks (campaign_id, ref, workflow_id, max_rounds, branch, state) VALUES (?, ?, ?, 3, 'b', ?)`
	_, err = s.db.ExecContext(ctx, insertTask, "C-0002", "o/r#12", "campaign/C-0002/task/o/r#12", TaskWorking)
	require.ErrorContains(t, err, "UNIQUE", "one open task per ref")
	_, err = s.db.ExecContext(ctx, `UPDATE campaign_tasks SET state = ? WHERE campaign_id = 'C-0001'`, TaskLanded)
	require.NoError(t, err)
	_, err = s.db.ExecContext(ctx, insertTask, "C-0002", "o/r#12", "campaign/C-0002/task/o/r#12", TaskWorking)
	require.NoError(t, err, "a landed task leaves its ref free")

	require.NoError(t, s.InsertCrew(ctx, Crew{ID: "sorell", Name: "Sorell"}, testEntry))
	_, err = s.db.ExecContext(ctx, `INSERT INTO task_workers (task, crew_id) VALUES ('t', 'sorell')`)
	require.NoError(t, err)
	_, err = s.db.ExecContext(ctx, `INSERT INTO task_workers (task, crew_id) VALUES ('t', 'sorell')`)
	require.ErrorContains(t, err, "UNIQUE")
	_, err = s.db.ExecContext(ctx, `INSERT INTO task_workers (task, crew_id) VALUES ('t', 'nobody')`)
	require.ErrorContains(t, err, "FOREIGN KEY")
}

// insertCampaign inserts campaign id and, with a ref, its task for it,
// whose workflow id is campaign/<id>/task/<ref>.
func insertCampaign(t *testing.T, s *Store, id, ref string) {
	t.Helper()
	_, err := s.db.ExecContext(t.Context(), `INSERT INTO campaigns (id, repo, base_ref, created_at) VALUES (?, 'app', 'main', ?)`,
		id, time.Now().UTC())
	require.NoError(t, err)
	if ref == "" {
		return
	}
	_, err = s.db.ExecContext(t.Context(), `INSERT INTO campaign_tasks (campaign_id, ref, workflow_id, max_rounds, branch)
		VALUES (?, ?, ?, 3, ?)`, id, ref, "campaign/"+id+"/task/"+ref, "campaign/"+id+"/o-r-12")
	require.NoError(t, err)
}

// InsertOrderOnce under one key twice returns one id and logs once, even
// when the second call's order differs; another key inserts another
// order. The kind and task are stored, work by default. An empty key, a
// bad kind and a review with no task are refused.
func TestInsertOrderOnce(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s := openTest(t)
	last, err := s.LastLogID(ctx)
	require.NoError(t, err)

	key := "campaign:C-0001:o/r#12:r1:work"
	o := Order{Title: "Implement o/r#12", Division: "eng", CreatedBy: key, Task: "campaign/C-0001/task/o/r#12",
		Source: "tracker:o/r#12", Repos: []OrderRepo{{Name: "app", BaseRef: "main"}}}
	id, created, err := s.InsertOrderOnce(ctx, o, LogEntry{Kind: "order", Text: "order created"})
	require.NoError(t, err)
	require.True(t, created)
	again := o
	again.Title = "changed"
	id2, created, err := s.InsertOrderOnce(ctx, again, LogEntry{Kind: "order", Text: "order created again"})
	require.NoError(t, err)
	require.False(t, created)
	require.Equal(t, id, id2)
	got := getT(t, s, id)
	require.Equal(t, "Implement o/r#12", got.Title)
	require.Equal(t, OrderKindWork, got.Kind)
	require.Equal(t, o.Task, got.Task)
	require.Equal(t, key, got.CreatedBy)
	require.Equal(t, o.Repos, got.Repos)
	entries, err := s.ListLog(ctx, last)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.Equal(t, id, entries[0].OrderID)

	review := Order{Title: "Review o/r#12 (round 1)", Division: "science", CreatedBy: "campaign:C-0001:o/r#12:r1:review",
		Kind: OrderKindReview, Task: o.Task, Round: 1, ReviewBase: testBase}
	rid, created, err := s.InsertOrderOnce(ctx, review, testEntry)
	require.NoError(t, err)
	require.True(t, created)
	require.NotEqual(t, id, rid)
	gotReview := getT(t, s, rid)
	require.Equal(t, OrderKindReview, gotReview.Kind)
	require.Equal(t, 1, gotReview.Round)
	require.Equal(t, testBase, gotReview.ReviewBase)
	require.Zero(t, got.Round)
	require.Empty(t, got.ReviewBase)

	_, _, err = s.InsertOrderOnce(ctx, Order{Title: "x"}, testEntry)
	require.ErrorContains(t, err, "needs createdBy")
	_, _, err = s.InsertOrderOnce(ctx, Order{Title: "x", CreatedBy: "k", Kind: "chore"}, testEntry)
	require.ErrorIs(t, err, ErrOrderKind)
	_, err = s.InsertOrder(ctx, Order{Title: "x", Kind: OrderKindReview}, testEntry)
	require.ErrorIs(t, err, ErrOrderKind)
	// #182: a review needs its round and base; a round needs a task, and a
	// review base a review.
	for _, bad := range []Order{
		{Title: "x", Kind: OrderKindReview, Task: o.Task, ReviewBase: testBase},
		{Title: "x", Kind: OrderKindReview, Task: o.Task, Round: 1},
		{Title: "x", Kind: OrderKindReview, Task: o.Task, Round: 1, ReviewBase: "main"},
		{Title: "x", Kind: OrderKindReview, Task: o.Task, Round: 1, ReviewBase: testBase[:12]},
		{Title: "x", Task: o.Task, Round: -1},
		{Title: "x", Round: 1},
		{Title: "x", Task: o.Task, Round: 1, ReviewBase: testBase},
	} {
		_, err = s.InsertOrder(ctx, bad, testEntry)
		require.ErrorIs(t, err, ErrOrderKind, "%+v", bad)
	}
	all, err := s.ListOrders(ctx)
	require.NoError(t, err)
	require.Len(t, all, 2, "the refusals wrote nothing")
}

// InsertBridgeHail under two raise keys raises two hails, and under one
// key twice raises one, also once it is closed. The key reads back, and
// the index holds it whatever path writes the row.
func TestInsertBridgeHailRaiseKey(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s := openTest(t)
	h := Hail{DeliverTo: "campaign/C-0001/task/o/r#12", Category: "campaign_stalled", Question: "?", RaiseKey: "stall-1"}
	first, err := s.InsertBridgeHail(ctx, h, LogEntry{Kind: "hail", Text: "stall 1"})
	require.NoError(t, err)
	again, err := s.InsertBridgeHail(ctx, h, LogEntry{Kind: "hail", Text: "stall 1 again"})
	require.NoError(t, err)
	require.Equal(t, first, again)
	require.NoError(t, s.DecideOpenHail(ctx, first, DeciderCaptain, "retry", "", testEntry, nil))
	again, err = s.InsertBridgeHail(ctx, h, LogEntry{Kind: "hail", Text: "stall 1 again"})
	require.NoError(t, err)
	require.Equal(t, first, again, "closed, it still answers the re-run")

	second := h
	second.RaiseKey = "stall-2"
	other, err := s.InsertBridgeHail(ctx, second, LogEntry{Kind: "hail", Text: "stall 2"})
	require.NoError(t, err)
	require.NotEqual(t, first, other)
	got, err := s.GetHail(ctx, other)
	require.NoError(t, err)
	require.Equal(t, "stall-2", got.RaiseKey)
	require.Equal(t, HailOpen, got.Status)

	unkeyed := h
	unkeyed.RaiseKey = ""
	third, err := s.InsertBridgeHail(ctx, unkeyed, LogEntry{Kind: "hail", Text: "no key"})
	require.NoError(t, err)
	require.NotContains(t, []string{first, other}, third, "the empty key is a key of its own")

	var n int
	require.NoError(t, s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM hails`).Scan(&n))
	require.Equal(t, 3, n)
	_, err = s.db.ExecContext(ctx, `INSERT INTO hails (id, deliver_to, category, raise_key, question) VALUES ('H-9999', ?, ?, ?, '?')`,
		h.DeliverTo, h.Category, "stall-2")
	require.ErrorContains(t, err, "UNIQUE")
}

// hookRecorder is a TaskHook that records what it is told, and the
// order's state as its transaction reads it.
type hookRecorder struct {
	mu   sync.Mutex
	got  []TaskOrder
	seen []string
	fail error
}

func (h *hookRecorder) hook(tx *sql.Tx, o TaskOrder) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.fail != nil {
		return h.fail
	}
	var state string
	if err := tx.QueryRow(`SELECT state FROM orders WHERE id = ?`, o.Order).Scan(&state); err != nil {
		return err
	}
	h.got = append(h.got, o)
	h.seen = append(h.seen, state)
	return nil
}

func (h *hookRecorder) take() []TaskOrder {
	h.mu.Lock()
	defer h.mu.Unlock()
	got := h.got
	h.got, h.seen = nil, nil
	return got
}

// The task hook runs, in the ending transaction, for every ending path of
// an order with a task: settle done and failed, withdraw, the cascades
// of a failed after and of a belayed parent, a belay of an unclaimed
// (recalled) order, the mission's settle of an open order and the tour's
// backstop. A split runs it for split and again for the parent's settle.
// An order with no task, and a re-run that ends nothing, run it not.
func TestTaskHookEndings(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s := openTest(t)
	rec := &hookRecorder{}
	s.SetTaskHook(rec.hook)
	const task = "campaign/C-0001/task/o/r#12"
	require.NoError(t, s.InsertCrew(ctx, Crew{ID: "sorell", Name: "Sorell", Division: "eng"}, testEntry))
	away := func() string {
		t.Helper()
		id := insertT(t, s, Order{Title: "w", Assignee: "sorell", Task: task})
		got, found := claimDirected(t, s, "sorell")
		require.True(t, found)
		require.Equal(t, id, got)
		return id
	}
	want := func(id, state string) {
		t.Helper()
		require.Equal(t, []TaskOrder{{Task: task, Order: id, State: state}}, rec.take())
	}

	// Settle done, and its re-run.
	id := away()
	for range 2 {
		_, _, err := s.SettleOrder(ctx, id, OrderDone, testEntry, nil)
		require.NoError(t, err)
	}
	want(id, OrderDone)

	// Settle failed.
	id = away()
	_, _, err := s.SettleOrder(ctx, id, OrderFailed, testEntry, nil)
	require.NoError(t, err)
	want(id, OrderFailed)

	// The tour's backstop.
	id = away()
	_, settled, err := s.SettleAssignedOrder(ctx, id, "sorell", OrderFailed, testEntry, true)
	require.NoError(t, err)
	require.True(t, settled)
	want(id, OrderFailed)

	// The mission's settle of an open order.
	id = away()
	_, _, settled, err = s.SettleOpenOrder(ctx, id, OrderFailed, testEntry)
	require.NoError(t, err)
	require.True(t, settled)
	want(id, OrderFailed)

	// Withdraw.
	id = insertT(t, s, Order{Title: "p", Division: "eng", Task: task})
	_, withdrawn, err := s.WithdrawOrder(ctx, id, testEntry)
	require.NoError(t, err)
	require.True(t, withdrawn)
	want(id, OrderWithdrawn)

	// A belay of an unclaimed order: recalled, its mission parked.
	id = away()
	_, recalled, err := s.RecallOrder(ctx, id, "sorell", testEntry)
	require.NoError(t, err)
	require.True(t, recalled)
	require.Empty(t, rec.take(), "a recall ends nothing")
	_, _, belayed, err := s.BelayOrder(ctx, id, testEntry, nil)
	require.NoError(t, err)
	require.True(t, belayed)
	want(id, OrderBelayed)

	// The cascade of a failed after: an order with no task fails, and its
	// dependent with one is withdrawn.
	after := insertT(t, s, Order{Title: "a", Assignee: "sorell"})
	_, found := claimDirected(t, s, "sorell")
	require.True(t, found)
	dep := insertT(t, s, Order{Title: "d", Division: "eng", Task: task, After: []string{after}})
	_, _, err = s.SettleOrder(ctx, after, OrderFailed, testEntry, nil)
	require.NoError(t, err)
	want(dep, OrderWithdrawn)

	// A split: split, then the child's done settles the parent done.
	parent := away()
	kids, err := s.InsertChildOrders(ctx, parent, "split", []Order{{Title: "k", Division: "eng"}},
		func([]string) (LogEntry, error) { return testEntry, nil })
	require.NoError(t, err)
	_, _, err = s.SettleOrder(ctx, parent, OrderSplit, testEntry, nil)
	require.NoError(t, err)
	want(parent, OrderSplit)
	got, found := claimPool(t, s, "sorell", "eng")
	require.True(t, found)
	require.Equal(t, kids[0], got)
	_, _, err = s.SettleOrder(ctx, kids[0], OrderDone, testEntry, nil)
	require.NoError(t, err)
	want(parent, OrderDone)

	// The cascade of a belayed parent: a pooled child with a task is
	// withdrawn, then the parent with none is belayed.
	split := insertT(t, s, Order{Title: "s", Assignee: "sorell"})
	_, found = claimDirected(t, s, "sorell")
	require.True(t, found)
	kids, err = s.InsertChildOrders(ctx, split, "split", []Order{{Title: "k", Division: "eng", Task: task}},
		func([]string) (LogEntry, error) { return testEntry, nil })
	require.NoError(t, err)
	_, _, err = s.SettleOrder(ctx, split, OrderSplit, testEntry, nil)
	require.NoError(t, err)
	_, _, belayed, err = s.BelaySplitOrder(ctx, split, testEntry, nil)
	require.NoError(t, err)
	require.True(t, belayed)
	want(kids[0], OrderWithdrawn)

	// Hook and write share the transaction: the hook read the order ended.
	id = away()
	_, _, err = s.SettleOrder(ctx, id, OrderDone, testEntry, nil)
	require.NoError(t, err)
	rec.mu.Lock()
	require.Equal(t, []string{OrderDone}, rec.seen)
	rec.mu.Unlock()
	rec.take()

	// A hook's error rolls the ending back.
	id = away()
	rec.fail = errors.New("no send")
	_, _, err = s.SettleOrder(ctx, id, OrderDone, testEntry, nil)
	require.ErrorContains(t, err, "no send")
	require.Equal(t, OrderAway, getT(t, s, id).State)
	rec.fail = nil
	_, _, err = s.SettleOrder(ctx, id, OrderDone, testEntry, nil)
	require.NoError(t, err)
	want(id, OrderDone)

	// No task, no hook; nor with the hook unset.
	plain := insertT(t, s, Order{Title: "plain", Division: "eng"})
	_, _, err = s.WithdrawOrder(ctx, plain, testEntry)
	require.NoError(t, err)
	require.Empty(t, rec.take())
	s.SetTaskHook(nil)
	id = insertT(t, s, Order{Title: "p", Division: "eng", Task: task})
	_, withdrawn, err = s.WithdrawOrder(ctx, id, testEntry)
	require.NoError(t, err)
	require.True(t, withdrawn)
	require.Empty(t, rec.take())
}

// workers returns task's task_workers rows, by crew id.
func workers(t *testing.T, s *Store, task string) []string {
	t.Helper()
	rows, err := s.db.QueryContext(t.Context(), `SELECT crew_id FROM task_workers WHERE task = ? ORDER BY crew_id`, task)
	require.NoError(t, err)
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		require.NoError(t, rows.Scan(&id))
		out = append(out, id)
	}
	require.NoError(t, rows.Err())
	return out
}

// The claim rule (#180): claiming a work order of a task records the
// claimer in task_workers, which a recall keeps; a review order's claim
// writes nothing. A worker never claims the task's review, pooled or
// directed, while another crew member of the division does. The round 1
// reviewer claims round 2's review, and one who claims a pooled fix order
// is barred from then on. Another task's review is not barred.
func TestClaimReviewBarred(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s := openTest(t)
	const task = "campaign/C-0001/task/o/r#12"
	for _, c := range []string{"sorell", "tuvok", "spock"} {
		require.NoError(t, s.InsertCrew(ctx, Crew{ID: c, Name: c, Division: "science"}, testEntry))
	}
	done := func(id string) {
		t.Helper()
		_, settled, err := s.SettleOrder(ctx, id, OrderDone, testEntry, nil)
		require.NoError(t, err)
		require.True(t, settled)
	}

	// Round 1's work, directed to sorell, and recalled once on the way.
	work := insertT(t, s, Order{Title: "Implement o/r#12", Assignee: "sorell", Task: task})
	got, found := claimDirected(t, s, "sorell")
	require.True(t, found)
	require.Equal(t, work, got)
	require.Equal(t, []string{"sorell"}, workers(t, s, task))
	_, _, err := s.RecallOrder(ctx, work, "sorell", testEntry)
	require.NoError(t, err)
	require.Equal(t, []string{"sorell"}, workers(t, s, task), "a recall keeps the row")
	_, err = s.db.ExecContext(ctx, `UPDATE orders SET division = 'science' WHERE id = ?`, work)
	require.NoError(t, err)
	got, found = claimPool(t, s, "sorell", "science")
	require.True(t, found)
	require.Equal(t, work, got)
	done(work)

	// Round 1's review in the pool: sorell skips it, tuvok takes it.
	review1 := insertT(t, s, Order{Title: "Review o/r#12 (round 1)", Division: "science", Kind: OrderKindReview, Task: task, Round: 1, ReviewBase: testBase})
	_, found = claimPool(t, s, "sorell", "science")
	require.False(t, found, "sorell worked on the task")
	got, found = claimPool(t, s, "tuvok", "science")
	require.True(t, found)
	require.Equal(t, review1, got)
	require.Equal(t, []string{"sorell"}, workers(t, s, task), "a review's claim records no worker")
	done(review1)

	// Directed to sorell, the review is skipped; directed to spock, taken.
	directed := insertT(t, s, Order{Title: "Review o/r#12 (directed)", Assignee: "sorell", Kind: OrderKindReview, Task: task, Round: 1, ReviewBase: testBase})
	_, found = claimDirected(t, s, "sorell")
	require.False(t, found, "sorell's directed claim skips its task's review")
	_, err = s.db.ExecContext(ctx, `UPDATE orders SET assignee = 'spock' WHERE id = ?`, directed)
	require.NoError(t, err)
	got, found = claimDirected(t, s, "spock")
	require.True(t, found)
	require.Equal(t, directed, got)
	done(directed)

	// Another task's review is sorell's to claim.
	elsewhere := insertT(t, s, Order{Title: "Review o/r#13", Division: "science", Kind: OrderKindReview, Task: "campaign/C-0001/task/o/r#13", Round: 1, ReviewBase: testBase})
	got, found = claimPool(t, s, "sorell", "science")
	require.True(t, found)
	require.Equal(t, elsewhere, got)
	done(elsewhere)

	// A pooled fix order goes to tuvok, round 1's reviewer, who is then a
	// worker; round 2's review is barred to tuvok and sorell, and spock
	// claims it.
	fix := insertT(t, s, Order{Title: "Fix o/r#12 (round 2)", Division: "science", Task: task})
	got, found = claimPool(t, s, "tuvok", "science")
	require.True(t, found)
	require.Equal(t, fix, got)
	require.Equal(t, []string{"sorell", "tuvok"}, workers(t, s, task))
	done(fix)
	review2 := insertT(t, s, Order{Title: "Review o/r#12 (round 2)", Division: "science", Kind: OrderKindReview, Task: task, Round: 2, ReviewBase: testBase})
	for _, c := range []string{"sorell", "tuvok"} {
		_, found = claimPool(t, s, c, "science")
		require.False(t, found, c)
	}
	got, found = claimPool(t, s, "spock", "science")
	require.True(t, found)
	require.Equal(t, review2, got)
	done(review2)

	// Round 1's reviewer, no worker, claims round 2's review.
	task2 := "campaign/C-0002/task/o/r#14"
	w := insertT(t, s, Order{Title: "Implement o/r#14", Assignee: "sorell", Task: task2})
	_, found = claimDirected(t, s, "sorell")
	require.True(t, found)
	done(w)
	for round := range 2 {
		r := insertT(t, s, Order{Title: "Review o/r#14", Division: "science", Kind: OrderKindReview, Task: task2, Round: round + 1,
			ReviewBase: testBase})
		got, found = claimPool(t, s, "tuvok", "science")
		require.True(t, found, "round %d", round+1)
		require.Equal(t, r, got)
		done(r)
	}
	require.Equal(t, []string{"sorell"}, workers(t, s, task2))
}

// EligibleReviewers counts the division's crew members who never worked
// on the task: dismissed, sickbay and barred ones are left out, a busy one
// and a worker on another task are not, nor is anyone in another
// division counted.
func TestEligibleReviewers(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s := openTest(t)
	const task = "campaign/C-0001/task/o/r#12"
	for _, c := range []Crew{
		{ID: "sorell", Division: "science"},
		{ID: "tuvok", Division: "science", Status: CrewAway},
		{ID: "spock", Division: "science", Status: CrewSickbay},
		{ID: "saavik", Division: "science", Status: CrewDismissed},
		{ID: "data", Division: "science"},
		{ID: "scotty", Division: "eng"},
	} {
		c.Name = c.ID
		require.NoError(t, s.InsertCrew(ctx, c, testEntry))
	}
	got, err := s.EligibleReviewers(ctx, task, "science")
	require.NoError(t, err)
	require.Equal(t, []string{"data", "sorell", "tuvok"}, got)

	_, err = s.db.ExecContext(ctx, `INSERT INTO task_workers (task, crew_id) VALUES (?, 'sorell'), ('other', 'data')`, task)
	require.NoError(t, err)
	got, err = s.EligibleReviewers(ctx, task, "science")
	require.NoError(t, err)
	require.Equal(t, []string{"data", "tuvok"}, got)

	got, err = s.EligibleReviewers(ctx, task, "")
	require.NoError(t, err)
	require.Empty(t, got)
	got, err = s.EligibleReviewers(ctx, task, "medical")
	require.NoError(t, err)
	require.Empty(t, got)
}

// SetTask writes the task's row and one campaign entry on the order
// concerned, with campaign, task and round in its payload, in one
// transaction. A re-run with the same values writes nothing; a missing
// task returns sql.ErrNoRows. The workflow id stays as it is.
func TestSetTask(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s := openTest(t)
	insertCampaign(t, s, "C-0001", "o/r#12")
	cur, err := s.GetTask(ctx, "C-0001", "o/r#12")
	require.NoError(t, err)
	require.Equal(t, TaskWorking, cur.State)
	require.Equal(t, 1, cur.Round)
	require.Nil(t, cur.LandedAt)
	last, err := s.LastLogID(ctx)
	require.NoError(t, err)

	next := cur
	next.State, next.Head, next.HeadCommit, next.OpenOrder = TaskReviewing, "ORD-0011", "3f2a9c1e0b7d", "ORD-0012"
	next.WorkflowID = "ignored"
	entry := LogEntry{OrderID: "ORD-0011", Text: "o/r#12 round 1: ORD-0011 done at 3f2a9c1e0b7d", Payload: json.RawMessage(`{"head":"ORD-0011"}`)}
	for i := range 2 {
		wrote, err := s.SetTask(ctx, next, entry)
		require.NoError(t, err)
		require.Equal(t, i == 0, wrote)
	}
	got, err := s.GetTask(ctx, "C-0001", "o/r#12")
	require.NoError(t, err)
	next.WorkflowID = cur.WorkflowID
	require.Equal(t, next, got)
	entries, err := s.ListLog(ctx, last)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.Equal(t, LogKindCampaign, entries[0].Kind)
	require.Equal(t, "ORD-0011", entries[0].OrderID)
	require.Equal(t, entry.Text, entries[0].Text)
	require.JSONEq(t, `{"head":"ORD-0011","campaign":"C-0001","ref":"o/r#12","round":1}`, string(entries[0].Payload))

	landed := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	next.State, next.LandedAt = TaskLanded, &landed
	wrote, err := s.SetTask(ctx, next, LogEntry{Text: "o/r#12 landed"})
	require.NoError(t, err)
	require.True(t, wrote)
	same := next
	local := landed.In(time.FixedZone("x", 3600))
	same.LandedAt = &local
	wrote, err = s.SetTask(ctx, same, LogEntry{Text: "again"})
	require.NoError(t, err)
	require.False(t, wrote, "the same instant is no change")
	got, err = s.GetTask(ctx, "C-0001", "o/r#12")
	require.NoError(t, err)
	require.NotNil(t, got.LandedAt)
	require.True(t, landed.Equal(*got.LandedAt))

	_, err = s.SetTask(ctx, CampaignTask{CampaignID: "C-0001", Ref: "o/r#99"}, LogEntry{Text: "x"})
	require.ErrorIs(t, err, sql.ErrNoRows)
}

// testBase is a review's base in tests: a full commit id.
const testBase = "8b1d0e2c4a55f00dfeedfacecafebeef01234567"

// A campaign order stays whole (#182): a route of a review to a crew
// member in task_workers is refused, naming the task by its row's ref,
// while one to another crew member goes through; a reorder of any order
// with a task is refused and writes nothing.
func TestCampaignOrderRefusals(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s := openTest(t)
	insertCampaign(t, s, "C-0001", "o/r#12")
	task := TaskWorkflowID("C-0001", "o/r#12")
	for _, c := range []string{"sorell", "tuvok"} {
		require.NoError(t, s.InsertCrew(ctx, Crew{ID: c, Name: c, Division: "science"}, testEntry))
	}
	work := insertT(t, s, Order{Title: "Implement o/r#12", Assignee: "sorell", Task: task, Round: 1,
		Repos: []OrderRepo{{Name: "app", BaseRef: "main"}}})
	_, found := claimDirected(t, s, "sorell")
	require.True(t, found)
	review := insertT(t, s, Order{Title: "Review o/r#12 (round 1)", Division: "science", Kind: OrderKindReview, Task: task,
		Round: 1, ReviewBase: testBase, Repos: []OrderRepo{{Name: "app", BaseRef: "order:" + work}}})
	last, err := s.LastLogID(ctx)
	require.NoError(t, err)

	str := func(v string) *string { return &v }
	route := func(o Order, to, div string, c *Crew) (LogEntry, error) {
		return LogEntry{Kind: "order", Text: "routed"}, nil
	}
	_, routed, err := s.RouteOrder(ctx, review, str("sorell"), nil, route)
	require.ErrorIs(t, err, ErrReviewBarred)
	require.EqualError(t, err, review+" reviews task o/r#12, which sorell worked on")
	require.False(t, routed)
	o, routed, err := s.RouteOrder(ctx, review, str("tuvok"), nil, route)
	require.NoError(t, err)
	require.True(t, routed)
	require.Equal(t, "tuvok", o.Assignee)
	// Back to the pool needs no check.
	_, routed, err = s.RouteOrder(ctx, review, str(""), nil, route)
	require.NoError(t, err)
	require.True(t, routed)

	base := "main"
	_, _, err = s.ReorderOrder(ctx, review, OrderChange{Base: &base}, func(_, _ Order) (LogEntry, error) {
		return LogEntry{Kind: "order", Text: "reordered"}, nil
	})
	require.ErrorIs(t, err, ErrReorder)
	require.ErrorContains(t, err, review+" is campaign C-0001's review of o/r#12: a campaign order can't be reordered")
	pooled := insertT(t, s, Order{Title: "Fix o/r#12 (round 2)", Division: "science", Task: task, Round: 2,
		Repos: []OrderRepo{{Name: "app", BaseRef: "main"}}})
	why, err := s.StartedWhy(ctx, getT(t, s, pooled))
	require.NoError(t, err)
	require.Equal(t, pooled+" is campaign C-0001's work on o/r#12: a campaign order can't be reordered", why)
	_, _, err = s.ReorderOrder(ctx, pooled, OrderChange{AfterAdd: []string{work}}, func(_, _ Order) (LogEntry, error) {
		return LogEntry{Kind: "order", Text: "reordered"}, nil
	})
	require.ErrorIs(t, err, ErrReorder)
	require.Empty(t, getT(t, s, pooled).After)

	entries, err := s.ListLog(ctx, last)
	require.NoError(t, err)
	var texts []string
	for _, e := range entries {
		texts = append(texts, e.Text)
	}
	require.Equal(t, []string{"routed", "routed"}, texts[:2], "the refusals logged nothing")
	require.Len(t, texts, 3) // and the fix order's creation
}

// TaskWorkflowID and ParseTaskID agree, and an id of another shape names
// itself in CampaignOrderText.
func TestTaskIDs(t *testing.T) {
	t.Parallel()
	id := TaskWorkflowID("C-0001", "o/r#12")
	require.Equal(t, "campaign/C-0001/task/o/r#12", id)
	c, ref, ok := ParseTaskID(id)
	require.True(t, ok)
	require.Equal(t, []string{"C-0001", "o/r#12"}, []string{c, ref})
	for _, bad := range []string{"", "campaign/", "campaign/C-0001", "campaign//task/x", "campaign/C-0001/task/", "other/C/task/x"} {
		_, _, ok := ParseTaskID(bad)
		require.False(t, ok, bad)
	}
	require.Equal(t, "ORD-0001 is the work on task t", CampaignOrderText(Order{ID: "ORD-0001", Task: "t"}))
	require.Equal(t, "ORD-0002 is campaign C-0001's review of o/r#12",
		CampaignOrderText(Order{ID: "ORD-0002", Task: id, Kind: OrderKindReview}))
}

// OpenTaskHeads lists the head of each task that has one and hasn't landed
// or been dropped, with its campaign's repo.
func TestOpenTaskHeads(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s := openTest(t)
	for i, state := range []string{TaskReviewing, TaskStalled, TaskLanded, TaskDropped, TaskWorking} {
		id := fmt.Sprintf("C-%04d", i+1)
		ref := fmt.Sprintf("o/r#%d", i+1)
		insertCampaign(t, s, id, ref)
		head := fmt.Sprintf("ORD-%04d", i+1)
		if i == 4 {
			head = "" // no head yet
		}
		_, err := s.db.ExecContext(ctx, `UPDATE campaign_tasks SET state = ?, head = ? WHERE campaign_id = ?`, state, head, id)
		require.NoError(t, err)
	}
	heads, err := s.OpenTaskHeads(ctx)
	require.NoError(t, err)
	require.Equal(t, []TaskHead{{Ref: "o/r#1", Repo: "app", Head: "ORD-0001"}, {Ref: "o/r#2", Repo: "app", Head: "ORD-0002"}}, heads)

	// The open work order of a task counts as a head between its end and
	// check-head (#184); an open review doesn't, nor a dropped task's.
	work, err := s.InsertOrder(ctx, Order{Title: "w", Division: "eng", State: OrderDone, Task: "campaign/C-0005/task/o/r#5", Round: 1},
		LogEntry{Kind: "order"})
	require.NoError(t, err)
	review, err := s.InsertOrder(ctx, Order{Title: "r", Division: "science", Kind: OrderKindReview, Task: "campaign/C-0001/task/o/r#1",
		Round: 1, ReviewBase: testBase}, LogEntry{Kind: "order"})
	require.NoError(t, err)
	for _, u := range []struct{ id, order string }{{"C-0005", work}, {"C-0001", review}, {"C-0004", work}} {
		_, err := s.db.ExecContext(ctx, `UPDATE campaign_tasks SET open_order = ? WHERE campaign_id = ?`, u.order, u.id)
		require.NoError(t, err)
	}
	heads, err = s.OpenTaskHeads(ctx)
	require.NoError(t, err)
	require.Equal(t, "ORD-0001", work)
	require.Equal(t, []TaskHead{{Ref: "o/r#1", Repo: "app", Head: "ORD-0001"}, {Ref: "o/r#5", Repo: "app", Head: work},
		{Ref: "o/r#2", Repo: "app", Head: "ORD-0002"}}, heads)
}

// The campaign reads and writes of #184: a campaign and its tasks,
// OpenCampaigns leaving out an ended one, EndCampaign once, SetTask with
// no text writing the row alone, TaskOrderIDs by kind, and
// AddVerdictFindings writing each key once and its entries only with a
// row.
func TestCampaignWrites(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s := openTest(t)
	insertCampaign(t, s, "C-0001", "o/r#12")
	insertCampaign(t, s, "C-0002", "")
	_, err := s.db.ExecContext(ctx, `UPDATE campaigns SET state = ? WHERE id = 'C-0002'`, CampaignEnded)
	require.NoError(t, err)
	cs, err := s.OpenCampaigns(ctx)
	require.NoError(t, err)
	require.Len(t, cs, 1)
	require.Equal(t, []any{"C-0001", CampaignRunning, "app", "main", "science", 3}, []any{cs[0].ID, cs[0].State, cs[0].Repo,
		cs[0].BaseRef, cs[0].ReviewDivision, cs[0].MaxRounds})
	tasks, err := s.CampaignTasks(ctx, "C-0001")
	require.NoError(t, err)
	require.Len(t, tasks, 1)
	require.Equal(t, "o/r#12", tasks[0].Ref)

	last, err := s.LastLogID(ctx)
	require.NoError(t, err)
	next := tasks[0]
	next.OpenHail = "H-0001"
	wrote, err := s.SetTask(ctx, next, LogEntry{})
	require.NoError(t, err)
	require.True(t, wrote)
	got, err := s.GetTask(ctx, "C-0001", "o/r#12")
	require.NoError(t, err)
	require.Equal(t, "H-0001", got.OpenHail)
	entries, err := s.ListLog(ctx, last)
	require.NoError(t, err)
	require.Empty(t, entries, "no text, no entry")

	for i := range 2 {
		wrote, err := s.EndCampaign(ctx, "C-0001", LogEntry{Text: "campaign C-0001 ended: o/r#12 landed"})
		require.NoError(t, err)
		require.Equal(t, i == 0, wrote)
	}
	c, err := s.GetCampaign(ctx, "C-0001")
	require.NoError(t, err)
	require.Equal(t, CampaignEnded, c.State)
	require.NotNil(t, c.EndedAt)
	entries, err = s.ListLog(ctx, last)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.Equal(t, LogKindCampaign, entries[0].Kind)
	require.JSONEq(t, `{"campaign":"C-0001"}`, string(entries[0].Payload))
	_, err = s.EndCampaign(ctx, "C-0009", LogEntry{Text: "x"})
	require.ErrorIs(t, err, sql.ErrNoRows)

	task := "campaign/C-0001/task/o/r#12"
	work, err := s.InsertOrder(ctx, Order{Title: "w", Division: "eng", Task: task, Round: 1}, LogEntry{Kind: "order"})
	require.NoError(t, err)
	review, err := s.InsertOrder(ctx, Order{Title: "r", Division: "science", Kind: OrderKindReview, Task: task, Round: 1,
		ReviewBase: testBase}, LogEntry{Kind: "order"})
	require.NoError(t, err)
	ids, err := s.TaskOrderIDs(ctx, task, "")
	require.NoError(t, err)
	require.Equal(t, []string{work, review}, ids)
	ids, err = s.TaskOrderIDs(ctx, task, OrderKindReview)
	require.NoError(t, err)
	require.Equal(t, []string{review}, ids)

	last, err = s.LastLogID(ctx)
	require.NoError(t, err)
	fs := []Finding{{Key: VerdictFindingKey(1), Title: "Leak", Body: "x.go:3\n\nbody", Severity: "medium"},
		{Key: VerdictFindingKey(4), Title: "Later", Severity: "low"}}
	for i := range 2 {
		n, err := s.AddVerdictFindings(ctx, review, "tuvok", fs, LogEntry{Kind: "order", Text: "verdict findings"})
		require.NoError(t, err)
		require.Equal(t, 2*(1-i), n)
	}
	rows, err := s.OrderFindings(ctx, review)
	require.NoError(t, err)
	require.Len(t, rows, 2)
	require.Equal(t, []any{"verdict.1", "tuvok", "Leak", "medium", FindingNew}, []any{rows[0].Key, rows[0].CrewID, rows[0].Title,
		rows[0].Severity, rows[0].State})
	entries, err = s.ListLog(ctx, last)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.Equal(t, review, entries[0].OrderID)
}

// InsertCampaign (#186) inserts the campaign and its task under the next
// id, with one entry, and refuses an id that isn't the next and a ref in
// another open task, writing nothing. A ref whose task was dropped may
// start again. BelayCampaign sets a running campaign belaying once and
// refuses an ended one.
func TestInsertAndBelayCampaign(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s := openTest(t)
	id, err := s.NextCampaignID(ctx)
	require.NoError(t, err)
	require.Equal(t, "C-0001", id)
	c := Campaign{ID: id, Land: "push", Repo: "app", BaseRef: "main", Assignee: "sorell", ReviewDivision: "science", MaxRounds: 2}
	task := CampaignTask{Ref: "o/r#12", MaxRounds: 2, Branch: "campaign/C-0001/o-r-12"}
	require.NoError(t, s.InsertCampaign(ctx, c, task, LogEntry{Text: "campaign C-0001 started"}))
	got, err := s.GetCampaign(ctx, id)
	require.NoError(t, err)
	require.Equal(t, []any{CampaignRunning, "push", "app", "main", "", "sorell", "science", 2},
		[]any{got.State, got.Land, got.Repo, got.BaseRef, got.Division, got.Assignee, got.ReviewDivision, got.MaxRounds})
	tk, err := s.GetTask(ctx, id, "o/r#12")
	require.NoError(t, err)
	require.Equal(t, []any{TaskWorkflowID(id, "o/r#12"), TaskWorking, 1, 2, "campaign/C-0001/o-r-12"},
		[]any{tk.WorkflowID, tk.State, tk.Round, tk.MaxRounds, tk.Branch})
	entries, err := s.ListLog(ctx, 0)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.Equal(t, LogKindCampaign, entries[0].Kind)
	require.JSONEq(t, `{"campaign":"C-0001","ref":"o/r#12"}`, string(entries[0].Payload))

	open, err := s.OpenTaskOf(ctx, "o/r#12")
	require.NoError(t, err)
	require.Equal(t, "C-0001", open)
	c.ID = "C-0002"
	var ote *OpenTaskError
	require.ErrorAs(t, s.InsertCampaign(ctx, c, task, LogEntry{Text: "again"}), &ote)
	require.Equal(t, "o/r#12 is already in campaign C-0001's open task", ote.Error())
	c.ID = "C-0005"
	require.ErrorIs(t, s.InsertCampaign(ctx, c, CampaignTask{Ref: "o/r#13", MaxRounds: 2, Branch: "b"}, LogEntry{Text: "x"}),
		ErrCampaignID)
	all, err := s.ListCampaigns(ctx)
	require.NoError(t, err)
	require.Len(t, all, 1)
	entries, err = s.ListLog(ctx, 0)
	require.NoError(t, err)
	require.Len(t, entries, 1)

	wrote, err := s.BelayCampaign(ctx, id, LogEntry{Text: "belayed"})
	require.NoError(t, err)
	require.True(t, wrote)
	wrote, err = s.BelayCampaign(ctx, id, LogEntry{Text: "belayed again"})
	require.NoError(t, err)
	require.False(t, wrote)
	got, err = s.GetCampaign(ctx, id)
	require.NoError(t, err)
	require.Equal(t, CampaignBelaying, got.State)
	tk.State = TaskDropped
	_, err = s.SetTask(ctx, tk, LogEntry{})
	require.NoError(t, err)
	_, err = s.EndCampaign(ctx, id, LogEntry{Text: "ended"})
	require.NoError(t, err)
	_, err = s.BelayCampaign(ctx, id, LogEntry{Text: "too late"})
	require.ErrorIs(t, err, ErrCampaignEnded)
	_, err = s.BelayCampaign(ctx, "C-0009", LogEntry{Text: "none"})
	require.ErrorIs(t, err, sql.ErrNoRows)

	// The dropped task's ref may start again, under the next id.
	c.ID = "C-0002"
	require.NoError(t, s.InsertCampaign(ctx, c, task, LogEntry{Text: "campaign C-0002 started"}))
	all, err = s.ListCampaigns(ctx)
	require.NoError(t, err)
	require.Equal(t, []string{"C-0001", "C-0002"}, []string{all[0].ID, all[1].ID})
}
