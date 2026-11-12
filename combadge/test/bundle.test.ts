// The bundle runs on its own (issue #13, plan §5): copied alone into a
// directory with no package.json or node_modules on its path, as provisioning
// does with CopyTo, serve and attach still start, talk and exit cleanly.
import { spawn, type ChildProcess } from "node:child_process";
import * as fs from "node:fs";
import * as net from "node:net";
import * as os from "node:os";
import * as path from "node:path";
import { afterEach, expect, inject, it } from "vitest";
import { onLines } from "../src/lines.js";
import { exited, sleep } from "./procs.js";

const children: ChildProcess[] = [];
let dir: string | undefined;

afterEach(async () => {
  for (const c of children.splice(0)) {
    c.kill("SIGKILL");
    await exited(c);
  }
  if (dir) fs.rmSync(dir, { recursive: true, force: true });
  dir = undefined;
});

/** Throws if dir or any ancestor holds a node_modules or package.json Node could resolve from. */
function assertIsolated(start: string): void {
  for (let d = start; ; d = path.dirname(d)) {
    for (const name of ["node_modules", "package.json"]) {
      if (fs.existsSync(path.join(d, name))) throw new Error(`${path.join(d, name)} exists; the bundle would not run in isolation`);
    }
    if (path.dirname(d) === d) return;
  }
}

/** Runs the copied bundle from inside its own dir, with no NODE_PATH to fall back on. */
function run(bin: string, args: string[]): ChildProcess {
  const env = { ...process.env };
  delete env.NODE_PATH;
  delete env.NODE_OPTIONS;
  const child = spawn(process.execPath, [bin, ...args], { cwd: path.dirname(bin), env, stdio: ["pipe", "pipe", "pipe"] });
  children.push(child);
  return child;
}

function accepts(socket: string): Promise<boolean> {
  return new Promise((resolve) => {
    const c = net.connect(socket);
    c.once("connect", () => (c.destroy(), resolve(true)));
    c.once("error", () => resolve(false));
  });
}

it("serve and attach run from the bundle alone, with no node_modules", async () => {
  dir = fs.mkdtempSync(path.join(os.tmpdir(), "combadge-bundle-"));
  assertIsolated(dir);
  const bin = path.join(dir, "combadge.mjs");
  fs.copyFileSync(inject("combadgeBin"), bin);
  expect(fs.readdirSync(dir)).toEqual(["combadge.mjs"]);

  const socket = path.join(dir, "x.sock");
  const serve = run(bin, ["serve", "--socket", socket, "--crew", "x", "--harness", "echo", "--outbox", path.join(dir, "outbox.ndjson")]);
  let serveErr = "";
  serve.stderr!.on("data", (d: Buffer) => (serveErr += d.toString()));
  const deadline = Date.now() + 5000;
  while (!(await accepts(socket))) {
    if (serve.exitCode !== null) throw new Error(`serve exited with ${serve.exitCode}: ${serveErr}`);
    if (Date.now() > deadline) throw new Error(`serve did not open its socket: ${serveErr}`);
    await sleep(20);
  }

  const attach = run(bin, ["attach", "--socket", socket, "--since", "0"]);
  const hello = await new Promise<Record<string, unknown>>((resolve, reject) => {
    const t = setTimeout(() => reject(new Error("no hello from attach")), 5000);
    onLines(attach.stdout!, (line) => {
      const msg = JSON.parse(line) as Record<string, unknown>;
      if (msg.type === "hello") (clearTimeout(t), resolve(msg));
    });
  });
  expect(hello).toMatchObject({ v: 1, seq: 1, crew: "x", type: "hello" });

  attach.stdin!.end();
  expect(await exited(attach)).toBe(0);
  serve.kill("SIGTERM");
  expect(await exited(serve)).toBe(0);
  expect(serveErr).toBe("");
});
