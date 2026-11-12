import * as fs from "node:fs";

/** Lock file content: who holds it, and in which boot. */
interface Holder {
  pid: number;
  /** Linux boot id; absent where /proc doesn't provide it. */
  boot?: string;
}

/** An empty or unparseable lock younger than this may still be being written. */
const UNREADABLE_GRACE_MS = 10_000;

/**
 * Takes an exclusive lock at file, so only one `serve` uses an outbox at a
 * time (tsh may run an Exec'd command twice). Returns a release function.
 *
 * The lock is a JSON `{pid, boot}` file, created atomically with its content
 * (temp file, then link). An existing lock is stale, and taken over, when:
 * its pid is ours or no longer alive; it comes from another boot (a pid from
 * before a reboot may now belong to anything); or it is empty or unparseable
 * and older than 10s.
 *
 * Node has no flock, so this is best effort: two processes taking over the
 * same stale lock at the same instant can both win. That isn't expected on a
 * beam.
 */
export function acquireLock(file: string): () => void {
  const mine = JSON.stringify({ pid: process.pid, boot: bootId() }) + "\n";
  for (let attempt = 0; attempt < 3; attempt++) {
    if (tryCreate(file, mine)) return () => release(file, mine);
    let held: Holder | undefined;
    let mtimeMs: number;
    try {
      ({ held, mtimeMs } = readLock(file));
    } catch (err) {
      if ((err as NodeJS.ErrnoException).code === "ENOENT") continue; // released meanwhile: try again
      throw err;
    }
    const reason = staleReason(held, mtimeMs);
    if (reason === undefined) {
      throw new Error(`already running: ${held ? `pid ${held.pid}` : "another serve"} holds ${file}`);
    }
    console.error(`combadge: taking over stale lock ${file} (${reason})`);
    fs.rmSync(file, { force: true });
  }
  throw new Error(`already running: could not take ${file}`);
}

/** Creates file holding content atomically; false if it already exists. */
function tryCreate(file: string, content: string): boolean {
  const tmp = `${file}.${process.pid}.tmp`;
  fs.writeFileSync(tmp, content);
  try {
    fs.linkSync(tmp, file);
    return true;
  } catch (err) {
    if ((err as NodeJS.ErrnoException).code === "EEXIST") return false;
    throw err;
  } finally {
    fs.rmSync(tmp, { force: true });
  }
}

function readLock(file: string): { held: Holder | undefined; mtimeMs: number } {
  const { mtimeMs } = fs.statSync(file);
  const raw = fs.readFileSync(file, "utf8");
  try {
    const h = JSON.parse(raw) as Partial<Holder>;
    if (Number.isSafeInteger(h.pid) && (h.pid as number) > 0) {
      const held: Holder = { pid: h.pid as number };
      if (typeof h.boot === "string") held.boot = h.boot;
      return { held, mtimeMs };
    }
  } catch {
    // fall through: unparseable
  }
  return { held: undefined, mtimeMs };
}

/** Why the lock is stale, or undefined if it is held. */
function staleReason(held: Holder | undefined, mtimeMs: number): string | undefined {
  if (!held) return Date.now() - mtimeMs > UNREADABLE_GRACE_MS ? "unreadable and old" : undefined;
  if (held.pid === process.pid) return "our own pid";
  const boot = bootId();
  if (held.boot !== undefined && boot !== undefined && held.boot !== boot) return "from another boot";
  if (!alive(held.pid)) return `pid ${held.pid} is gone`;
  return undefined;
}

/** Removes the lock only if it is still ours. */
function release(file: string, mine: string): void {
  try {
    if (fs.readFileSync(file, "utf8") === mine) fs.rmSync(file, { force: true });
  } catch {
    // already gone
  }
}

function bootId(): string | undefined {
  try {
    return fs.readFileSync("/proc/sys/kernel/random/boot_id", "utf8").trim() || undefined;
  } catch {
    return undefined;
  }
}

function alive(pid: number): boolean {
  try {
    process.kill(pid, 0);
    return true;
  } catch (err) {
    return (err as NodeJS.ErrnoException).code === "EPERM";
  }
}
