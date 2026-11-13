package bridge

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tscolari/bmus/internal/store"
	"github.com/tscolari/bmus/internal/transporter"
)

// ledgerProvider is a watchProvider whose later beams run script later:
// serve takes the last --script it is given.
type ledgerProvider struct {
	*watchProvider
	later string

	mu    sync.Mutex
	first string
}

func (p *ledgerProvider) Create(ctx context.Context) (transporter.Beam, error) {
	beam, err := p.watchProvider.Create(ctx)
	p.mu.Lock()
	defer p.mu.Unlock()
	if err == nil && p.first == "" {
		p.first = beam.Alias
	}
	return beam, err
}

func (p *ledgerProvider) StartCombadge(ctx context.Context, beam transporter.Beam, spec transporter.CombadgeSpec) error {
	p.mu.Lock()
	later := beam.Alias != p.first
	p.mu.Unlock()
	if later {
		spec.Args = append(slices.Clone(spec.Args), "--script", p.later)
	}
	return p.watchProvider.StartCombadge(ctx, beam, spec)
}

// The budget ledger (#118): an order with no budget of its own takes its
// division's, and its first send carries that limit with nothing used.
// The first beam's usage report becomes an order_usage row; the beam's
// watch change moves the order to a new beam, whose order send carries the
// same limit and the first beam's usage, and whose own report adds a
// second row.
func TestBudgetLedger(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	first, later := filepath.Join(dir, "first.json"), filepath.Join(dir, "later.json")
	require.NoError(t, os.WriteFile(first, []byte(`[
		{"at_pct": 10, "usage": {"input_tokens": 3000, "output_tokens": 500, "turns": 8}},
		{"at_pct": 50, "sleep_ms": 120000},
		{"at_pct": 100, "report": {"summary": "never reached"}}]`), 0o600))
	require.NoError(t, os.WriteFile(later, []byte(`[
		{"at_pct": 50, "usage": {"input_tokens": 1000, "output_tokens": 200, "turns": 3}},
		{"at_pct": 100, "report": {"summary": "done after the watch change"}}]`), 0o600))
	b := newHolodeckBridge(t, first)
	const lead = time.Hour
	short, err := transporter.NewHolodeck(transporter.HolodeckConfig{
		Root: b.root, Combadge: combadgeBundle(t),
		Harness:  []string{"--harness", "holodeck", "--script", first},
		Lifetime: lead + 15*time.Second,
	})
	require.NoError(t, err)
	b.provider, b.lead = &ledgerProvider{watchProvider: &watchProvider{Holodeck: short}, later: later}, lead
	b.divisions = map[string]DivisionConfig{"science": {Budget: Budget{Tokens: 10000, Turns: 50}}}

	hd := b.start(t)
	c := hd.addCrewWith(t, store.Crew{ID: "sorell", Name: "Sorell", Rank: "Lieutenant", Division: "science"})
	id := hd.createOrder(t)

	// sends is each run's (budget_tokens, budget_turns, used_tokens,
	// used_turns) as its first step logged it.
	sends := func(t require.TestingT) [][4]float64 {
		var out [][4]float64
		for _, e := range readSteps(t, b.stepLog) {
			if e["order"] != id || e["index"] != float64(0) {
				continue
			}
			var v [4]float64
			for i, k := range []string{"budget_tokens", "budget_turns", "used_tokens", "used_turns"} {
				v[i], _ = e[k].(float64)
			}
			out = append(out, v)
		}
		return out
	}
	usage := func(t require.TestingT) []store.Usage {
		u, err := hd.d.store.OrderUsage(context.Background(), id)
		require.NoError(t, err)
		return u
	}

	require.EventuallyWithT(t, func(ct *assert.CollectT) {
		require.Equal(ct, [][4]float64{{10000, 50, 0, 0}}, sends(ct), "the division's budget, nothing used")
		u := usage(ct)
		require.Len(ct, u, 1)
		require.Equal(ct, store.Usage{Beam: c.CurrentBeam, InputTokens: 3000, OutputTokens: 500, Turns: 8, Seq: u[0].Seq}, u[0])
	}, 15*time.Second, 20*time.Millisecond)

	res := missionResult(t, hd.d, id)
	require.Equal(t, MissionDone, res.Status, "%+v", res)
	require.Equal(t, "done after the watch change", res.Report.Summary)
	require.Equal(t, [][4]float64{{10000, 50, 0, 0}, {10000, 50, 3500, 8}}, sends(t),
		"the second send carries the limit and the first beam's usage")

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
