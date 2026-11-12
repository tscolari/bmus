import * as fs from "node:fs";
import * as path from "node:path";
import type { BeamToBridgeMessage } from "./protocol.js";

/** One outbox entry: its seq and the exact NDJSON line written (with "\n"). */
export interface Entry {
  seq: number;
  line: string;
}

/**
 * Append-only NDJSON file of every beam->bridge message (plan §5). Seq starts
 * at 1 and is monotonic for the life of the file: opening an existing outbox
 * resumes after its last entry.
 *
 * `append` writes and fdatasyncs a line before returning, and `serve` sends
 * it to attach connections only after that, so a line on the wire is on disk.
 * A crash can still leave a torn last line (no trailing newline): it was never
 * sent, so it is cut off on open and its seq is reused. Seqs stay contiguous,
 * which keeps the bridge's highest-contiguous ack moving.
 *
 * Nothing is ever trimmed in Phase 1; acks only lower `depth`.
 */
export class Outbox {
  readonly path: string;
  private fd: number;
  private entries: Entry[] = [];
  private nextSeq = 1;

  constructor(file: string) {
    this.path = file;
    fs.mkdirSync(path.dirname(file), { recursive: true });
    this.load();
    this.fd = fs.openSync(file, "a");
  }

  /** Highest seq written so far (0 when empty). */
  get lastSeq(): number {
    return this.nextSeq - 1;
  }

  /**
   * Assigns the next seq to msg, appends it durably and returns the entry.
   * Throws if the write fails; the outbox is then unusable and the caller
   * must stop rather than carry on with a half-written line.
   */
  append(msg: Omit<BeamToBridgeMessage, "seq">): Entry {
    const seq = this.nextSeq;
    // Envelope fields in protocol order; JSON.stringify drops an undefined order.
    const full = { v: msg.v, seq, crew: msg.crew, order: msg.order, type: msg.type, ts: msg.ts, body: msg.body };
    const entry = { seq, line: JSON.stringify(full) + "\n" };
    const buf = Buffer.from(entry.line);
    for (let off = 0; off < buf.length; ) off += fs.writeSync(this.fd, buf, off);
    fs.fdatasyncSync(this.fd);
    this.nextSeq++;
    this.entries.push(entry);
    return entry;
  }

  /** Entries with seq > since, oldest first. */
  since(since: number): Entry[] {
    return this.entries.filter((e) => e.seq > since);
  }

  /** Number of entries with seq > acked. */
  depth(acked: number): number {
    return this.since(acked).length;
  }

  close(): void {
    fs.closeSync(this.fd);
  }

  private load(): void {
    let data: Buffer;
    try {
      data = fs.readFileSync(this.path);
    } catch (err) {
      if ((err as NodeJS.ErrnoException).code === "ENOENT") return;
      throw err;
    }
    const end = data.lastIndexOf(0x0a) + 1;
    for (const raw of data.subarray(0, end).toString("utf8").split("\n")) {
      if (raw === "") continue;
      const seq = parseSeq(raw);
      if (seq === undefined) {
        console.error(`combadge: outbox ${this.path}: skipping unparseable line`);
        continue;
      }
      this.entries.push({ seq, line: raw + "\n" });
      this.nextSeq = Math.max(this.nextSeq, seq + 1);
    }
    if (end < data.length) {
      console.error(`combadge: outbox ${this.path}: dropping torn last line, reusing seq ${this.nextSeq}`);
      fs.truncateSync(this.path, end);
    }
  }
}

function parseSeq(line: string): number | undefined {
  try {
    const seq: unknown = (JSON.parse(line) as { seq?: unknown }).seq;
    return Number.isSafeInteger(seq) && (seq as number) > 0 ? (seq as number) : undefined;
  } catch {
    return undefined;
  }
}
