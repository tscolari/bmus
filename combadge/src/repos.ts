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
  /** Restored in the sense that a kept checkout's work tree already holds it. */
  inPlace?: boolean;
  files: string[];
  /** Why it wasn't restored: it is on refs/bmus/kept-wip/<order> instead. */
  kept?: string;
}

/** The ref an order's work in progress is snapshotted to (#145): never a branch, so nothing takes it for work. */
export function wipRef(orderId: string): string {
  return `refs/bmus/wip/${orderId}`;
}

/**
 * The ref a WIP that couldn't be restored moves to (#145): a later snapshot
 * of the work tree never overwrites it, and checkpoints carry it on.
 */
export function keptWipRef(orderId: string): string {
  return `refs/bmus/kept-wip/${orderId}`;
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
 *
 * Work in progress (#145): the shipped bundle's `refs/bmus/wip/<order>` is
 * authoritative. The existing checkout's (kept, or set aside by a stop as
 * `.<name>.prev`), freshly snapshotted, is taken instead only when the
 * shipped tip and WIP are the ones this beam last emitted (emittedRefs):
 * then the bridge has nothing newer. See placeWip for what happens to it.
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
      const existing = fs.existsSync(dir);
      // The existing checkout's work as it stands now, and what this beam last emitted from it.
      const source = existing ? dir : fs.existsSync(prev) ? prev : undefined;
      let local: string | undefined;
      let emitted: { tip?: string; wip?: string } = {};
      if (source !== undefined) {
        emitted = await readEmitted(source, orderId);
        local = (await snapshotWip({ name: r.name, dir: source, base_ref: r.base_ref, base_commit: "" }, orderId).catch(() => undefined))?.commit;
      }
      let shipped: string | undefined;
      let target = dir;
      if (!existing) {
        if (!path.isAbsolute(r.bundle_path)) throw new Error(`bundle_path ${r.bundle_path} is not absolute`);
        const tmp = path.join(path.dirname(dir), `.${r.name}.clone`); // names never start with "."
        fs.rmSync(tmp, { recursive: true, force: true });
        fs.mkdirSync(path.dirname(dir), { recursive: true });
        await git(path.dirname(dir), "clone", "--quiet", "--branch", branch, r.bundle_path, tmp);
        target = tmp;
      } else {
        await git(dir, "checkout", "--quiet", branch);
      }
      if (path.isAbsolute(r.bundle_path) && fs.existsSync(r.bundle_path)) {
        shipped = await fetchWip(target, r.bundle_path, ref, "refs/bmus/shipped-wip");
        await fetchWip(target, r.bundle_path, keptWipRef(orderId), keptWipRef(orderId), true);
      }
      const tip = await git(target, "rev-parse", "--verify", `refs/heads/${branch}^{commit}`);
      // Nothing newer from the bridge: the beam's own work stands.
      const fresh = emitted.tip === tip && emitted.wip === shipped;
      if (!fresh) local = undefined;
      else if (local !== undefined && source !== target) local = await fetchWip(target, source!, ref, "refs/bmus/checkout-wip");
      if (!existing) {
        fs.renameSync(target, dir);
        fs.rmSync(r.bundle_path, { force: true }); // the checkout holds it all; a full-history bundle can be large
      }
      fs.rmSync(prev, { recursive: true, force: true });
      const base_commit = await git(dir, "rev-parse", "--verify", `refs/remotes/origin/${branch}^{commit}`);
      const wip = await placeWip(dir, orderId, local, shipped, existing);
      out.push({ name: r.name, dir, base_ref: r.base_ref, base_commit, ...(wip && { wip }) });
    } catch (err) {
      fs.rmSync(prev, { recursive: true, force: true });
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
 * The refs a checkout records its latest emitted checkpoint on (#145):
 * the `bmus/<order>` tip and the WIP commit (none when it had no WIP).
 */
function emittedRefs(orderId: string): { tip: string; wip: string } {
  return { tip: `refs/bmus/emitted/${orderId}/tip`, wip: `refs/bmus/emitted/${orderId}/wip` };
}

async function readEmitted(dir: string, orderId: string): Promise<{ tip?: string; wip?: string }> {
  const refs = emittedRefs(orderId);
  const read = (r: string) => git(dir, "rev-parse", "--verify", "--quiet", `${r}^{commit}`).catch(() => undefined);
  const tip = await read(refs.tip);
  const wip = await read(refs.wip);
  return { ...(tip !== undefined && { tip }), ...(wip !== undefined && { wip }) };
}

/** Records in checkout the checkpoint it is emitting: its tip and its WIP, if any (#145). */
export async function recordEmitted(dir: string, orderId: string, tip: string, wip: string | undefined): Promise<void> {
  const refs = emittedRefs(orderId);
  await git(dir, "update-ref", refs.tip, tip);
  if (wip !== undefined) await git(dir, "update-ref", refs.wip, wip);
  else await git(dir, "update-ref", "-d", refs.wip);
}

/**
 * Fetches ref from source (a bundle or a checkout) into repo as `as`, and
 * returns its commit; undefined when source has none or it can't be
 * fetched (a bundle whose prerequisites the repo lacks, say). The fetched
 * ref is deleted again unless keep.
 */
async function fetchWip(repo: string, source: string, ref: string, as: string, keep = false): Promise<string | undefined> {
  try {
    await git(repo, "fetch", "--quiet", "--no-tags", "--no-write-fetch-head", source, `+${ref}:${as}`);
    const commit = await git(repo, "rev-parse", "--verify", "--quiet", `${as}^{commit}`);
    if (!keep) await git(repo, "update-ref", "-d", as);
    return commit;
  } catch {
    return undefined;
  }
}

/**
 * Places the order's work in progress (#145): the existing checkout's when
 * given (it is the newest), else the shipped bundle's. When its parent is
 * the `bmus/<order>` tip being started and the work tree is clean, its tree
 * is restored into the work tree with nothing staged, and it stays on
 * `refs/bmus/wip/<order>`. A kept checkout whose work tree already holds it
 * is left as it is. Any other WIP (on another commit, or over a work tree
 * with changes of its own) moves to `refs/bmus/kept-wip/<order>`, which no
 * snapshot overwrites and later checkpoints carry.
 */
async function placeWip(dir: string, orderId: string, local: string | undefined, shipped: string | undefined, existing: boolean): Promise<Wip | undefined> {
  const tip = await git(dir, "rev-parse", "--verify", `refs/heads/bmus/${orderId}^{commit}`);
  const commit = local ?? shipped;
  if (commit === undefined) {
    await git(dir, "update-ref", "-d", wipRef(orderId));
    return undefined;
  }
  const from: Wip["from"] = local !== undefined ? "checkout" : "bundle";
  const parent = await git(dir, "rev-parse", "--verify", "--quiet", `${commit}^1`).catch(() => "");
  const files = parent === "" ? [] : (await git(dir, "diff", "--no-renames", "--name-only", parent, commit)).split("\n").filter(Boolean);
  const wip: Wip = { commit, parent, from, restored: false, files };
  const dirty = (await git(dir, "status", "--porcelain")) !== "";
  if (parent === tip && from === "checkout" && existing && dirty) {
    // The kept checkout's own work tree is the WIP.
    await git(dir, "update-ref", wipRef(orderId), commit);
    return { ...wip, restored: true, inPlace: true };
  }
  if (parent === tip && !dirty) {
    await git(dir, "update-ref", wipRef(orderId), commit);
    await git(dir, "restore", `--source=${commit}`, "--worktree", "--", ".");
    return { ...wip, restored: true };
  }
  await git(dir, "update-ref", keptWipRef(orderId), commit);
  await git(dir, "update-ref", "-d", wipRef(orderId));
  const kept = parent !== tip ? `it was snapshotted on ${short(parent)}, not on bmus/${orderId}'s tip ${short(tip)}` : "the work tree already had uncommitted changes";
  return { ...wip, kept };
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
    if (w.inPlace) {
      lines.push(`Uncommitted work from an earlier run is still in ${r.dir}, as it was left; files: ${files}. Review it and commit what you keep.`);
    } else if (w.restored) {
      lines.push(`Uncommitted work was restored in ${r.dir} from ${wipRef(orderId)} (${short(w.commit)}, from ${where}), unstaged; files: ${files}. Review it and commit what you keep.`);
    } else {
      lines.push(`${keptWipRef(orderId)} in ${r.dir} (${short(w.commit)}, from ${where}) holds uncommitted work that was not restored: ${w.kept}. Files: ${files}. See it with git diff ${short(w.parent)} ${keptWipRef(orderId)}.`);
    }
  }
  return lines;
}

/** Untracked or modified files over this size stay out of a WIP snapshot (#145), so one large file can't sink a checkpoint. */
export const WIP_MAX_FILE_BYTES = 100 * 1024 * 1024;

/** What snapshotWip read and made. */
export interface Snapshot {
  /** The `bmus/<order>` tip. */
  tip: string;
  /** The work tree's tree (the tip's own when nothing is uncommitted); "" when skipped. */
  tree: string;
  /** The WIP commit, when there is uncommitted work and commit was asked for. */
  commit?: string;
  /** Why no WIP was taken: HEAD is elsewhere, or git is mid-operation. */
  skipped?: string;
  /** Files left out for their size. */
  large: string[];
}

/**
 * Snapshots checkout's uncommitted changes (#145), tracked and untracked
 * minus ignored files and files over WIP_MAX_FILE_BYTES, as a commit on
 * `refs/bmus/wip/<order>` whose parent is the `bmus/<order>` tip. It is
 * built in a temporary index seeded from the checkout's own, so the agent's
 * index and work tree are left as they were, and `bmus/<order>` never
 * moves. With nothing uncommitted the ref is deleted and the result has no
 * commit. With HEAD off `bmus/<order>` (another branch, detached) or a
 * rebase, merge or cherry-pick under way, it takes nothing and says why.
 * With opts.commit false it only reads the tree and writes no object or ref
 * (workKey).
 */
export async function snapshotWip(checkout: RepoCheckout, orderId: string, opts: { commit?: boolean } = {}): Promise<Snapshot> {
  const commitIt = opts.commit ?? true;
  const dir = checkout.dir;
  const ref = wipRef(orderId);
  const tip = await git(dir, "rev-parse", "--verify", "--quiet", `refs/heads/bmus/${orderId}^{commit}`).catch(() => {
    throw new Error(`bundling repo ${checkout.name}: branch bmus/${orderId} not found`);
  });
  const gitDir = await git(dir, "rev-parse", "--absolute-git-dir");
  const head = await git(dir, "symbolic-ref", "-q", "HEAD").catch(() => "");
  const busy = ["rebase-merge", "rebase-apply", "MERGE_HEAD", "CHERRY_PICK_HEAD", "REVERT_HEAD"].find((f) => fs.existsSync(path.join(gitDir, f)));
  if (head !== `refs/heads/bmus/${orderId}` || busy !== undefined) {
    if (commitIt) await git(dir, "update-ref", "-d", ref);
    const skipped = busy !== undefined ? `git has a ${busy.replace(/[-_].*/, "").toLowerCase()} under way` : `HEAD is ${head === "" ? "detached" : head.replace(/^refs\/heads\//, "")}, not bmus/${orderId}`;
    return { tip, tree: "", skipped, large: [] };
  }
  const index = path.join(gitDir, `bmus-wip-${process.pid}-${++snapshots}.index`);
  const env = { GIT_INDEX_FILE: index };
  try {
    // Seeded from the real index, so add --all rehashes only what changed.
    if (fs.existsSync(path.join(gitDir, "index"))) fs.copyFileSync(path.join(gitDir, "index"), index);
    else await gitWith(env, dir, "read-tree", tip);
    const candidates = (await gitWith(env, dir, "ls-files", "-z", "--modified", "--others", "--exclude-standard")).split("\0").filter(Boolean);
    const large: string[] = [];
    for (const f of new Set(candidates)) {
      const st = fs.lstatSync(path.join(dir, f), { throwIfNoEntry: false });
      if (st?.isFile() && st.size > WIP_MAX_FILE_BYTES) large.push(f);
    }
    await gitWith(env, dir, "add", "--all", "--", ".", ...large.map((f) => `:(exclude,literal)${f}`));
    const tree = await gitWith(env, dir, "write-tree");
    if (!commitIt) return { tip, tree, large };
    if (tree === (await git(dir, "rev-parse", `${tip}^{tree}`))) {
      await git(dir, "update-ref", "-d", ref);
      return { tip, tree, large };
    }
    const who = { GIT_AUTHOR_NAME: "bmus", GIT_AUTHOR_EMAIL: "bmus@bmus.invalid", GIT_COMMITTER_NAME: "bmus", GIT_COMMITTER_EMAIL: "bmus@bmus.invalid" };
    const commit = await gitWith(who, dir, "commit-tree", tree, "-p", tip, "-m", `bmus: work in progress on bmus/${orderId}`);
    await git(dir, "update-ref", ref, commit);
    return { tip, tree, commit, large };
  } finally {
    fs.rmSync(index, { force: true });
    fs.rmSync(`${index}.lock`, { force: true });
  }
}

let snapshots = 0;

/** The lines a checkpoint's note gains for what its WIP snapshot left out (#145). */
export function snapshotNote(name: string, snap: Snapshot): string[] {
  const lines: string[] = [];
  if (snap.skipped !== undefined) lines.push(`Repo ${name}: no snapshot of uncommitted work: ${snap.skipped}.`);
  if (snap.large.length > 0) lines.push(`Repo ${name}: left out of the snapshot of uncommitted work, over 100 MiB each: ${snap.large.join(", ")}.`);
  return lines;
}

/**
 * Writes the WIP bundle of a checkpoint (#145): `refs/bmus/wip/<order>`
 * and `refs/bmus/kept-wip/<order>`, those that exist, minus `bmus/<order>`
 * at tip and the base commit, which the bridge has from the checkpoint's
 * own bundle. It is a file of its own, so a large WIP can't keep the
 * commits off the bridge. Returns undefined with neither ref.
 */
export async function bundleWip(checkout: RepoCheckout, orderId: string, tip: string, out: string): Promise<string | undefined> {
  const refs: string[] = [];
  for (const r of [wipRef(orderId), keptWipRef(orderId)]) {
    if (await git(checkout.dir, "rev-parse", "--verify", "--quiet", `${r}^{commit}`).catch(() => "")) refs.push(r);
  }
  if (refs.length === 0) return undefined;
  const exclude = [`^${tip}`];
  if (checkout.base_commit !== "" && (await git(checkout.dir, "cat-file", "-e", `${checkout.base_commit}^{commit}`).then(() => true, () => false))) {
    exclude.push(`^${checkout.base_commit}`);
  }
  fs.mkdirSync(path.dirname(out), { recursive: true });
  const tmp = `${out}.tmp`;
  try {
    await git(checkout.dir, "bundle", "create", "--quiet", tmp, ...refs, ...exclude);
    fs.renameSync(tmp, out);
  } catch (err) {
    fs.rmSync(tmp, { force: true });
    throw err;
  }
  return out;
}

/** What bundleWork puts in a checkpoint's bundle (#145). */
export interface BundleOptions {
  /**
   * The previous checkpoint's commit: the bundle holds that commit and what
   * came after it (it excludes the commit's parents, so it is never empty
   * when the branch still holds it), and a full bundle (as without `since`)
   * goes to `<out minus .bundle>.full.bundle` as the fallback. When git
   * can't write that bundle (the branch moved behind it), the bundle is the
   * full one alone.
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
    /** Commits of the branch past from, 0 when from is unknown. */
    const past = async (from: string) => Number(await git(checkout.dir, "rev-list", "--count", `${from}..${ref}`).catch(() => "0"));
    const full = (await past(checkout.base_commit)) > 0 ? [`^${checkout.base_commit}`] : [];
    const write = async (file: string, exclude: string[]) => {
      fs.mkdirSync(path.dirname(file), { recursive: true });
      const tmp = `${file}.tmp`;
      try {
        await git(checkout.dir, "bundle", "create", "--quiet", tmp, ref, ...exclude);
        fs.renameSync(tmp, file);
      } catch (err) {
        fs.rmSync(tmp, { force: true });
        throw err;
      }
    };
    let fullPath: string | undefined;
    const parents =
      opts.since === undefined ? [] : (await git(checkout.dir, "rev-parse", `${opts.since}^@`).catch(() => "")).split(/\s+/).filter(Boolean);
    if (parents.length > 0) {
      try {
        await write(out, parents.map((p) => `^${p}`));
        fullPath = out.replace(/\.bundle$/, "") + ".full.bundle";
        await write(fullPath, full);
      } catch {
        fullPath = undefined;
        await write(out, full);
      }
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
