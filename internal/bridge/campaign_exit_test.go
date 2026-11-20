package bridge

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/dbos-inc/dbos-transact-golang/dbos"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tscolari/bmus/internal/regs"
	"github.com/tscolari/bmus/internal/store"
	"github.com/tscolari/bmus/internal/tracker"
	"github.com/tscolari/bmus/internal/tracker/trackertest"
	"github.com/tscolari/bmus/internal/transporter"
)

// crewHolodeck is a holodeck whose beams run a script of their crew
// member's own: serve takes the last --script it is given, so the one
// appended to the spec's Args replaces the holodeck's. A crew member with
// a gate starts no serve until the gate closes, and its tour waits in
// provisioning meanwhile.
type crewHolodeck struct {
	*transporter.Holodeck
	scripts map[string]string
	gates   map[string]chan struct{}
}

func (p *crewHolodeck) StartCombadge(ctx context.Context, beam transporter.Beam, spec transporter.CombadgeSpec) error {
	if gate := p.gates[spec.CrewID]; gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if s, ok := p.scripts[spec.CrewID]; ok {
		spec.Args = append(slices.Clone(spec.Args), "--script", s)
	}
	return p.Holodeck.StartCombadge(ctx, beam, spec)
}

// Plan Verification 9, the exit test of Campaigns stage 3, on holodeck
// with the fake tracker holding #12, a bare repository as app's
// registered remote, the default regs.yaml and standing orders on.
// Sorell and tuvok are both in science. Sorell's script commits and
// reports; tuvok's reports a verdict by round, one high in round 1 and
// none after (combadge's fixtures/verdicts.json). campaign.run with
// sorell as the assignee runs round 1 on sorell. Tuvok's serve is held
// back until round 1's review sits unclaimed in the science pool while
// sorell is idle on deck: the bar keeps sorell off it. Tuvok then claims
// it and blocks, the fix order goes to sorell on round 1's head, and
// tuvok passes round 2. The captain answers land on campaign_land. The
// remote's branch is at round 2's commit with round 1's under it,
// task_workers holds sorell alone, both reviews were claimed by tuvok,
// the campaign has ended and the issue is still open.
func TestCampaignsStage3Exit(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	sorellScript := filepath.Join(t.TempDir(), "sorell.json")
	require.NoError(t, os.WriteFile(sorellScript, []byte(`[{"at_pct": 50, "commit": "Retry the webhook sender"},
		{"at_pct": 100, "report": {"summary": "retries added"}}]`), 0o600))

	fake := &trackertest.Fake{}
	for i := 1; i < 12; i++ {
		fake.Add(tracker.Item{Title: "Earlier issue"})
	}
	ref := fake.Add(tracker.Item{Title: "Webhooks are never retried", Body: "A failed delivery is dropped."})
	require.Equal(t, "#12", ref)
	up := testRepo(t)
	base := up.rev(t, "main")
	remote := filepath.Join(t.TempDir(), "app.git")
	gitT(t, "", "clone", "-q", "--bare", up.url, remote)

	b := newHolodeckBridge(t, sorellScript)
	b.repo = upstream{url: remote}
	tuvokGate := make(chan struct{})
	b.provider = &crewHolodeck{Holodeck: b.h,
		scripts: map[string]string{"tuvok": fixture(t, "verdicts.json")},
		gates:   map[string]chan struct{}{"tuvok": tuvokGate}}
	b.regs = defaultRegs(t)
	b.divisions = map[string]DivisionConfig{"science": {}}
	b.tracker = fake
	hd := b.start(t)
	on := true
	require.NoError(t, call(t, b.home, "standing_orders.set", StandingOrdersSetArgs{On: &on}, nil))
	hd.addCrewWith(t, store.Crew{ID: "sorell", Name: "Sorell", Division: "science"})
	require.NoError(t, hd.d.AddCrew(ctx, store.Crew{ID: "tuvok", Name: "Tuvok", Division: "science"}))

	var v CampaignView
	require.NoError(t, call(t, b.home, "campaign.run", CampaignRunArgs{Issue: "#12", Repo: "app", Assignee: "sorell"}, &v))
	require.Equal(t, "C-0001", v.ID)
	require.Len(t, v.Tasks, 1)
	branch := "campaign/C-0001/-12"
	require.Equal(t, branch, v.Tasks[0].Branch)
	task := store.TaskWorkflowID(v.ID, ref)

	orders := func(c require.TestingT, kind string, n int) []store.Order {
		ids, err := hd.d.store.TaskOrderIDs(ctx, task, kind)
		require.NoError(c, err)
		require.Len(c, ids, n)
		var out []store.Order
		for _, id := range ids {
			out = append(out, hd.order(c, id))
		}
		return out
	}

	// Round 1's work is sorell's; its review waits in the science pool
	// with sorell idle on deck and tuvok's beam held back.
	var work1, review1 store.Order
	var tk store.CampaignTask
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		work1 = orders(c, store.OrderKindWork, 1)[0]
		require.Equal(c, store.OrderDone, work1.State)
		review1 = orders(c, store.OrderKindReview, 1)[0]
		var err error
		tk, err = hd.d.store.GetTask(ctx, v.ID, ref)
		require.NoError(c, err)
		require.Equal(c, review1.ID, tk.OpenOrder)
		require.Equal(c, store.CrewOnDeck, hd.crew(c, "sorell").Status)
	}, 120*time.Second, 50*time.Millisecond)
	require.Equal(t, []any{"sorell", 1}, []any{work1.Assignee, work1.Round})
	require.Equal(t, []any{store.OrderPool, "", "science", 1, base, "order:" + work1.ID},
		[]any{review1.State, review1.Assignee, review1.Division, review1.Round, review1.ReviewBase, review1.Repos[0].BaseRef})
	require.Equal(t, []any{store.TaskReviewing, work1.ID, "sorell"}, []any{tk.State, tk.Head, tk.Implementer})
	c1 := tk.HeadCommit
	require.NotEqual(t, base, c1)
	close(tuvokGate)

	// Tuvok blocks round 1; the fix goes to sorell on round 1's head.
	var fix store.Order
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		fix = orders(c, store.OrderKindWork, 2)[1]
	}, 120*time.Second, 50*time.Millisecond)
	require.Equal(t, []any{"sorell", 2, "order:" + work1.ID}, []any{fix.Assignee, fix.Round, fix.Repos[0].BaseRef})
	require.Contains(t, fix.Body, "1. high: TestSnapshot races compaction")

	// Tuvok passes round 2, and the captain lands the task.
	var land store.Hail
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		hails, err := hd.d.store.ListHails(ctx, store.HailOpen)
		require.NoError(c, err)
		land = store.Hail{}
		for _, h := range hails {
			if h.Category == regs.CategoryCampaignLand {
				land = h
			}
		}
		require.NotEmpty(c, land.ID)
	}, 120*time.Second, 50*time.Millisecond)
	require.Equal(t, campaignLandKey(1), land.RaiseKey)
	require.Empty(t, gitT(t, remote, "branch", "--list", branch), "nothing is pushed before the captain answers")
	require.NoError(t, call(t, b.home, "hails.answer", HailsAnswerArgs{HailID: land.ID, Decision: "land"}, nil))

	wh, err := dbos.RetrieveWorkflow[TaskResult](hd.d.dbos, task)
	require.NoError(t, err)
	res, err := wh.GetResult(dbos.WithHandleTimeout(120 * time.Second))
	require.NoError(t, err)
	require.Equal(t, TaskResult{State: store.TaskLanded}, res)
	var show CampaignShowResult
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		require.NoError(c, call(t, b.home, "campaign.show", CampaignIDArgs{ID: v.ID}, &show))
		require.Equal(c, store.CampaignEnded, show.Campaign.State)
	}, 30*time.Second, 50*time.Millisecond)
	require.Len(t, show.Orders, 4)

	reviews := orders(t, store.OrderKindReview, 2)
	review2 := reviews[1]
	require.Equal(t, []any{store.OrderDone, store.OrderDone, 2}, []any{reviews[0].State, review2.State, review2.Round})
	tk, err = hd.d.store.GetTask(ctx, v.ID, ref)
	require.NoError(t, err)
	require.Equal(t, []any{store.TaskLanded, fix.ID, 2}, []any{tk.State, tk.Head, tk.Round})
	c2 := tk.HeadCommit
	require.NotEqual(t, c1, c2)
	require.Equal(t, c2, gitT(t, remote, "rev-parse", "refs/heads/"+branch), "the remote's branch is at round 2's commit")
	gitT(t, remote, "merge-base", "--is-ancestor", c1, "refs/heads/"+branch)
	require.Equal(t, store.HailDecided, hd.hail(t, land.ID).Status)
	require.Equal(t, "land", hd.hail(t, land.ID).Decision)

	db := campaignDB(t, b.home)
	rows, err := db.QueryContext(ctx, `SELECT crew_id FROM task_workers WHERE task = ?`, task)
	require.NoError(t, err)
	var workers []string
	for rows.Next() {
		var w string
		require.NoError(t, rows.Scan(&w))
		workers = append(workers, w)
	}
	require.NoError(t, rows.Err())
	require.NoError(t, rows.Close())
	require.Equal(t, []string{"sorell"}, workers)

	claimers := map[string][]string{}
	var texts []string
	for _, e := range hd.logEntries(t) {
		if e.Text == "order away: mission started" {
			claimers[e.OrderID] = append(claimers[e.OrderID], e.CrewID)
		}
		texts = append(texts, e.Text)
	}
	require.Equal(t, map[string][]string{work1.ID: {"sorell"}, review1.ID: {"tuvok"}, fix.ID: {"sorell"},
		review2.ID: {"tuvok"}}, claimers)
	for _, want := range []string{
		ref + " round 1: review " + review1.ID + " blocked (1 high, 0 medium)",
		ref + " round 2: review " + review2.ID + " passed",
		ref + " landed on " + branch + " at " + c2[:12],
		"campaign C-0001 ended: " + ref + " landed",
	} {
		require.Contains(t, texts, want)
	}
	require.NotContains(t, texts, review1.ID+" committed on the head it reviewed; its commits are ignored")

	issue, err := fake.Get(ctx, ref)
	require.NoError(t, err)
	require.Equal(t, tracker.StateOpen, issue.State, "push mode leaves the issue open")
}
