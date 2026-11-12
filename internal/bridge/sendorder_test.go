package bridge

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tscolari/bmus/internal/store"
)

// #195: a tour that ends between the mission's ship-repos and send-order
// steps parks the order, not fails it after send-order's retries, and the
// order resumes on re-energize. kill kills serve once ship-repos is
// recorded, so send-order finds no connection and retries until the tour
// ends; check_failed has serve fail its harness check then (#172), and
// send-order starts only once the tour has ended.
func TestTourEndsBeforeSendOrder(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		parked string // the order's park entry
	}{
		{"kill", "order parked: beam lost"},
		{"check_failed", "order parked: harness check failed: " + checkFailReason},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			b := newHolodeckBridge(t, quickScript(t))
			f := &checkFail{}
			b.tunePump = f.wrap
			tourID := tourWorkflowID("sorell", 0)
			var (
				once    sync.Once
				hookErr = make(chan error, 1)
				marked  = make(chan time.Time, 1) // when the tour was marked ended
			)
			b.sendOrderHook = func(d *Daemon, _ string) {
				once.Do(func() {
					go func() {
						<-d.tourEnded(tourID)
						marked <- time.Now()
					}()
					hookErr <- endTourNow(d, b, f, tc.name, tourID)
				})
			}
			hd := b.start(t)
			hd.addCrew(t)
			id := hd.createOrder(t)
			mission := missionWorkflowID(id)

			require.Equal(t, CrewTourResult{Ended: store.CrewSickbay}, tourResult(t, hd.d, tourID))
			require.NoError(t, <-hookErr)
			var o store.Order
			require.EventuallyWithT(t, func(ct *assert.CollectT) {
				o = hd.order(ct, id)
				require.NotNil(ct, o.ParkedAt)
			}, 30*time.Second, 20*time.Millisecond)
			// Well under the retry wait's cap: the wait woke on the mark
			// rather than running out.
			require.Less(t, o.ParkedAt.Sub(<-marked), sendOrderMaxWait/3, "parked soon after the tour ended")
			require.Equal(t, store.OrderAway, o.State)
			requirePending(t, hd.d, mission)
			out, ok := stepOutput(t, hd.d, mission, "send-order")
			require.True(t, ok)
			require.Equal(t, "0", out, "no order went out")
			names := stepNames(t, hd.d, mission)
			require.NotContains(t, names, "order-failed")
			entries := hd.logEntries(t)
			require.Equal(t, []string{tc.parked}, orderTexts(entries, id, "order parked"))
			requireNoBackstop(t, entries)

			f.armed.Store(false)
			requireOutcome(t, OutcomeRequested)(hd.d.ReenergizeCrew(t.Context(), "sorell"))
			require.Equal(t, MissionDone, missionResult(t, hd.d, id).Status)
			require.Equal(t, store.OrderDone, hd.order(t, id).State)
			require.Equal(t, 2, countSteps(t, hd.d, mission, "send-order"))
		})
	}
}

// errTestTimeout is endTourNow giving up.
var errTestTimeout = errors.New("timed out")

// endTourNow ends tourID's tour from inside send-order's hook, before its
// first attempt: kill kills serve and waits until the pump has lost its
// connection, so the attempt fails as the tour ends; check_failed has the
// pump's next attach bring harness_check_failed and waits for the tour to
// be marked ended.
func endTourNow(d *Daemon, b *holodeckBridge, f *checkFail, how, tourID string) error {
	c, err := d.store.GetCrew(context.Background(), "sorell")
	if err != nil {
		return err
	}
	deadline := time.After(30 * time.Second)
	if how == "kill" {
		if err := b.h.Kill(c.CurrentBeam); err != nil {
			return err
		}
		for {
			if p, ok := d.Pump("sorell"); !ok || !p.Connected() {
				return nil
			}
			select {
			case <-deadline:
				return errTestTimeout
			case <-time.After(10 * time.Millisecond):
			}
		}
	}
	f.armed.Store(true)
	_ = f.last().Close()
	select {
	case <-d.tourEnded(tourID):
		return nil
	case <-deadline:
		return errTestTimeout
	}
}

// #195 review: with the tour's end mark lost (a fresh daemon, as after a
// bridge restart), send-order and ship-repos go by the crew row. A crew
// member in sickbay, dismissed or on a later tour (re-energized) ends the
// tour behind the mission: send-order records 0 and ship-repos TourEnded,
// touching no pump or beam. On the mission's own tour, on deck, send-order
// tries to send (no pump here) and retries.
func TestSendOrderTourOverWithoutMark(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		crew  func(context.Context, *store.Store) error
		ended bool
	}{
		{"sickbay", func(ctx context.Context, st *store.Store) error {
			return st.UpdateCrewStatus(ctx, "sorell", store.CrewSickbay, testEntry)
		}, true},
		{"dismissed", func(ctx context.Context, st *store.Store) error {
			return st.UpdateCrewStatus(ctx, "sorell", store.CrewDismissed, testEntry)
		}, true},
		{"later tour", func(ctx context.Context, st *store.Store) error {
			_, err := st.NextTour(ctx, "sorell", 0, testEntry)
			return err
		}, true},
		{"own tour", func(context.Context, *store.Store) error { return nil }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			st, err := store.Open(ctx, filepath.Join(privateHome(t), store.DBFile))
			require.NoError(t, err)
			t.Cleanup(func() { st.Close() })
			require.NoError(t, st.InsertCrew(ctx, store.Crew{ID: "sorell", Name: "Sorell", Status: store.CrewOnDeck}, testEntry))
			id, err := st.InsertOrder(ctx, store.Order{Title: "t", Assignee: "sorell", State: store.OrderAway,
				Repos: []store.OrderRepo{{Name: "app", BaseRef: "main"}}}, testEntry)
			require.NoError(t, err)
			require.NoError(t, tc.crew(ctx, st))
			d := &Daemon{store: st, logger: slog.New(slog.DiscardHandler),
				pumps: map[string]pumpEntry{}, tourEnds: map[string]chan struct{}{}}
			in := MissionInput{OrderID: id, CrewID: "sorell", TourID: tourWorkflowID("sorell", 0)}

			if !tc.ended {
				sctx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
				defer cancel()
				_, err := sendOrderStep(sctx, d, in, shipped{})
				require.ErrorIs(t, err, context.DeadlineExceeded, "still retrying")
				return
			}
			sent, err := sendOrderStep(ctx, d, in, shipped{})
			require.NoError(t, err)
			require.Equal(t, orderNotSent, sent)
			ship, err := shipRepos(ctx, d, in)
			require.NoError(t, err)
			require.Equal(t, shipped{TourEnded: true}, ship)
		})
	}
}
