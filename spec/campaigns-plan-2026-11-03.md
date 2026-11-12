# BMUS Campaigns: a backlog-driven work loop

*3 November 2026. Where bmus should converge. Each stage in §6 becomes a phase from Phase 6 on, with one epic, when picked up (AGENTS.md); the first stage's ticket extends the `phase:N` label range.*

## Context

This document gives the captain's manual backlog loop a shape on the ship, with the bridge as the only holder of credentials (D6, `spec/initial-plan-2026-10-16.md`). The loop is called a campaign, driven with `bmus campaign`.

## 1. Where bmus stands

Today only Command creates orders from inside the ship, through captain-approved `split_order` children (#155). `ReportBody` has `Summary`, `Repos`, `Artifacts` and `Usage`, and no verdict. Finished work lands in the bridge's clone as `bmus/<order>`, and `bmus order push` is a CLI action outside any workflow (phase-2 plan §10).

## 2. The loop

1. A brainstorm order files findings as it works.
2. At settle the bridge files them as issues at once, and a non-blocking hail lets the captain discard any.
3. The captain starts a campaign over a backlog query, with an in-flight limit and a landing mode.
4. Each pass asks Command to pick ready issues for the free slots, and each pick becomes an implementation order with `source: tracker:<ref>`.
5. Someone who hasn't worked on the task reviews it and returns a verdict.
6. Blocking findings start a fix round on the latest work, until a pass or the round cap.
7. A pass lands the task: a push the captain allows, a pull request, or a merge on green CI.
8. Other findings go through step 2, and issues inside the query come back at step 4.

## 3. Who drives it

I'd put the loop on the bridge, in a durable DBOS workflow, `campaign/<id>`, that holds the state machine and performs every effect. Command answers the judgement calls: picks, triage, medium findings, breakdowns.

A long-running Command mission could hold the loop instead. Every order it created would be a split, and a split always reaches the captain: hails from Command go to the captain (D7, #155 "Routing"), and nothing could be delegated.

Consults suit these calls: read-only, bounded, and recorded by the hail workflow (#150) for a replay to reuse. The campaign raises them as hails (§4.7).

## 4. Building blocks

### 4.1 Tracker (#54)

The `Tracker` in #54 gains `List(query)` over labels, milestone, parent and state, returning each item's ref, title, body, labels and `BlockedBy` refs (nil without dependency data). `Create` takes the same fields. #54's body will record `List`, `BlockedBy` and the `finding` message (§4.6).

The first adapter is GitHub Issues. #54 keeps adapters out of the core packages; I'd put it in `internal/adapters/github`, registered by config and using the bridge's own credentials (D6), beside the forge adapter of §4.9.

### 4.2 Order dependencies

Orders gain `after`: orders that must end done before this one can be claimed. A new `order_after` table holds them, and both claim queries skip an order with an `after` not yet done. An `after` naming an order that is already failed, belayed or withdrawn is refused at insert. One that ends that way later withdraws its unclaimed dependents in the same transaction, like #146's cascade.

`split_order` children gain `after: [index]`.

### 4.3 Starting from another order's work

Today an order can name `bmus/ORD-0001` as its base (`bundleRepoFrom`), and claimed early it starts from ORD-0001's base commit or latest checkpoint. A repo's `base_ref` will accept `order:<id>`, which implies an `after`. `ship-repos` resolves it to the commit in that order's done entry, and fails with `repo_failed` unless the order is done and its fetch succeeded. The new order still gets its own `bmus/<order>`.

### 4.4 Review orders and verdicts

Orders gain a `kind` (`work` or `review`) and a `task` reference. A review order starts from `order:<head>` with `review_base`, the task's starting commit, and its prompt asks for a diff of `review_base..HEAD`, no commits, and a verdict through `report`:

```json
"verdict": {"findings": [{"severity": "high", "title": "...", "body": "...", "path": "...", "line": 12}]}
```

A review without a valid verdict fails with `no_verdict`. The campaign derives the result: any `high` finding blocks, `low` ones never do, and each `medium` goes to Command, who chooses between fixing it now, filing it or dropping it. No blocking finding is a pass.

Reviews go to the campaign's `review_division`, `science` by default. An eligible reviewer is on deck with standing orders on and has been assignee of no order in the task. When nobody is eligible, the task stalls (§4.5). A different crew member is independent enough.

### 4.5 Fix rounds and stalls

A task's head is its latest order. A blocking verdict starts a fix order from `order:<head>`, directed to the implementer when on deck and pooled otherwise, carrying the issue, the blocking findings and the last report's summary. Its work becomes the new head.

Each order keeps its own branch: fetch-work forces a report onto `bmus/<order>`, and `PushOrder` pins one done entry's commit. The campaign checks that each new head descends from the last and pushes only the final one, and the remote branch holds the rounds as stacked commits.

The task stalls at the campaign's `max_rounds` (3 by default), on any of its orders ending failed, belayed or withdrawn, on a missing verdict or a rewritten head, or with no eligible reviewer. A stalled task frees its slot and raises `campaign_stalled`. Its issue stays out of the ready set while the hail waits. The captain's options are another round (it waits for a free slot), landing as it is, and dropping the task.

### 4.6 Findings from any crew member

Every role gets `file_finding({key, title, body, labels?, severity?, blocked_by?})`, where `blocked_by` names tracker refs or keys of the same order's findings. The tool holds nothing: the beam sends a new `finding` message and the bridge stores it.

Findings are batched at settle only. The mission starts `findings/<order>`, or the settle callback (§4.8) does for an order settled outside its mission, such as a split parent or a cascade-belayed child. It makes one consult per batch, carrying the findings and the open backlog's refs, titles, labels and blockers within the 32 KiB context cap. Command's triage is advisory: it dedupes, links blockers and drops obvious noise. The workflow then creates the issues at once, blockers first, one step per finding, with each ref stored on its row; a replay skips findings that have one. Each issue body carries a hidden marker, `<!-- bmus finding <order>/<key> -->`, and a step that crashed after `Create` finds its issue by listing `bmus:unreviewed` before creating again.

New issues carry the label `bmus:unreviewed`. The workflow then raises one `tracker_review` hail per batch, with options `keep` (the default) and `discard`, or the numbers to discard; it also lists the findings Command dropped, for the captain to restore. Nothing waits on it, and a campaign may pick the issues meanwhile. `keep` removes the label. `discard` closes those issues as not planned through `Tracker.Close`. A task that already picked one is dropped, its open order belayed and its open pull request closed, and its stacked dependents stall; after a merge, discard only closes the issue. An unanswered hail changes nothing. `tracker_review` is fixed to the captain in routing (discarding closes issues, and `tracker_close` is captain-only). Unlike `campaign_land`, it lets `on_timeout: default_option` decide `keep`.

### 4.7 Hails the bridge raises

Bridge-raised hails need `deliver_to`, a workflow id used in place of the mission, and nullable `order_id` and `crew_id`. `schema.sql` changes and `requiredColumns`-style checks refuse an older database, the store's precedent for disposable pre-release databases. `classify`, the consult and countermand each need an orderless path. The bridge builds these hails' questions itself, like `splitQuestion`. Issue titles go in the context and refs in the answer, out of reach of option rules such as `delete|force`, which match the question and the answer.

The new categories are `tracker_review`, `campaign_pick`, `campaign_triage`, `campaign_stalled` and `campaign_land`. `tracker_create` is no longer a hail. `CheckAnswer` gains per-category checks: a pick names only offered ready issues, and no more than the free slots.

The campaign acts on a countermandable decision, Command's or a `default_option` timeout's, only once its window (5 minutes by default) has passed, to avoid undoing anything. It sleeps durably past `countermand_until`, then re-reads the hail in a step. With a window of 0 it acts at once. The wait covers campaign hails; findings triage is advisory, and §4.6 creates issues without it.

### 4.8 Intake and the in-flight limit

A campaign runs at most `max_in_flight` tasks, 2 by default, each one order at a time. A `campaign_tasks` table holds each task's issue ref, state, round and head, with a unique index over open tasks' refs. The ready set leaves out open tasks, tasks the campaign dropped, and tasks it landed whose issue is still open.

Stacked tasks are allowed. A blocked issue is ready once every blocker but at most one has closed and that one's task has landed. Its first order starts from that blocker's `order:<head>`, also its `review_base`, and its pull request first targets the blocker's branch. When the blocker merges, the bridge retargets the dependent to the target branch, and `merge` mode then waits for checks newer than the retarget. A blocker whose pull request closes unmerged stalls its dependents, and a conflict stalls the dependent.

Each task is a child workflow, `campaign/<id>/task/<ref>`, reached on order ends through a settle callback like `BelayFunc`, sent only to a live workflow (`workflowLive`). Campaign orders carry a `created_by` key per task, round and kind, and a replay finds them like `ChildBatch` finds a split's children (#155).

The campaign wakes on a task end, on created issues and every 15 minutes. Every 100 passes it continues as `campaign/<id>/gen/<n>`, bounding history like a tour (initial plan §4). `bmus campaign belay <id>` stops intake, belays each task's open order and drops the task, with no stall hails.

### 4.9 Landing

Each campaign sets `land: push | pr | merge`. Every mode pushes through a campaign step running `PushOrder(head, campaign/<id>/<ref>)`, a change to phase-2 §10's CLI-only push. A non-forced push of a pinned commit is safe to repeat on replay.

- `push`: the push waits for the captain to allow a `campaign_land` hail showing the head, its commits over `review_base` and the review's summary. The task holds its slot meanwhile: its work is done but unlanded.
- `pr`: the bridge opens a pull request to the target branch, its body carrying `Closes <ref>`, for a human to review and merge. The task lands when the pull request opens, and the campaign tracks it until the merge closes the issue.
- `merge`: the bridge also opens the pull request, and merges it with a merge commit once CI on the head is green, never squashing, rebasing or rewriting a head. It merges only a pull request that targets the target branch; a stacked dependent waits for its retarget. Checks cover the head alone, and the target branch's CI is the first to test the merge result; requiring up-to-date heads would stall every dependent after its retarget.

Starting a campaign with `pr` or `merge` is the captain's consent for all its tasks, with no per-task hail. In `merge` mode, red or missing CI after `ci_timeout`, a conflict, or a merge the forge refuses (branch protection, required approvals) stalls the task and frees its slot. The captain then gets a `campaign_land` hail in place of `campaign_stalled`, with three options: a fix round with the failed checks as blocking findings, counted in `max_rounds`; merging anyway; or dropping the task. `campaign_land` is flagged captain-only in routing, whatever `regs.yaml` says, and `on_timeout: default_option` can never decide it.

A `Forge` interface beside `Tracker` opens, retargets and merges pull requests and reads a commit's checks. It looks up an open pull request for the head branch before opening one, and its state before merging.

## 5. Captain control points

| Step | Hail category | Default | Can `regs.yaml` hand it to Command? |
|---|---|---|---|
| Reviewing issues filed from findings | `tracker_review`, never blocking | captain | No, fixed in routing |
| Closing an issue | `tracker_close` | captain | Yes |
| Picking ready issues | `campaign_pick` | Command | Already; `captain_only` takes it back |
| Medium findings, breakdowns | `campaign_triage` | Command | Already; `captain_only` takes it back |
| Stalled task | `campaign_stalled` | captain | Yes |
| Landing with `pr` or `merge` | none; starting the campaign is the consent | captain | No |
| Landing with `push`, or a failed `merge` | `campaign_land` | captain | No, fixed in routing |
| Starting or belaying a campaign | none (CLI) | captain | No |

## 6. Stages

The stages build on each other, and each leaves something usable.

### Stage 1: tracker and handoff

The tracker with `List` and `BlockedBy`, the GitHub Issues adapter, `order_after`, and `order:<id>` bases. Stages 1 to 4 then run against real issues.

Exit: a Go test with a fake adapter lists issues with blockers, and an order based on `order:ORD-0001` waits for ORD-0001, then starts from its recorded commit.

### Stage 2: discovery

`file_finding`, the `finding` message and table, `findings/<order>`, orderless hails, `tracker_review`, and a default `regs.yaml` that drops `tracker_create` from `captain_only` and lists the new captain categories.

Exit: a brainstorm order files five findings, five issues labelled `bmus:unreviewed` appear on the fake adapter at settle with their blockers, and discarding two in the `tracker_review` hail closes them as not planned.

### Stage 3: one task, reviewed and fixed

Review orders and verdicts, the claim rule, fix rounds, stalls, `campaign_land`, and `bmus campaign run --issue <ref>` for one task.

Exit: in holodeck, a reviewer blocks once and then passes, the allowed land hail pushes the round-two head on top of round one, and the implementer claimed neither review.

### Stage 4: backlog intake

The query and ready filter, `campaign_pick`, `max_in_flight`, `campaign_tasks`, generations and `bmus campaign belay`.

Exit: with six issues, two blocked by two others, and `max_in_flight: 2`, at most two tasks are open, no blocked issue starts before its blockers close, and an issue filed mid-task is picked later.

### Stage 5: landing modes and stacking

The `Forge` interface and the GitHub Forge adapter, `land: pr | merge`, `ci_timeout`, stacked tasks and retargeting. Until then, blocked issues wait for their blockers to close.

Exit: with `land: merge` on a fake forge, a stacked dependent is retargeted and merged after its blocker, and a red check stalls a third task with a `campaign_land` hail.

## 7. Open questions

1. Confirm that the GitHub adapters live outside the core packages, in `internal/adapters/github`.
