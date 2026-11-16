/**
 * holodeck: the scripted stand-in harness (spec D10; plan §5, §11). It runs no
 * agent. It walks a JSON script through the same emit/onInbound surface the
 * sdk harness uses, so the comms code under test is the real one.
 *
 * `serve --harness holodeck --script <path> [--step-log <path>] [--work-dir <path>]`
 *
 * Script format: a JSON array of steps. Every step has `at_pct` (a number
 * 0-100, non-decreasing through the script) and exactly one action key:
 *
 *   {"at_pct": 10, "progress": "note"}
 *       Emits progress {pct: at_pct, note}.
 *   {"at_pct": 30, "usage": {"input_tokens": 1200, "output_tokens": 300, "turns": 4}}
 *       Emits usage with those counts (#117), which stand for the order's
 *       cumulative usage on this beam. Each count is a non-negative integer.
 *       Unlike the sdk harness, nothing checks they never decrease.
 *   {"at_pct": 35, "finding": {"key": "...", "title": "...", "body": "...", ...}}
 *       Emits a finding with that body as given (#153): only its being an
 *       object is checked, so a script can send what the bridge must drop.
 *       No answer comes back, and the walk goes on.
 *   {"at_pct": 40, "hail": {"category": "...", "question": "...", "options": [...], "context": {...}}}
 *       Emits a hail with ref `holodeck-<order>-<index>` (options/context
 *       optional) and waits for the answer echoing that ref. The answer's
 *       decision and decided_by go into the step log. With `"park_after_ms":
 *       N` beside `hail` (#145), a hail still unanswered after N ms parks:
 *       the holodeck writes a placeholder checkpoint into
 *       `<work-dir>/<order>/<index>/`, its note naming the hail and the step
 *       (chained, as every holodeck-written note is, with the agent's latest
 *       request_handover note and the carried answers: chainNote),
 *       emits it with `held: [ref]`, logs a `park` step at the index, and
 *       goes on waiting. An order whose handover note carries a holodeck
 *       park ("The holodeck parked its script at step N") walks from step
 *       N + 1: its hail is the bridge's to carry, and the run raises it no
 *       second time. Its stop notes name that park again (#174).
 *   {"at_pct": 30, "write": "notes.txt"}
 *       In each of the order's repos, appends `<order> step <index>` to that
 *       file (a relative path, created with its dirs) and leaves it
 *       uncommitted: work in progress for the checkpoints to snapshot (#145).
 *   {"at_pct": 50, "commit": "message"}
 *       In each of the order's repos, appends `<order> step <index>: <message>`
 *       to holodeck.txt and commits it with that message on the checked-out
 *       branch (`bmus/<order>`, as serve left it). The step log records each
 *       repo's new commit under `"commits"`. With no repos it only logs.
 *   {"at_pct": 60, "request_handover": "note"}
 *       Writes one git bundle per repo of `bmus/<order>` (`<name>.bundle`, as
 *       the sdk harness does, with the work in progress on
 *       `refs/bmus/wip/<order>` and, after the run's first checkpoint, only
 *       what came after it, beside the full one), plus transcript.jsonl (a placeholder) and
 *       handover.md (the note), into `<work-dir>/<order>/<index>/` (one dir
 *       per step, so a later handover doesn't overwrite an earlier one), then
 *       emits checkpoint_ready with their absolute paths.
 *   {"at_pct": 50, "sleep_ms": 3000}
 *       Waits (at most 2147483647 ms, Node's timer limit).
 *   {"at_pct": 100, "report": {"summary": "...", "artifacts": [...]}}
 *       Terminal. The run resolves with that report, its usage the tokens of
 *       the run's latest usage step, else of the order's earlier runs on this
 *       beam (Order.usage), else zero; serve
 *       bundles each repo's `bmus/<order>` into it and emits it.
 *   {"at_pct": 100, "crash": 137}
 *       Terminal. The whole serve process exits at once with that code (1-255)
 *       and emits nothing. This is the point: it simulates the beam's agent
 *       dying, and serve with it.
 *
 * Budget (#119): before every step but `usage` and `crash`, the holodeck
 * checks the order's budget as the sdk harness does at a tool call: over it
 * when the order's `used_tokens`/`used_turns` plus its own usage (the latest
 * usage step's counts, else Order.usage's) pass `budget_tokens`/`budget_turns`.
 * Over budget it emits a `budget` hail (ref `holodeck-<order>-<index>-budget`,
 * options extend/stop, context {used, limit, turns_used, turns_limit}) and
 * waits. The step log records it as `{"order","index","action":"budget",
 * "ts","decision","decided_by","delivery"}`, with the step's index. extend
 * takes the answer's `budget` as the new limit and runs the step; stop writes
 * a placeholder checkpoint into `<work-dir>/<order>/<script length>/`, emits
 * it as an ordinary checkpoint_ready and rejects the run with BudgetExceeded,
 * which serve emits as an error coded budget_exceeded.
 *
 * The script must end in exactly one terminal step. It is validated when serve
 * starts, not when an order arrives, so a bad script fails fast. The same
 * script runs for every order serve receives.
 *
 * Consults: the file may instead be an object, `{"steps": [...], "consult":
 * <reply>}`, to script Command's consult lane (phase 3 plan §5). Every
 * consult gets the same reply at once: either a consult_reply body without
 * hail_id (`{"decision": "answer" | "escalate", "option"?, "answer"?,
 * "rationale"?, "confidence", "risk_flags"?}`; serve adds the request's
 * hail_id, and a missing rationale is ""), or `"silence"`, which never
 * replies: each silent consult holds one of serve's MAX_CONSULTS slots until
 * serve closes, so past that many serve answers "Command busy". Without
 * `consult` the holodeck runs no consult lane and serve escalates every
 * consult itself. The step log records each consult as it arrives:
 * `{"action": "consult", "ts", "consult": <the consult body>}`.
 *
 * `--work-dir` defaults to `<outbox dir>/holodeck/`. The order's repos are
 * checked out by serve before the walk starts; the holodeck only records
 * their dirs.
 *
 * Inbound: `belay` rejects the run with "belayed: <reason>", like echo, and
 * stops the walk: no later step is emitted or logged, even one whose wait the
 * belay raced (e.g. a hail answer in the same read). `stand_down` and `recall`
 * stop the walk the same way, at once: a hail or sleep is cut short and a
 * commit or handover step under way finishes but isn't logged or emitted.
 * The harness then writes a placeholder checkpoint into
 * `<work-dir>/<order>/<script length>/` (an index no step has), emits it as
 * checkpoint_ready with `stopping: true` and rejects the run with Stopped,
 * after which serve emits nothing. One that arrives after the walk reached its
 * report, or after a belay or an earlier stop, is ignored. An answer whose ref
 * is unknown or already answered is logged and ignored, as is `checkpoint`,
 * which the bridge never sends. A `correction` (a countermand) changes
 * nothing in the walk; the step log records it as `{"order", "action":
 * "correction", "ts", "delivery", "correction": <the correction body>}`.
 *
 * Step log (`--step-log`, optional): one NDJSON line per executed step,
 * appended and fsynced once the step is done (for crash, just before exiting):
 * `{"order","index","action","ts"}` plus `"decision"`, `"decided_by"` and `"delivery"` for a hail, and
 * `"repos"`, each repo's name to its checkout dir, when the order has repos,
 * `"model"`, the order's model, when it has one (the holodeck runs no model and
 * only records it, so tests can see what reached the harness), and
 * `"handover"`, the note a resumed order carries, when it has one,
 * `"grants"`, the calls allowed on its carried hails (#145), when it has any, and
 * `"budget_tokens"`, `"budget_turns"`, `"used_tokens"` and `"used_turns"`,
 * the budget limit and other beams' usage the order carries, when nonzero
 * (#118). The bridge
 * restart test uses it to assert no step ran twice. The harness lives inside
 * serve, which survives a bridge restart, so steps are never replayed by
 * design. If serve itself restarts mid-order the order is lost
 * (`combadge_restarted`), not resumed, so its remaining steps never run.
 *
 * `"delivery"` numbers inbound messages in the order they reached the
 * harness (1, 2, ... across every order this serve runs): a hail's is its
 * answer's, a correction's its own. Lines are not written in that order: a
 * correction is logged as it arrives, a hail only once the walk has taken its
 * answer, so a correction delivered just after the answer (the same read)
 * can come first in the file. Compare `"delivery"` for delivery order.
 */
import * as fs from "node:fs";
import * as path from "node:path";
import { setTimeout as delay } from "node:timers/promises";
import type { AnswerBody, ConsultReplyBody, FindingBody, HailBody, ReportBody, UsageBody } from "../protocol.js";
import { git } from "../repos.js";
import { abortable, Budget, BudgetExceeded, chainNote, orderDir, startedFrom, Stopped, writePlaceholderCheckpoint } from "./common.js";
import type { Harness, HarnessOptions, HarnessReport, Inbound, Order } from "./types.js";

type HailSpec = Omit<HailBody, "ref">;
type ReportSpec = Omit<ReportBody, "usage" | "repos">;
type UsageSpec = UsageBody;

export type Step = { at_pct: number } & (
  | { progress: string }
  | { usage: UsageSpec }
  | { finding: FindingBody }
  | { hail: HailSpec; park_after_ms?: number }
  | { write: string }
  | { commit: string }
  | { request_handover: string }
  | { sleep_ms: number }
  | { report: ReportSpec }
  | { crash: number }
);

/** A holodeck script: the steps every order walks, and the reply every consult gets, if any. */
export interface Script {
  steps: Step[];
  consult?: ConsultSpec;
}

/** An inbound body and its place in delivery order (the step log's "delivery"). */
type Delivered<T> = { body: T; delivery: number };

export type ConsultSpec = (Omit<ConsultReplyBody, "hail_id" | "rationale"> & { rationale?: string }) | "silence";

const ACTIONS = ["progress", "usage", "finding", "hail", "write", "commit", "request_handover", "sleep_ms", "report", "crash"] as const;
/** A holodeck park's mark in its checkpoint's note, which a resumed order's handover carries (#145). */
const PARKED_AT = /The holodeck parked its script at step ([0-9]+)\./;
type Action = (typeof ACTIONS)[number];
const TERMINAL = new Set<Action>(["report", "crash"]);
/** Longest Node timer; anything above it fires after 1ms. */
const MAX_SLEEP_MS = 2_147_483_647;

export function holodeckHarness(opts: HarnessOptions): Harness {
  if (!opts.script) throw new Error("--script is required with --harness holodeck");
  const { steps, consult } = loadScript(opts.script);
  const workDir = path.resolve(opts.workDir);
  const stepLog = opts.stepLog === undefined ? undefined : path.resolve(opts.stepLog);
  let deliveries = 0; // inbound messages so far, for the step log's "delivery"

  return {
    name: "holodeck",
    ...(consult !== undefined && {
      consult: (req, signal) => {
        stepLog && appendStep(stepLog, { action: "consult", ts: new Date().toISOString(), consult: req });
        return consult === "silence"
          ? abortable(new Promise<never>(() => {}), signal)
          : Promise.resolve({ ...consult, hail_id: req.hail_id, rationale: consult.rationale ?? "" });
      },
    }),
    run(order, emit, onInbound) {
      const abort = new AbortController();
      const { signal } = abort;
      const answers = new Map<string, (a: Delivered<AnswerBody>) => void>();
      // The run's outcome when something stops the walk: a belay, or a stop
      // once its checkpoint is out. The first to call halt wins.
      let halt!: (err: Error) => void;
      const halted = new Promise<never>((_, reject) => (halt = reject));
      halted.catch(() => {}); // only observed through the race below
      let haltedBy: "belay" | "stop" | undefined;
      let finished = false; // the walk reached its report

      onInbound((m: Inbound) => {
        const delivery = ++deliveries;
        if (m.type === "answer") {
          const resolve = answers.get(m.body.ref);
          if (!resolve) return warn(`ignoring answer for unknown or already answered ref ${m.body.ref}`);
          answers.delete(m.body.ref);
          resolve({ body: m.body, delivery });
        } else if (m.type === "belay") {
          if (haltedBy === "belay") return;
          haltedBy = "belay";
          halt(new Error(`belayed: ${m.body.reason ?? "no reason given"}`));
          abort.abort();
        } else if (m.type === "correction") {
          stepLog && appendStep(stepLog, { order: order.id, action: "correction", ts: new Date().toISOString(), delivery, correction: m.body });
        } else if ((m.type === "stand_down" || m.type === "recall") && haltedBy === undefined && !finished) {
          haltedBy = "stop";
          abort.abort();
          void stop(new Stopped(m.type, m.body.reason));
        } else warn(`ignoring ${m.type} for ${order.id}`);
      });

      /** The latest request_handover note of this run: harness notes chain it (#145). */
      let agentNote: string | undefined;
      /** A resumed order's holodeck park (#145): the walk goes on after that step. */
      const parkedAt = Number(PARKED_AT.exec(order.body.handover ?? "")?.[1] ?? -1);
      /**
       * A stop's note: it names the park this run resumed from, if any, as
       * the chain no longer carries the earlier park's line (#174), so a run
       * resumed from it walks on from the same step.
       */
      const stopNote = (message: string) =>
        chainNote(
          `Order ${order.id}: ${message}. The holodeck stopped its script here.${parkedAt >= 0 ? ` The holodeck parked its script at step ${parkedAt}.` : ""}`,
          startedFrom(order),
          agentNote,
        );
      /** Each repo's commit in the run's latest checkpoint: the next one bundles what came after it (#145). */
      let since: Record<string, string> = {};
      /** writePlaceholderCheckpoint, from the run's previous checkpoint. */
      const placeholder = async (dir: string, transcript: Record<string, unknown>, note: string) => {
        const body = await writePlaceholderCheckpoint(dir, order, transcript, note, since);
        since = Object.fromEntries(body.repos.map((r) => [r.name, r.commit]));
        return body;
      };
      /** Once the walk has stopped, writes and emits the stopping checkpoint, then ends the run with Stopped. */
      const stop = async (stopped: Stopped) => {
        await walked.catch(() => {});
        try {
          const body = await placeholder(
            path.join(orderDir(workDir, order), String(steps.length)),
            { holodeck: true, order: order.id, stopped: stopped.command },
            stopNote(stopped.message),
          );
          if (haltedBy !== "stop") return; // a belay came in meanwhile and ended the run
          emit({ type: "checkpoint_ready", body: { ...body, stopping: true } });
          halt(stopped);
        } catch (err) {
          halt(err as Error);
        }
      };

      const recorded = {
        ...(order.repos.length > 0 && { repos: Object.fromEntries(order.repos.map((r) => [r.name, r.dir])) }),
        ...(order.body.model !== undefined && order.body.model !== "" && { model: order.body.model }),
        ...(order.body.handover !== undefined && order.body.handover !== "" && { handover: order.body.handover }),
        ...(order.body.grants !== undefined && order.body.grants.length > 0 && { grants: order.body.grants }),
        ...(order.body.budget_tokens > 0 && { budget_tokens: order.body.budget_tokens }),
        ...((order.body.budget_turns ?? 0) > 0 && { budget_turns: order.body.budget_turns }),
        ...((order.body.used_tokens ?? 0) > 0 && { used_tokens: order.body.used_tokens }),
        ...((order.body.used_turns ?? 0) > 0 && { used_turns: order.body.used_turns }),
      };
      /** The order's usage on this beam: the latest usage step's, else the order's earlier runs', else zero. */
      let own: UsageBody = { input_tokens: order.usage?.input_tokens ?? 0, output_tokens: order.usage?.output_tokens ?? 0, turns: order.usage?.turns ?? 0 };
      /** The order's budget (#119), checked before each step but usage and crash. */
      const budget = new Budget(order.body);
      /**
       * The order is over budget: raises a budget hail and waits for its answer, logged as a
       * "budget" step at index: extend raises the limit and the walk goes on;
       * stop writes a checkpoint into `<work-dir>/<order>/<script length>/`,
       * emits it and ends the run with BudgetExceeded.
       */
      const checkBudget = async (index: number) => {
        const ref = `holodeck-${order.id}-${index}-budget`;
        const answer = abortable(new Promise<Delivered<AnswerBody>>((resolve) => answers.set(ref, resolve)), signal);
        emit({ type: "hail", body: { ref, ...budget.hail(order.id, own) } });
        const { body: a, delivery } = await answer;
        signal.throwIfAborted();
        stepLog &&
          appendStep(stepLog, { order: order.id, index, action: "budget", ts: new Date().toISOString(), ...recorded, decision: a.decision, decided_by: a.decided_by, delivery });
        if (budget.answer(a)) return;
        const stopped = new BudgetExceeded(order.id, budget.context(own));
        const body = await placeholder(
          path.join(orderDir(workDir, order), String(steps.length)),
          { holodeck: true, order: order.id, stopped: "budget" },
          stopNote(stopped.message),
        );
        signal.throwIfAborted();
        emit({ type: "checkpoint_ready", body });
        throw stopped;
      };
      const walk = async (): Promise<HarnessReport> => {
        for (const [index, step] of steps.entries()) {
          if (index <= parkedAt && index < steps.length - 1) continue;
          signal.throwIfAborted();
          const log = (extra: Record<string, unknown> = {}) =>
            stepLog && appendStep(stepLog, { order: order.id, index, action: actionOf(step), ts: new Date().toISOString(), ...recorded, ...extra });
          // Only awaited when over budget: a walk under it reaches its first await where it did before.
          if (!("usage" in step) && !("crash" in step) && budget.over(own)) await checkBudget(index);
          if ("progress" in step) {
            emit({ type: "progress", body: { pct: step.at_pct, note: step.progress } });
            log();
          } else if ("usage" in step) {
            const { input_tokens, output_tokens, turns } = step.usage;
            own = { input_tokens, output_tokens, turns };
            emit({ type: "usage", body: { input_tokens, output_tokens, turns } });
            log();
          } else if ("finding" in step) {
            emit({ type: "finding", body: step.finding });
            log();
          } else if ("hail" in step) {
            const ref = `holodeck-${order.id}-${index}`;
            const answer = abortable(new Promise<Delivered<AnswerBody>>((resolve) => answers.set(ref, resolve)), signal);
            emit({ type: "hail", body: { ref, ...step.hail } });
            if (step.park_after_ms !== undefined) {
              const parkAfter = step.park_after_ms;
              const timer = new AbortController();
              const cancel = () => timer.abort();
              signal.addEventListener("abort", cancel, { once: true });
              void answer.then(cancel, cancel);
              void (async () => {
                const late = await delay(parkAfter, true, { signal: timer.signal }).catch(() => false);
                if (!late || signal.aborted) return;
                const body = await placeholder(
                  path.join(orderDir(workDir, order), String(index)),
                  { holodeck: true, order: order.id, parked: index },
                  chainNote(`Order ${order.id}: the holodeck parked on hail ${ref} (${step.hail.question}). The holodeck parked its script at step ${index}.`, startedFrom(order), agentNote),
                );
                if (signal.aborted) return;
                emit({ type: "checkpoint_ready", body: { ...body, held: [ref] } });
                log({ action: "park" });
              })().catch((err: unknown) => warn(`${order.id}: the park's checkpoint failed: ${err instanceof Error ? err.message : String(err)}`));
            }
            const { body: a, delivery } = await answer;
            signal.throwIfAborted();
            log({ decision: a.decision, decided_by: a.decided_by, delivery });
          } else if ("write" in step) {
            for (const r of order.repos) {
              const file = path.join(r.dir, step.write);
              fs.mkdirSync(path.dirname(file), { recursive: true });
              fs.appendFileSync(file, `${order.id} step ${index}\n`);
            }
            log();
          } else if ("commit" in step) {
            const commits = await commitWork(order, index, step.commit);
            signal.throwIfAborted();
            log(order.repos.length > 0 ? { commits } : {});
          } else if ("request_handover" in step) {
            const body = await placeholder(
              path.join(orderDir(workDir, order), String(index)),
              { holodeck: true, order: order.id, step: index },
              step.request_handover,
            );
            agentNote = step.request_handover;
            signal.throwIfAborted();
            emit({ type: "checkpoint_ready", body });
            log();
          } else if ("sleep_ms" in step) {
            await delay(step.sleep_ms, undefined, { signal });
            signal.throwIfAborted();
            log();
          } else if ("report" in step) {
            log();
            finished = true;
            return { ...step.report, usage: { input_tokens: own.input_tokens, output_tokens: own.output_tokens } };
          } else {
            log();
            process.exit(step.crash);
          }
        }
        throw new Error("holodeck script ended without a terminal step"); // validated away at startup
      };
      const walked = walk();
      // A walk the abort cut short ends however halt says.
      return Promise.race([halted, walked.catch((err: unknown) => (signal.aborted ? halted : Promise.reject(err as Error)))]);
    },
  };
}

function warn(msg: string): void {
  console.error(`combadge holodeck: ${msg}`);
}

/** The commit step: one commit per repo on its checked-out branch; returns each repo's new commit. */
async function commitWork(order: Order, index: number, message: string): Promise<Record<string, string>> {
  const commits: Record<string, string> = {};
  for (const r of order.repos) {
    fs.appendFileSync(path.join(r.dir, "holodeck.txt"), `${order.id} step ${index}: ${message}\n`);
    await git(r.dir, "add", "holodeck.txt");
    await git(r.dir, "-c", "user.name=holodeck", "-c", "user.email=holodeck@bmus.invalid", "-c", "commit.gpgsign=false",
      "commit", "--quiet", "--no-verify", "-m", message);
    commits[r.name] = await git(r.dir, "rev-parse", "HEAD");
  }
  return commits;
}

function appendStep(file: string, entry: Record<string, unknown>): void {
  const fd = fs.openSync(file, "a");
  try {
    fs.writeSync(fd, JSON.stringify(entry) + "\n");
    fs.fsyncSync(fd);
  } finally {
    fs.closeSync(fd);
  }
}

function actionOf(step: Step): Action {
  return ACTIONS.find((a) => a in step)!;
}

/** Reads and validates a script; throws a message naming the file and step. */
export function loadScript(file: string): Script {
  let raw: unknown;
  try {
    raw = JSON.parse(fs.readFileSync(file, "utf8"));
  } catch (err) {
    throw new Error(`holodeck script ${file}: ${err instanceof Error ? err.message : String(err)}`);
  }
  try {
    return parseScript(raw);
  } catch (err) {
    throw new Error(`holodeck script ${file}: ${err instanceof Error ? err.message : String(err)}`);
  }
}

/** A steps array, or `{"steps": [...], "consult": ...}`. */
export function parseScript(raw: unknown): Script {
  if (!isObject(raw)) return { steps: validateScript(raw) };
  const extra = Object.keys(raw).filter((k) => k !== "steps" && k !== "consult");
  if (extra.length) throw new Error(`unknown script key ${extra.join(", ")}`);
  return { steps: validateScript(raw.steps), ...(raw.consult !== undefined && { consult: validateConsult(raw.consult) }) };
}

function validateConsult(v: unknown): ConsultSpec {
  if (v === "silence") return v;
  const fail = (msg: string): never => {
    throw new Error(`consult: ${msg}`);
  };
  if (!isObject(v)) fail(`must be "silence" or a consult_reply body without hail_id`);
  const c = v as Record<string, unknown>;
  onlyKeys(c, ["decision", "option", "answer", "rationale", "confidence", "risk_flags"], "consult", fail);
  if (c.decision !== "answer" && c.decision !== "escalate") fail("decision must be answer or escalate");
  if (typeof c.confidence !== "number" || !(c.confidence >= 0 && c.confidence <= 1)) fail("confidence must be a number from 0 to 1");
  if (c.option !== undefined && !(Number.isSafeInteger(c.option) && (c.option as number) >= 0)) fail("option must be a non-negative integer");
  if (c.answer !== undefined && typeof c.answer !== "string") fail("answer must be a string");
  if (c.rationale !== undefined && typeof c.rationale !== "string") fail("rationale must be a string");
  if (c.risk_flags !== undefined && !isStrings(c.risk_flags)) fail("risk_flags must be an array of strings");
  return c as ConsultSpec;
}

export function validateScript(raw: unknown): Step[] {
  if (!Array.isArray(raw) || raw.length === 0) throw new Error("must be a non-empty JSON array of steps");
  let lastPct = 0;
  raw.forEach((step: unknown, i) => {
    const fail = (msg: string): never => {
      throw new Error(`step ${i}: ${msg}`);
    };
    if (!isObject(step)) fail("must be an object");
    const s = step as Record<string, unknown>;
    const pct = s.at_pct;
    if (typeof pct !== "number" || !(pct >= 0 && pct <= 100)) fail("at_pct must be a number from 0 to 100");
    if ((pct as number) < lastPct) fail(`at_pct ${String(pct)} is below the previous step's ${lastPct}`);
    lastPct = pct as number;
    if (s.park_after_ms !== undefined) {
      if (!("hail" in s)) fail("park_after_ms goes only with hail");
      if (!Number.isSafeInteger(s.park_after_ms) || (s.park_after_ms as number) < 0 || (s.park_after_ms as number) > MAX_SLEEP_MS) {
        fail(`park_after_ms must be an integer from 0 to ${MAX_SLEEP_MS}`);
      }
    }
    const keys = Object.keys(s).filter((k) => k !== "at_pct" && k !== "park_after_ms");
    const unknown = keys.filter((k) => !(ACTIONS as readonly string[]).includes(k));
    if (unknown.length) fail(`unknown key ${unknown.join(", ")}`);
    if (keys.length !== 1) fail(`needs exactly one action (${ACTIONS.join(", ")})`);
    const action = keys[0] as Action;
    const v = s[action];
    switch (action) {
      case "write": {
        const parts = typeof v === "string" ? v.split(/[\\/]/) : [];
        if (typeof v !== "string" || v === "" || path.isAbsolute(v) || parts.includes("..") || parts[0] === ".git") {
          fail("write must be a relative path inside the repo, outside .git");
        }
        break;
      }
      case "progress":
      case "commit":
      case "request_handover":
        if (typeof v !== "string") fail(`${action} must be a string`);
        break;
      case "sleep_ms":
        if (!Number.isSafeInteger(v) || (v as number) < 0 || (v as number) > MAX_SLEEP_MS) {
          fail(`sleep_ms must be an integer from 0 to ${MAX_SLEEP_MS}`);
        }
        break;
      case "crash":
        if (!Number.isInteger(v) || (v as number) < 1 || (v as number) > 255) fail("crash must be an exit code from 1 to 255");
        break;
      case "usage": {
        if (!isObject(v)) fail("usage must be an object");
        const u = v as Record<string, unknown>;
        onlyKeys(u, ["input_tokens", "output_tokens", "turns"], "usage", fail);
        for (const k of ["input_tokens", "output_tokens", "turns"]) {
          if (!Number.isSafeInteger(u[k]) || (u[k] as number) < 0) fail(`usage.${k} must be a non-negative integer`);
        }
        break;
      }
      case "finding":
        if (!isObject(v)) fail("finding must be an object");
        break;
      case "hail": {
        if (!isObject(v)) fail("hail must be an object");
        const h = v as Record<string, unknown>;
        onlyKeys(h, ["category", "question", "options", "context"], "hail", fail);
        if (!nonEmpty(h.category)) fail("hail.category must be a non-empty string");
        if (!nonEmpty(h.question)) fail("hail.question must be a non-empty string");
        if (h.options !== undefined && !isStrings(h.options)) fail("hail.options must be an array of strings");
        if (h.context !== undefined && !isObject(h.context)) fail("hail.context must be an object");
        break;
      }
      case "report": {
        if (!isObject(v)) fail("report must be an object");
        const r = v as Record<string, unknown>;
        onlyKeys(r, ["summary", "artifacts"], "report", fail);
        if (typeof r.summary !== "string") fail("report.summary must be a string");
        if (r.artifacts !== undefined && !isStrings(r.artifacts)) fail("report.artifacts must be an array of strings");
        break;
      }
    }
    const last = i === raw.length - 1;
    if (TERMINAL.has(action) && !last) fail(`${action} is terminal but is not the last step`);
    if (!TERMINAL.has(action) && last) fail("the script must end with a terminal step (report or crash)");
  });
  return raw as Step[];
}

function onlyKeys(o: Record<string, unknown>, allowed: string[], what: string, fail: (m: string) => never): void {
  const extra = Object.keys(o).filter((k) => !allowed.includes(k));
  if (extra.length) fail(`unknown ${what} key ${extra.join(", ")}`);
}

function isObject(v: unknown): v is Record<string, unknown> {
  return typeof v === "object" && v !== null && !Array.isArray(v);
}

function nonEmpty(v: unknown): boolean {
  return typeof v === "string" && v.length > 0;
}

function isStrings(v: unknown): boolean {
  return Array.isArray(v) && v.every((x) => typeof x === "string");
}
