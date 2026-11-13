// Process-level tests of the holodeck harness: serve runs a script, attach
// relays it, as the bridge's holodeck CI tests (#18) will.
import * as fs from "node:fs";
import * as path from "node:path";
import { PassThrough } from "node:stream";
import { fileURLToPath } from "node:url";
import { afterEach, describe, expect, it, vi } from "vitest";
import { holodeckHarness, loadScript, parseScript, validateScript } from "../src/harness/holodeck.js";
import type { Inbound, Outbound } from "../src/harness/types.js";
import { onLines } from "../src/lines.js";
import type { BeamToBridgeMessage, ConsultBody, OrderBody } from "../src/protocol.js";
import { git, orderBundle } from "./gitfix.js";
import { Attach, CREW, cleanup, exited, serveFails, sleep, startServe, tempDir } from "./procs.js";

afterEach(cleanup);

const fixture = (name: string) => fileURLToPath(new URL(`fixtures/${name}`, import.meta.url));
const orderBody: OrderBody = { title: "Holodeck run", body: "Test order.", repos: [], budget_tokens: 0 };
const ORDER = "ORD-0001";
const holodeck = (script: string, dir: string) => [
  "--harness", "holodeck", "--script", script, "--step-log", path.join(dir, "steps.ndjson"),
  "--work-dir", path.join(dir, "work"),
];
const ofType = (type: string) => (m: BeamToBridgeMessage) => m.type === type && m.order === ORDER;
/** Messages of the order, skipping heartbeats and hellos. */
const ofOrder = (m: BeamToBridgeMessage) => m.order === ORDER;

function stepLog(dir: string): Record<string, unknown>[] {
  if (!fs.existsSync(path.join(dir, "steps.ndjson"))) return [];
  return fs.readFileSync(path.join(dir, "steps.ndjson"), "utf8").trim().split("\n").map((l) => JSON.parse(l) as Record<string, unknown>);
}

function expectCheckpoint(m: BeamToBridgeMessage, dir: string, index: number, note: string): void {
  if (m.type !== "checkpoint_ready") throw new Error(`expected checkpoint_ready, got ${m.type}`);
  const orderDir = path.join(dir, "work", ORDER, String(index));
  expect(m.body).toEqual({
    repos: [],
    transcript_path: path.join(orderDir, "transcript.jsonl"),
    handover_path: path.join(orderDir, "handover.md"),
  });
  expect(fs.readFileSync(m.body.handover_path, "utf8")).toBe(`${note}\n`);
  JSON.parse(fs.readFileSync(m.body.transcript_path, "utf8"));
}

describe("holodeck harness", () => {
  it("fixtures are valid scripts", () => {
    for (const f of ["default.json", "crash.json", "pause.json", "work.json"]) expect(() => loadScript(fixture(f))).not.toThrow();
    expect(loadScript(fixture("default.json")).steps.some((s) => "request_handover" in s)).toBe(true);
  });

  it("emits exactly the default script's messages, in order", async () => {
    const dir = tempDir();
    await startServe(dir, 60_000, holodeck(fixture("default.json"), dir));
    const a = new Attach(dir, 0); // validates every message against the schema
    a.send({ seq: 1, order: ORDER, type: "order", body: orderBody });

    expect((await a.next(ofOrder)).body).toEqual({ pct: 10, note: "surveying the codebase" });
    const hail = await a.next(ofOrder);
    if (hail.type !== "hail") throw new Error(`expected hail, got ${hail.type}`);
    expect(hail.body).toEqual({
      ref: `holodeck-${ORDER}-1`,
      category: "tool_approval",
      question: "Run make deploy?",
      options: ["allow", "deny"],
      context: { tool: "Bash", command: "make deploy" },
    });
    a.send({
      seq: 2, order: ORDER, type: "answer",
      body: { ref: hail.body.ref, hail_id: "H-0001", decided_by: "captain", decision: "allow", option: 0 },
    });
    expectCheckpoint(await a.next(ofOrder), dir, 2, "mid-mission checkpoint");
    expect(await a.next(ofOrder)).toMatchObject({ type: "progress", body: { pct: 80, note: "tests passing" } });
    expect(await a.next(ofOrder)).toMatchObject({
      type: "report",
      body: {
        summary: "holodeck mission complete",
        repos: [],
        artifacts: ["README.md"],
        usage: { input_tokens: 0, output_tokens: 0 },
      },
    });

    const outbox = fs.readFileSync(path.join(dir, "outbox.ndjson"), "utf8").trim().split("\n");
    const types = outbox.map((l) => JSON.parse(l) as BeamToBridgeMessage).filter(ofOrder).map((m) => m.type);
    expect(types).toEqual(["progress", "hail", "checkpoint_ready", "progress", "report"]);

    // Each step logged once, the hail with its decision.
    const log = stepLog(dir);
    expect(log.map((e) => [e.order, e.index, e.action])).toEqual([
      [ORDER, 0, "progress"],
      [ORDER, 1, "hail"],
      [ORDER, 2, "request_handover"],
      [ORDER, 3, "progress"],
      [ORDER, 4, "report"],
    ]);
    expect(log[1]).toMatchObject({ decision: "allow", decided_by: "captain" });
    for (const e of log) expect(Number.isNaN(Date.parse(e.ts as string))).toBe(false);
    await a.close();
  });

  it("commits on bmus/<order>, bundles it from the base commit in checkpoints and the report", async () => {
    const dir = tempDir();
    await startServe(dir, 60_000, holodeck(fixture("work.json"), dir));
    const a = new Attach(dir, 0);
    const { repo, commit: base } = orderBundle(dir, ORDER);
    a.send({ seq: 1, order: ORDER, type: "order", body: { ...orderBody, repos: [repo] } });
    const checkout = path.join(dir, "work", ORDER, "app");

    expect(await a.next(ofOrder)).toMatchObject({ type: "progress" });
    const cp = await a.next(ofOrder);
    if (cp.type !== "checkpoint_ready") throw new Error(`expected checkpoint_ready, got ${cp.type}`);
    const log = stepLog(dir);
    const first = (log[1]?.commits as Record<string, string>).app!;
    expect(git(checkout, "rev-parse", `${first}^`)).toBe(base);
    const cpDir = path.join(dir, "work", ORDER, "2");
    expect(cp.body).toEqual({
      repos: [{ name: "app", bundle_path: path.join(cpDir, "app.bundle"), commit: first, branch: `bmus/${ORDER}` }],
      transcript_path: path.join(cpDir, "transcript.jsonl"),
      handover_path: path.join(cpDir, "handover.md"),
    });

    const report = await a.next(ofOrder);
    if (report.type !== "report") throw new Error(`expected report, got ${report.type}`);
    const second = git(checkout, "rev-parse", `bmus/${ORDER}`);
    expect(git(checkout, "rev-parse", `${second}^`)).toBe(first);
    const out = path.join(dir, "work", ORDER, "out", "app.bundle");
    expect(report.body).toEqual({
      summary: "holodeck work committed",
      repos: [{ name: "app", bundle_path: out, commit: second, branch: `bmus/${ORDER}` }],
      artifacts: ["holodeck.txt"],
      usage: { input_tokens: 0, output_tokens: 0 },
    });
    expect(stepLog(dir)[3]).toMatchObject({ action: "commit", commits: { app: second } });

    // Each bundle carries bmus/<order> only, needs the base commit (which the
    // bridge's clone has), and fetches into the source repo.
    const src = path.join(dir, "src-app");
    expect(git(src, "bundle", "list-heads", out)).toBe(`${second} refs/heads/bmus/${ORDER}`);
    expect(git(src, "bundle", "list-heads", path.join(cpDir, "app.bundle"))).toBe(`${first} refs/heads/bmus/${ORDER}`);
    const empty = path.join(dir, "empty");
    git(dir, "init", "-q", empty);
    expect(() => git(empty, "bundle", "verify", out)).toThrow(base);
    git(src, "fetch", "-q", out, `+refs/heads/bmus/${ORDER}:refs/heads/bmus/${ORDER}`);
    expect(git(src, "rev-parse", `bmus/${ORDER}`)).toBe(second);
    expect(fs.readdirSync(path.join(dir, "work", ORDER, "out"))).toEqual(["app.bundle"]);
    await a.close();
  });

  it("crash exits serve with the script's code after the checkpoint", async () => {
    const dir = tempDir();
    // crash.json sleeps 60s so #18's sickbay test can kill serve first; shorten it here.
    const steps = JSON.parse(fs.readFileSync(fixture("crash.json"), "utf8")) as Record<string, unknown>[];
    for (const s of steps) if ("sleep_ms" in s) s.sleep_ms = 50;
    const script = path.join(dir, "crash-fast.json");
    fs.writeFileSync(script, JSON.stringify(steps));

    const serve = await startServe(dir, 60_000, holodeck(script, dir));
    const a = new Attach(dir, 0);
    a.send({ seq: 1, order: ORDER, type: "order", body: orderBody });
    expect(await a.next(ofOrder)).toMatchObject({ type: "progress", body: { pct: 10 } });
    expectCheckpoint(await a.next(ofOrder), dir, 1, "checkpoint before the beam is lost");
    expect(await exited(serve)).toBe(137);
    await exited(a.child);

    const types = fs.readFileSync(path.join(dir, "outbox.ndjson"), "utf8").trim().split("\n")
      .map((l) => JSON.parse(l) as BeamToBridgeMessage).filter(ofOrder).map((m) => m.type);
    expect(types).toEqual(["progress", "checkpoint_ready"]); // crash emits nothing
    expect(stepLog(dir).map((e) => e.action)).toEqual(["progress", "request_handover", "sleep_ms", "crash"]);
  });

  it("a belay during a sleep rejects the run", async () => {
    const dir = tempDir();
    await startServe(dir, 60_000, holodeck(fixture("pause.json"), dir));
    const a = new Attach(dir, 0);
    a.send({ seq: 1, order: ORDER, type: "order", body: orderBody });
    expect(await a.next(ofOrder)).toMatchObject({ type: "progress", body: { pct: 10 } });
    expect(await a.next(ofOrder)).toMatchObject({ type: "progress", body: { pct: 50 } });
    a.send({ seq: 2, order: ORDER, type: "belay", body: { reason: "captain changed course" } });
    expect(await a.next(ofOrder, 2000)).toMatchObject({ type: "error", body: { reason: "belayed: captain changed course" } });
    expect(stepLog(dir).map((e) => e.index)).toEqual([0, 1]); // the sleep and what follows never ran
    await a.close();
  });

  it("a correction during a run is recorded in the step log and changes nothing; one for another order is dropped", async () => {
    const dir = tempDir();
    await startServe(dir, 60_000, holodeck(fixture("pause.json"), dir));
    const a = new Attach(dir, 0);
    a.send({ seq: 1, order: ORDER, type: "order", body: orderBody });
    expect(await a.next(ofOrder)).toMatchObject({ type: "progress", body: { pct: 10 } });
    expect(await a.next(ofOrder)).toMatchObject({ type: "progress", body: { pct: 50 } });
    a.send({ seq: 2, order: "ORD-0002", type: "correction", body: { hail_id: "H-0002", ref: "ORD-0002", note: "deny" } });
    const body = { hail_id: "H-0001", ref: ORDER, note: "deny (staging is frozen)" };
    a.send({ seq: 3, order: ORDER, type: "correction", body });
    await vi.waitFor(() => expect(stepLog(dir).filter((e) => e.action === "correction")).toHaveLength(1), { timeout: 5000 });
    const [entry] = stepLog(dir).filter((e) => e.action === "correction");
    expect(entry).toMatchObject({ order: ORDER, action: "correction", correction: body });
    expect(Number.isNaN(Date.parse(entry!.ts as string))).toBe(false);
    a.send({ seq: 4, order: ORDER, type: "belay", body: { reason: "done here" } });
    expect(await a.next(ofOrder, 2000)).toMatchObject({ type: "error", body: { reason: "belayed: done here" } });
    expect(stepLog(dir).filter((e) => e.action === "correction")).toHaveLength(1);
    await a.close();
  });

  it("a stand_down during a sleep emits a stopping checkpoint and nothing after it; the order is neither running nor completed", async () => {
    const dir = tempDir();
    const serve = await startServe(dir, 60_000, holodeck(fixture("pause.json"), dir));
    const a = new Attach(dir, 0);
    a.send({ seq: 1, order: ORDER, type: "order", body: orderBody });
    expect(await a.next(ofOrder)).toMatchObject({ type: "progress", body: { pct: 10 } });
    expect(await a.next(ofOrder)).toMatchObject({ type: "progress", body: { pct: 50 } });
    a.send({ seq: 2, order: ORDER, type: "stand_down", body: { reason: "watch change" } });
    const cp = await a.next(ofOrder, 2000);
    if (cp.type !== "checkpoint_ready") throw new Error(`expected checkpoint_ready, got ${cp.type}`);
    const cpDir = path.join(dir, "work", ORDER, "5"); // one past the script's last index
    expect(cp.body).toEqual({
      repos: [],
      transcript_path: path.join(cpDir, "transcript.jsonl"),
      handover_path: path.join(cpDir, "handover.md"),
      stopping: true,
    });
    expect(fs.readFileSync(cp.body.handover_path, "utf8")).toContain(`Order ${ORDER}: stopped by stand_down: watch change`);
    await expect(a.next(ofOrder, 300)).rejects.toThrow("timed out");
    expect(stepLog(dir).map((e) => e.index)).toEqual([0, 1]); // the sleep and what follows never ran
    const state = JSON.parse(fs.readFileSync(path.join(dir, "state.json"), "utf8")) as Record<string, unknown>;
    expect(state.running).toBeUndefined();
    expect(state.completed).toEqual([]);
    expect(state.stopped).toEqual([ORDER]);
    await a.close();

    // A restarted serve doesn't report the stopped order lost.
    serve.kill("SIGKILL");
    await exited(serve);
    await startServe(dir, 60_000, holodeck(fixture("pause.json"), dir));
    const b = new Attach(dir, 0);
    await b.next((m) => m.type === "hello" && m.seq > cp.seq);
    const types = fs.readFileSync(path.join(dir, "outbox.ndjson"), "utf8").trim().split("\n")
      .map((l) => JSON.parse(l) as BeamToBridgeMessage).filter(ofOrder).map((m) => m.type);
    expect(types).toEqual(["progress", "progress", "checkpoint_ready"]);
    await b.close();
  });

  it("keeps a stopped order's usage in state.json across a restart and hands it to its next run (#117)", async () => {
    const dir = tempDir();
    const script = path.join(dir, "usage-pause.json");
    fs.writeFileSync(script, JSON.stringify([
      { at_pct: 10, usage: { input_tokens: 700, output_tokens: 60, turns: 5 } },
      { at_pct: 50, sleep_ms: 60_000 },
      { at_pct: 100, report: { summary: "done" } },
    ]));
    const serve = await startServe(dir, 60_000, holodeck(script, dir));
    const a = new Attach(dir, 0);
    a.send({ seq: 1, order: ORDER, type: "order", body: orderBody });
    expect(await a.next(ofOrder)).toMatchObject({ type: "usage", body: { input_tokens: 700, output_tokens: 60, turns: 5 } });
    a.send({ seq: 2, order: ORDER, type: "stand_down", body: {} });
    expect(await a.next(ofOrder)).toMatchObject({ type: "checkpoint_ready", body: { stopping: true } });
    const readState = () => JSON.parse(fs.readFileSync(path.join(dir, "state.json"), "utf8")) as { usage: Record<string, unknown> };
    expect(readState().usage).toEqual({ [ORDER]: { input_tokens: 700, output_tokens: 60, turns: 5 } });
    await a.close();
    serve.kill("SIGKILL");
    await exited(serve);

    // The resent order's run starts from the kept counts: a script with no usage step reports them.
    const plain = path.join(dir, "plain.json");
    fs.writeFileSync(plain, JSON.stringify([{ at_pct: 100, report: { summary: "done" } }]));
    await startServe(dir, 60_000, holodeck(plain, dir));
    const b = new Attach(dir, 0);
    b.send({ seq: 3, order: ORDER, type: "order", body: orderBody });
    expect(await b.next(ofType("report"))).toMatchObject({ body: { usage: { input_tokens: 700, output_tokens: 60 } } });
    await vi.waitFor(() => expect(readState().usage).toEqual({})); // a completed order's usage is dropped
    await b.close();
  });

  it("after a stand_down, an order command for the same id runs again on a fresh checkout from the new in/ bundle", async () => {
    const dir = tempDir();
    const script = path.join(dir, "long.json");
    fs.writeFileSync(script, JSON.stringify([
      { at_pct: 10, commit: "first run" },
      { at_pct: 20, request_handover: "after the first commit" },
      { at_pct: 50, sleep_ms: 60_000 },
      { at_pct: 100, report: { summary: "done" } },
    ]));
    await startServe(dir, 60_000, holodeck(script, dir));
    const a = new Attach(dir, 0);
    const orderDir = path.join(dir, "work", ORDER);
    const inBundle = path.join(orderDir, "in", "app.bundle");
    const { repo } = orderBundle(dir, ORDER);
    fs.mkdirSync(path.dirname(inBundle), { recursive: true });
    fs.renameSync(repo.bundle_path, inBundle);
    a.send({ seq: 1, order: ORDER, type: "order", body: { ...orderBody, repos: [{ ...repo, bundle_path: inBundle }] } });
    expect(await a.next(ofOrder)).toMatchObject({ type: "checkpoint_ready" });
    a.send({ seq: 2, order: ORDER, type: "recall", body: {} });
    const stop = await a.next(ofOrder);
    expect(stop).toMatchObject({ type: "checkpoint_ready", body: { stopping: true } });
    if (stop.type !== "checkpoint_ready") throw new Error("unreachable");
    expect(fs.readFileSync(stop.body.handover_path, "utf8")).toContain("stopped by recall: no reason given");
    const firstRun = stop.body.repos[0]!.commit;
    expect(fs.readdirSync(orderDir).sort()).toEqual(["1", "4", "app", "in"]); // checkpoints at steps 1 and 4 (one past the last)

    // The bridge ships the checkpoint's work back as the new bundle, one commit on, into in/.
    const src = path.join(dir, "src-app");
    git(src, "checkout", "-q", `bmus/${ORDER}`);
    fs.writeFileSync(path.join(src, "resumed.txt"), "from the checkpoint\n");
    git(src, "add", "resumed.txt");
    git(src, "commit", "-q", "-m", "checkpoint");
    const tip = git(src, "rev-parse", "HEAD");
    git(src, "bundle", "create", "--quiet", inBundle, `refs/heads/bmus/${ORDER}`);
    fs.writeFileSync(path.join(orderDir, "in", "keep.txt"), "in/ survives\n");

    a.send({ seq: 3, order: ORDER, type: "order", body: { ...orderBody, repos: [{ ...repo, bundle_path: inBundle }], handover: "resume" } });
    expect(await a.next(ofOrder)).toMatchObject({ type: "checkpoint_ready" });
    const checkout = path.join(orderDir, "app");
    expect(git(checkout, "rev-parse", `origin/bmus/${ORDER}`)).toBe(tip);
    expect(git(checkout, "rev-parse", `bmus/${ORDER}^`)).toBe(tip); // the second run's commit, on the new bundle
    expect(() => git(checkout, "cat-file", "-e", `${firstRun}^{commit}`)).toThrow(); // a fresh clone: the first run's commit isn't there
    expect(fs.readFileSync(path.join(orderDir, "in", "keep.txt"), "utf8")).toBe("in/ survives\n");
    expect(fs.existsSync(path.join(orderDir, "4"))).toBe(false); // the first run's stopping checkpoint is gone
    const commits = stepLog(dir).filter((e) => e.action === "commit").map((e) => (e.commits as Record<string, string>).app);
    expect(commits).toHaveLength(2);
    expect(git(checkout, "rev-parse", `${commits[1]!}^`)).toBe(tip);
    const state = JSON.parse(fs.readFileSync(path.join(dir, "state.json"), "utf8")) as Record<string, unknown>;
    expect(state).toMatchObject({ running: ORDER, stopped: [] });
    await a.close();
  });

  it("a belay during a hail wait rejects the run", async () => {
    const dir = tempDir();
    await startServe(dir, 60_000, holodeck(fixture("default.json"), dir));
    const a = new Attach(dir, 0);
    a.send({ seq: 1, order: ORDER, type: "order", body: orderBody });
    await a.next(ofType("hail"));
    a.send({ seq: 2, order: ORDER, type: "belay", body: { reason: "stop" } });
    expect(await a.next(ofOrder)).toMatchObject({ type: "error", body: { reason: "belayed: stop" } });
    expect(stepLog(dir).map((e) => e.index)).toEqual([0]);
    await a.close();
  });

  it("a belay and the hail's answer in one write stop the walk at the hail", async () => {
    const dir = tempDir();
    const script = path.join(dir, "race.json");
    fs.writeFileSync(script, JSON.stringify([
      { at_pct: 10, hail: { category: "clarifying_question", question: "Go?" } },
      { at_pct: 20, progress: "must not run" },
      { at_pct: 30, request_handover: "must not run" },
      { at_pct: 40, crash: 137 },
    ]));
    const serve = await startServe(dir, 60_000, holodeck(script, dir));
    const a = new Attach(dir, 0);
    a.send({ seq: 1, order: ORDER, type: "order", body: orderBody });
    const hail = await a.next(ofType("hail"));
    if (hail.type !== "hail") throw new Error("unreachable");
    const line = (seq: number, type: string, body: object) =>
      JSON.stringify({ v: 1, seq, crew: CREW, order: ORDER, type, ts: new Date().toISOString(), body }) + "\n";
    a.child.stdin!.write(
      line(2, "belay", { reason: "race" }) +
        line(3, "answer", { ref: hail.body.ref, hail_id: "H-0001", decided_by: "captain", decision: "go" }),
    );
    expect(await a.next(ofOrder)).toMatchObject({ type: "error", body: { reason: "belayed: race" } });
    await sleep(200);
    expect(serve.exitCode).toBeNull();
    const types = fs.readFileSync(path.join(dir, "outbox.ndjson"), "utf8").trim().split("\n")
      .map((l) => JSON.parse(l) as BeamToBridgeMessage).filter(ofOrder).map((m) => m.type);
    expect(types).toEqual(["hail", "error"]);
    expect(stepLog(dir)).toEqual([]);
    expect(fs.existsSync(path.join(dir, "work"))).toBe(false);
    await a.close();
  });

  it("ignores an answer with the wrong ref and proceeds on the right one", async () => {
    const dir = tempDir();
    await startServe(dir, 60_000, holodeck(fixture("default.json"), dir));
    const a = new Attach(dir, 0);
    a.send({ seq: 1, order: ORDER, type: "order", body: orderBody });
    const hail = await a.next(ofType("hail"));
    if (hail.type !== "hail") throw new Error("unreachable");
    const answer = (seq: number, ref: string) =>
      a.send({ seq, order: ORDER, type: "answer", body: { ref, hail_id: "H-0001", decided_by: "policy", decision: "deny" } });
    answer(2, "not-a-ref");
    await sleep(200);
    answer(3, hail.body.ref);
    // Nothing was emitted for the wrong ref: the next message is the step after the hail.
    expectCheckpoint(await a.next(ofOrder), dir, 2, "mid-mission checkpoint");
    expect(stepLog(dir)[1]).toMatchObject({ index: 1, action: "hail", decision: "deny", decided_by: "policy" });
    await a.close();
  });

  it("refuses to start with an invalid script", async () => {
    const dir = tempDir();
    const script = path.join(dir, "bad.json");
    fs.writeFileSync(script, JSON.stringify([{ at_pct: 10, progress: "no terminal step" }]));
    const bad = await serveFails(dir, undefined, holodeck(script, dir));
    expect(bad.code).not.toBe(0);
    expect(bad.stderr).toContain(`holodeck script ${script}: step 0: the script must end with a terminal step`);
    expect(fs.existsSync(path.join(dir, "outbox.ndjson"))).toBe(false); // failed before serve started

    const missing = await serveFails(dir, undefined, ["--harness", "holodeck"]);
    expect(missing.code).not.toBe(0);
    expect(missing.stderr).toContain("--script is required with --harness holodeck");
  });
});

describe("validateScript", () => {
  const report = { at_pct: 100, report: { summary: "done" } };
  const cases: [string, unknown, string][] = [
    ["non-array", { at_pct: 0, report: { summary: "x" } }, "must be a non-empty JSON array"],
    ["empty array", [], "must be a non-empty JSON array"],
    ["non-object step", ["progress", report], "step 0: must be an object"],
    ["missing at_pct", [{ progress: "x" }, report], "step 0: at_pct must be a number from 0 to 100"],
    ["at_pct above 100", [{ at_pct: 101, report: { summary: "x" } }], "step 0: at_pct must be a number from 0 to 100"],
    ["decreasing at_pct", [{ at_pct: 50, progress: "x" }, { at_pct: 40, progress: "y" }, report], "step 1: at_pct 40 is below the previous step's 50"],
    ["two actions", [{ at_pct: 10, progress: "x", sleep_ms: 1 }, report], "step 0: needs exactly one action"],
    ["no action", [{ at_pct: 10 }, report], "step 0: needs exactly one action"],
    ["unknown key", [{ at_pct: 10, progres: "x" }, report], "step 0: unknown key progres"],
    ["no terminal step", [{ at_pct: 10, progress: "x" }], "step 0: the script must end with a terminal step"],
    ["terminal not last", [report, { at_pct: 100, progress: "x" }], "step 0: report is terminal but is not the last step"],
    ["two terminals", [{ at_pct: 100, crash: 1 }, report], "step 0: crash is terminal but is not the last step"],
    ["crash 0", [{ at_pct: 100, crash: 0 }], "step 0: crash must be an exit code from 1 to 255"],
    ["crash 256", [{ at_pct: 100, crash: 256 }], "step 0: crash must be an exit code from 1 to 255"],
    ["crash non-integer", [{ at_pct: 100, crash: 1.5 }], "step 0: crash must be an exit code from 1 to 255"],
    ["negative sleep", [{ at_pct: 0, sleep_ms: -1 }, report], "step 0: sleep_ms must be an integer from 0 to 2147483647"],
    ["sleep above timer max", [{ at_pct: 0, sleep_ms: 2_147_483_648 }, report], "step 0: sleep_ms must be an integer from 0 to 2147483647"],
    ["sleep non-integer", [{ at_pct: 0, sleep_ms: "10" }, report], "step 0: sleep_ms must be an integer"],
    ["progress non-string", [{ at_pct: 0, progress: 1 }, report], "step 0: progress must be a string"],
    ["commit non-string", [{ at_pct: 0, commit: {} }, report], "step 0: commit must be a string"],
    ["hail unknown key", [{ at_pct: 0, hail: { category: "c", question: "q", ref: "r" } }, report], "step 0: unknown hail key ref"],
    ["hail no question", [{ at_pct: 0, hail: { category: "c" } }, report], "step 0: hail.question must be a non-empty string"],
    ["hail bad options", [{ at_pct: 0, hail: { category: "c", question: "q", options: [1] } }, report], "step 0: hail.options must be an array of strings"],
    ["hail bad context", [{ at_pct: 0, hail: { category: "c", question: "q", context: [] } }, report], "step 0: hail.context must be an object"],
    ["usage not an object", [{ at_pct: 0, usage: 5 }, report], "step 0: usage must be an object"],
    ["usage unknown key", [{ at_pct: 0, usage: { input_tokens: 1, output_tokens: 1, turns: 1, seq: 1 } }, report], "step 0: unknown usage key seq"],
    ["usage without turns", [{ at_pct: 0, usage: { input_tokens: 1, output_tokens: 1 } }, report], "step 0: usage.turns must be a non-negative integer"],
    ["usage negative tokens", [{ at_pct: 0, usage: { input_tokens: -1, output_tokens: 1, turns: 1 } }, report], "step 0: usage.input_tokens must be a non-negative integer"],
    ["report unknown key", [{ at_pct: 100, report: { summary: "x", usage: {} } }], "step 0: unknown report key usage"],
    ["report with branch", [{ at_pct: 100, report: { summary: "x", branch: "b" } }], "step 0: unknown report key branch"],
    ["report no summary", [{ at_pct: 100, report: {} }], "step 0: report.summary must be a string"],
    ["report bad artifacts", [{ at_pct: 100, report: { summary: "x", artifacts: "a" } }], "step 0: report.artifacts must be an array of strings"],
  ];
  it.each(cases)("rejects %s", (_, script, message) => {
    expect(() => validateScript(script)).toThrow(message);
  });

  it("accepts a minimal script and the timer max", () => {
    expect(validateScript([{ at_pct: 0, sleep_ms: 2_147_483_647 }, { at_pct: 100, crash: 137 }])).toHaveLength(2);
  });
});

describe("holodeck run (in process)", () => {
  it("records the order's model, handover and budget in the step log, and nothing without them", async () => {
    const dir = tempDir();
    const script = path.join(dir, "short.json");
    fs.writeFileSync(script, JSON.stringify([{ at_pct: 10, progress: "p" }, { at_pct: 100, report: { summary: "done" } }]));
    const h = holodeckHarness({ script, stepLog: path.join(dir, "steps.ndjson"), workDir: path.join(dir, "work") });
    const body = { ...orderBody, model: "model-x", handover: "pick up here", budget_tokens: 6500, budget_turns: 42 };
    await h.run({ id: ORDER, body, repos: [] }, () => {}, () => {});
    await h.run({ id: "ORD-0002", body: orderBody, repos: [] }, () => {}, () => {});
    expect(stepLog(dir).map((e) => [e.order, e.model, e.handover, e.budget_tokens, e.budget_turns])).toEqual([
      [ORDER, "model-x", "pick up here", 6500, 42], [ORDER, "model-x", "pick up here", 6500, 42],
      ["ORD-0002", undefined, undefined, undefined, undefined], ["ORD-0002", undefined, undefined, undefined, undefined],
    ]);
  });

  it("emits a usage step's counts, and reports the run's latest tokens, else the order's earlier ones (#117)", async () => {
    const dir = tempDir();
    const script = path.join(dir, "usage.json");
    fs.writeFileSync(script, JSON.stringify([
      { at_pct: 10, usage: { input_tokens: 100, output_tokens: 20, turns: 1 } },
      { at_pct: 50, usage: { input_tokens: 900, output_tokens: 70, turns: 3 } },
      { at_pct: 100, report: { summary: "done" } },
    ]));
    const h = holodeckHarness({ script, stepLog: path.join(dir, "steps.ndjson"), workDir: path.join(dir, "work") });
    const emitted: Outbound[] = [];
    const first = await h.run({ id: ORDER, body: orderBody, repos: [] }, (m) => emitted.push(m), () => {});
    expect(emitted).toEqual([
      { type: "usage", body: { input_tokens: 100, output_tokens: 20, turns: 1 } },
      { type: "usage", body: { input_tokens: 900, output_tokens: 70, turns: 3 } },
    ]);
    expect(first.usage).toEqual({ input_tokens: 900, output_tokens: 70 });
    expect(stepLog(dir).map((e) => e.action)).toEqual(["usage", "usage", "report"]);

    const plain = path.join(dir, "plain.json");
    fs.writeFileSync(plain, JSON.stringify([{ at_pct: 100, report: { summary: "done" } }]));
    const p = holodeckHarness({ script: plain, workDir: path.join(dir, "work") });
    const usage = { input_tokens: 5, output_tokens: 1, turns: 1 };
    expect((await p.run({ id: ORDER, body: orderBody, repos: [], usage }, () => {}, () => {})).usage).toEqual({ input_tokens: 5, output_tokens: 1 });
    expect((await p.run({ id: ORDER, body: orderBody, repos: [] }, () => {}, () => {})).usage).toEqual({ input_tokens: 0, output_tokens: 0 });
  });

  it("records each repo's checkout dir in the step log", async () => {
    const dir = tempDir();
    const script = path.join(dir, "short.json");
    fs.writeFileSync(script, JSON.stringify([{ at_pct: 10, progress: "p" }, { at_pct: 20, crash: 1 }]));
    const h = holodeckHarness({ script, stepLog: path.join(dir, "steps.ndjson"), workDir: path.join(dir, "work") });
    const exit = vi.spyOn(process, "exit").mockImplementation((() => undefined) as never);
    try {
      const repos = [{ name: "app", dir: "/w/ORD-0001/app", base_ref: "main", base_commit: "abc" }];
      void h.run({ id: ORDER, body: orderBody, repos }, () => {}, () => {}).catch(() => {});
      await vi.waitFor(() => expect(exit).toHaveBeenCalled());
      const log = stepLog(dir);
      expect(log.map((e) => e.action)).toEqual(["progress", "crash"]);
      for (const e of log) expect(e.repos).toEqual({ app: "/w/ORD-0001/app" });
    } finally {
      exit.mockRestore();
    }
  });

  it("a belay and an answer delivered back to back stop the walk before later steps", async () => {
    const dir = tempDir();
    const script = path.join(dir, "race.json");
    fs.writeFileSync(script, JSON.stringify([
      { at_pct: 10, hail: { category: "clarifying_question", question: "Go?" } },
      { at_pct: 20, progress: "must not run" },
      { at_pct: 40, crash: 137 },
    ]));
    const exit = vi.spyOn(process, "exit").mockImplementation((() => undefined) as never);
    try {
      const h = holodeckHarness({ script, stepLog: path.join(dir, "steps.ndjson"), workDir: path.join(dir, "work") });
      const emitted: string[] = [];
      let deliver!: (m: Inbound) => void;
      const run = h.run({ id: ORDER, body: orderBody, repos: [] }, (m) => emitted.push(m.type), (fn) => (deliver = fn));
      deliver({ type: "belay", body: { reason: "race" } });
      deliver({ type: "answer", body: { ref: `holodeck-${ORDER}-0`, hail_id: "H-0001", decided_by: "captain", decision: "go" } });
      await expect(run).rejects.toThrow("belayed: race");
      await sleep(50);
      expect(emitted).toEqual(["hail"]);
      expect(exit).not.toHaveBeenCalled();
      expect(stepLog(dir)).toEqual([]);
    } finally {
      exit.mockRestore();
    }
  });

  it("an answer and a correction read in one chunk are logged in delivery order (#178)", async () => {
    const dir = tempDir();
    const script = path.join(dir, "countermand.json");
    fs.writeFileSync(script, JSON.stringify([
      { at_pct: 10, hail: { category: "tool_permission", question: "Deploy?" } },
      { at_pct: 100, report: { summary: "done" } },
    ]));
    const h = holodeckHarness({ script, stepLog: path.join(dir, "steps.ndjson"), workDir: path.join(dir, "work") });
    let deliver!: (m: Inbound) => void;
    const run = h.run({ id: ORDER, body: orderBody, repos: [] }, () => {}, (fn) => (deliver = fn));
    // As serve reads them: both lines in one chunk, dispatched synchronously.
    const stdin = new PassThrough();
    onLines(stdin, (l) => deliver(JSON.parse(l) as Inbound));
    const correction = { hail_id: "H-0001", ref: ORDER, note: "deny (frozen)" };
    stdin.write(
      JSON.stringify({ type: "answer", body: { ref: `holodeck-${ORDER}-0`, hail_id: "H-0001", decided_by: "command", decision: "allow" } }) + "\n" +
        JSON.stringify({ type: "correction", body: correction }) + "\n",
    );
    await run;
    const log = stepLog(dir);
    expect(log.map((e) => e.action)).toEqual(["correction", "hail", "report"]); // file order: the hail waits for the walk
    const delivered = log.filter((e) => e.delivery !== undefined).sort((a, b) => (a.delivery as number) - (b.delivery as number));
    expect(delivered).toMatchObject([
      { action: "hail", index: 0, decided_by: "command", delivery: 1 },
      { action: "correction", correction, delivery: 2 },
    ]);
  });
});

describe("holodeck consults", () => {
  const steps = [{ at_pct: 100, report: { summary: "done" } }];
  const req: ConsultBody = { hail_id: "H-0091", crew: "tomas", order: "ORD-0042", category: "scope_change", question: "Widen?", options: ["yes", "no"] };
  const harness = (consult: unknown) => {
    const dir = tempDir();
    const script = path.join(dir, "command.json");
    fs.writeFileSync(script, JSON.stringify(consult === undefined ? steps : { steps, consult }));
    return holodeckHarness({ script, workDir: path.join(dir, "work") });
  };

  it("answers every consult with the scripted reply, under the request's hail_id", async () => {
    const h = harness({ decision: "answer", option: 1, confidence: 0.9, risk_flags: ["scope_change"] });
    expect(await h.consult!(req, new AbortController().signal)).toEqual({
      hail_id: "H-0091", decision: "answer", option: 1, rationale: "", confidence: 0.9, risk_flags: ["scope_change"],
    });
    const escalating = harness({ decision: "escalate", rationale: "captain's call", confidence: 0 });
    expect(await escalating.consult!({ ...req, hail_id: "H-0092" }, new AbortController().signal)).toEqual({
      hail_id: "H-0092", decision: "escalate", rationale: "captain's call", confidence: 0,
    });
  });

  it("stays silent until aborted on silence, and runs no consult lane without one", async () => {
    const abort = new AbortController();
    const silent = harness("silence").consult!(req, abort.signal);
    const settled = await Promise.race([silent.then(() => "settled", () => "settled"), sleep(50).then(() => "pending")]);
    expect(settled).toBe("pending");
    abort.abort(new Error("closing"));
    await expect(silent).rejects.toThrow("closing");
    expect(harness(undefined).consult).toBeUndefined();
  });

  const cases: [string, unknown, string][] = [
    ["an unknown script key", { steps, consults: [] }, "unknown script key consults"],
    ["bad steps", { steps: [], consult: "silence" }, "must be a non-empty JSON array"],
    ["a non-object consult", { steps, consult: "never" }, 'consult: must be "silence" or a consult_reply body'],
    ["a hail_id", { steps, consult: { hail_id: "H-1", decision: "answer", confidence: 1 } }, "unknown consult key hail_id"],
    ["a bad decision", { steps, consult: { decision: "maybe", confidence: 1 } }, "consult: decision must be answer or escalate"],
    ["no confidence", { steps, consult: { decision: "answer" } }, "consult: confidence must be a number from 0 to 1"],
    ["confidence above 1", { steps, consult: { decision: "answer", confidence: 2 } }, "consult: confidence must be a number from 0 to 1"],
    ["a negative option", { steps, consult: { decision: "answer", option: -1, confidence: 1 } }, "consult: option must be a non-negative integer"],
    ["bad risk_flags", { steps, consult: { decision: "answer", confidence: 1, risk_flags: "x" } }, "consult: risk_flags must be an array of strings"],
  ];
  it.each(cases)("rejects %s", (_, script, message) => {
    expect(() => parseScript(script)).toThrow(message);
  });

  it("serve emits the scripted reply with no order, beside the order's walk", async () => {
    const dir = tempDir();
    const script = path.join(dir, "command.json");
    fs.writeFileSync(script, JSON.stringify({
      steps: [{ at_pct: 10, hail: { category: "clarifying_question", question: "Go?" } }, { at_pct: 100, report: { summary: "done" } }],
      consult: { decision: "answer", option: 0, rationale: "scripted", confidence: 0.75 },
    }));
    await startServe(dir, 60_000, holodeck(script, dir));
    const a = new Attach(dir, 0);
    a.send({ seq: 1, order: ORDER, type: "order", body: orderBody });
    const hail = await a.next(ofType("hail"));
    a.send({ seq: 2, type: "consult", body: req });
    const reply = await a.next((m) => m.type === "consult_reply");
    expect(reply).not.toHaveProperty("order");
    expect(reply.body).toEqual({ hail_id: "H-0091", decision: "answer", option: 0, rationale: "scripted", confidence: 0.75 });
    if (hail.type !== "hail") throw new Error("unreachable");
    a.send({ seq: 3, order: ORDER, type: "answer", body: { ref: hail.body.ref, hail_id: "H-0001", decided_by: "captain", decision: "go" } });
    expect((await a.next(ofType("report"))).body).toMatchObject({ summary: "done" });
    const log = stepLog(dir);
    // A hail step is logged once answered, a consult as it arrives.
    expect(log.map((e) => e.action)).toEqual(["consult", "hail", "report"]);
    expect(log[0]).toMatchObject({ consult: req });
    await a.close();
  });
});
