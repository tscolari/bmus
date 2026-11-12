// #145 spike: hail parking against the real Agent SDK.
//
// Throwaway: not shipped, not built, not run in CI. Run it inside a beam the
// captain created (spike.sh does that), where ANTHROPIC_BASE_URL and
// ANTHROPIC_API_KEY point at the Teleport LLM gateway.
//
// Each scenario runs in a fresh temp git repo:
//   1. query() with combadge's options (bypassPermissions plus PreToolUse
//      hooks, streaming input, an AbortController). A PreToolUse hook asks
//      for the spike's Bash command, so it reaches canUseTool, as a
//      Regulations-style "ask" would in Phase 3.
//   2. canUseTool holds that Bash call open, never resolved from our side.
//   3. The query is aborted the way belay does it today (abortController.abort()),
//      or with q.interrupt() when SPIKE_STOP=interrupt.
//   4. query({resume: sessionId}) with the captain's answer as the first user turn:
//        A: "...: allow"   B: combadge's stand_down text   C: "...: deny"
//   5. Every canUseTool call on the resumed query is recorded and compared
//      with the held one.
//
// Output: spike-results.jsonl (one JSON object per line) and a summary on
// stdout, ending with one PASS/FAIL/OBSERVED line per question of #145.
//
// Knobs (all optional):
//   SPIKE_DRY=1               load the SDK and build every options object; no API calls
//   SPIKE_SCENARIOS=A,B,C     which scenarios to run (default all three)
//   SPIKE_STOP=abort|interrupt  how the first query is ended (default abort, as belay)
//   SPIKE_HOLD_MS=3000        how long canUseTool is held before the stop
//   SPIKE_STEP_TIMEOUT_MS=240000  per-step timeout
//   SPIKE_MODEL=<model>       passed to query() as model (default: the SDK's)
//   SPIKE_CLAUDE_PATH=<path>  claude binary (default: `claude` on PATH, as combadge; else the SDK's own)
//   SPIKE_PERMISSION_MODE=bypassPermissions|default  (default bypassPermissions, as combadge). If the
//                             hook's "ask" doesn't reach canUseTool under bypassPermissions (the command
//                             runs unasked), that is recorded as Q0 and every scenario is rerun with
//                             permissionMode "default", where Bash asks canUseTool by itself.
//   SPIKE_RESULTS=<file>      results file, appended to (default ./spike-results-<UTC time>-<pid>.jsonl,
//                             unique per run, so a run tsh retries doesn't overwrite the first one)
//   SPIKE_MAX_TURNS=8         maxTurns on each query() (default 12 for scenario B, which needs a few more)
//   SPIKE_MAX_BUDGET_USD=0.5  maxBudgetUsd on each query()
//
// It never prints environment variables or keys: the header records only
// whether ANTHROPIC_BASE_URL / ANTHROPIC_API_KEY are set.

import { execFileSync } from "node:child_process";
import { randomBytes } from "node:crypto";
import * as fs from "node:fs";
import * as os from "node:os";
import * as path from "node:path";
import { fileURLToPath } from "node:url";
import { isDeepStrictEqual } from "node:util";
import { createSdkMcpServer, query, tool } from "@anthropic-ai/claude-agent-sdk";
import { z } from "zod";

const here = path.dirname(fileURLToPath(import.meta.url));
const env = process.env;
const DRY = env.SPIKE_DRY === "1";
const SCENARIOS = (env.SPIKE_SCENARIOS ?? "A,B,C").split(",").map((s) => s.trim().toUpperCase()).filter(Boolean);
const STOP = env.SPIKE_STOP === "interrupt" ? "interrupt" : "abort";
const HOLD_MS = num(env.SPIKE_HOLD_MS, 3_000);
const STEP_MS = num(env.SPIKE_STEP_TIMEOUT_MS, 240_000);
const MODEL = env.SPIKE_MODEL || undefined;
let permissionMode = env.SPIKE_PERMISSION_MODE === "default" ? "default" : "bypassPermissions";
const RUN_ID = `${new Date().toISOString().replace(/[-:]/g, "").replace(/\.\d+Z$/, "Z")}-${process.pid}`;
const RESULTS = path.resolve(env.SPIKE_RESULTS || `spike-results-${RUN_ID}.jsonl`);
const MAX_TURNS = num(env.SPIKE_MAX_TURNS, 8);
/** B's resume re-issues (denied), runs git, commits, hands over and answers: about 6-7 turns. */
const MAX_TURNS_B = num(env.SPIKE_MAX_TURNS, 12);
const MAX_BUDGET_USD = num(env.SPIKE_MAX_BUDGET_USD, 0.5);
const LAST_N = 40;
const TRUNC = 500;
/** Marks the spike's own Bash command; the ask hook only asks for commands holding it. */
const MARK = "hail-parking-";

// Mirrors combadge/src/harness/sdk.ts.
const MCP_SERVER = "combadge";
const ALLOWED_TOOLS = ["progress", "report", "request_handover"].map((t) => `mcp__${MCP_SERVER}__${t}`);

function num(v, dflt) {
  const n = Number(v);
  return v !== undefined && v !== "" && Number.isFinite(n) && n >= 0 ? n : dflt;
}

// ---- output ----

const t0 = Date.now();
if (!DRY) fs.appendFileSync(RESULTS, ""); // never truncated: a retried run appends, its header marks where

/** Appends one record to the results file (not in dry mode). */
function log(rec) {
  const line = JSON.stringify({ at: new Date().toISOString(), t_ms: Date.now() - t0, ...rec }, replacer);
  if (!DRY) fs.appendFileSync(RESULTS, line + "\n");
}

function replacer(_k, v) {
  if (typeof v === "function") return "[function]";
  if (v instanceof Error) return errInfo(v);
  if (typeof v === "bigint") return String(v);
  return v;
}

function say(...lines) {
  for (const l of lines) console.log(l);
}

function trunc(s, n = TRUNC) {
  const str = typeof s === "string" ? s : JSON.stringify(s) ?? String(s);
  return str.length > n ? `${str.slice(0, n)}...[${str.length - n} more]` : str;
}

function errInfo(e) {
  if (e instanceof Error) return { name: e.name, message: e.message, ...(e.cause !== undefined && { cause: String(e.cause) }) };
  return { name: typeof e, message: String(e) };
}

// Our held promises must not crash the run; record anything stray.
const unhandled = [];
process.on("unhandledRejection", (reason) => {
  unhandled.push(errInfo(reason));
  log({ kind: "unhandled_rejection", error: errInfo(reason) });
});

// ---- helpers ----

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

/** Resolves {ok: true, value} or {ok: false, timeout: true} after ms; never rejects (errors become {ok:false, error}). */
function within(p, ms) {
  let timer;
  return Promise.race([
    p.then(
      (value) => ({ ok: true, value }),
      (error) => ({ ok: false, error }),
    ),
    new Promise((r) => (timer = setTimeout(() => r({ ok: false, timeout: true }), ms))),
  ]).finally(() => clearTimeout(timer));
}

function deferred() {
  let resolve;
  const promise = new Promise((r) => (resolve = r));
  return { promise, resolve };
}

/** combadge's Input: a streaming prompt, the first message then whatever is pushed, until closed. */
class Input {
  queue = [];
  wake;
  closed = false;
  constructor(first) {
    this.push(first);
  }
  push(text) {
    if (this.closed) return;
    this.queue.push({ type: "user", message: { role: "user", content: text }, parent_tool_use_id: null });
    this.wake?.();
  }
  close() {
    this.closed = true;
    this.wake?.();
  }
  async *[Symbol.asyncIterator]() {
    for (;;) {
      const m = this.queue.shift();
      if (m) {
        yield m;
        continue;
      }
      if (this.closed) return;
      await new Promise((r) => (this.wake = r));
      this.wake = undefined;
    }
  }
}

function findOnPath(name) {
  for (const dir of (env.PATH ?? "").split(path.delimiter)) {
    if (!dir) continue;
    const p = path.resolve(dir, name);
    try {
      fs.accessSync(p, fs.constants.X_OK);
      if (fs.statSync(p).isFile()) return p;
    } catch {}
  }
  return undefined;
}

function run(cmd, args, opts = {}) {
  try {
    return { ok: true, out: execFileSync(cmd, args, { encoding: "utf8", timeout: 15_000, stdio: ["ignore", "pipe", "pipe"], ...opts }).trim() };
  } catch (e) {
    return { ok: false, error: trunc(e.stderr?.toString() || e.message, 300) };
  }
}

/** Summary of one SDK message: type and subtype, plus tool_use/tool_result/text blocks truncated. */
function summarize(m, t) {
  const s = { t_ms: t, type: m.type, ...(m.subtype !== undefined && { subtype: m.subtype }) };
  if (m.type === "assistant" || m.type === "user") {
    const content = m.message?.content;
    if (typeof content === "string") s.text = trunc(content);
    else if (Array.isArray(content)) {
      s.blocks = content
        .map((b) => {
          if (b.type === "tool_use") return { type: "tool_use", id: b.id, name: b.name, input: trunc(b.input) };
          if (b.type === "tool_result") return { type: "tool_result", tool_use_id: b.tool_use_id, is_error: b.is_error, content: trunc(b.content) };
          if (b.type === "text") return { type: "text", text: trunc(b.text) };
          return { type: b.type };
        });
    }
    if (m.isSynthetic) s.isSynthetic = true;
  } else if (m.type === "result") {
    Object.assign(s, {
      is_error: m.is_error,
      num_turns: m.num_turns,
      duration_ms: m.duration_ms,
      total_cost_usd: m.total_cost_usd,
      ...(typeof m.result === "string" && { result: trunc(m.result) }),
      ...(Array.isArray(m.errors) && { errors: m.errors.map((e) => trunc(e, 300)) }),
    });
  } else if (m.type === "system" && m.subtype === "init") {
    Object.assign(s, { session_id: m.session_id, model: m.model, claude_code_version: m.claude_code_version, permissionMode: m.permissionMode, apiKeySource: m.apiKeySource });
  }
  return s;
}

/** The last n entries of a session transcript, summarized like SDK messages. */
function transcriptTail(file, n = 12) {
  if (!file) return { found: false, reason: "no transcript_path from hooks" };
  let raw;
  try {
    raw = fs.readFileSync(file, "utf8");
  } catch (e) {
    return { found: false, reason: e.code ?? e.message };
  }
  const lines = raw.split("\n").filter(Boolean);
  const tail = lines.slice(-n).map((l) => {
    try {
      const e = JSON.parse(l);
      return { ...summarize(e, undefined), ...(e.toolUseResult !== undefined && { toolUseResult: trunc(e.toolUseResult, 300) }) };
    } catch {
      return { unparsed: trunc(l, 200) };
    }
  });
  return { found: true, entries: lines.length, tail };
}

// ---- scenarios ----

const KINDS = {
  A: { kind: "allow", title: "allow on resume" },
  B: { kind: "stand_down", title: "stand_down on resume" },
  C: { kind: "deny", title: "deny on resume" },
};

/** combadge's stopRequest(order, "stand_down", reason), verbatim apart from the order id. */
function standDownText(orderId) {
  const reason = "spike #145: watch change while a hail is parked";
  return [
    `This beam is standing down (${reason}). Stop the work at the nearest safe point: commit what you have on bmus/${orderId},`,
    `then call mcp__${MCP_SERVER}__request_handover with a note for whoever continues it (what is done, what is left, anything surprising).`,
    "Don't call report and don't start anything new; the session ends once the checkpoint is written.",
  ].join(" ");
}

/** combadge's HailWithdrawn message, what a stand_down denies a waiting or new hail with. */
function withdrawnText(orderId) {
  return `standing down: no answer is coming. Commit your work on bmus/${orderId} and call mcp__${MCP_SERVER}__request_handover.`;
}

/** A trimmed copy of combadge's instructions(order, checkout). */
function instructionsText(dir, orderId) {
  return [
    "You are a crew member working on an order from the captain, who is not watching this session.",
    `Work in ${dir}, on branch bmus/${orderId}. Commit your work on it. Don't push: the bridge takes your commits from here.`,
    `Report progress with mcp__${MCP_SERVER}__progress at meaningful milestones.`,
    "If you need a decision from the captain, ask with AskUserQuestion; the answer may take a while.",
    `When the work is finished and committed, call mcp__${MCP_SERVER}__report with a summary and any artifact paths, then stop.`,
  ].join("\n");
}

function setupRepo(kind, orderId) {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), `hail-parking-${kind}-`));
  const git = (...args) => run("git", args, { cwd: dir });
  const steps = [
    git("init", "-q", "-b", `bmus/${orderId}`),
    git("config", "user.name", "bmus spike"),
    git("config", "user.email", "spike@bmus.invalid"),
    git("commit", "-q", "--allow-empty", "-m", "spike: start"),
  ];
  const failed = steps.find((s) => !s.ok);
  return { dir, git: failed ? { ok: false, error: failed.error } : { ok: true } };
}

/** marker.txt's lines split into the agent's own runs and the stale resolve's (see staleResolve). */
function markerCounts(dir, token) {
  const lines = markerLines(dir) ?? [];
  return { lines, runs: lines.filter((l) => l.trim() === `${MARK}${token}`).length, stale: lines.filter((l) => l.trim() === `${MARK}${token}-stale`).length };
}

/** Which top-level keys of two tool inputs differ. */
function differingKeys(a, b) {
  const keys = new Set([...Object.keys(a ?? {}), ...Object.keys(b ?? {})]);
  return [...keys].filter((k) => !isDeepStrictEqual(a?.[k], b?.[k])).sort();
}

function markerLines(dir) {
  try {
    return fs.readFileSync(path.join(dir, "marker.txt"), "utf8").split("\n").filter(Boolean);
  } catch {
    return null;
  }
}

/**
 * Builds a scenario's shared state and its options factory. phase is
 * "first" or "resume"; each phase records into its own lists.
 */
function makeScenario(letter, claudePath) {
  const { kind, title } = KINDS[letter];
  const orderId = `spike-${letter.toLowerCase()}`;
  const token = randomBytes(4).toString("hex");
  const codeword = `${["amber", "cobalt", "saffron", "viridian", "magenta"][randomBytes(1)[0] % 5]}-${randomBytes(3).toString("hex")}`;
  const command = `echo ${MARK}${token} >> marker.txt`;
  const sc = {
    letter, kind, title, orderId, token, codeword, command,
    dir: undefined,
    held: undefined, // the first query's held canUseTool call
    heldSeen: deferred(),
    calls: { first: [], resume: [] }, // canUseTool calls
    hooks: { first: [], resume: [] }, // PreToolUse/PostToolUse on Bash and our MCP tools
    mcp: [], // stub combadge tool calls
    transcriptPath: undefined,
  };

  const now = () => Date.now() - t0;

  /** Bound to the phase whose query it was built for, so a late hook from the aborted query isn't counted as the resume's. */
  const recordHook = (phase) => async (input) => {
    if (input.agent_id === undefined && input.transcript_path) sc.transcriptPath = input.transcript_path;
    if (input.tool_name === "Bash" || input.tool_name?.startsWith(`mcp__${MCP_SERVER}__`)) {
      const rec = { t_ms: now(), event: input.hook_event_name, tool: input.tool_name, tool_use_id: input.tool_use_id, input: trunc(input.tool_input), session_id: input.session_id };
      if (input.hook_event_name === "PostToolUse") rec.response = trunc(input.tool_response, 300);
      const cmd = input.tool_input?.command;
      const marked = input.tool_name === "Bash" && typeof cmd === "string" && cmd.includes(MARK);
      if (phase === "first" && marked && input.hook_event_name === "PreToolUse") sc.markedPreToolUse = true;
      if (phase === "first" && marked && input.hook_event_name === "PostToolUse" && !sc.held) {
        sc.ranWithoutAsk = true; // the hook asked, but canUseTool never saw it
        sc.heldSeen.resolve("ran");
      }
      if (phase === "first" && sc.staleResolvedAt !== undefined) rec.afterStaleResolve = true;
      sc.hooks[phase].push(rec);
      log({ kind: "hook", scenario: letter, phase, ...rec });
    }
    return {};
  };
  /** Asks for the spike's own command so it reaches canUseTool; everything else runs under bypassPermissions. */
  const askForMarked = async (input) => {
    const cmd = input.tool_input?.command;
    if (typeof cmd === "string" && cmd.includes(MARK)) {
      return { hookSpecificOutput: { hookEventName: "PreToolUse", permissionDecision: "ask", permissionDecisionReason: "spike: this command goes to the captain" } };
    }
    return {};
  };
  /** combadge's askPolicy. */
  const askPolicy = async () => ({ hookSpecificOutput: { hookEventName: "PreToolUse", permissionDecision: "ask", permissionDecisionReason: "questions go to the captain" } });

  const canUseTool = (phase) => async (toolName, input, opts) => {
    const signal = opts?.signal;
    const rec = {
      phase,
      t_ms: now(),
      toolName,
      input,
      toolUseID: opts?.toolUseID,
      requestId: opts?.requestId,
      decisionReason: opts?.decisionReason,
      optionKeys: Object.keys(opts ?? {}).sort(),
      signal: { passed: signal instanceof AbortSignal, abortedOnEntry: signal?.aborted ?? null, fired: false },
    };
    if (signal instanceof AbortSignal) {
      signal.addEventListener("abort", () => {
        rec.signal.fired = true;
        rec.signal.firedAt_ms = now();
        rec.signal.reason = errInfo(signal.reason);
        log({ kind: "canUseTool_signal_fired", scenario: letter, phase, toolUseID: rec.toolUseID, reason: rec.signal.reason });
      }, { once: true });
    }
    sc.calls[phase].push(rec);
    log({ kind: "canUseTool", scenario: letter, ...rec, signal: { ...rec.signal } });

    const marked = toolName === "Bash" && typeof input?.command === "string" && input.command.includes(MARK);
    if (toolName === "AskUserQuestion") {
      return { behavior: "deny", message: "No questions in this spike: run the command you were given." };
    }
    if (phase === "first") {
      if (marked && !sc.held) {
        sc.held = rec;
        sc.heldSeen.resolve("held");
        // Held open. Only scenario A ever resolves it, after the resume has
        // started, to test the plan's first draft (see staleResolve).
        return new Promise((resolve) => (sc.heldResolve = resolve));
      }
      return { behavior: "deny", message: "spike: only the requested command may run" };
    }
    // resume: anything but the spike's command runs (under bypassPermissions it never gets here)
    if (!marked) return { behavior: "allow", updatedInput: input };
    if (kind === "allow") return { behavior: "allow", updatedInput: input };
    if (kind === "stand_down") return { behavior: "deny", message: withdrawnText(orderId) };
    return { behavior: "deny", message: "denied by the captain" };
  };

  const text = (t, isError = false) => ({ content: [{ type: "text", text: t }], ...(isError && { isError }) });
  const server = (phase) =>
    createSdkMcpServer({
      name: MCP_SERVER,
      tools: [
        tool("progress", "Tell the captain how far along the order is. pct is 0-100; note is one short line.", { pct: z.number().min(0).max(100), note: z.string() }, async (a) => {
          sc.mcp.push({ phase, tool: "progress", args: a });
          return text("progress recorded");
        }),
        tool("report", "Record the order's final report: a summary of what was done and any artifact paths. Call it once, when the work is finished and committed, then stop.", { summary: z.string(), artifacts: z.array(z.string()).optional() }, async (a) => {
          sc.mcp.push({ phase, tool: "report", args: { summary: trunc(a.summary), artifacts: a.artifacts } });
          return text("report recorded; finish now");
        }),
        tool("request_handover", `Checkpoint the work for whoever continues it: bundles the commits on branch bmus/${orderId} (commit there first; uncommitted changes and other branches are not included), copies this session's transcript and stores the note.`, { note: z.string() }, async (a) => {
          sc.mcp.push({ phase, tool: "request_handover", args: { note: trunc(a.note) } });
          return text("checkpoint written (spike stub: nothing was bundled)");
        }),
      ],
    });

  /** combadge's run() options, with the spike's ask hook and recording hooks. */
  sc.options = (phase, abortController, extra = {}) => ({
    cwd: sc.dir,
    ...(claudePath && { pathToClaudeCodeExecutable: claudePath }),
    permissionMode,
    ...(permissionMode === "bypassPermissions" && { allowDangerouslySkipPermissions: true }),
    abortController,
    systemPrompt: { type: "preset", preset: "claude_code", append: instructionsText(sc.dir, orderId) },
    hooks: {
      PreToolUse: [
        { hooks: [recordHook(phase)] },
        { matcher: "Bash", hooks: [askForMarked] },
        { matcher: "AskUserQuestion", hooks: [askPolicy] },
      ],
      PostToolUse: [{ hooks: [recordHook(phase)] }],
    },
    canUseTool: canUseTool(phase),
    mcpServers: { [MCP_SERVER]: server(phase) },
    allowedTools: ALLOWED_TOOLS,
    // Spend caps, not in combadge's options; the per-step timeouts still apply.
    maxTurns: kind === "stand_down" ? MAX_TURNS_B : MAX_TURNS,
    maxBudgetUsd: MAX_BUDGET_USD,
    ...(MODEL && { model: MODEL }),
    ...extra,
  });

  sc.firstPrompt = [
    `Remember this codeword for later in this conversation: ${codeword}. Don't write it to any file and don't run any command with it.`,
    `Now run exactly this shell command with the Bash tool, verbatim, as one Bash call, in the current directory: ${command}`,
    "Run nothing else. Once it has run, say that it is done.",
  ].join("\n");
  sc.resumeText = {
    allow: `The captain answered your request to run \`${command}\`: allow`,
    deny: `The captain answered your request to run \`${command}\`: deny`,
    stand_down: standDownText(orderId),
  }[kind];
  sc.codewordQuestion = "What was the codeword I gave you at the start of this conversation? Reply with just the codeword.";
  return sc;
}

/**
 * Drives one query to its end, recording every message. onResult(m, n) is
 * called for each result message (n counts from 1).
 */
function drive(label, sc, q, onResult) {
  const started = Date.now();
  const out = { label, messages: [], last: [], sessionIds: new Set(), results: [], texts: [], end: undefined };
  out.done = (async () => {
    try {
      for await (const m of q) {
        const s = summarize(m, Date.now() - t0);
        out.messages.push(s);
        out.last.push(s);
        if (out.last.length > LAST_N) out.last.shift();
        if (typeof m.session_id === "string") out.sessionIds.add(m.session_id);
        if (m.type === "assistant" && Array.isArray(m.message?.content)) {
          for (const b of m.message.content) if (b.type === "text" && b.text.trim()) out.texts.push(b.text);
        }
        log({ kind: "sdk_message", scenario: sc.letter, query: label, ...s });
        if (m.type === "result") {
          out.results.push(s);
          onResult?.(m, out.results.length);
        }
      }
      out.end = { how: "iterator_completed" };
    } catch (e) {
      out.end = { how: "iterator_threw", error: errInfo(e), isAbortError: e?.name === "AbortError" || /abort/i.test(e?.message ?? "") };
    }
    out.end.duration_ms = Date.now() - started;
    return out;
  })();
  return out;
}

/**
 * The plan's first draft: resolve the aborted query's held canUseTool with
 * allow once the resumed query is up. Its updatedInput writes a "-stale"
 * line instead of the agent's, so marker.txt shows whether it ran anything.
 */
async function staleResolve(sc, d2) {
  const deadline = Date.now() + STEP_MS;
  while (d2.sessionIds.size === 0 && d2.end === undefined && Date.now() < deadline) await sleep(100);
  const staleCommand = sc.held.input.command.replace(`${MARK}${sc.token}`, `${MARK}${sc.token}-stale`);
  const r = {
    resumeStarted: d2.sessionIds.size > 0,
    staleCommand,
    markerBefore: markerCounts(sc.dir, sc.token),
    unhandledBefore: unhandled.length,
  };
  sc.staleResolvedAt = r.at_ms = Date.now() - t0;
  try {
    const ret = sc.heldResolve?.({ behavior: "allow", updatedInput: { ...sc.held.input, command: staleCommand } });
    Object.assign(r, { called: typeof sc.heldResolve === "function", threw: false, returned: String(ret) });
  } catch (e) {
    Object.assign(r, { called: true, threw: true, error: errInfo(e) });
  }
  await sleep(3_000);
  r.markerAfter3s = markerCounts(sc.dir, sc.token);
  return r;
}

async function scenario(letter, claudePath) {
  const sc = makeScenario(letter, claudePath);
  const repo = setupRepo(sc.kind, sc.orderId);
  sc.dir = repo.dir;
  const rep = { scenario: letter, scenarioKind: sc.kind, title: sc.title, permissionMode, dir: sc.dir, git: repo.git, command: sc.command, codeword: sc.codeword, stop: STOP, hold_ms: HOLD_MS };
  const t = { start: Date.now() };
  say("", `=== Scenario ${letter}: ${sc.title} ===`, `  dir ${sc.dir}`, `  command: ${sc.command}`);
  log({ kind: "scenario_start", ...rep });

  // ---- first query: hold canUseTool, then stop ----
  const abort1 = new AbortController();
  const input1 = new Input(sc.firstPrompt);
  const q1 = query({ prompt: input1, options: sc.options("first", abort1) });
  const d1 = drive("first", sc, q1, () => input1.close());

  const seen = await within(Promise.race([sc.heldSeen.promise, d1.done.then(() => "ended")]), STEP_MS);
  t.held = Date.now();
  if (!seen.ok || seen.value !== "held") {
    rep.ranWithoutAsk = Boolean(sc.ranWithoutAsk);
    rep.markedPreToolUse = Boolean(sc.markedPreToolUse);
    rep.error = sc.ranWithoutAsk
      ? `the command ran without canUseTool being asked (PreToolUse "ask" under permissionMode ${permissionMode})`
      : seen.ok ? "first query ended before canUseTool was asked for the command" : `no canUseTool for the command within ${STEP_MS}ms`;
    say(`  ! ${rep.error}`);
    abort1.abort();
    input1.close();
    await within(d1.done, 15_000);
    // canUseTool was never reached for the command, though the hook saw it, it ran, or the query just ended:
    // what a PreToolUse "ask" shadowed by the permission mode looks like. main reruns in "default" mode.
    rep.notAsked = rep.ranWithoutAsk || rep.markedPreToolUse || d1.end?.how === "iterator_completed";
    rep.first = { end: d1.end, sessionIds: [...d1.sessionIds], calls: sc.calls.first, hooks: sc.hooks.first, last: d1.last };
    if (d1.end?.error) say(`    first query: ${trunc(`${d1.end.error.name}: ${d1.end.error.message}`, 400)}`);
    log({ kind: "scenario_end", ...rep });
    return { sc, rep };
  }
  say(`  canUseTool(${sc.held.toolName}) held: toolUseID=${sc.held.toolUseID ?? "-"} signal=${sc.held.signal.passed ? "yes" : "no"}`);
  await sleep(HOLD_MS);

  const sessionId = [...d1.sessionIds][0];
  t.stop = Date.now();
  let interruptResult;
  if (STOP === "interrupt") {
    const r = await within(q1.interrupt(), STEP_MS);
    interruptResult = r.ok ? { ok: true, value: trunc(r.value ?? null, 300) } : { ok: false, ...(r.timeout ? { timeout: true } : { error: errInfo(r.error) }) };
    input1.close();
  } else {
    abort1.abort();
    input1.close();
  }
  const ended1 = await within(d1.done, STEP_MS);
  t.firstEnded = Date.now();
  if (!ended1.ok) {
    abort1.abort();
    rep.firstEndTimeout = true;
  }
  await sleep(1_000); // let a late abort signal land
  rep.first = {
    end: d1.end ?? { how: "did_not_end_within_timeout" },
    stop_to_end_ms: t.firstEnded - t.stop,
    ...(interruptResult && { interrupt: interruptResult }),
    sessionIds: [...d1.sessionIds],
    held: { ...sc.held, signal: { ...sc.held.signal } },
    calls: sc.calls.first.map((c) => ({ ...c, signal: { ...c.signal } })),
    hooks: sc.hooks.first,
    mcp: sc.mcp.filter((c) => c.phase === "first"),
    markerAfterFirst: markerLines(sc.dir),
    last: d1.last,
  };
  rep.transcriptAfterFirst = transcriptTail(sc.transcriptPath);
  log({ kind: "first_query_end", scenario: letter, ...rep.first, transcript: rep.transcriptAfterFirst });
  say(`  first query ${rep.first.end.how}${rep.first.end.error ? ` (${rep.first.end.error.name}: ${trunc(rep.first.end.error.message, 160)})` : ""} ${rep.first.stop_to_end_ms}ms after ${STOP}`);
  say(`  held signal fired: ${sc.held.signal.fired ? `yes (${sc.held.signal.reason?.name}: ${trunc(sc.held.signal.reason?.message ?? "", 120)})` : "no"}`);

  if (!sessionId) {
    rep.error = "no session id from the first query; can't resume";
    say(`  ! ${rep.error}`);
    log({ kind: "scenario_end", ...rep });
    return { sc, rep };
  }

  // ---- resumed query ----
  const abort2 = new AbortController();
  const input2 = new Input(sc.resumeText);
  const askCodeword = sc.kind === "allow";
  const q2 = query({ prompt: input2, options: sc.options("resume", abort2, { resume: sessionId }) });
  let textsBeforeCodeword = 0;
  const d2 = drive("resume", sc, q2, (_m, n) => {
    if (askCodeword && n === 1) {
      textsBeforeCodeword = d2.texts.length;
      input2.push(sc.codewordQuestion);
    } else input2.close();
  });
  t.resume = Date.now();
  const stale = sc.kind === "allow" ? staleResolve(sc, d2) : undefined;
  const ended2 = await within(d2.done, STEP_MS * (askCodeword ? 2 : 1));
  if (!ended2.ok) {
    rep.resumeTimeout = true;
    abort2.abort();
    input2.close();
    await within(d2.done, 15_000);
  }
  t.resumeEnded = Date.now();
  if (stale) {
    rep.staleResolve = (await within(stale, 15_000)).value ?? { error: "staleResolve did not finish" };
    const m = markerCounts(sc.dir, sc.token);
    Object.assign(rep.staleResolve, {
      staleLinesAtEnd: m.stale,
      firstPhaseHooksAfterResolve: sc.hooks.first.filter((h) => h.afterStaleResolve),
      unhandledRejectionsSinceResolve: unhandled.slice(rep.staleResolve.unhandledBefore ?? unhandled.length),
    });
    log({ kind: "stale_resolve", scenario: letter, ...rep.staleResolve });
  }

  const resumedBash = sc.calls.resume.filter((c) => c.toolName === "Bash");
  const markerCalls = resumedBash.filter((c) => typeof c.input?.command === "string" && c.input.command.includes(MARK));
  const marker = markerCounts(sc.dir, sc.token);
  const codewordAnswer = askCodeword ? d2.texts.slice(textsBeforeCodeword).join("\n") : undefined;
  rep.resume = {
    prompt: sc.resumeText,
    end: d2.end ?? { how: "did_not_end_within_timeout" },
    results: d2.results.map((r) => ({ subtype: r.subtype, is_error: r.is_error, num_turns: r.num_turns, total_cost_usd: r.total_cost_usd })),
    duration_ms: t.resumeEnded - t.resume,
    sessionIds: [...d2.sessionIds],
    sameSession: d2.sessionIds.size > 0 && [...d2.sessionIds].every((s) => s === sessionId),
    canUseToolCalls: sc.calls.resume.map((c) => ({
      ...c,
      signal: { ...c.signal },
      sameTool: c.toolName === sc.held.toolName,
      inputDeepEqual: isDeepStrictEqual(c.input, sc.held.input),
      sameCommand: c.input?.command === sc.held.input?.command,
      differingKeys: differingKeys(c.input, sc.held.input),
      sameToolUseID: c.toolUseID !== undefined && c.toolUseID === sc.held.toolUseID,
    })),
    reissued: markerCalls.length,
    reissuedIdentical: markerCalls.filter((c) => isDeepStrictEqual(c.input, sc.held.input)).length,
    reissuedSameCommand: markerCalls.filter((c) => c.input?.command === sc.held.input?.command).length,
    hooks: sc.hooks.resume,
    bashRunsSeenByHooks: sc.hooks.resume.filter((h) => h.event === "PostToolUse" && h.tool === "Bash").length,
    mcp: sc.mcp.filter((c) => c.phase === "resume"),
    marker: { exists: markerLines(sc.dir) !== null, lines: marker.lines, runs: marker.runs, staleRuns: marker.stale },
    gitLog: run("git", ["log", "--oneline", "-5"], { cwd: sc.dir }),
    finalText: trunc(d2.texts.at(-1) ?? "", 1_000),
    ...(askCodeword && { codewordAnswer: trunc(codewordAnswer, 500), codewordRecalled: codewordAnswer.includes(sc.codeword) }),
    last: d2.last,
  };
  rep.heldSignalAtEnd = { ...sc.held.signal };
  rep.transcriptAfterResume = transcriptTail(sc.transcriptPath, 20);
  rep.timing_ms = { untilHeld: t.held - t.start, stopToFirstEnd: t.firstEnded - t.stop, resume: t.resumeEnded - t.resume, total: t.resumeEnded - t.start };
  log({ kind: "scenario_end", ...rep });

  say(
    `  resumed: ${rep.resume.end.how}, same session: ${rep.resume.sameSession}, canUseTool calls: ${sc.calls.resume.length} (marked command: ${markerCalls.length}, same command: ${rep.resume.reissuedSameCommand}, deep-equal input: ${rep.resume.reissuedIdentical})`,
    `  marker.txt runs: ${rep.resume.marker.runs}; request_handover calls: ${rep.resume.mcp.filter((c) => c.tool === "request_handover").length}`,
    `  final text: ${trunc(rep.resume.finalText, 300).replace(/\n/g, " ")}`,
  );
  if (rep.staleResolve) say(`  stale resolve of the held canUseTool: threw ${rep.staleResolve.threw}; stale marker lines ${rep.staleResolve.staleLinesAtEnd}; first-query hooks after it ${rep.staleResolve.firstPhaseHooksAfterResolve.length}`);
  if (askCodeword) say(`  codeword answer: ${trunc(codewordAnswer, 200).replace(/\n/g, " ")} (expected ${sc.codeword})`);
  return { sc, rep };
}

// ---- verdicts ----

/** How a resumed query ended, so a cap (error_max_turns, error_max_budget_usd) doesn't read as the agent's doing. */
function resumeEnd(r) {
  const e = r.resume.end;
  const subtypes = r.resume.results.map((x) => `${x.subtype}${x.is_error ? " (is_error)" : ""}`).join(", ") || "none";
  return `resume ended: ${e.how}${e.error ? ` (${trunc(e.error.message, 160)})` : ""}; result subtypes: ${subtypes}${r.resumeTimeout ? "; hit the step timeout" : ""}`;
}

function verdicts(results) {
  const out = [];
  const v = (status, question, detail) => {
    out.push({ status, question, detail });
    log({ kind: "verdict", status, question, detail });
  };
  const by = Object.fromEntries(results.map((r) => [r.rep.scenario, r]));
  const A = by.A?.rep, B = by.B?.rep, C = by.C?.rep;

  if (A?.resume) {
    const calls = A.resume.canUseToolCalls.filter((c) => c.toolName === "Bash");
    const same = A.resume.reissuedSameCommand > 0;
    const diffs = [...new Set(calls.flatMap((c) => c.differingKeys))];
    v(same ? "PASS" : "FAIL", "Q1 After allow, does the agent re-issue the same tool call (same command)?",
      (same
        ? `yes: ${A.resume.reissued} re-issued call(s), ${A.resume.reissuedSameCommand} with the same command, ${A.resume.reissuedIdentical} with deep-equal input` +
            `${diffs.length ? ` (keys that differ: ${diffs.join(", ")})` : ""}; toolUseID same as the held call: ${calls.some((c) => c.sameToolUseID)}`
        : calls.length
          ? `re-issued with a different command: ${calls.map((c) => trunc(c.input, 200)).join(" | ")} vs held ${trunc(A.first.held.input, 200)}`
          : "no Bash canUseTool call on the resumed query") + `; ${resumeEnd(A)}`);
    v(A.resume.reissuedIdentical > 0 ? "PASS" : "OBSERVED", "Q1a Is the re-issued input deep-equal to the held one (description and all)?",
      `${A.resume.reissuedIdentical} of ${calls.length} deep-equal; differing keys: ${diffs.join(", ") || "none"}`);
    v(A.resume.marker.runs === 1 ? "PASS" : "FAIL", "Q1b After allow, does the command run exactly once?",
      `marker.txt has ${A.resume.marker.runs} line(s) from the agent (plus ${A.resume.marker.staleRuns} from the stale resolve); PostToolUse Bash on resume: ${A.resume.bashRunsSeenByHooks}`);
    v("OBSERVED", "Q1c Is the re-issued call's toolUseID the held one's?",
      `held ${A.first.held.toolUseID ?? "(none)"}; resumed ${A.resume.canUseToolCalls.map((c) => c.toolUseID ?? "(none)").join(", ") || "(no calls)"}`);
  } else v("FAIL", "Q1 After allow, does the agent re-issue the same tool call (same command)?", `scenario A did not reach the resume: ${A?.error ?? "not run"}`);

  for (const r of [A, B, C].filter(Boolean)) {
    if (!r.first?.held) continue;
    const s = r.heldSignalAtEnd ?? r.first.held.signal;
    v("OBSERVED", `Q2 (${r.scenario}) Does the held canUseTool settle when the query is stopped (${r.stop})?`,
      `signal passed: ${s.passed}; fired: ${s.fired}${s.fired ? ` after ${s.firedAt_ms - r.first.held.t_ms}ms held, reason ${s.reason?.name}: ${trunc(s.reason?.message ?? "", 160)}` : ""}; ` +
        `iterator: ${r.first.end.how}${r.first.end.error ? ` (${r.first.end.error.name}: ${trunc(r.first.end.error.message, 160)})` : ""}, ${r.first.stop_to_end_ms}ms after the stop; ` +
        (r.scenario === "A"
          ? "the spike resolves this promise itself only after the resume is up: see Q7"
          : "the spike never resolves this promise, so nothing settles it from the SDK side other than the signal"));
  }

  if (A?.staleResolve) {
    const s = A.staleResolve;
    const effect = s.staleLinesAtEnd > 0 || s.firstPhaseHooksAfterResolve.length > 0;
    v("OBSERVED", "Q7 Does resolving the aborted query's held canUseTool (the plan's first draft) do anything once the resume is up?",
      `${effect ? "YES, it had an effect" : "no visible effect"}: resolver called ${s.called}, threw ${s.threw}${s.error ? ` (${s.error.name}: ${s.error.message})` : ""}; ` +
        `"-stale" marker lines ${s.markerAfter3s?.stale ?? "?"} after 3s, ${s.staleLinesAtEnd} at the end; first-query hooks after it ${s.firstPhaseHooksAfterResolve.length}; ` +
        `unhandled rejections since ${s.unhandledRejectionsSinceResolve.length}; resume had started: ${s.resumeStarted}` +
        (A.firstEndTimeout ? "; WARNING: the first query never ended within the step timeout, so its claude process may still have been alive" : "; the first query had ended before the resolve"));
  } else if (A?.resume) v("OBSERVED", "Q7 Does resolving the aborted query's held canUseTool (the plan's first draft) do anything once the resume is up?", "not run");

  const q3 = "Q3 Is the session's earlier context intact after resume?";
  if (A?.resume) {
    const ok = A.resume.results.filter((x) => x.subtype === "success" && !x.is_error).length;
    const detail = `asked for the codeword ${A.codeword}; answer: ${trunc(A.resume.codewordAnswer ?? "", 200)}; same session id: ${A.resume.sameSession}; ${resumeEnd(A)}`;
    if (A.resume.codewordRecalled) v("PASS", q3, detail);
    else if (ok < 2) v("OBSERVED", q3, `no answer to judge: the resume has ${ok} successful result(s), not the two (command, then codeword) it needs, so a cap, timeout or error ended it first; ${detail}`);
    else v("FAIL", q3, detail);
  } else v("FAIL", q3, `scenario A did not reach the resume: ${A?.error ?? "not run"}`);

  if (B?.resume) {
    const handovers = B.resume.mcp.filter((c) => c.tool === "request_handover").length;
    v("OBSERVED", "Q4 What does a stand_down injected in place of the answer do?",
      `re-issued the command: ${B.resume.reissued} time(s) (denied with HailWithdrawn's text); marker runs: ${B.resume.marker.runs}; request_handover calls: ${handovers}; ` +
        `commits after the spike's start commit: ${B.resume.gitLog.ok ? B.resume.gitLog.out.split("\n").length - 1 : "?"}; final text: ${trunc(B.resume.finalText, 200)}; ${resumeEnd(B)}`);
  } else if (B) v("FAIL", "Q4 What does a stand_down injected in place of the answer do?", `scenario B did not reach the resume: ${B.error ?? "?"}`);

  if (C?.resume) {
    v("OBSERVED", "Q5 (extra) After deny, does the agent ask again?",
      `re-asked: ${C.resume.reissued} time(s); marker runs: ${C.resume.marker.runs}; final text: ${trunc(C.resume.finalText, 200)}`);
  } else if (C) v("FAIL", "Q5 (extra) After deny, does the agent ask again?", `scenario C did not reach the resume: ${C.error ?? "?"}`);

  for (const r of [A, B, C].filter(Boolean)) {
    if (r.resume) v("OBSERVED", `Q6 (${r.scenario}) Does resume keep the session id?`, `first ${r.first.sessionIds.join(",")}; resumed ${r.resume.sessionIds.join(",")}`);
  }
  return out;
}

// ---- main ----

async function main() {
  let sdkVersion;
  try {
    sdkVersion = JSON.parse(fs.readFileSync(path.join(here, "node_modules/@anthropic-ai/claude-agent-sdk/package.json"), "utf8")).version;
  } catch {
    sdkVersion = "(could not read node_modules/@anthropic-ai/claude-agent-sdk/package.json)";
  }
  const claudePath = env.SPIKE_CLAUDE_PATH ? path.resolve(env.SPIKE_CLAUDE_PATH) : findOnPath("claude");
  const claudeVersion = claudePath ? run(claudePath, ["--version"]) : { ok: false, error: "no claude on PATH; the SDK's bundled binary is used" };
  const header = {
    kind: "header",
    issue: 145,
    date: new Date().toISOString(),
    dry: DRY,
    sdkVersion,
    claudePath: claudePath ?? "(SDK default)",
    claudeVersion: claudeVersion.ok ? claudeVersion.out : claudeVersion.error,
    node: process.version,
    platform: `${process.platform}/${process.arch}`,
    host: os.hostname(),
    scenarios: SCENARIOS,
    stop: STOP,
    hold_ms: HOLD_MS,
    step_timeout_ms: STEP_MS,
    model: MODEL ?? "(SDK default)",
    permissionMode,
    runId: RUN_ID,
    results: RESULTS,
    maxTurns: MAX_TURNS,
    maxTurnsB: MAX_TURNS_B,
    maxBudgetUsd: MAX_BUDGET_USD,
    env: { ANTHROPIC_BASE_URL: Boolean(env.ANTHROPIC_BASE_URL), ANTHROPIC_API_KEY: Boolean(env.ANTHROPIC_API_KEY) },
  };
  log(header);
  say(
    "#145 hail-parking spike",
    `  date ${header.date}  node ${header.node}  ${header.platform}`,
    `  agent sdk ${sdkVersion}  claude ${header.claudeVersion} (${header.claudePath})`,
    `  stop=${STOP} hold=${HOLD_MS}ms step-timeout=${STEP_MS}ms model=${header.model} permissionMode=${permissionMode}`,
    `  caps per query: maxTurns=${MAX_TURNS} (B ${MAX_TURNS_B}) maxBudgetUsd=${MAX_BUDGET_USD}  results ${RESULTS}`,
    `  ANTHROPIC_BASE_URL set: ${header.env.ANTHROPIC_BASE_URL}  ANTHROPIC_API_KEY set: ${header.env.ANTHROPIC_API_KEY}`,
  );

  const unknown = SCENARIOS.filter((s) => !KINDS[s]);
  if (unknown.length) throw new Error(`unknown scenario(s) ${unknown.join(",")}; use A, B, C`);

  if (DRY) {
    if (typeof query !== "function") throw new Error("the SDK exports no query()");
    for (const letter of SCENARIOS) {
      const sc = makeScenario(letter, claudePath);
      sc.dir = path.join(os.tmpdir(), `hail-parking-dry-${letter}`);
      const first = sc.options("first", new AbortController());
      const resume = sc.options("resume", new AbortController(), { resume: "00000000-0000-0000-0000-000000000000" });
      for (const o of [first, resume]) {
        if (o.mcpServers[MCP_SERVER]?.type !== "sdk") throw new Error(`scenario ${letter}: createSdkMcpServer did not return an sdk server`);
        if (typeof o.canUseTool !== "function") throw new Error(`scenario ${letter}: canUseTool missing`);
      }
      const show = (o) => JSON.stringify({ ...o, mcpServers: Object.keys(o.mcpServers), abortController: "[AbortController]" }, replacer);
      say("", `[dry] scenario ${letter} (${sc.title})`, `  first options: ${show(first)}`, `  resume options: ${show(resume)}`, `  first prompt: ${JSON.stringify(sc.firstPrompt)}`, `  resume prompt: ${JSON.stringify(sc.resumeText)}`);
    }
    say("", "[dry] SDK loaded and every options object built; no query() was called.");
    return 0;
  }

  const runAll = async () => {
    const results = [];
    for (const letter of SCENARIOS) {
      try {
        const r = await scenario(letter, claudePath);
        results.push(r);
        if (r.rep.notAsked && permissionMode === "bypassPermissions") break; // rerun below in default mode
      } catch (e) {
        say(`  ! scenario ${letter} crashed: ${e?.stack ?? e}`);
        log({ kind: "scenario_crash", scenario: letter, error: errInfo(e), stack: trunc(e?.stack ?? "", 2_000) });
        results.push({ rep: { scenario: letter, error: `crashed: ${errInfo(e).message}` } });
      }
    }
    return results;
  };

  const askedMode = permissionMode;
  let results = await runAll();
  const unasked = results.find((r) => r.rep.notAsked);
  const anyHeld = results.find((r) => r.sc?.held);
  const q0 = {
    question: `Q0 Does a PreToolUse "ask" reach canUseTool under permissionMode ${askedMode}${askedMode === "bypassPermissions" ? " (combadge's setup)" : ""}?`,
    // Only a command the hook saw (or that ran) without reaching canUseTool is a definite no.
    status: unasked && (unasked.rep.ranWithoutAsk || unasked.rep.markedPreToolUse) ? "FAIL" : anyHeld ? "PASS" : "OBSERVED",
    detail: unasked
      ? `in scenario ${unasked.rep.scenario} canUseTool was never called for the command (${unasked.rep.ranWithoutAsk ? "it ran unasked" : unasked.rep.markedPreToolUse ? "PreToolUse saw it, then the query ended" : "the query ended without asking"})`
      : anyHeld
        ? `yes: canUseTool was called for the hooked Bash command (scenario ${anyHeld.rep.scenario})`
        : "unknown: no scenario got as far as the command",
  };
  log({ kind: "verdict", ...q0 });
  if (unasked && askedMode === "bypassPermissions") {
    say("", `!!! ${q0.detail}. Rerunning every scenario with permissionMode "default".`);
    permissionMode = "default";
    log({ kind: "rerun", permissionMode });
    results = await runAll();
  }

  const vs = [q0, ...verdicts(results)];
  say("", `=== Verdicts (permissionMode ${permissionMode}${permissionMode !== askedMode ? `, after a rerun: ${askedMode} never reached canUseTool, see Q0` : ""}) ===`);
  log({ kind: "verdicts_mode", permissionMode, askedMode });
  for (const x of vs) say(`${x.status.padEnd(8)} ${x.question}`, `         ${x.detail}`);
  say("", `results: ${RESULTS}`, `total ${Math.round((Date.now() - t0) / 1000)}s`);
  log({ kind: "done", total_ms: Date.now() - t0 });
  return 0;
}

main().then(
  (code) => process.exit(code),
  (e) => {
    console.error(`spike failed: ${e?.stack ?? e}`);
    log({ kind: "fatal", error: errInfo(e) });
    process.exit(1);
  },
);
