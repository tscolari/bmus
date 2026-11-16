// protocol/cases/verdict.json (#181) through ajv and the sdk report tool's
// zod. comms TestSchemaVerdict runs the same cases through the Go validator
// and comms.Verdict.Check.
import * as fs from "node:fs";
import * as path from "node:path";
import { describe, expect, it } from "vitest";
import { verdictInput } from "../src/harness/sdk.js";
import type { Message } from "../src/protocol.js";
import { compileSchema, protocolDir } from "./schema.js";

interface VerdictCase {
  name: string;
  ok: boolean;
  schema?: boolean;
  check?: boolean;
  zod?: boolean;
  verdict: unknown;
}

/** Replaces each {"repeat": x, "times": n}: x repeated n times when a string, else n copies of x. */
function expand(v: unknown): unknown {
  if (Array.isArray(v)) return v.map(expand);
  if (typeof v !== "object" || v === null) return v;
  const o = v as Record<string, unknown>;
  if ("repeat" in o && "times" in o && Object.keys(o).length === 2) {
    const n = o.times as number;
    return typeof o.repeat === "string" ? o.repeat.repeat(n) : Array.from({ length: n }, () => expand(o.repeat));
  }
  return Object.fromEntries(Object.entries(o).map(([k, x]) => [k, expand(x)]));
}

const { validate } = compileSchema();
const cases = (JSON.parse(fs.readFileSync(path.join(protocolDir, "cases", "verdict.json"), "utf8")) as { cases: VerdictCase[] }).cases;
const golden = JSON.parse(fs.readFileSync(path.join(protocolDir, "golden", "report.review.json"), "utf8")) as Message<"report">;

describe("verdict cases", () => {
  it("has cases", () => expect(cases.length).toBeGreaterThan(10));

  it.each(cases.map((c) => [c.name, c] as const))("%s", (_, c) => {
    const verdict = expand(c.verdict);
    const msg = { ...golden, body: { ...golden.body, verdict } };
    expect(validate(msg), JSON.stringify(validate.errors)).toBe(c.schema ?? c.ok);
    expect(verdictInput.safeParse(verdict).success).toBe(c.zod ?? c.ok);
  });
});
