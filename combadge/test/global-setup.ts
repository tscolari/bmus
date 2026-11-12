import * as fs from "node:fs";
import * as os from "node:os";
import * as path from "node:path";
import type { TestProject } from "vitest/node";
import { buildCombadge } from "../esbuild.config.mjs";

declare module "vitest" {
  export interface ProvidedContext {
    /** Path of a freshly built combadge bundle, run as `node <bin> serve|attach`. */
    combadgeBin: string;
  }
}

/**
 * Bundles combadge once so the process tests spawn serve and attach as real
 * processes. COMBADGE_BIN skips the build and tests that bundle instead: CI
 * sets it to the `npm run build` output, so the shipped file is what runs.
 */
export default async function setup(project: TestProject) {
  const prebuilt = process.env.COMBADGE_BIN;
  if (prebuilt) {
    const bin = path.resolve(prebuilt);
    if (!fs.existsSync(bin)) throw new Error(`COMBADGE_BIN: ${bin} does not exist; run npm run build first`);
    project.provide("combadgeBin", bin);
    return;
  }
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "combadge-bin-"));
  const bin = path.join(dir, "combadge.mjs");
  await buildCombadge(bin);
  project.provide("combadgeBin", bin);
  return () => fs.rmSync(dir, { recursive: true, force: true });
}
