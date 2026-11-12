// Process-level tests: serve and attach run as real `node combadge.mjs`
// processes (bundled once in global-setup.ts), as they do on a beam.
import * as fs from "node:fs";
import * as path from "node:path";
import { afterEach, describe, expect, it, vi } from "vitest";
import { echoHarness } from "../src/harness/echo.js";
import type { Inbound, Outbound } from "../src/harness/types.js";
import type { BeamToBridgeMessage, OrderBody } from "../src/protocol.js";
import { git, orderBundle } from "./gitfix.js";
import { Attach, CREW, cleanup, exited, outboxLastSeq, serveFails, sleep, startServe, tempDir } from "./procs.js";

afterEach(cleanup);

const orderBody: OrderBody = { title: "Widen scope?", body: "Test order.", repos: [], budget_tokens: 0 };
const ofType = (type: string, order?: string) => (m: BeamToBridgeMessage) =>
  m.type === type && (order === undefined || m.order === order);

describe("serve + attach", () => {
  it("replays to a later attach only what is newer than its --since", async () => {
    const dir = tempDir();
    fs.writeFileSync(path.join(dir, "combadge.sock"), "stale"); // serve must remove a stale socket file
    await startServe(dir);

    const first = new Attach(dir, 0);
    const seen: BeamToBridgeMessage[] = [];
    while (seen.length < 4) seen.push(await first.next());
    expect(seen[0]).toMatchObject({ seq: 1, crew: CREW }); // seq starts at 1
    expect(seen.map((m) => m.seq)).toEqual([1, 2, 3, 4]);
    expect(await first.close()).toBe(0);
    const lastSeen = seen.at(-1)!.seq;

    // Heartbeats keep landing in the outbox with nobody attached.
    while (outboxLastSeq(dir) < lastSeen + 2) await sleep(20);
    const before = outboxLastSeq(dir);

    const second = new Attach(dir, lastSeen);
    const got: BeamToBridgeMessage[] = [];
    // Everything up to `before` is replay; the hello after it is this attach's own.
    while (!got.some((m) => m.type === "hello" && m.seq > before)) got.push(await second.next());
    expect(got.length).toBeGreaterThan(2);
    expect(got.map((m) => m.seq)).toEqual(got.map((_, i) => lastSeen + 1 + i)); // contiguous, newer only
    expect(got.every((m) => m.type === "heartbeat" || m.type === "hello")).toBe(true);
    await second.close();
  });

  it("streams live to overlapping attaches and relays commands from either", async () => {
    const dir = tempDir();
    await startServe(dir, 60_000);
    const a = new Attach(dir, 0);
    await a.next(ofType("hello"));
    const b = new Attach(dir, 0);
    await b.next((m) => m.type === "hello" && m.seq === 2);

    // Order via b; the echo harness hails, and both connections see it.
    b.send({ seq: 1, order: "ORD-0001", type: "order", body: orderBody });
    const hail = await a.next(ofType("hail", "ORD-0001"));
    expect(await b.next(ofType("hail"))).toEqual(hail);
    if (hail.type !== "hail") throw new Error("unreachable");
    expect(hail.body.question).toBe("Widen scope?");

    // Answer via a; the harness resolves and serve emits the report.
    // A resend of the running order under a new command id is ignored, not a busy error.
    b.send({ seq: 5, order: "ORD-0001", type: "order", body: orderBody });
    a.send({
      seq: 2, order: "ORD-0001", type: "answer",
      body: { ref: hail.body.ref, hail_id: "H-0001", decided_by: "captain", decision: "Stay on target" },
    });
    const report = await b.next(ofType("report", "ORD-0001"));
    expect(report.body).toEqual({ summary: "Stay on target", repos: [], usage: { input_tokens: 0, output_tokens: 0 } });
    // So is a late resend of an order that already finished.
    b.send({ seq: 6, order: "ORD-0001", type: "order", body: orderBody });

    // A command id already handled is ignored, even for a new order.
    a.send({ seq: 1, order: "ORD-0002", type: "order", body: orderBody });
    // A rejected run becomes an error for its order.
    a.send({ seq: 3, order: "ORD-0003", type: "order", body: orderBody });
    a.send({ seq: 4, order: "ORD-0003", type: "belay", body: { reason: "captain changed course" } });
    const err = await a.next(ofType("error"));
    expect(err).toMatchObject({ order: "ORD-0003", body: { reason: "belayed: captain changed course" } });
    const outbox = fs.readFileSync(path.join(dir, "outbox.ndjson"), "utf8");
    expect(outbox).not.toContain("ORD-0002");
    expect(outbox).not.toContain("busy");
    expect(outbox.match(/"order":"ORD-0001","type":"progress"/g)).toHaveLength(1);

    await a.close();
    await b.close();
  });

  it("checks an order's repos out before the harness starts, and fails the order when it can't", async () => {
    const dir = tempDir();
    await startServe(dir, 60_000);
    const a = new Attach(dir, 0);
    const { repo, commit } = orderBundle(dir, "ORD-0001");
    a.send({ seq: 1, order: "ORD-0001", type: "order", body: { ...orderBody, repos: [repo] } });
    await a.next(ofType("hail", "ORD-0001"));
    // The echo harness has started: the checkout is under the default --work-dir, <outbox dir>/echo.
    const checkout = path.join(dir, "echo", "ORD-0001", "app");
    expect(git(checkout, "rev-parse", "--abbrev-ref", "HEAD")).toBe("bmus/ORD-0001");
    expect(git(checkout, "rev-parse", "HEAD")).toBe(commit);

    a.send({ seq: 2, order: "ORD-0001", type: "belay", body: {} });
    await a.next(ofType("error", "ORD-0001"));
    a.send({ seq: 3, order: "ORD-0002", type: "order", body: { ...orderBody, repos: [{ ...repo, bundle_path: path.join(dir, "nope.bundle") }] } });
    const err = await a.next(ofType("error", "ORD-0002"));
    expect(err.body).toMatchObject({ reason: expect.stringMatching(/^checking out repo app: /) as unknown });
    expect(fs.readFileSync(path.join(dir, "outbox.ndjson"), "utf8")).not.toMatch(/"order":"ORD-0002","type":"(progress|hail)"/);
    await a.close();
  });

  it("bundles each repo's bmus/<order> into <work-dir>/<order>/out/ for the report, and fails the order when it can't", async () => {
    const dir = tempDir();
    await startServe(dir, 60_000);
    const a = new Attach(dir, 0);
    const answer = (seq: number, ref: string) =>
      a.send({ seq, order: ref.replace(/^echo-/, ""), type: "answer", body: { ref, hail_id: "H-0001", decided_by: "captain", decision: "go" } });

    const { repo, commit } = orderBundle(path.join(dir, "a"), "ORD-0001");
    a.send({ seq: 1, order: "ORD-0001", type: "order", body: { ...orderBody, repos: [repo] } });
    await a.next(ofType("hail", "ORD-0001"));
    answer(2, "echo-ORD-0001");
    const report = await a.next(ofType("report", "ORD-0001"));
    const out = path.join(dir, "echo", "ORD-0001", "out", "app.bundle");
    expect(report.body).toEqual({
      summary: "go",
      repos: [{ name: "app", bundle_path: out, commit, branch: "bmus/ORD-0001" }],
      usage: { input_tokens: 0, output_tokens: 0 },
    });
    // No commits past the base: the bundle holds the whole history (git refuses an empty one).
    const empty = path.join(dir, "empty");
    git(dir, "init", "-q", empty);
    expect(git(empty, "bundle", "verify", out)).toContain("complete history");

    // The branch is gone by the time the run resolves: the order fails.
    const two = orderBundle(path.join(dir, "b"), "ORD-0002");
    a.send({ seq: 3, order: "ORD-0002", type: "order", body: { ...orderBody, repos: [two.repo] } });
    await a.next(ofType("hail", "ORD-0002"));
    const checkout = path.join(dir, "echo", "ORD-0002", "app");
    git(checkout, "checkout", "-q", "--detach");
    git(checkout, "branch", "-q", "-D", "bmus/ORD-0002");
    answer(4, "echo-ORD-0002");
    const err = await a.next(ofType("error", "ORD-0002"));
    expect(err.body).toEqual({ reason: "bundling repo app: branch bmus/ORD-0002 not found" });
    expect(fs.existsSync(path.join(dir, "echo", "ORD-0002", "out"))).toBe(false);
    await a.close();
  });

  it("holds a command that arrives during the checkout until the harness starts", async () => {
    const dir = tempDir();
    await startServe(dir, 60_000);
    const a = new Attach(dir, 0);
    const { repo } = orderBundle(dir, "ORD-0001");
    a.send({ seq: 1, order: "ORD-0001", type: "order", body: { ...orderBody, repos: [repo] } });
    a.send({ seq: 2, order: "ORD-0001", type: "belay", body: { reason: "too soon" } });
    const err = await a.next(ofType("error", "ORD-0001"));
    expect(err.body).toEqual({ reason: "belayed: too soon" });
    await a.close();
  });

  it("a stand_down ends the run after a stopping checkpoint, with no report or error; one whose checkpoint fails ends in error", async () => {
    const dir = tempDir();
    await startServe(dir, 60_000);
    const a = new Attach(dir, 0);
    const { repo, commit } = orderBundle(dir, "ORD-0001");
    a.send({ seq: 1, order: "ORD-0001", type: "order", body: { ...orderBody, repos: [repo] } });
    await a.next(ofType("hail", "ORD-0001"));
    a.send({ seq: 2, order: "ORD-0001", type: "stand_down", body: { reason: "watch change" } });
    const cp = await a.next((m) => m.order === "ORD-0001");
    const cpDir = path.join(dir, "echo", "ORD-0001", "1");
    expect(cp).toMatchObject({
      type: "checkpoint_ready",
      body: {
        repos: [{ name: "app", bundle_path: path.join(cpDir, "app.bundle"), commit, branch: "bmus/ORD-0001" }],
        handover_path: path.join(cpDir, "handover.md"),
        stopping: true,
      },
    });
    // The hail's answer comes too late, and the order is free to run again.
    a.send({ seq: 3, order: "ORD-0001", type: "answer", body: { ref: "echo-ORD-0001", hail_id: "H-1", decided_by: "captain", decision: "late" } });
    const second = orderBundle(path.join(dir, "b"), "ORD-0002").repo;
    a.send({ seq: 4, order: "ORD-0002", type: "order", body: { ...orderBody, repos: [second] } });
    await a.next(ofType("hail", "ORD-0002"));
    git(path.join(dir, "echo", "ORD-0002", "app"), "checkout", "-q", "--detach");
    git(path.join(dir, "echo", "ORD-0002", "app"), "branch", "-q", "-D", "bmus/ORD-0002");
    a.send({ seq: 5, order: "ORD-0002", type: "recall", body: {} });
    const err = await a.next((m) => m.order !== undefined);
    expect(err).toMatchObject({ type: "error", order: "ORD-0002", body: { reason: "bundling repo app: branch bmus/ORD-0002 not found" } });
    const types = fs.readFileSync(path.join(dir, "outbox.ndjson"), "utf8").trim().split("\n")
      .map((l) => JSON.parse(l) as BeamToBridgeMessage).filter((m) => m.order === "ORD-0001").map((m) => m.type);
    expect(types).toEqual(["progress", "hail", "checkpoint_ready"]);

    // The error came during a stop, so the order isn't done: the bridge parks
    // it and may send it here again, and it runs on a fresh checkout.
    // serve records the order after emitting its error.
    await vi.waitFor(() => {
      const state = JSON.parse(fs.readFileSync(path.join(dir, "state.json"), "utf8")) as Record<string, unknown>;
      expect(state).toMatchObject({ completed: [], stopped: ["ORD-0001", "ORD-0002"] });
    });
    git(path.join(dir, "b", "src-app"), "bundle", "create", "--quiet", second.bundle_path, "refs/heads/bmus/ORD-0002");
    a.send({ seq: 6, order: "ORD-0002", type: "order", body: { ...orderBody, repos: [second] } });
    await a.next(ofType("hail", "ORD-0002"));
    expect(git(path.join(dir, "echo", "ORD-0002", "app"), "rev-parse", "--abbrev-ref", "HEAD")).toBe("bmus/ORD-0002");
    await a.close();
  });

  it("a restart while a stop was pending reports the order lost but lets a resend run it", async () => {
    const dir = tempDir();
    const serve = await startServe(dir, 60_000);
    const a = new Attach(dir, 0);
    a.send({ seq: 1, order: "ORD-0001", type: "order", body: orderBody });
    await a.next(ofType("hail", "ORD-0001"));
    await a.close();
    serve.kill("SIGKILL");
    await exited(serve);
    // As if serve died after relaying a stand_down, before the harness answered it.
    const statePath = path.join(dir, "state.json");
    const before = JSON.parse(fs.readFileSync(statePath, "utf8")) as Record<string, unknown>;
    expect(before).toMatchObject({ running: "ORD-0001" });
    fs.writeFileSync(statePath, JSON.stringify({ ...before, stopping: "ORD-0001" }));

    await startServe(dir, 60_000);
    const b = new Attach(dir, 0);
    expect(await b.next(ofType("error", "ORD-0001"))).toMatchObject({ body: { code: "combadge_restarted" } });
    expect(JSON.parse(fs.readFileSync(statePath, "utf8"))).toMatchObject({ completed: [], stopped: ["ORD-0001"] });
    expect(JSON.parse(fs.readFileSync(statePath, "utf8"))).not.toHaveProperty("stopping");
    b.send({ seq: 3, order: "ORD-0001", type: "order", body: orderBody });
    await b.next(ofType("hail", "ORD-0001")); // runs, rather than "already done"
    await b.close();
  });

  it("acks lower the heartbeat's outbox_depth and are not deduped", async () => {
    const dir = tempDir();
    await startServe(dir);
    const a = new Attach(dir, 0);
    const hb = async (pred: (depth: number, seq: number) => boolean) => {
      for (;;) {
        const m = await a.next(ofType("heartbeat"));
        if (m.type === "heartbeat" && pred(m.body.outbox_depth, m.seq)) return m;
      }
    };
    await hb((_, seq) => seq >= 5);
    a.send({ seq: 6, type: "ack", body: { seq: 1_000_000 } }); // above the last seq: ignored
    a.send({ seq: 7, type: "ack", body: { seq: 3 } });
    await hb((depth, seq) => depth === seq - 1 - 3); // depth counts entries above the ack, before this beat
    a.send({ seq: 7, type: "ack", body: { seq: 5 } }); // same command id: still applied
    await hb((depth, seq) => depth === seq - 1 - 5);
    await a.close();
  });

  it("survives malformed command lines", async () => {
    const dir = tempDir();
    await startServe(dir, 60_000);
    const a = new Attach(dir, 0);
    await a.next(ofType("hello"));
    for (const line of [
      "not json",
      "null",
      "[]",
      '{"v":1,"type":"ack","seq":1}',
      '{"v":1,"type":"ack","seq":2,"body":null}',
      '{"v":1,"type":"ack","seq":3,"body":{"seq":"x"}}',
      '{"v":1,"type":"order","seq":4}',
      '{"v":1,"type":"order","seq":5,"order":"ORD-0001","body":[]}',
      '{"v":1,"type":"answer","seq":"6","body":{}}',
    ]) a.sendRaw(line);
    a.send({ seq: 7, order: "ORD-0001", type: "order", body: orderBody });
    expect(await a.next()).toMatchObject({ type: "progress", order: "ORD-0001" });
    await a.close();
  });

  it("refuses a second serve on the same outbox", async () => {
    const dir = tempDir();
    await startServe(dir, 60_000);
    const before = fs.readFileSync(path.join(dir, "outbox.ndjson"), "utf8");

    // Same socket: it accepts, so the second serve leaves it alone.
    const sameSocket = await serveFails(dir);
    expect(sameSocket.code).not.toBe(0);
    expect(sameSocket.stderr).toContain("already running");
    // Other socket, same outbox: the lock next to the outbox is held.
    const otherSocket = await serveFails(dir, path.join(dir, "other.sock"));
    expect(otherSocket.code).not.toBe(0);
    expect(otherSocket.stderr).toContain("already running");

    expect(fs.readFileSync(path.join(dir, "outbox.ndjson"), "utf8")).toBe(before);
    const a = new Attach(dir, 0); // the first serve still answers on its socket
    expect(await a.next(ofType("hello"))).toMatchObject({ seq: 1 });
    await a.close();
  });

  it("resumes seq and dedup state across a crash, reusing a torn line's seq", async () => {
    const dir = tempDir();
    const outboxPath = path.join(dir, "outbox.ndjson");
    const serve1 = await startServe(dir, 60_000);
    const a = new Attach(dir, 0);
    await a.next(ofType("hello"));
    a.send({ seq: 10, order: "ORD-0001", type: "order", body: orderBody });
    const hail = await a.next(ofType("hail"));
    expect(hail.seq).toBe(3);

    // Crash mid-order, mid-write.
    serve1.kill("SIGKILL");
    await exited(serve1);
    await exited(a.child); // attach exits once the socket closes
    fs.appendFileSync(outboxPath, '{"v":1,"seq":4,"crew":"sor');

    await startServe(dir, 60_000);
    expect(fs.readFileSync(outboxPath, "utf8").endsWith("\n")).toBe(true); // torn tail cut off
    const b = new Attach(dir, 3);
    // The interrupted order is reported lost at seq 4: the torn line was never sent, so its seq is reused.
    const lost = await b.next();
    expect(lost).toMatchObject({ seq: 4, type: "error", order: "ORD-0001", body: { code: "combadge_restarted" } });
    expect(await b.next()).toMatchObject({ seq: 5, type: "hello" });

    // Command 10 was handled before the crash, so resending it does nothing.
    b.send({ seq: 10, order: "ORD-0001", type: "order", body: orderBody });
    b.send({ seq: 11, order: "ORD-0002", type: "order", body: orderBody });
    expect(await b.next()).toMatchObject({ seq: 6, type: "progress", order: "ORD-0002" });
    await b.close();
  });
});

describe("echo harness (in process)", () => {
  it("a belay while the stopping checkpoint is written wins, and no checkpoint goes out", async () => {
    const dir = tempDir();
    const emitted: Outbound[] = [];
    let deliver!: (m: Inbound) => void;
    const run = echoHarness({ workDir: dir }).run(
      { id: "ORD-0001", body: orderBody, repos: [] },
      (m) => emitted.push(m),
      (fn) => (deliver = fn),
    );
    deliver({ type: "stand_down", body: {} });
    deliver({ type: "belay", body: { reason: "changed course" } });
    await expect(run).rejects.toThrow("belayed: changed course");
    await sleep(50);
    expect(emitted.map((m) => m.type)).toEqual(["progress", "hail"]);
  });
});
