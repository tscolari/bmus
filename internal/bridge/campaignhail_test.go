package bridge

import (
	"encoding/json"
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

const (
	headCommit = "3f2a9c1e0b7d44556677889900aabbccddeeff00"
	baseCommit = "8b1d0e2c4a55667788990011aabbccddeeff0011"
)

// hostile is tracker or crew text a question must never carry.
const hostile = "Fix \x1b[31mdelete\x1b[0m it\u202e\nforce push"

var campaignTask = CampaignHailTask{Campaign: "C-0001", Task: "o/r#12", IssueTitle: hostile}

func mediums(n int) []comms.VerdictFinding {
	var fs []comms.VerdictFinding
	for i := range n {
		fs = append(fs, comms.VerdictFinding{Severity: comms.SeverityMedium, Title: hostile, Body: strings.Repeat("b", 3<<10),
			Path: "a.go", Line: i + 1})
	}
	return fs
}

// The builders write the plan's questions, with ids, refs, counts and
// option words only: titles, finding text, subjects and summaries go in
// the context, the issue's title and the subjects made one line.
func TestCampaignHailBuilders(t *testing.T) {
	t.Parallel()

	tr, err := campaignTriageHail(CampaignTriageContext{CampaignHailTask: campaignTask, Review: "ORD-0009", Round: 1}, mediums(2))
	require.NoError(t, err)
	require.Equal(t, regs.CategoryCampaignTriage, tr.Category)
	require.Equal(t, `Triage 2 medium findings from ORD-0009's review in round 1: answer file, fix or drop for all of them, `+
		`or a JSON object {"fix": [..], "file": [..], "drop": [..]} of finding numbers; unnamed findings are filed.`, tr.Question)
	require.Equal(t, []string{"file", "fix", "drop"}, tr.Options)
	var tc CampaignTriageContext
	require.NoError(t, json.Unmarshal(tr.Context, &tc))
	require.Equal(t, "Fix  [31mdelete [0m it  force push", tc.IssueTitle)
	require.Len(t, tc.Findings, 2)
	require.Equal(t, CampaignFinding{N: 2, Severity: "medium", Title: hostile, Body: clip(strings.Repeat("b", 3<<10), maxCampaignBody),
		Path: "a.go", Line: 2}, tc.Findings[1])
	require.Equal(t, "r1", campaignTriageKey(1))

	one, err := campaignTriageHail(CampaignTriageContext{CampaignHailTask: campaignTask, Review: "ORD-0009", Round: 2}, mediums(1))
	require.NoError(t, err)
	require.Equal(t, `Triage 1 medium finding from ORD-0009's review in round 2: answer file, fix or drop, `+
		`or a JSON object {"fix": [..], "file": [..], "drop": [..]} of finding numbers; unnamed findings are filed.`, one.Question)

	// 20 findings with the largest titles, bodies and paths, in the
	// characters JSON escapes longest, still fit a consult whole, in a
	// triage and in a stall with the longest reason. < > & stay as they
	// are.
	var big []comms.VerdictFinding
	for range 20 {
		big = append(big, comms.VerdictFinding{Severity: "high", Title: strings.Repeat("\u2028", 200),
			Body: strings.Repeat("\x01", 8<<10), Path: strings.Repeat(`"`, 512), Line: 1})
	}
	full, err := campaignTriageHail(CampaignTriageContext{CampaignHailTask: campaignTask, Review: "ORD-0009", Round: 1}, big)
	require.NoError(t, err)
	require.LessOrEqual(t, len(full.Context), maxConsultContext)
	require.NoError(t, json.Unmarshal(full.Context, &tc))
	require.Len(t, tc.Findings, 20)
	fullStall, err := campaignStalledHail(CampaignStalledContext{CampaignHailTask: campaignTask, Code: "max_rounds",
		Reason: strings.Repeat(`"`, 4<<10), Round: 3, MaxRounds: 3, Head: "ORD-0010", HeadCommit: headCommit}, big)
	require.NoError(t, err)
	require.LessOrEqual(t, len(fullStall.Context), maxConsultContext)
	var fsc CampaignStalledContext
	require.NoError(t, json.Unmarshal(fullStall.Context, &fsc))
	require.Len(t, fsc.Blocking, 20)
	require.Len(t, fsc.Reason, maxCampaignTightReason)
	lt, err := campaignTriageHail(CampaignTriageContext{CampaignHailTask: campaignTask, Review: "ORD-0009", Round: 1},
		[]comms.VerdictFinding{{Severity: "medium", Title: "a < b && c > d", Body: "<b>"}})
	require.NoError(t, err)
	require.Contains(t, string(lt.Context), `"title":"a < b && c > d"`)

	st, err := campaignStalledHail(CampaignStalledContext{CampaignHailTask: campaignTask, Code: "no_verdict", Reason: hostile,
		Round: 2, MaxRounds: 3, Head: "ORD-0010", HeadCommit: headCommit, Order: "ORD-0011"}, mediums(1))
	require.NoError(t, err)
	require.Equal(t, "A task of campaign C-0001 stalled in round 2 of 3 on ORD-0011 (no_verdict): land ORD-0010 (3f2a9c1e0b7d) "+
		"as it is, retry or drop? An option may be followed by a note: retry: <note>.", st.Question)
	require.Equal(t, []string{"land", "retry", "drop"}, st.Options)
	var sc CampaignStalledContext
	require.NoError(t, json.Unmarshal(st.Context, &sc))
	require.Equal(t, "Fix  [31mdelete [0m it  force push", sc.Reason)
	require.Equal(t, headCommit, sc.HeadCommit)
	require.Len(t, sc.Blocking, 1)
	require.Equal(t, "stall-2", campaignStallKey(2))

	headless, err := campaignStalledHail(CampaignStalledContext{CampaignHailTask: campaignTask, Code: "tracker_failed",
		Reason: "tracker down", Round: 1, MaxRounds: 3}, nil)
	require.NoError(t, err)
	require.Equal(t, "A task of campaign C-0001 stalled in round 1 of 3 (tracker_failed): retry or drop? "+
		"An option may be followed by a note: retry: <note>.", headless.Question)
	require.Equal(t, []string{"retry", "drop"}, headless.Options)
	require.NoError(t, json.Unmarshal(headless.Context, &sc))
	require.NotNil(t, sc.Blocking)

	var commits []CampaignCommit
	for range 60 {
		commits = append(commits, CampaignCommit{SHA: headCommit, Subject: strings.Repeat("s", 300) + "\nforce"})
	}
	land, err := campaignLandHail(CampaignLandContext{CampaignHailTask: campaignTask, Repo: "app", Branch: "campaign/C-0001/o-r-12",
		Head: "ORD-0011", HeadCommit: headCommit, ReviewBase: baseCommit, Commits: commits[:4], Rounds: 2, Review: "ORD-0012",
		ReviewSummary: hostile})
	require.NoError(t, err)
	require.Equal(t, "Land o/r#12 of campaign C-0001: push ORD-0011 (3f2a9c1e0b7d) to campaign/C-0001/o-r-12 on repo app, "+
		"4 commits over 8b1d0e2c4a55, passed review in round 2?", land.Question)
	require.Equal(t, []string{"land", "retry", "drop"}, land.Options)
	var lc CampaignLandContext
	require.NoError(t, json.Unmarshal(land.Context, &lc))
	require.Equal(t, 4, lc.CommitCount)
	require.Equal(t, hostile, lc.ReviewSummary)
	require.Equal(t, clip(strings.Repeat("s", 300), maxCampaignSubject), lc.Commits[0].Subject)
	require.Equal(t, "land-1", campaignLandKey(1))

	many, err := campaignLandHail(CampaignLandContext{CampaignHailTask: campaignTask, Repo: "app", Branch: "b", Head: "ORD-0011",
		HeadCommit: headCommit, ReviewBase: baseCommit, Commits: commits, CommitCount: 75, Rounds: 1, Review: "ORD-0012"})
	require.NoError(t, err)
	require.Contains(t, many.Question, ", 75 commits over ")
	require.NoError(t, json.Unmarshal(many.Context, &lc))
	require.Len(t, lc.Commits, maxCampaignCommits)
	require.Equal(t, 75, lc.CommitCount)

	for _, h := range []store.Hail{tr, one, st, headless, land, many} {
		for _, bad := range []string{"\x1b", "\u202e", "delete", "force", "sss"} {
			require.NotContains(t, h.Question, bad)
		}
	}

	// Refusals.
	_, err = campaignTriageHail(CampaignTriageContext{CampaignHailTask: campaignTask, Review: "ORD-0009", Round: 1}, nil)
	require.ErrorContains(t, err, "needs a finding")
	_, err = campaignTriageHail(CampaignTriageContext{CampaignHailTask: campaignTask, Round: 1}, mediums(1))
	require.ErrorContains(t, err, "needs its review and round")
	_, err = campaignTriageHail(CampaignTriageContext{Review: "ORD-0009", Round: 1}, mediums(1))
	require.ErrorContains(t, err, "needs its campaign and task")
	_, err = campaignStalledHail(CampaignStalledContext{CampaignHailTask: campaignTask, Code: "No Verdict", Round: 1, MaxRounds: 3}, nil)
	require.ErrorContains(t, err, `stall code "No Verdict"`)
	_, err = campaignStalledHail(CampaignStalledContext{CampaignHailTask: campaignTask, Code: "x", Round: 1, MaxRounds: 3,
		Head: "ORD-0010", HeadCommit: "3f2a9c1e0b7d"}, nil)
	require.ErrorContains(t, err, "full commit id")
	_, err = campaignStalledHail(CampaignStalledContext{CampaignHailTask: campaignTask, Code: "x", Round: 1, MaxRounds: 3,
		Head: "ORD-0010"}, nil)
	require.ErrorContains(t, err, "full commit id")
	_, err = campaignLandHail(CampaignLandContext{CampaignHailTask: campaignTask, Repo: "app", Branch: "b", Head: "ORD-0011",
		HeadCommit: headCommit, ReviewBase: "8b1d0e2c4a55", Rounds: 1, Review: "ORD-0012"})
	require.ErrorContains(t, err, "full commit ids")
	_, err = campaignLandHail(CampaignLandContext{CampaignHailTask: campaignTask, Repo: "app", Head: "ORD-0011",
		HeadCommit: headCommit, ReviewBase: baseCommit, Rounds: 1, Review: "ORD-0012"})
	require.ErrorContains(t, err, "needs its repo, branch")
}

// Every triage form, its normal form and option, the normal form parsing
// back to the same decision, and the refusals.
func TestParseCampaignTriage(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		in, normal string
		option     int
		want       campaignTriage
	}{
		{"file", "file", 0, campaignTriage{Fix: []int{}, File: []int{1, 2, 3}, Drop: []int{}}},
		{" FIX ", "fix", 1, campaignTriage{Fix: []int{1, 2, 3}, File: []int{}, Drop: []int{}}},
		{"Drop", "drop", 2, campaignTriage{Fix: []int{}, File: []int{}, Drop: []int{1, 2, 3}}},
		{"{}", "file", 0, campaignTriage{Fix: []int{}, File: []int{1, 2, 3}, Drop: []int{}}},
		{`{"fix": [3, 1, 2]}`, "fix", 1, campaignTriage{Fix: []int{1, 2, 3}, File: []int{}, Drop: []int{}}},
		{`{"drop": [2], "fix": [], "file": [3]}`, `{"fix":[],"file":[1,3],"drop":[2]}`, 0,
			campaignTriage{Fix: []int{}, File: []int{1, 3}, Drop: []int{2}}},
		{`{"fix": [1]}`, `{"fix":[1],"file":[2,3],"drop":[]}`, 0, campaignTriage{Fix: []int{1}, File: []int{2, 3}, Drop: []int{}}},
		// Ties go to the option listed first: file, then fix, then drop.
		{`{"fix": [1], "drop": [2], "file": [3]}`, `{"fix":[1],"file":[3],"drop":[2]}`, 0,
			campaignTriage{Fix: []int{1}, File: []int{3}, Drop: []int{2}}},
		{`{"fix": [1, 2], "drop": [3]}`, `{"fix":[1,2],"file":[],"drop":[3]}`, 1,
			campaignTriage{Fix: []int{1, 2}, File: []int{}, Drop: []int{3}}},
		{`{"fix": [1], "drop": [3, 2]}`, `{"fix":[1],"file":[],"drop":[2,3]}`, 2,
			campaignTriage{Fix: []int{1}, File: []int{}, Drop: []int{2, 3}}},
		{"```json\n{\"drop\": [1]}\n```", `{"fix":[],"file":[2,3],"drop":[1]}`, 0,
			campaignTriage{Fix: []int{}, File: []int{2, 3}, Drop: []int{1}}},
	} {
		got, err := parseCampaignTriage(tc.in, 3)
		require.NoError(t, err, tc.in)
		require.Equal(t, tc.want, got, tc.in)
		require.Equal(t, tc.normal, got.normal(), tc.in)
		require.Equal(t, tc.option, got.option(), tc.in)
		again, err := parseCampaignTriage(got.normal(), 3)
		require.NoError(t, err, tc.in)
		require.Equal(t, got, again, tc.in)
	}
	for in, want := range map[string]string{
		"":                                "not file, fix, drop or a JSON object",
		"file 1":                          "not file, fix, drop or a JSON object",
		"keep":                            "not file, fix, drop or a JSON object",
		"[1]":                             "not file, fix, drop or a JSON object",
		`{"Fix": [1]}`:                    `"Fix" is not fix, file or drop`,
		`{"FIX": [1]}`:                    `"FIX" is not fix, file or drop`,
		`{"skip": [1]}`:                   `"skip" is not fix, file or drop`,
		`{"fix": [1], "fix": [2]}`:        "fix named twice",
		`{"fix": null}`:                   "fix is not a list of finding numbers",
		`{"fix": 1}`:                      "fix is not a list of finding numbers",
		`{"fix": {"1": 1}}`:               "fix is not a list of finding numbers",
		`{"fix": [0]}`:                    "fix: 0 is not a finding number from 1 to 3",
		`{"fix": [4]}`:                    "fix: 4 is not a finding number from 1 to 3",
		`{"fix": [-1]}`:                   "fix: -1 is not a finding number from 1 to 3",
		`{"fix": [1.0]}`:                  "fix: 1.0 is not a finding number from 1 to 3",
		`{"fix": [1e0]}`:                  "fix: 1e0 is not a finding number from 1 to 3",
		`{"fix": ["1"]}`:                  `fix: "1" is not a finding number from 1 to 3`,
		`{"fix": [99999999999999999999]}`: "fix: 99999999999999999999 is not a finding number from 1 to 3",
		`{"fix": [1, 1]}`:                 "1 named twice (fix and fix)",
		`{"fix": [1], "drop": [1]}`:       "1 named twice (fix and drop)",
		`{"fix": [1]} {}`:                 "more than one JSON value",
		`{"fix": [1]`:                     "EOF",
		`{"fix": [1],}`:                   "invalid character",
	} {
		_, err := parseCampaignTriage(in, 3)
		require.ErrorContains(t, err, want, in)
	}
}

// Stalled and land decisions: an option, then an optional one-line note.
func TestParseCampaignChoice(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]campaignChoice{
		"land":                                {Option: "land"},
		" RETRY ":                             {Option: "retry"},
		"retry:":                              {Option: "retry"},
		"retry: the flaky test is real":       {Option: "retry", Note: "the flaky test is real"},
		"Drop :  not worth it: too big  ":     {Option: "drop", Note: "not worth it: too big"},
		"retry: " + strings.Repeat("é", 1024): {Option: "retry", Note: strings.Repeat("é", 1024)},
	} {
		got, err := parseCampaignChoice(in, campaignLandOptions)
		require.NoError(t, err, in)
		require.Equal(t, want, got, in)
		again, err := parseCampaignChoice(got.normal(), campaignLandOptions)
		require.NoError(t, err, in)
		require.Equal(t, got, again, in)
	}
	require.Equal(t, "retry: x", campaignChoice{Option: "retry", Note: "x"}.normal())
	for in, want := range map[string]string{
		"":                                       `"" is not land, retry or drop`,
		"retry the flaky test":                   `"retry the flaky test" is not land, retry or drop`,
		"keep":                                   `"keep" is not land, retry or drop`,
		"retry: a\nb":                            `no control or format character ('\n' at byte 8)`,
		"retry: a\u2028b":                        `no control or format character ('\u2028' at byte 8)`,
		"retry: \x1b[31m":                        `no control or format character ('\x1b' at byte 7)`,
		"retry: a\u202eb":                        `no control or format character ('\u202e' at byte 8)`,
		"retry: a\u200bb":                        `no control or format character ('\u200b' at byte 8)`,
		"land\ufeff":                             `no control or format character ('\ufeff' at byte 4)`,
		"retry: \xff":                            "not valid UTF-8",
		"retry: " + strings.Repeat("x", 2<<10+1): "the note is 2049 bytes, over 2048",
	} {
		_, err := parseCampaignChoice(in, campaignLandOptions)
		require.ErrorContains(t, err, want, in)
	}
	_, err := parseCampaignChoice("land", campaignHeadlessOptions)
	require.ErrorContains(t, err, `"land" is not retry or drop`)
}

// checkCampaignAnswer reads an option alone as its text, checks an option
// given with a decision against it, and says what the hail takes.
func TestCheckCampaignAnswer(t *testing.T) {
	t.Parallel()
	tr, err := campaignTriageHail(CampaignTriageContext{CampaignHailTask: campaignTask, Review: "ORD-0009", Round: 1}, mediums(2))
	require.NoError(t, err)
	tr.ID, tr.DeliverTo = "H-0001", "campaign/C-0001/task/o/r#12"
	st, err := campaignStalledHail(CampaignStalledContext{CampaignHailTask: campaignTask, Code: "max_rounds", Round: 3, MaxRounds: 3,
		Head: "ORD-0010", HeadCommit: headCommit}, nil)
	require.NoError(t, err)
	st.ID, st.DeliverTo = "H-0002", tr.DeliverTo
	land, err := campaignLandHail(CampaignLandContext{CampaignHailTask: campaignTask, Repo: "app", Branch: "b", Head: "ORD-0011",
		HeadCommit: headCommit, ReviewBase: baseCommit, Rounds: 1, Review: "ORD-0012"})
	require.NoError(t, err)
	land.ID, land.DeliverTo = "H-0003", tr.DeliverTo
	require.True(t, isCampaignHail(tr) && isCampaignHail(st) && isCampaignHail(land))
	require.False(t, isCampaignHail(store.Hail{Category: regs.CategoryCampaignLand}), "a beam's hail")

	opt := func(i int) *int { return &i }
	for _, tc := range []struct {
		h        store.Hail
		decision string
		option   *int
		normal   string
		want     int
	}{
		{tr, "", opt(1), "fix", 1},
		{tr, "FIX", opt(1), "fix", 1},
		{tr, `{"drop": [1, 2]}`, nil, "drop", 2},
		{tr, `{"drop": [1]}`, opt(0), `{"fix":[],"file":[2],"drop":[1]}`, 0},
		{st, "", opt(2), "drop", 2},
		{st, "retry: one more round", opt(1), "retry: one more round", 1},
		{land, "land", nil, "land", 0},
	} {
		normal, got, err := checkCampaignAnswer(tc.h, tc.decision, tc.option)
		require.NoError(t, err, tc.decision)
		require.Equal(t, tc.normal, normal, tc.decision)
		require.Equal(t, tc.want, *got, tc.decision)
	}
	for _, tc := range []struct {
		h        store.Hail
		decision string
		option   *int
		want     string
	}{
		{tr, `{"fix": [3]}`, nil, `hail H-0001: fix: 3 is not a finding number from 1 to 2; answer file, fix or drop, ` +
			`or a JSON object {"fix": [..], "file": [..], "drop": [..]} of finding numbers from 1 to 2, each named once`},
		{tr, "fix", opt(2), `hail H-0001: option 2 (drop) does not match the decision "fix"`},
		{st, "keep", nil, `hail H-0002: "keep" is not land, retry or drop; answer land, retry or drop, ` +
			`optionally followed by a one-line note (retry: <note>)`},
		{land, "retry: x", opt(0), `hail H-0003: option 0 (land) does not match the decision "retry: x"`},
		{land, "", opt(3), `hail H-0003: option 3 out of range (3 options)`},
		{land, "land", opt(-1), `hail H-0003: option -1 out of range (3 options)`},
		{tr, "", opt(9), `hail H-0001: option 9 out of range (3 options)`},
	} {
		_, _, err := checkCampaignAnswer(tc.h, tc.decision, tc.option)
		var re *rpcError
		require.ErrorAs(t, err, &re, tc.decision)
		require.Equal(t, CodeBadRequest, re.code)
		require.EqualError(t, err, tc.want)
	}
}

// Classify: campaign_land is the captain's under any regs.yaml, with its
// reminders and no timeout; the default regs.yaml gives campaign_stalled
// to the captain as captain_only[8] and campaign_triage to Command, with
// a countermand window.
func TestClassifyCampaignHails(t *testing.T) {
	t.Parallel()
	zero := 0
	esc := regs.Escalation{CaptainReminderEvery: 10 * time.Minute, CaptainTimeout: time.Hour,
		OnTimeout: regs.OnTimeoutDefaultOption, DefaultOption: &zero}
	def := defaultRegs(t)
	def.Escalation = esc
	permissive := loadRegsText(t, "classify:\n  - {tag: any, match: '.'}\nauto_allow:\n  - tag: any\n  - match: '.*'\n"+
		"command_may_answer:\n  min_confidence: 0\n")
	permissive.Escalation = esc
	land := store.Hail{Category: regs.CategoryCampaignLand, Question: "Land o/r#12?", Options: campaignLandOptions}
	stalled := store.Hail{Category: regs.CategoryCampaignStalled, Question: "o/r#12 stalled", Options: campaignLandOptions}
	triage := store.Hail{Category: regs.CategoryCampaignTriage, Question: "Triage 2 medium findings", Options: campaignTriageOptions}
	fixed := "category campaign_land is fixed to the captain"
	for _, tc := range []struct {
		name    string
		cfg     *regs.Config
		hail    store.Hail
		route   string
		why     string
		timeout time.Duration
	}{
		{"land default", def, land, regs.RouteCaptain, fixed, 0},
		{"land permissive", permissive, land, regs.RouteCaptain, fixed, 0},
		{"land unconfigured", nil, land, regs.RouteCaptain, "regs.yaml not configured", 0},
		{"stalled default", def, stalled, regs.RouteCaptain, `captain_only[8]: category "campaign_stalled"`, time.Hour},
		{"stalled permissive", permissive, stalled, regs.RouteCommand, "", time.Hour},
		{"triage default", def, triage, regs.RouteCommand, "", time.Hour},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d, id := bridgeHailStore(t, tc.cfg, tc.hail)
			r, err := d.classifyHail(t.Context(), id)
			require.NoError(t, err)
			require.Equal(t, tc.route, r.Route)
			require.Equal(t, tc.why, r.Why)
			require.Equal(t, tc.timeout, r.Timeout)
			require.False(t, r.Advisory)
			if tc.cfg != nil {
				require.Equal(t, 10*time.Minute, r.RemindEvery, "reminders kept")
				require.Equal(t, regs.DefaultCountermandWindow, r.CountermandWindow)
			}
		})
	}
}

// A task's ref is free text a captain_only rule may fit: the triage and
// stalled questions leave it out, so Command still answers them and its
// answer passes regs.CheckAnswer. The land question names it, and the
// category is the captain's anyway.
func TestCampaignQuestionsLeaveOutRef(t *testing.T) {
	t.Parallel()
	cfg := loadRegsText(t, strings.Replace(string(regs.Default), "  - category: campaign_stalled\n", "", 1))
	for _, ref := range []string{"acme/salesforce#12", "acme/git-push-hooks#3", "acme/deploy-destroy#1"} {
		task := CampaignHailTask{Campaign: "C-0001", Task: ref, IssueTitle: "git push --force to deploy"}
		tr, err := campaignTriageHail(CampaignTriageContext{CampaignHailTask: task, Review: "ORD-0009", Round: 1}, mediums(2))
		require.NoError(t, err)
		st, err := campaignStalledHail(CampaignStalledContext{CampaignHailTask: task, Code: "no_verdict", Reason: "git push --force",
			Round: 1, MaxRounds: 3, Head: "ORD-0010", HeadCommit: headCommit, Order: "ORD-0011"}, nil)
		require.NoError(t, err)
		for _, h := range []store.Hail{tr, st} {
			require.NotContains(t, h.Question, ref)
			rh := regsHail(h, store.Order{})
			route, _, rule := cfg.Classify(rh)
			require.Equal(t, regs.RouteCommand, route, "%s %s: %s", ref, h.Category, rule)
			one := 1
			ok, why := cfg.CheckAnswer(rh, regs.Reply{Decision: regs.DecisionAnswer, Option: &one, Confidence: 1})
			require.True(t, ok, "%s %s: %s", ref, h.Category, why)
		}
		land, err := campaignLandHail(CampaignLandContext{CampaignHailTask: task, Repo: "app", Branch: "campaign/C-0001/x",
			Head: "ORD-0011", HeadCommit: headCommit, ReviewBase: baseCommit, Rounds: 1, Review: "ORD-0012"})
		require.NoError(t, err)
		require.Contains(t, land.Question, ref)
		route, _, rule := cfg.Classify(regsHail(land, store.Order{}))
		require.Equal(t, [2]string{regs.RouteCaptain, "category campaign_land is fixed to the captain"}, [2]string{route, rule})
	}
}

// campaignRaiser stands in for a task workflow (#184): it raises each of
// ins in turn under its key and waits for each decision to be final
// (raiseCampaignHail).
func campaignRaiser(ctx dbos.Context, ins []raiserInput) ([]store.Hail, error) {
	var out []store.Hail
	for _, in := range ins {
		h, err := raiseCampaignHail(ctx, in.Key, store.Hail{Category: in.Category, Question: in.Question, Options: in.Options,
			Context: in.Context}, store.LogEntry{Kind: logHail, Text: "raised " + in.Category})
		if err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, nil
}

// raiseCampaign starts campaignRaiser as raiser/<name> with ins.
func raiseCampaign(t *testing.T, d *Daemon, name string, ins ...raiserInput) dbos.WorkflowHandle[[]store.Hail] {
	t.Helper()
	h, err := dbos.RunWorkflow(d.dbos, campaignRaiser, ins, dbos.WithWorkflowID("raiser/"+name))
	require.NoError(t, err)
	return h
}

// raiserInputOf is h, raised under key.
func raiserInputOf(h store.Hail, key string) raiserInput {
	return raiserInput{Category: h.Category, Question: h.Question, Options: h.Options, Context: h.Context, Key: key}
}

// waitOpenHail waits for hail category's open hail and returns its id.
func waitOpenHail(t *testing.T, hd *hdaemon, category string) string {
	t.Helper()
	var id string
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		hails, err := hd.d.store.ListHails(t.Context(), store.HailOpen)
		require.NoError(c, err)
		for _, h := range hails {
			if h.Category == category {
				id = h.ID
			}
		}
		require.NotEmpty(c, id)
	}, 30*time.Second, 20*time.Millisecond)
	return id
}

// Command's triage reply that doesn't parse goes to the captain, whose
// JSON answer is stored in normal form; a second triage under another key
// is a second hail, the captain's option alone decides it, and neither
// waits for a window.
func TestCampaignTriageRefusedToCaptain(t *testing.T) {
	t.Parallel()
	b := newHolodeckBridge(t, commandScript(t,
		map[string]any{"decision": "answer", "answer": `{"fix": [3]}`, "rationale": "fix the third", "confidence": 0.9}))
	b.registerExtra = registerRaiser
	b.regs = defaultRegs(t)
	b.consultTimeout = time.Minute
	hd := b.start(t)
	hd.addCommand(t)

	tr, err := campaignTriageHail(CampaignTriageContext{CampaignHailTask: campaignTask, Review: "ORD-0009", Round: 1}, mediums(2))
	require.NoError(t, err)
	run := raiseCampaign(t, hd.d, "triage", raiserInputOf(tr, campaignTriageKey(1)), raiserInputOf(tr, campaignTriageKey(2)))

	id := waitOpenHail(t, hd, regs.CategoryCampaignTriage)
	e := hd.waitLog(t, "hail "+id+" for the captain")
	require.Equal(t, "hail "+id+" for the captain: Command's answer does not fit the hail: "+
		"hail "+id+": fix: 3 is not a finding number from 1 to 2; answer file, fix or drop, or a JSON object "+
		`{"fix": [..], "file": [..], "drop": [..]} of finding numbers from 1 to 2, each named once`, e.Text)
	require.Equal(t, store.HailOpen, hd.hail(t, id).Status)

	require.ErrorContains(t, hd.d.AnswerHail(t.Context(), id, `{"fix": [1], "fix": [2]}`, nil), "fix named twice")
	require.NoError(t, hd.d.AnswerHail(t.Context(), id, `{"drop": [2]}`, nil))

	second := waitOpenHail(t, hd, regs.CategoryCampaignTriage)
	require.NotEqual(t, id, second)
	one := 1
	hd.waitLog(t, "hail "+second+" for the captain")
	require.NoError(t, hd.d.AnswerHail(t.Context(), second, "", &one))

	hails, err := run.GetResult(dbos.WithHandleTimeout(60 * time.Second))
	require.NoError(t, err)
	require.Len(t, hails, 2)
	require.Equal(t, `{"fix":[],"file":[1],"drop":[2]}`, hails[0].Decision)
	require.Equal(t, "r1", hails[0].RaiseKey)
	require.Equal(t, comms.DecidedByCaptain, hails[0].DecidedBy)
	require.Nil(t, hails[0].CountermandUntil)
	require.Equal(t, "fix", hails[1].Decision)
	require.Equal(t, "r2", hails[1].RaiseKey)
	require.NotContains(t, stepNames(t, hd.d, "raiser/triage"), "campaign-window")
	require.Len(t, consults(t, b.stepLog), 2)
	noMissions(t, hd.d)
}

// Command's triage decision is stored in normal form with a countermand
// window. The raiser waits out the whole window, longer than the tour's
// settle wait that caps a mission's, then re-reads the row and gets the
// captain's countermand, itself checked and normalised.
func TestCampaignTriageCountermandWindow(t *testing.T) {
	t.Parallel()
	const window = 4 * time.Second
	b := newHolodeckBridge(t, commandScript(t,
		map[string]any{"decision": "answer", "answer": `{"fix": [1]}`, "rationale": "fix the first", "confidence": 0.9}))
	b.registerExtra = registerRaiser
	b.regs = loadRegsText(t, strings.Replace(string(regs.Default), "countermand_window: 5m", "countermand_window: 4s", 1))
	b.consultTimeout = time.Minute
	b.settle = 200 * time.Millisecond
	hd := b.start(t)
	hd.addCommand(t)

	tr, err := campaignTriageHail(CampaignTriageContext{CampaignHailTask: campaignTask, Review: "ORD-0009", Round: 1}, mediums(2))
	require.NoError(t, err)
	run := raiseCampaign(t, hd.d, "window", raiserInputOf(tr, campaignTriageKey(1)))

	var h store.Hail
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		hails, err := hd.d.store.ListHails(t.Context(), store.HailDecided)
		require.NoError(c, err)
		require.Len(c, hails, 1)
		h = hails[0]
	}, 30*time.Second, 20*time.Millisecond)
	require.Equal(t, comms.DecidedByCommand, h.DecidedBy)
	require.Equal(t, `{"fix":[1],"file":[2],"drop":[]}`, h.Decision)
	require.NotNil(t, h.CountermandUntil)
	e := hd.waitLog(t, "hail "+h.ID+" decided by Command")
	require.Equal(t, "hail "+h.ID+" decided by Command: "+`{"fix":[1],"file":[2],"drop":[]}`, e.Text)
	requirePending(t, hd.d, "raiser/window")

	_, err = hd.d.CountermandHail(t.Context(), h.ID, `{"fix": [5]}`, nil, "")
	require.ErrorContains(t, err, "5 is not a finding number from 1 to 2")
	_, err = hd.d.CountermandHail(t.Context(), h.ID, `{"fix": [2], "drop": [1]}`, nil, "both matter")
	require.NoError(t, err)

	hails, err := run.GetResult(dbos.WithHandleTimeout(60 * time.Second))
	require.NoError(t, err)
	done := time.Now()
	require.Len(t, hails, 1)
	got := hails[0]
	require.False(t, done.Before(*h.CountermandUntil), "the raiser waited out the whole window")
	require.Greater(t, window, b.settle)
	require.Equal(t, comms.DecidedByCaptain, got.DecidedBy)
	require.Equal(t, `{"fix":[2],"file":[],"drop":[1]}`, got.Decision)
	require.NotNil(t, got.CountermandedAt)
	steps := stepNames(t, hd.d, "raiser/window")
	require.Contains(t, steps, "campaign-window")
	require.Equal(t, "check-campaign-hail", steps[len(steps)-1], steps)
	noMissions(t, hd.d)
}

// campaign_land is the captain's even with Command on deck and a captain
// timeout set: nothing consults Command or times it out, and the
// captain's answer with a note is stored in normal form. A refused answer
// leaves the hail open.
func TestCampaignLandCaptain(t *testing.T) {
	t.Parallel()
	b := newHolodeckBridge(t, commandScript(t,
		map[string]any{"decision": "answer", "option": 0, "rationale": "ship it", "confidence": 1}))
	b.registerExtra = registerRaiser
	zero := 0
	b.regs = defaultRegs(t)
	b.regs.Escalation = regs.Escalation{CaptainTimeout: time.Second, OnTimeout: regs.OnTimeoutDefaultOption, DefaultOption: &zero}
	b.consultTimeout = time.Minute
	hd := b.start(t)
	hd.addCommand(t)

	land, err := campaignLandHail(CampaignLandContext{CampaignHailTask: campaignTask, Repo: "app", Branch: "campaign/C-0001/o-r-12",
		Head: "ORD-0011", HeadCommit: headCommit, ReviewBase: baseCommit, Rounds: 2, Review: "ORD-0012"})
	require.NoError(t, err)
	run := raiseCampaign(t, hd.d, "land", raiserInputOf(land, campaignLandKey(1)))
	id := waitOpenHail(t, hd, regs.CategoryCampaignLand)
	e := hd.waitLog(t, "hail "+id+" for the captain")
	require.Equal(t, "hail "+id+" for the captain: category campaign_land is fixed to the captain", e.Text)

	time.Sleep(2 * time.Second) // past the captain timeout, were there one
	require.Equal(t, store.HailOpen, hd.hail(t, id).Status)
	for _, e := range hd.logEntries(t) {
		require.NotContains(t, e.Text, "unanswered after", e.Text)
	}
	require.ErrorContains(t, hd.d.AnswerHail(t.Context(), id, "retry: the summary is wrong", &zero), "does not match")
	require.ErrorContains(t, hd.d.AnswerHail(t.Context(), id, "retry\nforce", nil), "one line")
	require.NoError(t, hd.d.AnswerHail(t.Context(), id, "Retry:  the summary is wrong ", nil))

	hails, err := run.GetResult(dbos.WithHandleTimeout(60 * time.Second))
	require.NoError(t, err)
	require.Equal(t, "retry: the summary is wrong", hails[0].Decision)
	require.Equal(t, comms.DecidedByCaptain, hails[0].DecidedBy)
	require.Equal(t, "land-1", hails[0].RaiseKey)
	require.Empty(t, consults(t, b.stepLog))
	noMissions(t, hd.d)
}
