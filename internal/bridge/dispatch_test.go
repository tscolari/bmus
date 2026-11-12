package bridge

import (
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tscolari/bmus/internal/store"
)

// shortScript writes a holodeck script whose missions report at once.
func shortScript(t *testing.T) string {
	t.Helper()
	script := filepath.Join(t.TempDir(), "short.json")
	require.NoError(t, os.WriteFile(script, []byte(`[{"at_pct": 50, "progress": "half"}, {"at_pct": 100, "report": {"summary": "done"}}]`), 0o600))
	return script
}

// createPoolOrder creates a pool order of division at priority.
func (hd *hdaemon) createPoolOrder(t *testing.T, division string, priority int) string {
	t.Helper()
	id, err := hd.d.CreateOrder(t.Context(), store.Order{Title: "pool run", Body: "walk the script", Division: division,
		Priority: priority, Repos: []store.OrderRepo{{Name: "app", BaseRef: "main"}}})
	require.NoError(t, err)
	return id
}

// missionDone waits for order id's mission to start and requires it to end
// done.
func missionDone(t *testing.T, d *Daemon, id string) {
	t.Helper()
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		require.Len(c, workflows(c, d, missionWorkflowID(id)), 1)
	}, 30*time.Second, 20*time.Millisecond)
	require.Equal(t, MissionDone, missionResult(t, d, id).Status)
}

// claims returns the ids of the orders claimed, in claim order.
func claims(entries []store.LogEntry) []string {
	var out []string
	for _, e := range entries {
		if e.Text == "order away: mission started" {
			out = append(out, e.OrderID)
		}
	}
	return out
}

// #96's acceptance: two crew members of ops, standing orders off, a
// routine then a red pool order. Nothing is claimed until standing orders
// go on, which wakes the division; the red order is claimed first.
func TestPoolClaimsRedFirst(t *testing.T) {
	t.Parallel()
	b := newHolodeckBridge(t, shortScript(t))
	hd := b.start(t)
	hd.addCrewWith(t, store.Crew{ID: "sorell", Name: "Sorell", Division: "ops"})
	hd.addCrewWith(t, store.Crew{ID: "tuvok", Name: "Tuvok", Division: "ops"})
	on, err := hd.d.StandingOrders(t.Context())
	require.NoError(t, err)
	require.False(t, on)

	routine := hd.createPoolOrder(t, "ops", store.PriorityRoutine)
	red := hd.createPoolOrder(t, "ops", store.PriorityRed)
	time.Sleep(2 * time.Second) // twice DBOS's notification poll: the wake-ups are read
	require.Equal(t, store.OrderPool, hd.order(t, routine).State, "standing orders are off")
	require.Equal(t, store.OrderPool, hd.order(t, red).State)
	s, err := hd.d.Status(t.Context())
	require.NoError(t, err)
	require.Equal(t, ConditionRed, s.Condition)
	require.Equal(t, []string{red + " red alert"}, s.Reasons)

	require.NoError(t, hd.d.SetStandingOrders(t.Context(), true))
	missionDone(t, hd.d, red)
	missionDone(t, hd.d, routine)
	require.Equal(t, []string{red, routine}, claims(hd.logEntries(t)))
	assignees := map[string]bool{hd.order(t, routine).Assignee: true, hd.order(t, red).Assignee: true}
	require.Equal(t, map[string]bool{"sorell": true, "tuvok": true}, assignees, "one each")
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		s, err := hd.d.Status(t.Context())
		require.NoError(c, err)
		require.Equal(c, ConditionGreen, s.Condition, "the red order is done")
	}, 10*time.Second, 50*time.Millisecond)
	requireNoBackstop(t, hd.logEntries(t))
}

// #96's acceptance: a pool order whose wake-up is lost is still claimed by
// an idle crew member of its division within one re-check
// (missionCheckEvery).
func TestPoolOrderClaimedWithoutWake(t *testing.T) {
	t.Parallel()
	b := newHolodeckBridge(t, shortScript(t))
	b.checkEvery = 2 * time.Second
	var wakes atomic.Int32
	b.wakeSend = func(string, CrewSignal) error {
		wakes.Add(1)
		return errors.New("lost")
	}
	hd := b.start(t)
	require.NoError(t, hd.d.SetStandingOrders(t.Context(), true))
	hd.addCrewWith(t, store.Crew{ID: "sorell", Name: "Sorell", Division: "ops"})
	hd.addCrewWith(t, store.Crew{ID: "kirk", Name: "Kirk", Division: "command"})

	before := wakes.Load()
	start := time.Now()
	id := hd.createPoolOrder(t, "ops", store.PriorityRoutine)
	require.Equal(t, before+1, wakes.Load(), "one wake-up, for sorell, and it was lost")
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		o := hd.order(c, id)
		require.Equal(c, store.OrderAway, o.State)
		require.Equal(c, "sorell", o.Assignee)
	}, 2*b.checkEvery+3*time.Second, 50*time.Millisecond)
	t.Logf("claimed %s after CreateOrder", time.Since(start).Round(time.Millisecond))
	missionDone(t, hd.d, id)
}
