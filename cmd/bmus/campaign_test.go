package main

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tscolari/bmus/internal/bridge"
	"github.com/tscolari/bmus/internal/regs"
	"github.com/tscolari/bmus/internal/store"
)

// testCampaignView is campaign C-0001 on o/r#12, stalled in round 2 of 3
// with a reason holding control characters.
func testCampaignView() bridge.CampaignView {
	created := time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)
	return bridge.CampaignView{ID: "C-0001", State: store.CampaignRunning, Land: "push", Repo: "app", BaseRef: "main",
		Division: "eng", Assignee: "sorell", ReviewDivision: "science", MaxRounds: 3, CreatedAt: created,
		Tasks: []bridge.CampaignTaskView{{Ref: "o/r#12", WorkflowID: "campaign/C-0001/task/o/r#12", State: store.TaskStalled,
			Round: 2, MaxRounds: 3, Branch: "campaign/C-0001/o-r-12", Implementer: "sorell", Head: "ORD-0003",
			HeadCommit: "3f2a9c1e0b7d55aa", OpenHail: "H-0002", Why: "it broke\n\x1b[2Jbadly"}}}
}

// show prints the campaign, its task and one row per order, with the
// stall's reason on one line and no escape left.
func TestPrintCampaign(t *testing.T) {
	t.Parallel()
	var b bytes.Buffer
	require.NoError(t, printCampaign(&b, bridge.CampaignShowResult{Campaign: testCampaignView(), Orders: []bridge.OrderView{
		{ID: "ORD-0001", Kind: "work", Round: 1, State: store.OrderDone, Assignee: "sorell"},
		{ID: "ORD-0002", Kind: "review", Round: 1, State: store.OrderDone, Assignee: "tuvok"},
		{ID: "ORD-0004", Kind: "review", Round: 2, State: store.OrderFailed},
	}}))
	require.Equal(t, `campaign    C-0001
state       running
repo        app from main
work        sorell (eng)
reviews     science pool
max rounds  3
land        push
created     2026-11-19T10:00:00Z

task        o/r#12
state       stalled
round       2 of 3
branch      campaign/C-0001/o-r-12
head        ORD-0003 at 3f2a9c1e0b7d
open order  -
open hail   H-0002
why         it broke [2Jbadly

ORDER     KIND    ROUND  STATE   ASSIGNEE
ORD-0001  work    1      done    sorell
ORD-0002  review  1      done    tuvok
ORD-0004  review  2      failed  -
`, b.String())
	require.NotContains(t, b.String(), "\x1b")
}

// list prints one row per campaign, its task's ref, state and round.
func TestPrintCampaigns(t *testing.T) {
	t.Parallel()
	ended := testCampaignView()
	ended.ID, ended.State, ended.Tasks[0].State = "C-0002", store.CampaignEnded, store.TaskLanded
	var b bytes.Buffer
	require.NoError(t, printCampaigns(&b, []bridge.CampaignView{testCampaignView(), ended}))
	require.Equal(t, `ID      STATE    ISSUE   TASK     ROUND  REPO  BRANCH
C-0001  running  o/r#12  stalled  2/3    app   campaign/C-0001/o-r-12
C-0002  ended    o/r#12  landed   2/3    app   campaign/C-0001/o-r-12
`, b.String())
}

// run prints the plan's line; belay names what it closed and ended.
func TestCampaignLines(t *testing.T) {
	t.Parallel()
	v := testCampaignView()
	require.Equal(t, "campaign C-0001 running o/r#12 on app from main: work for sorell, reviews in science, up to 3 rounds, "+
		"landing by push to campaign/C-0001/o-r-12", runLine(v))
	v.Assignee, v.MaxRounds = "", 1
	require.Equal(t, "campaign C-0001 running o/r#12 on app from main: work in eng, reviews in science, 1 round, "+
		"landing by push to campaign/C-0001/o-r-12", runLine(v))

	v.State = store.CampaignBelaying
	require.Equal(t, "campaign C-0001 belaying: hail H-0002 closed, order ORD-0004 belay requested",
		campaignBelayLine(bridge.CampaignBelayResult{Campaign: v, Hails: []string{"H-0002"},
			Orders: []bridge.CampaignOrderEnded{{ID: "ORD-0004", Action: "belay requested"}}}))
	require.Equal(t, "campaign C-0001 belaying", campaignBelayLine(bridge.CampaignBelayResult{Campaign: v}))
}

// `bmus campaign` against a bridge with no tracker: run is refused and
// creates nothing, as are pr and an out-of-range max-rounds. A campaign
// inserted before the bridge starts runs at startup and stalls (its
// work order's tracker source is refused); show and list print it, belay
// closes its stall hail, and the campaign ends with no second stall hail.
func TestCampaignCLI(t *testing.T) {
	t.Parallel()
	e := newE2E(t)
	ctx := t.Context()
	st, err := store.Open(ctx, filepath.Join(e.home, store.DBFile))
	require.NoError(t, err)
	require.NoError(t, st.InsertCrew(ctx, store.Crew{ID: "sorell", Name: "Sorell", Division: "eng", Status: store.CrewOnDeck},
		store.LogEntry{Kind: "crew", Text: "added"}))
	require.NoError(t, st.InsertCampaign(ctx, store.Campaign{ID: "C-0001", Land: "push", Repo: "app", BaseRef: "main",
		Assignee: "sorell", ReviewDivision: "science", MaxRounds: 3},
		store.CampaignTask{Ref: "#12", MaxRounds: 3, Branch: "campaign/C-0001/-12"}, store.LogEntry{Text: "started"}))
	require.NoError(t, st.Close())
	e.startBridge(t)

	fail := func(args ...string) string {
		t.Helper()
		cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		var stderr bytes.Buffer
		cmd := e.cmd(cctx, args...)
		cmd.Stderr = &stderr
		require.Error(t, cmd.Run(), "bmus %s", strings.Join(args, " "))
		return stderr.String()
	}
	require.Contains(t, fail("campaign", "run", "--issue", "#13", "--repo", "app", "--assignee", "sorell"),
		"campaign run: no tracker configured")
	require.Contains(t, fail("campaign", "run", "--issue", "#13", "--repo", "app", "--assignee", "sorell", "--land", "pr"),
		"land pr and merge come with stage 5")
	require.Contains(t, fail("campaign", "run", "--issue", "#13", "--repo", "app", "--assignee", "sorell", "--max-rounds", "0"),
		"max_rounds 0 out of range (1 to 10)")
	require.Contains(t, fail("campaign", "run", "--repo", "app"), `required flag(s) "issue" not set`)
	require.Contains(t, fail("campaign", "show", "C-0009"), "campaign C-0009: not found")

	var stalled bridge.CampaignShowResult
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		out := e.run(c, "campaign", "show", "C-0001", "--json")
		stalled = bridge.CampaignShowResult{}
		require.NoError(c, json.Unmarshal([]byte(out), &stalled))
		require.Equal(c, store.TaskStalled, stalled.Campaign.Tasks[0].State)
		require.NotEmpty(c, stalled.Campaign.Tasks[0].OpenHail)
	}, 60*time.Second, 100*time.Millisecond)
	hailID := stalled.Campaign.Tasks[0].OpenHail
	require.Empty(t, stalled.Orders)
	out := e.run(t, "campaign", "show", "C-0001")
	require.Regexp(t, `(?m)^state\s+stalled$`, out)
	require.Regexp(t, `(?m)^open hail\s+`+hailID+`$`, out)
	require.Regexp(t, `(?m)^why\s+.*order source tracker:#12: no tracker configured`, out)
	require.Regexp(t, `(?m)^C-0001\s+running\s+#12\s+stalled\s+1/3\s+app\s+campaign/C-0001/-12$`,
		e.run(t, "campaign", "list"))

	require.Equal(t, "campaign C-0001 belaying: hail "+hailID+" closed\n", e.run(t, "campaign", "belay", "C-0001", "--reason", "no tracker"))
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		var list bridge.CampaignListResult
		e.runJSON(c, &list, "campaign", "list")
		require.Len(c, list.Campaigns, 1)
		require.Equal(c, store.CampaignEnded, list.Campaigns[0].State)
		require.Equal(c, store.TaskDropped, list.Campaigns[0].Tasks[0].State)
		require.Equal(c, "campaign belayed", list.Campaigns[0].Tasks[0].Why)
	}, 60*time.Second, 100*time.Millisecond)
	var hails bridge.HailsListResult
	e.runJSON(t, &hails, "hails", "list", "--status", "all")
	n := 0
	for _, h := range hails.Hails {
		if h.Category == regs.CategoryCampaignStalled {
			n++
			require.Equal(t, store.HailTimedOut, h.Status)
		}
	}
	require.Equal(t, 1, n)
	require.Contains(t, fail("campaign", "belay", "C-0001"), "campaign C-0001 has ended")
}
