package store

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// tableColumns is table's columns as pragma_table_info describes them,
// by name: type, NOT NULL and default.
func tableColumns(t *testing.T, db *sql.DB, table string) map[string]string {
	t.Helper()
	rows, err := db.QueryContext(t.Context(), `SELECT name, type, "notnull", COALESCE(dflt_value, 'NULL') FROM pragma_table_info(?)`, table)
	require.NoError(t, err)
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var name, typ, def string
		var notNull bool
		require.NoError(t, rows.Scan(&name, &typ, &notNull, &def))
		out[name] = typ + " notnull=" + map[bool]string{true: "1", false: "0"}[notNull] + " default=" + def
	}
	require.NoError(t, rows.Err())
	return out
}

// A database made with the schema from before #111
// (testdata/schema-before-111.sql, schema.sql as it was) opens, keeps its
// rows, and gains the beams and orders columns and the order_usage,
// order_egress and backups tables, the same as a new database has them.
// Opening it again changes nothing.
func TestOpenAddsPhase4Schema(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	old, err := os.ReadFile(filepath.Join("testdata", "schema-before-111.sql"))
	require.NoError(t, err)
	path := filepath.Join(t.TempDir(), DBFile)
	db, err := sql.Open("sqlite", path)
	require.NoError(t, err)
	_, err = db.Exec(string(old))
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO crew (id, name) VALUES ('sorell', 'Sorell');
		INSERT INTO beams (uuid, alias, crew_id, tour_no, created_at, expires_at)
		VALUES ('old-beam', 'old-beam', 'sorell', 0, '2026-11-01 10:00:00+00:00', '2026-11-01 22:00:00+00:00');
		INSERT INTO orders (id, title, state, assignee, budget_tokens) VALUES ('ORD-0001', 'old', 'done', 'sorell', 1000);`)
	require.NoError(t, err)
	require.NoError(t, db.Close())

	fresh := openTest(t)
	for range 2 {
		s, err := Open(ctx, path)
		require.NoError(t, err)
		for _, table := range []string{"beams", "orders", "order_usage", "order_egress", "backups"} {
			require.Equal(t, tableColumns(t, fresh.db, table), tableColumns(t, s.db, table), table)
		}

		beams, err := s.ListBeams(ctx, "sorell")
		require.NoError(t, err)
		require.Len(t, beams, 1)
		require.Equal(t, "old-beam", beams[0].Alias)
		require.Nil(t, beams[0].RemovedAt, "removed_at is added NULL")
		var labels string
		require.NoError(t, s.db.QueryRowContext(ctx, `SELECT labels FROM beams WHERE uuid = 'old-beam'`).Scan(&labels))
		require.Equal(t, "{}", labels)

		o, err := s.GetOrder(ctx, "ORD-0001")
		require.NoError(t, err)
		require.Equal(t, "old", o.Title)
		require.Equal(t, int64(1000), o.BudgetTokens)
		var turns, extended int
		require.NoError(t, s.db.QueryRowContext(ctx, `SELECT budget_turns, budget_extended FROM orders WHERE id = 'ORD-0001'`).Scan(&turns, &extended))
		require.Zero(t, turns)
		require.Zero(t, extended)
		require.NoError(t, s.Close())
	}

	// The new tables take rows, keyed as the plan has them.
	s, err := Open(ctx, path)
	require.NoError(t, err)
	defer s.Close()
	hailID, err := s.InsertHail(ctx, Hail{OrderID: "ORD-0001", CrewID: "sorell", Category: "egress", Question: "?"}, testEntry)
	require.NoError(t, err)
	_, err = s.db.ExecContext(ctx, `INSERT INTO order_usage (order_id, beam, input_tokens, output_tokens, turns, seq)
		VALUES ('ORD-0001', 'old-beam', 10, 20, 3, 4)`)
	require.NoError(t, err)
	_, err = s.db.ExecContext(ctx, `INSERT INTO order_usage (order_id, beam) VALUES ('ORD-0001', 'old-beam')`)
	require.ErrorContains(t, err, "UNIQUE", "one row per order and beam")
	_, err = s.db.ExecContext(ctx, `INSERT INTO order_egress (order_id, domain, hail_id) VALUES ('ORD-0001', 'example.com', ?)`, hailID)
	require.NoError(t, err)
	_, err = s.db.ExecContext(ctx, `INSERT INTO order_egress (order_id, domain, hail_id) VALUES ('ORD-0001', 'example.com', ?)`, hailID)
	require.ErrorContains(t, err, "UNIQUE", "one row per order and domain")
	_, err = s.db.ExecContext(ctx, `INSERT INTO backups (started_at, dir) VALUES (?, '/backups/x')`, time.Now().UTC())
	require.NoError(t, err)
	var finished sql.NullTime
	var bytes int64
	var backupErr string
	require.NoError(t, s.db.QueryRowContext(ctx, `SELECT finished_at, bytes, error FROM backups`).Scan(&finished, &bytes, &backupErr))
	require.False(t, finished.Valid)
	require.Zero(t, bytes)
	require.Empty(t, backupErr)
}

// SetBeamRemoved marks every unmarked row of the crew member's with the
// alias. A row already marked keeps its time, and another crew member's
// row and an unknown alias are untouched.
func TestSetBeamRemoved(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTest(t)
	require.NoError(t, s.InsertCrew(ctx, Crew{ID: "sorell", Name: "Sorell"}, testEntry))
	require.NoError(t, s.InsertCrew(ctx, Crew{ID: "tuvok", Name: "Tuvok"}, testEntry))
	exp := time.Now().Add(time.Hour)
	require.NoError(t, s.InsertBeam(ctx, BeamRecord{UUID: "u1", Alias: "a", CrewID: "sorell", ExpiresAt: exp}, testEntry))
	require.NoError(t, s.InsertBeam(ctx, BeamRecord{UUID: "u2", Alias: "b", CrewID: "sorell", TourNo: 1, ExpiresAt: exp}, testEntry))
	require.NoError(t, s.InsertBeam(ctx, BeamRecord{UUID: "u3", Alias: "a", CrewID: "sorell", TourNo: 2, ExpiresAt: exp}, testEntry))
	require.NoError(t, s.InsertBeam(ctx, BeamRecord{UUID: "u4", Alias: "a", CrewID: "tuvok", ExpiresAt: exp}, testEntry))

	first := time.Date(2026, 11, 13, 10, 0, 0, 0, time.UTC)
	require.NoError(t, s.SetBeamRemoved(ctx, "sorell", "a", first))
	require.NoError(t, s.SetBeamRemoved(ctx, "sorell", "missing", first))
	require.NoError(t, s.SetBeamRemoved(ctx, "sorell", "a", first.Add(time.Minute)))
	beams, err := s.ListBeams(ctx, "sorell")
	require.NoError(t, err)
	require.Len(t, beams, 3)
	require.NotNil(t, beams[0].RemovedAt)
	require.True(t, first.Equal(*beams[0].RemovedAt), "a marked row keeps its time")
	require.Nil(t, beams[1].RemovedAt, "another alias")
	require.NotNil(t, beams[2].RemovedAt)
	require.True(t, first.Equal(*beams[2].RemovedAt))
	other, err := s.ListBeams(ctx, "tuvok")
	require.NoError(t, err)
	require.Nil(t, other[0].RemovedAt, "another crew member's beam")
}
