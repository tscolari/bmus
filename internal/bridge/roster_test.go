package bridge

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/tscolari/bmus/internal/store"
)

func writeRoster(t *testing.T, doc string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), RosterFile)
	require.NoError(t, os.WriteFile(path, []byte(doc), 0o600))
	return path
}

func TestLoadRoster(t *testing.T) {
	t.Parallel()
	r, err := LoadRoster(writeRoster(t, `divisions:
  ops: {harness: echo, model: m1}
  science: {}
standing_orders: true
transporter:
  max_beams: 10
`))
	require.NoError(t, err)
	require.Equal(t, RosterConfig{
		Divisions:      map[string]DivisionConfig{"ops": {Harness: "echo", Model: "m1"}, "science": {}},
		StandingOrders: true,
		Transporter:    TransporterLimits{MaxBeams: 10, MaxProvisioning: 3, RatePerMinute: 5},
	}, r)

	// A missing or empty file is a zero config, with the defaults.
	defaults := RosterConfig{Transporter: TransporterLimits{MaxBeams: 18, MaxProvisioning: 3, RatePerMinute: 5}}
	r, err = LoadRoster(filepath.Join(t.TempDir(), "missing.yaml"))
	require.NoError(t, err)
	require.Equal(t, defaults, r)
	r, err = LoadRoster(writeRoster(t, ""))
	require.NoError(t, err)
	require.Equal(t, defaults, r)

	for name, tc := range map[string]struct{ doc, err string }{
		"unknown key":       {"standing_orders: true\nstanding: false\n", "field standing not found"},
		"unknown nested":    {"divisions:\n  ops: {harnes: echo}\n", "field harnes not found"},
		"unknown limit":     {"transporter:\n  max_beam: 3\n", "field max_beam not found"},
		"bad division name": {"divisions:\n  ops/x: {}\n", `division name "ops/x"`},
		"unknown harness":   {"divisions:\n  ops: {harness: codex}\n", `division ops: unknown harness "codex" (want one of sdk, holodeck, echo)`},
		"negative limit":    {"transporter:\n  rate_per_minute: -1\n", "transporter.rate_per_minute must not be negative"},
		"bad type":          {"standing_orders: maybe\n", "cannot unmarshal"},
		"two documents":     {"standing_orders: true\n---\nstanding_orders: false\n", "one YAML document"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := LoadRoster(writeRoster(t, tc.doc))
			require.ErrorContains(t, err, tc.err)
		})
	}
}

// AddCrew and CreateOrder refuse a division missing from the roster. A
// roster without divisions checks nothing.
func TestDivisionChecks(t *testing.T) {
	t.Parallel()
	home := privateHome(t)
	cfg := testConfig(home)
	cfg.Transporter = &fakeProvider{blockProvision: true}
	cfg.Repos = map[string]string{"app": testRepo(t).url}
	cfg.Divisions = map[string]DivisionConfig{"ops": {}, "science": {}}
	r := startDaemonConfig(t, cfg)
	ctx := t.Context()

	err := r.d.AddCrew(ctx, store.Crew{ID: "kirk", Name: "Kirk", Division: "command"})
	require.ErrorContains(t, err, `unknown division "command" (the roster has ops, science)`)
	require.Equal(t, CodeBadRequest, errorCode(ctx, err))
	require.NoError(t, r.d.AddCrew(ctx, store.Crew{ID: "sorell", Name: "Sorell", Division: "science"}))
	require.NoError(t, r.d.AddCrew(ctx, store.Crew{ID: "tuvok", Name: "Tuvok"}))

	repos := []store.OrderRepo{{Name: "app", BaseRef: "main"}}
	_, err = r.d.CreateOrder(ctx, store.Order{Title: "x", Assignee: "sorell", Division: "command", Repos: repos})
	require.ErrorContains(t, err, `unknown division "command"`)
	require.Equal(t, CodeBadRequest, errorCode(ctx, err))
	_, err = r.d.CreateOrder(ctx, store.Order{Title: "x", Assignee: "sorell", Division: "ops", Repos: repos})
	require.NoError(t, err)
	r.stop(t)

	// Without divisions in the roster, any division passes.
	cfg.Divisions = nil
	cfg.Transporter = &fakeProvider{blockProvision: true}
	r = startDaemonConfig(t, cfg)
	defer r.stop(t)
	require.NoError(t, r.d.AddCrew(ctx, store.Crew{ID: "kirk", Name: "Kirk", Division: "command"}))
	_, err = r.d.CreateOrder(ctx, store.Order{Title: "y", Assignee: "kirk", Division: "command", Repos: repos})
	require.NoError(t, err)
}

// The roster's standing_orders seeds the setting once; after that the live
// switch wins, across restarts and over a changed roster.
func TestStandingOrders(t *testing.T) {
	t.Parallel()
	home := privateHome(t)
	cfg := testConfig(home)
	cfg.StandingOrders = true
	r := startDaemonConfig(t, cfg)
	ctx := t.Context()

	var res StandingOrdersResult
	require.NoError(t, call(t, home, "standing_orders.get", nil, &res))
	require.True(t, res.On, "seeded from the roster")
	on := false
	require.NoError(t, call(t, home, "standing_orders.set", StandingOrdersSetArgs{On: &on}, &res))
	require.False(t, res.On)
	require.NoError(t, r.d.SetStandingOrders(ctx, false), "setting the same value is a no-op")
	requireRPCError(t, call(t, home, "standing_orders.set", map[string]any{}, nil), CodeBadRequest)
	r.stop(t)

	r = startDaemonConfig(t, cfg)
	require.NoError(t, call(t, home, "standing_orders.get", nil, &res))
	require.False(t, res.On, "the live setting wins over the roster after a restart")
	require.NoError(t, r.d.SetStandingOrders(ctx, true))
	r.stop(t)

	cfg.StandingOrders = false
	r = startDaemonConfig(t, cfg)
	defer r.stop(t)
	on, err := r.d.StandingOrders(ctx)
	require.NoError(t, err)
	require.True(t, on, "a changed roster doesn't reset the setting")

	entries, err := r.d.store.ListLog(ctx, 0)
	require.NoError(t, err)
	var texts []string
	for _, e := range entries {
		require.Equal(t, logBridge, e.Kind)
		texts = append(texts, e.Text)
	}
	require.Equal(t, []string{"standing orders on (roster default)", "standing orders off", "standing orders on"}, texts)
}

// A crew member's harness and model override its division's defaults, and
// an order's model overrides both (#59).
func TestResolveHarnessAndModel(t *testing.T) {
	t.Parallel()
	d := &Daemon{divisions: map[string]DivisionConfig{"ops": {Harness: "echo", Model: "m1"}, "science": {}}}
	for _, tc := range []struct {
		crew                   store.Crew
		harness, harnessSource string
	}{
		{store.Crew{Division: "ops", Harness: "sdk"}, "sdk", SourceCrew},
		{store.Crew{Division: "ops"}, "echo", SourceDivision},
		{store.Crew{Division: "science"}, "", SourceDefault},
		{store.Crew{}, "", SourceDefault},
		{store.Crew{Division: "gone"}, "", SourceDefault},
	} {
		h, src := d.resolveHarness(tc.crew)
		require.Equal(t, [2]string{tc.harness, tc.harnessSource}, [2]string{h, src}, "%+v", tc.crew)
	}
	for _, tc := range []struct {
		order, crew, division string
		model, source         string
	}{
		{"x", "c", "ops", "x", SourceOrder},
		{"", "c", "ops", "c", SourceCrew},
		{"", "", "ops", "m1", SourceDivision},
		{"x", "", "ops", "x", SourceOrder},
		{"", "", "science", "", SourceDefault},
		{"", "c", "", "c", SourceCrew},
		{"", "", "", "", SourceDefault},
	} {
		m, src := d.resolveModel(store.Order{Model: tc.order}, store.Crew{Model: tc.crew, Division: tc.division})
		require.Equal(t, [2]string{tc.model, tc.source}, [2]string{m, src}, "%+v", tc)
	}
}

// #147: Command's guidance is a setting the captain sets, clears and reads
// back; one over MaxCommandGuidance is refused.
func TestCommandGuidance(t *testing.T) {
	t.Parallel()
	home := privateHome(t)
	r := startDaemon(t, home)
	defer r.stop(t)

	var res CommandGuidanceResult
	require.NoError(t, call(t, home, "command.guidance.get", nil, &res))
	require.Empty(t, res.Guidance, "none set")
	empty := ""
	require.NoError(t, call(t, home, "command.guidance.set", CommandGuidanceSetArgs{Guidance: &empty}, &res), "clearing none is a no-op")
	g := "  prefer small PRs\n"
	require.NoError(t, call(t, home, "command.guidance.set", CommandGuidanceSetArgs{Guidance: &g}, &res))
	require.Equal(t, "prefer small PRs", res.Guidance, "trimmed")
	require.NoError(t, call(t, home, "command.guidance.get", nil, &res))
	require.Equal(t, "prefer small PRs", res.Guidance)
	g = "prefer small PRs"
	require.NoError(t, call(t, home, "command.guidance.set", CommandGuidanceSetArgs{Guidance: &g}, &res), "the same value is a no-op")

	requireRPCError(t, call(t, home, "command.guidance.set", map[string]any{}, nil), CodeBadRequest)
	long := strings.Repeat("x", MaxCommandGuidance+1)
	requireRPCError(t, call(t, home, "command.guidance.set", CommandGuidanceSetArgs{Guidance: &long}, nil), CodeBadRequest)
	long = long[1:]
	require.NoError(t, call(t, home, "command.guidance.set", CommandGuidanceSetArgs{Guidance: &long}, &res), "at the limit")
	require.NoError(t, call(t, home, "command.guidance.set", CommandGuidanceSetArgs{Guidance: &empty}, &res))
	require.NoError(t, call(t, home, "command.guidance.get", nil, &res))
	require.Empty(t, res.Guidance, "cleared")

	entries, err := r.d.store.ListLog(t.Context(), 0)
	require.NoError(t, err)
	var texts []string
	for _, e := range entries {
		if e.Kind == logBridge && strings.HasPrefix(e.Text, "command guidance") {
			texts = append(texts, e.Text)
			if e.Text == "command guidance cleared" {
				require.JSONEq(t, `{}`, string(e.Payload), "no payload")
			}
		}
	}
	require.Equal(t, []string{"command guidance set", "command guidance set", "command guidance cleared"}, texts)
}
