# Hail-parking SDK spike (#145)

A throwaway script for [#145](https://github.com/tscolari/bmus/issues/145). It is not shipped, not built, and not run in CI. combadge's tsconfig, esbuild entry and vitest config don't include this folder.

It answers the questions that block Phase 3 §7 (`spec/phase-3-plan-2026-10-29.md`) by running the real Agent SDK (the version combadge pins, 0.3.301) inside a beam:

1. `query()` with combadge's options (bypassPermissions, PreToolUse hooks, streaming input), asked to remember a codeword and run `echo hail-parking-<token> >> marker.txt`. A PreToolUse hook answers `ask` for that command, so it reaches `canUseTool`.
2. `canUseTool` holds the call open, and the query is aborted the way `belay` does it.
3. `query({resume: sessionId})` starts with the captain's answer as the first user turn:
   - **A:** `...: allow`
   - **B:** combadge's stand_down text
   - **C:** `...: deny`
4. It records whether the agent re-issues the same call with the same input, whether the held `canUseTool` signal fires, how many times the command ran, and (in A) whether the agent still knows the codeword.

The stdout ends with one PASS/FAIL/OBSERVED line per question. Q0 checks combadge's own assumption that a hook's `ask` reaches `canUseTool` under bypassPermissions. If canUseTool is never called for the command (it runs unasked, or PreToolUse sees it and the query ends, or the query ends without asking), the spike reruns every scenario with `permissionMode: "default"`. SDK 0.3.301 warns `CLAUDE_SDK_CAN_USE_TOOL_SHADOWED` ("canUseTool will not be invoked") whenever bypassPermissions is set, so Q0 may well FAIL on the beam. If it does, the rerun is expected, and it roughly doubles the cost of the cheap first phase.

Scenario A also tests the plan's first draft directly (Q7). Once the resumed query is up, it resolves the aborted query's held `canUseTool` with allow and a `-stale` variant of the command, then records whether anything runs or errors.

## Run

From your own machine, with `tsh` logged in as yourself (the captain), plus `node` and `npm`:

```
./combadge/spike/hail-parking/spike.sh
```

The script:

- creates a beam, or reuses `BEAM`;
- checks the beam has `node`, and that the LLM gateway accepts its key (a 302 means a bot-owned beam, which can't reach the model);
- copies the spike over and runs `npm ci` there (or installs locally for the beam's platform and ships `node_modules` if the beam has no registry);
- runs the spike, copies the results back, and removes the beam it created.

It costs a handful of short model calls per scenario, on the SDK's default model unless `SPIKE_MODEL` says otherwise. Each `query()` is capped at `maxTurns` 8 (12 for scenario B) and `maxBudgetUsd` 0.5 (`SPIKE_MAX_TURNS`, `SPIKE_MAX_BUDGET_USD`).

| Env | Effect |
| --- | --- |
| `BEAM=<id>` | Reuse this beam (it is never removed) instead of creating one. |
| `KEEP_BEAM=1` | Keep the beam the script created. |
| `SPIKE_FORCE=1` | Run even when the gateway check fails. |
| `SPIKE_SCENARIOS=A,B,C` | Pick the scenarios to run. |
| `SPIKE_STOP=interrupt` | End the first query with `q.interrupt()` instead of an abort. |
| `SPIKE_MODEL`, `SPIKE_HOLD_MS`, `SPIKE_STEP_TIMEOUT_MS`, `SPIKE_PERMISSION_MODE`, `SPIKE_CLAUDE_PATH`, `SPIKE_RESULTS`, `SPIKE_MAX_TURNS`, `SPIKE_MAX_BUDGET_USD` | See the header of `spike.mjs`. |

`SPIKE_DRY=1 node spike.mjs`, after `npm ci` here, loads the SDK and builds every options object without calling the API.

## Send back

Both files land next to the script:

- `spike-results-<beam>-<UTC time>.jsonl`: every SDK message, canUseTool call and hook, plus the session transcript's tail. If tsh retried the run after a dropped connection, there are several files, suffixed `-2`, `-3` and so on: send them all.
- `spike-stdout-<beam>-<UTC time>.log`: the run's stdout.

The spike never writes keys or environment variables itself; the header only says whether `ANTHROPIC_BASE_URL` and `ANTHROPIC_API_KEY` are set. The files do hold the hostname, the temp paths, the random codewords, the agent's replies and the tool output it saw, and a stray secret could reach them only through those. Skim them before you share them.

The findings go to `spec/spike-hail-parking-<date>.md` once these are in.
