import * as fs from "node:fs";
import * as path from "node:path";

interface StateData {
  /** Bridge command ids (envelope seq) already handled. */
  handled: number[];
  /** Highest outbox seq the bridge acked. */
  acked: number;
  /** Order a harness is running, so a restart can report it lost. */
  running?: string;
  /** The running order, once a stand_down or recall for it was relayed: it ends stopped, not completed, on an error or a restart. */
  stopping?: string;
  /** Orders that ended (report, error or lost to a restart); a resend is ignored. */
  completed: string[];
  /** Orders a stand_down or recall stopped; a resend runs again, from a clean order dir. */
  stopped: string[];
}

/** One atomic change to the state; every field present is applied. */
export interface Change {
  handled?: number;
  acked?: number;
  /** null clears the running order (and stopping); an order id also takes it off the stopped orders. */
  running?: string | null;
  stopping?: string;
  completed?: string;
  stopped?: string;
}

/**
 * Small JSON file next to the outbox holding what `serve` must remember across
 * restarts: handled command ids (dedup), the last ack, the running order, the
 * completed orders and the stopped ones. Each `record` is one durable write: temp file, fsync,
 * rename, fsync of the directory.
 *
 * Command ids are kept as a set, not a high-water mark, so dedup assumes only
 * that the bridge never reuses an id, not that ids arrive in order.
 */
export class State {
  private data: StateData;
  private handled: Set<number>;
  private completed: Set<string>;

  constructor(readonly path: string) {
    this.data = read(path);
    this.handled = new Set(this.data.handled);
    this.completed = new Set(this.data.completed);
  }

  get acked(): number {
    return this.data.acked;
  }

  get running(): string | undefined {
    return this.data.running;
  }

  get stopping(): string | undefined {
    return this.data.stopping;
  }

  isHandled(id: number): boolean {
    return this.handled.has(id);
  }

  isCompleted(order: string): boolean {
    return this.completed.has(order);
  }

  isStopped(order: string): boolean {
    return this.data.stopped.includes(order);
  }

  record(change: Change): void {
    if (change.handled !== undefined && !this.handled.has(change.handled)) {
      this.handled.add(change.handled);
      this.data.handled.push(change.handled);
    }
    if (change.acked !== undefined) this.data.acked = Math.max(this.data.acked, change.acked);
    if (change.running === null) {
      delete this.data.running;
      delete this.data.stopping;
    }
    else if (change.running !== undefined) {
      this.data.running = change.running;
      this.data.stopped = this.data.stopped.filter((o) => o !== change.running);
    }
    if (change.completed !== undefined && !this.completed.has(change.completed)) {
      this.completed.add(change.completed);
      this.data.completed.push(change.completed);
    }
    if (change.stopping !== undefined) this.data.stopping = change.stopping;
    if (change.stopped !== undefined && !this.data.stopped.includes(change.stopped)) this.data.stopped.push(change.stopped);
    this.save();
  }

  private save(): void {
    const tmp = `${this.path}.tmp`;
    const fd = fs.openSync(tmp, "w");
    try {
      fs.writeFileSync(fd, JSON.stringify(this.data) + "\n");
      fs.fsyncSync(fd);
    } finally {
      fs.closeSync(fd);
    }
    fs.renameSync(tmp, this.path);
    const dir = fs.openSync(path.dirname(this.path), "r");
    try {
      fs.fsyncSync(dir);
    } finally {
      fs.closeSync(dir);
    }
  }
}

function read(file: string): StateData {
  let raw: string;
  try {
    raw = fs.readFileSync(file, "utf8");
  } catch (err) {
    if ((err as NodeJS.ErrnoException).code === "ENOENT") return { handled: [], acked: 0, completed: [], stopped: [] };
    throw err;
  }
  const parsed = JSON.parse(raw) as Partial<StateData>;
  const data: StateData = {
    handled: parsed.handled ?? [],
    acked: parsed.acked ?? 0,
    completed: parsed.completed ?? [],
    stopped: parsed.stopped ?? [],
  };
  if (parsed.running !== undefined) data.running = parsed.running;
  if (parsed.stopping !== undefined) data.stopping = parsed.stopping;
  return data;
}
