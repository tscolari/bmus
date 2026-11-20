package bridge

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dbos-inc/dbos-transact-golang/dbos"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tscolari/bmus/internal/regs"
	"github.com/tscolari/bmus/internal/store"
	"github.com/tscolari/bmus/internal/tracker"
	"github.com/tscolari/bmus/internal/tracker/trackertest"
)

// runArgs are campaign.run's args for the rig's issue, on repo app, with
// round 1's work for sorell.
func (g *campaignRig) runArgs() CampaignRunArgs {
	return CampaignRunArgs{Issue: g.ref, Repo: "app", Assignee: "sorell"}
}

// counts are the rows of the campaign tables and of orders, and the
// campaign entries in the ship's log.
func (g *campaignRig) counts() [4]int {
	g.t.Helper()
	var n [4]int
	db := campaignDB(g.t, g.home)
	for i, q := range []string{`SELECT COUNT(*) FROM campaigns`, `SELECT COUNT(*) FROM campaign_tasks`,
		`SELECT COUNT(*) FROM orders`, `SELECT COUNT(*) FROM log WHERE kind = 'campaign'`} {
		require.NoError(g.t, db.QueryRowContext(g.t.Context(), q).Scan(&n[i]), q)
	}
	return n
}

// Plan Verification 11: each of campaign.run's refusals creates nothing,
// neither a campaign, a task, an order nor a log entry. The issue's
// second campaign is refused once the first holds it.
func TestCampaignRunRefusals(t *testing.T) {
	t.Parallel()
	g := newCampaignRig(t, campaignOpts{noCampaign: true})
	closed := g.fake.Add(tracker.Item{Title: "Done already"})
	require.NoError(t, g.fake.Close(t.Context(), closed, ""))
	gitT(t, g.remote, "branch", "campaign/C-0001/taken", "main")

	maxRounds := func(n int) *int { return &n }
	for _, c := range []struct {
		name   string
		change func(a *CampaignRunArgs)
		code   string
		want   string
	}{
		{"ref the tracker refuses", func(a *CampaignRunArgs) { a.Issue = "twelve" }, CodeBadRequest, `issue twelve: `},
		{"issue not found", func(a *CampaignRunArgs) { a.Issue = "#99" }, CodeNotFound, "issue #99: "},
		{"issue closed", func(a *CampaignRunArgs) { a.Issue = closed }, CodeBadRequest, "issue " + closed + " is closed"},
		{"no issue", func(a *CampaignRunArgs) { a.Issue = " " }, CodeBadRequest, "an issue is required"},
		{"unknown repo", func(a *CampaignRunArgs) { a.Repo = "nope" }, CodeBadRequest, `unknown repo "nope"`},
		{"bad base", func(a *CampaignRunArgs) { a.Base = "-x" }, CodeBadRequest, `base_ref "-x" must not start with '-'`},
		{"base order missing", func(a *CampaignRunArgs) { a.Base = "order:ORD-0042" }, CodeBadRequest, "no such order"},
		{"no division or assignee", func(a *CampaignRunArgs) { a.Assignee = "" }, CodeBadRequest,
			"a division or an assignee is required"},
		{"unknown division", func(a *CampaignRunArgs) { a.Division = "nope" }, CodeBadRequest, `unknown division "nope"`},
		{"unknown assignee", func(a *CampaignRunArgs) { a.Assignee = "nobody" }, CodeNotFound, "assignee nobody"},
		{"unknown review division", func(a *CampaignRunArgs) { a.ReviewDivision = "nope" }, CodeBadRequest,
			`unknown division "nope"`},
		{"review division without crew", func(a *CampaignRunArgs) { a.ReviewDivision = DivisionCommand }, CodeBadRequest,
			"review division command has no crew member"},
		{"max rounds 0", func(a *CampaignRunArgs) { a.MaxRounds = maxRounds(0) }, CodeBadRequest, "max_rounds 0 out of range"},
		{"max rounds 11", func(a *CampaignRunArgs) { a.MaxRounds = maxRounds(11) }, CodeBadRequest, "max_rounds 11 out of range"},
		{"land pr", func(a *CampaignRunArgs) { a.Land = "pr" }, CodeBadRequest, "land pr and merge come with stage 5"},
		{"land merge", func(a *CampaignRunArgs) { a.Land = "merge" }, CodeBadRequest, "land pr and merge come with stage 5"},
		{"unknown land", func(a *CampaignRunArgs) { a.Land = "ftp" }, CodeBadRequest, `unknown land "ftp"`},
		{"bad branch", func(a *CampaignRunArgs) { a.Branch = "a..b" }, CodeBadRequest, `"a..b" is not a valid branch name`},
		{"branch on the remote", func(a *CampaignRunArgs) { a.Branch = "main" }, CodeBadRequest,
			"branch main already exists on repo app's remote, at " + g.base[:12]},
	} {
		a := g.runArgs()
		c.change(&a)
		err := call(t, g.home, "campaign.run", a, nil)
		re := requireRPCError(t, err, c.code)
		require.Contains(t, re.Message, c.want, c.name)
		require.Equal(t, [4]int{}, g.counts(), c.name)
	}

	// The default branch, campaign/C-0001/<slug>, already on the remote.
	taken := g.fake.Add(tracker.Item{Title: "Taken"})
	gitT(t, g.remote, "branch", "campaign/C-0001/"+strings.ReplaceAll(taken, "#", "-"), "main")
	a := g.runArgs()
	a.Issue = taken
	re := requireRPCError(t, call(t, g.home, "campaign.run", a, nil), CodeBadRequest)
	require.Contains(t, re.Message, "branch campaign/C-0001/-"+taken[1:]+" already exists on repo app's remote")
	require.Equal(t, [4]int{}, g.counts())

	// No tracker.
	noTracker := testConfig(privateHome(t))
	r := startDaemonConfig(t, noTracker)
	re = requireRPCError(t, call(t, r.d.home, "campaign.run", g.runArgs(), nil), CodeBadRequest)
	require.Contains(t, re.Message, "no tracker configured")

	// The issue in an open task: the first run holds it.
	require.NoError(t, call(t, g.home, "campaign.run", g.runArgs(), nil))
	// Round 1's work order, created by the task, is in before the counts.
	g.waitOpenOrder(g.waitOrder("r1:work").ID)
	before := g.counts()
	re = requireRPCError(t, call(t, g.home, "campaign.run", g.runArgs(), nil), CodeBadRequest)
	require.Equal(t, g.ref+" is already in campaign C-0001's open task", re.Message)
	require.Equal(t, before, g.counts())
}

// campaign.run inserts the campaign and its task with the default branch,
// starts the workflow (round 1's work goes to sorell), and logs the
// start; campaign.list and campaign.show read the plan's fields.
func TestCampaignRunShowList(t *testing.T) {
	t.Parallel()
	g := newCampaignRig(t, campaignOpts{noCampaign: true})
	a := g.runArgs()
	a.Division = "eng"
	var v CampaignView
	require.NoError(t, call(t, g.home, "campaign.run", a, &v))
	require.Equal(t, []any{"C-0001", store.CampaignRunning, "push", "app", "main", "eng", "sorell", "science", 3},
		[]any{v.ID, v.State, v.Land, v.Repo, v.BaseRef, v.Division, v.Assignee, v.ReviewDivision, v.MaxRounds})
	require.Len(t, v.Tasks, 1)
	require.Equal(t, []any{g.ref, g.task, store.TaskWorking, 1, 3, "campaign/C-0001/-1"},
		[]any{v.Tasks[0].Ref, v.Tasks[0].WorkflowID, v.Tasks[0].State, v.Tasks[0].Round, v.Tasks[0].MaxRounds, v.Tasks[0].Branch})
	require.Contains(t, g.texts(), "campaign C-0001 started: "+g.ref+" on app from main: work for sorell, reviews in science, "+
		"up to 3 rounds, landing by push to campaign/C-0001/-1")

	work := g.waitOrder("r1:work")
	require.Equal(t, []any{"sorell", store.OrderKindWork, 1}, []any{work.Assignee, work.Kind, work.Round})
	g.waitOpenOrder(work.ID)

	var show CampaignShowResult
	require.NoError(t, call(t, g.home, "campaign.show", CampaignIDArgs{ID: "C-0001"}, &show))
	require.Equal(t, work.ID, show.Campaign.Tasks[0].OpenOrder)
	require.Len(t, show.Orders, 1)
	require.Equal(t, []any{work.ID, store.OrderKindWork, 1, store.OrderDirected, "sorell"},
		[]any{show.Orders[0].ID, show.Orders[0].Kind, show.Orders[0].Round, show.Orders[0].State, show.Orders[0].Assignee})

	// A second campaign on another issue, with its own branch and
	// rounds, pooled in eng.
	other := g.fake.Add(tracker.Item{Title: "Second"})
	rounds := 1
	require.NoError(t, call(t, g.home, "campaign.run", CampaignRunArgs{Issue: other, Repo: "app", Division: "eng",
		MaxRounds: &rounds, Branch: "feature/second"}, &v))
	require.Equal(t, []any{"C-0002", 1, "feature/second"}, []any{v.ID, v.MaxRounds, v.Tasks[0].Branch})
	require.Contains(t, g.texts(), "campaign C-0002 started: "+other+" on app from main: work in eng, reviews in science, "+
		"1 round, landing by push to feature/second")

	// A third on the second's branch is refused in the insert, and
	// creates nothing.
	third := g.fake.Add(tracker.Item{Title: "Third"})
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		tk, err := g.d.store.GetTask(t.Context(), "C-0002", other)
		require.NoError(c, err)
		require.NotEmpty(c, tk.OpenOrder)
	}, 30*time.Second, 20*time.Millisecond)
	before := g.counts()
	re := requireRPCError(t, call(t, g.home, "campaign.run", CampaignRunArgs{Issue: third, Repo: "app", Division: "eng",
		Branch: "feature/second"}, nil), CodeBadRequest)
	require.Equal(t, "branch feature/second on repo app is campaign C-0002's, whose task "+other+" is open", re.Message)
	require.Equal(t, before, g.counts())

	var list CampaignListResult
	require.NoError(t, call(t, g.home, "campaign.list", nil, &list))
	require.Len(t, list.Campaigns, 2)
	require.Equal(t, []string{"C-0001", "C-0002"}, []string{list.Campaigns[0].ID, list.Campaigns[1].ID})
	require.Equal(t, other, list.Campaigns[1].Tasks[0].Ref)

	requireRPCError(t, call(t, g.home, "campaign.show", CampaignIDArgs{ID: "C-0009"}, nil), CodeNotFound)
	requireRPCError(t, call(t, g.home, "campaign.show", CampaignIDArgs{}, nil), CodeBadRequest)
}

// requireNoStall requires that no campaign_stalled hail was ever raised.
func (g *campaignRig) requireNoStall() {
	g.t.Helper()
	hails, err := g.d.store.ListHails(g.t.Context(), "")
	require.NoError(g.t, err)
	for _, h := range hails {
		require.NotEqual(g.t, regs.CategoryCampaignStalled, h.Category, "hail %s", h.ID)
	}
}

// belay calls campaign.belay on the rig's campaign.
func (g *campaignRig) belayRPC(reason string) CampaignBelayResult {
	g.t.Helper()
	var res CampaignBelayResult
	require.NoError(g.t, call(g.t, g.home, "campaign.belay", CampaignBelayArgs{ID: testCampaign, Reason: reason}, &res))
	return res
}

// Plan Verification 11: campaign.belay with an open order ends the
// campaign with no stall hail: an unclaimed order is withdrawn. A belay of
// an ended campaign is refused, and of none is not found. A claimed order
// is TestCampaignBelayHolodeck's.
func TestCampaignBelayOpenOrder(t *testing.T) {
	t.Parallel()
	{
		g := newCampaignRig(t, campaignOpts{})
		work := g.waitOrder("r1:work")
		g.waitOpenOrder(work.ID)
		res := g.belayRPC("wrong issue")
		require.Equal(t, store.CampaignBelaying, res.Campaign.State)
		require.Equal(t, []CampaignOrderEnded{{ID: work.ID, Action: taskOrderWithdrawn}}, res.Orders)
		require.Empty(t, res.Hails)
		require.Equal(t, TaskResult{State: store.TaskDropped, Why: "campaign belayed"}, g.result())
		require.Equal(t, store.OrderWithdrawn, getOrder(t, g.d, work.ID).State)
		g.requireNoStall()
		require.Contains(t, g.texts(), "campaign C-0001 belayed by the captain: wrong issue")

		re := requireRPCError(t, call(t, g.home, "campaign.belay", CampaignBelayArgs{ID: testCampaign}, nil), CodeBadRequest)
		require.Equal(t, "campaign C-0001 has ended", re.Message)
		requireRPCError(t, call(t, g.home, "campaign.belay", CampaignBelayArgs{ID: "C-0002"}, nil), CodeNotFound)
	}
}

// Plan Verification 11: campaign.belay with an open land hail closes it
// timed out ("campaign belayed") and ends the campaign with no stall
// hail, no second land hail and no push.
func TestCampaignBelayLandHail(t *testing.T) {
	t.Parallel()
	g := newCampaignRig(t, campaignOpts{})
	work, _ := g.pass()
	h, _ := g.landHail()
	// Belay after the task's own check of the campaign once it raised the
	// hail, or that check closes the hail and the sweep finds none.
	g.waitStepAfter("task-land-hail", "check-campaign")
	res := g.belayRPC("")
	require.Equal(t, []string{h.ID}, res.Hails)
	require.Empty(t, res.Orders)
	require.Equal(t, TaskResult{State: store.TaskDropped, Why: "campaign belayed"}, g.result())
	closed, err := g.d.store.GetHail(t.Context(), h.ID)
	require.NoError(t, err)
	require.Equal(t, []string{store.HailTimedOut, campaignBelayedWhy}, []string{closed.Status, closed.Rationale})
	require.Equal(t, []string{"land-1"}, g.landHails())
	require.Empty(t, g.remoteAt(testBranch))
	require.Empty(t, g.pushes(work.ID))
	g.requireNoStall()
	require.Contains(t, g.texts(), fmt.Sprintf("%s dropped in round 1: campaign belayed", g.ref))
}

// campaign.belay with a stall hail open closes it, and the task drops
// with no further stall hail.
func TestCampaignBelayStallHail(t *testing.T) {
	t.Parallel()
	g := newCampaignRig(t, campaignOpts{})
	work := g.waitOrder("r1:work")
	g.claim(work.ID, "sorell")
	g.fail(work.ID, "crew_error")
	h, _ := g.stallHail()
	g.waitStepAfter("task-stall-hail", "check-campaign")
	res := g.belayRPC("")
	require.Equal(t, []string{h.ID}, res.Hails)
	require.Equal(t, TaskResult{State: store.TaskDropped, Why: "campaign belayed"}, g.result())
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		hails, err := g.d.store.ListHails(t.Context(), "")
		require.NoError(c, err)
		require.Len(c, hails, 1)
	}, 5*time.Second, 20*time.Millisecond)
}

// Plan Verification 11 on holodeck: campaign.run starts the campaign, and
// sorell's beam takes round 1's work and sleeps. campaign.belay asks the
// order's mission for a belay; run again before the mission settles it,
// it logs nothing new for the campaign. The mission belays the beam, the
// task drops with no stall hail and the campaign ends.
func TestCampaignBelayHolodeck(t *testing.T) {
	t.Parallel()
	script := filepath.Join(t.TempDir(), "sleep.json")
	require.NoError(t, os.WriteFile(script, []byte(`[{"at_pct": 10, "progress": "working"}, {"at_pct": 10, "sleep_ms": 120000},
		{"at_pct": 100, "report": {"summary": "never"}}]`), 0o600))
	b := newHolodeckBridge(t, script)
	b.divisions = map[string]DivisionConfig{"eng": {}, "science": {}}
	fake := &trackertest.Fake{}
	ref := fake.Add(tracker.Item{Title: "Fix the flake"})
	b.tracker = fake
	hd := b.start(t)
	ctx := t.Context()
	hd.addCrewWith(t, store.Crew{ID: "sorell", Name: "Sorell", Division: "eng"})
	hd.addCrewWith(t, store.Crew{ID: "tuvok", Name: "Tuvok", Division: "science"})

	var v CampaignView
	require.NoError(t, call(t, b.home, "campaign.run", CampaignRunArgs{Issue: ref, Repo: "app", Assignee: "sorell"}, &v))
	task := store.TaskWorkflowID(v.ID, ref)
	var work store.Order
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		ids, err := hd.d.store.TaskOrderIDs(ctx, task, store.OrderKindWork)
		require.NoError(c, err)
		require.Len(c, ids, 1)
		work, err = hd.d.store.GetOrder(ctx, ids[0])
		require.NoError(c, err)
		require.NotNil(c, work.ProgressPct)
	}, 120*time.Second, 50*time.Millisecond)

	var res CampaignBelayResult
	require.NoError(t, call(t, b.home, "campaign.belay", CampaignBelayArgs{ID: v.ID, Reason: "not now"}, &res))
	require.Equal(t, []CampaignOrderEnded{{ID: work.ID, Action: taskOrderBelay}}, res.Orders)
	require.NoError(t, call(t, b.home, "campaign.belay", CampaignBelayArgs{ID: v.ID}, &res))

	wh, err := dbos.RetrieveWorkflow[TaskResult](hd.d.dbos, task)
	require.NoError(t, err)
	tr, err := wh.GetResult(dbos.WithHandleTimeout(120 * time.Second))
	require.NoError(t, err)
	require.Equal(t, TaskResult{State: store.TaskDropped, Why: "campaign belayed"}, tr)
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		cp, err := hd.d.store.GetCampaign(ctx, v.ID)
		require.NoError(c, err)
		require.Equal(c, store.CampaignEnded, cp.State)
	}, 30*time.Second, 50*time.Millisecond)
	require.Equal(t, store.OrderBelayed, getOrder(t, hd.d, work.ID).State)
	hails, err := hd.d.store.ListHails(ctx, "")
	require.NoError(t, err)
	require.Empty(t, hails)
	n := 0
	for _, e := range hd.logEntries(t) {
		if strings.HasPrefix(e.Text, "campaign "+v.ID+" belayed by the captain") {
			n++
		}
	}
	require.Equal(t, 1, n)
}

// A belay that commits while the task creates an order or raises a hail,
// after the belay's sweep looked, is caught by the task's own check after
// that step (#186). The hook sets the campaign belaying right after the
// step, as a belay whose sweep found nothing yet would leave it: the
// order is withdrawn or the hail closed timed out, and the task drops
// with no stall hail, no second land hail and no push.
func TestCampaignBelayRacesStep(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		step string
		run  func(g *campaignRig)
	}{
		{"create-work", func(g *campaignRig) {
			work := g.waitOrder("r1:work")
			require.Equal(t, TaskResult{State: store.TaskDropped, Why: "campaign belayed"}, g.result())
			require.Equal(t, store.OrderWithdrawn, getOrder(t, g.d, work.ID).State)
		}},
		{"task-stall-hail", func(g *campaignRig) {
			work := g.waitOrder("r1:work")
			g.claim(work.ID, "sorell")
			g.fail(work.ID, "crew_error")
			require.Equal(t, TaskResult{State: store.TaskDropped, Why: "campaign belayed"}, g.result())
			hails, err := g.d.store.ListHails(t.Context(), "")
			require.NoError(t, err)
			require.Len(t, hails, 1)
			require.Equal(t, []string{store.HailTimedOut, campaignBelayedWhy}, []string{hails[0].Status, hails[0].Rationale})
		}},
		{"task-land-hail", func(g *campaignRig) {
			work, _ := g.pass()
			require.Equal(t, TaskResult{State: store.TaskDropped, Why: "campaign belayed"}, g.result())
			require.Equal(t, []string{"land-1"}, g.landHails())
			hails, err := g.d.store.ListHails(t.Context(), store.HailTimedOut)
			require.NoError(t, err)
			require.Len(t, hails, 1)
			require.Equal(t, campaignBelayedWhy, hails[0].Rationale)
			require.Empty(t, g.remoteAt(testBranch))
			require.Empty(t, g.pushes(work.ID))
			g.requireNoStall()
		}},
	} {
		t.Run(c.step, func(t *testing.T) {
			t.Parallel()
			var once sync.Once
			g := newCampaignRig(t, campaignOpts{stepHook: func(d *Daemon, step string) {
				if step != c.step {
					return
				}
				once.Do(func() {
					db, err := sql.Open("sqlite", filepath.Join(d.home, store.DBFile)+"?_pragma=busy_timeout(5000)")
					if assert.NoError(t, err) {
						defer db.Close()
						_, err = db.Exec(`UPDATE campaigns SET state = 'belaying'`)
						assert.NoError(t, err)
					}
				})
			}})
			c.run(g)
		})
	}
}
