import * as fs from "node:fs";
import * as net from "node:net";
import * as os from "node:os";
import * as path from "node:path";
import { parseArgs } from "node:util";
import { BudgetExceeded, escalate, Stopped } from "./harness/common.js";
import { harnesses } from "./harness/index.js";
import { loadEnvFile } from "./env.js";
import { acquireLock } from "./lock.js";
import { ROLES, type Harness, type Inbound, type Role, type SelfCheck } from "./harness/types.js";
import { onLines } from "./lines.js";
import { Outbox } from "./outbox.js";
import { bundleWork, checkoutRepos, previousCheckout, wipNote } from "./repos.js";
import {
  PROTOCOL_VERSION,
  type BeamToBridgeBodies,
  type BeamToBridgeType,
  type BridgeToBeamMessage,
  type ConsultReplyBody,
  type Message,
  type ReportBody,
  type UsageBody,
} from "./protocol.js";
import { State } from "./state.js";
import { VERSION } from "./version.js";

export interface ServeOptions {
  /** Unix socket attach connects to. A stale file there is removed. */
  socket: string;
  /** Crew member id stamped on every outbound message. */
  crew: string;
  /** Outbox NDJSON file; state.json is kept next to it. */
  outbox: string;
  /** Holds one dir per order: its repo checkouts (repos.ts) and the harness's checkpoints. */
  workDir: string;
  harness: Harness;
  /** Heartbeat period in ms. Default 10s. */
  heartbeatIntervalMs?: number;
}

export interface Server {
  close(): Promise<void>;
  /**
   * Resolves when serve ends itself: after a failed harness check, once an
   * attach has drained its replay, or CHECK_FAILED_EXIT_MS after the check
   * with no attach. Otherwise it never resolves.
   */
  ended: Promise<void>;
}

/** The crew-level error code of a failed harness check (#172). */
export const HARNESS_CHECK_FAILED = "harness_check_failed";

/** How long serve waits for an attach to read a failed check before it exits anyway. */
export const CHECK_FAILED_EXIT_MS = 10 * 60_000;

interface Run {
  id: string;
  listeners: ((m: Inbound) => void)[];
  /** Whether harness.run has been called; until then inbound commands wait in `pending`. */
  started: boolean;
  pending: Inbound[];
}

/** Types relayed to the running harness; everything else bridge->beam is handled here. */
const RELAYED = new Set<string>(["answer", "checkpoint", "stand_down", "recall", "belay", "correction"]);

/** Consults a harness runs at once; one more is escalated at once (Command is busy). */
export const MAX_CONSULTS = 4;

/** A live connection buffering more than this is dropped; the pump reconnects with --since. */
const MAX_BUFFERED = 8 << 20;

/**
 * The long-running in-beam process (plan §5). Writes every beam->bridge message
 * to the outbox, serves it to attach connections over a Unix socket, runs
 * orders through the harness, answers consults beside them (handleConsult),
 * and emits heartbeats.
 *
 * Attach handshake: a connection's first line is `{"since":N}` (combadge
 * internal, not a protocol message). serve replays outbox entries with
 * seq > N, then streams new ones live and appends a `hello`. Every later line
 * from any connection is an inbound command.
 *
 * Harness check (#172): serve runs the harness's selfCheck once at startup.
 * The socket opens at once, but no attach is served until the check is
 * done: its handshake, and commands after it, wait. A pass or an
 * inconclusive result (logged) changes nothing. A fail is written to the
 * outbox as a crew-level error with code harness_check_failed and the
 * check's reason; serve then sends no hello, drops every command, and ends
 * (Server.ended) once an attach has drained its replay, or after
 * CHECK_FAILED_EXIT_MS with none.
 *
 * Only one serve may use an outbox: a second one fails with "already running"
 * if the socket accepts a connection or the lock next to the outbox is held.
 */
export async function serve(opts: ServeOptions): Promise<Server> {
  if (await accepts(opts.socket)) throw new Error(`already running: ${opts.socket} accepts connections`);
  const dir = path.dirname(opts.outbox);
  fs.mkdirSync(dir, { recursive: true });
  const release = acquireLock(path.join(dir, "serve.lock"));
  try {
    return await start(opts, release);
  } catch (err) {
    release();
    throw err;
  }
}

async function start(opts: ServeOptions, release: () => void): Promise<Server> {
  const outbox = new Outbox(opts.outbox);
  const state = new State(path.join(path.dirname(opts.outbox), "state.json"));
  const sockets = new Set<net.Socket>(); // every accepted connection
  const live = new Set<net.Socket>(); // connections past replay, getting new entries
  let run: Run | undefined;
  /** Aborted on close: every consult in flight is cut short, and its reply dropped. */
  const consults = new AbortController();
  let inFlight = 0;
  let closed = false;
  /** The harness check failed: no hello, no commands, and serve ends after the first replay. */
  let failed = false;
  let endServe!: () => void;
  const ended = new Promise<void>((resolve) => (endServe = resolve));
  let exitTimer: NodeJS.Timeout | undefined;

  function emit<T extends BeamToBridgeType>(type: T, body: BeamToBridgeBodies[T], order?: string): void {
    // Kept before the outbox line: a crash in between leaves state ahead of
    // the outbox, never behind it, so the counts never go back (#117).
    if (type === "usage" && order !== undefined) state.record({ usage: { order, counts: body as UsageBody } });
    const msg = { v: PROTOCOL_VERSION, crew: opts.crew, order, type, ts: new Date().toISOString(), body };
    let line: string;
    try {
      ({ line } = outbox.append(msg as Omit<Message<BeamToBridgeType>, "seq">));
    } catch (err) {
      console.error(`combadge serve: fatal: outbox write failed: ${String(err)}`);
      process.exit(1);
    }
    for (const c of live) {
      c.write(line);
      if (c.writableLength > MAX_BUFFERED) {
        warn(`dropping attach connection: ${c.writableLength} bytes unsent`);
        c.destroy();
      }
    }
  }

  function startOrder(msg: Message<"order">): void {
    const id = msg.order;
    if (!id) {
      state.record({ handled: msg.seq });
      return warn(`order command ${msg.seq} has no order id`);
    }
    if (run?.id === id || state.isCompleted(id)) {
      state.record({ handled: msg.seq });
      return warn(`ignoring order command ${msg.seq}: ${id} is ${run?.id === id ? "already running" : "already done"}`);
    }
    if (run) {
      state.record({ handled: msg.seq });
      return emit("error", { reason: `already running ${run.id}`, code: "busy" }, id);
    }
    const resumed = state.isStopped(id);
    state.record({ handled: msg.seq, running: id });
    const current: Run = { id, listeners: [], started: false, pending: [] };
    run = current;
    let done = false;
    // Emits the run's final message before recording the order as ended, as
    // the restart path does: a crash in between yields report (or error)
    // followed by combadge_restarted, which the bridge can ignore, rather than
    // losing the report.
    //
    // An error after a stand_down or recall was relayed (the harness couldn't
    // checkpoint, say) records the order stopped, not completed: the bridge
    // parks it on its earlier checkpoints and may send it here again.
    const finish = <T extends "report" | "error">(type: T, body: BeamToBridgeBodies[T], err?: unknown) => {
      done = true;
      run = undefined;
      emit(type, body, id);
      // A budget stop (#119) ends the order whatever stop came meanwhile.
      const stopped = type === "error" && state.stopping === id && !(err instanceof BudgetExceeded);
      state.record(stopped ? { running: null, stopped: id } : { running: null, completed: id });
    };
    // A stand_down or recall ended the run after its stopping checkpoint:
    // nothing to emit, and the order is neither running nor completed, so a
    // restart reports nothing and a later order command runs it again.
    const stop = () => {
      done = true;
      run = undefined;
      state.record({ running: null, stopped: id });
    };
    // The repos are checked out first, for every harness; a failed checkout
    // fails the order. Inbound commands arriving meanwhile (a belay, say) wait
    // in `pending` and reach the harness once run has registered its
    // listeners, which it does before its first await. When the run resolves,
    // each repo's bmus/<order> is bundled into <work-dir>/<order>/out/ for the
    // bridge to pull (#53); a failed bundle fails the order. An order that
    // was stopped runs again from a clean order dir (see clearStopped).
    const result = (async (): Promise<ReportBody> => {
      if (resumed) await clearStopped(opts.workDir, id);
      const earlier = state.usageOf(id);
      const repos = await checkoutRepos(opts.workDir, id, Array.isArray(msg.body.repos) ? msg.body.repos : []);
      // Work in progress checkoutRepos found or restored goes into the handover note (#145).
      const wip = wipNote(id, repos);
      const body = wip.length === 0 ? msg.body : { ...msg.body, handover: [msg.body.handover, ...wip].filter(Boolean).join("\n\n") };
      const started = opts.harness.run(
        { id, body, repos, ...(earlier !== undefined && { usage: earlier }) },
        (m) => (done ? warn(`dropping ${m.type} emitted after ${id} ended`) : emit(m.type, m.body, id)),
        (fn) => current.listeners.push(fn),
      );
      current.started = true;
      for (const m of current.pending.splice(0)) deliver(current, m);
      const { summary, artifacts, usage } = await started;
      const out = path.join(path.resolve(opts.workDir), id, "out");
      const work = [];
      for (const r of repos) work.push(await bundleWork(r, id, path.join(out, `${r.name}.bundle`)));
      return { summary, repos: work, ...(artifacts && { artifacts }), usage };
    })();
    result.then(
      (report) => finish("report", report),
      (err: unknown) =>
        err instanceof Stopped
          ? stop()
          : finish(
              "error",
              { reason: err instanceof Error ? err.message : String(err), ...(err instanceof BudgetExceeded && { code: err.code }) },
              err,
            ),
    );
  }

  function relay(msg: Message<Inbound["type"]>): void {
    if (!run || (msg.order !== undefined && msg.order !== run.id)) {
      state.record({ handled: msg.seq });
      return warn(`dropping ${msg.type} ${msg.seq} for ${msg.order ?? "no order"}: running ${run?.id ?? "nothing"}`);
    }
    const stopping = msg.type === "stand_down" || msg.type === "recall";
    state.record({ handled: msg.seq, ...(stopping && { stopping: run.id }) });
    const payload = { type: msg.type, body: msg.body } as Inbound;
    if (run.started) deliver(run, payload);
    else run.pending.push(payload);
  }

  function deliver(r: Run, payload: Inbound): void {
    for (const fn of r.listeners) {
      try {
        fn(payload);
      } catch (err) {
        warn(`harness inbound handler threw: ${String(err)}`);
      }
    }
  }

  /**
   * Command's consult lane (D7; phase 3 plan §5), apart from `run`: a consult
   * is answered whether an order runs or not, and up to MAX_CONSULTS may be
   * in flight at once; one past that is escalated at once. The command is
   * recorded handled on arrival, as an order is: one lost to a restart gets
   * no reply, and the bridge's own consult timeout sends the hail to the
   * captain.
   */
  function handleConsult(msg: Message<"consult">): void {
    state.record({ handled: msg.seq });
    const req = msg.body;
    if (typeof req.hail_id !== "string" || req.hail_id === "") return warn(`ignoring consult ${msg.seq} with no hail_id`);
    const hailId = req.hail_id;
    const { consult, name } = opts.harness;
    if (!consult) return emit("consult_reply", escalate(hailId, `harness ${name} does not run a consult lane`));
    if (inFlight >= MAX_CONSULTS) return emit("consult_reply", escalate(hailId, `Command busy (${inFlight} consults in flight)`));
    inFlight++;
    const reply = (body: ConsultReplyBody) => {
      inFlight--;
      if (!closed) emit("consult_reply", { ...body, hail_id: hailId });
    };
    Promise.resolve()
      .then(() => consult.call(opts.harness, req, consults.signal))
      .then(reply, (err: unknown) => reply(escalate(hailId, `consult failed: ${err instanceof Error ? err.message : String(err)}`)));
  }

  function handleCommand(line: string): void {
    if (failed) return warn(`dropping a command: the harness check failed`);
    let msg: BridgeToBeamMessage;
    try {
      msg = JSON.parse(line) as BridgeToBeamMessage;
    } catch {
      return warn(`ignoring unparseable command line`);
    }
    if (!isObject(msg) || typeof msg.type !== "string" || !Number.isSafeInteger(msg.seq) || !isObject(msg.body)) {
      return warn(`ignoring malformed command`);
    }
    // Acks are idempotent and not deduped (D3); every other command is, by id.
    if (msg.type === "ack") {
      const acked = msg.body.seq;
      if (!Number.isSafeInteger(acked) || acked < 0) return warn(`ignoring ack with bad seq`);
      if (acked > outbox.lastSeq) return warn(`ignoring ack of ${acked}, above last seq ${outbox.lastSeq}`);
      if (acked > state.acked) state.record({ acked });
      return;
    }
    if (state.isHandled(msg.seq)) return;
    if (msg.type === "order") startOrder(msg);
    else if (msg.type === "consult") handleConsult(msg);
    else if (RELAYED.has(msg.type)) relay(msg as Message<Inbound["type"]>);
    else {
      state.record({ handled: msg.seq });
      warn(`ignoring unsupported command ${msg.type}`);
    }
  }

  /** Writes entries after since to conn, waiting on drain, then makes it live. */
  function replay(conn: net.Socket, since: number): void {
    let cursor = since;
    const step = () => {
      if (conn.destroyed) return;
      for (const e of outbox.since(cursor)) {
        cursor = e.seq;
        if (!conn.write(e.line)) return void conn.once("drain", step);
      }
      // Caught up synchronously, so no entry can slip between replay and live.
      live.add(conn);
      if (failed) {
        // The replay held the check's error: serve ends once it is on its way.
        const done = () => {
          clearTimeout(exitTimer);
          warn(`an attach read the failed harness check; exiting`);
          endServe();
        };
        if (conn.writableLength === 0) done();
        else conn.once("drain", done);
        return;
      }
      const model = opts.harness.model;
      emit("hello", {
        protocol: PROTOCOL_VERSION,
        harness: opts.harness.name,
        combadge_version: VERSION,
        ...(model !== undefined && model !== "" && { model }),
      });
    };
    step();
  }

  function accept(conn: net.Socket): void {
    sockets.add(conn);
    conn.on("error", (err) => warn(`attach connection: ${err.message}`));
    conn.on("close", () => {
      sockets.delete(conn);
      live.delete(conn);
    });
    let handshaken = false;
    /** Commands that came after the handshake while the harness check ran. */
    let waiting: string[] | undefined;
    const command = (line: string) => {
      try {
        handleCommand(line);
      } catch (err) {
        warn(`command failed: ${err instanceof Error ? err.message : String(err)}`);
      }
    };
    onLines(conn, (line) => {
      if (conn.destroyed) return;
      if (handshaken) {
        if (waiting) waiting.push(line);
        else command(line);
        return;
      }
      const since = parseHandshake(line);
      if (since === undefined) {
        warn(`bad attach handshake`);
        conn.destroy();
        return;
      }
      handshaken = true;
      waiting = [];
      void checked.then(() => {
        if (conn.destroyed || closed) return;
        replay(conn, since);
        for (const l of waiting!.splice(0)) command(l);
        waiting = undefined;
      });
    });
  }

  if (state.running !== undefined) {
    const lost = state.running;
    emit("error", { reason: "combadge restarted while running this order", code: "combadge_restarted" }, lost);
    // One being stopped stays resendable, as after any error during a stop.
    state.record(state.stopping === lost ? { running: null, stopped: lost } : { running: null, completed: lost });
  }

  /** Runs the harness check; attaches are served once it settles (see serve). */
  async function runCheck(): Promise<void> {
    if (!opts.harness.selfCheck) return;
    let r: SelfCheck;
    try {
      r = await opts.harness.selfCheck();
    } catch (err) {
      r = { result: "inconclusive", reason: `the check threw: ${err instanceof Error ? err.message : String(err)}` };
    }
    if (closed) return;
    for (const note of r.notes ?? []) warn(`harness check: ${note}`);
    if (r.result === "pass") return warn(`harness check passed: a PreToolUse ask on AskUserQuestion reaches canUseTool`);
    if (r.result === "inconclusive") return warn(`harness check inconclusive; going on: ${r.reason}`);
    failed = true;
    warn(`harness check failed: ${r.reason}; exiting once an attach has read it, or in ${CHECK_FAILED_EXIT_MS / 60_000}m`);
    emit("error", { reason: r.reason, code: HARNESS_CHECK_FAILED });
    exitTimer = setTimeout(() => {
      warn(`no attach read the failed harness check; exiting`);
      endServe();
    }, CHECK_FAILED_EXIT_MS);
  }
  const checked = runCheck();

  fs.rmSync(opts.socket, { force: true }); // stale: nothing accepted on it above
  const server = net.createServer(accept);
  await new Promise<void>((resolve, reject) => {
    server.once("error", reject);
    server.listen(opts.socket, () => {
      server.off("error", reject);
      resolve();
    });
  });

  const outboxDir = path.dirname(opts.outbox);
  const beat = setInterval(() => {
    fs.promises.statfs(outboxDir).then(
      (st) => {
        if (closed) return;
        emit("heartbeat", {
          disk_free_bytes: st.bavail * st.bsize,
          mem_free_bytes: os.freemem(),
          outbox_depth: outbox.depth(state.acked),
        });
      },
      (err: Error) => warn(`heartbeat statfs: ${err.message}`),
    );
  }, opts.heartbeatIntervalMs ?? 10_000);

  return {
    ended,
    async close() {
      closed = true;
      clearTimeout(exitTimer);
      consults.abort();
      clearInterval(beat);
      for (const c of sockets) c.destroy();
      await new Promise<void>((resolve) => server.close(() => resolve()));
      outbox.close();
      release();
    },
  };
}

/**
 * Removes everything under `<workDir>/<order>/` but `in/`: the repo
 * checkouts, `out/` and the checkpoint dirs of a stopped run. `in/` holds the
 * bundles the bridge shipped for the order's next run, from its latest
 * checkpoint. Each repo checkout is set aside as `.<name>.prev` instead
 * (previousCheckout), so checkoutRepos can take its work in progress from it
 * and then removes it (#145).
 */
async function clearStopped(workDir: string, order: string): Promise<void> {
  const dir = path.join(path.resolve(workDir), order);
  let entries: string[];
  try {
    entries = await fs.promises.readdir(dir);
  } catch (err) {
    if ((err as NodeJS.ErrnoException).code === "ENOENT") return;
    throw err;
  }
  for (const e of entries) {
    if (e === "in") continue;
    const p = path.join(dir, e);
    // Repo names never start with "." and are never "out" or all digits.
    if (!e.startsWith(".") && e !== "out" && !/^[0-9]+$/.test(e) && fs.existsSync(path.join(p, ".git"))) {
      const prev = previousCheckout(p);
      await fs.promises.rm(prev, { recursive: true, force: true });
      await fs.promises.rename(p, prev);
      continue;
    }
    if (e.endsWith(".prev")) continue; // set aside by an earlier clear whose run never checked out
    await fs.promises.rm(p, { recursive: true, force: true });
  }
}

/** Whether something accepts connections on socket. */
function accepts(socket: string): Promise<boolean> {
  return new Promise((resolve) => {
    const c = net.connect(socket);
    c.once("connect", () => {
      c.destroy();
      resolve(true);
    });
    c.once("error", () => resolve(false));
  });
}

function isObject(v: unknown): v is Record<string, unknown> {
  return typeof v === "object" && v !== null && !Array.isArray(v);
}

function parseHandshake(line: string): number | undefined {
  try {
    const since: unknown = (JSON.parse(line) as { since?: unknown }).since;
    return Number.isSafeInteger(since) && (since as number) >= 0 ? (since as number) : undefined;
  } catch {
    return undefined;
  }
}

function warn(msg: string): void {
  console.error(`combadge serve: ${msg}`);
}

const UNIT_MS = { ms: 1, s: 1000, m: 60_000, h: 3_600_000 } as const;

/** The shortest --checkpoint-every but 0 (#145): a checkpoint costs a snapshot, bundles and a pull. */
export const MIN_CHECKPOINT_EVERY_MS = 60_000;

/** The longest delay setTimeout takes (2^31-1 ms, about 596h). */
const MAX_TIMEOUT_MS = 2 ** 31 - 1;

/** Milliseconds in a duration like Go's: `15m`, `90s`, `1h30m`, `500ms`. NaN when it isn't one. */
export function parseDuration(text: string): number {
  if (!/^(\d+(\.\d+)?(ms|s|m|h))+$/.test(text)) return NaN;
  let ms = 0;
  for (const [, n, unit] of text.matchAll(/(\d+(?:\.\d+)?)(ms|s|m|h)/g)) ms += Number(n) * UNIT_MS[unit as keyof typeof UNIT_MS];
  return Math.round(ms);
}

/**
 * `combadge serve --socket <path> --crew <id> [--outbox <path>]
 * [--harness <name>] [--heartbeat-interval <ms>] [--work-dir <path>]
 * [--script <path> [--step-log <path>]] [--claude-path <path>]
 * [--hail-park-after <duration>] [--checkpoint-every <duration>]
 * [--checkpoint-turns <n>] [--check-model <model>] [--env-file <path>]
 * [--role command]`.
 * --script and --step-log are holodeck's (see harness/holodeck.ts); --script
 * is required with it. --claude-path, --hail-park-after (default 15m, see
 * parseDuration), --checkpoint-every and --checkpoint-turns (the checkpoint
 * cadence, #145: default 15m and 10 turns, 0 turns either off; the
 * interval is at least 1m) and --check-model (the startup check's model, the SDK's
 * default when unset; #172) are sdk's (see harness/sdk.ts). --role
 * command is passed by the bridge for Command's crew members, whatever their
 * harness; only sdk uses it (split_order, #155).
 * --work-dir holds one dir per order, with its repo checkouts and
 * checkpoints, by default `<outbox dir>/<harness>`. --env-file loads `env -0`
 * output into serve's environment before the harness is built (see env.ts). Runs
 * until SIGTERM/SIGINT.
 */
export async function main(argv: string[]): Promise<void> {
  const { values } = parseArgs({
    args: argv,
    options: {
      socket: { type: "string" },
      crew: { type: "string" },
      outbox: { type: "string", default: path.join(os.homedir(), "combadge", "outbox.ndjson") },
      harness: { type: "string", default: "sdk" },
      "heartbeat-interval": { type: "string", default: "10000" },
      script: { type: "string" },
      "step-log": { type: "string" },
      "work-dir": { type: "string" },
      "claude-path": { type: "string" },
      "hail-park-after": { type: "string" },
      "checkpoint-every": { type: "string" },
      "checkpoint-turns": { type: "string" },
      "check-model": { type: "string" },
      "env-file": { type: "string" },
      role: { type: "string" },
    },
    strict: true,
  });
  if (values["env-file"] !== undefined) loadEnvFile(values["env-file"]);
  if (!values.socket) throw new Error("--socket is required");
  if (!values.crew) throw new Error("--crew is required");
  const factory = Object.hasOwn(harnesses, values.harness) ? harnesses[values.harness] : undefined;
  if (!factory) {
    throw new Error(`unknown harness ${values.harness} (available: ${Object.keys(harnesses).join(", ")})`);
  }
  const only: [flag: "script" | "step-log" | "claude-path" | "hail-park-after" | "checkpoint-every" | "checkpoint-turns" | "check-model", harnesses: string[]][] = [
    ["script", ["holodeck"]],
    ["step-log", ["holodeck"]],
    ["claude-path", ["sdk"]],
    ["hail-park-after", ["sdk"]],
    ["checkpoint-every", ["sdk"]],
    ["checkpoint-turns", ["sdk"]],
    ["check-model", ["sdk"]],
  ];
  for (const [flag, allowed] of only) {
    if (values[flag] !== undefined && !allowed.includes(values.harness)) {
      throw new Error(`--${flag} is only for --harness ${allowed.join(" or ")}`);
    }
  }
  const role = values.role;
  if (role !== undefined && !(ROLES as readonly string[]).includes(role)) {
    throw new Error(`unknown role ${role} (roles: ${ROLES.join(", ")})`);
  }
  const parkAfter = values["hail-park-after"];
  const hailParkAfterMs = parkAfter === undefined ? undefined : parseDuration(parkAfter);
  if (hailParkAfterMs !== undefined && !(hailParkAfterMs > 0)) {
    throw new Error(`--hail-park-after ${parkAfter} is not a positive duration (e.g. 15m, 90s, 1h30m)`);
  }
  // Node runs a longer setTimeout after 1ms.
  if (hailParkAfterMs !== undefined && hailParkAfterMs > MAX_TIMEOUT_MS) throw new Error(`--hail-park-after ${parkAfter} is too long (at most 596h)`);
  const every = values["checkpoint-every"];
  // 0 (Go prints it 0s) turns the time trigger off (#145).
  const checkpointEveryMs = every === undefined ? undefined : every === "0" ? 0 : parseDuration(every);
  if (checkpointEveryMs !== undefined && !(checkpointEveryMs >= 0)) {
    throw new Error(`--checkpoint-every ${every} is not a duration (e.g. 15m, 1h; 0 turns it off)`);
  }
  if (checkpointEveryMs !== undefined && checkpointEveryMs > 0 && checkpointEveryMs < MIN_CHECKPOINT_EVERY_MS) {
    throw new Error(`--checkpoint-every ${every} is too short (at least 1m; 0 turns it off)`);
  }
  if (checkpointEveryMs !== undefined && checkpointEveryMs > MAX_TIMEOUT_MS) throw new Error(`--checkpoint-every ${every} is too long (at most 596h)`);
  const turns = values["checkpoint-turns"];
  const checkpointTurns = turns === undefined ? undefined : /^[0-9]+$/.test(turns) ? Number(turns) : NaN;
  if (checkpointTurns !== undefined && !Number.isSafeInteger(checkpointTurns)) {
    throw new Error(`--checkpoint-turns ${turns} is not a whole number of turns (0 turns it off)`);
  }
  const workDir = path.resolve(values["work-dir"] ?? path.join(path.dirname(values.outbox), values.harness));
  // Built before serve starts, so a bad holodeck script or sdk flag fails here.
  const harness = await factory({
    workDir,
    ...(values.script !== undefined && { script: values.script }),
    ...(values["step-log"] !== undefined && { stepLog: values["step-log"] }),
    ...(values["claude-path"] !== undefined && { claudePath: values["claude-path"] }),
    ...(hailParkAfterMs !== undefined && { hailParkAfterMs }),
    ...(checkpointEveryMs !== undefined && { checkpointEveryMs }),
    ...(checkpointTurns !== undefined && { checkpointTurns }),
    ...(values["check-model"] !== undefined && { checkModel: values["check-model"] }),
    ...(role !== undefined && { role: role as Role }),
  });
  const interval = Number(values["heartbeat-interval"]);
  if (!Number.isSafeInteger(interval) || interval <= 0) throw new Error("--heartbeat-interval must be a positive integer (ms)");

  const server = await serve({
    socket: values.socket,
    crew: values.crew,
    outbox: values.outbox,
    workDir,
    harness,
    heartbeatIntervalMs: interval,
  });
  const stop = () => void server.close().then(() => process.exit(0));
  process.once("SIGTERM", stop);
  process.once("SIGINT", stop);
  // A failed harness check (#172): exit 1 once the bridge has it.
  void server.ended.then(() => server.close()).then(() => process.exit(1));
}
