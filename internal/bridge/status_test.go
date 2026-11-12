package bridge

import (
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/tscolari/bmus/internal/store"
)

// Each condition and the reasons that set it (#70).
func TestStatusConditions(t *testing.T) {
	t.Parallel()
	crew := func(id, status, beam string) store.Crew {
		return store.Crew{ID: id, Status: status, CurrentBeam: beam}
	}
	for _, tc := range []struct {
		name      string
		crew      []store.Crew
		red       []string // open red orders
		login     string   // the login's reason
		waits     []string // crew members whose provisioning waits for a login
		command   bool     // Command down
		condition string
		reasons   []string
		counts    CrewCounts
		beams     int
	}{
		{name: "no crew", condition: ConditionGreen, reasons: []string{}},
		{name: "on deck", crew: []store.Crew{crew("sorell", store.CrewOnDeck, "b1")},
			condition: ConditionGreen, reasons: []string{}, counts: CrewCounts{OnDeck: 1}, beams: 1},
		{name: "away", crew: []store.Crew{crew("sorell", store.CrewAway, "b1"), crew("kirk", store.CrewOnDeck, "")},
			condition: ConditionGreen, reasons: []string{}, counts: CrewCounts{OnDeck: 1, Away: 1}, beams: 1},
		{name: "hailing", crew: []store.Crew{crew("kirk", store.CrewAway, "b1"), crew("sorell", store.CrewHailing, "b2")},
			condition: ConditionYellow, reasons: []string{"sorell hailing"}, counts: CrewCounts{Away: 1, Hailing: 1}, beams: 2},
		{name: "sickbay", crew: []store.Crew{crew("sorell", store.CrewSickbay, "b1")},
			condition: ConditionRed, reasons: []string{"sorell in sickbay"}, counts: CrewCounts{Sickbay: 1}},
		{name: "sickbay outranks hailing",
			crew: []store.Crew{crew("kirk", store.CrewHailing, "b1"), crew("spock", store.CrewSickbay, ""),
				crew("sorell", store.CrewSickbay, "b3")},
			condition: ConditionRed, reasons: []string{"spock in sickbay", "sorell in sickbay"},
			counts: CrewCounts{Hailing: 1, Sickbay: 2}, beams: 1},
		{name: "dismissed", crew: []store.Crew{crew("sorell", store.CrewDismissed, "b1")},
			condition: ConditionGreen, reasons: []string{}},
		{name: "red alert", red: []string{"ORD-0002", "ORD-0005"},
			condition: ConditionRed, reasons: []string{"ORD-0002 red alert", "ORD-0005 red alert"}},
		{name: "red alert after sickbay, outranking hailing",
			crew: []store.Crew{crew("kirk", store.CrewHailing, "b1"), crew("sorell", store.CrewSickbay, "")},
			red:  []string{"ORD-0001"}, condition: ConditionRed, reasons: []string{"sorell in sickbay", "ORD-0001 red alert"},
			counts: CrewCounts{Hailing: 1, Sickbay: 1}, beams: 1},
		{name: "login expiring", login: "tsh login expires in 10m",
			condition: ConditionYellow, reasons: []string{"tsh login expires in 10m"}},
		{name: "login after hailing", crew: []store.Crew{crew("sorell", store.CrewHailing, "b1")}, login: "tsh login expired",
			condition: ConditionYellow, reasons: []string{"sorell hailing", "tsh login expired"}, counts: CrewCounts{Hailing: 1}, beams: 1},
		{name: "login listed when red", crew: []store.Crew{crew("sorell", store.CrewSickbay, "b1")}, login: "tsh login expired",
			condition: ConditionRed, reasons: []string{"sorell in sickbay", "tsh login expired"}, counts: CrewCounts{Sickbay: 1}},
		{name: "Command down", command: true, condition: ConditionYellow, reasons: []string{"Command not on deck"}},
		{name: "Command down after hailing, before the login", crew: []store.Crew{crew("sorell", store.CrewHailing, "b1")},
			command: true, login: "tsh login expired", condition: ConditionYellow,
			reasons: []string{"sorell hailing", "Command not on deck", "tsh login expired"}, counts: CrewCounts{Hailing: 1}, beams: 1},
		{name: "beaming in", crew: []store.Crew{crew("sorell", store.CrewBeamingIn, ""), crew("kirk", store.CrewBeamingIn, "b2")},
			condition: ConditionGreen, reasons: []string{}, counts: CrewCounts{BeamingIn: 2}, beams: 1},
		{name: "beaming in, waiting for a login",
			crew: []store.Crew{crew("kirk", store.CrewBeamingIn, ""), crew("sorell", store.CrewBeamingIn, ""),
				crew("spock", store.CrewOnDeck, "b3")},
			waits: []string{"kirk", "sorell", "spock"}, login: "tsh login expired", condition: ConditionYellow,
			reasons: []string{"2 crew waiting for a beam (tsh login)", "tsh login expired"},
			counts:  CrewCounts{BeamingIn: 2, OnDeck: 1}, beams: 1},
		{name: "beam wait listed when red, after Command",
			crew:  []store.Crew{crew("sorell", store.CrewBeamingIn, ""), crew("vance", store.CrewSickbay, "b1")},
			waits: []string{"sorell"}, command: true, login: "tsh login expired", condition: ConditionRed,
			reasons: []string{"vance in sickbay", "Command not on deck", "1 crew waiting for a beam (tsh login)", "tsh login expired"},
			counts:  CrewCounts{BeamingIn: 1, Sickbay: 1}},
		{name: "beam wait alone is yellow", crew: []store.Crew{crew("sorell", store.CrewBeamingIn, "")}, waits: []string{"sorell"},
			condition: ConditionYellow, reasons: []string{"1 crew waiting for a beam (tsh login)"}, counts: CrewCounts{BeamingIn: 1}},
		{name: "Command down listed when red", crew: []store.Crew{crew("vance", store.CrewSickbay, "b1")}, command: true,
			condition: ConditionRed, reasons: []string{"vance in sickbay", "Command not on deck"}, counts: CrewCounts{Sickbay: 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			waits := map[string]bool{}
			for _, id := range tc.waits {
				waits[id] = true
			}
			s := statusOf(store.StatusSnapshot{Crew: tc.crew, RedOrders: tc.red, CommandDown: tc.command}, tc.login, waits)
			require.Equal(t, tc.condition, s.Condition)
			require.Equal(t, tc.reasons, s.Reasons)
			require.Equal(t, tc.counts, s.Crew)
			require.Equal(t, tc.beams, s.BeamsLive)
		})
	}
}

// Status reads the counters from the store: open orders are pool or
// directed, away ones away or hailing; open red orders (a hailing one
// included, a done one not) are red alerts.
func TestStatusReadsStore(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), store.DBFile))
	require.NoError(t, err)
	defer st.Close()
	d := &Daemon{store: st}

	s, err := d.Status(ctx)
	require.NoError(t, err)
	// Without regs.yaml (#149): information, not a reason.
	require.Equal(t, BridgeStatus{Condition: ConditionGreen, Reasons: []string{}, Info: []string{"regs.yaml not configured"}}, s)

	e := store.LogEntry{Kind: "test"}
	require.NoError(t, st.InsertCrew(ctx, store.Crew{ID: "sorell", Name: "Sorell", CurrentBeam: "b1"}, e))
	require.NoError(t, st.InsertCrew(ctx, store.Crew{ID: "kirk", Name: "Kirk", Status: store.CrewSickbay, CurrentBeam: "b2"}, e))
	for _, o := range []store.Order{
		{Title: "claimed", Assignee: "sorell", Priority: store.PriorityRed},
		{Title: "directed", Assignee: "sorell"},
		{Title: "pool", Priority: store.PriorityYellow},
		{Title: "done", State: store.OrderDone, Priority: store.PriorityRed},
		{Title: "failed", State: store.OrderFailed},
		{Title: "red pool", Priority: store.PriorityRed},
	} {
		_, err := st.InsertOrder(ctx, o, e)
		require.NoError(t, err)
	}
	claimed, found, err := st.ClaimOldestDirectedOrder(ctx, "sorell", e)
	require.NoError(t, err)
	require.True(t, found)
	_, err = st.InsertHail(ctx, store.Hail{OrderID: claimed, CrewID: "sorell", Question: "Widen?"}, e)
	require.NoError(t, err)
	last, err := st.AppendLog(ctx, e)
	require.NoError(t, err)

	s, err = d.Status(ctx)
	require.NoError(t, err)
	require.Equal(t, BridgeStatus{
		Condition: ConditionRed,
		Reasons:   []string{"kirk in sickbay", "ORD-0001 red alert", "ORD-0006 red alert"},
		Info:      []string{"regs.yaml not configured"},
		Crew:      CrewCounts{Hailing: 1, Sickbay: 1},
		Orders:    OrderCounts{Open: 3, Away: 1},
		HailsOpen: 1,
		BeamsLive: 1,
		LastLogID: last,
	}, s)

	// Standing orders (#78): the roster's default until set, read without
	// seeding the setting; then the setting.
	d.standingOrdersDefault = true
	s, err = d.Status(ctx)
	require.NoError(t, err)
	require.True(t, s.StandingOrders)
	require.Equal(t, last, s.LastLogID, "nothing written")
	_, err = st.GetSetting(ctx, settingStandingOrders)
	require.ErrorIs(t, err, sql.ErrNoRows)
	require.NoError(t, d.SetStandingOrders(ctx, false))
	s, err = d.Status(ctx)
	require.NoError(t, err)
	require.False(t, s.StandingOrders)

	d.regsConfigured = true
	s, err = d.Status(ctx)
	require.NoError(t, err)
	require.Equal(t, []string{}, s.Info)
}

// bridge.status's JSON, which the console's status event carries and
// `bmus status --json` prints: these keys, no more.
func TestStatusJSON(t *testing.T) {
	t.Parallel()
	b, err := json.Marshal(BridgeStatus{Condition: ConditionGreen, Reasons: []string{}, Info: []string{}, Console: "127.0.0.1:4096",
		StandingOrders: true})
	require.NoError(t, err)
	require.JSONEq(t, `{"condition":"green","reasons":[],"info":[],"crew":{"beaming_in":0,"on_deck":0,"away":0,"hailing":0,"sickbay":0},
		"orders":{"open":0,"away":0},"hails_open":0,"beams_live":0,"last_log_id":0,"console":"127.0.0.1:4096",
		"standing_orders":true}`, string(b))
}

// The login's reason: none until 30 minutes before it expires (or with no
// expiry, or before it is read), then the minutes left, rounded up, then
// expired from transporter.LoginExpiryMargin before it. A missing login is
// expired. Two failed reads in a row make it unknown, unless the last
// known state is expired.
func TestLoginReason(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		login loginState
		want  string
	}{
		{login: loginState{}},
		{login: loginState{known: true}},
		{login: loginState{known: true, until: now.Add(31 * time.Minute)}},
		{login: loginState{known: true, until: now.Add(30 * time.Minute)}, want: "tsh login expires in 30m"},
		{login: loginState{known: true, until: now.Add(10 * time.Minute)}, want: "tsh login expires in 10m"},
		{login: loginState{known: true, until: now.Add(9*time.Minute + time.Second)}, want: "tsh login expires in 10m"},
		{login: loginState{known: true, until: now.Add(3 * time.Minute)}, want: "tsh login expires in 3m"},
		{login: loginState{known: true, until: now.Add(2*time.Minute + time.Second)}, want: "tsh login expires in 3m"},
		{login: loginState{known: true, until: now.Add(2 * time.Minute)}, want: "tsh login expired"},
		{login: loginState{known: true, until: now.Add(time.Second)}, want: "tsh login expired"},
		{login: loginState{failures: 1, lastErr: "boom"}},
		{login: loginState{failures: 2, lastErr: "boom"}, want: "tsh login unknown: boom"},
		{login: loginState{known: true, until: now.Add(time.Hour), failures: 2, lastErr: "boom"}, want: "tsh login unknown: boom"},
		{login: loginState{known: true, absent: true, failures: 2, lastErr: "boom"}, want: "tsh login expired"},
		{login: loginState{known: true, until: now}, want: "tsh login expired"},
		{login: loginState{known: true, until: now.Add(-time.Hour)}, want: "tsh login expired"},
		{login: loginState{known: true, absent: true}, want: "tsh login expired"},
	} {
		require.Equal(t, tc.want, tc.login.reason(now), "%+v", tc.login)
	}
}

// Command is down with someone in the command division and nobody of it
// on deck; nobody in it, or only dismissed crew members, isn't down.
func TestCommandDown(t *testing.T) {
	t.Parallel()
	crew := func(id, division, status string) store.Crew {
		return store.Crew{ID: id, Division: division, Status: status}
	}
	for _, tc := range []struct {
		name string
		crew []store.Crew
		down bool
	}{
		{"nobody", nil, false},
		{"no Command", []store.Crew{crew("sorell", "ops", store.CrewSickbay)}, false},
		{"on deck", []store.Crew{crew("vance", DivisionCommand, store.CrewOnDeck)}, false},
		{"away", []store.Crew{crew("vance", DivisionCommand, store.CrewAway)}, false},
		{"hailing", []store.Crew{crew("vance", DivisionCommand, store.CrewHailing)}, false},
		// As before #106, when it was on_deck while beaming in.
		{"beaming in", []store.Crew{crew("vance", DivisionCommand, store.CrewBeamingIn)}, false},
		{"sickbay", []store.Crew{crew("vance", DivisionCommand, store.CrewSickbay)}, true},
		{"dismissed", []store.Crew{crew("vance", DivisionCommand, store.CrewDismissed)}, false},
		{"one of two on deck", []store.Crew{crew("ross", DivisionCommand, store.CrewSickbay),
			crew("vance", DivisionCommand, store.CrewAway)}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tc.down, commandDown(tc.crew))
		})
	}
}
