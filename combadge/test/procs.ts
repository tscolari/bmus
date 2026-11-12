import { spawn, type ChildProcess } from "node:child_process";
import * as fs from "node:fs";
import * as net from "node:net";
import * as os from "node:os";
import * as path from "node:path";
import { inject } from "vitest";
import type { BeamToBridgeMessage, BridgeToBeamMessage } from "../src/protocol.js";
import { onLines } from "../src/lines.js";
import { compileSchema } from "./schema.js";

const { validate } = compileSchema();

export const CREW = "sorell";

const children = new Set<ChildProcess>();
const dirs: string[] = [];

/** Fresh temp dir holding the socket, outbox and state file. */
export function tempDir(): string {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "combadge-test-"));
  dirs.push(dir);
  return dir;
}

/** Kills every process and removes every temp dir these helpers made; call from afterEach. */
export async function cleanup(): Promise<void> {
  const running = [...children];
  for (const c of running) c.kill("SIGKILL");
  await Promise.all(running.map(exited));
  for (const d of dirs.splice(0)) fs.rmSync(d, { recursive: true, force: true });
}

function start(args: string[], stderr: "inherit" | "pipe" = "inherit"): ChildProcess {
  const child = spawn(process.execPath, [inject("combadgeBin"), ...args], { stdio: ["pipe", "pipe", stderr] });
  children.add(child);
  child.on("exit", () => children.delete(child));
  return child;
}

export function exited(child: ChildProcess): Promise<number | null> {
  if (child.exitCode !== null || child.signalCode !== null) return Promise.resolve(child.exitCode);
  return new Promise((resolve) => child.once("exit", (code) => resolve(code)));
}

/** Default harness flags: the echo harness. Holodeck tests pass their own. */
export const ECHO = ["--harness", "echo"];

function serveArgs(dir: string, heartbeatMs: number, socket = path.join(dir, "combadge.sock"), harness = ECHO): string[] {
  return [
    "serve", "--socket", socket, "--crew", CREW, "--outbox", path.join(dir, "outbox.ndjson"),
    ...harness, "--heartbeat-interval", String(heartbeatMs),
  ];
}

/** Runs a `serve` expected to fail at startup; resolves with its exit code and stderr. */
export async function serveFails(
  dir: string,
  socket?: string,
  harness = ECHO,
): Promise<{ code: number | null; stderr: string }> {
  const child = start(serveArgs(dir, 60_000, socket, harness), "pipe");
  let stderr = "";
  child.stderr!.on("data", (d: Buffer) => (stderr += d.toString()));
  const code = await exited(child);
  return { code, stderr };
}

/** Seq of the last complete line in dir's outbox file (0 when there is none). */
export function outboxLastSeq(dir: string): number {
  const data = fs.readFileSync(path.join(dir, "outbox.ndjson"), "utf8");
  const complete = data.slice(0, data.lastIndexOf("\n") + 1).trim();
  return complete ? (JSON.parse(complete.split("\n").at(-1)!) as { seq: number }).seq : 0;
}

/** Starts `serve` (echo harness unless given other flags) and waits for its socket to accept. */
export async function startServe(dir: string, heartbeatMs = 50, harness = ECHO): Promise<ChildProcess> {
  const socket = path.join(dir, "combadge.sock");
  const child = start(serveArgs(dir, heartbeatMs, undefined, harness));
  const deadline = Date.now() + 5000;
  while (!(await accepts(socket))) {
    if (child.exitCode !== null) throw new Error(`serve exited with ${child.exitCode}`);
    if (Date.now() > deadline) throw new Error("serve did not open its socket");
    await sleep(20);
  }
  return child;
}

/** Probes the socket without a handshake: serve waits for one, and we hang up. */
function accepts(socket: string): Promise<boolean> {
  return new Promise((resolve) => {
    const c = net.connect(socket);
    c.once("connect", () => (c.destroy(), resolve(true)));
    c.once("error", () => resolve(false));
  });
}

/** A running `attach` process: messages it prints, and a way to send commands. */
export class Attach {
  readonly child: ChildProcess;
  private queue: BeamToBridgeMessage[] = [];
  private waiters: (() => void)[] = [];

  constructor(dir: string, since: number) {
    this.child = start(["attach", "--socket", path.join(dir, "combadge.sock"), "--since", String(since)]);
    onLines(this.child.stdout!, (line) => {
      const msg = JSON.parse(line) as BeamToBridgeMessage;
      if (!validate(msg)) throw new Error(`invalid message from serve: ${line}: ${JSON.stringify(validate.errors)}`);
      this.queue.push(msg);
      for (const w of this.waiters.splice(0)) w();
    });
  }

  /** Writes a command line to attach's stdin. */
  send(cmd: Omit<BridgeToBeamMessage, "v" | "crew" | "ts">): void {
    const full = { v: 1, seq: cmd.seq, crew: CREW, order: cmd.order, type: cmd.type, ts: new Date().toISOString(), body: cmd.body };
    if (!validate(full)) throw new Error(`invalid command in test: ${JSON.stringify(validate.errors)}`);
    this.child.stdin!.write(JSON.stringify(full) + "\n");
  }

  /** Writes a raw line to attach's stdin. */
  sendRaw(line: string): void {
    this.child.stdin!.write(line + "\n");
  }

  /** Returns the next message (consuming everything before it) that matches pred. */
  async next(pred: (m: BeamToBridgeMessage) => boolean = () => true, timeoutMs = 5000): Promise<BeamToBridgeMessage> {
    const deadline = Date.now() + timeoutMs;
    for (;;) {
      const m = this.queue.shift();
      if (m) {
        if (pred(m)) return m;
        continue;
      }
      const left = deadline - Date.now();
      if (left <= 0) throw new Error("timed out waiting for a message");
      await new Promise<void>((resolve) => {
        const t = setTimeout(resolve, left);
        this.waiters.push(() => (clearTimeout(t), resolve()));
      });
    }
  }

  /** Closes stdin and waits for attach to exit. */
  async close(): Promise<number | null> {
    this.child.stdin!.end();
    return exited(this.child);
  }
}

export function sleep(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}
