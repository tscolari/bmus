# Phase 4 decisions, 2026-11-13

Decisions taken while implementing Phase 4 that refine `phase-4-plan-2026-11-12.md`. Each one links to the issue holding the full discussion.

## Schema (#111)

- **Migration.** Follows the schema convention in `phase-1-decisions-2026-10-17.md` (#58). `beams.removed_at`, `beams.labels`, `orders.budget_turns` and `orders.budget_extended` are `addedColumns` entries. `order_usage`, `order_egress` and `backups` are new tables, which an older database gains through `CREATE TABLE IF NOT EXISTS`. Nothing goes in `requiredColumns`, so every existing database opens. `workflowVersion` stays `"1"`.
- **Types.** `order_usage` counts default to 0 and its `order_id` references `orders`. `order_egress.hail_id` references `hails`, as a grant always comes from a hail. `backups.id` is an autoincrement integer, `finished_at` is NULL while a backup runs or after a crash in one, and `error` is empty for a backup that passed its check.
- **removed_at.** `store.SetBeamRemoved(crew, alias, at)` marks every unmarked row of that crew member with that alias. Only one beam holds an alias at a time, so older rows with it are gone too. `removeBeam` calls it when the beam is removed or found gone. A removal that fails leaves the row NULL, which reconcile (plan section 5) reads as a beam that may still exist. `DismissCrew` sets it through `removeBeam`, directly or through the dismissed tour's `remove-beam` step. `BeamRecord.RemovedAt` carries it.
- **Accessors.** Only `SetBeamRemoved` and `RemovedAt`. Each feature issue adds the accessors its tables need.
- **Tests.** `TestOpenAddsPhase4Schema` opens a database made with `testdata/schema-before-111.sql`, a copy of `schema.sql` before this change, twice. Its rows survive, and each changed or new table's columns match a new database's. `TestSetBeamRemoved` covers the store method. The bridge tests for remove-previous, a dismissal that removes the beam itself and a dismissal through the tour check `removed_at`, and a failed removal leaves it NULL.
