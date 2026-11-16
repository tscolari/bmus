// In-process tests of serve with a fake harness: hello's model, the
// order's model and its handover as the harness gets them.
import * as fs from "node:fs";
import * as net from "node:net";
import * as path from "node:path";
import { afterEach, describe, expect, it } from "vitest";
import { chainNote, startedFrom } from "../src/harness/common.js";
import type { Harness, Order } from "../src/harness/types.js";
import { onLines } from "../src/lines.js";
import type { BeamToBridgeMessage, OrderBody } from "../src/protocol.js";
import { serve, type Server } from "../src/serve.js";
import { keptWipRef } from "../src/repos.js";
import { git, orderBundle } from "./gitfix.js";
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
async function attach(harness: Harness, dir = tempDir()) {
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

  it("gives the harness the bridge's handover apart: a first run's kept WIP line stays out of a harness note's chain (#174)", async () => {
    const dir = tempDir();
    const { repo } = orderBundle(dir, "ORD-0001");
    // The shipped bundle carries a kept WIP, which serve names in the handover.
    const src = path.join(dir, "src-app");
    git(src, "checkout", "-q", "--detach", "main~1");
    fs.writeFileSync(path.join(src, "kept.txt"), "kept\n");
    git(src, "add", "kept.txt");
    git(src, "commit", "-q", "-m", "wip");
    git(src, "update-ref", keptWipRef("ORD-0001"), "HEAD");
    git(src, "bundle", "create", "--quiet", repo.bundle_path, "refs/heads/bmus/ORD-0001", keptWipRef("ORD-0001"));
    const h = fakeHarness();
    const a = await attach(h, dir);
    a.send({ v: 1, seq: 1, crew: "sorell", order: "ORD-0001", type: "order", ts: new Date().toISOString(), body: { ...orderBody, repos: [repo] } });
    await a.next((m) => m.type === "report" && m.order === "ORD-0001");
    const order = h.orders[0]!;
    expect(order.body.handover).toContain("holds uncommitted work kept from an earlier run");
    expect(order.sentHandover).toBe("");
    const note = chainNote("Order ORD-0001: a cadence checkpoint.", startedFrom(order), undefined);
    expect(note).not.toContain("kept from an earlier run");
    expect(note).toContain("The agent has written no handover note yet.");
    a.close();
  });
});
