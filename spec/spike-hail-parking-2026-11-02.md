# Spike: resuming a session parked mid-permission

Status: run (2026-11-02, #145). The script is `combadge/spike/hail-parking/`.

## Why

`phase-3-plan-2026-10-29.md` §7 parks a long hail: the harness aborts the agent's `query()` and resumes the session once the captain answers. The plan's first draft resolved `canUseTool`'s held promise after the resume. This spike checks what the Agent SDK does with a held permission call across an abort and a resume.

## Setup

- Agent SDK 0.3.301, the version combadge pins. `claude` 2.1.258 (`/usr/bin/claude`) on the beam, node v24.13.0, linux/x64.
- A beam owned by the captain. The model is the SDK's default, through the beam's LLM gateway (tool use ids are `toolu_bdrk_*`).
- combadge's options: `permissionMode: "bypassPermissions"`, streaming input, and a PreToolUse hook answering `ask` for the spike's command (`echo hail-parking-<token> >> marker.txt`). In scenario A the first prompt also gave the agent a codeword to remember.
- `canUseTool` held the call for 3s. The query was then aborted, the way `belay` aborts it, and `query({resume: sessionId})` was sent one of three messages:
  - A: "The captain answered your request to run <command>: allow"
  - B: combadge's stand_down text
  - C: the same answer with deny
- A single run of each scenario, 54s in all.

## The issue's questions

### Does the agent re-issue the same call, with identical input?

Yes. After allow the resumed agent re-issued the call once with the same command, and the input was deep-equal to the held one, `description` included (Q1, Q1a). The command ran exactly once (Q1b).

### Does the original `canUseTool` promise settle?

No. About 2s after the abort the `signal` passed to the held call fired with an AbortError, and the first query's iterator then threw "Claude Code process aborted by user". The promise itself stayed pending in all three scenarios (Q2). In scenario A, once the resume was up, the spike resolved it with allow and a `-stale` variant of the command (Q7). Nothing ran, the first query fired no hooks, and no error or unhandled rejection followed.

### Is the earlier context intact?

Yes. Asked after the resume, the agent gave back the codeword from the first query, and the session id was the same (Q3).

### What does a stand_down do?

The resumed agent did not re-issue the call. canUseTool wasn't called, the command didn't run and nothing was committed. It called `request_handover` once and ended with "Handover checkpoint written. Stopping here per instructions." (Q4)

## Other findings

- Q0. On every query the SDK printed `CLAUDE_SDK_CAN_USE_TOOL_SHADOWED`: "canUseTool will not be invoked: permissionMode 'bypassPermissions' auto-approves every tool call ... use a PreToolUse hook instead". The warning didn't hold. The hook's `ask` still reached canUseTool, and the spike's fallback rerun under `permissionMode: "default"` wasn't needed.
- Q1c. The re-issued call has a new toolUseID (`toolu_bdrk_01PxXroEWR9FYjXA4ZZtN85q` held, `toolu_bdrk_017jEmEdU8xNVPsh7rnNmoiL` re-issued).
- Q5. After deny the agent didn't ask again. It said the captain had denied the command and stopped.
- Q6. Resume kept the session id in all three scenarios.

## What the transcripts show

The CLI session transcripts, whose tails the results file records after each query, and for A the SDK message stream, show two more things.

The aborted CLI takes one more model turn. At the abort it records the held call's result as an error: "Tool permission request failed: AbortError: Tool permission stream closed before response received". The model then answered inside the dying query. In A it only got as far as thinking. In B and C it re-issued the same Bash call, about 2s after the abort and before the held call's signal fired. That call reached neither the PreToolUse hook nor `canUseTool`. The CLI refused it itself with "The user doesn't want to take this action right now. STOP what you are doing and wait for the user to tell you how to proceed." Nothing ran. The resumed session's transcript keeps that refusal, which the captain never gave, and in B the agent later wrote that the command "was denied by the user both times". Its handover note said the same. In A, where the dying query made no second call, the answer was enough for the agent to run the command.

On resume the CLI adds a user turn of its own, "Continue from where you left off.", ahead of the first message the harness sends. An assistant entry, "No response requested.", follows it in all three scenarios, word for word and with no thinking block. That entry never reached the SDK's message stream and isn't counted in the resumed result's `num_turns`. It looks written by the CLI, and the data can't tell whether a model call produced it. The answer is the second user turn of the resumed session.

## Limits

One run on one model, with nothing repeated: every PASS is 1 of 1. The counters covered `canUseTool` and hooks only; the two findings above come from the transcript tails, which the results file cuts to their last entries. The hold was 3s, and only abort was tried (`SPIKE_STOP=interrupt` wasn't run).

## Design

The design these findings support is in `phase-3-plan-2026-10-29.md` §7.
