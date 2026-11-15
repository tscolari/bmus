package store

import (
	"database/sql"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// RecordFinding keeps a finding while its order is open and assigned to
// the recording crew member, replaces a new row with the same key, and
// drops the rest: an unknown, ended or split order, another crew member's
// order, a key past MaxFindings and a key whose row the findings workflow
// moved on (#153).
func TestRecordFinding(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s := openTest(t)
	id, err := s.InsertOrder(ctx, Order{Title: "t", Assignee: "a"}, LogEntry{Kind: "order"})
	require.NoError(t, err)
	seq := uint64(0)
	recordAt := func(crew string, tour int, seq uint64, order string, f Finding) error {
		t.Helper()
		var dropped error
		recorded, err := s.RecordInbox(ctx, crew, tour, seq, func(tx InboxTx) error {
			dropped = tx.RecordFinding(order, f, time.Time{})
			if errors.Is(dropped, ErrFindingDropped) {
				return nil
			}
			return dropped
		})
		require.NoError(t, err)
		require.True(t, recorded)
		return dropped
	}
	record := func(crew, order string, f Finding) error {
		t.Helper()
		seq++
		return recordAt(crew, 0, seq, order, f)
	}
	findings := func(order string) []Finding {
		t.Helper()
		got, err := s.OrderFindings(ctx, order)
		require.NoError(t, err)
		for i := range got {
			require.False(t, got[i].ReceivedAt.IsZero())
			got[i].ReceivedAt = got[i].ReceivedAt.UTC()
		}
		return got
	}

	// Kept, with its fields; one without labels or blockers stores [].
	require.NoError(t, record("a", id, Finding{Key: "leak", Title: "Leak", Body: "b", Labels: []string{"bug"},
		Severity: "high", BlockedBy: []string{"flaky", "#12"}, CrewID: "ignored", State: "filed"}))
	require.NoError(t, record("a", id, Finding{Key: "flaky", Title: "Flaky"}))
	got := findings(id)
	require.Len(t, got, 2)
	first := got[1].ReceivedAt
	require.Equal(t, Finding{OrderID: id, Key: "leak", CrewID: "a", Title: "Leak", Body: "b", Labels: []string{"bug"},
		Severity: "high", BlockedBy: []string{"flaky", "#12"}, ReceivedAt: first, State: FindingNew}, got[1])
	require.Equal(t, Finding{OrderID: id, Key: "flaky", CrewID: "a", Title: "Flaky", Labels: []string{}, BlockedBy: []string{},
		ReceivedAt: got[0].ReceivedAt, State: FindingNew}, got[0])

	// The same key replaces a new row, every field of it.
	require.NoError(t, record("a", id, Finding{Key: "leak", Title: "Two leaks"}))
	got = findings(id)
	require.Len(t, got, 2)
	require.Equal(t, "Two leaks", got[1].Title)
	require.Empty(t, got[1].Body)
	require.Empty(t, got[1].Labels)
	require.Empty(t, got[1].Severity)
	require.Empty(t, got[1].BlockedBy)
	require.False(t, got[1].ReceivedAt.Before(first))

	// Each kept finding logged, with its tour and seq.
	logged := func() []string {
		t.Helper()
		entries, err := s.ListLog(ctx, 0)
		require.NoError(t, err)
		var out []string
		for _, e := range entries {
			if e.Kind == FindingLogKind {
				require.Equal(t, id, e.OrderID)
				out = append(out, e.CrewID+" "+e.Text+" "+string(e.Payload))
			}
		}
		return out
	}
	require.Equal(t, []string{
		`a finding leak: Leak {"tour":0,"seq":1,"key":"leak"}`,
		`a finding flaky: Flaky {"tour":0,"seq":2,"key":"flaky"}`,
		`a finding leak: Two leaks {"tour":0,"seq":3,"key":"leak"}`,
	}, logged())

	// Recorded out of seq order after a reconnect: seq 10 then seq 9 of the
	// same key, and the older one is dropped. A later tour wins over a
	// higher seq of an earlier one.
	require.NoError(t, recordAt("a", 0, 10, id, Finding{Key: "leak", Title: "Newer"}))
	err = recordAt("a", 0, 9, id, Finding{Key: "leak", Title: "Older"})
	require.ErrorIs(t, err, ErrFindingDropped)
	require.ErrorContains(t, err, "already has a later version than tour 0 seq 9")
	require.NoError(t, recordAt("a", 1, 2, id, Finding{Key: "leak", Title: "Next tour"}))
	err = recordAt("a", 0, 11, id, Finding{Key: "leak", Title: "Earlier tour"})
	require.ErrorIs(t, err, ErrFindingDropped)
	require.Equal(t, "Next tour", findings(id)[1].Title)
	require.Len(t, logged(), 5)
	seq = 11

	// Reassigned, the order takes the next crew member's findings: its tour
	// numbers are its own.
	assign := func(crew string) {
		t.Helper()
		_, err := s.db.ExecContext(ctx, `UPDATE orders SET assignee = ? WHERE id = ?`, crew, id)
		require.NoError(t, err)
	}
	assign("b")
	require.NoError(t, recordAt("b", 0, 1, id, Finding{Key: "leak", Title: "From b"}))
	require.Equal(t, "b", findings(id)[1].CrewID)
	assign("a")

	// Fill the order to MaxFindings: the next new key is dropped, a known
	// one still replaces.
	for i := 2; i < MaxFindings; i++ {
		require.NoError(t, record("a", id, Finding{Key: fmt.Sprintf("k%02d", i), Title: "t"}))
	}
	require.Len(t, findings(id), MaxFindings)
	err = record("a", id, Finding{Key: "one-more", Title: "t"})
	require.ErrorIs(t, err, ErrFindingDropped)
	require.ErrorContains(t, err, "already has 20 findings")
	require.Len(t, findings(id), MaxFindings)
	require.NoError(t, record("a", id, Finding{Key: "k02", Title: "Revised"}))
	require.Len(t, findings(id), MaxFindings)

	// A row the findings workflow moved on isn't replaced.
	_, err = s.db.ExecContext(ctx, `UPDATE findings SET state = ? WHERE order_id = ? AND key = ?`, FindingFiled, id, "flaky")
	require.NoError(t, err)
	err = record("a", id, Finding{Key: "flaky", Title: "Flaky again"})
	require.ErrorIs(t, err, ErrFindingDropped)
	require.ErrorContains(t, err, "already filed")
	got = findings(id)
	require.Equal(t, "Flaky", got[0].Title)
	require.Equal(t, FindingFiled, got[0].State)

	// Another crew member's order, and an unknown one.
	other, err := s.InsertOrder(ctx, Order{Title: "o", Assignee: "a"}, LogEntry{Kind: "order"})
	require.NoError(t, err)
	err = record("b", other, Finding{Key: "x", Title: "t"})
	require.ErrorIs(t, err, ErrFindingDropped)
	require.ErrorContains(t, err, "not assigned to b")
	err = record("a", "ORD-9999", Finding{Key: "x", Title: "t"})
	require.ErrorIs(t, err, ErrFindingDropped)
	require.ErrorContains(t, err, "no order ORD-9999")

	// An ended or split order keeps nothing, whoever its assignee.
	for _, state := range []string{OrderDone, OrderFailed, OrderBelayed, OrderWithdrawn, OrderSplit} {
		_, err = s.db.ExecContext(ctx, `UPDATE orders SET state = ? WHERE id = ?`, state, other)
		require.NoError(t, err)
		err = record("a", other, Finding{Key: "x", Title: "t"})
		require.ErrorIs(t, err, ErrFindingDropped, state)
		require.ErrorContains(t, err, "is "+state)
	}
	// Open again, it keeps one: away and hailing as well as directed.
	for _, state := range []string{OrderAway, OrderHailing} {
		_, err = s.db.ExecContext(ctx, `UPDATE orders SET state = ? WHERE id = ?`, state, other)
		require.NoError(t, err)
		require.NoError(t, record("a", other, Finding{Key: "x-" + state, Title: "t"}))
	}
	require.Len(t, findings(other), 2)
	require.Len(t, findings(id), MaxFindings, "the other order's findings count apart")
}

// The findings workflow's writes (#155): triage drops and adds blockers to
// new rows only, a ref is stored without settling, and a settle moves a
// new row once, logging it.
func TestFindingWorkflowWrites(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s := openTest(t)
	id, err := s.InsertOrder(ctx, Order{Title: "t", Assignee: "a"}, LogEntry{Kind: "order"})
	require.NoError(t, err)
	for i, k := range []string{"a", "b", "c"} {
		_, err := s.RecordInbox(ctx, "a", 0, uint64(i+1), func(tx InboxTx) error {
			return tx.RecordFinding(id, Finding{Key: k, Title: k, BlockedBy: []string{"#1"}}, time.Time{})
		})
		require.NoError(t, err)
	}
	n, err := s.CountFindings(ctx, id)
	require.NoError(t, err)
	require.Equal(t, 3, n)

	settled, err := s.SettleFinding(ctx, id, "c", FindingFiled, "#9", "", LogEntry{Kind: "order", OrderID: id, Text: "c filed"})
	require.NoError(t, err)
	require.True(t, settled)
	require.NoError(t, s.TriageFindings(ctx, id, []FindingTriage{
		{Key: "a", Drop: true, Why: "noise", DuplicateOf: "#2"},
		{Key: "b", AddBlockedBy: []string{"#1", "a"}},
		{Key: "c", Drop: true, Why: "too late"},
		{Key: "missing", Drop: true},
	}, LogEntry{Kind: "order", OrderID: id, Text: "triaged"}))
	// A re-run of the step writes nothing twice.
	require.NoError(t, s.TriageFindings(ctx, id, []FindingTriage{{Key: "b", AddBlockedBy: []string{"c"}}},
		LogEntry{Kind: "order", OrderID: id, Text: "triaged"}))
	require.NoError(t, s.AppendLogOnce(ctx, id, "notes", LogEntry{Kind: "order", OrderID: id, Text: "note 1"},
		LogEntry{Kind: "order", OrderID: id, Text: "note 2"}))
	require.NoError(t, s.AppendLogOnce(ctx, id, "notes", LogEntry{Kind: "order", OrderID: id, Text: "note 1"}))
	a, err := s.GetFinding(ctx, id, "a")
	require.NoError(t, err)
	require.Equal(t, FindingDropped, a.State)
	require.Equal(t, "noise", a.Why)
	require.Equal(t, "#2", a.DuplicateOf)
	b, err := s.GetFinding(ctx, id, "b")
	require.NoError(t, err)
	require.Equal(t, FindingNew, b.State)
	require.Equal(t, []string{"#1", "a"}, b.BlockedBy)
	c, err := s.GetFinding(ctx, id, "c")
	require.NoError(t, err)
	require.Equal(t, FindingFiled, c.State, "a settled row is left alone")
	require.Equal(t, "#9", c.Ref)

	require.NoError(t, s.SetFindingRef(ctx, id, "b", "#5"))
	b, err = s.GetFinding(ctx, id, "b")
	require.NoError(t, err)
	require.Equal(t, FindingNew, b.State)
	require.Equal(t, "#5", b.Ref)
	settled, err = s.SettleFinding(ctx, id, "b", FindingFailed, "", "boom", LogEntry{Kind: "order", OrderID: id, Text: "b failed"})
	require.NoError(t, err)
	require.True(t, settled)
	b, err = s.GetFinding(ctx, id, "b")
	require.NoError(t, err)
	require.Equal(t, FindingFailed, b.State)
	require.Equal(t, "#5", b.Ref, "an empty ref keeps the stored one")
	require.Equal(t, "boom", b.Why)
	settled, err = s.SettleFinding(ctx, id, "b", FindingFiled, "#6", "", LogEntry{Kind: "order", OrderID: id, Text: "b again"})
	require.NoError(t, err)
	require.False(t, settled)

	entries, err := s.ListLog(ctx, 0)
	require.NoError(t, err)
	var texts []string
	for _, e := range entries {
		if e.Kind == "order" && e.OrderID == id && e.Text != "" {
			texts = append(texts, e.Text)
		}
	}
	require.Equal(t, []string{"c filed", "triaged", "note 1", "note 2", "b failed"}, texts)
	_, err = s.GetFinding(ctx, id, "missing")
	require.ErrorIs(t, err, sql.ErrNoRows)
}
