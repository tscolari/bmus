package bridge

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tscolari/bmus/internal/store"
	"github.com/tscolari/bmus/internal/transporter"
)

// The budget ledger (#118): an order with no budget of its own takes its
// division's, and its first send carries all of it. The first beam's
// usage report becomes an order_usage row; the beam's watch change moves
// the order to a new beam, whose order send carries the budget less that
// usage, and whose own report adds a second row.
func TestBudgetLedger(t *testing.T) {
	t.Parallel()
	script := filepath.Join(t.TempDir(), "script.json")
	require.NoError(t, os.WriteFile(script, []byte(`[
		{"at_pct": 10, "usage": {"input_tokens": 3000, "output_tokens": 500, "turns": 8}},
		{"at_pct": 50, "sleep_ms": 120000},
		{"at_pct": 100, "report": {"summary": "never reached"}}]`), 0o600))
	b := newHolodeckBridge(t, script)
	const lead = time.Hour
	short, err := transporter.NewHolodeck(transporter.HolodeckConfig{
		Root: b.root, Combadge: combadgeBundle(t),
		Harness:  []string{"--harness", "holodeck", "--script", script},
		Lifetime: lead + 10*time.Second,
	})
	require.NoError(t, err)
	b.provider, b.lead = &watchProvider{Holodeck: short}, lead
	b.divisions = map[string]DivisionConfig{"science": {Budget: Budget{Tokens: 10000, Turns: 50}}}

	hd := b.start(t)
	c := hd.addCrewWith(t, store.Crew{ID: "sorell", Name: "Sorell", Rank: "Lieutenant", Division: "science"})
	id := hd.createOrder(t)

	// budgets is each run's (budget_tokens, budget_turns) as its first
	// step logged it, by beam run.
	budgets := func(t require.TestingT) [][2]float64 {
		var out [][2]float64
		for _, e := range readSteps(t, b.stepLog) {
			if e["order"] != id || e["index"] != float64(0) {
				continue
			}
			tokens, _ := e["budget_tokens"].(float64)
			turns, _ := e["budget_turns"].(float64)
			out = append(out, [2]float64{tokens, turns})
		}
		return out
	}
	usage := func(t require.TestingT) []store.Usage {
		u, err := hd.d.store.OrderUsage(context.Background(), id)
		require.NoError(t, err)
		return u
	}

	require.EventuallyWithT(t, func(ct *assert.CollectT) {
		require.Equal(ct, [][2]float64{{10000, 50}}, budgets(ct), "the division's whole budget")
		u := usage(ct)
		require.Len(ct, u, 1)
		require.Equal(ct, store.Usage{Beam: c.CurrentBeam, InputTokens: 3000, OutputTokens: 500, Turns: 8, Seq: u[0].Seq}, u[0])
	}, 15*time.Second, 20*time.Millisecond)

	// The new beam's serve runs the second script.
	require.NoError(t, os.WriteFile(script, []byte(`[
		{"at_pct": 50, "usage": {"input_tokens": 1000, "output_tokens": 200, "turns": 3}},
		{"at_pct": 100, "report": {"summary": "done after the watch change"}}]`), 0o600))

	res := missionResult(t, hd.d, id)
	require.Equal(t, MissionDone, res.Status, "%+v", res)
	require.Equal(t, [][2]float64{{10000, 50}, {10000 - 3500, 50 - 8}}, budgets(t), "the second send carries what remains")

	var cr store.Crew
	require.EventuallyWithT(t, func(ct *assert.CollectT) {
		cr = hd.crew(ct, "sorell")
		require.Equal(ct, 1, cr.TourNo)
	}, 10*time.Second, 50*time.Millisecond)
	require.NotEqual(t, c.CurrentBeam, cr.CurrentBeam)
	got := map[string][3]int64{}
	for _, u := range usage(t) {
		got[u.Beam] = [3]int64{u.InputTokens, u.OutputTokens, u.Turns}
	}
	require.Equal(t, map[string][3]int64{c.CurrentBeam: {3000, 500, 8}, cr.CurrentBeam: {1000, 200, 3}}, got)
}
