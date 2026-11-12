// Builds bmus once for the suite and checks the combadge bundle the
// holodeck beams run is there. The binary's path reaches the tests through
// BMUS_E2E_BIN; the combadge bundle's through BMUS_COMBADGE.
import { execFileSync } from "node:child_process";
import * as fs from "node:fs";
import * as os from "node:os";
import * as path from "node:path";

const repoRoot = path.resolve(import.meta.dirname, "..", "..");

export default function globalSetup() {
  const combadge = process.env["BMUS_COMBADGE"] || path.join(repoRoot, "combadge", "dist", "combadge.mjs");
  if (!fs.existsSync(combadge)) {
    throw new Error(`no combadge bundle at ${combadge}: run "npm ci && npm run build" in combadge/, or set BMUS_COMBADGE`);
  }
  process.env["BMUS_COMBADGE"] = path.resolve(combadge);

  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "bmus-e2e-bin-"));
  const bin = path.join(dir, "bmus");
  execFileSync("go", ["build", "-o", bin, "./cmd/bmus"], { cwd: repoRoot, stdio: "inherit" });
  process.env["BMUS_E2E_BIN"] = bin;

  return () => fs.rmSync(dir, { recursive: true, force: true });
}
