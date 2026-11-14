# BMUS Campaigns stage 2: discovery

## Context

Stage 2 of `spec/campaigns-plan-2026-11-03.md` (section 6) lets any crew member file findings as it works. When the order's mission ends, the bridge files them as tracker issues labelled `bmus:unreviewed` and raises one `tracker_review` hail in which the captain keeps or discards them. It builds sections 4.6 and 4.7 of that plan and the first row of its section 5 table. Its exit test is Verification item 5.

Stage 1 (epic #141) has landed; most of Phase 4 (epic #109) is open, and nothing here waits on it. Section 9 lists where they meet. The tickets are under epic #150.

What the code has today:

- The tracker (decisions log, "Tracker interface") has `Create` with a `Mark` that finds an issue a lost answer already created and adds its missing links, and `Close` with `not_planned`. Nothing calls `Create` in production. No method removes a label.
- Every hail belongs to an order and a crew member. `hails.order_id` and `hails.crew_id` are `NOT NULL` foreign keys, and the store opens with `foreign_keys(1)`.
- The logic that decides a hail sends the answer to the order's mission (see `decideOpen`, which policy and Command decisions use, and `AnswerHail`), and the logic that countermands sends a correction there (`CountermandHail`). Routing reads the order for its priority and the crew member for its division (`classifyHail`). A consult carries the order's spec and recent log (`consultBody`), and the protocol schema requires `crew` and `order` on a consult.
- `regs.CaptainFixed` routes `budget` to the captain whatever `regs.yaml` says, and the hail workflow then treats the hail as captain-only, so `on_timeout: default_option` holds (decisions log, "Budget enforcement (#119)").
- The default `regs.yaml` lists `tracker_create` under `captain_only`. No code raises that category.
- `progress` and `usage` go from the pump straight to the store, in the inbox transaction, and never into a workflow.

Where the campaigns plan has gone stale:

- Its issue numbers (#54, #150, #155, #146) are the decisions log's. This repository's tracker issue is #33. This plan names log entries by title.
- The marker lookup is settled. The "Tracker interface" entry looks a finding's issue up by time under its key; a lookup by `bmus:unreviewed` would miss an issue `keep` had already relabelled.
- It starts `findings/<order>` from the mission or from a settle callback. The callback isn't needed (section 4).
- It lets `on_timeout: default_option` decide `keep`, which `CaptainFixed` rules out (section 7).
- "A brainstorm order" isn't a kind of order. Any order whose agent calls `file_finding` qualifies.

## Approach

### 1. Schema

**Hails.** `order_id` and `crew_id` become nullable, and `hails` gains `deliver_to TEXT`: the workflow id of the bridge workflow that raised the hail, NULL for a hail from a beam. SQLite can't drop a `NOT NULL` in place. Nobody runs bmus on a database worth keeping yet, so this changes `schema.sql` directly, with no migration, and a `requiredColumns` entry for `hails.deliver_to` refuses an older database with a message to start a fresh one.


A new unique index, `hails_deliver_to ON hails (deliver_to, category) WHERE deliver_to IS NOT NULL`, makes one hail per raiser and category. `store.Hail` scans the nullable columns as empty strings.

**Findings.** A new table, created with `CREATE TABLE IF NOT EXISTS`:

```sql
CREATE TABLE IF NOT EXISTS findings (
    order_id     TEXT NOT NULL REFERENCES orders(id),
    key          TEXT NOT NULL,
    crew_id      TEXT NOT NULL,          -- who filed it
    title        TEXT NOT NULL,
    body         TEXT NOT NULL DEFAULT '',
    labels       TEXT NOT NULL DEFAULT '[]',
    severity     TEXT NOT NULL DEFAULT '',
    blocked_by   TEXT NOT NULL DEFAULT '[]',
    received_at  TIMESTAMP NOT NULL,
    state        TEXT NOT NULL DEFAULT 'new', -- new, dropped, filed, failed, kept, discarded, restored
    why          TEXT NOT NULL DEFAULT '',    -- Command's reason for dropping it, or the filing error
    duplicate_of TEXT NOT NULL DEFAULT '',
    ref          TEXT NOT NULL DEFAULT '',    -- its issue, once created
    PRIMARY KEY (order_id, key)
);
```

`workflowVersion` stays `"1"`.

### 2. Bridge-raised hails

A bridge workflow raises a hail through `store.InsertBridgeHail(h, entry)`: `deliver_to`, category, question, options and context, with no order and no crew member. A second insert for the same `deliver_to` and category returns the first hail's id, so a re-run of the raising step raises nothing new. The raiser then starts `hail/<id>` as a child it never awaits, as the mission does. The raise's log entry names the order the raiser works for. Later entries the hail workflow writes (routing, reminders) name none.

What changes for a hail with `deliver_to`:

- **Classify.** It reads no order or crew member. The Regulations engine sees priority 0, and the "from Command" branch never applies. `on_timeout: belay` holds instead, noting there is no order to belay.
- **Advisory categories.** A new bridge-side set, holding only `findings_triage` (section 4). Wherever routing would reach the captain (no `regs.yaml`, a `captain_only` rule, Command not on deck, a consult timeout, a rejected or escalated reply), a step `skip-advisory` closes the hail `timed_out` with that reason. Classify records a countermand window of 0 for them.
- **Consult.** `consultBody` leaves out `order`, `crew`, `order_spec` and `recent_log`. The schema and `protocol.ts` make `crew` and `order` optional, and a golden variant, `consult.orderless.json`, covers the shape. The sdk harness's consult prompt already writes the hail's fields as one JSON document, and absent fields drop out of it.
- **Decisions and closes.** No answer or correction goes to a mission. The transaction that decides or closes the hail (`AnswerHail`, `decideOpen`, the `TimeOutOpenHailThen` paths) sends the hail's id to `deliver_to` on `hail-closed` when that workflow is live, beside the hail workflow's own wake in `notifyHailClosed`. The raiser reads the hail's row, as `awaitCaptain` does.
- **Countermand.** `store.CountermandHail` is unchanged. `Daemon.CountermandHail` writes the row and its log entry and sends nothing. Per campaigns plan section 4.7, a raiser that acts on a countermandable decision sleeps past `countermand_until` and re-reads the row. Neither stage 2 category can be countermanded (triage has no window, and the captain answers `tracker_review`), so stage 4's `campaign_pick` is the first real user. A bridge test covers the path with a hail it inserts directly.
- **Views.** `HailView` gains `deliver_to`, omitted when empty, and keeps `order_id` and `crew_id` empty. `bmus hails list` prints `-` for both. `bmus status` counts the hail in `hails_open`; the condition, which reads crew members hailing, doesn't change.

### 3. Findings from the crew

**Protocol.** A new beam-to-bridge message, `finding`, order-bound: `FindingBody{key, title, body, labels?, severity?, blocked_by?}`. The schema checks:

- `key` is `^[a-z0-9][a-z0-9-]{0,39}$`, which fits a tracker marker key;
- `title` is one line of 1 to 200 characters;
- `body` is at most 32 KiB;
- `labels` has at most 5 entries of 1 to 50 characters, without commas;
- `severity` is `low`, `medium` or `high`;
- `blocked_by` has at most 10 entries, each a key or a tracker ref.

A golden file, `finding.json`, covers the full shape.

**Combadge.** Every role gets `file_finding`, checked by zod and listed in `ALLOWED_TOOLS`. It emits the message and returns "Finding <key> recorded: the bridge files it as an issue when the order ends." It raises no hail and leaves parking alone. A run refuses its 21st finding with an error result. The same key again replaces the earlier finding, so an agent can revise one. The order's system prompt gains one line on when to file. Holodeck scripts gain a `finding` step that emits its body as given; echo files none.

**Pump.** Like `usage`, a finding goes straight to the store in the inbox transaction (`InboxTx.RecordFinding`). It is kept only while its order exists, hasn't ended, and has the pump's crew member as assignee. A key already recorded is replaced while its row is `new`. An order's 21st key is dropped. Each kept finding logs "finding <key>: <title>" (kind `finding`) on its order. Anything else is dropped with a warning, like a bad `progress` body.

combadge sends its outbox in seq order, and the pump records each message before it reads the next. Every finding a run filed before its report is therefore stored before the mission reads that report.

### 4. The findings workflow

**Start.** The campaigns plan has the mission start `findings/<order>`, and a settle callback start it for orders settled outside their mission: a split parent, a cascade-belayed child. I'd keep only the mission. Findings come only from a beam running the order's mission, and that mission ends after the last of them (section 3). A split parent's findings are Command's, filed before its report. A cascade-belayed child whose mission had already ended started its findings workflow when it ended.

`missionWorkflow`, after it tells the tour, runs a step `count-findings` and, when the order has any, starts `findings/<order>` (`internal/bridge/findings.go`, registered as `findings`, no recovery limit) as a child it never awaits. Every ending counts: done, split, failed and belayed. These steps come after the mission's last recorded step, so a mission recorded before them still replays.

**Steps.**

1. `begin` records the time, the marker's `Since`, and reads the findings. With no tracker it logs "findings of ORD-0001 not filed: no tracker configured" and ends; the rows stay `new`.
2. `list-backlog` reads open issues with `Tracker.List`, capped at 200 (section 6). A failure skips triage with the error as its reason.
3. Triage, below.
4. One `file-finding` step per finding Command didn't drop.
5. The `tracker_review` hail and its answer (section 5).

**Triage.** The findings workflow raises a `findings_triage` hail with no options. Its context holds the order's id and title, the findings numbered from 1 (key, title, body cut to 2 KiB, labels, severity, blockers), then backlog entries (ref, title, labels, blockers) while the encoded context stays within the consult's 32 KiB, and how many were left out. The bridge writes the question:

> Triage 5 findings from ORD-0001 against the open backlog in the context. Answer with a JSON object of finding numbers and refs: drop (numbers that are noise), duplicates (a number to the ref it repeats), blocked_by (a number to refs or f<number>). {} files them all.

Command answers in free text and explains in its rationale. The answer holds numbers and refs only, so an option rule such as `delete|destroy|force` finds no prose to match. A repository whose name holds such a word trips it, and triage is then skipped.

The workflow waits on `hail-closed`, re-reading the row on each wake and every 24 hours. A hail Command decided is parsed strictly against the batch and the offered backlog. An answer that doesn't parse, or names anything else, files everything and notes why. A hail `skip-advisory` closed files everything with its reason. Triage is advisory: it writes `dropped`, `why` (Command's rationale, cut to 2 KiB) and `duplicate_of` on the rows and adds blockers, and nothing waits on a countermand window.

**Filing.** Findings are filed blockers first. A topological sort over the batch's own keys drops the edge that closes a cycle and logs it. Each finding's blockers are:

- refs it named, made canonical (a ref `Canon` refuses is dropped with a note);
- refs Command added;
- the issues of earlier findings it named. A blocker that was dropped or failed drops the link; one marked a duplicate links to the issue it repeats.

Each `file-finding` step reads its row first and skips one with a ref. It calls `Create` with:

- the title;
- the body, then a line "Filed by <crew> from ORD-0001 (severity high).";
- the finding's labels plus `bmus:unreviewed`;
- the blockers;
- `Mark{Key: "finding <nonce> ORD-0001/<key>", Since}`.

The nonce is `store.Nonce`, as in tracker comment keys, so a fresh database's ORD-0001 never matches an older one's marker. The step then stores the ref, sets `filed` and logs "finding <key> filed as o/r#12". When `Create` returns a ref with a link error, the ref is stored and the step fails: its retry finds the issue by marker and adds only the missing links. Steps retry 5 times with backoff up to 2 minutes, then the row is set `failed` with the error and filing goes on.

### 5. The tracker_review hail

**Raise.** After filing, `raise-review` raises one `tracker_review` hail per order, options `[keep, discard]`. A batch with nothing filed, dropped or failed raises none. Its context lists `issues` (`ref`, `key`, `title`), `dropped` (`key`, `title`, `why`, `duplicate_of`) and `failed` (`key`, `title`, `error`). The question names no titles: "Review 5 issues filed from ORD-0001's findings (1 dropped, 0 failed): keep, discard, or answer with the refs to discard and the keys to restore."

**Routing.** Fixed to the captain (section 7). Classify records no reminder and no captain timeout for it, as nothing waits on the hail. An unanswered review changes nothing, and its issues keep `bmus:unreviewed`.

**Answers.** A decision is one of:

```
keep
discard
discard <ref> [<ref>...]
```

optionally followed by `; restore <key> [<key>...]`, and `restore <key>...` alone means keep everything and restore those. `restore` takes dropped and failed keys. `AnswerHail` checks a `tracker_review` decision against the hail's context, writes it in that normal form and sets option 1 when anything is discarded, else 0. Anything else is a bad request naming the refs and keys it may use.

**Acting.** The findings workflow reads the decision and runs one step per issue:

- `review-keep` removes `bmus:unreviewed` (`Tracker.Unlabel`);
- `review-discard` closes the issue as not planned;
- `review-restore` files the finding as section 4 does, without `bmus:unreviewed` (the captain chose it), under the same marker key.

Each is safe to repeat. Rows move to `kept`, `discarded` or `restored`, and one entry sums the review: "review of ORD-0001's findings: kept 3, discarded 2, restored 1". An action out of retries is logged on the order and the rest go on.

Stage 2 has no campaigns. Dropping a task that already picked a discarded issue belongs to stage 4.

### 6. Tracker additions

- `Unlabel(ctx, ref, label)`. GitHub: read the issue as `Close` does, then `DELETE /repos/{o}/{r}/issues/{n}/labels/{name}`. A 404 for a label the issue lacks is success. The fake removes the label.
- `Query.Limit`: the most items `List` returns, 0 for no limit. The GitHub adapter stops paging there, before reading blockers for the rest.
- The fake keeps a created item's labels and exposes each closed item's reason for tests.

### 7. Regulations

`regs.CaptainFixed` gains `tracker_review`: "category tracker_review is fixed to the captain". Discarding closes issues, and `tracker_close` is captain-only by default. The campaigns plan also wanted `on_timeout: default_option` to decide `keep`. `CaptainFixed` makes `default_option` hold, and I'd leave it so: an unanswered review and `keep` differ only in the label, and an issue nobody reviewed should keep saying so.

The default `regs.yaml` drops `tracker_create` from `captain_only`. The rules after it renumber: `tracker_close` becomes `captain_only[3]`, the option rule 4, `tag: opaque` 5 and `tracker_comment` 6. Tests pinning `captain_only[7]` and `[4]` change, and old hail rows keep the rationale they were decided with. The two fixtures under `internal/regs/testdata` drop it too. Comments in the default name the new categories: `tracker_review` is always the captain's, like `budget`; `findings_triage` goes to Command, and listing it under `captain_only` turns triage off. `TestNeverAutoAnswered` swaps `tracker_create` for `tracker_review`, and `TestDefaultCaptainOnly` loses it.

### 8. Console and CLI

**CLI.** `bmus hails review <id>` prints the batch: one row per issue, dropped or failed finding (N, REF or KEY, STATE, TITLE, WHY). With `--discard <ref>` and `--restore <key>` (both repeatable) or `--keep`, it builds the decision and sends `hails.answer`. `bmus hails answer <id> --option 1` discards everything, as for any hail. No new RPC.

**Console.**

- `HailCard` drops the belay control for a hail with `deliver_to`, and its meta line names the bridge and the order whose findings it reviews.
- A `ReviewCard` renders `tracker_review` hails. It lists each issue with a Discard checkbox and each dropped or failed finding with a Restore checkbox and its reason. Buttons: "Keep all" (option 0), "Apply" (the decision from the checkboxes) and "Discard all" behind a `ConfirmAction`. Titles and reasons render as text.
- A `findings_triage` hail shows as "Command is triaging ORD-0001's findings" with no controls. `AnswerHail` refuses one: "hail H-0008 is Command's advisory triage; review the issues in its tracker_review hail".
- `api.ts` gains `deliver_to`. The answer goes through `POST /api/hails/{id}/answer`.

### 9. Stage 1 and Phase 4

- **#125 (egress hails)** adds `checkEgressRequest` to classify and appends `- category: egress` after `tracker_comment`. If it lands first, its rule becomes `captain_only[7]` here, and its tests move with it. Egress hails always have an order.
- **#128 (security review)** takes in both categories, the review card's rendering of agent text, and the triage answer's exposure to option rules.
- **#112 and #113 (backups and restore)**: a backup taken before this stage won't open after it, which is fine while nobody keeps a database.
- **#127 (chaos)** could script findings and check that each order's findings are filed once. I'd add that after its exit run, outside this stage.
- Command's consults spend tokens outside any order budget (Phase 4 plan, open question 3). Triage adds one consult per order with findings.

### 10. Ordering

1. Schema (section 1) and `InsertBridgeHail`.
2. Bridge-raised hails (section 2) with the consult schema change, and `CaptainFixed` plus the default `regs.yaml` (section 7).
3. In parallel with 2: the `finding` message, `file_finding`, the holodeck step and the pump (section 3), and the tracker additions (section 6).
4. The findings workflow: start, triage and filing (section 4).
5. `tracker_review`: raise, the answer form, acting, and `bmus hails review` (section 5, section 8's CLI).
6. The console (section 8).
7. The exit test, a dated decisions log entry per ticket, and the README's Stage 2 tick.

## Verification

1. `go build ./... && go vet ./... && go test ./...`; `cd combadge && npm run build && npm test`; `cd web && npm test && npm run e2e`.
2. Store: a database made from today's `schema.sql` is refused with the fresh-database message. `InsertBridgeHail` twice returns one id. `RecordFinding` keeps, replaces and drops as section 3 says.
3. Regs: `tracker_review` routes to the captain under a `captain_only` rule for it, without one, unchecked, and under an `auto_allow` that fits any command. The default no longer lists `tracker_create`.
4. Bridge, orderless: classify reads no order. The consult matches `consult.orderless.json`. `skip-advisory` closes a triage hail for each reason. `AnswerHail` wakes the raiser and sends no mission anything. A countermand of an orderless hail inside its window writes the row and sends nothing.
5. Exit test (holodeck, fake tracker, default `regs.yaml`, Command scripted to answer `{}`): an order's script files five findings, two blocked by earlier keys and one by an existing issue, then reports. Once the order ends the fake holds five issues labelled `bmus:unreviewed` with those blockers, and a `tracker_review` hail is open. Answering `discard #2 #4` closes both as not planned and removes the label from the other three.
6. Triage: Command drops one finding, marks one a duplicate of an existing issue and adds a blocker; the review lists both, and restoring the dropped one files it without the label. With Command not on deck, and again with no `regs.yaml`, triage is skipped and all five are filed.
7. Replays: a `file-finding` step run twice files one issue. A step whose `Create` returned a ref with a link error adds only the missing link on retry. A bridge restart while the review hail is open resumes the wait, and the answer is acted on once.
8. GitHub adapter, against the `httptest` fake: `Unlabel` with the label present and absent, on a pull request, and `List` stopping at `Limit`.
9. combadge: `file_finding` for every role, its zod refusals, the 21st refused, a replaced key, and the holodeck step through serve.
10. CLI and console: `bmus hails review` with and without flags and its bad requests; vitest for `ReviewCard`; Playwright answers a review from the page.
11. Manual, once: one order on a real bridge files two findings to a scratch GitHub repository, and the review keeps one and discards the other.

## Decisions

The captain settled the open questions on 2026-11-14:

1. **No migration.** The hails change goes into `schema.sql` directly and an older database is refused (section 1).
2. **`tracker_review` stays fixed to the captain**, with no timeout.
3. **A failed or belayed order's findings are filed.**
4. **A skipped triage files everything** without asking the captain.
5. **The caps stand:** 20 findings per order and 200 backlog items.

## Assumptions

- GitHub creates a label a new issue names when the repository lacks it, for a token with push access, and drops labels silently for one without. The adapter checks the created issue carries every label it asked for and returns an error naming any it lacks.
- Command's consult turns out the triage JSON within the sdk harness's 2-minute consult timeout, in one turn, for 20 findings and 200 backlog items.
- Filing 20 issues in sequence stays under GitHub's secondary rate limits on content creation. A rate-limited `Create` retries like any other error.
