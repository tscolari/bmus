// Loading the environment serve should have had (#86).
import * as fs from "node:fs";

/**
 * Variables an env file never sets: they describe the shell or session that
 * wrote it (`sh -c 'env -0 > file'` over `tsh beams exec`), not the beam.
 */
const SESSION_VAR = /^(SSH_.*|PWD|OLDPWD|SHLVL|_|MOTD_SHOWN|TERM)$/;

/**
 * Reads `env -0` output from file (NUL-separated `NAME=value` entries) into
 * env. A variable env already has keeps its value, and session variables
 * (SESSION_VAR) are skipped. Returns the names it set.
 *
 * beam-init services start with little more than HOME, PATH and USER, while
 * `tsh beams exec` sessions also get the LLM proxy's settings
 * (ANTHROPIC_BASE_URL and friends) that the sdk harness's claude needs. The
 * tsh transporter captures the latter into a file and passes it to serve.
 */
export function loadEnvFile(file: string, env: NodeJS.ProcessEnv = process.env): string[] {
  const set: string[] = [];
  for (const entry of fs.readFileSync(file, "utf8").split("\0")) {
    const eq = entry.indexOf("=");
    if (eq <= 0) continue; // the empty entry after the last NUL
    const name = entry.slice(0, eq);
    if (SESSION_VAR.test(name) || Object.hasOwn(env, name)) continue;
    env[name] = entry.slice(eq + 1);
    set.push(name);
  }
  return set;
}
