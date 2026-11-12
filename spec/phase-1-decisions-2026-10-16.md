# Phase 1 decisions, 2026-10-16

Decisions taken while implementing Phase 1 that change or refine `phase-1-plan-2026-10-16.md`. Each one links to the issue holding the full discussion.

## `tsh beams exec` needs `--` (#7)

The plan's correction in its Context section is wrong. `tsh beams exec` parses flags anywhere on its command line: the root tsh app is `Interspersed(true)` and `beams exec` declares the remote command as a plain positional list. A remote command carrying flags (`combadge attach --since 5`) fails with an unknown-flag error, and an argument matching a tsh global flag such as `--debug` is silently taken by tsh. Running kingpin with tsh's setup confirmed both.

The transporter therefore runs `tsh beams exec <alias> -- <cmd> [args...]`. kingpin consumes the first `--` and passes later ones through, so `beamctl start --name=combadge -- node ...` works behind it.

tsh also joins the remote command with spaces into a single SSH exec string, which the beam's shell parses again. `Exec` shell-quotes every remote argument, giving the tsh and holodeck providers the same semantics. Callers pass absolute remote paths (no `~`).

tsh may run the whole remote command again, up to 10 times, after a connection problem. Anything started through `Exec` must tolerate a second run, `beamctl start` at provisioning included (#15).

## Transporter interface (#7)

`Provider` gains `CopyTo` and `CopyFrom`. Provisioning uploads `combadge.mjs` and the Mission pulls checkpoints, and both need to work against holodeck. tsh implements them with `tsh beams scp`; holodeck with plain local copies (see #9 below).

`Provider` also gains `StartCombadge` and `AttachCombadge` (#9), so CrewTour and the pump never branch on the provider. `StartCombadge` is idempotent and returns nil when serve already runs.
- **tsh:** uploads the bundle to `/tmp/bmus/combadge.mjs` and runs `beamctl start --name=combadge -- node /tmp/bmus/combadge.mjs serve ...`. A combadge that `beamctl list` shows as running counts as started. A dead one (stopped, exited, failed) is restarted, and if that fails it is pruned and started again, because beam-init refuses a duplicate name. Any other state is an error and nothing is touched. It doesn't wait for the socket, so the first attach can fail into the pump's backoff. #19 checks `/tmp/bmus` and `node`'s availability on a real beam.
- **holodeck:** starts serve detached in its own process group, under `<root>/<alias>/`, records its pid in a pid file, reaps it, and waits until the socket accepts. A `start.lock` flock serialises starts across bridge processes. Probe reads `/proc/<pid>/stat`, so a zombie counts as dead, and only counts a pid whose command line names this beam's socket, which rules out pid reuse. Without `/proc` it falls back to `kill(pid, 0)`. Copies are identity-mapped local copies, because serve reports real absolute paths. `Remove` deletes the beam dir and the checkpoint files in it, so the Mission pulls checkpoints before a beam is removed.

`Probe` separates "gone" from "unknown". A beam missing from `ls`, failing `exec true`, or whose combadge isn't `running` in `beamctl list` (tsh) is unhealthy (`false, nil`). A failing `ls`, for example on an expired login, is an error, and the sickbay check doesn't act on errors.

`Provider` also gains `Exists` (#15). It reports only whether the beam is still there: `tsh beams ls` lists the alias, or the holodeck beam directory exists. Provisioning uses it to tell a beam to restart serve in from a beam to replace.

## Store writes survive DBOS step replay (#5)

DBOS re-runs a step whose side effect committed before its output was recorded. Every store write a workflow step calls is safe to repeat:

- `ClaimOldestDirectedOrder` returns the crew member's current `away` order before claiming a new one. Every mission exit must therefore move its order out of `away` (#15, #16).
- `InsertHail` returns the order's open hail if there is one. Phase 1 holds the agent while a hail is open, so an order has at most one.
- Deciding or timing out a hail twice with the same arguments is a no-op.

Section 7 has no column for a report or failure reason. They go in the log entry's `payload`.

## Protocol (#6)

- The bridge assigns hail ids. A beam's `hail` carries its own `ref` instead, and the `answer` echoes `ref` with the `hail_id`.
- A `tool_approval` hail offers `["allow","deny"]`, and answer option 0 means allow. A `clarifying_question` hail asks one question; `AskUserQuestion` with several questions becomes several hails, one after another.
- Beam seq starts at 1 and stays monotonic for the life of the combadge outbox file. Bridge-side seq is the command id. A new beam restarting its seq at 1 would collide with inbox dedup; Phase 1 never starts a second beam for a tour, and #22 tracks the Phase 2 fix.
- `checkpoint_ready` carries the bundled `commit`.
- The schema binds each `type` to its body with `if`/`then` rather than `oneOf`, which keeps validation errors to the message's own type. It compiles under ajv strict mode.
- `order` carries no Regulations excerpt until Phase 3.

## DBOS wiring (#8)

Checked against the source of `dbos-transact-golang` v1.7.0.

- **ApplicationVersion is pinned.** DBOS only recovers PENDING workflows whose recorded version matches the running one. By default it derives the version from a SHA-256 of the executable plus the app name, so every rebuild of `bmus` would orphan in-flight tours and missions. `internal/bridge` sets `ApplicationVersion` to the constant `workflowVersion` ("1"). Bump it only when workflow code changes incompatibly. The `DBOS__APPVERSION` environment variable overrides the config and must stay unset.
- **Workflow names are pinned.** The workflows are registered as `crew-tour` and `mission` with `WithWorkflowName`. DBOS records the name in each workflow's status row and looks it up at recovery, and without an explicit name it would use the Go function's fully qualified name. Renaming a function or moving the module would then strand every PENDING workflow without any error. These names are as permanent as `AppName`.
- **Recovery also filters on executor ID.** That ID is `"local"` by default, so `DBOS__VMID` must stay unset. A Conductor key must not be set either: with Conductor configured, DBOS hands recovery to Conductor and stops recovering on its own.
- **Bridge databases made before the first release are disposable.** #15 and #16 change the stub workflows' step sequences without bumping `workflowVersion`. Versioning starts with the first release.
- **The DBOS root context is detached from the daemon's context.** If the context passed to `dbos.NewContext` is cancelled by anything other than `dbos.Shutdown`, DBOS durably marks every running workflow CANCELLED, and recovery never resumes them. `Run` passes `context.WithoutCancel(ctx)` and stops DBOS only through `Shutdown`, which leaves workflows PENDING.
- **DSN.** `sqlite:/abs/path/bridge.db` parses as the plan says, and DBOS rejects `sqlite://...` because it has a host part. It still breaks on a `?`, `#` or `%` in the path. Both DBOS and modernc split the DSN at the first `?`, and DBOS decodes `%XX` in a plain path while the store doesn't, so the two could open different files. The bridge therefore builds a percent-encoded SQLite URI with `store.FileURI` (`file:///abs/...`). The store opens `<uri>?<params>` and DBOS gets `sqlite:<uri>`. DBOS passes an opaque `file:` URL through to the driver unchanged, and SQLite decodes it.
- **Workflows reach the daemon through the context.** The `*Daemon` (store, DBOS context, transporter and, from #14, the pump registry) is a value on the context given to `dbos.NewContext`. DBOS derives every workflow context from it, including workflows resumed by recovery, and every step context too. Code uses `daemonFrom(ctx)`. No package-level state is involved.
- **Recovery goes through a queue.** On `Launch`, DBOS moves this executor's PENDING workflows to ENQUEUED on its internal queue, and the queue runner resumes them shortly after. Every resume increments `recovery_attempts`, and nothing resets it. A Phase 1 tour lives until dismissal or sickbay, so with the default limit of 100 a healthy tour would be dead-lettered silently after 100 bridge restarts. `crew-tour` is therefore registered with `WithMaxRecoveryAttempts(-1)`, and DBOS only enforces the limit when it is greater than 0. Missions keep the default, because they are bounded.
- **Shutdown takes longer than its timeout.** The timeout `dbos.Shutdown` takes (10s in the bridge) applies to each of its roughly six stages separately, so the worst case is about a minute. #17 and any systemd `TimeoutStopSec` need to allow for that.
- **Re-attach is idempotent.** `RunWorkflow` with an existing workflow ID inserts nothing and returns a handle to the existing workflow, whatever its status. Finished tours aren't re-run. If a crew member who isn't dismissed has a tour in ERROR, CANCELLED or MAX_RECOVERY_ATTEMPTS_EXCEEDED, the daemon logs a warning. A crew member added while the daemon runs needs its tour started by the `crew.add` handler (#17).
- **Tour numbers start at 0.** `crew.tour_no` defaults to 0 and is used as stored, so the first tour is `crew/<id>/tour/0`. Whatever starts the next tour increments `tour_no` first.
- **Serialization** is DBOS's default JSON, so workflow inputs, results and messages need exported fields.

## Combadge (#10, #11)

- **The Agent SDK harness uses the beam's preinstalled `claude`.** The SDK starts a native `claude` binary that comes from per-platform optional dependencies, and no bundle can inline it. The sdk harness passes `pathToClaudeCodeExecutable`, pointing at the `claude` beams ship with. This keeps the plan's single-file deploy, provided the pinned SDK version works with that `claude`; #19 checks it on a real beam. If it doesn't, provisioning uploads a matching binary with `CopyTo`. CI still downloads that binary as part of the SDK's optional dependencies (setup-node caches it), because `--omit=optional` would also drop the native builds of TypeScript, esbuild and vitest.
- **The bundle is `dist/combadge.mjs`.** It is ESM rather than the plan's CommonJS, because the SDK is ESM-only and uses `import.meta.url`. A `createRequire` banner covers CommonJS dependencies.
- **`serve` takes `--crew <id>`.** The provisioning command lines change accordingly:
  - holodeck: `node dist/combadge.mjs serve --socket <sock> --crew <id> --harness holodeck --script <abs path> [--step-log <path>] [--work-dir <abs dir>]`
  - tsh: `node <abs>/combadge.mjs serve --socket <abs sock> --crew <id>` (run under `beamctl`)
- **Attach handshake.** Each attach connection opens with a combadge-internal handshake line, `{"since":N}`. `serve` replays entries with seq above N and then appends a `hello`, so seqs on the wire stay ascending. The bridge therefore sees `hello` only after the backlog.
- **Outbox durability.** The outbox is append-only and fsynced before any socket send. After a crash, a torn last line is cut and its seq reused. That line never reached a socket, so reusing its seq keeps the bridge's contiguous ack moving.
- **Command dedup.** `serve` ignores repeated command ids and remembers them in `state.json` next to the outbox. The bridge's command ids must therefore stay unique for the beam's lifetime, bridge restarts included (#14).
- **Order lifecycle.** `serve` runs one order at a time:
  - A second order while one is running gets `error` with code `busy`.
  - A resend of the running order, or of a finished one, is ignored.
  - An order lost to a `serve` restart gets `error` with code `combadge_restarted`, so the Mission doesn't wait out its 24h timeout.
- **A second `serve` on the same outbox exits.** tsh may run provisioning commands twice, and two writers would interleave seqs.

## Holodeck harness (#12)

- **Scripts.** A script is a JSON array of steps, each with an `at_pct` (the terminal step too) and one action: `progress`, `hail`, `request_handover`, `sleep_ms`, then a terminal `report` or `crash`. serve validates it at startup and runs it for every order it receives. The fixtures `default.json`, `crash.json` and `pause.json` under `combadge/test/fixtures/` match the plan's acceptance run, its sickbay scenario and its restart scenario.
- **`crash` kills serve itself.** That is how a beam's agent dying is simulated.
- **Step log.** `--step-log` writes one fsynced NDJSON line per executed step: `{order, index, action, ts}`, plus `decision` on hails. The restart test asserts that no `(order, index)` pair appears twice. Steps never replay across a bridge restart, because serve outlives the bridge. An order lost to a serve restart is reported as `combadge_restarted` and isn't resumed.
- **Checkpoint files are placeholders.** `request_handover` writes fixed-content files to `<work-dir>/<order>/<step index>/`, and `commit` is the sha1 of the bundle file. `--work-dir` defaults to `<outbox dir>/holodeck/`. The bundle isn't a real git bundle, so holodeck tests check that the files exist and match `commit`, and never `git fetch` them.

## Comms pump (#14)

- **Placement.** The pump is `internal/comms/pump.go`. It imports neither `internal/bridge` (bridge imports comms) nor `internal/transporter` (transporter's tests import comms). It reaches the beam through `comms.Link` (Attach, Probe) and hands its output to `comms.Sink` (ToMission, CrewError, HeartbeatMissed). The bridge implements both, over `transporter.Provider` and `dbos.Send`. DBOS stays out of `internal/comms`.
- **Registry.** `Daemon` holds one pump per crew member: `EnsurePump(crewID, beam, tourNo)` (idempotent for the same beam and tour, replaces the pump otherwise), `StopPump`, `Pump`, and `SendCommand(crewID, msg)`, which workflow steps use. Pumps run on a daemon context and stop before DBOS and the store shut down.
- **Pumps after a restart.** CrewTour's provisioning step starts the first pump (#15), but after a restart that step replays from its record and doesn't run. `Run` therefore calls `EnsurePump` after `Launch` for every crew member that is on_deck, away or hailing with a non-empty `current_beam`, for tour `tour_no`.
- **Exactly-once inbound, by one transaction.** The inbox row and the message's effect commit in one SQLite transaction (`store.RecordInbox`). A progress message's effect is a log row. For a message to a workflow, the effect is `dbos.Send(..., dbos.WithSendTransaction(tx))`. DBOS v1.7.0 accepts a caller's `*sql.Tx` for a send made outside a workflow. On SQLite its notifications table has no schema prefix and lives in `bridge.db`, and DBOS uses the same modernc driver, so the store's own transaction works. `TestPumpSinkSendIsAtomicWithInbox` checks a rollback and a commit. The pump acks only after the commit. If the Sink fails, nothing is recorded: the pump drops the connection and the beam replays the message after a reconnect. On SQLite every `Recv` wakes on DBOS's 1s notification poller, DBOS's own sends included, so a workflow sees a crew message up to a second late.
- **Send to a missing workflow fails.** `notifications` references `workflow_status`, so a message for a mission that hasn't started is refused, and the pump keeps retrying it. #15/#16 must start the mission workflow before the beam can answer its order command. A message whose workflow still doesn't exist after a bounded retry is recorded with an `undeliverable` log entry, so one bad message can't stall the crew member's stream.
- **heartbeat_missed** is sent with `WithIdempotencyKey(<tour id>/<beam>/heartbeat_missed)`, at most once per tour and beam, even from a pump restarted by a new bridge. It carries the pump's beam in `CrewSignal.Beam` (#15).
- **Acks.** An ack carries the highest contiguous recorded seq (`store.ContiguousInboxSeq`). Every message is recorded, heartbeats and hello included, so the ack keeps moving. Each attach uses `--since` that value as read from the store. The first ack on each connection is resent, in case the last one was lost.
- **Command ids** are `max(last+1, now in µs)` per pump. They stay unique across bridge restarts unless the wall clock steps back further than the gap since the last command, and they stay below 2^53. A step that DBOS replays sends again under a new id. That is safe for Phase 1's commands: serve ignores a running or finished order, and an answer to a hail ref it isn't waiting on.
- **Send while disconnected** returns `comms.ErrNotConnected`. A session counts as connected from its first inbound line, which proves the attach handshake reached serve. A nil error from Send means the command was written to attach, not that serve received it. There is no redelivery in Phase 1.
- **Step errors aren't retried by default.** `RunAsStep` defaults to `WithStepMaxRetries(0)`: the error is recorded as the step's outcome and returned once, and a workflow that returns it ends in ERROR for good. Steps that send commands should set `dbos.WithStepMaxRetries(n)` with `WithStepBaseInterval`/`WithStepMaxInterval`, or have the step return "not sent" as a value and loop in the workflow with `dbos.Sleep`. Don't branch workflow code on `errors.Is` of a recorded step error. Once exhausted, retries surface as `ErrorCodeMaxStepRetriesExceeded`.
- **Sickbay check.** Freshness is the newest heartbeat's `min(ts, arrival)`. Replayed heartbeats count by their old `ts`, and a beam clock running ahead can't hide a stall. Before the first heartbeat the pump's start time is used. The check probes the beam only once heartbeats are stale (30s, checked every 5s) and the crew member isn't in sickbay. A probe error only logs. A healthy beam with stale heartbeats means the attach connection is stuck, so the pump drops it and reconnects, unless lines are still arriving (see #32 below). tsh's Probe also runs `beamctl list` and reports any combadge state other than `running` as unhealthy, so a crashed or crash-looping serve on a live beam reaches sickbay (#30). The pump signals at most once and keeps running until the tour calls `StopPump`.
- **Stale heartbeats while lines still arrive (#32).** After a long bridge outage the beam replays its backlog, and the replayed heartbeats stay stale by `ts` until the replay catches up, which can take longer than the 30s window. Recycling the connection then would restart the replay every check. The pump therefore tracks the arrival of any inbound line:
  - A healthy probe recycles the connection only if no line at all has arrived for the stale window. This is checked after the probe returns.
  - The probe itself is skipped while the live connection is delivering: it has been live for at least the stale window and its newest line is younger than that. A connection delivering lines is neither stuck nor attached to a dead beam, because serve dying closes the socket and attach exits. The connection-age condition keeps #30 working: a crash-looping serve emits a `hello` on every attach, so lines arrive, but over short connections. Serve's 10s heartbeat interval is well under the 30s window, so no such connection lives long enough to skip the probe. A dead beam delivers no lines and is probed as before.
- **Abandoned connections.** A write that times out marks its connection abandoned before cancelling it. Later writes to that connection, such as the reader's acks for lines read before attach dies, fail at once with `ErrNotConnected`. Without that, each one would hold the write lock for another `WriteTimeout` and stall `Send` on the next connection.
- **An error without an order** is a crew-level error. The pump logs it and sends `CrewSignal{Kind: "crew_error", Reason, Code}` to the tour, never to `order/`.

## CrewTour and Mission (#15, #16)

- **The tour never blocks on a mission.** This corrects plan §7. The tour waits only on `Recv[CrewSignal]("crew-events")`, so a `heartbeat_missed` that arrives mid-mission is seen. While idle it claims an order (`claim-order`: claim, crew `away`, log) and, if it finds one, starts `order/<id>` as a child workflow and records it as running. The wait is 24h when idle and 10 minutes while a mission runs. Signals:
  - `order_assigned`: an idle tour claims at once. A busy tour ignores it, because the claim after the mission picks the order up.
  - `mission_done{OrderID}` for the running order: `settle-mission`, which runs the backstop (fail the order if it is still `away` or `hailing`, logged with code `unsettled`) (changed by #35, see phase-1-decisions-2026-10-17.md) and sets the crew member `on_deck`. Then the tour claims again. A `mission_done` for any other order is stale and ignored.
  - `heartbeat_missed{Beam}`: acted on only when `Beam` is the tour's own beam. The tour then ends in sickbay (see "Ending a tour" below).
  - `dismissed`: the tour ends (see "Ending a tour" below). The crew row's status is left to whoever dismissed the crew member (changed by #35, see phase-1-decisions-2026-10-17.md).
  - `crew_error`: logged (kind `fail`, with reason and code).
  - A wait that times out while a mission runs triggers `mission-alive`, which checks that `order/<id>` is still PENDING or ENQUEUED. A mission that ended without `mission_done` (a panic, its recovery attempts used up, a failed final step) is settled within 10 minutes instead of leaving the crew member `away` for a day.
- **Ending a tour.** On a matching `heartbeat_missed` (code `beam_lost`, reason "beam lost") or on `dismissed` (code `dismissed`):
  - A running mission gets a synthetic `error{reason, code}` on `from-crew`, sent as a durable `dbos.Send`.
  - The tour calls `StopPump`.
  - It waits up to 5 minutes for that mission's `mission_done`. The deadline is recorded in a `settle-deadline` step, so a resumed tour waits only for what is left. The tour never decides to wait again from the clock, only from recorded Recv outcomes.
  - `tour-end` runs the backstop. On sickbay it also sets the crew member `sickbay` and logs `beam` with code `beam_lost`.
  - Dismissal doesn't tell the combadge, so serve keeps running the order until its beam goes (Phase 2's `stand_down` and beam removal).
- **Every mission exit sends `mission_done`.** `missionWorkflow` wraps the body: whatever the body returns, the order is settled (done or failed) and then `mission_done` goes to `MissionInput.TourID`. A failed order is a SUCCESS workflow with `MissionResult{Status:"failed", Reason, Code}`, not a workflow error. The only exception is DBOS shutting down mid-mission: the workflow returns at once and stays PENDING. Sending to a tour that already ended succeeds, because `notifications` only references the workflow row, and the message is never read.
- **Mission steps.** `send-order` builds the `order` command from the orders row as the mission's first step, so the mission exists before the beam can answer. Then the mission loops on `Recv[comms.Message]("from-crew", 24h)`:
  - `hail`: `record-hail` inserts the hail, sets the crew member `hailing` and logs it.
  - `answer`: `decide-hail` confirms the decision with `MarkHailDecided`, which is a no-op because AnswerHail already decided it, sets the crew member `away` and logs it. Then `send-answer` sends the command.
  - `checkpoint_ready`: `pull-checkpoint` creates `<home>/cargo/<order>/<seq>/`, copies `{work.bundle,transcript.jsonl,handover.md}` into it from the crew member's `current_beam` with `Provider.CopyFrom`, and inserts the `checkpoints` row. An `ErrCheckpointConflict` isn't retried. A pull that still fails after retries is logged with code `checkpoint_failed`, and the work goes on.
  - `report`: `order-done` closes any open hail, marks the order done, and puts the report in the `done` log payload.
  - `error`, or a timeout: `order-failed` closes any open hail as `timed_out`, marks the order failed, and logs reason and code in the `fail` payload. An error whose body doesn't decode fails the order with a reason saying so.
  - A Recv that fails to decode is skipped, because its record replays the same way. Any other Recv error is returned.
  - A `combadge_restarted` that follows a `report` can't reach a mission. The mission has already returned on the report, and a message sent to a finished workflow is never read.
- **Hail answers travel on `from-crew`, not a separate `from-hail` topic.** This deviates from plan §8. A mission waiting on a hail therefore still sees `beam_lost`, `dismissed` or a crew error. Otherwise the order would stay `away` until the hail's 24h timeout. `AnswerHail` runs one store transaction (`store.DecideOpenHail`) holding a conditional open → decided update and `dbos.Send(order/<id>, answer, "from-crew", WithSendTransaction(tx))`, the comms pump's pattern. Of two concurrent answers exactly one succeeds and the other gets `ErrHailClosed`, and a decided hail always has its answer queued. The mission ignores an answer whose `hail_id` isn't its open hail, and it fills in `ref` itself.
- **Provisioning is one step, safe to re-run and retry.**
  - A new beam is recorded only in the `beams` table, right after `Create`. `crew.current_beam` and `on_deck` are set only once `StartCombadge` has succeeded. A bridge restarting mid-provisioning therefore never has `ensurePumps` attach a pump to a beam with no serve, and a heartbeat_missed from such a pump can't send the new tour to sickbay.
  - A re-run starts from the crew member's latest `beams` row. If that beam still exists, it is reused and `StartCombadge`, which is idempotent, is called on it again. Existence is checked with the new `Provider.Exists`: tsh checks that `tsh beams ls` lists the alias, holodeck that the beam directory exists. Unlike `Probe`, it ignores whether serve is running.
  - A failed `StartCombadge` fails the attempt, and the next retry tries the same beam. Only a beam that is gone is replaced. Retries therefore don't create a VM each, and a transient failure never destroys a good beam. A beam that serve can't start in uses up the retries (20, from 1s to 1 minute apart), and the crew member goes to sickbay with code `provision_failed`.
  - `beams.uuid` holds the alias, since providers have no other id in Phase 1. `InsertBeam` is a no-op for the same crew member and returns `ErrBeamTaken` for another.
- **`EnsurePump` and `StopPump` aren't steps.** Their effects are process-local and idempotent. The tour calls `EnsurePump` on every run, right after the provisioning step returns, so a replay registers the pump again.
  - `ErrDaemonStopping` is transient: the tour waits for its context to be cancelled and returns, so it stays PENDING. If it returned the error instead, it would be recorded as ERROR, because pumps close before DBOS shuts down.
  - Any other failure is retried 5 times with backoff. After that the crew member goes to sickbay with code `pump_failed`. In practice such a failure is `StartPump` rejecting its config, which is deterministic. A replay whose `EnsurePump` succeeds after an original run that went to sickbay is not handled (changed by #35, see phase-1-decisions-2026-10-17.md).
- **Step errors.** Workflow code checks a step's error only with `err != nil`. It first checks `ctx.Err()`: a cancelled context means a shutdown interrupted the step, nothing was recorded, and the workflow returns. Recv timeouts are matched with `errors.Is(err, dbos.ErrTimeout)`. DBOS gob-encodes its own `*Error`, so that match survives replay. Retry policies:
  - sends through the pump: 15 retries, 0.5s to 15s apart, about 2m45s in all;
  - provisioning: 20 retries, 1s to 1 minute apart;
  - checkpoint copies: 5 retries;
  - store-only steps: 5 retries.

  A send that runs out of retries fails the order with code `send_failed`.
- **Replay safety per write:**
  - The claim, `InsertHail`, the hail confirm and timeout, `InsertBeam` and the status updates are all idempotent (#5).
  - `InsertCheckpoint` is idempotent for an identical row. A different row for the same `(order, seq)` returns `ErrCheckpointConflict`.
  - Cargo files are overwritten atomically.
  - Re-sent commands get new ids, and serve ignores them (#14).
  - Captain's log entries are the one non-idempotent write: a step re-run after a crash can log twice.
- **Captain's log kinds:**
  - `beam`: beam up, crew to sickbay (payload `{beam, reason, code}`).
  - `order`: order created, order away, checkpoint pulled or failed. Progress is logged by the pump.
  - `hail`: opened, decided, closed unanswered.
  - `done`: the payload is the report.
  - `fail`: the payload is `{reason, code}`, used for order failures and crew errors.
- **Daemon API for the RPC handlers (#17):**
  - `AddCrew(ctx, store.Crew)` takes a new crew member with status `on_deck` (or empty) and `TourNo` 0. It returns `ErrExists` for an id already taken, inserts the crew member, and starts `crew/<id>/tour/0`.
  - `CreateOrder(ctx, store.Order) (id, error)` returns `ErrNotFound` for an unknown assignee and `ErrUnavailable` for one in sickbay or dismissed. Otherwise it inserts the order, logs it, and sends `order_assigned` to the assignee's tour. A failed send is only logged.
  - `AnswerHail(ctx, hailID, decision, option *int)` returns `ErrNotFound` for an unknown hail and `ErrHailClosed` for one that isn't open, including when a concurrent answer won. It rejects an option out of range, and an empty decision defaults to the chosen option's text.
- **Config.** `Config.CombadgeArgs` is appended to every `StartCombadge`. Holodeck tests pass `--heartbeat-interval` and `--step-log` through it. The holodeck `--script` stays in `HolodeckConfig.Harness`.
