package bridge

import (
	"context"
	"fmt"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tscolari/bmus/internal/store"
	"github.com/tscolari/bmus/internal/transporter"
)

// loginGateProvider is a watchProvider behind a fake tsh login gate (#102):
// with expired set, LoginValidUntil reports no login, and every call that
// would run tsh fails with transporter.ErrLoginExpired, as Tsh's gate does.
type loginGateProvider struct {
	*watchProvider
	expired atomic.Bool
}

func (p *loginGateProvider) gate(call string) error {
	if p.expired.Load() {
		return fmt.Errorf("tsh beams %s: %w", call, transporter.ErrLoginExpired)
	}
	return nil
}

func (p *loginGateProvider) LoginValidUntil(context.Context) (time.Time, error) {
	if err := p.gate("status"); err != nil {
		return time.Time{}, err
	}
	return time.Time{}, nil
}

func (p *loginGateProvider) Create(ctx context.Context) (transporter.Beam, error) {
	if err := p.gate("add"); err != nil {
		return transporter.Beam{}, err
	}
	return p.watchProvider.Create(ctx)
}

func (p *loginGateProvider) Exists(ctx context.Context, beam transporter.Beam) (bool, error) {
	if err := p.gate("ls"); err != nil {
		return false, err
	}
	return p.watchProvider.Exists(ctx, beam)
}

func (p *loginGateProvider) Probe(ctx context.Context, beam transporter.Beam) (bool, error) {
	if err := p.gate("ls"); err != nil {
		return false, err
	}
	return p.watchProvider.Probe(ctx, beam)
}

func (p *loginGateProvider) Remove(ctx context.Context, beam transporter.Beam) error {
	if err := p.gate("rm"); err != nil {
		return err
	}
	return p.watchProvider.Remove(ctx, beam)
}

func (p *loginGateProvider) StartCombadge(ctx context.Context, beam transporter.Beam, spec transporter.CombadgeSpec) error {
	if err := p.gate("exec"); err != nil {
		return err
	}
	return p.watchProvider.StartCombadge(ctx, beam, spec)
}

func (p *loginGateProvider) AttachCombadge(ctx context.Context, beam transporter.Beam, since uint64) (transporter.Session, error) {
	if err := p.gate("exec"); err != nil {
		return nil, err
	}
	return p.watchProvider.AttachCombadge(ctx, beam, since)
}

// #106's acceptance: the bridge stops with the tsh login expired and stays
// down past its crew member's beam lifetime. On restart the tour changes
// the watch, and the new tour's provisioning waits for a login: the crew
// member is beaming_in with no beam, and the status says one crew member
// waits for a beam. A new login brings it on deck with a new beam.
func TestBeamingInWaitsForLogin(t *testing.T) {
	t.Parallel()
	script := quickScript(t)
	b := newHolodeckBridge(t, script)
	const lead = 2 * time.Second
	short, err := transporter.NewHolodeck(transporter.HolodeckConfig{
		Root: b.root, Combadge: combadgeBundle(t),
		Harness: []string{"--harness", "holodeck", "--script", script},
		// The first beam's watch is due 15s after it is made, and it
		// expires 2s later: the first bridge stops before either.
		Lifetime: lead + 15*time.Second,
	})
	require.NoError(t, err)
	p := &loginGateProvider{watchProvider: &watchProvider{Holodeck: short}}
	b.provider, b.lead = p, lead
	b.loginCheckEvery, b.loginWait = 20*time.Millisecond, 50*time.Millisecond

	hd := b.start(t)
	c := hd.addCrew(t)
	beams, err := hd.d.store.ListBeams(t.Context(), "sorell")
	require.NoError(t, err)
	require.Len(t, beams, 1)
	expires := beams[0].ExpiresAt
	made := expires.Add(-lead - 15*time.Second)

	p.expired.Store(true)
	requireReasons(t, hd.d, ConditionYellow, "tsh login expired")
	hd.stop(t)
	require.True(t, time.Now().Before(expires.Add(-lead)),
		"the first bridge must stop before the watch change: it stopped %s after the beam was made, the watch is due at %s",
		time.Since(made).Round(time.Millisecond), (expires.Sub(made) - lead).Round(time.Millisecond))
	// Down past the beam's lifetime.
	require.Eventually(t, func() bool { return time.Now().After(expires) }, 30*time.Second, 50*time.Millisecond)

	hd = b.start(t)
	require.EventuallyWithT(t, func(ct *assert.CollectT) {
		cr := hd.crew(ct, "sorell")
		require.Equal(ct, store.CrewBeamingIn, cr.Status)
		require.Equal(ct, 1, cr.TourNo)
		require.Empty(ct, cr.CurrentBeam)
		require.Contains(ct, logTexts(t, hd.d, "sorell"), "provisioning waits for a tsh login")
	}, 30*time.Second, 50*time.Millisecond)
	requireReasons(t, hd.d, ConditionYellow, "1 crew waiting for a beam (tsh login)", "tsh login expired")
	// As bmus status and bmus crew list read them.
	var s BridgeStatus
	require.NoError(t, call(t, b.home, "bridge.status", nil, &s))
	require.Equal(t, []string{"1 crew waiting for a beam (tsh login)", "tsh login expired"}, s.Reasons)
	require.Equal(t, CrewCounts{BeamingIn: 1}, s.Crew)
	require.Zero(t, s.BeamsLive)
	var list CrewListResult
	require.NoError(t, call(t, b.home, "crew.list", nil, &list))
	require.Len(t, list.Crew, 1)
	require.Equal(t, store.CrewBeamingIn, list.Crew[0].Status)
	require.Empty(t, list.Crew[0].CurrentBeam)

	p.expired.Store(false)
	var cr store.Crew
	require.EventuallyWithT(t, func(ct *assert.CollectT) {
		cr = hd.crew(ct, "sorell")
		require.Equal(ct, store.CrewOnDeck, cr.Status)
		require.NotEmpty(ct, cr.CurrentBeam)
	}, 30*time.Second, 50*time.Millisecond)
	require.Equal(t, 1, cr.TourNo)
	require.NotEqual(t, c.CurrentBeam, cr.CurrentBeam, "a new beam")
	requireReasons(t, hd.d, ConditionGreen)

	names, _ := tourSteps(t, hd.d, tourWorkflowID("sorell", 1))
	require.EventuallyWithT(t, func(ct *assert.CollectT) {
		names, _ = tourSteps(ct, hd.d, tourWorkflowID("sorell", 1))
		require.Contains(ct, names, "claim-order")
	}, 10*time.Second, 50*time.Millisecond)
	helloBeforeClaim(t, names)
	texts := logTexts(t, hd.d, "sorell")
	handover := slices.Index(texts, "watch change: tour 0 hands over to tour 1")
	require.GreaterOrEqual(t, handover, 0, "%q", texts)
	require.Equal(t, 1, countPrefix(texts, "provisioning waits for a tsh login"), "%q", texts)
	onDeck := slices.Index(texts[handover:], "crew on deck: combadge said hello")
	require.Positive(t, onDeck, "the new beam's hello puts the crew member on deck: %q", texts)
	require.Zero(t, countPrefix(texts, "crew to sickbay"), "%q", texts)
}
