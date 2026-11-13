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
import { bundleWork } from "../repos.js";
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

/** Bundles each of the order's repos into `<dir>/<name>.bundle` (see bundleWork), in order. */
export async function bundleCheckpoint(dir: string, order: Order): Promise<WorkRepo[]> {
  const out: WorkRepo[] = [];
  for (const r of order.repos) out.push(await bundleWork(r, order.id, path.join(dir, `${r.name}.bundle`)));
  return out;
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
): Promise<CheckpointReadyBody> {
  fs.mkdirSync(dir, { recursive: true });
  const repos = await bundleCheckpoint(dir, order);
  const files = checkpointFiles(dir);
  // No fsync on purpose: these are placeholders, written before checkpoint_ready is emitted.
  fs.writeFileSync(files.transcript_path, JSON.stringify(transcript) + "\n");
  fs.writeFileSync(files.handover_path, note + "\n");
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
