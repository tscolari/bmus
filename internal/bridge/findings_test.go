package bridge

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tscolari/bmus/internal/comms"
	"github.com/tscolari/bmus/internal/store"
)

// #153: a holodeck script's finding steps reach the bridge's findings table
// and the order's log before its report settles the order, the second
// finding with the same key replacing the first. Nothing reaches the
// mission but the report.
func TestHolodeckFindingsRecorded(t *testing.T) {
	t.Parallel()
	b := newHolodeckBridge(t, fixture(t, "findings.json"))
	hd := b.start(t)
	hd.addCrew(t)
	id := hd.createOrder(t)
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		require.Len(c, workflows(c, hd.d, missionWorkflowID(id)), 1)
	}, 10*time.Second, 20*time.Millisecond)

	res := missionResult(t, hd.d, id)
	require.Equal(t, MissionDone, res.Status, "%+v", res)
	require.Equal(t, store.OrderDone, hd.order(t, id).State)

	got, err := hd.d.store.OrderFindings(t.Context(), id)
	require.NoError(t, err)
	require.Len(t, got, 2)
	for i := range got {
		require.False(t, got[i].ReceivedAt.IsZero())
		got[i].ReceivedAt = time.Time{}
	}
	require.Equal(t, []store.Finding{
		{OrderID: id, Key: "flaky-test", CrewID: "sorell", Title: "TestSnapshot races the compactor",
			Body: "The flake is a race with compaction.", Labels: []string{}, Severity: "high", BlockedBy: []string{}, State: store.FindingNew},
		{OrderID: id, Key: "snapshot-leak", CrewID: "sorell", Title: "Snapshot writer leaks a file handle on error",
			Body: "Short writes return without closing the temp file.", Labels: []string{"bug", "raft"}, Severity: "medium",
			BlockedBy: []string{"flaky-test", "#12"}, State: store.FindingNew},
	}, got)

	entries := hd.logEntries(t)
	var logged []string
	findingsBefore := -1
	for i, e := range entries {
		if e.Kind == comms.FindingLogKind {
			require.Equal(t, id, e.OrderID)
			require.Equal(t, "sorell", e.CrewID)
			logged = append(logged, e.Text)
			findingsBefore = i
		}
	}
	require.Equal(t, []string{
		"finding flaky-test: TestSnapshot is flaky",
		"finding snapshot-leak: Snapshot writer leaks a file handle on error",
		"finding flaky-test: TestSnapshot races the compactor",
	}, logged)
	for _, e := range entries[:findingsBefore] {
		require.NotEqual(t, logDone, e.Kind, "every finding is logged before the order is done")
	}
	requireNoBackstop(t, entries)
}
