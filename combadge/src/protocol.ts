// Wire protocol between the bridge and the combadge: one JSON message per line
// (spec D3, §6).
//
// These types mirror internal/comms/protocol.go, and both mirror
// protocol/messages.schema.json, the shared source of truth. Keep the three in
// step: test/protocol.golden.test.ts validates every protocol/golden/*.json
// against the schema. The schema is used in tests only, never on inbound
// messages at runtime (it rejects unknown properties, so a runtime validator
// would break on any optional field added later).

/** Protocol version carried in every envelope's `v` field. */
export const PROTOCOL_VERSION = 1;

/** Hail categories the combadge emits. `HailBody.category` stays an open string. */
export const CATEGORY_CLARIFYING_QUESTION = "clarifying_question";
export const CATEGORY_TOOL_APPROVAL = "tool_approval";
/**
 * Command splitting its order into child orders (#155): options allow/deny and
 * an OrderSplitContext. An allowed split is answered with option 0 and the new
 * orders' ids, comma separated, as the decision.
 */
export const CATEGORY_ORDER_SPLIT = "order_split";
/**
 * A crew member's requests to comment on or close a tracker item (#54):
 * options allow/deny and a TrackerCommentContext or TrackerCloseContext. The
 * bridge runs an allowed one with its own credentials, then answers with option
 * 0 and what it did as the decision, or option 1 and why it couldn't.
 */
export const CATEGORY_TRACKER_COMMENT = "tracker_comment";
export const CATEGORY_TRACKER_CLOSE = "tracker_close";

/** A tracker_comment hail's context. */
export interface TrackerCommentContext {
  ref: string;
  body: string;
}

/** A tracker_close hail's context. An absent reason is completed. */
export interface TrackerCloseContext {
  ref: string;
  reason?: "completed" | "not_planned";
}

/** The most child orders one order_split hail may ask for. */
export const MAX_SPLIT_CHILDREN = 10;
/** The longest child order title, in characters (the schema's maxLength). */
export const MAX_SPLIT_TITLE = 200;

/** One child order of an order_split hail. Absent division and priority take the parent order's. */
export interface OrderSplitChild {
  title: string;
  body: string;
  division?: string;
  /** 0 routine, 1 yellow, 2 red. */
  priority?: number;
}

/** An order_split hail's context. */
export interface OrderSplitContext {
  /** The hailing order's id. */
  parent?: string;
  children: OrderSplitChild[];
}

/** Options of a tool_approval hail, in this order. Answer option 0 allows the tool. */
export const OPTION_ALLOW = "allow";
export const OPTION_DENY = "deny";

// ---- beam -> bridge bodies ----

/** Opens each attach session. */
export interface HelloBody {
  protocol: number;
  harness: string;
  combadge_version: string;
  /** The harness's default model, the one an order without `model` runs on; absent when unknown. */
  model?: string;
}

/** Periodic beam health; missing beats trigger the sickbay probe. */
export interface HeartbeatBody {
  disk_free_bytes: number;
  mem_free_bytes: number;
  /** Outbox entries not yet acked by the bridge. */
  outbox_depth: number;
}

/** Agent-reported progress; goes straight to the captain's log. */
export interface ProgressBody {
  /** 0-100. */
  pct: number;
  note: string;
}

/**
 * A question or permission request the agent is blocked on. Carries the
 * combadge's own `ref` (unique per combadge process) instead of a hail id: the
 * bridge assigns hail ids and echoes `ref` in the answer.
 */
export interface HailBody {
  ref: string;
  category: string;
  question: string;
  /** Absent or empty means a free-text answer. */
  options?: string[];
  /** Free-form JSON object. */
  context?: Record<string, unknown>;
}

/**
 * A checkpoint on the beam's disk: one bundle per repo of the order, the
 * session transcript and the handover note. Paths are absolute on the beam.
 */
export interface CheckpointReadyBody {
  repos: WorkRepo[];
  transcript_path: string;
  handover_path: string;
  /** True on the checkpoint that answers a stand_down or recall: the run stops after it, with no report or error. */
  stopping?: boolean;
}

/** The work in one repo of an order, as a git bundle on the beam's disk. */
export interface WorkRepo {
  /** The order repo's name. */
  name: string;
  /** Absolute path of a bundle of `branch` minus the order's base commit, or of its whole history when it has no commits past it. */
  bundle_path: string;
  /** The commit `branch` points at in the bundle. */
  commit: string;
  /** The branch the bundle carries, `bmus/<order>`. */
  branch: string;
}

export interface Usage {
  input_tokens: number;
  output_tokens: number;
}

/**
 * Running usage of the order on this beam (#117): cumulative over every run
 * and query of the order on this beam, combadge restarts included (serve
 * keeps the latest counts in state.json). Input counts cache reads and
 * writes. Counts never decrease; the latest report for an order is the one
 * with the highest envelope seq. Sent every USAGE_EVERY_TURNS turns and at
 * every checkpoint, stop and report.
 */
export interface UsageBody extends Usage {
  /** Main-loop turns; a subagent's turns are not counted. */
  turns: number;
}

/** A harness sends `usage` every this many turns. */
export const USAGE_EVERY_TURNS = 10;

/** Final result of an order. */
export interface ReportBody {
  summary: string;
  /** The work, one bundle per repo of the order. */
  repos: WorkRepo[];
  artifacts?: string[];
  usage: Usage;
}

/** Harness crash, tool failure or exceeded budget. */
export interface ErrorBody {
  reason: string;
  /** e.g. budget_exceeded, exit_137. */
  code?: string;
}

/** Command's decision on a consulted hail. Command lane only (phase 3 plan §5). */
export interface ConsultReplyBody {
  hail_id: string;
  decision: "answer" | "escalate";
  option?: number;
  answer?: string;
  rationale: string;
  confidence: number;
  risk_flags?: string[];
}

// ---- bridge -> beam bodies ----

/** Lets the combadge trim its outbox. Idempotent, never deduped. */
export interface AckBody {
  /** Highest contiguous beam->bridge seq the bridge recorded; 0 means none. */
  seq: number;
}

/** Assigns the mission named by the envelope's `order`. */
export interface OrderBody {
  title: string;
  /** Order spec (markdown). */
  body: string;
  /** Repos shipped onto the beam for the order; serve checks each out before the harness starts. */
  repos: OrderRepo[];
  /** Tokens that remain of the order's budget for this beam (the resolved budget times 1 + budget_extended, less the usage recorded on every beam). 0 means no budget; a spent budget is 1. */
  budget_tokens: number;
  /** Main-loop turns that remain, as budget_tokens. Absent or 0 means no budget. */
  budget_turns?: number;
  dossier?: string;
  /** Model to run the order on (the order's, else the crew member's, else its division's default); opaque to the bridge. Absent means the harness's default. */
  model?: string;
  /** Note of the checkpoint the order resumes from; each repo's bundle then carries that checkpoint's commits. Absent on a fresh order. */
  handover?: string;
}

/** One repo of an order. */
export interface OrderRepo {
  /** Name in the bridge's registry, and the checkout's directory name. */
  name: string;
  /** Git ref the work starts from, as the captain gave it. */
  base_ref: string;
  /** Absolute path on the beam of a git bundle holding branch `bmus/<order>` at base_ref's commit. */
  bundle_path: string;
}

/**
 * Decision on a hail. For tool_approval, option 0 allows and anything else
 * (or none) denies with `decision` as the message. For clarifying_question,
 * `decision` is the answer text.
 */
export interface AnswerBody {
  ref: string;
  hail_id: string;
  decided_by: "policy" | "command" | "captain";
  decision: string;
  option?: number;
  rationale?: string;
}

/** A hail plus fleet context for Command. Command lane only (phase 3 plan §5). */
export interface ConsultBody {
  hail_id: string;
  crew: string;
  order: string;
  category: string;
  question: string;
  options?: string[];
  context?: Record<string, unknown>;
  order_spec?: string;
  recent_log?: string[];
  guidance?: string;
  regulations?: string;
  /** Model Command answers on (its crew member's, else its division's default); opaque to the bridge. Absent means the harness's default. */
  model?: string;
}

export interface CheckpointBody {
  reason?: string;
}
export interface StandDownBody {
  reason?: string;
}
export interface RecallBody {
  reason?: string;
}
export interface BelayBody {
  reason?: string;
}
/**
 * The captain countermanded a decision made without them (Command's, or a
 * captain timeout's default_option) after the agent already had it. Not a
 * second answer: a running session is told of it.
 */
export interface CorrectionBody {
  /** Bridge hail id of the countermanded decision. */
  hail_id: string;
  /** The order the hail belongs to, as the envelope's order. */
  ref: string;
  /** The new decision and the captain's reason. */
  note: string;
  /** The hail's question, for a run that never got the first answer (#190). */
  question?: string;
}

// ---- envelope ----

/** Body shape for each beam->bridge message type. */
export interface BeamToBridgeBodies {
  hello: HelloBody;
  heartbeat: HeartbeatBody;
  progress: ProgressBody;
  usage: UsageBody;
  hail: HailBody;
  checkpoint_ready: CheckpointReadyBody;
  report: ReportBody;
  error: ErrorBody;
  consult_reply: ConsultReplyBody;
}

/** Body shape for each bridge->beam message type. */
export interface BridgeToBeamBodies {
  ack: AckBody;
  order: OrderBody;
  answer: AnswerBody;
  consult: ConsultBody;
  checkpoint: CheckpointBody;
  stand_down: StandDownBody;
  recall: RecallBody;
  belay: BelayBody;
  correction: CorrectionBody;
}

export type Bodies = BeamToBridgeBodies & BridgeToBeamBodies;
export type BeamToBridgeType = keyof BeamToBridgeBodies;
export type BridgeToBeamType = keyof BridgeToBeamBodies;
export type MessageType = keyof Bodies;

export const BEAM_TO_BRIDGE_TYPES = [
  "hello", "heartbeat", "progress", "usage", "hail", "checkpoint_ready", "report", "error", "consult_reply",
] as const satisfies readonly BeamToBridgeType[];

export const BRIDGE_TO_BEAM_TYPES = [
  "ack", "order", "answer", "consult", "checkpoint", "stand_down", "recall", "belay", "correction",
] as const satisfies readonly BridgeToBeamType[];

// Compile-time check that the two lists above name every type in the body maps.
type Unlisted = Exclude<MessageType, (typeof BEAM_TO_BRIDGE_TYPES)[number] | (typeof BRIDGE_TO_BEAM_TYPES)[number]>;
const allTypesListed: [Unlisted] extends [never] ? true : never = true;
void allTypesListed;

/**
 * `{type, body}` pair for a type: the part of a message a harness deals with.
 * Distributes over unions, so `Payload<"a" | "b">` is a discriminated union.
 */
export type Payload<T extends MessageType = MessageType> = {
  [K in T]: { type: K; body: Bodies[K] };
}[T];

/** Types that belong to one order: the schema requires a non-empty `order` on them. */
export type OrderBoundType =
  | "progress" | "usage" | "hail" | "checkpoint_ready" | "report"
  | "order" | "answer" | "checkpoint" | "recall" | "belay";

/**
 * Envelope of every comms line. `seq` is per direction: on beam->bridge
 * messages it is the outbox seq (from 1, monotonic across combadge restarts);
 * on bridge->beam commands it is the command id the combadge dedups on, except
 * for `ack`. `order` is required on order-bound types and optional otherwise.
 */
export type Message<T extends MessageType = MessageType> = {
  [K in T]: {
    v: number;
    seq: number;
    crew: string;
    type: K;
    /** RFC 3339. */
    ts: string;
    body: Bodies[K];
  } & (K extends OrderBoundType ? { order: string } : { order?: string });
}[T];

export type BeamToBridgeMessage = Message<BeamToBridgeType>;
export type BridgeToBeamMessage = Message<BridgeToBeamType>;
