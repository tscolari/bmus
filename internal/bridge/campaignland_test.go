package bridge

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/dbos-inc/dbos-transact-golang/dbos"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tscolari/bmus/internal/comms"
	"github.com/tscolari/bmus/internal/regs"
	"github.com/tscolari/bmus/internal/store"
)

// testBranch is the rig's land branch.
const testBranch = "campaign/C-0001/1"

// The default land branch slugs the ref (decision 6), and a slug git
// wouldn't take as a branch is refused.
func TestCampaignBranch(t *testing.T) {
	t.Parallel()
	for ref, want := range map[string]string{
		"o/r#12":       "campaign/C-0001/o-r-12",
		"#1":           "campaign/C-0001/-1",
		"acme/app_x#3": "campaign/C-0001/acme-app_x-3",
		"o/r#é‮":       "campaign/C-0001/o-r---",
		"o/r.v2#1":     "campaign/C-0001/o-r.v2-1",
	} {
		got, err := campaignBranch(t.Context(), "C-0001", ref)
		require.NoError(t, err, ref)
		require.Equal(t, want, got, ref)
	}
	for _, ref := range []string{"o/r..x", "x.lock", "o/r#1."} {
		_, err := campaignBranch(t.Context(), "C-0001", ref)
		require.ErrorContains(t, err, "not a valid branch name", ref)
	}
}

// pass runs round 1 to a passing review and returns the work order and
// its commit.
func (g *campaignRig) pass() (store.Order, string) {
	g.t.Helper()
	work := g.waitOrder("r1:work")
	c1 := g.work(work.ID, "sorell", g.base)
	g.reviewDone(g.waitOrder("r1:review").ID, "tuvok", c1, verdictOf())
	return work, c1
}

// blockAtMaxRounds runs round 1 of a one-round task to a review that
// blocks, which stalls max_rounds, and returns the work order, its commit,
// the review and the stall hail.
func (g *campaignRig) blockAtMaxRounds() (store.Order, string, store.Order, store.Hail) {
	g.t.Helper()
	work := g.waitOrder("r1:work")
	c1 := g.work(work.ID, "sorell", g.base)
	review := g.waitOrder("r1:review")
	g.reviewDone(review.ID, "tuvok", c1, blockHigh())
	h, c := g.stallHail()
	require.Equal(g.t, stallMaxRounds, c.Code)
	return work, c1, review, h
}

// decideAs stores decision on open hail id as decided by by, with no
// countermand window, and wakes its raiser.
func (g *campaignRig) decideAs(id, decision, by string) {
	g.t.Helper()
	_, err := campaignDB(g.t, g.home).ExecContext(g.t.Context(),
		`UPDATE hails SET status = 'decided', decision = ?, decided_by = ?, decided_at = ? WHERE id = ?`,
		decision, by, time.Now().UTC(), id)
	require.NoError(g.t, err)
	require.NoError(g.t, dbos.Send(g.d.dbos, g.task, id, hailClosedTopic))
}

// waitText waits for a log entry whose text starts with prefix and
// returns its text.
func (g *campaignRig) waitText(prefix string) string {
	g.t.Helper()
	var text string
	require.EventuallyWithT(g.t, func(c *assert.CollectT) {
		entries, err := g.d.store.ListLog(g.t.Context(), 0)
		require.NoError(c, err)
		text = ""
		for _, e := range entries {
			if strings.HasPrefix(e.Text, prefix) {
				text = e.Text
			}
		}
		require.NotEmpty(c, text)
	}, 30*time.Second, 20*time.Millisecond)
	return text
}

// landHails are the task's campaign_land hails' raise keys.
func (g *campaignRig) landHails() []string {
	g.t.Helper()
	hails, err := g.d.store.ListHails(g.t.Context(), "")
	require.NoError(g.t, err)
	var keys []string
	for _, h := range hails {
		if h.Category == regs.CategoryCampaignLand {
			keys = append(keys, h.RaiseKey)
		}
	}
	return keys
}

// Plan Verification 8: a push replayed after the remote already holds
// the head pushes nothing, and the task lands.
func TestCampaignLandAlreadyPushed(t *testing.T) {
	t.Parallel()
	g := newCampaignRig(t, campaignOpts{})
	work, c1 := g.pass()
	h, _ := g.landHail()
	gitT(t, g.clone, "push", "-q", g.remote, c1+":refs/heads/"+testBranch)
	g.answer(h.ID, "land")
	require.Equal(t, TaskResult{State: store.TaskLanded}, g.result())
	require.Equal(t, c1, g.remoteAt(testBranch))
	require.Empty(t, g.pushes(work.ID), "nothing pushed again")
	require.Contains(t, g.texts(), g.ref+" landed on "+testBranch+" at "+c1[:12])
}

// Plan Verification 8: a remote branch moved on stalls push_rejected at
// once, with the head offered to land again. The captain's retry pushes
// again at once, with no new campaign_land.
func TestCampaignLandPushRejected(t *testing.T) {
	t.Parallel()
	g := newCampaignRig(t, campaignOpts{})
	work, c1 := g.pass()
	h, _ := g.landHail()
	other := g.commit("ORD-9999", g.base)
	gitT(t, g.clone, "push", "-q", g.remote, other+":refs/heads/"+testBranch)
	g.answer(h.ID, "land")

	sh, c := g.stallHail()
	require.Equal(t, []any{stallPushRejected, work.ID, work.ID, c1}, []any{c.Code, c.Order, c.Head, c.HeadCommit})
	require.Contains(t, c.Reason, "the push of "+work.ID+" to "+testBranch+" was rejected")
	require.Equal(t, campaignLandOptions, sh.Options)
	require.Equal(t, other, g.remoteAt(testBranch))
	require.Equal(t, []string{"repo app: push to " + testBranch + " at " + c1[:12] + " failed"}, g.pushes(work.ID),
		"rejected once, not retried")

	gitT(t, g.remote, "update-ref", "-d", "refs/heads/"+testBranch)
	g.answer(sh.ID, "retry")
	require.Equal(t, store.TaskLanded, g.result().State)
	require.Equal(t, c1, g.remoteAt(testBranch))
	require.Equal(t, []string{"land-1"}, g.landHails())
}

// A remote out of reach is tried 4 times in land-push and then stalls
// push_failed; a retry once it is back pushes.
func TestCampaignLandPushFailed(t *testing.T) {
	t.Parallel()
	g := newCampaignRig(t, campaignOpts{})
	work, c1 := g.pass()
	h, _ := g.landHail()
	away := g.remote + ".away"
	require.NoError(t, os.Rename(g.remote, away))
	g.answer(h.ID, "land")

	sh, c := g.stallHail()
	require.Equal(t, []any{stallPushFailed, work.ID}, []any{c.Code, c.Head})
	require.Contains(t, c.Reason, "failed 4 times")
	require.NoError(t, os.Rename(away, g.remote))
	g.answer(sh.ID, "retry")
	require.Equal(t, store.TaskLanded, g.result().State)
	require.Equal(t, c1, g.remoteAt(testBranch))
}

// Plan Verification 8: land on a stall decided by the captain pushes at
// once; decided by Command it raises campaign_land, whose context says
// no review passed.
func TestCampaignLandFromStall(t *testing.T) {
	t.Parallel()
	t.Run("captain", func(t *testing.T) {
		t.Parallel()
		g := newCampaignRig(t, campaignOpts{maxRounds: 1})
		work, c1, _, h := g.blockAtMaxRounds()
		g.answer(h.ID, "land")
		require.Equal(t, store.TaskLanded, g.result().State)
		require.Equal(t, c1, g.remoteAt(testBranch))
		require.Empty(t, g.landHails())
		require.Contains(t, g.texts(), g.ref+" round 1: "+work.ID+" ("+c1[:12]+") lands on "+testBranch+" as the captain decided")
	})
	t.Run("command", func(t *testing.T) {
		t.Parallel()
		g := newCampaignRig(t, campaignOpts{maxRounds: 1})
		work, c1, review, h := g.blockAtMaxRounds()
		g.decideAs(h.ID, "land", comms.DecidedByCommand)
		lh, lc := g.landHail()
		require.Equal(t, []any{work.ID, review.ID, false, 1}, []any{lc.Head, lc.Review, lc.Passed, lc.CommitCount})
		require.Contains(t, lh.Question, "in round 1 with no passing review?")
		require.Empty(t, g.remoteAt(testBranch), "nothing pushed before the captain answers")
		g.answer(lh.ID, "land")
		require.Equal(t, store.TaskLanded, g.result().State)
		require.Equal(t, c1, g.remoteAt(testBranch))
	})
}

// Plan Verification 8: campaign_land goes to the captain under a
// regs.yaml whose captain timeout would decide any other hail, and never
// times out. retry starts a fix round past max_rounds with the
// captain's note as its one blocking finding, and the next land hail,
// land-2, drops the task.
func TestCampaignLandCaptainRetryDrop(t *testing.T) {
	t.Parallel()
	cfg := defaultRegs(t)
	zero := 0
	cfg.Escalation = regs.Escalation{CaptainTimeout: time.Second, OnTimeout: regs.OnTimeoutDefaultOption,
		DefaultOption: &zero}
	g := newCampaignRig(t, campaignOpts{maxRounds: 1, regs: cfg})
	work, c1 := g.pass()
	h, _ := g.landHail()
	e := g.waitText("hail " + h.ID + " for the captain")
	require.Equal(t, "hail "+h.ID+" for the captain: category campaign_land is fixed to the captain", e)
	require.Never(t, func() bool {
		hl, err := g.d.store.GetHail(t.Context(), h.ID)
		return err != nil || hl.Status != store.HailOpen
	}, 2500*time.Millisecond, 50*time.Millisecond, "no captain timeout")

	g.answer(h.ID, "retry: tighten the docs")
	fix := g.waitOrder("r2:fix")
	require.Contains(t, fix.Body, "The captain asked for another round")
	require.Contains(t, fix.Body, "tighten the docs")
	require.Equal(t, "order:"+work.ID, fix.Repos[0].BaseRef)
	c2 := g.work(fix.ID, "sorell", c1)
	review := g.waitOrder("r2:review")
	require.Contains(t, review.Body, "The captain asked for another round on the findings below")
	g.reviewDone(review.ID, "tuvok", c2, verdictOf())

	h2, lc := g.landHail()
	require.Equal(t, []any{"land-2", 2, review.ID, true}, []any{h2.RaiseKey, lc.Rounds, lc.Review, lc.Passed})
	g.answer(h2.ID, "drop")
	require.Equal(t, TaskResult{State: store.TaskDropped, Why: "dropped on " + h2.ID}, g.result())
	require.Empty(t, g.remoteAt(testBranch))
}

// A land hail closed undecided with the campaign running asks again after
// one recheck (land-2); closed with the campaign being belayed, it drops
// the task. A captain's land on a stall of a campaign being belayed drops
// the task and pushes nothing.
func TestCampaignLandBelayed(t *testing.T) {
	t.Parallel()
	t.Run("land hail closed", func(t *testing.T) {
		t.Parallel()
		g := newCampaignRig(t, campaignOpts{})
		g.pass()
		h, _ := g.landHail()
		g.closeHail(h.ID)
		require.EventuallyWithT(t, func(c *assert.CollectT) {
			require.Equal(c, []string{"land-1", "land-2"}, g.landHails())
		}, 30*time.Second, 20*time.Millisecond)
		h2, _ := g.landHail()
		g.waitStepAfter("task-land-hail", "check-campaign")
		g.belayCampaign()
		g.closeHail(h2.ID)
		require.Equal(t, TaskResult{State: store.TaskDropped, Why: "campaign belayed"}, g.result())
		require.Empty(t, g.remoteAt(testBranch))
	})
	t.Run("land hail answered land", func(t *testing.T) {
		t.Parallel()
		g := newCampaignRig(t, campaignOpts{})
		work, _ := g.pass()
		h, _ := g.landHail()
		g.waitStepAfter("task-land-hail", "check-campaign")
		g.belayCampaign()
		g.answer(h.ID, "land")
		require.Equal(t, TaskResult{State: store.TaskDropped, Why: "campaign belayed"}, g.result())
		require.Empty(t, g.remoteAt(testBranch))
		require.Empty(t, g.pushes(work.ID))
	})
	t.Run("land on a stall", func(t *testing.T) {
		t.Parallel()
		g := newCampaignRig(t, campaignOpts{maxRounds: 1})
		work, _, _, h := g.blockAtMaxRounds()
		g.waitStepAfter("task-stall-hail", "check-campaign")
		g.belayCampaign()
		g.answer(h.ID, "land")
		require.Equal(t, TaskResult{State: store.TaskDropped, Why: "campaign belayed"}, g.result())
		require.Empty(t, g.remoteAt(testBranch))
		require.Empty(t, g.pushes(work.ID))
		require.Empty(t, g.landHails())
	})
}

// Plan Verification 10 for the land hail: the bridge stopped while the
// hail is open resumes the task, the captain's land pushes once, and each
// order and hail was created once.
func TestCampaignRestartDuringLand(t *testing.T) {
	t.Parallel()
	g := newCampaignRig(t, campaignOpts{})
	work, c1 := g.pass()
	h, _ := g.landHail()
	g.restart()
	requirePending(t, g.d, g.task)
	g.answer(h.ID, "land")
	require.Equal(t, store.TaskLanded, g.result().State)
	require.Equal(t, c1, g.remoteAt(testBranch))
	require.Equal(t, []string{"repo app pushed to " + testBranch + " at " + c1[:12]}, g.pushes(work.ID))
	require.Equal(t, []string{"r1:work", "r1:review"}, g.taskOrders())
	require.Equal(t, []string{"land-1"}, g.landHails())
}

// A stall before the push had no land consent: after Command's land on a
// stall, a land context git can't read stalls push_failed, and the
// captain's retry of it asks with campaign_land before anything is
// pushed. Command's retry of a push_rejected stall asks again under
// land-2 and pushes nothing.
func TestCampaignLandRetryConsent(t *testing.T) {
	t.Parallel()
	t.Run("land-context stall", func(t *testing.T) {
		t.Parallel()
		g := newCampaignRig(t, campaignOpts{maxRounds: 1})
		work, c1, _, h := g.blockAtMaxRounds()
		away := g.clone + ".away"
		require.NoError(t, os.Rename(g.clone, away))
		g.decideAs(h.ID, "land", comms.DecidedByCommand)
		sh, c := g.stallHail()
		require.Equal(t, stallPushFailed, c.Code)
		require.Contains(t, c.Reason, "land-context: ")
		require.NoError(t, os.Rename(away, g.clone))
		g.answer(sh.ID, "retry")
		lh, _ := g.landHail()
		require.Equal(t, "land-1", lh.RaiseKey)
		require.Empty(t, g.remoteAt(testBranch), "nothing pushed before the captain answers campaign_land")
		require.Empty(t, g.pushes(work.ID))
		g.answer(lh.ID, "land")
		require.Equal(t, store.TaskLanded, g.result().State)
		require.Equal(t, c1, g.remoteAt(testBranch))
	})
	t.Run("command retry of push_rejected", func(t *testing.T) {
		t.Parallel()
		g := newCampaignRig(t, campaignOpts{})
		work, _ := g.pass()
		h, _ := g.landHail()
		other := g.commit("ORD-9999", g.base)
		gitT(t, g.clone, "push", "-q", g.remote, other+":refs/heads/"+testBranch)
		g.answer(h.ID, "land")
		sh, c := g.stallHail()
		require.Equal(t, stallPushRejected, c.Code)
		g.decideAs(sh.ID, "retry", comms.DecidedByCommand)
		require.EventuallyWithT(t, func(c *assert.CollectT) {
			require.Equal(c, []string{"land-1", "land-2"}, g.landHails())
		}, 30*time.Second, 20*time.Millisecond)
		g.waitHail(regs.CategoryCampaignLand)
		require.Equal(t, other, g.remoteAt(testBranch))
		require.Len(t, g.pushes(work.ID), 1, "the rejected push alone")
		require.Equal(t, store.TaskLanding, g.row().State)
	})
}

// A head whose done entry holds work on another repo too is refused
// before anything is pushed: push_failed, naming the repos.
func TestCampaignLandOtherRepos(t *testing.T) {
	t.Parallel()
	g := newCampaignRig(t, campaignOpts{})
	work := g.waitOrder("r1:work")
	g.claim(work.ID, "sorell")
	c1 := g.commit(work.ID, g.base)
	ctx := t.Context()
	_, err := g.d.store.AppendLog(ctx, store.LogEntry{Kind: logOrder, OrderID: work.ID,
		Text:    "repo app shipped to the beam at " + g.base[:12] + " (main)",
		Payload: mustJSON(t, map[string]string{"repo": "app", "base_ref": "main", "commit": g.base, "from": "main"})})
	require.NoError(t, err)
	p := donePayload{Summary: "summary", Repos: []landedRepo{{Name: "app", Branch: orderBranch(work.ID), Commit: c1},
		{Name: "lib", Branch: orderBranch(work.ID), Commit: c1}}}
	_, settled, err := g.d.store.SettleOrder(ctx, work.ID, store.OrderDone,
		store.LogEntry{Kind: logDone, CrewID: "sorell", OrderID: work.ID, Text: "done", Payload: mustJSON(t, p)}, nil)
	require.NoError(t, err)
	require.True(t, settled)
	g.reviewDone(g.waitOrder("r1:review").ID, "tuvok", c1, verdictOf())
	h, _ := g.landHail()
	g.answer(h.ID, "land")
	_, c := g.stallHail()
	require.Equal(t, stallPushFailed, c.Code)
	require.Contains(t, c.Reason, work.ID+" holds work on repos [app, lib]; a campaign lands work on repo app alone")
	require.Empty(t, g.remoteAt(testBranch))
	require.Empty(t, g.pushes(work.ID))
}
