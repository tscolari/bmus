// In-process tests of serve's consult lane (phase 3 plan §5) with fake
// harnesses: consults are answered beside a running order without touching it.
import * as fs from "node:fs";
import * as net from "node:net";
import * as path from "node:path";
import { afterEach, describe, expect, it } from "vitest";
import type { Harness, HarnessReport, Inbound } from "../src/harness/types.js";
import { onLines } from "../src/lines.js";
import type { BeamToBridgeMessage, ConsultBody, ConsultReplyBody, OrderBody } from "../src/protocol.js";
import { MAX_CONSULTS, serve, type Server } from "../src/serve.js";
import { cleanup, tempDir } from "./procs.js";
import { compileSchema } from "./schema.js";

const { validate } = compileSchema();
const servers: Server[] = [];

afterEach(async () => {
  for (const s of servers.splice(0)) await s.close();
  await cleanup();
});

const ORDER = "ORD-0001";
const orderBody: OrderBody = { title: "t", body: "b", repos: [], budget_tokens: 0 };
const consultBody = (hailId: string): ConsultBody => ({
  hail_id: hailId, crew: "tomas", order: "ORD-0042", category: "clarifying_question", question: "Which one?", options: ["a", "b"],
});

/** Resolves when settle is called; settle's argument is the value. */
function deferred<T>() {
  let settle!: (v: T) => void;
  const promise = new Promise<T>((r) => (settle = r));
  return { promise, settle };
}

/**
 * A harness whose run holds until released, recording what reaches it, and
 * whose consult (when given) is the test's.
 */
function fakeHarness(consult?: Harness["consult"]) {
  const release = deferred<HarnessReport>();
  const started = deferred<void>();
  const inbound: Inbound[] = [];
  const harness: Harness = {
    name: "fake",
    ...(consult && { consult }),
    run(_order, _emit, onInbound) {
      onInbound((m) => inbound.push(m));
      started.settle();
      return release.promise;
    },
  };
  return { harness, release: release.settle, started: started.promise, inbound };
}

async function attach(harness: Harness) {
  const dir = tempDir();
  const socket = path.join(dir, "combadge.sock");
  const server = await serve({ socket, crew: "kirkland", outbox: path.join(dir, "outbox.ndjson"), workDir: path.join(dir, "work"), harness, heartbeatIntervalMs: 60_000 });
  servers.push(server);
  const conn = net.connect(socket);
  const got: BeamToBridgeMessage[] = [];
  const waiters: (() => void)[] = [];
  onLines(conn, (line) => {
    const m = JSON.parse(line) as BeamToBridgeMessage;
    if (!validate(m)) throw new Error(`invalid message from serve: ${line}`);
    got.push(m);
    for (const w of waiters.splice(0)) w();
  });
  conn.write(`{"since":0}\n`);
  let seq = 0;
  return {
    server,
    outbox: path.join(dir, "outbox.ndjson"),
    got,
    async next(pred: (m: BeamToBridgeMessage) => boolean): Promise<BeamToBridgeMessage> {
      for (;;) {
        const i = got.findIndex(pred);
        if (i >= 0) return got.splice(i, 1)[0]!;
        await new Promise<void>((r) => waiters.push(r));
      }
    },
    send(type: string, body: object, order?: string) {
      const m = { v: 1, seq: ++seq, crew: "kirkland", ...(order && { order }), type, ts: new Date().toISOString(), body };
      if (!validate(m)) throw new Error(`invalid command in test: ${JSON.stringify(validate.errors)}`);
      conn.write(JSON.stringify(m) + "\n");
    },
    close: () => conn.destroy(),
  };
}

const replyFor = (hailId: string) => (m: BeamToBridgeMessage) => m.type === "consult_reply" && m.body.hail_id === hailId;

describe("serve consult lane", () => {
  it("answers consults while an order runs, concurrently, without touching the run", async () => {
    const pending = new Map<string, (r: ConsultReplyBody) => void>();
    const seen: ConsultBody[] = [];
    const fake = fakeHarness((req) => {
      seen.push(req);
      return new Promise((resolve) => pending.set(req.hail_id, resolve));
    });
    const a = await attach(fake.harness);
    a.send("order", orderBody, ORDER);
    await fake.started;

    a.send("consult", consultBody("H-0001"));
    a.send("consult", consultBody("H-0002"));
    while (pending.size < 2) await new Promise((r) => setTimeout(r, 5));
    expect(seen.map((r) => r.hail_id)).toEqual(["H-0001", "H-0002"]);
    // The second one in answers first: neither waits on the other.
    pending.get("H-0002")!({ hail_id: "ignored", decision: "answer", option: 1, rationale: "b fits", confidence: 0.9 });
    const second = await a.next(replyFor("H-0002"));
    expect(second).not.toHaveProperty("order");
    expect(second.body).toEqual({ hail_id: "H-0002", decision: "answer", option: 1, rationale: "b fits", confidence: 0.9 });
    pending.get("H-0001")!({ hail_id: "H-0001", decision: "escalate", rationale: "unsure", confidence: 0.2 });
    expect((await a.next(replyFor("H-0001"))).body).toMatchObject({ decision: "escalate" });

    // The run saw nothing of either, and still ends with its own report.
    expect(fake.inbound).toEqual([]);
    expect(a.got.some((m) => m.order === ORDER)).toBe(false);
    fake.release({ summary: "done", usage: { input_tokens: 0, output_tokens: 0 } });
    expect((await a.next((m) => m.type === "report" && m.order === ORDER)).body).toMatchObject({ summary: "done" });
    a.close();
  });

  it("escalates at once from a harness without a consult lane, with or without an order running", async () => {
    const fake = fakeHarness();
    const a = await attach(fake.harness);
    a.send("consult", consultBody("H-0003"));
    const reply = await a.next(replyFor("H-0003"));
    expect(reply).not.toHaveProperty("order");
    expect(reply.body).toEqual({ hail_id: "H-0003", decision: "escalate", rationale: "harness fake does not run a consult lane", confidence: 0 });

    a.send("order", orderBody, ORDER);
    await fake.started;
    a.send("consult", consultBody("H-0004"));
    expect((await a.next(replyFor("H-0004"))).body).toMatchObject({ decision: "escalate", confidence: 0 });
    expect(fake.inbound).toEqual([]);
    a.close();
  });

  it("escalates a consult whose harness call throws or rejects", async () => {
    let calls = 0;
    const a = await attach(
      fakeHarness(() => {
        if (++calls === 1) throw new Error("boom");
        return Promise.reject(new Error("bang"));
      }).harness,
    );
    a.send("consult", consultBody("H-0005"));
    a.send("consult", consultBody("H-0006"));
    expect((await a.next(replyFor("H-0005"))).body).toEqual({ hail_id: "H-0005", decision: "escalate", rationale: "consult failed: boom", confidence: 0 });
    expect((await a.next(replyFor("H-0006"))).body).toMatchObject({ rationale: "consult failed: bang", confidence: 0 });
    a.close();
  });

  it("aborts consults in flight on close", async () => {
    const signals: AbortSignal[] = [];
    const a = await attach(
      fakeHarness((_req, signal) => {
        signals.push(signal);
        return new Promise(() => {});
      }).harness,
    );
    a.send("consult", consultBody("H-0007"));
    while (signals.length === 0) await new Promise((r) => setTimeout(r, 5));
    expect(signals[0]!.aborted).toBe(false);
    await servers.pop()!.close();
    expect(signals[0]!.aborted).toBe(true);
    a.close();
  });

  it(`escalates a consult past ${MAX_CONSULTS} in flight at once, and takes more once one ends`, async () => {
    const pending: ((r: ConsultReplyBody) => void)[] = [];
    const a = await attach(fakeHarness(() => new Promise((resolve) => pending.push(resolve))).harness);
    for (let i = 1; i <= MAX_CONSULTS + 1; i++) a.send("consult", consultBody(`H-1${i}`));
    expect((await a.next(replyFor(`H-1${MAX_CONSULTS + 1}`))).body).toEqual({
      hail_id: `H-1${MAX_CONSULTS + 1}`, decision: "escalate", rationale: `Command busy (${MAX_CONSULTS} consults in flight)`, confidence: 0,
    });
    expect(pending).toHaveLength(MAX_CONSULTS);
    pending[0]!({ hail_id: "H-11", decision: "escalate", rationale: "r", confidence: 0.1 });
    await a.next(replyFor("H-11"));
    a.send("consult", consultBody("H-20"));
    while (pending.length < MAX_CONSULTS + 1) await new Promise((r) => setTimeout(r, 5));
    a.close();
  });

  it("writes no consult_reply for a consult that resolves after close", async () => {
    const pending: ((r: ConsultReplyBody) => void)[] = [];
    const a = await attach(fakeHarness(() => new Promise((resolve) => pending.push(resolve))).harness);
    a.send("consult", consultBody("H-0008"));
    while (pending.length === 0) await new Promise((r) => setTimeout(r, 5));
    await servers.pop()!.close();
    pending[0]!({ hail_id: "H-0008", decision: "answer", option: 0, rationale: "late", confidence: 1 });
    await new Promise((r) => setTimeout(r, 50));
    const types = fs.readFileSync(a.outbox, "utf8").trim().split("\n").map((l) => (JSON.parse(l) as { type: string }).type);
    expect(types).toEqual(["hello"]);
    a.close();
  });
});
