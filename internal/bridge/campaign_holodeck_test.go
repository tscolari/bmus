package bridge

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dbos-inc/dbos-transact-golang/dbos"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tscolari/bmus/internal/comms"
	"github.com/tscolari/bmus/internal/regs"
	"github.com/tscolari/bmus/internal/store"
	"github.com/tscolari/bmus/internal/tracker"
	"github.com/tscolari/bmus/internal/tracker/trackertest"
)

// Plan Verification 7's triage, on holodeck: every order commits and
// reports verdicts by round, three mediums and a low in rounds 1 and 2,
// and Command triages each review's mediums as fix 2, drop 3, file the
// rest, under an 8s countermand window. Round 1's decision is acted on
// only after the window: the fix order carries the second medium alone.
// The captain countermands round 2's to file inside its window, and the
// task acts on the countermand: nothing blocks, and the task waits to
// land. The filed mediums and the lows are written as findings.
func TestCampaignTriageByCommand(t *testing.T) {
	t.Parallel()
	const window = 8 * time.Second
	medium := func(title string) map[string]any {
		return map[string]any{"severity": "medium", "title": title, "body": title + " body", "path": "x.go", "line": 3}
	}
	v := map[string]any{"findings": []any{medium("First"), medium("Second"), medium("Third"),
		map[string]any{"severity": "low", "title": "Later", "body": "follow up"}}}
	steps, err := json.Marshal([]map[string]any{
		{"at_pct": 40, "commit": "the work"},
		{"at_pct": 100, "report": map[string]any{"summary": "all done", "verdicts": []any{v, v}}},
	})
	require.NoError(t, err)
	stepsFile := filepath.Join(t.TempDir(), "steps.json")
	require.NoError(t, os.WriteFile(stepsFile, steps, 0o600))
	data, err := json.Marshal(map[string]any{"steps": json.RawMessage(steps), "consult": map[string]any{"decision": "answer",
		"answer": `{"fix": [2], "drop": [3]}`, "rationale": "the second matters", "confidence": 0.9}})
	require.NoError(t, err)
	script := filepath.Join(t.TempDir(), "campaign.json")
	require.NoError(t, os.WriteFile(script, data, 0o600))

	b := newHolodeckBridge(t, script)
	b.regs = loadRegsText(t, strings.Replace(string(regs.Default), "countermand_window: 5m", "countermand_window: 8s", 1))
	b.consultTimeout = time.Minute
	b.divisions = map[string]DivisionConfig{"eng": {}, "science": {}, DivisionCommand: {}}
	fake := &trackertest.Fake{}
	ref := fake.Add(tracker.Item{Title: "Fix the flake", Body: "It flakes."})
	b.tracker = fake
	hd := b.start(t)
	ctx := t.Context()
	require.NoError(t, hd.d.SetStandingOrders(ctx, true))
	hd.addCrewWith(t, store.Crew{ID: "sorell", Name: "Sorell", Division: "eng"})
	hd.addCrewWith(t, store.Crew{ID: "tuvok", Name: "Tuvok", Division: "science"})
	hd.addCommand(t)

	db := campaignDB(t, b.home)
	_, err = db.ExecContext(ctx, `INSERT INTO campaigns (id, repo, base_ref, division, assignee, review_division, created_at)
		VALUES (?, 'app', 'main', 'eng', 'sorell', 'science', ?)`, testCampaign, time.Now().UTC())
	require.NoError(t, err)
	task := store.TaskWorkflowID(testCampaign, ref)
	_, err = db.ExecContext(ctx, `INSERT INTO campaign_tasks (campaign_id, ref, workflow_id, max_rounds, branch) VALUES (?, ?, ?, 3, ?)`,
		testCampaign, ref, task, "campaign/C-0001/1")
	require.NoError(t, err)
	require.NoError(t, hd.d.startCampaign(testCampaign))

	triage := func(round int) store.Hail {
		var h store.Hail
		require.EventuallyWithT(t, func(c *assert.CollectT) {
			hails, err := hd.d.store.ListHails(ctx, store.HailDecided)
			require.NoError(c, err)
			h = store.Hail{}
			for _, x := range hails {
				if x.Category == regs.CategoryCampaignTriage && x.RaiseKey == campaignTriageKey(round) {
					h = x
				}
			}
			require.NotEmpty(c, h.ID)
		}, 120*time.Second, 50*time.Millisecond)
		require.Equal(t, comms.DecidedByCommand, h.DecidedBy)
		require.Equal(t, `{"fix":[2],"file":[1],"drop":[3]}`, h.Decision)
		require.NotNil(t, h.CountermandUntil)
		require.InDelta(t, window.Seconds(), h.CountermandUntil.Sub(*h.DecidedAt).Seconds(), 1)
		return h
	}
	byKey := func(key string) store.Order {
		var found store.Order
		require.EventuallyWithT(t, func(c *assert.CollectT) {
			os, err := hd.d.store.ListOrders(ctx)
			require.NoError(c, err)
			found = store.Order{}
			for _, o := range os {
				if o.CreatedBy == "campaign:"+testCampaign+":"+ref+":"+key {
					found = o
				}
			}
			require.NotEmpty(c, found.ID)
		}, 120*time.Second, 50*time.Millisecond)
		return found
	}

	h1 := triage(1)
	fix := byKey("r2:fix")
	created := hd.waitLog(t, "order created: Fix "+ref)
	require.False(t, created.TS.Before(*h1.CountermandUntil), "round 1's triage acted on after its window")
	require.Contains(t, fix.Body, "1. medium: Second")
	require.NotContains(t, fix.Body, "First")
	require.NotContains(t, fix.Body, "Third")

	h2 := triage(2)
	_, err = hd.d.CountermandHail(ctx, h2.ID, "file", nil, "file them all")
	require.NoError(t, err)

	wh, err := dbos.RetrieveWorkflow[TaskResult](hd.d.dbos, task)
	require.NoError(t, err)
	res, err := wh.GetResult(dbos.WithHandleTimeout(120 * time.Second))
	require.NoError(t, err)
	require.Equal(t, store.TaskLanding, res.State)
	require.Equal(t, "file", hd.hail(t, h2.ID).Decision)

	review1, review2 := byKey("r1:review"), byKey("r2:review")
	keys := func(id string) []string {
		fs, err := hd.d.store.OrderFindings(ctx, id)
		require.NoError(t, err)
		var out []string
		for _, f := range fs {
			out = append(out, f.Key)
		}
		return out
	}
	require.Equal(t, []string{"verdict.1", "verdict.4"}, keys(review1.ID))
	require.Equal(t, []string{"verdict.1", "verdict.2", "verdict.3", "verdict.4"}, keys(review2.ID))
	var texts []string
	for _, e := range hd.logEntries(t) {
		texts = append(texts, e.Text)
	}
	require.Contains(t, texts, ref+" round 1: review "+review1.ID+" blocked (0 high, 1 medium); filed 2, dropped 1")
	require.Contains(t, texts, ref+" round 2: review "+review2.ID+" passed; filed 4, dropped 0")
	require.Contains(t, texts, review1.ID+" committed on the head it reviewed; its commits are ignored")
}
