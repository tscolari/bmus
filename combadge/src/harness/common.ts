// Helpers shared by the harnesses.
import * as fs from "node:fs";
import * as path from "node:path";
import {
  CATEGORY_BUDGET,
  CODE_BUDGET_EXCEEDED,
  OPTION_EXTEND,
  OPTION_STOP,
  type AnswerBody,
  type BudgetContext,
  type BudgetLimit,
  type CheckpointReadyBody,
  type ConsultReplyBody,
  type HailBody,
  type OrderBody,
  type UsageBody,
  type WorkRepo,
} from "../protocol.js";
import { bundleWip, bundleWork, isSnapshotNoteLine, recordEmitted, snapshotNote, snapshotWip, type Snapshot } from "../repos.js";
import type { Order } from "./types.js";

/** Rejects with the signal's reason once it aborts, if p hasn't settled. */
export function abortable<T>(p: Promise<T>, signal: AbortSignal): Promise<T> {
  return new Promise<T>((resolve, reject) => {
    const onAbort = () => reject(signal.reason as Error);
    if (signal.aborted) return onAbort();
    signal.addEventListener("abort", onAbort, { once: true });
    p.then(
      (v) => (signal.removeEventListener("abort", onAbort), resolve(v)),
      (e: unknown) => (signal.removeEventListener("abort", onAbort), reject(e as Error)),
    );
  });
}

/** `<workDir>/<order>`, refusing order ids that would leave workDir. */
export function orderDir(workDir: string, order: Order): string {
  if (!/^[A-Za-z0-9._-]+$/.test(order.id) || order.id === "." || order.id === "..") {
    throw new Error(`order id ${JSON.stringify(order.id)} is not usable as a directory name`);
  }
  return path.join(workDir, order.id);
}

/** Paths of the transcript and the note inside a checkpoint dir; each repo's bundle is `<name>.bundle` next to them. */
export function checkpointFiles(dir: string) {
  return {
    transcript_path: path.join(dir, "transcript.jsonl"),
    handover_path: path.join(dir, "handover.md"),
  };
}

/**
 * Snapshots each of the order's repos' work in progress (snapshotWip,
 * #145) and bundles it, in order: `bmus/<order>` into `<dir>/<name>.bundle`
 * (see bundleWork) and the WIP refs into `<dir>/<name>.wip.bundle`
 * (bundleWip), named in `wip_bundle_path`. since holds each repo's previous
 * checkpoint commit, by name: that repo's bundle starts there, with the
 * full one beside it. Each checkout records the checkpoint as emitted
 * (recordEmitted). key says what the checkpoint holds (each repo's tip and
 * work tree), so a harness can tell whether anything changed since
 * (workKey); notes are what the snapshots left out, for the checkpoint's
 * note.
 */
export async function bundleCheckpoint(
  dir: string,
  order: Order,
  since: Record<string, string> = {},
): Promise<{ repos: WorkRepo[]; key: string; notes: string[] }> {
  const repos: WorkRepo[] = [];
  const keys: string[] = [];
  const notes: string[] = [];
  for (const r of order.repos) {
    const snap = await snapshotWip(r, order.id);
    keys.push(keyOf(r.name, snap));
    notes.push(...snapshotNote(r.name, snap));
    const prev = since[r.name];
    const work = await bundleWork(r, order.id, path.join(dir, `${r.name}.bundle`), prev !== undefined ? { since: prev } : {});
    const wip = await bundleWip(r, order.id, work.commit, path.join(dir, `${r.name}.wip.bundle`));
    await recordEmitted(r.dir, order.id, work.commit, snap.commit);
    repos.push({ ...work, ...(wip !== undefined && { wip_bundle_path: wip }) });
  }
  return { repos, key: keys.join(" "), notes };
}

const keyOf = (name: string, snap: Snapshot) => `${name}:${snap.tip}:${snap.tree}:${snap.skipped ?? ""}`;

/**
 * What the order's repos hold now, as bundleCheckpoint's key: each repo's
 * `bmus/<order>` tip and work tree. It writes no object and moves no ref.
 */
export async function workKey(order: Order): Promise<string> {
  const keys: string[] = [];
  for (const r of order.repos) keys.push(keyOf(r.name, await snapshotWip(r, order.id, { commit: false })));
  return keys.join(" ");
}

/** The opening of each line the bridge adds for a carried hail's answer (carriedNote in internal/bridge). */
export const CARRIED_ANSWER = "While the order was parked, the captain answered hail ";
/** The most a harness-written note carries forward from earlier checkpoints, its markers included (#174). */
export const CHAIN_MAX_BYTES = 8 * 1024;

const CHAIN_BEGIN = "--- Carried forward from earlier checkpoints ---";
const CHAIN_END = "--- End of what was carried forward ---";
const CHAIN_CUT = "(Cut to stay under 8 KiB: ";
const ANSWERS = "The captain's answers to hails carried from a park, which no handover note of the agent's has taken in yet:";
const AGENT_THIS_RUN = "The agent's latest handover note, written in this run:";
const AGENT_EARLIER = "The agent's latest handover note, written in an earlier run:";
const NO_AGENT = "The agent has written no handover note yet.";
const CUT_MARK = "[...]";

/** What a handover note carries for the next harness note: the agent's latest note and the answers no agent note has taken in. */
export interface Chained {
  agent?: string;
  answers: string[];
}

/**
 * What the handover note a run started from carries forward (#174), from
 * the note as the bridge sent it (Order.sentHandover: serve's WIP lines are
 * not in it). Trailing lines opening with CARRIED_ANSWER are the bridge's.
 * The rest is the checkpoint's note: a harness-written one (it has the
 * CHAIN_BEGIN block) gives its block's agent note and answers, and its own
 * text (park, stop and cadence lines) is dropped; any other is the agent's
 * own note, without the snapshotNote lines its checkpoint added.
 */
export function chainedFrom(started: string | undefined): Chained {
  const paras = (started ?? "").trim().split("\n\n");
  const appended: string[] = [];
  while (paras.length > 0 && isAnswer(paras[paras.length - 1]!)) appended.unshift(paras.pop()!.trim());
  const lines = paras.join("\n\n").split("\n");
  const begin = lines.indexOf(CHAIN_BEGIN);
  const end = lines.lastIndexOf(CHAIN_END);
  if (begin >= 0 && end > begin) {
    const block = lines.slice(begin + 1, end);
    let i = 0;
    const answers: string[] = [];
    while (i < block.length && block[i]!.startsWith(CHAIN_CUT)) i++;
    if (block[i] === ANSWERS) for (i++; i < block.length && isAnswer(block[i]!); i++) answers.push(block[i]!.trim());
    while (i < block.length && block[i]!.trim() === "") i++;
    const header = block[i];
    const agent = header === AGENT_THIS_RUN || header === AGENT_EARLIER ? block.slice(i + 1).join("\n").trim() : "";
    return { ...(agent !== "" && { agent }), answers: [...answers, ...appended] };
  }
  const last = paras[paras.length - 1];
  if (last !== undefined && last.split("\n").every(isSnapshotNoteLine)) paras.pop();
  const agent = paras.join("\n\n").trim();
  return { ...(agent !== "" && { agent }), answers: appended };
}

const isAnswer = (line: string) => line.startsWith(CARRIED_ANSWER) && !line.trim().includes("\n");
const bytes = (s: string) => Buffer.byteLength(s, "utf8");

/**
 * A harness-written checkpoint note (#145): own, then what the next run
 * needs from earlier checkpoints, between CHAIN_BEGIN and CHAIN_END (#174).
 * That is the agent's latest note (agent, this run's request_handover note,
 * else the newest one in started, the handover note the run started from as
 * the bridge sent it) and the carried answers not yet delivered: those in
 * started, unless this run's agent wrote a note, which came after them.
 * Earlier park, stop, cadence, WIP and snapshot lines are not carried. The
 * block stays under CHAIN_MAX_BYTES: the agent's note, the oldest part, is
 * cut first (from its end), then the oldest answers, and the block says so.
 * A later run gets only the note of the checkpoint it resumes from.
 */
export function chainNote(own: string, started: string | undefined, agent: string | undefined): string {
  const thisRun = agent !== undefined && agent.trim() !== "";
  const from = chainedFrom(started);
  const note = thisRun ? agent.trim() : from.agent;
  const answers = thisRun ? [] : [...new Set(from.answers)];
  const header = thisRun ? AGENT_THIS_RUN : AGENT_EARLIER;
  const render = (answers: string[], note: string | undefined, cut: number) =>
    [
      CHAIN_BEGIN,
      ...(cut > 0 ? [`${CHAIN_CUT}${cut} bytes of the oldest text were cut.)`] : []),
      ...(answers.length > 0 ? [ANSWERS, ...answers, ""] : []),
      ...(note === undefined ? [NO_AGENT] : [header, note]),
      CHAIN_END,
    ].join("\n");
  let block = render(answers, note, 0);
  if (bytes(block) > CHAIN_MAX_BYTES) {
    // The cut line's room is taken at its longest, so the result never grows past the cap.
    const room = (answers: string[]) => CHAIN_MAX_BYTES - bytes(render(answers, CUT_MARK, Number.MAX_SAFE_INTEGER));
    let kept = answers;
    let cut = 0;
    while (kept.length > 0 && room(kept) < 0) {
      cut += bytes(kept[0]!) + 1;
      kept = kept.slice(1);
    }
    let text = CUT_MARK;
    if (note !== undefined) {
      // The note goes whole before any answer does.
      const keep = kept.length < answers.length ? 0 : Math.max(0, room(kept) - bytes(` ${CUT_MARK}`));
      const head = Buffer.from(note, "utf8").subarray(0, keep).toString("utf8").replace(/\uFFFD+$/, "").trimEnd();
      text = head === "" ? CUT_MARK : `${head} ${CUT_MARK}`;
      cut += bytes(note) - bytes(head);
    }
    block = render(kept, note === undefined ? undefined : text, cut);
  }
  return `${own.trimEnd()}\n\n${block}`;
}

/** The handover note the order's run started from, as the bridge sent it (#174): chainNote's `started`. */
export const startedFrom = (order: Order): string | undefined => order.sentHandover ?? order.body.handover;

/** note with notes, if any, after a blank line. */
export function withNotes(note: string, notes: string[]): string {
  return notes.length === 0 ? note : `${note.trimEnd()}\n\n${notes.join("\n")}`;
}

/**
 * Writes a placeholder checkpoint into dir, as the holodeck and echo
 * harnesses do: one bundle per repo (see bundleCheckpoint), transcript.jsonl
 * holding the one line `transcript`, and the note in handover.md.
 */
export async function writePlaceholderCheckpoint(
  dir: string,
  order: Order,
  transcript: Record<string, unknown>,
  note: string,
  since: Record<string, string> = {},
): Promise<CheckpointReadyBody> {
  fs.mkdirSync(dir, { recursive: true });
  const { repos, notes } = await bundleCheckpoint(dir, order, since);
  const files = checkpointFiles(dir);
  // No fsync on purpose: these are placeholders, written before checkpoint_ready is emitted.
  fs.writeFileSync(files.transcript_path, JSON.stringify(transcript) + "\n");
  fs.writeFileSync(files.handover_path, withNotes(note, notes) + "\n");
  return { repos, ...files };
}

/** The bridge->beam commands that stop a run after a checkpoint. */
export type StopCommand = "stand_down" | "recall";

/**
 * What a run rejects with once it has emitted the checkpoint answering a
 * stand_down or recall (the one with `stopping: true`). serve ends the order
 * with no report or error, so the bridge can resume it later.
 */
export class Stopped extends Error {
  constructor(
    readonly command: StopCommand,
    reason?: string,
  ) {
    super(`stopped by ${command}: ${reason ?? "no reason given"}`);
    this.name = "Stopped";
  }
}

/** A consult_reply handing the hail to the captain, with confidence 0. */
export function escalate(hailId: string, rationale: string): ConsultReplyBody {
  return { hail_id: hailId, decision: "escalate", rationale, confidence: 0 };
}

// ---- budgets (#119) ----

/**
 * An order's budget as a harness enforces it: the limit the order carries
 * (budget_tokens, budget_turns; 0 is unlimited), raised by each extension the
 * captain grants, and the usage the bridge recorded on the order's other beams
 * (used_tokens, used_turns). The harness is over budget when that usage plus
 * its own count for the order passes the limit; its count already holds this
 * beam's earlier runs, so no baseline is needed.
 */
export class Budget {
  limit: BudgetLimit;
  private readonly usedTokens: number;
  private readonly usedTurns: number;

  constructor(body: OrderBody) {
    this.limit = { tokens: body.budget_tokens ?? 0, turns: body.budget_turns ?? 0 };
    this.usedTokens = body.used_tokens ?? 0;
    this.usedTurns = body.used_turns ?? 0;
  }

  /** No limit in tokens or in turns. */
  get unlimited(): boolean {
    return this.limit.tokens === 0 && this.limit.turns === 0;
  }

  /** The order's usage and limit, given the harness's own count: a budget hail's context. */
  context(own: UsageBody): BudgetContext {
    return {
      used: this.usedTokens + own.input_tokens + own.output_tokens,
      limit: this.limit.tokens,
      turns_used: this.usedTurns + own.turns,
      turns_limit: this.limit.turns,
    };
  }

  /** Whether the order is over its limit in tokens or in turns. */
  over(own: UsageBody): boolean {
    const c = this.context(own);
    return (c.limit > 0 && c.used > c.limit) || (c.turns_limit > 0 && c.turns_used > c.turns_limit);
  }

  /** The hail an over-budget harness raises. The bridge writes its own question from the context. */
  hail(orderId: string, own: UsageBody): Omit<HailBody, "ref"> {
    const c = this.context(own);
    return {
      category: CATEGORY_BUDGET,
      question: `${orderId} is over its budget: ${c.used} tokens of ${c.limit || "unlimited"}, ${c.turns_used} turns of ${c.turns_limit || "unlimited"}. Extend it, or stop?`,
      options: [OPTION_EXTEND, OPTION_STOP],
      context: { ...c },
    };
  }

  /**
   * Takes the captain's answer: true for extend (option 0), raising the limit
   * to the one the answer carries, false for anything else (stop). An extend
   * without a limit keeps the old one, so the next call hails again.
   */
  answer(a: AnswerBody): boolean {
    if (a.option !== 0) return false;
    if (a.budget) this.limit = { tokens: a.budget.tokens, turns: a.budget.turns };
    return true;
  }
}

/**
 * What a run rejects with once the captain answered its budget hail stop and
 * its checkpoint is out (#119). serve ends the order with an error coded
 * CODE_BUDGET_EXCEEDED, and the bridge fails it.
 */
export class BudgetExceeded extends Error {
  readonly code = CODE_BUDGET_EXCEEDED;
  constructor(orderId: string, c: BudgetContext) {
    const field = (used: number, limit: number, unit: string) => `${used} ${unit}${limit > 0 ? ` of ${limit}` : ""}`;
    super(`budget exceeded: the captain stopped ${orderId} at ${field(c.used, c.limit, "tokens")} and ${field(c.turns_used, c.turns_limit, "turns")}`);
    this.name = "BudgetExceeded";
  }
}
