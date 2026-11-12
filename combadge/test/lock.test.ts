import * as fs from "node:fs";
import * as os from "node:os";
import * as path from "node:path";
import { afterEach, beforeEach, describe, expect, it } from "vitest";
import { acquireLock } from "../src/lock.js";

const bootFile = "/proc/sys/kernel/random/boot_id";
const boot = fs.existsSync(bootFile) ? fs.readFileSync(bootFile, "utf8").trim() : undefined;

let dir: string;
let lock: string;
beforeEach(() => {
  dir = fs.mkdtempSync(path.join(os.tmpdir(), "combadge-lock-"));
  lock = path.join(dir, "serve.lock");
});
afterEach(() => fs.rmSync(dir, { recursive: true, force: true }));

const write = (content: unknown) => fs.writeFileSync(lock, JSON.stringify(content) + "\n");
const holder = () => JSON.parse(fs.readFileSync(lock, "utf8")) as { pid: number; boot?: string };

describe("acquireLock", () => {
  it("creates a {pid, boot} lock and removes it on release", () => {
    const release = acquireLock(lock);
    expect(holder()).toEqual(boot ? { pid: process.pid, boot } : { pid: process.pid });
    expect(() => acquireLock(lock)).not.toThrow(); // own pid: stale, taken over
    release();
    expect(fs.existsSync(lock)).toBe(false);
    expect(fs.readdirSync(dir)).toEqual([]); // no temp files left behind
  });

  it("takes over a lock naming this process's own pid", () => {
    write({ pid: process.pid, boot });
    acquireLock(lock)();
  });

  it.runIf(boot)("takes over a live pid's lock from another boot", () => {
    write({ pid: process.ppid, boot: "00000000-0000-0000-0000-000000000000" });
    acquireLock(lock);
    expect(holder().pid).toBe(process.pid);
  });

  it("refuses a live pid's lock from this boot", () => {
    write({ pid: process.ppid, boot });
    expect(() => acquireLock(lock)).toThrow(/already running: pid \d+/);
    expect(holder().pid).toBe(process.ppid);
  });

  it("takes over a dead pid's lock", () => {
    write({ pid: 2 ** 22 + 1, boot }); // above Linux's pid_max ceiling
    acquireLock(lock);
    expect(holder().pid).toBe(process.pid);
  });

  it("refuses a fresh unparseable lock but takes over an old one", () => {
    fs.writeFileSync(lock, "");
    expect(() => acquireLock(lock)).toThrow(/already running/);
    const old = new Date(Date.now() - 60_000);
    fs.utimesSync(lock, old, old);
    acquireLock(lock);
    expect(holder().pid).toBe(process.pid);
  });
});
