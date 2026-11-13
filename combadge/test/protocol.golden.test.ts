import * as fs from "node:fs";
import * as path from "node:path";
import { describe, expect, it } from "vitest";
import {
  BEAM_TO_BRIDGE_TYPES,
  BRIDGE_TO_BEAM_TYPES,
  PROTOCOL_VERSION,
  type Message,
} from "../src/protocol.js";
import { compileSchema, protocolDir } from "./schema.js";

const { schema, validate } = compileSchema();
const goldenDir = path.join(protocolDir, "golden");
const goldens = fs.readdirSync(goldenDir).filter((f) => f.endsWith(".json")).sort();
const tsTypes: readonly string[] = [...BEAM_TO_BRIDGE_TYPES, ...BRIDGE_TO_BEAM_TYPES];

describe("protocol golden files", () => {
  it("has one golden per message type, and variants named <type>.<variant>.json of known types", () => {
    const names = goldens.map((f) => f.replace(/\.json$/, ""));
    expect(names.filter((n) => !n.includes("."))).toEqual([...tsTypes].sort());
    for (const n of names.filter((n) => n.includes("."))) expect(tsTypes).toContain(n.split(".")[0]);
  });

  it("lists the same message types as the schema", () => {
    const schemaTypes = (schema as { properties: { type: { enum: string[] } } }).properties.type.enum;
    expect([...tsTypes].sort()).toEqual([...schemaTypes].sort());
  });

  it.each(goldens)("%s validates against the schema", (file) => {
    const raw = fs.readFileSync(path.join(goldenDir, file), "utf8");
    const msg = JSON.parse(raw) as Message;
    if (!validate(msg)) expect.fail(JSON.stringify(validate.errors, null, 2));
    expect(msg.v).toBe(PROTOCOL_VERSION);
    expect(file.split(".")[0]).toBe(msg.type);
    // The TS envelope serializes in protocol order, so a golden survives a round trip byte for byte.
    const { v, seq, crew, order, type, ts, body } = msg;
    expect(JSON.stringify({ v, seq, crew, order, type, ts, body })).toBe(raw.trim());
  });

  it("accepts a correction without a question and rejects one with an empty question (#190)", () => {
    const golden = JSON.parse(fs.readFileSync(path.join(goldenDir, "correction.json"), "utf8")) as Message<"correction">;
    const { question: _, ...without } = golden.body;
    expect(validate({ ...golden, body: without })).toBe(true);
    expect(validate({ ...golden, body: { ...golden.body, question: "" } })).toBe(false);
    expect(validate.errors?.map((e) => e.keyword)).toContain("minLength");
  });

  it("rejects a beam->bridge message with seq 0", () => {
    const hello: Message<"hello"> = {
      v: 1, seq: 0, crew: "sorell", type: "hello", ts: "2026-10-16T09:00:00Z",
      body: { protocol: 1, harness: "echo", combadge_version: "0.1.0" },
    };
    expect(validate(hello)).toBe(false);
  });
});
