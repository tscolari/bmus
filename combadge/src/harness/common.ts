// Helpers shared by the harnesses.
import * as fs from "node:fs";
import * as path from "node:path";
import type { CheckpointReadyBody, ConsultReplyBody, WorkRepo } from "../protocol.js";
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
