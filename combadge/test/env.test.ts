import * as fs from "node:fs";
import * as os from "node:os";
import * as path from "node:path";
import { afterEach, beforeEach, describe, expect, it } from "vitest";
import { loadEnvFile } from "../src/env.js";
import { ECHO, serveFails } from "./procs.js";

let dir: string;
beforeEach(() => (dir = fs.mkdtempSync(path.join(os.tmpdir(), "combadge-env-"))));
afterEach(() => fs.rmSync(dir, { recursive: true, force: true }));

const envFile = (...entries: string[]) => {
  const file = path.join(dir, "beam.env");
  fs.writeFileSync(file, entries.map((e) => e + "\0").join(""));
  return file;
};

describe("loadEnvFile", () => {
  it("sets missing variables, keeps existing ones and skips session ones", () => {
    const file = envFile(
      "HOME=/elsewhere",
      "ANTHROPIC_BASE_URL=https://anthropic.example",
      "MULTI=a\nb=c",
      "EMPTY=",
      "SSH_CONNECTION=1 2 3 4",
      "PWD=/home/beams",
      "SHLVL=1",
      "_=/usr/bin/sh",
    );
    const env: NodeJS.ProcessEnv = { HOME: "/home/beams" };
    expect(loadEnvFile(file, env)).toEqual(["ANTHROPIC_BASE_URL", "MULTI", "EMPTY"]);
    expect(env).toEqual({ HOME: "/home/beams", ANTHROPIC_BASE_URL: "https://anthropic.example", MULTI: "a\nb=c", EMPTY: "" });
  });

  it("reads an empty file as nothing to set", () => {
    expect(loadEnvFile(envFile(), {})).toEqual([]);
  });
});

describe("serve --env-file", () => {
  it("fails at startup when the file is missing", async () => {
    const res = await serveFails(dir, undefined, [...ECHO, "--env-file", path.join(dir, "nope.env")]);
    expect(res.code).not.toBe(0);
    expect(res.stderr).toContain("nope.env");
  });
});
