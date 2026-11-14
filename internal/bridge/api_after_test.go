package bridge

import (
	"log/slog"
	"path/filepath"
	"slices"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/tscolari/bmus/internal/store"
)

// The order-done settle (settleReport) wakes the crew on deck when an
// order waiting on the settled one is now ready to claim (#142), and
// wakes nobody when the settle readies nothing: an order still waiting on
// another, or an order with nothing waiting on it.
func TestSettleReportWakesReady(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), store.DBFile))
	require.NoError(t, err)
	defer st.Close()
	var (
		mu    sync.Mutex
		woken []string
	)
	d := &Daemon{store: st, logger: slog.New(slog.DiscardHandler), wakeSend: func(tour string, sig CrewSignal) error {
		mu.Lock()
		defer mu.Unlock()
		woken = append(woken, tour)
		return nil
	}}
	wakes := func() []string {
		mu.Lock()
		defer mu.Unlock()
		out := slices.Clone(woken)
		woken = nil
		return out
	}
	e := store.LogEntry{Kind: "test"}
	require.NoError(t, st.InsertCrew(ctx, store.Crew{ID: "sorell", Name: "Sorell", Division: "ops"}, e))
	require.NoError(t, st.InsertCrew(ctx, store.Crew{ID: "kirk", Name: "Kirk", Division: "ops"}, e))
	require.NoError(t, st.InsertCrew(ctx, store.Crew{ID: "spock", Name: "Spock", Status: store.CrewAway}, e))
	insert := func(o store.Order) string {
		id, err := st.InsertOrder(ctx, o, e)
		require.NoError(t, err)
		return id
	}
	alone := insert(store.Order{Title: "alone", Assignee: "spock", State: store.OrderAway})
	a := insert(store.Order{Title: "a", Assignee: "spock", State: store.OrderAway})
	b := insert(store.Order{Title: "b", Assignee: "spock", State: store.OrderAway})
	insert(store.Order{Title: "after both", Division: "ops", After: []string{a, b}})

	for _, id := range []string{alone, a} {
		current, err := d.settleReport(ctx, id, store.OrderDone, e, e)
		require.NoError(t, err)
		require.Equal(t, store.OrderDone, current)
		require.Empty(t, wakes(), "%s readies nothing", id)
	}
	_, err = d.settleReport(ctx, b, store.OrderDone, e, e)
	require.NoError(t, err)
	require.ElementsMatch(t, []string{tourWorkflowID("sorell", 0), tourWorkflowID("kirk", 0)}, wakes(),
		"the crew on deck, not spock, who is away")
}
