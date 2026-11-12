// Checking out an order's repos before its harness starts (#51, D6), and
// bundling the work in them when it comes back (#53).
import { execFile, execFileSync } from "node:child_process";
import * as fs from "node:fs";
import * as path from "node:path";
import { promisify } from "node:util";
import type { OrderRepo, WorkRepo } from "./protocol.js";

const execFileP = promisify(execFile);

/** A repo checked out for an order, as harnesses get it. */
export interface RepoCheckout {
  name: string;
  /** `<work-dir>/<order>/<name>`: a clone of the order's bundle, on branch `bmus/<order>`. */
  dir: string;
  /** The ref the work starts from, as the captain gave it. */
  base_ref: string;
  /** base_ref's commit, where `bmus/<order>` started (the bundle's `origin/bmus/<order>`). */
  base_commit: string;
}

/** Same rule as the protocol schema's order_repo.name. */
const REPO_NAME = /^[A-Za-z0-9_-][A-Za-z0-9._-]*$/;
/**
 * Names taken by other files under `<work-dir>/<order>/`: `in/` holds the
 * bundles going in, `out/` the bundles coming back and numbered dirs the
 * checkpoints. The bridge refuses them too.
 */
const RESERVED_NAME = /^(in|out|[0-9]+)$/;
/** Same rule as harness/common.ts's orderDir. */
const ORDER_ID = /^[A-Za-z0-9._-]+$/;

/**
 * Checks out each of an order's repos, in order: the bridge copied a bundle
 * of branch `bmus/<order>` (at base_ref's commit) to `bundle_path`, and this
 * clones it into `<workDir>/<order>/<name>` with `bmus/<order>` checked out.
 * Harness-neutral: serve runs it for every harness before `run`.
 *
 * The bundle is removed once cloned. Re-delivery: a checkout that is already there (the order arrived again,
 * say after serve lost its state) is kept as it is, commits included, and
 * `bmus/<order>` is checked out in it. A clone goes to a temporary dir that
 * is renamed into place only once complete, so a clone cut short by a crash
 * is redone, never mistaken for a checkout.
 */
export async function checkoutRepos(workDir: string, orderId: string, repos: readonly OrderRepo[]): Promise<RepoCheckout[]> {
  if (!ORDER_ID.test(orderId) || orderId === "." || orderId === "..") {
    throw new Error(`order id ${JSON.stringify(orderId)} is not usable as a directory name`);
  }
  const names = new Set<string>();
  for (const r of repos) {
    if (!REPO_NAME.test(r.name)) throw new Error(`repo name ${JSON.stringify(r.name)} is not usable as a directory name`);
    if (RESERVED_NAME.test(r.name)) throw new Error(`repo name ${JSON.stringify(r.name)} is reserved for the order's own files`);
    if (names.has(r.name)) throw new Error(`repo ${r.name} is listed twice`);
    names.add(r.name);
  }
  const branch = `bmus/${orderId}`;
  const out: RepoCheckout[] = [];
  for (const r of repos) {
    const dir = path.join(path.resolve(workDir), orderId, r.name);
    try {
      if (!fs.existsSync(dir)) {
        if (!path.isAbsolute(r.bundle_path)) throw new Error(`bundle_path ${r.bundle_path} is not absolute`);
        const tmp = path.join(path.dirname(dir), `.${r.name}.clone`); // names never start with "."
        fs.rmSync(tmp, { recursive: true, force: true });
        fs.mkdirSync(path.dirname(dir), { recursive: true });
        await git(path.dirname(dir), "clone", "--quiet", "--branch", branch, r.bundle_path, tmp);
        fs.renameSync(tmp, dir);
        fs.rmSync(r.bundle_path, { force: true }); // the checkout holds it all; a full-history bundle can be large
      } else {
        await git(dir, "checkout", "--quiet", branch);
      }
      const base_commit = await git(dir, "rev-parse", "--verify", `refs/remotes/origin/${branch}^{commit}`);
      out.push({ name: r.name, dir, base_ref: r.base_ref, base_commit });
    } catch (err) {
      throw new Error(`checking out repo ${r.name}: ${err instanceof Error ? err.message.trim() : String(err)}`);
    }
  }
  return out;
}

/**
 * Writes a bundle of the order's branch, `bmus/<order>`, in checkout to
 * `out` (its dir created as needed) and returns it as a report or
 * checkpoint names it. The bundle holds the branch minus the order's base
 * commit, which the bridge's clone has; when the branch has no commits past
 * it (or base_commit is unknown), it holds the whole history, because git
 * refuses an empty bundle. `commit` is read back from the bundle, so it is
 * what the bundle carries even if the branch moves meanwhile. The bundle is
 * written next to `out` and renamed into place, so `out` is never partial.
 */
export async function bundleWork(checkout: RepoCheckout, orderId: string, out: string): Promise<WorkRepo> {
  const branch = `bmus/${orderId}`;
  const ref = `refs/heads/${branch}`;
  try {
    await git(checkout.dir, "rev-parse", "--verify", "--quiet", `${ref}^{commit}`).catch(() => {
      throw new Error(`branch ${branch} not found`);
    });
    const ahead = await git(checkout.dir, "rev-list", "--count", `${checkout.base_commit}..${ref}`).catch(() => "0");
    const exclude = Number(ahead) > 0 ? [`^${checkout.base_commit}`] : [];
    fs.mkdirSync(path.dirname(out), { recursive: true });
    const tmp = `${out}.tmp`;
    try {
      await git(checkout.dir, "bundle", "create", "--quiet", tmp, ref, ...exclude);
      fs.renameSync(tmp, out);
    } catch (err) {
      fs.rmSync(tmp, { force: true });
      throw err;
    }
    const head = (await git(checkout.dir, "bundle", "list-heads", out, ref)).split(/\s+/)[0] ?? "";
    if (!/^[0-9a-f]{40,64}$/.test(head)) throw new Error(`bundle ${out} has no ${ref}`);
    return { name: checkout.name, bundle_path: out, commit: head, branch };
  } catch (err) {
    throw new Error(`bundling repo ${checkout.name}: ${err instanceof Error ? err.message.trim() : String(err)}`);
  }
}

/** Runs git in cwd with gitEnv and returns its trimmed stdout. */
export async function git(cwd: string, ...args: string[]): Promise<string> {
  const { stdout } = await execFileP("git", args, { cwd, env: gitEnv(), maxBuffer: 16 << 20 });
  return stdout.trim();
}

/** `git rev-parse --local-env-vars` as of git 2.43, in case the command fails. */
const LOCAL_ENV_VARS = `GIT_ALTERNATE_OBJECT_DIRECTORIES GIT_CONFIG GIT_CONFIG_PARAMETERS GIT_CONFIG_COUNT
  GIT_OBJECT_DIRECTORY GIT_DIR GIT_WORK_TREE GIT_IMPLICIT_WORK_TREE GIT_GRAFT_FILE GIT_INDEX_FILE
  GIT_NO_REPLACE_OBJECTS GIT_REPLACE_REF_BASE GIT_PREFIX GIT_SHALLOW_FILE GIT_COMMON_DIR`.split(/\s+/);
let localEnvVars: Set<string> | undefined;

/**
 * serve's environment minus the variables that point git at another
 * repository (`git rev-parse --local-env-vars`, GIT_CONFIG_KEY_<n> and
 * GIT_CONFIG_VALUE_<n>), so a checkout's git works on that checkout only.
 */
export function gitEnv(env: NodeJS.ProcessEnv = process.env): NodeJS.ProcessEnv {
  if (!localEnvVars) {
    localEnvVars = new Set(LOCAL_ENV_VARS);
    try {
      for (const v of execFileSync("git", ["rev-parse", "--local-env-vars"], { encoding: "utf8" }).split(/\s+/)) if (v) localEnvVars.add(v);
    } catch {
      // keep the built-in list
    }
  }
  const out: NodeJS.ProcessEnv = {};
  for (const [k, v] of Object.entries(env)) {
    if (localEnvVars.has(k) || k.startsWith("GIT_CONFIG_KEY_") || k.startsWith("GIT_CONFIG_VALUE_")) continue;
    out[k] = v;
  }
  return out;
}
