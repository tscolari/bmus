// In-process tests of serve with a fake harness: hello's model and the
// order's model as the harness gets them.
import * as net from "node:net";
import * as path from "node:path";
import { afterEach, describe, expect, it } from "vitest";
import type { Harness, Order } from "../src/harness/types.js";
import { onLines } from "../src/lines.js";
import type { BeamToBridgeMessage, OrderBody } from "../src/protocol.js";
import { serve, type Server } from "../src/serve.js";
import { cleanup, tempDir } from "./procs.js";
import { compileSchema } from "./schema.js";

const { validate } = compileSchema();
const servers: Server[] = [];

afterEach(async () => {
  for (const s of servers.splice(0)) await s.close();
  await cleanup();
});

const orderBody: OrderBody = { title: "t", body: "b", repos: [], budget_tokens: 0 };

/** A harness that records each order it gets and reports at once. */
function fakeHarness(model?: string): Harness & { orders: Order[] } {
  const orders: Order[] = [];
  return {
    name: "fake",
    ...(model !== undefined && { model }),
    orders,
    run(order) {
      orders.push(order);
      return Promise.resolve({ summary: "ok", branch: `bmus/${order.id}`, usage: { input_tokens: 0, output_tokens: 0 } });
    },
  };
}

/** Starts serve with harness and attaches from 0; returns the attach's messages and a way to send. */
async function attach(harness: Harness) {
  const dir = tempDir();
  const socket = path.join(dir, "combadge.sock");
  servers.push(
    await serve({ socket, crew: "sorell", outbox: path.join(dir, "outbox.ndjson"), workDir: path.join(dir, "work"), harness, heartbeatIntervalMs: 60_000 }),
  );
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
  return {
    async next(pred: (m: BeamToBridgeMessage) => boolean): Promise<BeamToBridgeMessage> {
      for (;;) {
        const i = got.findIndex(pred);
        if (i >= 0) return got.splice(i, 1)[0]!;
        await new Promise<void>((r) => waiters.push(r));
      }
    },
    send: (m: object) => conn.write(JSON.stringify(m) + "\n"),
    close: () => conn.destroy(),
  };
}

describe("serve and models", () => {
  it("reports the harness's default model in hello, and none when it has none", async () => {
    const a = await attach(fakeHarness("model-default"));
    expect((await a.next((m) => m.type === "hello")).body).toMatchObject({ harness: "fake", model: "model-default" });
    a.close();
    const b = await attach(fakeHarness());
    expect((await b.next((m) => m.type === "hello")).body).not.toHaveProperty("model");
    b.close();
  });

  it("hands the order's model to the harness as it came", async () => {
    const h = fakeHarness();
    const a = await attach(h);
    a.send({ v: 1, seq: 1, crew: "sorell", order: "ORD-0001", type: "order", ts: new Date().toISOString(), body: { ...orderBody, model: "model-x" } });
    await a.next((m) => m.type === "report" && m.order === "ORD-0001");
    a.send({ v: 1, seq: 2, crew: "sorell", order: "ORD-0002", type: "order", ts: new Date().toISOString(), body: orderBody });
    await a.next((m) => m.type === "report" && m.order === "ORD-0002");
    expect(h.orders.map((o) => o.body.model)).toEqual(["model-x", undefined]);
    a.close();
  });
});
