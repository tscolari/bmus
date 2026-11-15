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
  /** The order's work in progress checkoutRepos found (#145), if any; see Wip. */
  wip?: Wip;
}

/**
 * Work in progress checkoutRepos found for a repo (#145): the commit on
 * `refs/bmus/wip/<order>`, where it came from, and whether its tree was
 * restored into the work tree. `files` are the paths it changes from its
 * parent; `kept` says why it wasn't restored.
 */
export interface Wip {
  commit: string;
  parent: string;
  /** "checkout": the order's existing checkout on this beam; "bundle": the shipped bundle. */
  from: "checkout" | "bundle";
  restored: boolean;
  files: string[];
  kept?: string;
}

/** The ref an order's work in progress is snapshotted to (#145): never a branch, so nothing takes it for work. */
export function wipRef(orderId: string): string {
  return `refs/bmus/wip/${orderId}`;
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
  const ref = wipRef(orderId);
  const out: RepoCheckout[] = [];
  for (const r of repos) {
    const dir = path.join(path.resolve(workDir), orderId, r.name);
    const prev = previousCheckout(dir);
    try {
      /** WIP candidates (#145): the existing checkout's (kept, or set aside by a stop), then the shipped bundle's. */
      let local: string | undefined;
      let shipped: string | undefined;
      if (!fs.existsSync(dir)) {
        if (!path.isAbsolute(r.bundle_path)) throw new Error(`bundle_path ${r.bundle_path} is not absolute`);
        const tmp = path.join(path.dirname(dir), `.${r.name}.clone`); // names never start with "."
        fs.rmSync(tmp, { recursive: true, force: true });
        fs.mkdirSync(path.dirname(dir), { recursive: true });
        await git(path.dirname(dir), "clone", "--quiet", "--branch", branch, r.bundle_path, tmp);
        shipped = await fetchWip(tmp, r.bundle_path, ref, "refs/bmus/shipped-wip");
        if (fs.existsSync(prev)) local = await fetchWip(tmp, prev, ref, "refs/bmus/checkout-wip");
        fs.renameSync(tmp, dir);
        fs.rmSync(r.bundle_path, { force: true }); // the checkout holds it all; a full-history bundle can be large
      } else {
        await git(dir, "checkout", "--quiet", branch);
        local = await git(dir, "rev-parse", "--verify", "--quiet", `${ref}^{commit}`).catch(() => undefined);
        if (path.isAbsolute(r.bundle_path) && fs.existsSync(r.bundle_path)) {
          shipped = await fetchWip(dir, r.bundle_path, ref, "refs/bmus/shipped-wip");
        }
      }
      fs.rmSync(prev, { recursive: true, force: true });
      const base_commit = await git(dir, "rev-parse", "--verify", `refs/remotes/origin/${branch}^{commit}`);
      const wip = await placeWip(dir, orderId, local, shipped);
      out.push({ name: r.name, dir, base_ref: r.base_ref, base_commit, ...(wip && { wip }) });
    } catch (err) {
      throw new Error(`checking out repo ${r.name}: ${err instanceof Error ? err.message.trim() : String(err)}`);
    }
  }
  return out;
}

/**
 * Where clearStopped (serve.ts) sets a stopped order's checkout aside, so
 * the next run can take its work in progress (#145): `<order>/.<name>.prev`.
 */
export function previousCheckout(dir: string): string {
  return path.join(path.dirname(dir), `.${path.basename(dir)}.prev`);
}

/**
 * Fetches `refs/bmus/wip/<order>` from source (a bundle or a checkout) into
 * repo as `as`, and returns its commit; undefined when source has none or
 * it can't be fetched (a bundle whose prerequisites the repo lacks, say).
 */
async function fetchWip(repo: string, source: string, ref: string, as: string): Promise<string | undefined> {
  try {
    await git(repo, "fetch", "--quiet", "--no-tags", "--no-write-fetch-head", source, `+${ref}:${as}`);
    const commit = await git(repo, "rev-parse", "--verify", "--quiet", `${as}^{commit}`);
    await git(repo, "update-ref", "-d", as);
    return commit;
  } catch {
    return undefined;
  }
}

/**
 * Picks the order's work in progress from the candidates (#145), sets
 * `refs/bmus/wip/<order>` to it, and restores its tree into the work tree
 * with nothing staged when its parent is the `bmus/<order>` tip being
 * started and the work tree has nothing uncommitted. The existing
 * checkout's WIP comes first: it was snapshotted on this beam, at least as
 * late as the bundle's. A WIP on any other commit is kept as the ref only.
 */
async function placeWip(dir: string, orderId: string, local: string | undefined, shipped: string | undefined): Promise<Wip | undefined> {
  const tip = await git(dir, "rev-parse", "--verify", `refs/heads/bmus/${orderId}^{commit}`);
  const found: { commit: string; parent: string; from: Wip["from"] }[] = [];
  for (const [commit, from] of [[local, "checkout"], [shipped, "bundle"]] as const) {
    if (commit === undefined) continue;
    const parent = await git(dir, "rev-parse", "--verify", "--quiet", `${commit}^1`).catch(() => "");
    found.push({ commit, parent, from });
  }
  // On the tip first, then the bundle's: the bridge ships its newest.
  const pick = found.find((w) => w.parent === tip) ?? found.find((w) => w.from === "bundle") ?? found[0];
  if (!pick) return undefined;
  await git(dir, "update-ref", wipRef(orderId), pick.commit);
  const files = pick.parent === "" ? [] : (await git(dir, "diff", "--no-renames", "--name-only", pick.parent, pick.commit)).split("\n").filter(Boolean);
  const wip: Wip = { ...pick, restored: false, files };
  if (pick.parent !== tip) return { ...wip, kept: `it was snapshotted on ${short(pick.parent)}, not on bmus/${orderId}'s tip ${short(tip)}` };
  if ((await git(dir, "status", "--porcelain")) !== "") return { ...wip, kept: "the work tree already had uncommitted changes" };
  await git(dir, "restore", `--source=${pick.commit}`, "--worktree", "--", ".");
  return { ...wip, restored: true };
}

const short = (commit: string) => (commit === "" ? "no commit" : commit.slice(0, 12));

/**
 * The lines a run's handover note gains for the work in progress
 * checkoutRepos found (#145), one per repo that had any; empty with none.
 */
export function wipNote(orderId: string, repos: readonly RepoCheckout[]): string[] {
  const lines: string[] = [];
  for (const r of repos) {
    const w = r.wip;
    if (!w) continue;
    const where = w.from === "checkout" ? "the order's existing checkout on this beam" : "the shipped checkpoint";
    const files = w.files.length > 0 ? w.files.join(", ") : "no files";
    if (w.restored) {
      lines.push(`Uncommitted work was restored in ${r.dir} from ${wipRef(orderId)} (${short(w.commit)}, from ${where}), unstaged; files: ${files}. Review it and commit what you keep.`);
    } else {
      lines.push(`${wipRef(orderId)} in ${r.dir} (${short(w.commit)}, from ${where}) holds uncommitted work that was not restored: ${w.kept}. Files: ${files}. See it with git diff ${short(w.parent)} ${wipRef(orderId)}.`);
    }
  }
  return lines;
}

/**
 * Snapshots checkout's uncommitted changes (#145), tracked and untracked
 * minus ignored files, as a commit on `refs/bmus/wip/<order>` whose parent
 * is the `bmus/<order>` tip. It is built in a temporary index, so the
 * agent's index and work tree are left as they were, and `bmus/<order>`
 * never moves. With nothing uncommitted the ref is deleted and the result
 * has no commit. tip and tree are what it read: the tip, and the work
 * tree's tree (the tip's own when nothing is uncommitted).
 */
export async function snapshotWip(checkout: RepoCheckout, orderId: string): Promise<{ tip: string; tree: string; commit?: string }> {
  const dir = checkout.dir;
  const ref = wipRef(orderId);
  const tip = await git(dir, "rev-parse", "--verify", "--quiet", `refs/heads/bmus/${orderId}^{commit}`).catch(() => {
    throw new Error(`bundling repo ${checkout.name}: branch bmus/${orderId} not found`);
  });
  const gitDir = await git(dir, "rev-parse", "--absolute-git-dir");
  const index = path.join(gitDir, `bmus-wip-${process.pid}-${++snapshots}.index`);
  const env = { GIT_INDEX_FILE: index };
  try {
    await gitWith(env, dir, "read-tree", tip);
    await gitWith(env, dir, "add", "--all", "--", ".");
    const tree = await gitWith(env, dir, "write-tree");
    if (tree === (await git(dir, "rev-parse", `${tip}^{tree}`))) {
      await git(dir, "update-ref", "-d", ref);
      return { tip, tree };
    }
    const who = { GIT_AUTHOR_NAME: "bmus", GIT_AUTHOR_EMAIL: "bmus@bmus.invalid", GIT_COMMITTER_NAME: "bmus", GIT_COMMITTER_EMAIL: "bmus@bmus.invalid" };
    const commit = await gitWith(who, dir, "commit-tree", tree, "-p", tip, "-m", `bmus: work in progress on bmus/${orderId}`);
    await git(dir, "update-ref", ref, commit);
    return { tip, tree, commit };
  } finally {
    fs.rmSync(index, { force: true });
    fs.rmSync(`${index}.lock`, { force: true });
  }
}

let snapshots = 0;

/** What bundleWork puts in a checkpoint's bundle beyond `bmus/<order>` (#145). */
export interface BundleOptions {
  /** Carry `refs/bmus/wip/<order>` too, when it exists. */
  wip?: boolean;
  /**
   * The previous checkpoint's commit: the bundle holds only what came
   * after it, when `bmus/<order>` has commits past it, and a full bundle
   * (as without `since`) goes to `<out minus .bundle>.full.bundle` as
   * the fallback. Otherwise the bundle is the full one.
   */
  since?: string;
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
export async function bundleWork(checkout: RepoCheckout, orderId: string, out: string, opts: BundleOptions = {}): Promise<WorkRepo> {
  const branch = `bmus/${orderId}`;
  const ref = `refs/heads/${branch}`;
  try {
    await git(checkout.dir, "rev-parse", "--verify", "--quiet", `${ref}^{commit}`).catch(() => {
      throw new Error(`branch ${branch} not found`);
    });
    const refs = [ref];
    if (opts.wip && (await git(checkout.dir, "rev-parse", "--verify", "--quiet", `${wipRef(orderId)}^{commit}`).catch(() => ""))) {
      refs.push(wipRef(orderId));
    }
    /** Commits of the branch past from, 0 when from is unknown. */
    const past = async (from: string) => Number(await git(checkout.dir, "rev-list", "--count", `${from}..${ref}`).catch(() => "0"));
    const full = (await past(checkout.base_commit)) > 0 ? [`^${checkout.base_commit}`] : [];
    const write = async (file: string, exclude: string[]) => {
      fs.mkdirSync(path.dirname(file), { recursive: true });
      const tmp = `${file}.tmp`;
      try {
        await git(checkout.dir, "bundle", "create", "--quiet", tmp, ...refs, ...exclude);
        fs.renameSync(tmp, file);
      } catch (err) {
        fs.rmSync(tmp, { force: true });
        throw err;
      }
    };
    const incremental = opts.since !== undefined && opts.since !== checkout.base_commit && (await past(opts.since)) > 0;
    let fullPath: string | undefined;
    if (incremental) {
      fullPath = out.replace(/\.bundle$/, "") + ".full.bundle";
      await write(fullPath, full);
      await write(out, [`^${opts.since}`]);
    } else await write(out, full);
    const head = (await git(checkout.dir, "bundle", "list-heads", out, ref)).split(/\s+/)[0] ?? "";
    if (!/^[0-9a-f]{40,64}$/.test(head)) throw new Error(`bundle ${out} has no ${ref}`);
    return { name: checkout.name, bundle_path: out, commit: head, branch, ...(fullPath !== undefined && { full_bundle_path: fullPath }) };
  } catch (err) {
    throw new Error(`bundling repo ${checkout.name}: ${err instanceof Error ? err.message.trim() : String(err)}`);
  }
}

/** Runs git in cwd with gitEnv and returns its trimmed stdout. */
export async function git(cwd: string, ...args: string[]): Promise<string> {
  return gitWith({}, cwd, ...args);
}

/** git with extra variables set over gitEnv (a temporary index, an identity). */
async function gitWith(extra: NodeJS.ProcessEnv, cwd: string, ...args: string[]): Promise<string> {
  const { stdout } = await execFileP("git", args, { cwd, env: { ...gitEnv(), ...extra }, maxBuffer: 16 << 20 });
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
