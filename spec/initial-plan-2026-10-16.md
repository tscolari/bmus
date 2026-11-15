# BMUS Bridge — Project Plan

*Draft v0.1 · 16 October 2026*

## 0. Summary

BMUS ("Beam Me Up, Scotty") is a local-first control plane for a fleet of coding agents. The **bridge** runs on the developer's machine; every **crew member** (agent) lives in its own Teleport Beam; the developer is the **captain**. Crew report back when they finish, need another order, or hit a decision they can't make. Science and Operations crew hail **Command**; Command answers what it safely can and escalates the rest to the captain through the console.

The plan below commits to a small number of load-bearing decisions:

| # | Decision | One-line rationale |
|---|---|---|
| D1 | Bridge is a single Go binary with one SQLite file | Matches Teleport's stack, cross-compiles, no services to run |
| D2 | Durable execution via **DBOS Transact Go v1** with its SQLite driver | In-process library, durable messages + sleeps + rate-limited queues, no sidecar |
| D3 | Bridge-initiated comms over Teleport SSH (`tsh beams exec`) carrying an NDJSON protocol | Beams can't reach a laptop; SSH is owner-scoped and audited already |
| D4 | In-beam daemon (**combadge**) runs the agent via the Claude Agent SDK | `canUseTool` turns clarifying questions and permission prompts into hails |
| D5 | Beams are cattle, crew are pets — state lives on the bridge, beams rotate | 24h TTL, no snapshotting, beams die |
| D6 | For the MVP the bridge is the only thing that pushes git; code moves as bundles | No GitHub credentials in beams; captain controls what leaves the ship |
| D7 | Command lives in a beam; its *authority* is enforced on the bridge by deterministic **Regulations** | Keeps the laptop secret-free; LLM judgment never overrides hard rules |
| D8 | Transporter shells out to `tsh` first, moves to the Beams API later | Ship now, upgrade to restricted egress + runtime allowlists when the API is easy |
| D9 | Console is the existing mock, served on localhost with hardening | Design already exists; localhost UIs are an attack surface |
| D10 | A **holodeck** provider simulates beams and agents | Fast UI work and deterministic CI without burning beams or inference |

Rough size: about ten engineer-weeks to a hardened multi-crew bridge with chain of command, parallelisable to roughly six with two people.

---

## 1. What we are building

The concept comes from the BMUS Bridge mock (bridge console + ship's manifest). The vocabulary is the spec:

| Term | Meaning in the system |
|---|---|
| Bridge | Control loop + console on the developer's machine |
| Transporter room | Beam manager: provisions, energizes, recalls beams |
| Captain | The developer/operator |
| Crew member | An agent bound to one beam per tour |
| Divisions | Command (plan, triage, split), Science (research, analysis, review), Operations (build, refactor, test, fix) |
| Directed order | Routed to a named crew member; queues until they are on deck |
| Open order | In the pool for any free crew member of the matching division |
| Standing orders | Auto-dispatch switch; free crew pull from the pool, red alerts first |
| Alert level | Priority: routine, yellow, red |
| On deck / Away / Hailing / Sickbay | Idle with warm beam / working / blocked on a decision / beam dead or unhealthy |
| Beam up | Order finished, or recalled early and returned to the pool |
| Captain's log | Append-only audit trail |
| Condition | Red if anyone is in sickbay or a red alert is open; yellow while anyone is hailing or a yellow alert is open; else green |

New constraints from this round: the bridge runs on the developer's machine; agents run isolated in beams and report back for decisions and new work; the hail chain is *other divisions → Command → captain*; and the bridge's main loops must run on a lightweight, self-contained, persistent workflow engine, ideally SQLite-backed.

**Operating assumption for v1: the laptop is up while the fleet is working.** If it sleeps, agents keep working on their current orders, but hails, dispatch, beam rotation and re-energize wait until it wakes. An always-responsive bridge is a future step that moves the bridge to another, always-on machine (see Phase 5); nothing in v1 should be built to work around a sleeping laptop.

---

## 2. What Beams gives us (and the constraints that shape the design)

Facts below are from the public Beams docs (18.x) and the public `gravitational/teleport` repository. Every API detail here should be re-checked against the current release during Phase 0.

| Capability / limit | Design consequence |
|---|---|
| Each beam is a Firecracker microVM with a Teleport-issued identity; `tsh beams add` returns in about 5 s and supports `--no-console` and `--format=json` | Provisioning is cheap enough to treat beams as disposable |
| `tsh beams exec / ssh / scp / rm / ls / publish` (tsh ≥ 18.8.0) | Everything we need for v0 is scriptable |
| Beams auto-purge after **24 h** | Crew must rotate beams ("watch change"); no state may live only in a beam |
| Beta caps: **20 concurrent beams per tenant**, **5 new beams/min**, per-tenant inference cap | Roster ≤ ~18 plus spares for re-energize; creation must be rate-limited; token budgets per order |
| 2 vCPU / 8 GiB / ~20 GiB per beam | Heavy builds of large repos will be slow; warm caches in a custom image |
| `ANTHROPIC_BASE_URL` / `OPENAI_BASE_URL` preset; credentials injected by the LLM proxy outside the beam | No inference keys anywhere — not in beams, ideally not on the laptop either |
| `claude` and `codex` preinstalled; `beam-init` is PID 1 and `beamctl start/stop/restart/list/logs` manages long-running services that survive SSH disconnects | The combadge runs as a `beamctl` service; agents keep working when the bridge disconnects |
| Single-user by design: only the creating user can access a beam (owner labels + `beam-user` role) | One bridge per developer; fleet sharing is out of scope |
| In beta, delegation inherits the user's **full** permission set and egress is unrestricted | Run the bridge as a low-privilege Teleport user until permissions can be scoped per beam |
| Only port 8080 can be published | Don't spend the one port on bridge comms; leave it for app previews |
| gRPC `BeamService` (`api/proto/teleport/beams/v1`) has `CreateBeam` with `EgressMode` (restricted/unrestricted) and `allowed_domains`, `UpdateBeam` that can change `allowed_domains` and publishing, user labels (except the `teleport.internal/beam/` prefix), `GetBeam` by alias, `ListBeams` | Native API unlocks per-crew egress allowlists and runtime "egress hails"; labels let us reconcile after a bridge restart |
| Public `tsh beams add` currently creates beams with unrestricted egress | Restricted egress needs the API path (or a newer tsh) |

Licensing note: the `api` module (where the Beams protos and generated clients live) is Apache-2.0, while `lib/` (including the SSH client code `tsh` uses) is AGPL. Shelling out to `tsh` for SSH in v0 sidesteps that question.

---

## 3. Architecture

```
 Developer machine                                         Teleport Cloud
┌─────────────────────────────────────────────────┐       ┌──────────────────────────────────┐
│ bmus (single Go binary)                          │       │  Beam bm-1c9e04 (Sorell, Science)│
│                                                  │       │ ┌──────────────────────────────┐ │
│  Console (localhost web UI, SSE)   CLI (bmus …)  │       │ │ beam-init (PID 1)            │ │
│        │                              │          │       │ │  └─ combadge  (beamctl svc)  │ │
│        ▼                              ▼          │       │ │      ├─ outbox (local)        │ │
│  ┌───────────────────────────────────────────┐   │       │ │      ├─ Agent SDK harness ──► LLM proxy
│  │ Bridge core                               │   │       │ │      │   canUseTool → hails   │ │
│  │  DBOS workflows: Crew, Mission, Hail,     │   │       │ │      │   hooks → regs checks  │ │
│  │  Dispatch, WatchChange, Reconcile         │   │       │ │      └─ workdir (git)         │ │
│  │  Regulations engine (regs.yaml)           │   │  SSH  │ └──────────────────────────────┘ │
│  │  Comms pumps (1 per beam) ◄───NDJSON──────┼───┼──────►│  Beam bm-a4410f (Dunbar, Ops)    │
│  │  Transporter (tsh │ gRPC │ holodeck)      │   │ tsh   │  Beam bm-7f3a21 (Varga, Command) │
│  │  Cargo bay (bundles, transcripts)         │   │ exec  │      └─ consult lane              │
│  └───────────────────────────────────────────┘   │       │  …up to the tenant cap           │
│        │                                         │       └──────────────────────────────────┘
│        ▼                                         │
│  ~/.bmus/bridge.db  (SQLite, WAL: DBOS system    │
│                      tables + domain tables)     │
│  ~/.bmus/cargo/     (git bundles, session files) │
└─────────────────────────────────────────────────┘
```

**Bridge core** owns all durable state and every decision about routing. **Comms pumps** are plain goroutines, one per live beam, that hold a long-lived `tsh beams exec <beam> -- combadge attach --since <seq>` session; they record inbound messages, write progress straight to the log, and forward state-changing messages into workflows. **Transporter** is an interface with three implementations: `tsh` (v0), native API (v1), and holodeck (sim). **Cargo bay** is a directory of checkpoints the bridge has pulled out of beams.

**Combadge** is the only BMUS code inside a beam. It speaks the protocol on stdio of the attach session, keeps a local outbox so it can keep working while the bridge is away, runs the agent harness for the current order, turns questions and risky tool calls into hails, and produces checkpoints on request.

---

## 4. Key design decisions

### D1 — Local-first single binary, one SQLite file

`bmus` is one Go binary providing the daemon (`bmus bridge`), the console, and a CLI (`bmus order`, `bmus hails`, `bmus crew`, `bmus log`). The CLI talks to the daemon over a local socket rather than opening the database itself, which keeps SQLite single-writer. Everything lives under `~/.bmus/`. The database runs in WAL mode, takes a process lock on startup, and is backed up nightly with `VACUUM INTO`.

Go is chosen because it is Teleport's language, the Beams API client is Go, and a single static binary is the easiest thing to put on a developer machine. The in-beam piece is a separate language decision (D4).

### D2 — Workflow engine: DBOS Transact Go with SQLite

DBOS was the starting hunch and it holds up. Go v1.0.0 shipped on 31 July with SQLite as an optional driver (pure-Go `modernc.org/sqlite`, so no cgo), database-backed queues and schedules that can be created at runtime, and a `Client` that is a sub-interface of the workflow context. The primitives map almost one-to-one onto the ship: durable `Send`/`Recv` with topics and timeouts are hails and orders; `SetEvent`/`GetEvent` publish mission status to the console; durable `Sleep` drives reminders and watch changes; queues with concurrency and rate limits are the transporter's 20-beam and 5-per-minute limits.

**Alternatives considered**

| Option | In-process? | Storage | Human-in-the-loop primitive | Verdict |
|---|---|---|---|---|
| **DBOS Transact Go v1** | Yes, library | SQLite (opt. driver) or Postgres | `Recv` with timeout, `Send` with idempotency key | **Chosen.** Smallest footprint with every primitive we need. v1 is fresh — pin the version. Their docs position SQLite for dev/test; our "production" is a single local process, which is the case where that caveat matters least |
| go-workflows (cschleiden) | Yes, library | SQLite, Postgres, MySQL, Redis | Signals, timers, sub-workflows | **Runner-up.** Temporal-style replay with an embedded diagnostic UI. Stricter determinism rules (no `select`, no map iteration in workflow code) and a smaller community |
| Temporal (dev server, SQLite file) | No, separate server | SQLite via dev server | Signals, updates | Most mature, best UI, but the dev server is not meant for production and embedding the server is heavy. Overkill for one user |
| Restate | No, separate Rust server | Embedded log/KV (not SQLite) | Awakeables, virtual objects | Lovely conceptual fit (virtual object per crew member) but makes the bridge two processes; Go SDK still pre-1.0 |
| Inngest single-node | No, separate server | SQLite + in-memory Redis with periodic snapshots | `waitForEvent` | Snapshot window means a laptop crash can lose recent state; single-node mode labelled beta |
| durabletask-go | Yes, library | SQLite | `RaiseEvent` / `WaitForSingleEvent` | Workable, but lineage is split across Microsoft, Dapr and forks; API stability unclear |
| LangGraph-style checkpointers | Yes | SQLite | Interrupts | Built for a single agent loop, not fleet orchestration; Python/TS |
| Hand-rolled step log on SQLite | Yes | SQLite | Whatever we build | Explicitly what we said we wouldn't do |

**How we use DBOS well.** Workflow functions re-run from the top on recovery, with completed steps returning their recorded output, so all I/O (tsh calls, DB reads, transmits) goes in steps and workflow code between steps stays deterministic. Workflow IDs are deterministic (`order/ORD-0417`, `hail/H-0091`, `crew/sorell/tour/12`) so starts are idempotent. History is bounded by giving each crew tour (≤24 h) its own workflow and keeping chatty progress events out of workflows entirely. Domain tables sit in the same SQLite file so a single backup captures everything.

### D3 — Comms: bridge-initiated, over Teleport SSH, NDJSON

A beam cannot dial a laptop, and the one publishable port is better left for app previews. So the bridge dials out: for each live beam a comms pump runs `tsh beams exec <beam> -- combadge attach --since <last_acked_seq>`, reads newline-delimited JSON from stdout, and writes commands to stdin. This rides on owner-only SSH that Teleport already authorises and records.

Delivery is at-least-once in both directions with dedup on the receiver. Every beam→bridge message carries a per-crew sequence number; the bridge records `(crew, seq)` before acking, and the combadge trims its outbox on ack. Every bridge→beam command carries a command ID the combadge dedups. When the laptop sleeps or the network drops, the pump reconnects with backoff and resumes from the last acked sequence. Agents keep working meanwhile; they only stall when they need an answer.

### D4 — Combadge: in-beam daemon running the Claude Agent SDK

The combadge is started as a `beamctl` service so it outlives SSH sessions and restarts cleanly. It runs the agent through the Claude Agent SDK rather than raw `claude -p`, because the SDK's `canUseTool` callback fires both for tools that need approval and for `AskUserQuestion`, and pauses the agent until the callback returns. That is exactly a hail: the combadge emits it, waits for the answer over comms, and returns it to the agent. `PreToolUse` hooks run before anything else in the permission flow and are where we enforce in-beam policy (deny force-push, route `git push` to a hail, flag network calls to non-allowed domains).

The combadge also exposes a few in-process tools to the agent — `progress(pct, note)`, `report(summary, artifacts)`, `request_handover(note)` — so progress bars mean something and final reports are structured.

Recommended language for the combadge is TypeScript (first-class Agent SDK, and the beam image already has Node). The protocol is language-neutral, so a Go or Python combadge is a later option, and a Codex harness can sit behind the same adapter interface.

Hail waiting has two modes. Short waits hold the agent's query open. Long waits (beyond ~15 minutes, or approaching a watch change) **park** the mission: checkpoint, end the query, and resume the session later with the answer injected. v1 can ship with hold-only plus park-on-watch-change; general parking comes in Phase 3.

### D5 — Beams are cattle, crew are pets

A crew member's identity (name, division, rank, dossier of learned conventions, session lineage, current order) lives on the bridge. A beam is just the body for the current tour. Three paths reuse the same checkpoint/restore mechanism:

**Watch change.** About an hour before a beam's expiry, the crew workflow asks the combadge to reach a safe point, checkpoint, and stand down; the transporter energizes a fresh beam; the combadge restores and resumes.

**Re-energize from sickbay.** Missing heartbeats trigger a `GetBeam`/`ls` probe. If the beam is gone or unhealthy (OOM, disk full, exit 137), the crew member goes to sickbay with the last checkpoint intact. Re-energize provisions a fresh beam and restores.

**Recall.** "Beam up" asks for a checkpoint, then returns the order to the pool. The checkpoint stays attached as handover notes for whoever picks it up next.

A checkpoint is a git bundle of the work branch relative to its base commit, the agent session transcript, and a short handover note written by the agent. The bridge pulls them with `tsh beams scp` into the cargo bay. Session resume across beams depends on the transcript landing at the same path with the same working directory; Phase 0 must verify that on a real beam. If it doesn't hold, the fallback is "resume from handover note + diff", which is lossy but robust.

### D6 — The bridge is the only git pusher (MVP)

Code goes in as a bundle (or the beam clones read-only from a pre-baked mirror in the custom image), crew commit locally, and work comes back out as bundles. The bridge applies them to a local worktree and pushes with the developer's own credentials, optionally after the captain reviews. No GitHub credentials ever enter a beam, which matters while delegation inherits full permissions. When the Beams GitHub integration lands, read paths can move to it first; write paths only once delegation can be scoped.

The cost is moving bytes. A large repository should come from a custom crew image carrying a recent shallow mirror plus warmed Go module and build caches, and bundles should be incremental against a base commit already in the image.

### D7 — Chain of command

**Routing.** Hails from Science and Operations go to Command. Hails from Command go to the captain. If Command is in sickbay or not on deck, hails go to the captain directly and the condition goes yellow.

**Where Command runs.** Command is a crew member with its own beam, using the LLM proxy like everyone else, so the laptop holds no inference credentials. Command's beam runs two lanes: a *consult lane* (short, read-only Agent SDK queries; preempts) and a *mission lane* (its own orders, like splitting an RFC into tickets). The bridge sends each consult with the context Command cannot see on its own: the order spec, the hail, recent log lines, the captain's standing guidance, and the relevant Regulations.

If an LLM proxy becomes reachable from workstations, a bridge-local Command becomes viable without secrets. Keeping the consult interface identical lets us move it later without touching the rest.

**Authority.** Command proposes; Regulations decide. Regulations are deterministic rules in `regs.yaml`, evaluated on the bridge before routing and again on Command's proposed answer. Some hail categories are captain-only no matter what Command says. Command must return a structured decision — answer or escalate, chosen option, rationale, confidence, risk flags — and anything below the confidence threshold escalates. Auto-answers appear in the console with a **countermand** button open for a few minutes; countermanding sends a correction to the crew member.

```yaml
# regs.yaml — illustrative
captain_only:            # Command may never answer these
  - category: destructive        # force-push, branch delete, rm outside workdir, DROP/TRUNCATE
  - category: scope_change       # widen beyond the order ("same API in three other services")
  - category: egress             # new allowed_domains
  - category: access_request     # Teleport access requests / elevated roles
  - category: external_effect    # merge, release, message to humans
  - alert: red
auto_allow:              # no hail at all
  - tool: Bash
    match: "^(go test|go build|npm (ci|install)|make lint)\\b"
command_may_answer:
  min_confidence: 0.75
  countermand_window: 5m
escalation:
  captain_reminder_every: 30m
  on_timeout: hold        # hold | belay | default_option
```

**Prompt injection.** Hail text originates from an agent that may have read untrusted content. Command treats it as data, consults are read-only, and the Regulations are not an LLM. The console shows the raw question and the crew member's context next to Command's summary so the captain is never deciding on a paraphrase alone.

### D8 — Transporter: tsh first, API next

v0 wraps `tsh beams add --no-console --format=json`, `ls --format=json`, `rm`, `exec`, `scp`. The bridge needs the captain's `tsh` session to be valid; when the cert expires, the condition goes yellow and the console prompts a re-login instead of failing silently.

v1 calls `BeamService` directly to get restricted egress with per-division allowlists at creation, `UpdateBeam` to add domains at runtime (approved through an *egress hail*), and labels such as `bmus/bridge=<install-id>` and `bmus/crew=<id>` so a restarted bridge can reconcile against `ListBeams`.

Provisioning goes through a DBOS queue with a concurrency limit at the tenant cap and a rate limit of five per minute, so a burst of re-energizes can't trip the API limit. A scheduled reconcile workflow reaps beams the bridge doesn't recognise and flags crew whose beam vanished.

### D9 — Console

The existing mock becomes the real UI, embedded in the binary and fed by a small HTTP API plus server-sent events from the domain tables. Hails also raise OS notifications. Because a localhost UI that can answer "allow force-push?" is an attractive target for any web page the developer visits, the console binds to 127.0.0.1 only, requires a per-session token, checks `Host` and `Origin` headers (DNS-rebinding protection), and uses CSRF tokens on every mutation.

### D10 — Holodeck

The holodeck transporter fakes beams as local processes or containers, and the holodeck harness replaces the agent with scripted behaviours: progress, a hail at 40–90%, success, a sudden 137. That's essentially the mock's tick loop made real. It powers UI development, demos, and CI tests of recovery paths that would otherwise need real beams and real tokens.

---

## 5. Workflows

| Workflow | ID scheme | Started by | Waits on | Ends when |
|---|---|---|---|---|
| **Crew tour** | `crew/<id>/tour/<n>` | Roster start, re-energize, watch change | `Recv("orders")`, heartbeats via comms, durable sleep until T-60m | Tour expiry (starts tour n+1), sickbay, dismissal |
| **Mission** | `order/<id>` | Dispatch claims an order for a crew member | `Recv("from-crew")` for hail, report, error; `Recv("from-hail")` for decisions | Done, failed, belayed, recalled |
| **Hail** | `hail/<id>` | Mission receives a hail | `Recv("consult-reply")`, `Recv("captain")` with reminder sleeps | Decision delivered to the mission |
| **Dispatch** | singleton, event-nudged + scheduled | Order created, crew on deck, standing orders toggled | — | Each pass is short |
| **Transporter** | `beam/<crew>/<n>` on the transporter queue | Crew tour | Queue slot under rate/concurrency limits | Beam ready or provisioning failed |
| **Reconcile** | scheduled every few minutes | Schedule | — | Each pass is short |

**Dispatch** mirrors the mock: for each on-deck crew member, take the oldest highest-priority *directed* order first; if standing orders are on, take the highest-priority *pool* order whose division matches. A single dispatcher means "rank" is a tie-break policy rather than a lock, and the claim is a single conditional `UPDATE … WHERE state='pool'` inside a step.

**Hail flow**

```
 Crew (combadge)  ── hail H-0091 ──►  Comms pump: record seq, ack, Send → Mission wf
                                                                         │
                                                                         ▼
                                                           Hail wf  hail/H-0091
                                                                         │
                                                     regs.route(H-0091) ─┤
            ┌──────────────────────────────┬─────────────────────────────┴───────────────┐
            ▼                              ▼                                             ▼
      auto_allow                    command_first                                 captain_only
   decide(by=policy)        transmit consult → Command beam                    notify captain
            │               Recv(consult-reply, 10m)                                    │
            │               regs.check(answer)                                          │
            │                 ok & conf ≥ t ──► decide(by=command)                      │
            │                 otherwise ──────► escalate ───────────────────────────────┤
            │                                         │                                 ▼
            │                                         │              SetEvent(status=captain)
            │                                         │              Recv(captain) + reminder sleeps
            │                                         │              decide(by=captain)
            ▼                                         ▼                                 ▼
       Send(from-hail) → Mission wf → transmit answer{cmd-id} → combadge resumes the agent
```

Command-decided answers stay countermandable for the configured window; a countermand is just a second `answer` with `decided_by=captain` that supersedes the first.

**Crew and order states** extend the mock with two sub-states the chain of command needs: a hail is either *with Command* or *with the captain*, and an order can be *parked* (checkpointed and waiting on a long decision). Everything else — pool, directed, away, hailing, done, failed, belayed, withdrawn; deck, away, hailing, sickbay — carries over unchanged.

---

## 6. Protocol

```json
{"v":1,"seq":1842,"crew":"sorell","order":"ORD-0414","type":"hail","ts":"2026-10-16T09:12:03Z",
 "body":{"hail_id":"H-0091","category":"scope_change",
         "question":"The same deprecated API shows up in three other services. Widen the scope?",
         "options":["Widen scope","Stay on target"],
         "context":{"files":["lib/raft/snapshot.go"],"diff_stat":"+212 −40"}}}
```

| Direction | Type | Purpose |
|---|---|---|
| beam → bridge | `hello`, `heartbeat` | Version, harness, beam health (disk, mem), outbox depth |
| beam → bridge | `progress` | Self-reported percent and note; goes straight to the log, not into workflows |
| beam → bridge | `hail` | Question or permission request with category, options, context |
| beam → bridge | `consult_reply` | Command lane only: structured decision |
| beam → bridge | `checkpoint_ready` | Paths of bundle, transcript, handover note for the bridge to `scp` |
| beam → bridge | `report` | Final result: summary, branch, artifacts, token usage |
| beam → bridge | `error` | Harness crash, tool failure, budget exceeded |
| bridge → beam | `ack` | Highest contiguous seq recorded; lets the combadge trim its outbox |
| bridge → beam | `order` | Assign mission: spec, base ref, budget, regs excerpt, dossier |
| bridge → beam | `answer` | Hail decision with `decided_by` (policy, command, captain) |
| bridge → beam | `consult` | Command lane only: hail plus fleet context |
| bridge → beam | `checkpoint`, `stand_down`, `recall`, `belay` | Lifecycle control |

A JSON Schema for these lives in `protocol/` and is shared by the Go bridge and the TypeScript combadge, with golden-file tests on both sides.

---

## 7. Data model (domain tables)

| Table | Key columns |
|---|---|
| `crew` | id, name, rank, division, spec, status, current_beam, tour_no, dossier_path, harness |
| `beams` | uuid, alias, crew_id, region, created_at, expires_at, egress_mode, allowed_domains, compute_status |
| `orders` | id, title, body, division, priority, state, assignee, parent_id, base_ref, branch, budget_tokens, created_by |
| `hails` | id, order_id, crew_id, category, question, options, context, route, status, decided_by, decision, rationale, confidence, countermand_until |
| `checkpoints` | order_id, seq, commit, bundle_path, transcript_path, handover_path, created_at |
| `log` | id, ts, kind (beam/order/hail/done/fail), crew_id, order_id, text, payload |
| `inbox` | crew_id, seq, received_at (dedup) |

`parent_id` on orders lets Command split a big order into children; the parent waits on its children and closes when they do.

---

## 8. Security model

**Identity and blast radius.** In beta every beam carries the captain's full Teleport permissions. Until a beam's permissions can be scoped, the bridge should run as a dedicated Teleport user with no production access — the Beams long-running-agents guide recommends the same for agent demos. Once they can be, scope them per division: Science read-only, Operations limited to staging, Command consults with no infrastructure access at all.

**Egress.** Unrestricted in beta. With the API path, every beam starts restricted with a division allowlist (package registries for Operations; docs sites for Science), and new domains go through captain-only egress hails executed with `UpdateBeam`.

**Secrets.** None in beams; none on the laptop for inference (Command runs in a beam; bridge-side LLM use, if any, goes through `tsh proxy app` to the LLM proxy). Git credentials stay on the laptop (D6).

**In-beam policy.** Agents do not run with permissions bypassed. Permission mode plus `canUseTool` and `PreToolUse` hooks route risky calls to hails, and `auto_allow` keeps routine tool use frictionless.

**Audit.** The captain's log is append-only. Every order ID is exported into the beam environment so Teleport's audit events and session recordings can be correlated with bridge records.

**Console.** Localhost-only, tokenised, Host/Origin checks, CSRF (D9).

---

## 9. Delivery plan

### Phase 0 — Spikes (≈1 week)

Each spike answers a yes/no question and leaves a short write-up.

| Spike | Question | Pass condition |
|---|---|---|
| S1 DBOS + SQLite | Do `Recv` waits and durable sleeps survive kill -9, reboot and laptop sleep? Does the queue rate limiter hold 5/min? | A 24 h `Recv` resumes after restart; no lost or duplicate messages across 1,000 sends |
| S2 Comms over `tsh beams exec` | Is a long-lived stdio session stable? How does it behave across sleep, network flaps and cert expiry? | Reconnect with resume-from-seq works; latency under 1 s; failure modes documented |
| S3 Agent SDK in a beam | Does the SDK work against the preset proxy URL? Do `canUseTool` and hooks behave as expected? Can a session transcript copied to a fresh beam be resumed? | Hail round-trip works end to end; resume works, or the handover-note fallback is chosen |
| S4 Repo logistics | How long do bundle in/out and builds of a large repository take on 2 vCPU? Can we use a custom image today? | Numbers recorded; image strategy chosen |
| S5 Beams API from Go | Using the Beams gRPC API: restricted egress, `allowed_domains`, `UpdateBeam`, labels, auth from a tsh profile | Working client snippet, or a documented reason to stay on tsh longer |

### Phase 1 — One crew member, end to end (≈2 weeks)

Deliverables: `bmus` binary with DBOS on SQLite; tsh transporter; combadge v0 (Agent SDK harness, outbox, protocol, `beamctl` service); crew-tour and mission workflows; directed orders; hails straight to the captain via CLI; captain's log; holodeck transporter and harness.

Exit: kill the bridge mid-mission and restart it, and the mission completes; kill the beam and the crew member lands in sickbay with a checkpoint; the same scenarios pass in holodeck CI.

### Phase 2 — Fleet and console (≈3 weeks)

Deliverables: roster config; pool, divisions, priorities and the standing-orders dispatcher; transporter queue limits; checkpoints into the cargo bay and re-energize-with-resume; watch change before TTL; recall, withdraw, re-route; the console wired to live data with SSE and notifications; bridge-side push of returned bundles.

Exit: six crew run real tasks for a full day, survive at least one TTL rotation each, and the console matches the mock's behaviour.

### Phase 3 — Chain of command (≈2 weeks)

Deliverables: Command's consult lane; Regulations engine and `regs.yaml`; routing, re-check, countermand window; Command order-splitting into child orders; hail parking for long waits; metrics for auto-resolved share, captain overrides and time-to-answer.

Exit: a table-driven test suite proves no captain-only category is ever auto-answered (including Command picking a captain-only option); over a week of use, measured escalation and override rates are reported.

### Phase 4 — Hardening (≈2 weeks)

Deliverables: native API transporter with restricted egress and egress hails; reconcile workflow; per-order token and turn budgets; console hardening; nightly backups; scoped permissions per division if Beams supports them.

Exit: a security review of the console and Regulations; a chaos run in holodeck (random beam loss, sleep, cert expiry) with zero lost orders.

### Phase 5 — Drawing board

| Idea | Proposed approach |
|---|---|
| **Redshirts** | Order-scoped crew: beam created for one order and destroyed on return. Cheap now that checkpoints and the transporter exist; good fit for reviews and one-off research |
| **Away teams** | One beam per member (keeps "one crew, one beam", avoids fighting over 2 vCPU), coordinated by a parent order. Work is exchanged through the bridge as bundles; Command acts as team lead |
| **Rank** | Tie-break in the single dispatcher (seniority, then time on deck); no distributed locking needed |
| **Shore leave** | Idle crew past a threshold have their beam removed; return is a fresh beam from the custom image plus restore. Without snapshotting, "image stays ready" means the custom image, not a frozen VM |
| **Always-on bridge + subspace hails** | Move the bridge daemon to an always-on machine and switch DBOS from SQLite to Postgres (a configuration change, not a rewrite); the laptop becomes a console client, and hails can reach the captain by push/Slack with replies that unblock directly. To verify: how that machine acts as the captain for beams (beams are single-user), and how a new bridge process takes over unfinished workflows |
| **Local transporters** | Run crew without a Beams tenant (added 2026-11-15, epic #164). Step 1 is a container provider over Docker or Podman, which also works on macOS (#165); step 2 is a Firecracker provider on Linux with KVM, for VM isolation (#166). Both sit behind `transporter.Provider`, share one crew image, and reach inference through a host-side proxy that adds the captain's API key, so no key enters the crew. Egress is enforced per container network or tap, from the same `CreateSpec.Egress` as Beams. TTL is the bridge's own timer, and the tsh login gate stays dormant |

---

## 10. Risks

| Risk | Likelihood | Impact | Mitigation |
|---|---|---|---|
| Beta limits or `tsh` output change | Medium | Medium | JSON output only, version check at startup, transporter interface, API path in Phase 4 |
| Session resume across beams doesn't work | Medium | Medium | S3 decides early; handover-note + diff fallback |
| Full-permission delegation lets an agent touch real infra | Medium | High | Low-privilege bridge user, Regulations, in-beam hooks, scope beam permissions as soon as Beams allows |
| Prompt injection via hails steers Command or the captain | Medium | High | Deterministic Regulations, read-only consults, raw context in the console |
| Laptop asleep stalls hails for hours | High | Low/Med | Accepted for v1 (laptop assumed up); agents continue until blocked; always-on bridge on another machine later |
| Inference cap exhausted by a runaway crew member | Medium | Medium | Per-order budgets, usage in reports, condition yellow near cap |
| Builds too slow on 2 vCPU for core | High | Medium | Warmed caches in the image; route heavy builds to fewer, longer missions |
| SQLite corruption or loss | Low | High | WAL, single process lock, nightly `VACUUM INTO`, cargo bay is plain files |
| DBOS v1 is new | Medium | Low | Pin version, keep workflow code in one package, go-workflows as a known fallback |

---

## 11. Open questions

The first is the most consequential: **is Command's placement in a beam the right default**, or do we wait until the bridge can reach the LLM proxy so Command can run on it? The recommendation is "beam now, keep the interface portable", but it costs one beam out of twenty.

Second, **should other Command-division crew (e.g. Helm) hail the captain directly, or the designated First Officer?** The rule as stated sends Command's questions to the captain; with more than one Command crew member, a single officer of the watch answering the others is simpler.

Third, **what should an unanswered hail do by default** — hold, belay, or take a pre-declared default? This probably varies by alert level.

Fourth, **does a red alert preempt?** Today it only jumps the pool queue. Recalling a routine mission to free a crew member for a red alert is easy to build but changes the feel of the ship.

Fifth, **where does BMUS live?** A separate repo consuming the Apache-2.0 `api` module seems right.

---

## 12. Proposed repository layout

```
bmus/
  cmd/bmus/             single binary: daemon, console, CLI
  internal/bridge/      DBOS workflows: crew, mission, hail, dispatch, watch, reconcile
  internal/transporter/ Provider interface + tsh, api, holodeck implementations
  internal/comms/       protocol codec, per-beam pumps, dedup, acks
  internal/regs/        Regulations engine and tests
  internal/store/       domain tables and migrations
  internal/console/     HTTP API, SSE, auth, embedded assets
  web/                  console UI (from the BMUS mock)
  combadge/             TypeScript in-beam daemon (Agent SDK harness, outbox, tools)
  image/                custom crew image (combadge, mirror, warmed caches)
  protocol/             JSON Schema + golden files shared by both sides
```

---

## References

Beams overview, commands and limits — https://goteleport.com/docs/beams/
Long-running agents with `beamctl` — https://goteleport.com/docs/beams/long-running-agents/
Beams protos — `api/proto/teleport/beams/v1/` in https://github.com/gravitational/teleport
DBOS Go v1.0.0 release — https://github.com/dbos-inc/dbos-transact-golang/releases/tag/v1.0.0
DBOS Go workflow communication — https://docs.dbos.dev/golang/tutorials/workflow-communication
go-workflows — https://github.com/cschleiden/go-workflows
Restate concepts — https://docs.restate.dev/foundations/key-concepts
Inngest self-hosting — https://www.inngest.com/docs/self-hosting
durabletask-go — https://github.com/microsoft/durabletask-go
Claude Code headless mode — https://docs.claude.com/en/docs/claude-code/headless
Claude Agent SDK, approvals and user input — https://code.claude.com/docs/en/agent-sdk/user-input
