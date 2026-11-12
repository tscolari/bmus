# Phase 1 plan amendment: groundwork for the console

Two additions to `phase-1-plan-2026-10-16.md`, from `console-plan-2026-10-18.md`. Both are small, both are useful from the CLI today, and both change contracts that get harder to change once the console exists. The console itself stays in Phase 2.

## 1. Every state change writes its log entry in the same transaction (#69)

Today crew status changes (`SetCrewStatus`, `UpdateCrewStatus`, `UpdateCrewStatusIf`) and some order and beam changes commit on their own, with the captain's log entry written by a separate call, or not at all. A crash between the two leaves a change the log never recorded. The console's change feed (the log, replayed by id) would then miss it for good.

- Every store method that changes a row of `crew`, `orders`, `hails` or `beams` takes the `LogEntry` describing the change and appends it in the same transaction, the way `SettleOrder` and `TimeOutOpenHail` already do. A conditional change that touches nothing writes nothing.
- Callers in `crewtour.go`, `mission.go` and `api.go` pass the entry they used to append afterwards. Where no entry was written (crew going away, a beam coming up quietly), they write one.
- A test in `internal/store` checks the rule mechanically. It runs each mutating method against a fresh store and asserts that every changed row comes with exactly one new log entry naming it.

The log's wording stays as it is. Only atomicity and coverage change.

## 2. Bridge status: condition and counters in one call (#70)

The console header shows condition and counters, and `bmus crew list` can't answer "is anything wrong?" in one look either.

- New RPC `bridge.status` returns `{condition, reasons, crew: {on_deck, away, hailing, sickbay}, orders: {open, away}, hails_open, beams_live, last_log_id}`.
- Condition follows the manifest's definition: `red` with anyone in sickbay, `yellow` while anyone is hailing, else `green`. The manifest's other red trigger, an open red-alert order, joins when Phase 2 gives `orders.priority` its alert levels. `reasons` lists what set it, for example `["sorell in sickbay"]`. An expired `tsh` session will add a yellow reason when that check exists (D8), and the field is there for it.
- New command `bmus status` prints it, with `--json` like the other commands.
- The computation lives in one function in `internal/bridge`, which the console's `status` event reuses later.

## Issues

Both are sub-issues of the Phase 1 epic (#1). Neither blocks #19, the real-beam verification. The console's feed (#72) waits on #69, and its panels (#75) on #70.
