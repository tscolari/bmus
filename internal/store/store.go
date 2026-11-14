// Package store holds the bridge's domain tables (crew, beams, orders, hails,
// checkpoints, log, inbox, settings) in the same SQLite file DBOS uses for
// workflow state.
package store

import (
	"cmp"
	"context"
	"crypto/rand"
	"database/sql"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"modernc.org/sqlite" // also registers the "sqlite" driver
	sqlite3 "modernc.org/sqlite/lib"
)

//go:embed schema.sql
var schema string

// Crew statuses.
const (
	// CrewBeamingIn: the crew member's tour is provisioning its beam, or
	// waiting for its combadge's hello (#106). It claims no order until
	// the hello moves it on_deck. current_beam is empty until serve is up.
	CrewBeamingIn = "beaming_in"
	CrewOnDeck    = "on_deck"
	CrewAway      = "away"
	CrewHailing   = "hailing"
	CrewSickbay   = "sickbay"
	CrewDismissed = "dismissed"
)

// Order states.
const (
	OrderPool      = "pool"
	OrderDirected  = "directed"
	OrderAway      = "away"
	OrderHailing   = "hailing"
	OrderDone      = "done"
	OrderFailed    = "failed"
	OrderBelayed   = "belayed"
	OrderWithdrawn = "withdrawn"
	// OrderSplit: the order's mission ended with child orders still open
	// (#155). Not terminal: the store settles it done or failed once its
	// last child ends (settleSplit).
	OrderSplit = "split"
)

// endedStates are the order states a child order counts as ended in: a
// split parent waits for every child to reach one (settleSplit). Recall
// moves a child back to pool, still open.
var endedStates = []string{OrderDone, OrderFailed, OrderBelayed, OrderWithdrawn}

// endedSQL is endedStates as an SQL list.
const endedSQL = `('done', 'failed', 'belayed', 'withdrawn')`

// CodeChildrenFailed is the code in the fail entry of a split order that
// settled failed: some child didn't end done (settleSplit).
const CodeChildrenFailed = "children_failed"

// Order priorities (orders.priority). Claims take the highest first, then
// the oldest (spec phase-2-plan section 3); an open red order sets the
// bridge's condition red (section 12).
const (
	PriorityRoutine = 0
	PriorityYellow  = 1
	PriorityRed     = 2
)

// ErrCountermandExpired is returned when countermanding a hail that has no
// countermand window open: not decided, decided by the captain or an
// auto_allow rule (no countermand_until), or past countermand_until.
var ErrCountermandExpired = errors.New("countermand window closed")

// ErrHailClosed is returned when deciding or timing out a hail that was
// already closed in a different way.
var ErrHailClosed = errors.New("hail already closed")

// Hail statuses.
const (
	HailOpen     = "open"
	HailDecided  = "decided"
	HailTimedOut = "timed_out"
)

// Store is the bridge's domain store. It is safe for concurrent use.
type Store struct {
	db *sql.DB
	// nonce is the database's own random id (Nonce).
	nonce string

	// logHook and logHookErr are SetLogHook's, and logHookLast the last
	// log id handed to logHook. hookMu serialises publishLog. appends
	// counts appendLog's inserts, rolled back ones included: withTx reads
	// the log after a commit only if it moved.
	hookMu      sync.Mutex
	logHook     func([]LogEntry)
	logHookErr  func(error)
	logHookLast int64
	appends     atomic.Int64

	// busyRetry bounds how long begin retries a BEGIN that failed with
	// SQLITE_BUSY (#204). onBusy, if set, is called before each retry
	// (tests).
	busyRetry time.Duration
	onBusy    func()

	// writeSem, one slot, queues the store's own write transactions
	// (#204).
	writeSem chan struct{}
}

// Lock waits (#204). busyTimeoutMS is the store connections'
// busy_timeout. SQLite's busy handler spends it polling the write lock,
// without a queue, so a steady stream of other writers can starve it.
// busyRetryFor bounds the retries of a BEGIN that still got SQLITE_BUSY.
const (
	busyTimeoutMS = 5000
	busyRetryFor  = 30 * time.Second
)

// File names under the bridge directory (see DefaultDir).
const (
	DBFile   = "bridge.db"
	LockFile = "bridge.lock"
)

// DefaultDir returns ~/.bmus, where the bridge keeps its database and lock.
func DefaultDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".bmus"), nil
}

// FileURI returns path as an absolute SQLite "file:" URI with every special
// character percent-encoded, so a "?", "#" or "%" in the path reaches SQLite
// intact instead of being read as the start of a query string, a fragment or
// an escape. Both this store and DBOS (as "sqlite:" + FileURI) open
// bridge.db through it.
func FileURI(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	return (&url.URL{Scheme: "file", Path: filepath.ToSlash(abs)}).String(), nil
}

// Open opens (creating if needed) the SQLite database at path and applies the
// schema. path is a plain file path, not DBOS's "sqlite:" URL.
func Open(ctx context.Context, path string) (*Store, error) {
	return open(ctx, path, busyTimeoutMS)
}

// open is Open with the connections' busy_timeout in milliseconds.
func open(ctx context.Context, path string, busyMS int) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	uri, err := FileURI(path)
	if err != nil {
		return nil, err
	}
	q := url.Values{
		"_pragma": {fmt.Sprintf("busy_timeout(%d)", busyMS), "journal_mode(WAL)", "foreign_keys(1)"},
		// BEGIN IMMEDIATE, so the COUNT(*)+1 id generation in insertWithID
		// holds the write lock from its first read.
		"_txlock": {"immediate"},
		// Store time.Time as "2006-01-02 15:04:05.999999999-07:00", which
		// SQLite's date functions understand.
		"_time_format": {"sqlite"},
	}
	db, err := sql.Open("sqlite", uri+"?"+q.Encode())
	if err != nil {
		return nil, err
	}
	if _, err := db.ExecContext(ctx, schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	for _, c := range addedColumns {
		if err := addColumn(ctx, db, c.table, c.column, c.def); err != nil {
			db.Close()
			return nil, fmt.Errorf("apply schema: %w", err)
		}
	}
	// Columns ADD COLUMN can't (or shouldn't) add to an older database:
	// fail here rather than have every write that needs them fail later.
	for _, c := range requiredColumns {
		var n int
		if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = ?`, c.table, c.column).Scan(&n); err != nil {
			db.Close()
			return nil, fmt.Errorf("apply schema: %w", err)
		}
		if n == 0 {
			db.Close()
			return nil, fmt.Errorf("%s predates %s; delete %s and start again (pre-release databases are disposable)",
				filepath.Base(path), c.what, path)
		}
	}
	nonce, err := ensureNonce(ctx, db)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	return &Store{db: db, nonce: nonce, busyRetry: busyRetryFor, writeSem: make(chan struct{}, 1)}, nil
}

// nonceSetting is the settings row holding the database's nonce.
const nonceSetting = "nonce"

// ensureNonce is the database's nonce, made the first time Open sees the
// database: 16 random hex digits, kept in the settings table. Two
// processes opening it at once agree, as the first insert wins.
func ensureNonce(ctx context.Context, db *sql.DB) (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO settings (key, value) VALUES (?, ?) ON CONFLICT (key) DO NOTHING`,
		nonceSetting, hex.EncodeToString(b)); err != nil {
		return "", err
	}
	var nonce string
	err := db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, nonceSetting).Scan(&nonce)
	return nonce, err
}

// Nonce is this database's random id, made when it was created and kept
// across restarts. A new database gets a new one: the tracker's
// idempotency keys carry it (#206), so a hail id a fresh database reuses
// never matches a marker an older one wrote.
func (s *Store) Nonce() string { return s.nonce }

// addedColumns are the columns schema.sql gained after its tables were
// first created. CREATE TABLE IF NOT EXISTS leaves an existing table as it
// is, so Open adds each one an older database lacks. A column added this
// way must be nullable or have a constant default, as SQLite's ALTER TABLE
// ADD COLUMN requires, and its definition must match schema.sql's.
var addedColumns = []struct{ table, column, def string }{
	{"crew", "model", `TEXT NOT NULL DEFAULT ''`},   // #58
	{"orders", "model", `TEXT NOT NULL DEFAULT ''`}, // #58
	{"orders", "parked_at", `TIMESTAMP`},            // #100
	{"orders", "progress_pct", `REAL`},              // #141
	{"orders", "progress_note", `TEXT NOT NULL DEFAULT ''`},
	{"orders", "progress_tour", `INTEGER NOT NULL DEFAULT 0`},
	{"orders", "progress_seq", `INTEGER NOT NULL DEFAULT 0`},
	{"orders", "source", `TEXT NOT NULL DEFAULT ''`}, // #54
	{"hails", "created_at", `TIMESTAMP`},             // #147
	{"hails", "decided_at", `TIMESTAMP`},
	{"hails", "countermanded_at", `TIMESTAMP`},
	{"hails", "original_decided_by", `TEXT NOT NULL DEFAULT ''`},
	{"hails", "countermand_option", `INTEGER`}, // #54
	{"beams", "removed_at", `TIMESTAMP`},       // #111
	{"beams", "labels", `TEXT NOT NULL DEFAULT '{}'`},
	{"orders", "budget_turns", `INTEGER NOT NULL DEFAULT 0`},
	{"orders", "budget_extended", `INTEGER NOT NULL DEFAULT 0`},
}

// requiredColumns are columns schema.sql gained that Open doesn't add to an
// older database, which it refuses instead.
var requiredColumns = []struct{ table, column, what string }{
	// Part of the inbox's key, so ADD COLUMN can't add it.
	{"inbox", "tour_no", "the tour-scoped inbox (#22)"},
	// Which tour created each beam: an older database's beams would all
	// read as tour 0's.
	{"beams", "tour_no", "the tour-numbered beams (#97)"},
	// Part of the checkpoints' keys.
	{"checkpoints", "beam", "the beam-keyed checkpoints (#100)"},
	{"checkpoint_repos", "beam", "the beam-keyed checkpoints (#100)"},
}

// addColumn adds column to table unless it is already there. Two processes
// opening an older database at once may both try; the loser's "duplicate
// column" error is ignored once the column is seen to exist.
func addColumn(ctx context.Context, db *sql.DB, table, column, def string) error {
	has := func() (bool, error) {
		var n int
		err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = ?`, table, column).Scan(&n)
		return n > 0, err
	}
	ok, err := has()
	if err != nil || ok {
		return err
	}
	if _, err := db.ExecContext(ctx, `ALTER TABLE `+table+` ADD COLUMN `+column+` `+def); err != nil {
		if ok, herr := has(); herr == nil && ok {
			return nil
		}
		return fmt.Errorf("add %s.%s: %w", table, column, err)
	}
	return nil
}

// Close closes the underlying database.
func (s *Store) Close() error { return s.db.Close() }

// Crew is a row of the crew table.
type Crew struct {
	ID          string
	Name        string
	Rank        string
	Division    string
	Spec        string
	Status      string
	CurrentBeam string
	TourNo      int
	DossierPath string
	Harness     string
	// Model is the model the crew member's orders run on unless an order
	// names its own. Empty means the harness's default.
	Model string
}

const crewCols = `id, name, rank, division, spec, status, current_beam, tour_no, dossier_path, harness, model`

func scanCrew(row interface{ Scan(...any) error }) (Crew, error) {
	var c Crew
	err := row.Scan(&c.ID, &c.Name, &c.Rank, &c.Division, &c.Spec, &c.Status, &c.CurrentBeam, &c.TourNo, &c.DossierPath, &c.Harness, &c.Model)
	return c, err
}

// InsertCrew adds a crew member and appends entry to the captain's log, in
// one transaction. An empty Status defaults to on_deck. An empty Harness
// is stored as is: unset, serve's default (sdk) unless a division's applies.
func (s *Store) InsertCrew(ctx context.Context, c Crew, entry LogEntry) error {
	return s.withTx(ctx, func(tx *sql.Tx) error { return s.insertCrew(ctx, tx, c, entry) })
}

// ErrRosterFull is returned by InsertCrewWithin when the crew members not
// dismissed already reach its limit.
var ErrRosterFull = errors.New("roster full")

// InsertCrewWithin is InsertCrew, refusing with ErrRosterFull (wrapped) when
// limit or more crew members aren't dismissed. The count and the insert
// share one transaction, so concurrent calls can't both take the last slot.
func (s *Store) InsertCrewWithin(ctx context.Context, c Crew, limit int, entry LogEntry) error {
	return s.withTx(ctx, func(tx *sql.Tx) error {
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM crew WHERE status <> ?`, CrewDismissed).Scan(&n); err != nil {
			return err
		}
		if n >= limit {
			return fmt.Errorf("%w: %d crew members not dismissed, limit %d", ErrRosterFull, n, limit)
		}
		return s.insertCrew(ctx, tx, c, entry)
	})
}

func (s *Store) insertCrew(ctx context.Context, tx *sql.Tx, c Crew, entry LogEntry) error {
	if c.Status == "" {
		c.Status = CrewOnDeck
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO crew (`+crewCols+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		c.ID, c.Name, c.Rank, c.Division, c.Spec, c.Status, c.CurrentBeam, c.TourNo, c.DossierPath, c.Harness, c.Model)
	if err != nil {
		return err
	}
	_, err = s.appendLog(ctx, tx, entry)
	return err
}

// GetCrew returns the crew member with the given id, or sql.ErrNoRows.
func (s *Store) GetCrew(ctx context.Context, id string) (Crew, error) {
	return scanCrew(s.db.QueryRowContext(ctx, `SELECT `+crewCols+` FROM crew WHERE id = ?`, id))
}

// CrewDivision returns crew member id's division, read in tx, or
// sql.ErrNoRows if there is no such crew member.
func CrewDivision(ctx context.Context, tx *sql.Tx, id string) (string, error) {
	var division string
	err := tx.QueryRowContext(ctx, `SELECT division FROM crew WHERE id = ?`, id).Scan(&division)
	return division, err
}

// HailRoute returns hail id's route, read in tx, or sql.ErrNoRows if there
// is no such hail.
func HailRoute(ctx context.Context, tx *sql.Tx, id string) (string, error) {
	var route string
	err := tx.QueryRowContext(ctx, `SELECT route FROM hails WHERE id = ?`, id).Scan(&route)
	return route, err
}

// ListCrew returns every crew member ordered by id.
func (s *Store) ListCrew(ctx context.Context) ([]Crew, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+crewCols+` FROM crew ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Crew
	for rows.Next() {
		c, err := scanCrew(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// SetCrewStatus sets a crew member's status and current beam alias and
// appends entry to the captain's log, in one transaction. A crew member
// already in status on beam is left alone and nothing is logged, so a
// re-run of the step that set it logs nothing. dismissed is terminal: a
// dismissed crew member is left alone too (#100). A missing crew member
// returns sql.ErrNoRows.
func (s *Store) SetCrewStatus(ctx context.Context, id, status, beam string, entry LogEntry) error {
	return s.updateCrew(ctx, id, entry, `UPDATE crew SET status = ?, current_beam = ?
		WHERE id = ? AND status <> ? AND (status <> ? OR current_beam <> ?)`,
		status, beam, id, CrewDismissed, status, beam)
}

// UpdateCrewStatusFrom sets a crew member's status to status, leaving its
// current beam as is, and appends entry to the captain's log, in one
// transaction, only while its status is from: a crew member in any other
// status (dismissed or sickbay, say) is left alone and nothing is logged,
// so a re-run writes nothing. It reports whether it changed the row. A
// missing crew member returns sql.ErrNoRows.
func (s *Store) UpdateCrewStatusFrom(ctx context.Context, id, from, status string, entry LogEntry) (changed bool, err error) {
	return s.updateCrewChanged(ctx, id, entry, `UPDATE crew SET status = ? WHERE id = ? AND status = ?`, status, id, from)
}

// UpdateCrewStatus sets a crew member's status, leaving its current beam as
// is, and appends entry to the captain's log, in one transaction. A crew
// member already in status, or dismissed (terminal, #100), is left alone and
// nothing is logged. A missing crew member returns sql.ErrNoRows.
func (s *Store) UpdateCrewStatus(ctx context.Context, id, status string, entry LogEntry) error {
	return s.updateCrew(ctx, id, entry, `UPDATE crew SET status = ? WHERE id = ? AND status <> ? AND status <> ?`,
		status, id, status, CrewDismissed)
}

// updateCrew runs a conditional crew UPDATE and appends entry if it changed
// the row. If it touched nothing it returns sql.ErrNoRows for a missing crew
// member and nil otherwise.
func (s *Store) updateCrew(ctx context.Context, id string, entry LogEntry, query string, args ...any) error {
	_, err := s.updateCrewChanged(ctx, id, entry, query, args...)
	return err
}

// updateCrewChanged is updateCrew, also reporting whether it changed the
// row.
func (s *Store) updateCrewChanged(ctx context.Context, id string, entry LogEntry, query string, args ...any) (changed bool, err error) {
	err = s.withTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, query, args...)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			var current string
			return tx.QueryRowContext(ctx, `SELECT status FROM crew WHERE id = ?`, id).Scan(&current)
		}
		changed = true
		_, err = s.appendLog(ctx, tx, entry)
		return err
	})
	return changed, err
}

// Order is a row of the orders table.
type Order struct {
	ID           string
	Title        string
	Body         string
	Division     string
	Priority     int
	State        string
	Assignee     string
	ParentID     string
	Branch       string
	BudgetTokens int64
	// BudgetTurns is the order's own turn budget, 0 for none (#118);
	// BudgetExtended counts the extensions the captain granted (#111).
	BudgetTurns    int64
	BudgetExtended int64
	CreatedBy      string
	// Model is the model the order runs on. Empty means the assignee's.
	Model string
	// Source is where the order came from, free-form: "tracker:<ref>"
	// for a tracker item (#54). Empty for none.
	Source string
	// Repos are the repos the order works on, in order (the order_repos
	// table).
	Repos []OrderRepo
	// ParkedAt is set while the order's mission is parked, waiting for a
	// tour to resume it (ParkOrder); nil otherwise.
	ParkedAt *time.Time
	// ProgressPct and ProgressNote are the latest progress report of the
	// order's mission (InboxTx.SetOrderProgress); ProgressPct is nil
	// without one. Leaving away and hailing clears them; a parked mission
	// keeps its last report until the resuming tour reports.
	ProgressPct  *float64
	ProgressNote string
	// OpenChildren counts the order's child orders (parent_id) not yet
	// ended (endedStates), read with the order.
	OpenChildren int
	// After are the orders this order waits on (the order_after table,
	// #142), in their creation order: it is claimed only once each has
	// ended done. InsertOrder takes them; WaitingOn, read with the order,
	// are those not done yet.
	After     []string
	WaitingOn []string
	// AfterSiblings are, for InsertChildOrders only, the earlier children
	// of the same batch this child waits on, by index; the store adds
	// their ids to After.
	AfterSiblings []int
	// Used is the order's usage (order_usage), summed over its beams,
	// and AssigneeDivision its assignee's division, "" without one; both
	// read with the order (#120).
	Used             UsageTotal
	AssigneeDivision string
}

// UsageTotal is an order's usage summed over its beams (#120).
type UsageTotal struct {
	InputTokens  int64
	OutputTokens int64
	Turns        int64
}

// OrderRepo is a row of the order_repos table: a repo of the bridge's
// registry, by name, and the ref the work starts from.
type OrderRepo struct {
	Name    string
	BaseRef string
}

const orderCols = `id, title, body, division, priority, state, assignee, parent_id, branch, budget_tokens, budget_turns, budget_extended, created_by, model, source, parked_at, progress_pct, progress_note,
	(SELECT COUNT(*) FROM orders c WHERE c.parent_id = orders.id AND c.state NOT IN ` + endedSQL + `),
	(SELECT COALESCE(group_concat(a.after_id, ' ' ORDER BY p.rowid), '') FROM order_after a JOIN orders p ON p.id = a.after_id
		WHERE a.order_id = orders.id),
	(SELECT COALESCE(group_concat(a.after_id, ' ' ORDER BY p.rowid), '') FROM order_after a JOIN orders p ON p.id = a.after_id
		WHERE a.order_id = orders.id AND p.state <> 'done'),
	(SELECT COALESCE(SUM(u.input_tokens), 0) FROM order_usage u WHERE u.order_id = orders.id),
	(SELECT COALESCE(SUM(u.output_tokens), 0) FROM order_usage u WHERE u.order_id = orders.id),
	(SELECT COALESCE(SUM(u.turns), 0) FROM order_usage u WHERE u.order_id = orders.id),
	COALESCE((SELECT a.division FROM crew a WHERE a.id = orders.assignee), '')`

func scanOrder(row interface{ Scan(...any) error }) (Order, error) {
	var (
		o              Order
		parked         sql.NullTime
		pct            sql.NullFloat64
		after, waiting string
	)
	err := row.Scan(&o.ID, &o.Title, &o.Body, &o.Division, &o.Priority, &o.State, &o.Assignee, &o.ParentID, &o.Branch,
		&o.BudgetTokens, &o.BudgetTurns, &o.BudgetExtended, &o.CreatedBy, &o.Model, &o.Source, &parked, &pct, &o.ProgressNote, &o.OpenChildren,
		&after, &waiting, &o.Used.InputTokens, &o.Used.OutputTokens, &o.Used.Turns, &o.AssigneeDivision)
	o.After, o.WaitingOn = strings.Fields(after), strings.Fields(waiting)
	if parked.Valid {
		o.ParkedAt = &parked.Time
	}
	if pct.Valid {
		o.ProgressPct = &pct.Float64
	}
	return o, err
}

// clearProgress is the SET clause that clears an order's progress, for
// every update that moves an order out of away and hailing (#141): the
// next mission on it starts with none.
const clearProgress = `progress_pct = NULL, progress_note = '', progress_tour = 0, progress_seq = 0`

// orderRepos returns the repos of every order with one of ids (every order
// when ids is nil), by order id.
func (s *Store) orderRepos(ctx context.Context, ids []string) (map[string][]OrderRepo, error) {
	q := `SELECT order_id, name, base_ref FROM order_repos`
	args := make([]any, len(ids))
	if ids != nil {
		for i, id := range ids {
			args[i] = id
		}
		q += ` WHERE order_id IN (` + strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",") + `)`
	}
	rows, err := s.db.QueryContext(ctx, q+` ORDER BY order_id, pos`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]OrderRepo{}
	for rows.Next() {
		var id string
		var r OrderRepo
		if err := rows.Scan(&id, &r.Name, &r.BaseRef); err != nil {
			return nil, err
		}
		out[id] = append(out[id], r)
	}
	return out, rows.Err()
}

// InsertOrder creates an order and returns its generated id (ORD-0001, ...),
// and appends entry, with its OrderID set to that id, to the captain's log,
// in one transaction. o.ID and o.ParkedAt are ignored. An empty State
// defaults to directed when Assignee is set, pool otherwise. o.After are
// the orders it waits on (order_after, #142): an unknown order, the order
// itself, or one that ended failed, belayed or withdrawn is refused
// (ErrAfter, wrapped), and nothing is written.
func (s *Store) InsertOrder(ctx context.Context, o Order, entry LogEntry) (string, error) {
	var id string
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		var err error
		id, err = s.insertOrder(ctx, tx, o, entry)
		return err
	})
	if err != nil {
		return "", err
	}
	return id, nil
}

// insertOrder is InsertOrder's work, in tx.
func (s *Store) insertOrder(ctx context.Context, tx *sql.Tx, o Order, entry LogEntry) (string, error) {
	if o.State == "" {
		o.State = OrderPool
		if o.Assignee != "" {
			o.State = OrderDirected
		}
	}
	id, err := nextID(ctx, tx, "orders", "ORD-%04d")
	if err != nil {
		return "", err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO orders (id, title, body, division, priority, state, assignee, parent_id,
		branch, budget_tokens, budget_turns, created_by, model, source) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, o.Title, o.Body, o.Division, o.Priority, o.State, o.Assignee, o.ParentID, o.Branch, o.BudgetTokens, o.BudgetTurns,
		o.CreatedBy, o.Model, o.Source)
	if err != nil {
		return "", err
	}
	for i, r := range o.Repos {
		if _, err := tx.ExecContext(ctx, `INSERT INTO order_repos (order_id, pos, name, base_ref) VALUES (?, ?, ?, ?)`,
			id, i, r.Name, r.BaseRef); err != nil {
			return "", fmt.Errorf("repo %s: %w", r.Name, err)
		}
	}
	if err := insertAfter(ctx, tx, id, o.After); err != nil {
		return "", err
	}
	entry.OrderID = id
	_, err = s.appendLog(ctx, tx, entry)
	return id, err
}

// ErrAfter is returned, wrapped, by InsertOrder and InsertChildOrders for
// an after they refuse (#142): an order that doesn't exist, the order
// itself, or one that ended failed, belayed or withdrawn and so never
// will be done.
var ErrAfter = errors.New("bad after")

// insertAfter records, in tx, that order id waits on each of after
// (order_after), refusing any ErrAfter names. A repeated id counts once.
// An order already done is accepted: it is satisfied at once.
func insertAfter(ctx context.Context, tx *sql.Tx, id string, after []string) error {
	for _, a := range after {
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
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO order_after (order_id, after_id) VALUES (?, ?)`, id, a); err != nil {
			return err
		}
	}
	return nil
}

// ErrParentEnded is returned by InsertChildOrders when the parent order is
// no longer on its mission (away or hailing).
var ErrParentEnded = errors.New("parent order no longer away")

// InsertChildOrders creates children, child orders of order parent (#155),
// in one transaction: each with InsertOrder's defaults, its ParentID set to
// parent and its CreatedBy to createdBy, each logged "order created:
// <title>" (kind order), then entry(ids), the split's own entry, appended
// for the parent. It returns the children's ids, in children's order.
// A child's AfterSiblings, indexes of earlier children, join its After as
// those children's ids (#142); a later or its own index is ErrAfter.
//
// createdBy marks the batch: when the parent already has children created
// by it, their ids are returned and nothing is written, so a re-run of the
// step that made them makes no more. The parent must be away or hailing
// (ErrParentEnded, wrapped, otherwise); a missing parent returns
// sql.ErrNoRows.
func (s *Store) InsertChildOrders(ctx context.Context, parent, createdBy string, children []Order,
	entry func(ids []string) (LogEntry, error)) (ids []string, err error) {
	if createdBy == "" {
		return nil, errors.New("store: InsertChildOrders needs createdBy")
	}
	err = s.withTx(ctx, func(tx *sql.Tx) error {
		var err error
		if ids, err = childBatch(ctx, tx, parent, createdBy); err != nil || len(ids) > 0 {
			return err
		}
		var state string
		if err := tx.QueryRowContext(ctx, `SELECT state FROM orders WHERE id = ?`, parent).Scan(&state); err != nil {
			return err
		}
		if state != OrderAway && state != OrderHailing {
			return fmt.Errorf("order %s is %s: %w", parent, state, ErrParentEnded)
		}
		for i, c := range children {
			c.ParentID, c.CreatedBy = parent, createdBy
			c.After = slices.Clone(c.After)
			for _, j := range c.AfterSiblings {
				if j < 0 || j >= i {
					return fmt.Errorf("child %d: after %d: a child waits on earlier children only: %w", i, j, ErrAfter)
				}
				c.After = append(c.After, ids[j])
			}
			payload, err := withField(nil, "parent_id", parent)
			if err != nil {
				return err
			}
			id, err := s.insertOrder(ctx, tx, c, LogEntry{Kind: logKindOrder, Text: "order created: " + c.Title, Payload: payload})
			if err != nil {
				return err
			}
			ids = append(ids, id)
		}
		e, err := entry(ids)
		if err != nil {
			return err
		}
		e.OrderID = parent
		_, err = s.appendLog(ctx, tx, e)
		return err
	})
	if err != nil {
		return nil, err
	}
	return ids, nil
}

// ChildBatch returns the ids of order parent's children created by
// createdBy (InsertChildOrders), in creation order; none when there are
// none.
func (s *Store) ChildBatch(ctx context.Context, parent, createdBy string) ([]string, error) {
	var ids []string
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		var err error
		ids, err = childBatch(ctx, tx, parent, createdBy)
		return err
	})
	return ids, err
}

// childBatch is ChildBatch in tx.
func childBatch(ctx context.Context, tx *sql.Tx, parent, createdBy string) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id FROM orders WHERE parent_id = ? AND created_by = ? ORDER BY rowid`, parent, createdBy)
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

// splitRefusedMark starts the note RefuseSplit adds to a hail's rationale.
const splitRefusedMark = "split refused: "

// ExtendBudget grants order orderID one more extension of its budget for
// budget hail hailID (#119): it records the extension in
// budget_extensions, keyed by the hail, increments the order's
// budget_extended and appends the entry entry builds from the order as
// extended, in one transaction. A hail already recorded writes nothing: a
// re-run returns the order as it stands. It returns the order (without its
// repos) and whether it wrote. A missing order returns sql.ErrNoRows.
func (s *Store) ExtendBudget(ctx context.Context, orderID, hailID string, entry func(Order) (LogEntry, error)) (o Order, wrote bool, err error) {
	err = s.withTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO budget_extensions (hail_id, order_id, granted_at) VALUES (?, ?, ?)`,
			hailID, orderID, time.Now().UTC())
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 1 {
			res, err := tx.ExecContext(ctx, `UPDATE orders SET budget_extended = budget_extended + 1 WHERE id = ?`, orderID)
			if err != nil {
				return err
			}
			if n, err := res.RowsAffected(); err != nil || n == 0 {
				return cmp.Or(err, sql.ErrNoRows)
			}
			wrote = true
		}
		if o, err = scanOrder(tx.QueryRowContext(ctx, `SELECT `+orderCols+` FROM orders WHERE id = ?`, orderID)); err != nil || !wrote {
			return err
		}
		e, err := entry(o)
		if err != nil {
			return err
		}
		_, err = s.appendLog(ctx, tx, e)
		return err
	})
	if err != nil {
		return Order{}, false, err
	}
	return o, wrote, nil
}

// SetBudgetFunc checks a budget change on order o, as read in the
// transaction, to tokens and turns, and returns the log entry to append
// (SetOrderBudget). An error aborts the change.
type SetBudgetFunc func(o Order, tokens, turns int64) (LogEntry, error)

// SetOrderBudget sets order id's own budget (#120): tokens and turns,
// each kept as it is when nil, 0 meaning none of its own. check runs on
// the order as read, with the new values, in the same transaction; the
// update and the entry it returns are written only when a value changes.
// budget_extended is kept. It returns the order (without its repos) and
// whether it wrote. A missing order returns sql.ErrNoRows.
func (s *Store) SetOrderBudget(ctx context.Context, id string, tokens, turns *int64, check SetBudgetFunc) (o Order, wrote bool, err error) {
	err = s.withTx(ctx, func(tx *sql.Tx) error {
		read := func() (err error) {
			o, err = scanOrder(tx.QueryRowContext(ctx, `SELECT `+orderCols+` FROM orders WHERE id = ?`, id))
			return err
		}
		if err := read(); err != nil {
			return err
		}
		tok, tur := o.BudgetTokens, o.BudgetTurns
		if tokens != nil {
			tok = *tokens
		}
		if turns != nil {
			tur = *turns
		}
		entry, err := check(o, tok, tur)
		if err != nil {
			return err
		}
		if tok == o.BudgetTokens && tur == o.BudgetTurns {
			return nil
		}
		if _, err := tx.ExecContext(ctx, `UPDATE orders SET budget_tokens = ?, budget_turns = ? WHERE id = ?`,
			tok, tur, id); err != nil {
			return err
		}
		wrote = true
		if _, err := s.appendLog(ctx, tx, entry); err != nil {
			return err
		}
		return read()
	})
	if err != nil {
		return Order{}, false, err
	}
	return o, wrote, nil
}

// RefuseSplit records on decided hail id that its split created no child
// orders (#155): it appends "split refused: <why>" to the hail's rationale
// ("; " after one already there) and appends entry, in one transaction,
// only while the rationale carries no such note yet. A re-run writes
// nothing. It reports whether it wrote. A missing hail returns
// sql.ErrNoRows.
func (s *Store) RefuseSplit(ctx context.Context, id, why string, entry LogEntry) (bool, error) {
	return s.NoteRefusal(ctx, id, splitRefusedMark, why, entry)
}

// NoteRefusal records on decided hail id that the bridge didn't carry out
// what it allowed: it appends mark and why to the hail's rationale ("; "
// after one already there) and appends entry, in one transaction, only
// while the rationale carries no mark yet. A re-run writes nothing. It
// reports whether it wrote. A missing hail returns sql.ErrNoRows.
func (s *Store) NoteRefusal(ctx context.Context, id, mark, why string, entry LogEntry) (bool, error) {
	var wrote bool
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		var rationale string
		if err := tx.QueryRowContext(ctx, `SELECT rationale FROM hails WHERE id = ?`, id).Scan(&rationale); err != nil {
			return err
		}
		if strings.Contains(rationale, mark) {
			return nil
		}
		note := mark + why
		if rationale != "" {
			note = rationale + "; " + note
		}
		if _, err := tx.ExecContext(ctx, `UPDATE hails SET rationale = ? WHERE id = ?`, note, id); err != nil {
			return err
		}
		wrote = true
		_, err := s.appendLog(ctx, tx, entry)
		return err
	})
	return wrote, err
}

// HasChildren reports whether order id has child orders.
func (s *Store) HasChildren(ctx context.Context, id string) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM orders WHERE parent_id = ?)`, id).Scan(&n)
	return n == 1, err
}

// GetOrder returns the order with the given id, or sql.ErrNoRows.
func (s *Store) GetOrder(ctx context.Context, id string) (Order, error) {
	o, err := scanOrder(s.db.QueryRowContext(ctx, `SELECT `+orderCols+` FROM orders WHERE id = ?`, id))
	if err != nil {
		return Order{}, err
	}
	repos, err := s.orderRepos(ctx, []string{id})
	if err != nil {
		return Order{}, err
	}
	o.Repos = repos[id]
	return o, nil
}

// ListOrders returns every order in creation order.
func (s *Store) ListOrders(ctx context.Context) ([]Order, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+orderCols+` FROM orders ORDER BY rowid`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Order
	for rows.Next() {
		o, err := scanOrder(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close() // before the next query takes a connection
	repos, err := s.orderRepos(ctx, nil)
	if err != nil {
		return nil, err
	}
	for i := range out {
		out[i].Repos = repos[out[i].ID]
	}
	return out, nil
}

// ClaimOldestDirectedOrder returns the order crewID should be working on. A
// crew member runs one mission at a time, so if it already has an away order
// that order is returned unchanged; otherwise the highest-priority directed
// order assigned to it, the oldest of those, is moved to away and returned;
// one waiting on an order in its after not yet done is skipped (#142).
// This makes a re-run of the
// claim (e.g. a replayed DBOS step) return the same order. found is false when
// there is nothing to work on, or the crew member is dismissed or beaming
// in (#106).
//
// A claim that moves an order also sets the crew member away and appends
// entry, with its OrderID set to the order's id, to the captain's log, all
// in one transaction. Returning the order already away sets the crew member
// away only if it is on_deck (a new tour taking up its parked order, #100),
// appending entry only then, so a re-run writes nothing.
//
// Callers must move the order out of away (SettleOrder) on every mission
// exit, or every later claim keeps returning it.
func (s *Store) ClaimOldestDirectedOrder(ctx context.Context, crewID string, entry LogEntry) (id string, found bool, err error) {
	err = s.withTx(ctx, func(tx *sql.Tx) error {
		var status string
		if err := tx.QueryRowContext(ctx, `SELECT status FROM crew WHERE id = ?`, crewID).Scan(&status); err != nil &&
			!errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if status == CrewDismissed || status == CrewBeamingIn {
			// A dismissed crew member takes nothing (#100), nor does one
			// beaming in: its combadge hasn't said hello (#106).
			return sql.ErrNoRows
		}
		err := tx.QueryRowContext(ctx, `SELECT id FROM orders WHERE assignee = ? AND state = ? `+claimOrderBy+` LIMIT 1`,
			crewID, OrderAway).Scan(&id)
		if err == nil {
			res, err := tx.ExecContext(ctx, `UPDATE crew SET status = ? WHERE id = ? AND status = ?`, CrewAway, crewID, CrewOnDeck)
			if err != nil {
				return err
			}
			if n, err := res.RowsAffected(); err != nil || n == 0 {
				return err
			}
			entry.OrderID = id
			_, err = s.appendLog(ctx, tx, entry)
			return err
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		err = tx.QueryRowContext(ctx, `
			UPDATE orders SET state = ?
			WHERE rowid = (SELECT o.rowid FROM orders o WHERE o.assignee = ? AND o.state = ? AND NOT `+waitingSQL+` `+claimOrderBy+` LIMIT 1)
			RETURNING id`, OrderAway, crewID, OrderDirected).Scan(&id)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE crew SET status = ? WHERE id = ?`, CrewAway, crewID); err != nil {
			return err
		}
		entry.OrderID = id
		_, err = s.appendLog(ctx, tx, entry)
		return err
	})
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return id, true, nil
}

// claimOrderBy is the order claims take orders in: the highest priority
// first, then the oldest.
const claimOrderBy = `ORDER BY priority DESC, rowid ASC`

// waitingSQL is true for order o, unclaimed, while it waits on an order in
// its after not yet done (#142): a split order counts as not done until
// it settles done. Both claims skip it.
const waitingSQL = `EXISTS (SELECT 1 FROM order_after a JOIN orders p ON p.id = a.after_id
	WHERE a.order_id = o.id AND p.state <> 'done')`

// ClaimOldestPoolOrder claims the highest-priority pool order of division,
// the oldest of those, for crewID (the standing-orders dispatcher, spec
// phase-2-plan section 4), skipping one waiting on an order in its after
// not yet done (#142): it sets the order's assignee to crewID and its
// state to away, sets the crew member away and appends entry, with its
// OrderID set to the order's id, all in one transaction. A claimed pool
// order is then indistinguishable from a claimed directed one, so the next
// ClaimOldestDirectedOrder returns it as the order already away. The
// order's parked_at is left as it is: a recalled order (#101) keeps its
// parked mission, which the claimer resumes.
//
// found is false when there is nothing to claim, the division is empty
// (an empty division matches nothing) or the crew member is dismissed or
// beaming in (#106). The caller claims directed orders first: this doesn't
// check that the crew member has no order away.
func (s *Store) ClaimOldestPoolOrder(ctx context.Context, crewID, division string, entry LogEntry) (id string, found bool, err error) {
	if division == "" {
		return "", false, nil
	}
	err = s.withTx(ctx, func(tx *sql.Tx) error {
		var status string
		if err := tx.QueryRowContext(ctx, `SELECT status FROM crew WHERE id = ?`, crewID).Scan(&status); err != nil {
			return err // a missing crew member claims nothing
		}
		if status == CrewDismissed || status == CrewBeamingIn {
			return sql.ErrNoRows
		}
		err := tx.QueryRowContext(ctx, `
			UPDATE orders SET state = ?, assignee = ?
			WHERE rowid = (SELECT o.rowid FROM orders o WHERE o.state = ? AND o.division = ? AND NOT `+waitingSQL+` `+claimOrderBy+` LIMIT 1)
			RETURNING id`, OrderAway, crewID, OrderPool, division).Scan(&id)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE crew SET status = ? WHERE id = ?`, CrewAway, crewID); err != nil {
			return err
		}
		entry.OrderID = id
		_, err = s.appendLog(ctx, tx, entry)
		return err
	})
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return id, true, nil
}

// SettleOrder ends order id as state (OrderDone or OrderFailed, or
// OrderSplit for an order with child orders) and appends entry, in one
// transaction, only while the order is still away or hailing, and clears
// its parked_at. It returns the order's state afterwards and whether this
// call moved it. The same transaction settles a split parent the order
// was its last open child of, and a split order whose children all ended
// already (settleFamily).
//
// An order already in state is left alone and nothing is logged: a re-run
// of the step that settled it, or a mission failing an order the tour's
// backstop already failed. An order that ended otherwise (say the backstop
// failed it while the mission was still pulling its work) is left alone
// too, and late, when not nil, is appended instead of entry, so the log
// keeps one terminal entry per order.
func (s *Store) SettleOrder(ctx context.Context, id, state string, entry LogEntry, late *LogEntry) (current string, settled bool, err error) {
	return s.settleOrder(ctx, id, "", false, state, entry, late)
}

// SettleAssignedOrder is SettleOrder with no late entry that also leaves
// the order alone unless assignee is its assignee and, unless evenParked,
// its mission isn't parked: a tour's backstop never settles an order a
// recall or another crew member's claim has taken from its crew member
// meanwhile, nor one whose mission is alive and parked (#100). evenParked
// is for a mission known to be over (#101 review): its parked order has no
// mission left to resume it.
func (s *Store) SettleAssignedOrder(ctx context.Context, id, assignee, state string, entry LogEntry, evenParked bool) (current string, settled bool, err error) {
	if assignee == "" {
		return "", false, errors.New("store: SettleAssignedOrder needs an assignee")
	}
	return s.settleOrder(ctx, id, assignee, evenParked, state, entry, nil)
}

// settleOrder is SettleOrder or, with a non-empty assignee,
// SettleAssignedOrder.
func (s *Store) settleOrder(ctx context.Context, id, assignee string, evenParked bool, state string, entry LogEntry, late *LogEntry) (current string, settled bool, err error) {
	err = s.withTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE orders SET state = ?, parked_at = NULL, `+clearProgress+`
			WHERE id = ? AND state IN (?, ?) AND (? = '' OR (assignee = ? AND (? OR parked_at IS NULL)))`,
			state, id, OrderAway, OrderHailing, assignee, assignee, evenParked)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n > 0 {
			current, settled = state, true
			if _, err := s.appendLog(ctx, tx, entry); err != nil {
				return err
			}
			return s.settleFamily(ctx, tx, id)
		}
		if err := tx.QueryRowContext(ctx, `SELECT state FROM orders WHERE id = ?`, id).Scan(&current); err != nil {
			return err
		}
		// A split order may have settled from split since, by its last
		// child: a re-run, not late work.
		if current == state || late == nil || (state == OrderSplit && (current == OrderDone || current == OrderFailed)) {
			return nil
		}
		_, err = s.appendLog(ctx, tx, *late)
		return err
	})
	if err != nil {
		return "", false, err
	}
	return current, settled, nil
}

// logKindOrder and logKindFail are the bridge's captain's log kinds for
// an order's events and failures, which settleSplit's entries use.
const (
	logKindOrder = "order"
	logKindFail  = "fail"
)

// settleFamily runs settleSplit, in tx, for what order id's own change
// may have settled: id itself when it is split (its children may all have
// ended already), its parent when id has ended. Every transaction that
// ends an order (settleOrder, SettleOpenOrder, WithdrawOrder) calls it
// right after its write, so a split parent settles in the transaction
// that ends its last open child (#155). A belayed parent is belayed
// before its cascade touches any child (cascadeBelay): settleSplit,
// which moves only a split order, leaves it alone.
//
// An order that ended failed, belayed or withdrawn withdraws the unclaimed
// orders waiting on it (cascadeAfter, #142).
//
// A belayed order's children were cascaded to (cascadeBelay, #146). An
// order that failed with child orders still open (its mission failed after
// create-children) leaves them running: it logs "order ended with N open
// child orders, which run on: ORD-0002, ..." (kind order, payload
// open_children).
func (s *Store) settleFamily(ctx context.Context, tx *sql.Tx, id string) error {
	var parent, state, assignee string
	if err := tx.QueryRowContext(ctx, `SELECT parent_id, state, assignee FROM orders WHERE id = ?`, id).Scan(&parent, &state, &assignee); err != nil {
		return err
	}
	if state == OrderSplit {
		return s.settleSplit(ctx, tx, id)
	}
	if !slices.Contains(endedStates, state) {
		return nil
	}
	if state != OrderBelayed {
		if err := s.noteOrphans(ctx, tx, id, assignee); err != nil {
			return err
		}
	}
	if state != OrderDone {
		if err := s.cascadeAfter(ctx, tx, id, state); err != nil {
			return err
		}
	}
	if parent != "" {
		return s.settleSplit(ctx, tx, parent)
	}
	return nil
}

// cascadeAfter withdraws, in tx, the unclaimed orders (pool or directed,
// no parked mission) waiting on order id, which has just ended state
// (failed, belayed or withdrawn) and so will never be done (#142). Each
// is logged "order withdrawn: it waits on ORD-0001, which failed" (or
// "which was belayed", "which was withdrawn"; kind order, payload reason
// and after), in rowid order, and its own withdraw cascades in turn
// (settleFamily), down the chain. A dependent already claimed is left to
// run, and nothing is logged for it.
func (s *Store) cascadeAfter(ctx context.Context, tx *sql.Tx, id, state string) error {
	rows, err := tx.QueryContext(ctx, `SELECT o.id, o.assignee FROM order_after a JOIN orders o ON o.id = a.order_id
		WHERE a.after_id = ? AND o.state IN (?, ?) AND o.parked_at IS NULL ORDER BY o.rowid`, id, OrderPool, OrderDirected)
	if err != nil {
		return err
	}
	type dep struct{ ID, Assignee string }
	var deps []dep
	for rows.Next() {
		var d dep
		if err := rows.Scan(&d.ID, &d.Assignee); err != nil {
			rows.Close()
			return err
		}
		deps = append(deps, d)
	}
	rows.Close()
	if err := rows.Err(); err != nil || len(deps) == 0 {
		return err
	}
	how := "was " + state
	if state == OrderFailed {
		how = "failed"
	}
	reason := "it waits on " + id + ", which " + how
	payload, err := json.Marshal(map[string]string{"reason": reason, "after": id})
	if err != nil {
		return err
	}
	for _, d := range deps {
		if _, err := s.withdraw(ctx, tx, d.ID, LogEntry{Kind: logKindOrder, CrewID: d.Assignee, OrderID: d.ID,
			Text: "order withdrawn: " + reason, Payload: payload}); err != nil {
			return err
		}
	}
	return nil
}

// OpenDependents returns the ids of the unclaimed orders (pool or
// directed) waiting on order id (#142), in creation order: the console
// refreshes them when id changes.
func (s *Store) OpenDependents(ctx context.Context, id string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT o.id FROM order_after a JOIN orders o ON o.id = a.order_id
		WHERE a.after_id = ? AND o.state IN (?, ?) ORDER BY o.rowid`, id, OrderPool, OrderDirected)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var d string
		if err := rows.Scan(&d); err != nil {
			return nil, err
		}
		ids = append(ids, d)
	}
	return ids, rows.Err()
}

// HasReadyDependents reports whether an unclaimed order with an after
// (#142) has every order in it done, so a claim would now take it.
func (s *Store) HasReadyDependents(ctx context.Context) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM orders o WHERE o.state IN (?, ?)
		AND EXISTS (SELECT 1 FROM order_after x WHERE x.order_id = o.id) AND NOT `+waitingSQL+`)`,
		OrderPool, OrderDirected).Scan(&n)
	return n == 1, err
}

// noteOrphans logs, in tx, the open child orders of order id, which has
// just ended (settleFamily). It logs nothing for an order without any.
func (s *Store) noteOrphans(ctx context.Context, tx *sql.Tx, id, assignee string) error {
	rows, err := tx.QueryContext(ctx, `SELECT id FROM orders WHERE parent_id = ? AND state NOT IN `+endedSQL+` ORDER BY rowid`, id)
	if err != nil {
		return err
	}
	var open []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			rows.Close()
			return err
		}
		open = append(open, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil || len(open) == 0 {
		return err
	}
	payload, err := withField(nil, "open_children", open)
	if err != nil {
		return err
	}
	_, err = s.appendLog(ctx, tx, LogEntry{Kind: logKindOrder, CrewID: assignee, OrderID: id, Payload: payload,
		Text: fmt.Sprintf("order ended with %d open child orders, which run on: %s", len(open), strings.Join(open, ", "))})
	return err
}

// settleSplit settles split order id, in tx, once none of its children is
// open: done when every child ended done, logged "order done: its N child
// orders are done" (kind order: the report's done entry stays the one
// `bmus push` reads), else failed, logged "order failed: child orders did
// not finish: ORD-0002 failed, ..." (kind fail, code CodeChildrenFailed),
// naming each child that didn't end done. An order that isn't split, or
// has a child still open, is left alone. A parent that settles may settle
// its own split parent in turn.
func (s *Store) settleSplit(ctx context.Context, tx *sql.Tx, id string) error {
	var state, assignee string
	err := tx.QueryRowContext(ctx, `SELECT state, assignee FROM orders WHERE id = ?`, id).Scan(&state, &assignee)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && state != OrderSplit) {
		return nil
	}
	if err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, `SELECT id, state FROM orders WHERE parent_id = ? ORDER BY rowid`, id)
	if err != nil {
		return err
	}
	type child struct{ ID, State string }
	var children []child
	for rows.Next() {
		var c child
		if err := rows.Scan(&c.ID, &c.State); err != nil {
			rows.Close()
			return err
		}
		children = append(children, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	var notDone []string
	for _, c := range children {
		if !slices.Contains(endedStates, c.State) {
			return nil
		}
		if c.State != OrderDone {
			notDone = append(notDone, c.ID+" "+c.State)
		}
	}
	to, kind := OrderDone, logKindOrder
	text := fmt.Sprintf("order done: its %d child orders are done", len(children))
	payload := map[string]any{"children": len(children)}
	if len(notDone) > 0 {
		to, kind = OrderFailed, logKindFail
		text = "order failed: child orders did not finish: " + strings.Join(notDone, ", ")
		payload["reason"], payload["code"], payload["not_done"] = text, CodeChildrenFailed, notDone
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE orders SET state = ? WHERE id = ? AND state = ?`, to, id, OrderSplit); err != nil {
		return err
	}
	if _, err := s.appendLog(ctx, tx, LogEntry{Kind: kind, CrewID: assignee, OrderID: id, Text: text, Payload: raw}); err != nil {
		return err
	}
	return s.settleFamily(ctx, tx, id)
}

// ParkOrder records that order id's mission is parked (#100): it sets
// parked_at and appends entry, in one transaction, only while the order is
// away or hailing and not parked yet, so a re-run writes nothing. It
// reports whether the order is still open (away or hailing) afterwards: a
// tour's backstop may have failed it first. A missing order returns
// sql.ErrNoRows.
func (s *Store) ParkOrder(ctx context.Context, id string, entry LogEntry) (open bool, err error) {
	err = s.withTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE orders SET parked_at = ? WHERE id = ? AND state IN (?, ?) AND parked_at IS NULL`,
			time.Now().UTC(), id, OrderAway, OrderHailing)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n > 0 {
			open = true
			_, err = s.appendLog(ctx, tx, entry)
			return err
		}
		var state string
		if err := tx.QueryRowContext(ctx, `SELECT state FROM orders WHERE id = ?`, id).Scan(&state); err != nil {
			return err
		}
		open = state == OrderAway || state == OrderHailing
		return nil
	})
	return open, err
}

// UnparkOrder clears order id's parked_at and appends entry, in one
// transaction, only while the order is away with crewID as its assignee: a
// tour resuming the order's parked mission (#100). It reports whether the
// order is now resumed on crewID. A re-run finds it already unparked on
// crewID and writes nothing; an order that moved on (settled, recalled)
// reads false. A missing order returns sql.ErrNoRows.
func (s *Store) UnparkOrder(ctx context.Context, id, crewID string, entry LogEntry) (resumed bool, err error) {
	err = s.withTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE orders SET parked_at = NULL
			WHERE id = ? AND state = ? AND assignee = ? AND parked_at IS NOT NULL`, id, OrderAway, crewID)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n > 0 {
			resumed = true
			_, err = s.appendLog(ctx, tx, entry)
			return err
		}
		var (
			state, assignee string
			parked          sql.NullTime
		)
		if err := tx.QueryRowContext(ctx, `SELECT state, assignee, parked_at FROM orders WHERE id = ?`, id).
			Scan(&state, &assignee, &parked); err != nil {
			return err
		}
		resumed = state == OrderAway && assignee == crewID && !parked.Valid
		return nil
	})
	return resumed, err
}

// RecallOrder returns order id to its division's pool (#101): only while
// it is away or hailing with crewID as its assignee, it moves it to pool,
// clears its assignee, sets parked_at (the recalled order keeps its parked
// mission, which the next claimer resumes) and appends entry, in one
// transaction. It returns the order's state afterwards and whether this
// call moved it: a re-run finds it moved and writes nothing. A missing
// order returns sql.ErrNoRows.
func (s *Store) RecallOrder(ctx context.Context, id, crewID string, entry LogEntry) (current string, recalled bool, err error) {
	if crewID == "" {
		return "", false, errors.New("store: RecallOrder needs the assignee")
	}
	err = s.withTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE orders SET state = ?, assignee = '', parked_at = COALESCE(parked_at, ?), `+clearProgress+`
			WHERE id = ? AND state IN (?, ?) AND assignee = ?`,
			OrderPool, time.Now().UTC(), id, OrderAway, OrderHailing, crewID)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n > 0 {
			current, recalled = OrderPool, true
			_, err = s.appendLog(ctx, tx, entry)
			return err
		}
		return tx.QueryRowContext(ctx, `SELECT state FROM orders WHERE id = ?`, id).Scan(&current)
	})
	if err != nil {
		return "", false, err
	}
	return current, recalled, nil
}

// BelayOrder ends order id as belayed (#101): SettleOpenOrder with
// OrderBelayed that, in the same transaction, cascades the belay to the
// order's child orders (cascadeBelay, #146), sending in-flight ones a
// belay through belay. A re-run finds the order belayed and writes
// nothing, cascade included.
func (s *Store) BelayOrder(ctx context.Context, id string, entry LogEntry, belay BelayFunc) (current, assignee string, belayed bool, err error) {
	return s.settleOpenOrder(ctx, id, OrderBelayed, entry, belay)
}

// BelaySplitOrder ends split order id as belayed and appends entry, in one
// transaction, and cascades the belay to its child orders (cascadeBelay,
// #146) in the same transaction. A split order has no mission: the
// captain's belay settles it here. It returns the order's state
// afterwards, what the cascade did to each child and whether this call
// moved it; an order no longer split (its last
// child settled it first) is left alone and nothing is logged. A missing
// order returns sql.ErrNoRows.
func (s *Store) BelaySplitOrder(ctx context.Context, id string, entry LogEntry, belay BelayFunc) (current string, cascade []ChildBelay, belayed bool, err error) {
	err = s.withTx(ctx, func(tx *sql.Tx) error {
		var assignee string
		err := tx.QueryRowContext(ctx, `UPDATE orders SET state = ? WHERE id = ? AND state = ? RETURNING assignee`,
			OrderBelayed, id, OrderSplit).Scan(&assignee)
		if errors.Is(err, sql.ErrNoRows) {
			return tx.QueryRowContext(ctx, `SELECT state FROM orders WHERE id = ?`, id).Scan(&current)
		}
		if err != nil {
			return err
		}
		current, belayed = OrderBelayed, true
		if _, err := s.appendLog(ctx, tx, entry); err != nil {
			return err
		}
		if cascade, err = s.cascadeBelay(ctx, tx, id, assignee, belay); err != nil {
			return err
		}
		return s.settleFamily(ctx, tx, id)
	})
	if err != nil {
		return "", nil, false, err
	}
	return current, cascade, belayed, nil
}

// BelayFunc sends child order o's mission a belay with reason, in tx
// (dbos.WithSendTransaction): a cascading belay's in-flight child (#146).
// It reports whether the belay reached a live mission (BelaySent), or why
// not: no mission started yet (BelayNoMission), or its mission has ended
// (BelayMissionEnded), with nothing sent. It runs holding the store's write
// slot: it must not call a Store write method (it would wait forever).
type BelayFunc func(tx *sql.Tx, o Order, reason string) (BelayReach, error)

// BelayReach is what a BelayFunc found of a child's mission.
type BelayReach int

const (
	BelayNoMission    BelayReach = iota // no mission yet: the cascade reports the child not reached
	BelaySent                           // the live mission was sent the belay: it settles the child
	BelayMissionEnded                   // the mission ended: the cascade settles the child belayed itself
)

// What a cascading belay did to a child order (ChildBelay.Action).
const (
	CascadeWithdrawn      = "withdrawn"       // in the pool or directed, no mission: withdrawn
	CascadeBelayRequested = "belay requested" // in flight: its mission was sent a belay
	CascadeBelayed        = "belayed"         // split, or its mission ended: belayed here, cascading in turn
	CascadeNotReached     = "not reached"     // in flight, but its mission has not started yet
	CascadeAlready        = "already"         // already ended: left alone ("already done")
)

// ChildBelay is what a cascading belay did to one child order (#146).
type ChildBelay struct {
	ID     string `json:"id"`
	State  string `json:"state"`  // the child's state when the cascade read it
	Action string `json:"action"` // a Cascade* value
}

// String is the child in the parent's cascade entry: "ORD-0002 withdrawn",
// "ORD-0004 already done".
func (c ChildBelay) String() string {
	switch c.Action {
	case CascadeAlready:
		return c.ID + " already " + c.State
	case CascadeNotReached:
		return c.ID + " not reached: its mission has not started"
	}
	return c.ID + " " + c.Action
}

// cascadeBelay carries the belay of order id, just settled belayed in tx,
// to its child orders (#146), in rowid order: a child in the pool or
// directed with no parked mission is withdrawn, logged "order withdrawn:
// its parent ORD-0001 was belayed"; a split child (no mission) is
// belayed the same way, "order belayed: ...", and cascades to its own
// children; any other open child (away, hailing, or recalled with its
// mission parked) gets a belay through belay, logged "belay requested:
// ...", and its mission settles it. One whose mission has ended, with
// nothing left to settle it, is belayed here as a split child is; one
// whose mission has not started yet is reported not reached. A child
// already ended is left alone.
// Each ended child settles nothing further: id is no longer split.
//
// It then logs one entry on id, "belay cascaded to 3 child orders:
// ORD-0002 withdrawn, ORD-0003 belay requested, ORD-0004 already done"
// (kind order, payload children), and returns what it did. An order with
// no children logs nothing. A nil belay reaches no mission.
func (s *Store) cascadeBelay(ctx context.Context, tx *sql.Tx, id, assignee string, belay BelayFunc) ([]ChildBelay, error) {
	rows, err := tx.QueryContext(ctx, `SELECT `+orderCols+` FROM orders WHERE parent_id = ? ORDER BY rowid`, id)
	if err != nil {
		return nil, err
	}
	var children []Order
	for rows.Next() {
		o, err := scanOrder(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		children = append(children, o)
	}
	rows.Close()
	if err := rows.Err(); err != nil || len(children) == 0 {
		return nil, err
	}
	reason := "its parent " + id + " was belayed"
	payload, err := json.Marshal(map[string]string{"reason": reason, "parent": id})
	if err != nil {
		return nil, err
	}
	childEntry := func(c Order, text string) LogEntry {
		return LogEntry{Kind: logKindOrder, CrewID: c.Assignee, OrderID: c.ID, Text: text + ": " + reason, Payload: payload}
	}
	var out []ChildBelay
	for _, c := range children {
		cb := ChildBelay{ID: c.ID, State: c.State}
		switch {
		case slices.Contains(endedStates, c.State):
			cb.Action = CascadeAlready
		case (c.State == OrderPool || c.State == OrderDirected) && c.ParkedAt == nil:
			if _, err := s.withdraw(ctx, tx, c.ID, childEntry(c, "order withdrawn")); err != nil {
				return nil, err
			}
			cb.Action = CascadeWithdrawn
		case c.State == OrderSplit:
			if _, err := tx.ExecContext(ctx, `UPDATE orders SET state = ? WHERE id = ? AND state = ?`, OrderBelayed, c.ID, OrderSplit); err != nil {
				return nil, err
			}
			if err := s.belayedHere(ctx, tx, c, childEntry(c, "order belayed"), belay); err != nil {
				return nil, err
			}
			cb.Action = CascadeBelayed
		default:
			reach := BelayNoMission
			if belay != nil {
				if reach, err = belay(tx, c, reason); err != nil {
					return nil, fmt.Errorf("belay child order %s: %w", c.ID, err)
				}
			}
			switch reach {
			case BelaySent:
				cb.Action = CascadeBelayRequested
				if _, err := s.appendLog(ctx, tx, childEntry(c, "belay requested")); err != nil {
					return nil, err
				}
			case BelayMissionEnded:
				if _, err := s.settleOpenTx(ctx, tx, c.ID, OrderBelayed); err != nil {
					return nil, err
				}
				if err := s.belayedHere(ctx, tx, c, childEntry(c, "order belayed"), belay); err != nil {
					return nil, err
				}
				cb.Action = CascadeBelayed
			default:
				cb.Action = CascadeNotReached
			}
		}
		out = append(out, cb)
	}
	parts := make([]string, len(out))
	for i, c := range out {
		parts[i] = c.String()
	}
	raw, err := json.Marshal(map[string]any{"children": out})
	if err != nil {
		return nil, err
	}
	_, err = s.appendLog(ctx, tx, LogEntry{Kind: logKindOrder, CrewID: assignee, OrderID: id, Payload: raw,
		Text: fmt.Sprintf("belay cascaded to %d child orders: %s", len(out), strings.Join(parts, ", "))})
	return out, err
}

// SettleOpenOrder ends order id as state and appends entry, in one
// transaction, only while its mission can still settle it: away or
// hailing, or unclaimed (pool or directed) with parked_at set, a recalled
// order whose mission is parked (#101). It clears parked_at. It returns
// the order's state afterwards, its assignee (the one it had when settled)
// and whether this call moved it; an order in any other state is left
// alone and nothing is logged, so a re-run writes nothing. A missing order
// returns sql.ErrNoRows. It refuses OrderBelayed: BelayOrder belays, and
// cascades to the order's child orders (#146).
func (s *Store) SettleOpenOrder(ctx context.Context, id, state string, entry LogEntry) (current, assignee string, settled bool, err error) {
	if state == OrderBelayed {
		return "", "", false, errors.New("store: belay with BelayOrder, which cascades to child orders")
	}
	return s.settleOpenOrder(ctx, id, state, entry, nil)
}

// settleOpenTx is SettleOpenOrder's write, in tx: it moves order id to
// state only while its mission can still settle it, returning its
// assignee, or sql.ErrNoRows when the order is in no such state.
func (s *Store) settleOpenTx(ctx context.Context, tx *sql.Tx, id, state string) (assignee string, err error) {
	err = tx.QueryRowContext(ctx, `UPDATE orders SET state = ?, parked_at = NULL, `+clearProgress+`
		WHERE id = ? AND (state IN (?, ?) OR (state IN (?, ?) AND parked_at IS NOT NULL))
		RETURNING assignee`,
		state, id, OrderAway, OrderHailing, OrderPool, OrderDirected).Scan(&assignee)
	return assignee, err
}

// belayedHere finishes, in tx, child order c the cascade has just moved to
// belayed itself (a split child, or one whose mission has ended): it logs
// entry, cascades to c's own children and settles its family.
func (s *Store) belayedHere(ctx context.Context, tx *sql.Tx, c Order, entry LogEntry, belay BelayFunc) error {
	if _, err := s.appendLog(ctx, tx, entry); err != nil {
		return err
	}
	if _, err := s.cascadeBelay(ctx, tx, c.ID, c.Assignee, belay); err != nil {
		return err
	}
	return s.settleFamily(ctx, tx, c.ID)
}

// settleOpenOrder is SettleOpenOrder, or BelayOrder for OrderBelayed.
func (s *Store) settleOpenOrder(ctx context.Context, id, state string, entry LogEntry, belay BelayFunc) (current, assignee string, settled bool, err error) {
	err = s.withTx(ctx, func(tx *sql.Tx) error {
		var err error
		assignee, err = s.settleOpenTx(ctx, tx, id, state)
		if err == nil {
			current, settled = state, true
			if _, err := s.appendLog(ctx, tx, entry); err != nil {
				return err
			}
			if state == OrderBelayed {
				if _, err := s.cascadeBelay(ctx, tx, id, assignee, belay); err != nil {
					return err
				}
			}
			return s.settleFamily(ctx, tx, id)
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		return tx.QueryRowContext(ctx, `SELECT state, assignee FROM orders WHERE id = ?`, id).Scan(&current, &assignee)
	})
	if err != nil {
		return "", "", false, err
	}
	return current, assignee, settled, nil
}

// WithdrawOrder moves order id to withdrawn and appends entry, in one
// transaction, only while it is unclaimed (pool or directed) and has no
// parked mission: such an order has no mission to tell (#101). It returns
// the order afterwards (without its repos) and whether this call withdrew
// it; otherwise nothing is written. A missing order returns sql.ErrNoRows.
func (s *Store) WithdrawOrder(ctx context.Context, id string, entry LogEntry) (o Order, withdrawn bool, err error) {
	err = s.withTx(ctx, func(tx *sql.Tx) error {
		if withdrawn, err = s.withdraw(ctx, tx, id, entry); err != nil {
			return err
		}
		o, err = scanOrder(tx.QueryRowContext(ctx, `SELECT `+orderCols+` FROM orders WHERE id = ?`, id))
		return err
	})
	if err != nil {
		return Order{}, false, err
	}
	return o, withdrawn, nil
}

// withdraw is WithdrawOrder's write, in tx: it reports whether it
// withdrew order id.
func (s *Store) withdraw(ctx context.Context, tx *sql.Tx, id string, entry LogEntry) (bool, error) {
	res, err := tx.ExecContext(ctx, `UPDATE orders SET state = ? WHERE id = ? AND state IN (?, ?) AND parked_at IS NULL`,
		OrderWithdrawn, id, OrderPool, OrderDirected)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil || n == 0 {
		return false, err
	}
	if _, err := s.appendLog(ctx, tx, entry); err != nil {
		return false, err
	}
	return true, s.settleFamily(ctx, tx, id)
}

// RouteFunc checks a route of order o to assignee and division, given the
// new assignee's crew row (nil when there is none, or no such crew
// member), and returns the route's log entry (RouteOrder).
type RouteFunc func(o Order, assignee, division string, crew *Crew) (LogEntry, error)

// RouteOrder routes order id (#101): in one transaction it reads the order,
// takes the new assignee and division from assignee and division (nil
// keeps the order's, empty clears it), reads the new assignee's crew row,
// and runs check, whose error rolls everything back and is returned as is.
// Then, only while the order is unclaimed (pool or directed), it sets the
// assignee, the division and the state (directed with an assignee, else
// pool) and appends check's entry. parked_at is kept: a routed recalled
// order is resumed by whoever claims it. A route that changes nothing
// writes and logs nothing. It returns the order afterwards (without its
// repos) and whether this call changed it; a claimed or settled order is
// left alone (check isn't run). A missing order returns sql.ErrNoRows.
func (s *Store) RouteOrder(ctx context.Context, id string, assignee, division *string, check RouteFunc) (o Order, routed bool, err error) {
	err = s.withTx(ctx, func(tx *sql.Tx) error {
		read := func() (err error) {
			o, err = scanOrder(tx.QueryRowContext(ctx, `SELECT `+orderCols+` FROM orders WHERE id = ?`, id))
			return err
		}
		if err := read(); err != nil {
			return err
		}
		if o.State != OrderPool && o.State != OrderDirected {
			return nil
		}
		to, div := o.Assignee, o.Division
		if assignee != nil {
			to = *assignee
		}
		if division != nil {
			div = *division
		}
		state := OrderPool
		if to != "" {
			state = OrderDirected
		}
		var crew *Crew
		if to != "" {
			c, err := scanCrew(tx.QueryRowContext(ctx, `SELECT `+crewCols+` FROM crew WHERE id = ?`, to))
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			if err == nil {
				crew = &c
			}
		}
		entry, err := check(o, to, div, crew)
		if err != nil {
			return err
		}
		if to == o.Assignee && div == o.Division && state == o.State {
			return nil
		}
		if _, err := tx.ExecContext(ctx, `UPDATE orders SET state = ?, assignee = ?, division = ? WHERE id = ?`,
			state, to, div, id); err != nil {
			return err
		}
		routed = true
		if _, err := s.appendLog(ctx, tx, entry); err != nil {
			return err
		}
		return read()
	})
	if err != nil {
		return Order{}, false, err
	}
	return o, routed, nil
}

// RequestOrder reads order id (without its repos) and the status of its
// assignee ("" without one), then runs fn with them, e.g. a check and a
// dbos.Send to the order's mission made with dbos.WithSendTransaction on
// tx, and appends the entry fn returns, all in one transaction: a request
// fn accepts is logged and delivered together, or neither (#101). An error
// from fn rolls everything back and is returned as is. A missing order
// returns sql.ErrNoRows.
//
// fn runs holding the store's write slot: it must not call a Store write
// method (it would wait forever).
func (s *Store) RequestOrder(ctx context.Context, id string, fn func(tx *sql.Tx, o Order, assigneeStatus string) (LogEntry, error)) error {
	return s.withTx(ctx, func(tx *sql.Tx) error {
		o, err := scanOrder(tx.QueryRowContext(ctx, `SELECT `+orderCols+` FROM orders WHERE id = ?`, id))
		if err != nil {
			return err
		}
		var status string
		if o.Assignee != "" {
			if err := tx.QueryRowContext(ctx, `SELECT status FROM crew WHERE id = ?`, o.Assignee).Scan(&status); err != nil &&
				!errors.Is(err, sql.ErrNoRows) {
				return err
			}
		}
		entry, err := fn(tx, o, status)
		if err != nil {
			return err
		}
		_, err = s.appendLog(ctx, tx, entry)
		return err
	})
}

// NextTour moves crew member crewID from tour from to tour from+1, clears
// its current beam and sets it beaming_in (#106: the new tour provisions a
// beam), appending entry, in one transaction, only while its tour_no is
// from and it isn't dismissed: a re-run, or a second caller, writes
// nothing (#100). It reports whether it changed the row, so
// a caller can tell a dismissal that committed after its own read (#113).
// With current_beam empty, a restarted daemon's ensurePumps can't attach
// the new tour's pump to the old beam. A missing crew member returns
// sql.ErrNoRows.
func (s *Store) NextTour(ctx context.Context, crewID string, from int, entry LogEntry) (changed bool, err error) {
	return s.updateCrewChanged(ctx, crewID, entry, `UPDATE crew SET tour_no = ?, current_beam = '', status = ?
		WHERE id = ? AND tour_no = ? AND status <> ?`,
		from+1, CrewBeamingIn, crewID, from, CrewDismissed)
}

// SetCrewStatusUnlessDismissed is SetCrewStatus for a crew member that
// isn't dismissed: a dismissed one is left alone, nothing is logged, and it
// returns dismissed true. provision uses it to record the new beam with
// the crew member beaming_in (#106), so a dismissal landing while a beam is
// being provisioned stands.
func (s *Store) SetCrewStatusUnlessDismissed(ctx context.Context, id, status, beam string, entry LogEntry) (dismissed bool, err error) {
	err = s.withTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE crew SET status = ?, current_beam = ?
			WHERE id = ? AND status <> ? AND (status <> ? OR current_beam <> ?)`,
			status, beam, id, CrewDismissed, status, beam)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n > 0 {
			_, err = s.appendLog(ctx, tx, entry)
			return err
		}
		var current string
		if err := tx.QueryRowContext(ctx, `SELECT status FROM crew WHERE id = ?`, id).Scan(&current); err != nil {
			return err
		}
		dismissed = current == CrewDismissed
		return nil
	})
	return dismissed, err
}

// DismissCrew sets crew member id dismissed and appends entry, then runs fn
// (nil-able) with the id of an order still assigned to it whose mission is
// parked ("" when there is none), e.g. a dbos.Send of the dismissal to that
// mission made with dbos.WithSendTransaction on tx, all in one transaction.
// It returns the crew member's tour_no as the transaction read it: the tour
// to signal, which a NextTour committing just before can't make stale
// (#113). A crew member already dismissed is left alone: nothing is
// logged, fn doesn't run, and changed is false. A missing crew member
// returns sql.ErrNoRows.
//
// fn runs holding the store's write slot: it must not call a Store write
// method (it would wait forever).
func (s *Store) DismissCrew(ctx context.Context, id string, entry LogEntry, fn func(tx *sql.Tx, parkedOrder string) error) (changed bool, tourNo int, err error) {
	err = s.withTx(ctx, func(tx *sql.Tx) error {
		err := tx.QueryRowContext(ctx, `UPDATE crew SET status = ? WHERE id = ? AND status <> ? RETURNING tour_no`,
			CrewDismissed, id, CrewDismissed).Scan(&tourNo)
		if errors.Is(err, sql.ErrNoRows) {
			var current string
			return tx.QueryRowContext(ctx, `SELECT status FROM crew WHERE id = ?`, id).Scan(&current)
		}
		if err != nil {
			return err
		}
		changed = true
		if _, err := s.appendLog(ctx, tx, entry); err != nil {
			return err
		}
		var order string
		err = tx.QueryRowContext(ctx, `SELECT id FROM orders WHERE assignee = ? AND state IN (?, ?) AND parked_at IS NOT NULL
			ORDER BY rowid LIMIT 1`, id, OrderAway, OrderHailing).Scan(&order)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if fn == nil {
			return nil
		}
		return fn(tx, order)
	})
	if err != nil {
		return false, 0, err
	}
	return changed, tourNo, nil
}

// Hail is a row of the hails table.
type Hail struct {
	ID               string
	OrderID          string
	CrewID           string
	Category         string
	Question         string
	Options          []string
	Context          json.RawMessage
	Route            string
	Status           string
	DecidedBy        string
	Decision         string
	Rationale        string
	Confidence       *float64
	CountermandUntil *time.Time
	// CreatedAt is nil on a hail from before #147. DecidedAt and
	// OriginalDecidedBy are when and by whom the hail was first decided
	// (closed); a countermand (#153) leaves them alone and sets
	// countermanded_at.
	CreatedAt         *time.Time
	DecidedAt         *time.Time
	OriginalDecidedBy string
	// CountermandedAt is when the captain last countermanded the
	// decision (CountermandHail), nil if never. CountermandOption is the
	// option that countermand picked, nil for a free-text one (or a
	// countermand from before #54).
	CountermandedAt   *time.Time
	CountermandOption *int
}

// CountermandedAway reports whether the captain countermanded h to
// anything but option 0, the option that allows. A countermand recorded
// with no option counts as allowing only when its decision is "allow".
func (h Hail) CountermandedAway() bool {
	if h.CountermandedAt == nil {
		return false
	}
	if h.CountermandOption != nil {
		return *h.CountermandOption != 0
	}
	return h.Decision != "allow"
}

// InsertHail records an open hail and returns its generated id (H-0001, ...).
// h.ID and h.Status are ignored; an empty Route defaults to "captain".
//
// In the same transaction it sets h.CrewID hailing if the crew member is
// away (one in sickbay or dismissed, after its tour ended, is left alone:
// #43) and h.OrderID is still its order, away or hailing (a late hail from
// an earlier tour's order leaves the current tour's status alone: #48),
// sets h.OrderID hailing if it is away with h.CrewID as its assignee (an
// order recalled or settled meanwhile is left alone: #134), and appends
// entry to the captain's log with the hail's id added to its payload
// object as hail_id. The hail's created_at is entry's TS (default now).
//
// In Phase 1 the agent is held inside its callback while a hail is open, so an
// order has at most one open hail. If one already exists for h.OrderID its id
// is returned and nothing is written, which makes a re-run of the insert
// (e.g. a replayed DBOS step) a no-op.
func (s *Store) InsertHail(ctx context.Context, h Hail, entry LogEntry) (string, error) {
	if h.Route == "" {
		h.Route = "captain"
	}
	if h.Options == nil {
		h.Options = []string{}
	}
	if len(h.Context) == 0 {
		h.Context = json.RawMessage("{}")
	}
	opts, err := json.Marshal(h.Options)
	if err != nil {
		return "", err
	}
	if entry.TS.IsZero() {
		entry.TS = time.Now()
	}
	var id string
	err = s.withTx(ctx, func(tx *sql.Tx) error {
		err := tx.QueryRowContext(ctx, `SELECT id FROM hails WHERE order_id = ? AND status = ? ORDER BY rowid LIMIT 1`,
			h.OrderID, HailOpen).Scan(&id)
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if id, err = nextID(ctx, tx, "hails", "H-%04d"); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO hails (id, order_id, crew_id, category, question, options, context, route, status, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			id, h.OrderID, h.CrewID, h.Category, h.Question, string(opts), string(h.Context), h.Route, HailOpen, entry.TS.UTC())
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE crew SET status = ? WHERE id = ? AND status = ?
			AND EXISTS (SELECT 1 FROM orders WHERE id = ? AND assignee = crew.id AND state IN (?, ?))`,
			CrewHailing, h.CrewID, CrewAway, h.OrderID, OrderAway, OrderHailing); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE orders SET state = ? WHERE id = ? AND state = ? AND assignee = ?`,
			OrderHailing, h.OrderID, OrderAway, h.CrewID); err != nil {
			return err
		}
		if entry.Payload, err = withField(entry.Payload, "hail_id", id); err != nil {
			return err
		}
		_, err = s.appendLog(ctx, tx, entry)
		return err
	})
	if err != nil {
		return "", err
	}
	return id, nil
}

const hailCols = `id, order_id, crew_id, category, question, options, context, route, status,
	decided_by, decision, rationale, confidence, countermand_until, created_at, decided_at, original_decided_by, countermanded_at,
	countermand_option`

func scanHail(row interface{ Scan(...any) error }) (Hail, error) {
	var (
		h          Hail
		opts, hctx string
		cu, ca, da sql.NullTime
		cma        sql.NullTime
		conf       sql.NullFloat64
		cmo        sql.NullInt64
	)
	err := row.Scan(&h.ID, &h.OrderID, &h.CrewID, &h.Category, &h.Question, &opts, &hctx, &h.Route, &h.Status,
		&h.DecidedBy, &h.Decision, &h.Rationale, &conf, &cu, &ca, &da, &h.OriginalDecidedBy, &cma, &cmo)
	if err != nil {
		return Hail{}, err
	}
	if err := json.Unmarshal([]byte(opts), &h.Options); err != nil {
		return Hail{}, fmt.Errorf("hail %s options: %w", h.ID, err)
	}
	h.Context = json.RawMessage(hctx)
	if conf.Valid {
		h.Confidence = &conf.Float64
	}
	if cu.Valid {
		h.CountermandUntil = &cu.Time
	}
	if ca.Valid {
		h.CreatedAt = &ca.Time
	}
	if da.Valid {
		h.DecidedAt = &da.Time
	}
	if cma.Valid {
		h.CountermandedAt = &cma.Time
	}
	if cmo.Valid {
		o := int(cmo.Int64)
		h.CountermandOption = &o
	}
	return h, nil
}

// GetHail returns the hail with the given id, or sql.ErrNoRows.
func (s *Store) GetHail(ctx context.Context, id string) (Hail, error) {
	return scanHail(s.db.QueryRowContext(ctx, `SELECT `+hailCols+` FROM hails WHERE id = ?`, id))
}

// GetHailLocked is GetHail read in a write transaction, with now taken
// inside it: a countermand in flight (CountermandHailOption, which takes
// its time in its own transaction) commits first, and one that starts
// later is judged against a later now. A caller that finds now past
// countermand_until therefore reads the hail's final decision.
func (s *Store) GetHailLocked(ctx context.Context, id string) (h Hail, now time.Time, err error) {
	err = s.withTx(ctx, func(tx *sql.Tx) error {
		now = time.Now()
		h, err = scanHail(tx.QueryRowContext(ctx, `SELECT `+hailCols+` FROM hails WHERE id = ?`, id))
		return err
	})
	return h, now, err
}

// OpenHailForOrder returns orderID's open hail, if it has one (an order has
// at most one, see InsertHail). found is false when none is open.
func (s *Store) OpenHailForOrder(ctx context.Context, orderID string) (h Hail, found bool, err error) {
	h, err = scanHail(s.db.QueryRowContext(ctx, `SELECT `+hailCols+` FROM hails WHERE order_id = ? AND status = ? ORDER BY rowid LIMIT 1`,
		orderID, HailOpen))
	if errors.Is(err, sql.ErrNoRows) {
		return Hail{}, false, nil
	}
	if err != nil {
		return Hail{}, false, err
	}
	return h, true, nil
}

// ListHails returns the hails with the given status (HailOpen, HailDecided,
// HailTimedOut), or every hail when status is empty, in creation order.
func (s *Store) ListHails(ctx context.Context, status string) ([]Hail, error) {
	q, args := `SELECT `+hailCols+` FROM hails`, []any{}
	if status != "" {
		q += ` WHERE status = ?`
		args = append(args, status)
	}
	rows, err := s.db.QueryContext(ctx, q+` ORDER BY rowid`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Hail
	for rows.Next() {
		h, err := scanHail(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// StatusSnapshot is what the bridge's status is computed from, read at one
// point in time.
type StatusSnapshot struct {
	Crew        []Crew         // every crew member, ordered by id
	OrderStates map[string]int // the number of orders in each state
	HailsOpen   int
	LastLogID   int64 // the captain's log's last id, 0 when empty
	// RedOrders are the ids of open red orders (pool, directed, away or
	// hailing with PriorityRed), in id order.
	RedOrders []string
	// CommandDown is whether Command's consults have nobody to go to:
	// regs.yaml is configured, and the command division has crew members
	// but none on deck. The bridge sets it from Crew (ReadStatus knows
	// neither regs.yaml nor divisions).
	CommandDown bool
}

// ReadStatus reads a StatusSnapshot in one read-only transaction (a
// deferred BEGIN, not the writers' BEGIN IMMEDIATE), so its parts agree
// with each other and with LastLogID.
func (s *Store) ReadStatus(ctx context.Context) (StatusSnapshot, error) {
	snap := StatusSnapshot{OrderStates: map[string]int{}}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return snap, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT `+crewCols+` FROM crew ORDER BY id`)
	if err != nil {
		return snap, err
	}
	for rows.Next() {
		c, err := scanCrew(rows)
		if err != nil {
			rows.Close()
			return snap, err
		}
		snap.Crew = append(snap.Crew, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return snap, err
	}
	rows, err = tx.QueryContext(ctx, `SELECT state, COUNT(*) FROM orders GROUP BY state`)
	if err != nil {
		return snap, err
	}
	for rows.Next() {
		var (
			state string
			n     int
		)
		if err := rows.Scan(&state, &n); err != nil {
			rows.Close()
			return snap, err
		}
		snap.OrderStates[state] = n
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return snap, err
	}
	rows, err = tx.QueryContext(ctx, `SELECT id FROM orders WHERE priority = ? AND state IN (?, ?, ?, ?) ORDER BY id`,
		PriorityRed, OrderPool, OrderDirected, OrderAway, OrderHailing)
	if err != nil {
		return snap, err
	}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return snap, err
		}
		snap.RedOrders = append(snap.RedOrders, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return snap, err
	}
	err = tx.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM hails WHERE status = ?), (SELECT COALESCE(MAX(id), 0) FROM log)`,
		HailOpen).Scan(&snap.HailsOpen, &snap.LastLogID)
	if err != nil {
		return snap, err
	}
	return snap, tx.Commit()
}

// MarkHailDecided records the decision on an open hail, as DecideOpenHail
// does. Repeating the same decision is a no-op that writes nothing, and so
// is confirming a decision by decidedBy that the captain has countermanded
// since (original_decided_by decidedBy, countermanded_at set: #153); a
// hail closed any other way returns ErrHailClosed, and a missing hail
// sql.ErrNoRows.
func (s *Store) MarkHailDecided(ctx context.Context, id, decidedBy, decision, rationale string, entry LogEntry) error {
	return s.withTx(ctx, func(tx *sql.Tx) error {
		decided, err := s.decideOpenHail(ctx, tx, id, HailDecision{DecidedBy: decidedBy, Decision: decision, Rationale: rationale}, entry)
		if err != nil || decided {
			return err
		}
		var (
			status, by, dec, orig string
			cm                    sql.NullTime
		)
		if err := tx.QueryRowContext(ctx, `SELECT status, decided_by, decision, original_decided_by, countermanded_at FROM hails WHERE id = ?`, id).
			Scan(&status, &by, &dec, &orig, &cm); err != nil {
			return err
		}
		if status == HailDecided && by == decidedBy && dec == decision {
			return nil
		}
		// The captain countermanded the decision before the mission
		// confirmed it (#153): decidedBy's first decision stands.
		if status == HailDecided && orig == decidedBy && cm.Valid {
			return nil
		}
		return fmt.Errorf("%s is %s: %w", id, status, ErrHailClosed)
	})
}

// DecideOpenHail records a decision on hail id only if it is still open, sets
// the hail's crew member back away if it is hailing, appends entry to the
// captain's log, and runs fn (nil-able), e.g. a dbos.Send made with
// dbos.WithSendTransaction on tx, all in one transaction: the decision and
// fn's writes commit or roll back together. Unlike MarkHailDecided it is not
// idempotent: a hail that isn't open, even one decided the same way, returns
// ErrHailClosed, so of two concurrent callers exactly one succeeds. A missing
// hail returns sql.ErrNoRows.
//
// fn runs holding the store's write slot: it must not call a Store write
// method (it would wait forever).
func (s *Store) DecideOpenHail(ctx context.Context, id, decidedBy, decision, rationale string, entry LogEntry, fn func(tx *sql.Tx) error) error {
	return s.DecideOpenHailAs(ctx, id, HailDecision{DecidedBy: decidedBy, Decision: decision, Rationale: rationale}, entry, fn)
}

// HailDecision is a decision on a hail. Route, if set, replaces the hail's
// route (an auto-allowed hail's "auto"). Confidence and CountermandUntil,
// if set, are written with it: a decision the captain may countermand
// until then (Command's, or the captain timeout's default_option).
type HailDecision struct {
	DecidedBy, Decision, Rationale string
	Route                          string
	Confidence                     *float64
	CountermandUntil               *time.Time
}

// DecideOpenHailAs is DecideOpenHail with dec's other fields written too.
//
// fn runs holding the store's write slot: it must not call a Store write
// method (it would wait forever).
func (s *Store) DecideOpenHailAs(ctx context.Context, id string, dec HailDecision, entry LogEntry, fn func(tx *sql.Tx) error) error {
	return s.withTx(ctx, func(tx *sql.Tx) error {
		decided, err := s.decideOpenHail(ctx, tx, id, dec, entry)
		if err != nil {
			return err
		}
		if !decided {
			var status string
			if err := tx.QueryRowContext(ctx, `SELECT status FROM hails WHERE id = ?`, id).Scan(&status); err != nil {
				return err
			}
			return fmt.Errorf("%s is %s: %w", id, status, ErrHailClosed)
		}
		if fn == nil {
			return nil
		}
		return fn(tx)
	})
}

// CountermandHail records the captain's countermand of hail id's decision
// (spec/phase-3-plan-2026-10-29.md section 6): decided_by captain, decision,
// rationale reason, countermanded_at now (read inside the transaction,
// which also becomes entry's TS), entry appended to the captain's log, and
// fn (nil-able) run, all in one transaction. decided_at,
// original_decided_by, route, confidence and countermand_until are left
// alone, so a second countermand within the same window is taken too. It
// requires the hail decided, its original_decided_by policy or command,
// and countermand_until set and not before now; otherwise it returns
// ErrCountermandExpired (wrapped), and a missing hail sql.ErrNoRows.
//
// fn runs holding the store's write slot: it must not call a Store write
// method (it would wait forever).
func (s *Store) CountermandHail(ctx context.Context, id, decision, reason string, entry LogEntry, fn func(tx *sql.Tx) error) error {
	return s.CountermandHailOption(ctx, id, decision, nil, reason, entry, fn)
}

// CountermandHailOption is CountermandHail recording option, the option
// the countermand picked (nil for a free-text one), as the hail's
// countermand_option.
//
// fn runs holding the store's write slot: it must not call a Store write
// method (it would wait forever).
func (s *Store) CountermandHailOption(ctx context.Context, id, decision string, option *int, reason string, entry LogEntry,
	fn func(tx *sql.Tx) error) error {
	return s.withTx(ctx, func(tx *sql.Tx) error {
		// After withTx took the write lock: a countermand that waited on
		// it is judged by when it ran.
		entry.TS = time.Now()
		var (
			status, by string
			until      sql.NullTime
		)
		err := tx.QueryRowContext(ctx, `SELECT status, original_decided_by, countermand_until FROM hails WHERE id = ?`, id).
			Scan(&status, &by, &until)
		if err != nil {
			return err
		}
		switch {
		case status != HailDecided:
			return fmt.Errorf("%s is %s: %w", id, status, ErrCountermandExpired)
		case by != "policy" && by != "command":
			return fmt.Errorf("%s was decided by %s: %w", id, by, ErrCountermandExpired)
		case !until.Valid:
			return fmt.Errorf("%s has no countermand window: %w", id, ErrCountermandExpired)
		case entry.TS.After(until.Time):
			return fmt.Errorf("%s could be countermanded until %s: %w", id, until.Time.UTC().Format(time.RFC3339), ErrCountermandExpired)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE hails SET decided_by = 'captain', decision = ?, rationale = ?, countermanded_at = ?,
			countermand_option = ? WHERE id = ?`, decision, reason, entry.TS.UTC(), option, id); err != nil {
			return err
		}
		if _, err := s.appendLog(ctx, tx, entry); err != nil || fn == nil {
			return err
		}
		return fn(tx)
	})
}

// ListCountermandable returns the decided hails whose countermand window is
// still open at now (countermand_until not before it), in creation order:
// the console's snapshot shows them with open hails (#153). The index on
// countermand_until bounds the read to windows ending after a minute
// before now; the exact check runs on the times read back.
func (s *Store) ListCountermandable(ctx context.Context, now time.Time) ([]Hail, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+hailCols+` FROM hails
		WHERE countermand_until >= ? AND status = ? AND original_decided_by IN ('policy', 'command') ORDER BY rowid`,
		now.UTC().Add(-time.Minute), HailDecided)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Hail
	for rows.Next() {
		h, err := scanHail(rows)
		if err != nil {
			return nil, err
		}
		if !now.After(*h.CountermandUntil) {
			out = append(out, h)
		}
	}
	return out, rows.Err()
}

// HailMetrics are the hail rates of spec/phase-3-plan-2026-10-29.md
// section 9, over the hails created at or after a point in time (#156).
type HailMetrics struct {
	// Total counts every hail created in the period, open or closed.
	// Decided counts those decided (status decided, decided_at set);
	// hails closed unanswered (timed_out: parked, recalled, belayed,
	// withdrawn, the order ended) and open ones are in Total only.
	Total, Decided int
	// Countermanded counts the decided hails the captain countermanded
	// (countermanded_at set).
	Countermanded int
	// ByOriginalDecider counts the decided hails by original_decided_by:
	// always policy, command and captain (0 when none), plus any other
	// value found.
	ByOriginalDecider map[string]int
	// MedianTimeToAnswer and P90TimeToAnswer are nearest-rank percentiles
	// of decided_at - created_at over the decided hails, 0 when Decided
	// is 0.
	MedianTimeToAnswer, P90TimeToAnswer time.Duration
}

// AutoResolvedShare is (policy + command) / Decided: the decided hails the
// captain didn't answer first. ok is false when nothing was decided.
func (m HailMetrics) AutoResolvedShare() (share float64, ok bool) {
	if m.Decided == 0 {
		return 0, false
	}
	return float64(m.ByOriginalDecider[DeciderPolicy]+m.ByOriginalDecider[DeciderCommand]) / float64(m.Decided), true
}

// CaptainOverrides is Countermanded / Decided. ok is false when nothing
// was decided.
func (m HailMetrics) CaptainOverrides() (rate float64, ok bool) {
	if m.Decided == 0 {
		return 0, false
	}
	return float64(m.Countermanded) / float64(m.Decided), true
}

// The original_decided_by values (comms.DecidedBy*).
const (
	DeciderPolicy  = "policy"
	DeciderCommand = "command"
	DeciderCaptain = "captain"
)

// HailMetrics reads the hails created at or after since in one query and
// computes their HailMetrics. Hails with no created_at (from before #147)
// are left out. created_at is stored in UTC as SQLite's text format
// ("2006-01-02 15:04:05.999999999+00:00"), which sorts as text: since is
// compared in UTC too.
func (s *Store) HailMetrics(ctx context.Context, since time.Time) (HailMetrics, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT status, original_decided_by, created_at, decided_at, countermanded_at
		FROM hails WHERE created_at >= ?`, since.UTC())
	if err != nil {
		return HailMetrics{}, err
	}
	defer rows.Close()
	m := HailMetrics{ByOriginalDecider: map[string]int{DeciderPolicy: 0, DeciderCommand: 0, DeciderCaptain: 0}}
	var answers []time.Duration
	for rows.Next() {
		var (
			status, orig string
			created      time.Time
			decided, cm  sql.NullTime
		)
		if err := rows.Scan(&status, &orig, &created, &decided, &cm); err != nil {
			return HailMetrics{}, err
		}
		m.Total++
		if status != HailDecided || !decided.Valid {
			continue
		}
		m.Decided++
		m.ByOriginalDecider[orig]++
		if cm.Valid {
			m.Countermanded++
		}
		answers = append(answers, max(decided.Time.Sub(created), 0))
	}
	if err := rows.Err(); err != nil {
		return HailMetrics{}, err
	}
	slices.Sort(answers)
	m.MedianTimeToAnswer = nearestRank(answers, 50)
	m.P90TimeToAnswer = nearestRank(answers, 90)
	return m, nil
}

// nearestRank is the p-th percentile (0 < p <= 100) of sorted by the
// nearest-rank method: the smallest value with at least p% of the values
// at or below it, sorted[ceil(p/100*n)-1]. It is 0 for no values.
func nearestRank(sorted []time.Duration, p int) time.Duration {
	n := len(sorted)
	if n == 0 {
		return 0
	}
	rank := (p*n + 99) / 100 // ceil(p*n/100)
	return sorted[max(rank, 1)-1]
}

// decideOpenHail decides hail id if it is open, sets its crew member and
// order back away (hailClosedAway), and appends entry, in tx. It reports
// whether the hail was open. decided_at (entry's TS, default now) and
// original_decided_by (dec.DecidedBy) are set only while still unset;
// route, confidence and countermand_until only when dec sets them.
func (s *Store) decideOpenHail(ctx context.Context, tx *sql.Tx, id string, dec HailDecision, entry LogEntry) (bool, error) {
	if entry.TS.IsZero() {
		entry.TS = time.Now()
	}
	var until sql.NullTime
	if dec.CountermandUntil != nil {
		until = sql.NullTime{Time: dec.CountermandUntil.UTC(), Valid: true}
	}
	res, err := tx.ExecContext(ctx, `UPDATE hails SET status = ?, decided_by = ?, decision = ?, rationale = ?, decided_at = COALESCE(decided_at, ?),
		original_decided_by = CASE original_decided_by WHEN '' THEN ? ELSE original_decided_by END,
		route = COALESCE(NULLIF(?, ''), route), confidence = COALESCE(?, confidence), countermand_until = COALESCE(?, countermand_until)
		WHERE id = ? AND status = ?`, HailDecided, dec.DecidedBy, dec.Decision, dec.Rationale, entry.TS.UTC(), dec.DecidedBy,
		dec.Route, dec.Confidence, until, id, HailOpen)
	if err != nil {
		return false, err
	}
	if n, err := res.RowsAffected(); err != nil || n == 0 {
		return false, err
	}
	if err := hailClosedAway(ctx, tx, id); err != nil {
		return false, err
	}
	_, err = s.appendLog(ctx, tx, entry)
	return err == nil, err
}

// hailClosedAway sets closed hail id's crew member and order back away, in
// tx: the crew member only if hailing (one in sickbay or dismissed is left
// alone: #43) and the hail's order is still its order, away or hailing
// (#48); the order only if hailing (#134).
func hailClosedAway(ctx context.Context, tx *sql.Tx, id string) error {
	if _, err := tx.ExecContext(ctx, `UPDATE crew SET status = ?
		WHERE id = (SELECT crew_id FROM hails WHERE id = ?) AND status = ?
		AND EXISTS (SELECT 1 FROM orders WHERE id = (SELECT order_id FROM hails WHERE id = ?)
			AND assignee = crew.id AND state IN (?, ?))`,
		CrewAway, id, CrewHailing, id, OrderAway, OrderHailing); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `UPDATE orders SET state = ?
		WHERE id = (SELECT order_id FROM hails WHERE id = ?) AND state = ?`, OrderAway, id, OrderHailing)
	return err
}

// TimeOutOpenHail moves hail id from open to timed_out and, only if that
// update changed the row, sets its crew member and order back away
// (hailClosedAway) and appends entry to the captain's log, all in one
// transaction. It reports whether it closed the hail. A hail that isn't open
// (already timed out, or decided) is left alone and nothing is logged, so of
// two callers racing to close the same hail exactly one logs, and a re-run
// after either outcome committed changes nothing. A missing hail returns
// sql.ErrNoRows.
func (s *Store) TimeOutOpenHail(ctx context.Context, id string, entry LogEntry) (closed bool, err error) {
	return s.TimeOutOpenHailThen(ctx, id, entry, nil)
}

// TimeOutOpenHailThen is TimeOutOpenHail that also runs fn (nil-able) in
// the same transaction once it has closed the hail, e.g. a dbos.Send made
// with dbos.WithSendTransaction on tx.
//
// fn runs holding the store's write slot: it must not call a Store write
// method (it would wait forever).
func (s *Store) TimeOutOpenHailThen(ctx context.Context, id string, entry LogEntry, fn func(tx *sql.Tx) error) (closed bool, err error) {
	err = s.withTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE hails SET status = ? WHERE id = ? AND status = ?`, HailTimedOut, id, HailOpen)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			var status string
			return tx.QueryRowContext(ctx, `SELECT status FROM hails WHERE id = ?`, id).Scan(&status)
		}
		closed = true
		if err := hailClosedAway(ctx, tx, id); err != nil {
			return err
		}
		if _, err = s.appendLog(ctx, tx, entry); err != nil || fn == nil {
			return err
		}
		return fn(tx)
	})
	if err != nil {
		return false, err
	}
	return closed, nil
}

// UpdateOpenHail sets hail id's route (unless route is empty) and appends
// entry to the captain's log, in one transaction, only while the hail is
// open: the hail workflow's routing, reminder and timeout entries. It
// reports whether the hail was open; a closed one is left alone and
// nothing is logged. A missing hail returns sql.ErrNoRows.
func (s *Store) UpdateOpenHail(ctx context.Context, id, route string, entry LogEntry) (open bool, err error) {
	err = s.withTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE hails SET route = COALESCE(NULLIF(?, ''), route) WHERE id = ? AND status = ?`,
			route, id, HailOpen)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			var status string
			return tx.QueryRowContext(ctx, `SELECT status FROM hails WHERE id = ?`, id).Scan(&status)
		}
		open = true
		_, err = s.appendLog(ctx, tx, entry)
		return err
	})
	if err != nil {
		return false, err
	}
	return open, nil
}

// Checkpoint is a row of the checkpoints table, with its checkpoint_repos
// rows.
type Checkpoint struct {
	OrderID string
	// Beam is the beam the checkpoint was pulled from. Seq is that beam's
	// message seq, so (OrderID, Beam, Seq) is the key: a resumed order's
	// next beam numbers from 1 again (#100).
	Beam string
	Seq  int
	// Repos are the checkpoint's bundles in the cargo bay, one per repo of
	// the order, in order.
	Repos          []CheckpointRepo
	TranscriptPath string
	HandoverPath   string
	CreatedAt      time.Time
}

// CheckpointRepo is a row of the checkpoint_repos table: one repo's bundle
// of a checkpoint, carrying bmus/<order> at Commit.
type CheckpointRepo struct {
	Name       string
	Commit     string
	BundlePath string
}

// ErrCheckpointConflict is returned by InsertCheckpoint when (order, beam,
// seq) is already recorded with different contents.
var ErrCheckpointConflict = errors.New("checkpoint already recorded with different contents")

// InsertCheckpoint records a checkpoint pulled into the cargo bay, its repos
// included, in one transaction. A zero CreatedAt defaults to now.
//
// It is idempotent, so a replayed DBOS step can repeat it: if (order, beam,
// seq) is already recorded with the same paths and repos, nothing changes
// and it returns nil (the first CreatedAt is kept). Different contents for
// the same key return ErrCheckpointConflict.
func (s *Store) InsertCheckpoint(ctx context.Context, c Checkpoint) error {
	if c.CreatedAt.IsZero() {
		c.CreatedAt = time.Now()
	}
	return s.withTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `INSERT INTO checkpoints (order_id, beam, seq, transcript_path, handover_path, created_at)
			VALUES (?, ?, ?, ?, ?, ?) ON CONFLICT (order_id, beam, seq) DO NOTHING`,
			c.OrderID, c.Beam, c.Seq, c.TranscriptPath, c.HandoverPath, c.CreatedAt.UTC())
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n > 0 {
			for i, r := range c.Repos {
				if _, err := tx.ExecContext(ctx, `INSERT INTO checkpoint_repos (order_id, beam, seq, pos, name, "commit", bundle_path)
					VALUES (?, ?, ?, ?, ?, ?, ?)`, c.OrderID, c.Beam, c.Seq, i, r.Name, r.Commit, r.BundlePath); err != nil {
					return fmt.Errorf("repo %s: %w", r.Name, err)
				}
			}
			return nil
		}
		var same int
		err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM checkpoints WHERE order_id = ? AND beam = ? AND seq = ?
			AND transcript_path = ? AND handover_path = ?`,
			c.OrderID, c.Beam, c.Seq, c.TranscriptPath, c.HandoverPath).Scan(&same)
		if err != nil {
			return err
		}
		repos, err := checkpointRepos(ctx, tx, c.OrderID, c.Beam, c.Seq)
		if err != nil {
			return err
		}
		if same == 0 || !slices.Equal(repos, c.Repos) {
			return fmt.Errorf("checkpoint %s/%s-%d: %w", c.OrderID, c.Beam, c.Seq, ErrCheckpointConflict)
		}
		return nil
	})
}

// checkpointRepos returns checkpoint (orderID, beam, seq)'s repos, in order.
func checkpointRepos(ctx context.Context, q interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, orderID, beam string, seq int) ([]CheckpointRepo, error) {
	rows, err := q.QueryContext(ctx, `SELECT name, "commit", bundle_path FROM checkpoint_repos
		WHERE order_id = ? AND beam = ? AND seq = ? ORDER BY pos`, orderID, beam, seq)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CheckpointRepo
	for rows.Next() {
		var r CheckpointRepo
		if err := rows.Scan(&r.Name, &r.Commit, &r.BundlePath); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ListCheckpoints returns orderID's checkpoints by seq, with their repos.
// Seqs restart on each beam, so it can't tell an order's beams apart:
// LatestCheckpoint and CheckpointsNewestFirst go by insertion order.
func (s *Store) ListCheckpoints(ctx context.Context, orderID string) ([]Checkpoint, error) {
	return s.listCheckpoints(ctx, orderID, `seq, rowid`, -1)
}

// CheckpointsNewestFirst returns orderID's checkpoints, with their repos,
// the last one recorded first.
func (s *Store) CheckpointsNewestFirst(ctx context.Context, orderID string) ([]Checkpoint, error) {
	return s.listCheckpoints(ctx, orderID, `rowid DESC`, -1)
}

// LatestCheckpoint returns orderID's last recorded checkpoint, with its
// repos (by insertion order: seqs restart on each beam). found is false
// when the order has none.
func (s *Store) LatestCheckpoint(ctx context.Context, orderID string) (c Checkpoint, found bool, err error) {
	cps, err := s.listCheckpoints(ctx, orderID, `rowid DESC`, 1)
	if err != nil || len(cps) == 0 {
		return Checkpoint{}, false, err
	}
	return cps[0], true, nil
}

// listCheckpoints returns at most limit (-1: all) of orderID's checkpoints
// in orderBy's order, with their repos.
func (s *Store) listCheckpoints(ctx context.Context, orderID, orderBy string, limit int) ([]Checkpoint, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT order_id, beam, seq, transcript_path, handover_path, created_at
		FROM checkpoints WHERE order_id = ? ORDER BY `+orderBy+` LIMIT ?`, orderID, limit)
	if err != nil {
		return nil, err
	}
	var out []Checkpoint
	for rows.Next() {
		var c Checkpoint
		if err := rows.Scan(&c.OrderID, &c.Beam, &c.Seq, &c.TranscriptPath, &c.HandoverPath, &c.CreatedAt); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, c)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	for i := range out {
		if out[i].Repos, err = checkpointRepos(ctx, s.db, orderID, out[i].Beam, out[i].Seq); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// BeamRecord is a row of the beams table.
type BeamRecord struct {
	// UUID is the provider's unique id for the beam. Providers in Phase 1
	// identify beams by alias only, so the bridge stores the alias here too.
	UUID   string
	Alias  string
	CrewID string
	// TourNo is the crew member's tour that created the beam.
	TourNo    int
	CreatedAt time.Time
	ExpiresAt time.Time
	// RemovedAt is when the bridge removed the beam or found it gone, nil
	// while it may still exist (SetBeamRemoved).
	RemovedAt *time.Time
}

// ErrBeamTaken is returned by InsertBeam for a UUID already recorded for
// another crew member.
var ErrBeamTaken = errors.New("beam already recorded for another crew member")

// InsertBeam records a provisioned beam and appends entry to the captain's
// log, in one transaction. A zero CreatedAt defaults to now. Recording the
// same UUID for the same crew member again is a no-op that writes nothing (a
// replayed DBOS step); for another crew member it returns ErrBeamTaken.
func (s *Store) InsertBeam(ctx context.Context, b BeamRecord, entry LogEntry) error {
	if b.CreatedAt.IsZero() {
		b.CreatedAt = time.Now()
	}
	return s.withTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `INSERT INTO beams (uuid, alias, crew_id, tour_no, created_at, expires_at)
			VALUES (?, ?, ?, ?, ?, ?) ON CONFLICT (uuid) DO NOTHING`,
			b.UUID, b.Alias, b.CrewID, b.TourNo, b.CreatedAt.UTC(), b.ExpiresAt.UTC())
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n > 0 {
			_, err = s.appendLog(ctx, tx, entry)
			return err
		}
		var owner string
		if err := tx.QueryRowContext(ctx, `SELECT crew_id FROM beams WHERE uuid = ?`, b.UUID).Scan(&owner); err != nil {
			return err
		}
		if owner != b.CrewID {
			return fmt.Errorf("beam %s (crew %s): %w", b.UUID, owner, ErrBeamTaken)
		}
		return nil
	})
}

// ListBeams returns crewID's beams, oldest first.
func (s *Store) ListBeams(ctx context.Context, crewID string) ([]BeamRecord, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT uuid, alias, crew_id, tour_no, created_at, expires_at, removed_at
		FROM beams WHERE crew_id = ? ORDER BY rowid`, crewID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []BeamRecord
	for rows.Next() {
		var b BeamRecord
		if err := rows.Scan(&b.UUID, &b.Alias, &b.CrewID, &b.TourNo, &b.CreatedAt, &b.ExpiresAt, &b.RemovedAt); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// SetBeamRemoved records at as when crewID's beam alias was removed or
// found gone, on each of its rows with that alias not yet marked. Providers
// reuse aliases, but only one beam holds an alias at a time, so the older
// rows are gone too. A row already marked keeps its time.
func (s *Store) SetBeamRemoved(ctx context.Context, crewID, alias string, at time.Time) error {
	return s.withTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE beams SET removed_at = ? WHERE crew_id = ? AND alias = ? AND removed_at IS NULL`,
			at.UTC(), crewID, alias)
		return err
	})
}

// LogEntry is a row of the append-only captain's log.
type LogEntry struct {
	ID      int64
	TS      time.Time
	Kind    string // beam, crew, order, hail, done, fail, bridge
	CrewID  string
	OrderID string
	Text    string
	Payload json.RawMessage
}

// AppendLog appends an entry to the captain's log and returns its id. A zero TS
// defaults to now. The log is append-only: nothing updates or deletes rows.
func (s *Store) AppendLog(ctx context.Context, e LogEntry) (int64, error) {
	var id int64
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		var err error
		id, err = s.appendLog(ctx, tx, e)
		return err
	})
	return id, err
}

// GetSetting returns the value of setting key, or sql.ErrNoRows if it was
// never set.
func (s *Store) GetSetting(ctx context.Context, key string) (string, error) {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, key).Scan(&v)
	return v, err
}

// SetSetting sets setting key to value and appends entry to the captain's
// log, in one transaction. A setting already at value is left alone and
// nothing is logged.
func (s *Store) SetSetting(ctx context.Context, key, value string, entry LogEntry) error {
	return s.withTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `INSERT INTO settings (key, value) VALUES (?, ?)
			ON CONFLICT (key) DO UPDATE SET value = excluded.value WHERE value <> excluded.value`, key, value)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil || n == 0 {
			return err
		}
		_, err = s.appendLog(ctx, tx, entry)
		return err
	})
}

// execer is what appendLog needs from a *sql.DB or *sql.Tx.
type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

func (s *Store) appendLog(ctx context.Context, db execer, e LogEntry) (int64, error) {
	s.appends.Add(1)
	if e.TS.IsZero() {
		e.TS = time.Now()
	}
	if len(e.Payload) == 0 {
		e.Payload = json.RawMessage("{}")
	}
	res, err := db.ExecContext(ctx, `INSERT INTO log (ts, kind, crew_id, order_id, text, payload) VALUES (?, ?, ?, ?, ?, ?)`,
		e.TS.UTC(), e.Kind, e.CrewID, e.OrderID, e.Text, string(e.Payload))
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// ListLog returns the captain's log entries with id above sinceID, oldest
// first.
func (s *Store) ListLog(ctx context.Context, sinceID int64) ([]LogEntry, error) {
	return s.ListLogLimit(ctx, sinceID, 0)
}

// ListLogLimit is ListLog returning at most limit entries (the oldest ones
// above sinceID); limit <= 0 means no limit.
func (s *Store) ListLogLimit(ctx context.Context, sinceID int64, limit int) ([]LogEntry, error) {
	if limit <= 0 {
		limit = -1 // SQLite: no limit
	}
	return s.queryLog(ctx, `SELECT id, ts, kind, crew_id, order_id, text, payload FROM log WHERE id > ? ORDER BY id LIMIT ?`, sinceID, limit)
}

// ListLogLast returns the newest limit entries with id at most maxID,
// oldest first.
func (s *Store) ListLogLast(ctx context.Context, maxID int64, limit int) ([]LogEntry, error) {
	out, err := s.queryLog(ctx, `SELECT id, ts, kind, crew_id, order_id, text, payload FROM log WHERE id <= ? ORDER BY id DESC LIMIT ?`, maxID, limit)
	slices.Reverse(out)
	return out, err
}

// ListOrderLogLast returns the newest limit entries about order orderID,
// oldest first.
func (s *Store) ListOrderLogLast(ctx context.Context, orderID string, limit int) ([]LogEntry, error) {
	out, err := s.queryLog(ctx, `SELECT id, ts, kind, crew_id, order_id, text, payload FROM log WHERE order_id = ? ORDER BY id DESC LIMIT ?`,
		orderID, limit)
	slices.Reverse(out)
	return out, err
}

// ListOrderLogKind returns the entries of kind about order orderID,
// oldest first.
func (s *Store) ListOrderLogKind(ctx context.Context, orderID, kind string) ([]LogEntry, error) {
	return s.queryLog(ctx, `SELECT id, ts, kind, crew_id, order_id, text, payload FROM log WHERE order_id = ? AND kind = ? ORDER BY id`,
		orderID, kind)
}

// LastLogID returns the captain's log's last id, 0 when it is empty.
func (s *Store) LastLogID(ctx context.Context) (int64, error) {
	var id int64
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(id), 0) FROM log`).Scan(&id)
	return id, err
}

func (s *Store) queryLog(ctx context.Context, query string, args ...any) ([]LogEntry, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []LogEntry
	for rows.Next() {
		var (
			e       LogEntry
			payload string
		)
		if err := rows.Scan(&e.ID, &e.TS, &e.Kind, &e.CrewID, &e.OrderID, &e.Text, &payload); err != nil {
			return nil, err
		}
		e.Payload = json.RawMessage(payload)
		out = append(out, e)
	}
	return out, rows.Err()
}

// LastOrderLog returns the newest entry of kind about order orderID, or
// sql.ErrNoRows if there is none.
func (s *Store) LastOrderLog(ctx context.Context, orderID, kind string) (LogEntry, error) {
	var (
		e       LogEntry
		payload string
	)
	err := s.db.QueryRowContext(ctx, `SELECT id, ts, kind, crew_id, order_id, text, payload FROM log
		WHERE order_id = ? AND kind = ? ORDER BY id DESC LIMIT 1`, orderID, kind).
		Scan(&e.ID, &e.TS, &e.Kind, &e.CrewID, &e.OrderID, &e.Text, &payload)
	e.Payload = json.RawMessage(payload)
	return e, err
}

// LatestOrderLogTS returns the time of order orderID's latest captain's
// log entry, of any kind; sql.ErrNoRows when it has none. Every move of an
// order to an ended state logs an entry in the same transaction, so for an
// ended order it is when the order ended or later (#108).
func (s *Store) LatestOrderLogTS(ctx context.Context, orderID string) (time.Time, error) {
	var ts time.Time
	err := s.db.QueryRowContext(ctx, `SELECT ts FROM log WHERE order_id = ? ORDER BY id DESC LIMIT 1`, orderID).Scan(&ts)
	return ts, err
}

// OrderEnded reports whether state is one an order ends in: done, failed,
// belayed or withdrawn.
func OrderEnded(state string) bool { return slices.Contains(endedStates, state) }

// SeenInbox reports whether the message seq from crewID's tour tourNo was
// already recorded.
func (s *Store) SeenInbox(ctx context.Context, crewID string, tourNo int, seq uint64) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM inbox WHERE crew_id = ? AND tour_no = ? AND seq = ?`,
		crewID, tourNo, int64(seq)).Scan(&n)
	return n > 0, err
}

// InsertInboxSeq records that message seq from crewID's tour tourNo was
// received. inserted is false if the seq was already recorded, in which case
// nothing changes.
func (s *Store) InsertInboxSeq(ctx context.Context, crewID string, tourNo int, seq uint64) (inserted bool, err error) {
	err = s.withTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO inbox (crew_id, tour_no, seq, received_at) VALUES (?, ?, ?, ?)`,
			crewID, tourNo, int64(seq), time.Now().UTC())
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		inserted = n > 0
		return err
	})
	return inserted, err
}

// InboxTx is the transaction RecordInbox runs its callback in.
type InboxTx struct {
	ctx    context.Context
	tx     *sql.Tx
	s      *Store
	crewID string
	tourNo int
	seq    uint64
	// usage collects the orders whose usage rows RecordUsage wrote:
	// RecordInbox hands them to the log hook as refreshes once the
	// transaction commits (#120).
	usage *[]string
}

// SQL returns the underlying transaction, e.g. for dbos.WithSendTransaction:
// DBOS's tables live in the same SQLite file, so a DBOS send made on it
// commits or rolls back together with the inbox row.
func (t InboxTx) SQL() *sql.Tx { return t.tx }

// AppendLog appends a captain's log entry within the transaction (see
// Store.AppendLog).
func (t InboxTx) AppendLog(e LogEntry) (int64, error) { return t.s.appendLog(t.ctx, t.tx, e) }

// SetOrderProgress records pct and note as order id's latest progress, as
// reported by the message being recorded (#141). It changes the order only
// while it is away or hailing on the message's crew member and not parked,
// and only if no report from a later tour, or a later seq of the same
// tour, is recorded on it: messages may be recorded out of seq order. It
// reports whether it changed the order. The caller logs the report.
func (t InboxTx) SetOrderProgress(id string, pct float64, note string) (bool, error) {
	res, err := t.tx.ExecContext(t.ctx, `UPDATE orders SET progress_pct = ?, progress_note = ?, progress_tour = ?, progress_seq = ?
		WHERE id = ? AND state IN (?, ?) AND assignee = ? AND parked_at IS NULL
		AND (progress_pct IS NULL OR progress_tour < ? OR (progress_tour = ? AND progress_seq < ?))`,
		pct, note, t.tourNo, int64(t.seq), id, OrderAway, OrderHailing, t.crewID, t.tourNo, t.tourNo, int64(t.seq))
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// Usage is a row of order_usage: what one beam reported spending on an
// order, cumulative for the order on that beam (#118).
type Usage struct {
	Beam         string
	InputTokens  int64
	OutputTokens int64
	Turns        int64
	// Seq is the envelope seq of the usage message the row holds.
	Seq uint64
}

// RecordUsage upserts u as order id's usage on beam u.Beam, with the
// message's seq (#118). A row already holding a higher or equal seq is
// kept: the latest report is the one with the highest seq, and a replayed
// or late one changes nothing. An unknown order writes nothing. It reports
// whether it wrote the row. Counts are recorded whatever the order's state
// or assignee: what a beam spent was spent.
func (t InboxTx) RecordUsage(id string, u Usage) (bool, error) {
	res, err := t.tx.ExecContext(t.ctx, `INSERT INTO order_usage (order_id, beam, input_tokens, output_tokens, turns, seq)
		SELECT ?, ?, ?, ?, ?, ? WHERE EXISTS (SELECT 1 FROM orders WHERE id = ?)
		ON CONFLICT (order_id, beam) DO UPDATE SET input_tokens = excluded.input_tokens,
			output_tokens = excluded.output_tokens, turns = excluded.turns, seq = excluded.seq
		WHERE excluded.seq > order_usage.seq`,
		id, u.Beam, u.InputTokens, u.OutputTokens, u.Turns, int64(t.seq), id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	if n > 0 && t.usage != nil {
		*t.usage = append(*t.usage, id)
	}
	return n > 0, nil
}

// OrderUsage returns order id's usage rows, one per beam that reported
// any, by beam.
func (s *Store) OrderUsage(ctx context.Context, id string) ([]Usage, error) {
	return orderUsage(ctx, s.db, id)
}

// Order returns order id as the transaction reads it, without its repos,
// or sql.ErrNoRows.
func (t InboxTx) Order(id string) (Order, error) {
	return scanOrder(t.tx.QueryRowContext(t.ctx, `SELECT `+orderCols+` FROM orders WHERE id = ?`, id))
}

// OrderUsage is Store.OrderUsage, read in the transaction: the row the
// message being recorded wrote included.
func (t InboxTx) OrderUsage(id string) ([]Usage, error) { return orderUsage(t.ctx, t.tx, id) }

// OrderLogMarked reports whether order id's log holds an entry of kind
// whose payload's field is value (a string).
func (t InboxTx) OrderLogMarked(id, kind, field, value string) (bool, error) {
	var n int
	err := t.tx.QueryRowContext(t.ctx, `SELECT EXISTS (SELECT 1 FROM log WHERE order_id = ? AND kind = ?
		AND json_valid(payload) AND json_extract(payload, '$.' || ?) = ?)`, id, kind, field, value).Scan(&n)
	return n == 1, err
}

// orderUsage is OrderUsage on q, a *sql.DB or *sql.Tx.
func orderUsage(ctx context.Context, q interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}, id string) ([]Usage, error) {
	rows, err := q.QueryContext(ctx, `SELECT beam, input_tokens, output_tokens, turns, seq FROM order_usage
		WHERE order_id = ? ORDER BY beam`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Usage
	for rows.Next() {
		var u Usage
		var seq int64
		if err := rows.Scan(&u.Beam, &u.InputTokens, &u.OutputTokens, &u.Turns, &seq); err != nil {
			return nil, err
		}
		u.Seq = uint64(seq)
		out = append(out, u)
	}
	return out, rows.Err()
}

// RecordInbox records that message seq from crewID's tour tourNo was
// received and, in the same transaction, runs fn, which does whatever
// handling the message needs (a log entry, a DBOS send). Either both commit
// or neither does: if fn fails the inbox row isn't recorded, so the message
// is handled again when it is redelivered. If seq is already recorded, fn
// isn't run and recorded is false. fn is nil-able.
//
// This is what makes the comms pump's inbound path exactly-once across a
// bridge crash: the message is acked to the beam only after this commits.
//
// fn runs holding the store's write slot: it must not call a Store write
// method (it would wait forever).
func (s *Store) RecordInbox(ctx context.Context, crewID string, tourNo int, seq uint64, fn func(InboxTx) error) (recorded bool, err error) {
	var usage []string
	err = s.withTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO inbox (crew_id, tour_no, seq, received_at) VALUES (?, ?, ?, ?)`,
			crewID, tourNo, int64(seq), time.Now().UTC())
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil || n == 0 {
			return err
		}
		recorded = true
		if fn == nil {
			return nil
		}
		return fn(InboxTx{ctx: ctx, tx: tx, s: s, crewID: crewID, tourNo: tourNo, seq: seq, usage: &usage})
	})
	if err != nil {
		return false, err
	}
	s.publishRefresh(usage)
	return recorded, nil
}

// ContiguousInboxSeq returns the highest N such that messages 1..N from
// crewID's tour tourNo are all recorded in the inbox, or 0 if seq 1 isn't.
// Beam seqs start at 1 on each tour's beam, so this is what the bridge acks
// and attaches with (--since N).
func (s *Store) ContiguousInboxSeq(ctx context.Context, crewID string, tourNo int) (uint64, error) {
	// With seq 1 recorded, the first run of consecutive seqs starts at 1 and
	// ends at the smallest recorded seq whose successor is missing.
	var n int64
	err := s.db.QueryRowContext(ctx, `
		SELECT COALESCE(MIN(i.seq), 0) FROM inbox i
		WHERE i.crew_id = ?1 AND i.tour_no = ?2 AND i.seq >= 1
		  AND EXISTS (SELECT 1 FROM inbox WHERE crew_id = ?1 AND tour_no = ?2 AND seq = 1)
		  AND NOT EXISTS (SELECT 1 FROM inbox j WHERE j.crew_id = ?1 AND j.tour_no = ?2 AND j.seq = i.seq + 1)`,
		crewID, tourNo).Scan(&n)
	if err != nil {
		return 0, err
	}
	return uint64(n), nil
}

// withTx runs fn in a transaction, committing if it returns nil, then
// hands the log hook what it appended. With _txlock=immediate the
// transaction holds the write lock from its first read.
func (s *Store) withTx(ctx context.Context, fn func(*sql.Tx) error) error {
	// A counter that didn't move by the commit means this transaction
	// appended nothing. Another writer's bump only costs a needless read.
	appends := s.appends.Load()
	if err := s.inTx(ctx, fn); err != nil {
		return err
	}
	if s.appends.Load() != appends {
		s.publishLog(ctx)
	}
	return nil
}

// inTx runs fn in a write transaction and commits it if fn returns nil.
// The store's writers take the writeSem slot first, so they queue in Go
// and at most one of them polls SQLite's busy handler against DBOS's
// writers (#204). A writer whose ctx ends while it queues returns
// ctx.Err().
func (s *Store) inTx(ctx context.Context, fn func(*sql.Tx) error) error {
	select {
	case s.writeSem <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-s.writeSem }()
	tx, err := s.begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// begin starts a write transaction. BEGIN IMMEDIATE takes the write lock
// or fails having done nothing, so a BEGIN that gets SQLITE_BUSY after
// busy_timeout is retried, with backoff, for up to s.busyRetry or until
// ctx ends (#204). An error from a ctx that ended wraps both ctx.Err() and
// the last SQLITE_BUSY.
func (s *Store) begin(ctx context.Context) (*sql.Tx, error) {
	deadline := time.Now().Add(s.busyRetry)
	wait := 10 * time.Millisecond
	for {
		tx, err := s.db.BeginTx(ctx, nil)
		if err == nil || !isBusy(err) || !time.Now().Before(deadline) {
			return tx, err
		}
		if s.onBusy != nil {
			s.onBusy()
		}
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return nil, fmt.Errorf("%w: %w", ctx.Err(), err)
		case <-t.C:
		}
		wait = min(2*wait, 500*time.Millisecond)
	}
}

// isBusy reports whether err is SQLITE_BUSY or one of its extended codes.
func isBusy(err error) bool {
	var se *sqlite.Error
	return errors.As(err, &se) && se.Code()&0xff == sqlite3.SQLITE_BUSY
}

// SetLogHook makes the store hand fn the captain's log entries each
// successful commit appended, oldest first, after the commit and in the
// committing goroutine (#72). Entries already in the log aren't handed
// over. fn must not block: it holds up the writer and every other
// commit's hook. Calls to fn don't overlap, and each holds the entries
// above the previous call's. A call may instead hold refreshes, entries
// with ID 0 and kind RefreshKind that are in no log (publishRefresh).
//
// SQLite has a single writer, and _txlock=immediate takes the write lock
// at BEGIN, so log ids rise in commit order: after any commit every entry
// up to the newest one is committed. publishLog reads the entries above
// the last one handed over. A commit whose read fails reports the error
// to onErr (nil-able) and leaves its entries to the next commit's.
func (s *Store) SetLogHook(ctx context.Context, fn func([]LogEntry), onErr func(error)) error {
	s.hookMu.Lock()
	defer s.hookMu.Unlock()
	last, err := s.LastLogID(ctx)
	if err != nil {
		return err
	}
	s.logHook, s.logHookErr, s.logHookLast = fn, onErr, last
	return nil
}

// publishLog hands the log entries above logHookLast to the log hook, if
// one is set. Called after each commit.
func (s *Store) publishLog(ctx context.Context) {
	s.hookMu.Lock()
	defer s.hookMu.Unlock()
	if s.logHook == nil {
		return
	}
	entries, err := s.ListLog(context.WithoutCancel(ctx), s.logHookLast)
	if err != nil {
		if s.logHookErr != nil {
			s.logHookErr(fmt.Errorf("read log for the hook: %w", err))
		}
		return
	}
	if len(entries) == 0 {
		return
	}
	s.logHookLast = entries[len(entries)-1].ID
	s.logHook(entries)
}

// RefreshKind is the kind of a refresh on the log hook (#120): a LogEntry
// with ID 0 that is in no log, naming an order whose view changed without
// an entry, as a usage report changes it. The console sends that order's
// view with no event id.
const RefreshKind = "refresh"

// publishRefresh hands the log hook a refresh (RefreshKind) for each of
// orders, if a hook is set and there are any. Called after a commit.
func (s *Store) publishRefresh(orders []string) {
	if len(orders) == 0 {
		return
	}
	s.hookMu.Lock()
	defer s.hookMu.Unlock()
	if s.logHook == nil {
		return
	}
	entries := make([]LogEntry, len(orders))
	for i, id := range orders {
		entries[i] = LogEntry{Kind: RefreshKind, OrderID: id}
	}
	s.logHook(entries)
}

// nextID returns the next id for table, formatted from COUNT(*)+1.
func nextID(ctx context.Context, tx *sql.Tx, table, format string) (string, error) {
	// COUNT(*)+1 is safe: orders/hails are never deleted and _txlock=immediate serializes writers.
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+table).Scan(&n); err != nil {
		return "", err
	}
	return fmt.Sprintf(format, n+1), nil
}

// withField returns the JSON object payload (empty means {}) with key set to
// value.
func withField(payload json.RawMessage, key string, value any) (json.RawMessage, error) {
	m := map[string]json.RawMessage{}
	if len(payload) > 0 {
		if err := json.Unmarshal(payload, &m); err != nil {
			return nil, fmt.Errorf("log payload: %w", err)
		}
	}
	v, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	m[key] = v
	return json.Marshal(m)
}
