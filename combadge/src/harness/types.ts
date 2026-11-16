import type { ConsultBody, ConsultReplyBody, OrderBody, Payload, ReportBody, UsageBody } from "../protocol.js";
import type { RepoCheckout } from "../repos.js";

/**
 * An order as `serve` hands it to a harness: the envelope's order id, its
 * body, and its repos, already checked out (see repos.ts), in the body's
 * order.
 */
export interface Order {
  id: string;
  body: OrderBody;
  repos: RepoCheckout[];
  /**
   * The order's usage on this beam so far (#117): serve's record of its
   * latest usage report from an earlier run, kept across combadge restarts.
   * Absent on an order's first run here. A harness that reports usage
   * carries on from it.
   */
  usage?: UsageBody;
  /**
   * The handover note as the bridge sent it (#174), before serve added its
   * WIP lines to body.handover: harness-written notes chain from it
   * (startedFrom, chainNote), so a start's WIP lines go no further.
   * serve always sets it, "" when the bridge sent none; only a harness run
   * without serve (a test) goes without it, and reads body.handover.
   */
  sentHandover?: string;
}

/**
 * What a harness may emit while it runs an order. `serve` adds the envelope
 * (v, seq, crew, order, ts) and writes it to the outbox. `report` and `error`
 * are not here: `serve` emits them when `run` resolves or rejects. `hello` and
 * `heartbeat` belong to `serve` too.
 */
export type Outbound = Payload<"progress" | "usage" | "finding" | "hail" | "checkpoint_ready">;

/**
 * Bridge->beam commands `serve` relays to the harness running the order.
 * `order` starts a run and `ack` is handled by `serve`, so neither is here.
 */
export type Inbound = Payload<"answer" | "checkpoint" | "stand_down" | "recall" | "belay" | "correction">;

export type Emit = (m: Outbound) => void;
export type OnInbound = (fn: (m: Inbound) => void) => void;

/**
 * What a harness's run resolves with: the report without its repos. `serve`
 * bundles each repo's `bmus/<order>` into `<work-dir>/<order>/out/<name>.bundle`
 * and adds them (#53).
 */
export type HarnessReport = Omit<ReportBody, "repos">;

/**
 * Drives the agent for one order at a time (spec D4; plan §5). `serve` owns
 * the order lifecycle: it calls `run` on an inbound `order`, emits `report`
 * with the resolved body and the bundled repos, or `error` with the
 * rejection's message. A run that rejects with Stopped (harness/common.ts),
 * after emitting a checkpoint_ready with `stopping: true` in answer to a
 * `stand_down` or `recall`, ends with nothing emitted.
 *
 * Inbound commands go only to handlers registered through `onInbound` at the
 * time they arrive, so a harness should register before its first await.
 */
export interface Harness {
  /** Name reported in `hello` (e.g. sdk, holodeck). */
  readonly name: string;
  /**
   * The model an order without `model` runs on, reported in `hello`.
   * Undefined when the harness doesn't know it (sdk: the SDK picks it) or has
   * no model (echo, holodeck).
   */
  readonly model?: string;
  run(order: Order, emit: Emit, onInbound: OnInbound): Promise<HarnessReport>;
  /**
   * Command's consult lane (D7; phase 3 plan §5): answers one consult,
   * independent of any run. serve calls it for each `consult`, while an order
   * runs or not, and with others in flight; it emits the result as a
   * `consult_reply` with the request's hail_id and no order. A rejection
   * becomes an escalate. signal aborts when serve closes. A harness without
   * it gets an escalate from serve at once.
   */
  consult?(req: ConsultBody, signal: AbortSignal): Promise<ConsultReplyBody>;
  /**
   * A check serve runs once at startup, before any attach is served (#172).
   * A fail goes to the bridge as a crew-level `harness_check_failed` error
   * and serve exits; inconclusive is logged. A harness without it isn't
   * checked.
   */
  selfCheck?(): Promise<SelfCheck>;
}

/**
 * What Harness.selfCheck found. reason is the bridge's on a fail, the log's
 * when inconclusive. notes are logged whatever the result, and decide nothing.
 */
export type SelfCheck = ({ result: "pass" } | { result: "fail" | "inconclusive"; reason: string }) & { notes?: string[] };

/** serve flags a harness factory may use; each harness ignores what it doesn't need. */
export interface HarnessOptions {
  /** holodeck: script path (`--script`). */
  script?: string;
  /** holodeck: NDJSON file recording each executed step (`--step-log`). */
  stepLog?: string;
  /** serve's `--work-dir`, holding `<order>/` dirs: repo checkouts and checkpoints. Defaults to `<outbox dir>/<harness>`. */
  workDir: string;
  /** sdk: the `claude` executable the Agent SDK runs (`--claude-path`); looked up on PATH when unset. */
  claudePath?: string;
  /** sdk: how long a hail holds the query before the run parks (`--hail-park-after`, #154); 15 minutes when unset. */
  hailParkAfterMs?: number;
  /**
   * sdk: the checkpoint cadence (#145): once this long has passed since the
   * latest checkpoint, the harness writes one if the work changed
   * (`--checkpoint-every`); 15 minutes when unset, 0 turns it off.
   */
  checkpointEveryMs?: number;
  /** sdk: the same, once this many main-loop turns have passed (`--checkpoint-turns`); 10 when unset, 0 turns it off. */
  checkpointTurns?: number;
  /** sdk: the model of the startup check (`--check-model`, #172); the SDK's default when unset. */
  checkModel?: string;
  /**
   * The crew member's role (`--role`): "command" for Command's crew members,
   * whose sdk runs get the split_order tool (#155). Other harnesses ignore it.
   */
  role?: Role;
}

/** Roles serve's `--role` takes. */
export const ROLES = ["command"] as const;
export type Role = (typeof ROLES)[number];

/** Builds a harness from serve's flags at startup, throwing on bad configuration. */
export type HarnessFactory = (opts: HarnessOptions) => Harness | Promise<Harness>;
