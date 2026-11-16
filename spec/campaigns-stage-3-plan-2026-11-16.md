# BMUS Campaigns stage 3: one task, reviewed and fixed

## Context

Stage 3 of `spec/campaigns-plan-2026-11-03.md` (section 6) takes one tracker issue from implementation to a pushed branch. `bmus campaign run --issue <ref>` creates a work order from the issue. A crew member who never worked on the task reviews the result and returns a verdict through `report`. Blocking findings start a fix round on the latest work, and a pass raises `campaign_land`, which the captain allows before the bridge pushes. The stage builds sections 4.4, 4.5, 4.7 (`campaign_triage`, `campaign_stalled`, `campaign_land`) and the push mode of 4.9, plus the rows of section 5 for those hails. Its exit test is Verification item 9.

Stages 1 and 2 have landed (epics #141 and #150; #158, stage 2's manual run, is still open). This stage becomes Phase 8 with label `phase:8`. Stage 4's backlog intake and stage 5's Forge are out of scope; section 10 says where the line falls.

What the code has today:

- An order has no kind and no link to anything but its parent and its `source`. `orders.created_by` exists and holds `captain` or `hail:<id>` (a split's children, found again by `store.ChildBatch` under their parent).
- `ReportBody` is `{summary, repos, artifacts?, usage}`. The mission writes it into the done entry's payload (`donePayload`, in the `order-done` step of `completeMission`), with each repo's fetched commit. A report's summary is the agent's `report` tool text, or the final result text when the agent never called it.
- An order takes exactly one repo (`maxOrderRepos`), and so does every task.
- Both claim queries skip an order waiting on an unfinished `after` (`waitingSQL` in `ClaimOldestDirectedOrder` and `ClaimOldestPoolOrder`). The pool claim runs only while the bridge-wide standing-orders setting is on (`claimOrder`). Nothing records who worked on an order once a recall clears its assignee.
- An order ends through many transactions: the mission's settle, a withdraw, a cascade, the tour's backstop, a dismissal. Each one calls `settleFamily` in the same transaction, which is the one place that sees every ending. `BelayFunc` is a callback the belay paths pass in, and nothing else takes one.
- A bridge workflow raises a hail and waits for it (`raiseBridgeHail`, `awaitBridgeHail`). The store allows one hail per raiser and category (`InsertBridgeHail`, unique index `hails_deliver_to`), and a second raise returns the first hail's id.
- The categories fixed to the captain are `budget` and `tracker_review` (`regs.CaptainFixed`). The default `regs.yaml` lists `order_split`, `tracker_close`, the option rule `(?i)delete|destroy|force`, `tag: opaque`, `tracker_comment` and `order_reorder` under `captain_only`, the last as `captain_only[7]`. `on_timeout: default_option` picks the configured index, whatever the category.
- The countermand wait (`awaitCountermandWindow`, shared by `tracker-window` and `reorder-window`) caps its sleep at the tour's settle wait, 5 minutes.
- Pushing is `PushOrder(ctx, id, target, force)`: the done entry's commit, verified under the repo's lock, pushed to the repo's registered URL. The CLI and the console call it, never a workflow.
- The tracker reads one item by ref (`Tracker.Get`). An order whose `source` is `tracker:<ref>` gets the item's text fenced into its body at creation (`resolveSource`, `sourceBody`), and the title "Tracker <ref>: <title>" when it has none.
- The holodeck runs one script for every order its serve receives, and its `report` step carries `summary` and `artifacts`.
- Jettison keeps `bmus/<order>` only while an open order names it in a base (`protectedBranches`).

Where the campaigns plan has gone stale:

- "An eligible reviewer ... has been assignee of no order in the task" counts the review orders too: every round would need a fresh reviewer. Section 3 reads it as work orders only (open question 2). The store keeps no assignee history either, and the rule needs a table.
- "On deck with standing orders on" treats standing orders as a crew member's state. It is one bridge-wide setting.
- Section 4.7 asks for several hails of one category from one raiser (a stall per round, a triage per review). Stage 2's index allows one.
- Section 4.8's settle callback "like `BelayFunc`" would have to be passed through every ending path. A store hook in `settleFamily` covers them all (section 5).
- Section 4.9 calls a repeated non-forced push safe. A push replayed after someone moved the remote branch on is rejected as a non-fast-forward, so the step looks at the remote first (section 7).
- Section 4.7 has `CheckAnswer` gain per-category checks. The regulations check sees no hail context (`regs.CheckAnswer`); stage 2 put such checks in the bridge (`checkReviewAnswer`), and this stage does the same.
- The countermand wait the plan leans on is capped for missions. A campaign isn't bound to a tour and needs the full window.
- The plan's issue numbers (#54, #146, #155) are the decisions log's, as stage 2's plan noted.

## Approach

### 1. Schema

`orders` gains two columns, added at `Open` as `addedColumns` entries, and an index:

```sql
kind TEXT NOT NULL DEFAULT 'work',  -- work or review
task TEXT NOT NULL DEFAULT '',      -- the task workflow's id, '' outside a campaign
CREATE INDEX IF NOT EXISTS orders_task ON orders (task, kind);
CREATE INDEX IF NOT EXISTS orders_created_by ON orders (created_by);
```

Fix orders are `work`: a task's head is its latest done work order, whoever made it.

`hails` gains `raise_key TEXT NOT NULL DEFAULT ''`: one raiser may then raise a category more than once. The unique index becomes `hails_raise ON hails (deliver_to, category, raise_key) WHERE deliver_to IS NOT NULL`, and `Open` drops `hails_deliver_to`. Stage 2's findings hails keep the empty key. `schema.sql` runs before `addedColumns`, and an index naming a column the table lacks fails there, so the index statement moves to a short list `Open` runs after the columns are added.

New tables:

```sql
CREATE TABLE IF NOT EXISTS campaigns (
    id              TEXT PRIMARY KEY,              -- C-0001
    state           TEXT NOT NULL DEFAULT 'running', -- running, belaying, ended
    land            TEXT NOT NULL DEFAULT 'push',  -- push; pr and merge are stage 5
    repo            TEXT NOT NULL,
    base_ref        TEXT NOT NULL,
    division        TEXT NOT NULL DEFAULT '',      -- where work orders go
    assignee        TEXT NOT NULL DEFAULT '',      -- or to whom
    review_division TEXT NOT NULL DEFAULT 'science',
    max_rounds      INTEGER NOT NULL DEFAULT 3,
    created_at      TIMESTAMP NOT NULL,
    ended_at        TIMESTAMP
);

CREATE TABLE IF NOT EXISTS campaign_tasks (
    campaign_id  TEXT NOT NULL REFERENCES campaigns(id),
    ref          TEXT NOT NULL,                -- canonical tracker ref
    workflow_id  TEXT NOT NULL UNIQUE,         -- campaign/<id>/task/<ref>
    state        TEXT NOT NULL DEFAULT 'working',
        -- working, reviewing, triage, fixing, landing, stalled, landed, dropped
    round        INTEGER NOT NULL DEFAULT 1,
    max_rounds   INTEGER NOT NULL,             -- the campaign's, raised by a retry past it
    branch       TEXT NOT NULL,                -- where the land pushes
    implementer  TEXT NOT NULL DEFAULT '',     -- round 1's assignee
    head         TEXT NOT NULL DEFAULT '',     -- latest done work order
    head_commit  TEXT NOT NULL DEFAULT '',
    review_base  TEXT NOT NULL DEFAULT '',     -- the commit round 1 started from
    open_order   TEXT NOT NULL DEFAULT '',
    open_hail    TEXT NOT NULL DEFAULT '',
    why          TEXT NOT NULL DEFAULT '',     -- the stall's reason, or the land's error
    landed_at    TIMESTAMP,
    PRIMARY KEY (campaign_id, ref)
);
CREATE UNIQUE INDEX IF NOT EXISTS campaign_tasks_open ON campaign_tasks (ref)
    WHERE state NOT IN ('landed', 'dropped');

-- Who worked on a task: written in the claim of each of its work orders.
CREATE TABLE IF NOT EXISTS task_workers (
    task    TEXT NOT NULL,
    crew_id TEXT NOT NULL REFERENCES crew(id),
    PRIMARY KEY (task, crew_id)
);
```

The plan put `campaign_tasks` in stage 4. One task still needs a row for its round, head and state, and the console and CLI read it. Stage 4 adds its query, `max_in_flight` and generation columns to `campaigns` and keeps the rest. `workflowVersion` stays `"1"`: missions recorded before replay unchanged, as the new steps run only for orders with a task.

Every new column has a constant default, so I'd add them in place and keep stage 2's databases (open question 1).

### 2. Review orders and verdicts

**Protocol.** `OrderBody` gains `kind` (absent means `work`) and `review {base, round}` for a review order. `ReportBody` gains `verdict`:

```json
"verdict": {"findings": [
  {"severity": "high", "title": "...", "body": "...", "path": "internal/x.go", "line": 12}
]}
```

The schema checks at most 20 findings; `severity` one of `high`, `medium`, `low`; `title` one non-blank line of at most 200 characters without control characters (the finding's pattern); `body` at most 8 KiB; `path` at most 512 bytes with no control characters; `line` an integer from 1. An empty list is a pass. The bridge holds the same limits (`comms.Verdict.Check`), and one test runs every case through both, as for findings (`TestSchemaVerdict`, beside `TestSchemaFinding`). Golden variants `order.review.json` and `report.review.json` cover both shapes in Go and TypeScript.

**The review's prompt.** The sdk harness's instructions for a review order replace the work lines: the checkout holds the head under review; `git diff <base>..HEAD` and `git log <base>..HEAD` show the task's work; run what helps, change nothing and commit nothing; then call `report` with a summary and a verdict, `high` for what must be fixed before landing, `medium` for what should be and is a judgement call, `low` for follow-up work. The `report` tool takes `verdict` for a review order and refuses a call without one ("a review's report needs a verdict; findings may be empty"). The reviewer keeps `file_finding` for things outside the task.

**No commits.** I'd enforce it on the bridge and leave the harness alone. A review's commits never become the head and are never pushed. The task workflow reads the review's done entry, and a commit there other than the head's logs "ORD-0009 committed on the head it reviewed; its commits are ignored" on the review order. A harness hook denying `git commit` would be bypassed by any script and buys nothing the bridge doesn't already guarantee.

**no_verdict.** The `order-done` step reads the report's verdict for a review order. One that is missing or fails `Check` settles the order failed with code `no_verdict` ("the review reported no valid verdict: <why>") in place of done, after pull-work as usual. A verdict on a work order is dropped with a warning. The done payload gains `verdict`, so the report page and the task read the verdict from the record. The echo harness reports no verdict, and its reviews fail this way.

**Holodeck.** The `report` step takes `verdict` (an object) or `verdicts` (an array). A review order uses `verdicts[round - 1]`, the last entry once the rounds pass its length, and one with neither fails `no_verdict`. One script serves every order its serve receives, so the round is how a single reviewer can block once and then pass. Validation checks only that each is an object, as for `finding`.

**Findings from a review.** A review order's mission doesn't start `findings/<order>` (section 5 does it after triage). The task workflow writes the low findings and the mediums Command files into `findings` with keys `verdict.1`, `verdict.2` (an agent's key can't hold a dot), state `new`, the reviewer as `crew_id`, and `path:line` as the body's first line (`store.AddVerdictFindings`, `INSERT OR IGNORE`). The findings workflow then files them with the reviewer's own findings, through Command's triage and one `tracker_review` hail. A review order can hold up to 40 rows that way, which the triage context's body halving already fits.

### 3. The claim rule

Each claim transaction writes `task_workers (task, crew_id)` when the order it moves to away is a work order with a task, with `INSERT OR IGNORE`. A recall keeps the row, which is the point.

Both claim queries skip a review order its claimer worked on:

```sql
-- reviewBarredSQL; ? is the claiming crew member
o.kind = 'review' AND EXISTS (SELECT 1 FROM task_workers w WHERE w.task = o.task AND w.crew_id = ?)
```

The pool claim is the one that matters: the campaign pools every review. The directed claim checks it too, for a review the captain routed to a crew member. The route itself refuses that: "ORD-0009 reviews task o/r#12, which sorell worked on" (`RouteOrder`). The reviewer of round 1 may review round 2.

Eligibility is the claim rule plus a division and a status. The task workflow counts eligible reviewers before each review and on each 15-minute wake while the review sits unclaimed (`store.EligibleReviewers(task, division)`): crew members of the review division, neither dismissed nor in sickbay, with no row in `task_workers`. None stalls the task with `no_reviewer`, and an unclaimed review is withdrawn first. Standing orders off is no stall: the review waits in the pool and the task logs once "review ORD-0009 waits in the science pool: standing orders are off". Eligible crew members all busy is also just a wait.

### 4. Campaign hails

**Raising.** A raise names its key (`store.Hail.RaiseKey`, passed through `raiseBridgeHail`). A re-run of a raise with the same key returns the first hail; another key raises a new one. The task uses `r<round>` for triage, `stall-<n>` for its nth stall and `land-<n>` for its nth land hail.

**Categories.** `campaign_triage`, `campaign_stalled` and `campaign_land`, in `regs` beside `CategoryTrackerReview`. The questions are the bridge's own and carry ids, refs, counts and option words. Issue titles, finding titles, commit subjects and summaries go in the context, out of reach of option rules that read the question and the answer.

| Hail | Options | Context |
|---|---|---|
| `campaign_triage` | `[file, fix, drop]` | campaign, task ref, issue title, review order, round, the mediums numbered from 1 (title, body cut to 2 KiB, path, line) |
| `campaign_stalled` | `[land, retry, drop]`, or `[retry, drop]` with no head | campaign, task ref, issue title, code, reason, round, max_rounds, head, head commit, the order it stalled on, blocking findings |
| `campaign_land` | `[land, retry, drop]` | campaign, task ref, issue title, repo, branch, head, head commit, review base, commits over it (at most 50, sha and subject), rounds, the passing review's order and summary |

Examples: "Triage 2 medium findings from ORD-0009's review of o/r#12, round 1: answer file, fix or drop for all of them, or a JSON object {"fix": [..], "file": [..], "drop": [..]} of finding numbers; unnamed findings are filed." and "Land o/r#12 of campaign C-0001: push ORD-0011 (3f2a9c1e0b7d) to campaign/C-0001/o-r-12 on repo app, 4 commits over 8b1d0e2c4a55, passed review in round 2?"

**Answers.** A per-category check in the bridge reads the decision against the hail's context (`checkCampaignAnswer`), from `AnswerHail` for the captain and beside `regs.CheckAnswer` for Command's consult reply. A reply it refuses counts as a rejected reply and routes the hail to the captain.

- Triage: an option alone applies to every medium. Otherwise the decision is one JSON object of `fix`, `file` and `drop` lists of numbers from 1 to the batch's size, each number at most once, parsed strictly like `parseTriage`. It is stored in normal form with all three lists, unnamed numbers under `file`, and the option of the largest list.
- Stalled and land: an option is required. The decision may follow it with a note, `retry: the flaky test is real`, one line of at most 2 KiB, which the next order's body carries as the captain's note.

**Waiting.** Classify sets `Timeout` to 0 for `campaign_land` whatever `regs.yaml` says, as it does for `tracker_review`, and keeps the reminders: the task waits on it. A triage or stall decided by Command, or by a timeout's `default_option`, has a countermand window. The task waits it out before acting with `awaitCountermandWindow`, which gains an uncapped mode for campaigns, then re-reads the hail in a step and acts on the row, a countermanded decision included. A captain's decision acts at once.

**Belay of a hail.** `on_timeout: belay` holds a campaign hail, as stage 2 made it hold any bridge hail.

### 5. The task workflow

**Shape.** `campaign/<id>` starts one child, `campaign/<id>/task/<ref>`, and waits for it. The task sends its parent `task-ended` with its final state, and the parent then marks the campaign ended (`campaign-end`). The parent waits for that message and rechecks the child's status every 15 minutes; stage 4 turns the same loop into intake. Both workflows live in `internal/bridge/campaign.go`, registered as `campaign` and `campaign-task` with no recovery limit. The parent's id is the task workflow's input along with the ref.

**Order ends.** The store gains a task hook (`Store.SetTaskHook`), which `settleFamily` calls in the ending transaction for an order with a task that ended or split. The bridge's hook sends `{order, state}` on topic `task-orders` to the order's task workflow when it is live (`notifyTask`, like `notifyRaiser`, with `dbos.WithSendTransaction`). The task waits for its open order by reading the row in a step (`check-order`), then receiving on `task-orders` with a 15-minute timeout, and loops until the order has ended (`awaitTaskOrder`). Like `awaitBridgeHail`, it trusts the row and never the message, so a lost or stale message costs at most one wait.

**Creating orders.** Each order is created in a step through the bridge's `CreateOrder` checks, with `created_by` set to a key per task, round, kind and attempt: `campaign:C-0001:o/r#12:r2:review`, then `:a2` on a retry. The store finds an order already created under the key and returns its id in the insert's transaction (`store.InsertOrderOnce`): a re-run of the step creates nothing. Every campaign order has `source: tracker:<ref>`, which fences the issue's current text into its body (`resolveSource`), and a bridge-written title that carries no tracker text: "Implement o/r#12", "Review o/r#12 (round 2)", "Fix o/r#12 (round 2)". A tracker error retries like filing (`trackerRetries`) and then stalls with `tracker_failed`.

**Rounds.** A round is one work order and its review; `max_rounds` 3 means the implementation and two fixes. One round:

1. Create the work order (`create-work`). Round 1's is directed to the campaign's assignee or pooled in its division, based on the campaign's `base_ref`. A later round is a fix order (below).
2. Wait for it. Once done, read its commit from the done entry (`read-head`, through `orderWork`). Round 1 also records the implementer (the order's assignee) and the review base, the commit it shipped from (`baseCommits`). Any other ending stalls (section 6).
3. Check the new head descends from the last with `git merge-base --is-ancestor <last head> <new head>` in the bridge's clone, under the repo's lock (`check-head`). A head that doesn't stalls with `rewritten`. The same step sets `head`, `head_commit` and the state `reviewing`.
4. Count eligible reviewers (`check-reviewers`, section 3).
5. Create the review (`create-review`): a pool order in the review division, kind `review`, based on `order:<head>`, its body naming the round, the head, the review base and, after round 1, the last round's blocking findings, fenced. The order command carries the base and round to the harness (`OrderBody.review`).
6. Wait for the review. A review that failed, was belayed or was withdrawn stalls the task, and `no_verdict` names its code.
7. Read the verdict from the done entry (`read-verdict`). Any `high` blocks. With mediums, the task raises `campaign_triage` and acts on the decision once its window has passed: `fix` mediums block, `file` mediums are filed, `drop` mediums are logged and left. Lows are always filed.
8. Write the findings to file (section 2) and start `findings/<review>` when the review order has any findings at all, the reviewer's own included (`file-review-findings`).
9. Blocking findings in round `max_rounds` stall with `max_rounds`. Earlier, the next round starts with a fix order. No blocking finding is a pass, and the task lands (section 7).

**Fix orders.** Kind `work`, based on `order:<head>`. Directed to the implementer while it is on deck or away, pooled in the campaign's division otherwise, or the implementer's division for a campaign started with `--assignee` alone. Its body, after the bridge's own lines, holds the blocking findings (title, path and line, body), the head's report summary and any captain's note, each fenced as `sourceBody` fences tracker text, and then the issue as its source. A crew member who claims a pooled fix order joins `task_workers` and can no longer review the task.

**Log.** Each step that moves the task writes its row and one entry in the same transaction (`store.SetTask`), kind `campaign`, on the order concerned when there is one, payload `{campaign, task, round}`: "o/r#12 round 2: ORD-0011 done at 3f2a9c1e0b7d", "o/r#12 round 1: review ORD-0009 blocked (1 high, 2 medium)", "o/r#12 stalled: ...", "o/r#12 landed on campaign/C-0001/o-r-12 at 3f2a9c1e0b7d". The console tags the kind CAMP.

**Determinism.** Every read of the store, the tracker, git or the clock is a step. The task's choices depend only on step results and hail rows read in steps. Sleeps are `dbos.Recv` timeouts and `dbos.Sleep`.

**Splits and reorders.** A campaign order must stay one order on one base. A split from an order with a task is refused ("ORD-0005 is campaign C-0001's work on o/r#12: a campaign order can't be split", in `checkSplit`), and so is a reorder of one, the captain's `bmus order rebase` and `after` included (`ReorderOrder`). Breakdowns belong to stage 4's `campaign_triage`.

**Jettison.** Jettison also keeps `bmus/<head>` of every task that hasn't landed or been dropped (`protectedBranches`). Between a pass and the land no open order names the head, and the push needs its branch.

### 6. Stalls

A task stalls on any of its orders ending failed, belayed or withdrawn (a split is refused, and one that happens anyway counts as failed), on `no_verdict`, `rewritten`, `no_reviewer`, `max_rounds`, `tracker_failed`, and a land push that is rejected or out of retries. It sets the state `stalled`, writes the reason and raises `campaign_stalled` under `stall-<n>`. The issue and the task's branches stay as they are.

Each option, once any window has passed:

- `retry` repeats what stalled with a fresh order under a new attempt key: the failed order's kind and base, a review once eligibility allows, a fix round from the last good head after `rewritten`, or the push. After `max_rounds` it raises the task's own `max_rounds` by one and starts the next fix round. A failed order's checkpoint stays with that order; the retry starts from the last head.
- `land` lands the head as it is. Decided by the captain, it is the land's consent and the push runs at once. Decided by Command or a timeout, it raises `campaign_land`, which only the captain answers.
- `drop` belays the task's open order, if any (a pooled one is withdrawn), starts `findings/<order>` for its reviews not yet filed, sets the task `dropped` and ends it.

Command can be handed `campaign_stalled` through `regs.yaml` (section 8). Its retries are bounded: each needs a new stall and a new decision.

### 7. Landing

Only `push` exists in stage 3, and `campaign run` refuses `pr` and `merge` with "land pr and merge come with stage 5". The land target is the task's `branch`, by default `campaign/<id>/<slug>`, the ref with every character outside `[A-Za-z0-9._-]` turned into `-` (`campaign/C-0001/o-r-12`), checked with `checkBranchName`. The remote is the repo's registered URL, as for any push, and stage 3 adds no push configuration.

**The hail.** After a pass, a step reads the commits over the review base (`git log --format='%H %s' <review_base>..<head>`, at most 50 subjects, each cut to 200 bytes) and the passing review's summary (`land-context`), and the task raises `campaign_land` under `land-<n>`. The category joins those fixed to the captain (`regs.CaptainFixed`), so Command never answers it and `default_option` never decides it. `land` pushes. `retry` starts a fix round with the captain's note as its one blocking finding, counted in the rounds and allowed past `max_rounds`. `drop` drops the task.

**The push.** One step, `land-push`, with no DBOS retries of its own:

1. `git ls-remote <url> refs/heads/<branch>`. The branch at the head's commit means an earlier run pushed it, and the step returns success.
2. Otherwise `PushOrder(ctx, head, branch, false)`: the head's recorded commit, verified in the clone, pushed without force. It logs the push on the head order as any push does.

The step retries transient errors 3 times with backoff from 5s, inside the step. `ErrPushRejected` (the branch exists and has moved) stalls with `push_rejected` at once; other errors past the retries stall with `push_failed`. `campaign run` refuses a branch that already exists on the remote, so a rejection means someone pushed meanwhile. The remote branch then holds round 1's commits under round 2's: each fix order started from the last head.

A last step sets the task `landed` with `landed_at` and logs it (`land-done`). Push mode writes nothing to the tracker: the issue stays open for the human who merges, and stage 4's ready set leaves a landed task's open issue out.

### 8. Regulations

- `campaign_land` joins the categories fixed to the captain (`regs.CaptainFixed`): "category campaign_land is fixed to the captain".
- The default `regs.yaml` lists `campaign_stalled` under `captain_only`, after `order_reorder`, as `captain_only[8]`, so earlier indexes keep their numbers. Removing it hands stalls to Command, and the comment says retries then cost tokens without the captain.
- `campaign_triage` goes to Command unless listed. The default's comment says so, and that listing it hands every medium to the captain.
- The `internal/regs/testdata` fixtures gain the stall rule. The tests of categories never auto-answered and fixed to the captain gain `campaign_land` (`TestNeverAutoAnswered`, `TestFixedToCaptain`).
- A bridge with no `regs.yaml` sends every campaign hail to the captain, triage included.

### 9. CLI and console

**CLI.** `bmus campaign`, with RPCs `campaign.run`, `campaign.list`, `campaign.show` and `campaign.belay`:

```
bmus campaign run --issue <ref> --repo <name> [--base <ref>] (--division <d> | --assignee <crew>)
                  [--review-division science] [--max-rounds 3] [--land push] [--branch <name>]
bmus campaign list [--json]
bmus campaign show <id> [--json]
bmus campaign belay <id> [--reason <text>]
```

The run refuses, before anything is written (`campaign.run`): no tracker; a ref `Canon` refuses; an issue `Get` can't find, or one already closed; an issue in another open task (the unique index); a repo or base `CreateOrder` would refuse (`checkOrderRepos`, `checkOrderBases`); an unknown division or assignee; a review division with no crew member at all; `--max-rounds` outside 1 to 10; `pr` or `merge`; a bad branch name; a branch that exists on the remote. It inserts the campaign and its task in one transaction, starts `campaign/<id>` and prints "campaign C-0001 running o/r#12 on app from main: work for sorell, reviews in science, up to 3 rounds, landing by push to campaign/C-0001/o-r-12". At startup the bridge starts the workflow of any running campaign that has none, for a crash between the insert and the start.

The show command prints the campaign, then the task (state, round of max, head and commit, open order, open hail, why), then one row per order (ORDER, KIND, ROUND, STATE, ASSIGNEE). Titles and reasons go through `oneLine`.

**Belay.** Stage 3 can stop a task without it: belay the open order, and the stall hail offers `drop`. That is two steps with a hail in between. I'd bring `bmus campaign belay` forward (open question 4). It sets the campaign `belaying` and, in one transaction per item, closes the task's open hail as timed out ("campaign belayed") and belays or withdraws its open order. The task wakes on either, reads `belaying` in its next step, drops itself with no stall hail, and the parent ends the campaign. Stage 4 adds stopping intake to the same command.

**Views.** `OrderView` gains `kind` and `task`. `HailView` gains nothing: the context names the campaign and task.

**Console.** Two cards, and no campaign panel yet:

- A `LandCard` for `campaign_land`: task ref and issue title, branch, head and review base, the commits, the review's summary. Buttons "Land" (option 0), "Another round" with a note field, and "Drop" behind a `ConfirmAction`. The meta line names the campaign and the task, and the head is the order the card is about (`bridgeHailOrder` reads the context's `head`).
- A `StalledCard` for `campaign_stalled`: the reason, round of max, the blocking findings, and the hail's options, `retry` with a note field.
- A `campaign_triage` hail that reaches the captain uses `HailCard`, whose option buttons apply to every medium; the decision field takes the JSON form.
- The open-orders card shows "review of o/r#12, round 2" or "work on o/r#12, round 2" for an order with a task.
- The mission report page lists a review's verdict under its summary.

All agent text renders as text children. Notifications read "Landing from the bridge" and "Stall from the bridge". A campaign panel waits for stage 4, when several tasks run at once.

### 10. Where stage 3 stops

Stage 4 adds the backlog query and ready filter, `campaign_pick`, `max_in_flight` and slots, generations (`campaign/<id>/gen/<n>`), the 15-minute and created-issue wakes on the parent, intake stop in `campaign belay`, and breakdowns through `campaign_triage`. It extends this stage's parent loop, `campaigns` columns and the open-ref index, and the task workflow is unchanged.

Stage 5 adds the `Forge`, `land: pr | merge`, `ci_timeout`, stacking and retargeting. The land step dispatches on `campaigns.land`, and a failed merge raises the same `campaign_land` with the same three options, the failed checks as `retry`'s blocking findings. Stacked tasks need a task's first order to start from another task's head, which `order:<id>` already allows.

### 11. Other open work

- **Budgets.** Campaign orders are created with no budget and take their division's (`resolveBudget`). A review division can therefore run reviews on a smaller budget than work. A budget stop fails the order and stalls the task, and `retry` starts a fresh order with a fresh budget, each needing a decision. Command's triage consults and any stall handed to Command spend tokens outside every order budget (Phase 4 plan, open question 3): one triage per review with mediums.
- **#128 (security review).** It takes in: verdict titles, bodies and paths, and commit subjects, rendered by the land and stalled cards and the report page; agent text fenced into fix orders' bodies; the questions' exposure to option rules (a repo name holding `force` sends triage to the captain, as stage 2 noted); a reviewer who blocks every round (bounded by `max_rounds`, and the captain sees each stall); a reviewer's commits, never pushed. The branch slug passes `checkBranchName`, and the push goes only to the registered URL.
- **#125 (egress hails).** If it lands first, its `captain_only` entry takes index 8 and `campaign_stalled` moves to 9, with the tests that pin it.
- **#130 (split children whose division has no crew).** The same gap for reviews is `campaign run`'s refusal and the `no_reviewer` stall.
- **#127 (chaos).** A seeded run could kill a beam mid-review and check each task lands once. I'd add it after its exit run.
- **#112 and #113 (backups and restore).** The new tables are in the backup like any other.
- **#177 (carried answers mid-run)** affects campaign orders like any order, and nothing here depends on it.
- **#121, #165, #166 (providers).** No overlap.

### 12. Ordering

1. Schema and store (section 1): the columns, tables and index swap, `InsertOrderOnce`, `task_workers` in both claims, `reviewBarredSQL`, `EligibleReviewers`, the task hook in `settleFamily`.
2. In parallel with 1: the protocol, golden files, combadge's review prompt and `report` verdict, and the holodeck's `verdict`/`verdicts` (section 2).
3. The mission: `kind` and `review` on the order command, `no_verdict` in `order-done`, no findings start for reviews, the route, split and reorder refusals, jettison's protection.
4. Hails and regulations (sections 4 and 8): `raise_key`, the three categories, `checkCampaignAnswer`, classify's timeout, the uncapped countermand wait, the default `regs.yaml`.
5. The workflows: parent, task, rounds, triage, fix orders and stalls (sections 5 and 6).
6. Landing (section 7).
7. CLI and RPCs, `campaign belay` included (section 9).
8. Console (section 9).
9. The exit test, a dated decisions log entry per ticket, and the README's Stage 3 tick.

## Verification

1. `go build ./... && go vet ./... && go test ./...`; `cd combadge && npm run build && npm test`; `cd web && npm test && npm run e2e`.
2. Store: a stage 2 database opens and gains the columns, tables and the new index, with its hails and findings intact (`testdata/schema-before-stage3.sql`). Inserting an order twice under one key returns one id (`InsertOrderOnce`). Raising under two keys raises two hails, and under one key twice raises one (`InsertBridgeHail`). The task hook runs for each ending path: settle done and failed, withdraw, cascade, a belay of an unclaimed order, the tour's backstop.
3. Claims: a crew member in `task_workers` never claims the task's review from the pool or directed, while another crew member of the division does; route refuses it; the round 1 reviewer claims round 2's review; a reviewer who claims a pooled fix order is then barred. The eligible count leaves out dismissed, sickbay and barred crew members (`EligibleReviewers`).
4. Protocol: `TestSchemaVerdict` (limits, an empty list, bad severities, control characters) in Go and under ajv; both golden variants round-trip.
5. combadge: the review prompt names the base; `report` without a verdict is refused for a review and taken for work; the holodeck picks `verdicts` by round and repeats the last.
6. Mission, on holodeck: a review with no verdict fails `no_verdict`; a review that commits is done and its commit is ignored with a note; a work order's verdict is dropped; no findings workflow starts for a review order at its mission's end.
7. Task workflow, on fake beams: a medium filed, one fixed and one dropped by Command's triage, each acted on only after an 8s countermand window, and one countermanded inside it acted on as countermanded; a triage reply that doesn't parse goes to the captain; lows and filed mediums reach the fake tracker through `findings/<review>`; a rewritten head stalls; no eligible reviewer stalls and withdraws the review; `max_rounds` stalls, and `retry` runs one more round; each order ending failed, belayed or withdrawn stalls; `drop` belays the open order and ends the campaign.
8. Landing: a push replayed after the remote has the head pushes nothing; a remote branch moved on stalls `push_rejected`; `land` from a captain-decided stall pushes, and from a Command-decided one raises `campaign_land`; `campaign_land` routes to the captain under any `regs.yaml`, with no timeout.
9. Exit test (holodeck, fake tracker holding #12, a bare repository as the registered remote, default `regs.yaml`, standing orders on). Sorell and tuvok are both in science. Sorell's script commits and reports. Tuvok's reports `verdicts: [{findings: [one high]}, {findings: []}]`. `bmus campaign run --issue '#12' --repo app --assignee sorell` runs round 1 on sorell; the review sits in the science pool while sorell is idle on deck, and tuvok claims it and blocks. The fix order goes to sorell, based on round 1's head, and tuvok passes round 2. The captain answers `land` on `campaign_land`. The remote's `campaign/C-0001/o-r-12` is at round 2's commit, round 1's commit is its ancestor, `task_workers` holds sorell alone, both reviews' claim entries name tuvok, and the campaign has ended.
10. Restart: the bridge stopped while each of the review, the triage hail and the land hail is open resumes the task; each order is created once and the push happens once.
11. CLI and console: `campaign run`'s refusals each create nothing; `campaign show` and `list`; `campaign belay` with an open order and again with an open land hail ends the campaign with no stall hail. Vitest for `LandCard` and `StalledCard` with hostile titles, subjects and summaries as text. Playwright answers a land hail from the page.
12. Manual, once: one issue in a scratch GitHub repository, sdk harness, two crew members; the review blocks or passes on its own judgement, and the captain lands the branch.

## Open questions

1. **Migration.** Every new column has a constant default, so I'd add them in place (`addedColumns`) and keep stage 2's databases, the #158 run's among them. The alternative is stage 2's precedent: a `requiredColumns` entry and a fresh database.
2. **Who may review.** I'd bar only crew members who worked on a work order in the task: the reviewer who blocked round 1 is the best judge of whether round 2 fixed it. The plan's wording bars every earlier reviewer too, and each round would then need a new one.
3. **Medium findings.** I'd send them to Command through `campaign_triage` as the plan says, with the default of filing whatever the answer leaves out. The other choice is to treat every medium as blocking in stage 3 and add triage in stage 4.
4. **Belay now.** I'd bring `bmus campaign belay` forward; it is a small command, and the alternative stop takes a belay and a stall hail.
5. **Stall routing.** I'd keep `campaign_stalled` captain-only in the default `regs.yaml`, with `retry` and `drop` open to Command once the captain removes the rule, and `land` always passing through the captain.
6. **Default branch.** `campaign/<id>/<slug>` keeps every campaign's branches apart. The plan's `campaign/<id>/<ref>` names the same thing; a ref holds `/` and `#`, so I'd slug it.

## Assumptions

- A review fits the sdk harness's ordinary run: the reviewer reads the diff and runs tests within its division's budget, and reports once.
- GitHub accepts a non-forced push that creates a new branch with every commit over the base, as one push.
- DBOS delivers a message sent in a store transaction to a workflow that is live then, including one that is recovering, as `notifyRaiser` relies on.
- One campaign rarely runs more than a handful of rounds, so a task workflow's history stays small without generations until stage 4.
