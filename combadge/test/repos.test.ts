// checkoutRepos (#51): an order's bundles become checkouts on bmus/<order>.
import * as fs from "node:fs";
import * as path from "node:path";
import { afterEach, describe, expect, it } from "vitest";
import { execFileSync } from "node:child_process";
import { bundleCheckpoint, workKey } from "../src/harness/common.js";
import { bundleWork, checkoutRepos, keptWipRef, previousCheckout, snapshotNote, snapshotWip, wipNote, wipRef, WIP_MAX_FILE_BYTES, type RepoCheckout } from "../src/repos.js";
import { git, orderBundle } from "./gitfix.js";
import { cleanup, tempDir } from "./procs.js";

afterEach(cleanup);

const ORDER = "ORD-0007";

describe("checkoutRepos", () => {
  it("clones each bundle into <work-dir>/<order>/<name> on bmus/<order> at base_ref's commit", async () => {
    const dir = tempDir();
    const a = orderBundle(dir, ORDER, "app");
    const b = orderBundle(dir, ORDER, "lib");
    const work = path.join(dir, "work");
    const got = await checkoutRepos(work, ORDER, [a.repo, b.repo]);
    expect(got).toEqual([
      { name: "app", dir: path.join(work, ORDER, "app"), base_ref: "main", base_commit: a.commit },
      { name: "lib", dir: path.join(work, ORDER, "lib"), base_ref: "main", base_commit: b.commit },
    ]);
    for (const c of got) {
      expect(git(c.dir, "rev-parse", "--abbrev-ref", "HEAD")).toBe(`bmus/${ORDER}`);
      expect(git(c.dir, "rev-parse", "HEAD")).toBe(c.base_commit);
      expect(git(c.dir, "rev-list", "--count", "HEAD")).toBe("2"); // full history
      expect(git(c.dir, "status", "--porcelain")).toBe("");
    }
    expect(fs.readdirSync(path.join(work, ORDER)).sort()).toEqual(["app", "lib"]);
    expect(fs.existsSync(a.repo.bundle_path)).toBe(false); // removed once cloned
  });

  it("keeps a checkout that is already there, work included, and redoes a clone cut short", async () => {
    const dir = tempDir();
    const { repo, commit } = orderBundle(dir, ORDER);
    const work = path.join(dir, "work");
    const [first] = await checkoutRepos(work, ORDER, [repo]);
    fs.writeFileSync(path.join(first!.dir, "b.txt"), "b\n");
    git(first!.dir, "add", "b.txt");
    git(first!.dir, "commit", "-q", "-m", "work");
    const head = git(first!.dir, "rev-parse", "HEAD");
    git(first!.dir, "checkout", "-q", "--detach");

    // Delivered again: the same checkout, back on its branch, with its commit.
    const [again] = await checkoutRepos(work, ORDER, [repo]);
    expect(again).toEqual({ ...first, base_commit: commit });
    expect(git(again!.dir, "rev-parse", "--abbrev-ref", "HEAD")).toBe(`bmus/${ORDER}`);
    expect(git(again!.dir, "rev-parse", "HEAD")).toBe(head);

    // A crash mid-clone leaves only the temporary dir (and the bundle); the next run replaces it.
    fs.rmSync(again!.dir, { recursive: true });
    fs.rmSync(path.join(dir, "src-app"), { recursive: true });
    const rebuilt = orderBundle(dir, ORDER); // new commits: fresh timestamps, new hashes
    const tmp = path.join(work, ORDER, ".app.clone");
    fs.mkdirSync(tmp);
    fs.writeFileSync(path.join(tmp, "junk"), "x");
    const [redone] = await checkoutRepos(work, ORDER, [repo]);
    expect(git(redone!.dir, "rev-parse", "HEAD")).toBe(rebuilt.commit);
    expect(fs.existsSync(tmp)).toBe(false);
  });

  it("ignores git variables that point at another repo", async () => {
    const dir = tempDir();
    const { repo, commit } = orderBundle(dir, ORDER);
    const decoy = orderBundle(path.join(dir, "decoy"), ORDER).repo.bundle_path.replace(/in\/app\.bundle$/, "src-app");
    const state = () => [git(decoy, "for-each-ref"), git(decoy, "config", "--list", "--local"), git(decoy, "status", "--porcelain")].join("\n");
    const before = state();
    const vars = { GIT_DIR: path.join(decoy, ".git"), GIT_WORK_TREE: decoy, GIT_INDEX_FILE: path.join(decoy, ".git", "index") };
    const saved = Object.keys(vars).map((k) => [k, process.env[k]] as const);
    Object.assign(process.env, vars);
    let got;
    try {
      got = await checkoutRepos(path.join(dir, "work"), ORDER, [repo]);
    } finally {
      for (const [k, v] of saved) if (v === undefined) delete process.env[k]; else process.env[k] = v;
    }
    expect(git(got[0]!.dir, "rev-parse", "HEAD")).toBe(commit);
    expect(git(got[0]!.dir, "rev-parse", "--abbrev-ref", "HEAD")).toBe(`bmus/${ORDER}`);
    expect(state()).toBe(before);
  });

  it("refuses names that leave the work dir and names a repo whose bundle fails", async () => {
    const dir = tempDir();
    const { repo } = orderBundle(dir, ORDER);
    const work = path.join(dir, "work");
    await expect(checkoutRepos(work, "..", [repo])).rejects.toThrow("is not usable as a directory name");
    await expect(checkoutRepos(work, ORDER, [{ ...repo, name: ".." }])).rejects.toThrow("is not usable as a directory name");
    await expect(checkoutRepos(work, ORDER, [{ ...repo, name: "a/b" }])).rejects.toThrow("is not usable as a directory name");
    await expect(checkoutRepos(work, ORDER, [repo, repo])).rejects.toThrow("repo app is listed twice");
    for (const name of ["in", "out", "1", "02"]) {
      await expect(checkoutRepos(work, ORDER, [{ ...repo, name }])).rejects.toThrow("is reserved for the order's own files");
    }
    await expect(checkoutRepos(work, ORDER, [{ ...repo, bundle_path: path.join(dir, "missing.bundle") }])).rejects.toThrow(
      /^checking out repo app: /,
    );
    // A bundle without the order's branch.
    await expect(checkoutRepos(work, "ORD-0008", [repo])).rejects.toThrow(/checking out repo app: .*bmus\/ORD-0008/s);
    expect(fs.existsSync(path.join(work, ORDER, "app"))).toBe(false);
    expect(await checkoutRepos(work, ORDER, [])).toEqual([]);
  });
});

describe("bundleWork", () => {
  it("reports the commit the bundle holds, even when the branch moves while it is written", async () => {
    const dir = tempDir();
    const { repo, commit: base } = orderBundle(dir, ORDER);
    const [checkout] = await checkoutRepos(path.join(dir, "work"), ORDER, [repo]);
    fs.writeFileSync(path.join(checkout!.dir, "b.txt"), "b\n");
    git(checkout!.dir, "add", "b.txt");
    git(checkout!.dir, "commit", "-q", "-m", "work");
    const work = git(checkout!.dir, "rev-parse", "HEAD");
    git(checkout!.dir, "commit", "-q", "--allow-empty", "-m", "later");
    const later = git(checkout!.dir, "rev-parse", "HEAD");
    git(checkout!.dir, "update-ref", `refs/heads/bmus/${ORDER}`, work);

    // A git that moves the branch to `later` just before writing the bundle,
    // as an agent committing meanwhile would.
    const bin = path.join(dir, "bin");
    fs.mkdirSync(bin);
    const real = execFileSync("sh", ["-c", "command -v git"], { encoding: "utf8" }).trim();
    fs.writeFileSync(path.join(bin, "git"),
      `#!/bin/sh\nif [ "$1" = bundle ] && [ "$2" = create ]; then "${real}" update-ref refs/heads/bmus/${ORDER} ${later}; fi\nexec "${real}" "$@"\n`,
      { mode: 0o755 });
    const saved = process.env.PATH;
    process.env.PATH = `${bin}${path.delimiter}${saved ?? ""}`;
    let got;
    try {
      got = await bundleWork(checkout!, ORDER, path.join(dir, "out", "app.bundle"));
    } finally {
      process.env.PATH = saved;
    }
    expect(got).toEqual({ name: "app", bundle_path: path.join(dir, "out", "app.bundle"), commit: later, branch: `bmus/${ORDER}` });
    expect(git(dir, "bundle", "list-heads", got.bundle_path)).toBe(`${later} refs/heads/bmus/${ORDER}`);
    expect(base).not.toBe(later);
  });
});

describe("work in progress (#145)", () => {
  const WIP = wipRef(ORDER);
  const KEPT = keptWipRef(ORDER);
  const order = (repos: RepoCheckout[]) => ({ id: ORDER, body: { title: "t", body: "b", repos: [], budget_tokens: 0 }, repos });
  const refExists = (dir: string, ref: string) => {
    try {
      git(dir, "rev-parse", "--verify", "--quiet", ref);
      return true;
    } catch {
      return false;
    }
  };
  /** Ships what a checkpoint holds as the bridge does: one bundle of bmus/<order> and the WIP refs, with their history. */
  function ship(from: string, out: string, refs: string[] = [WIP]) {
    fs.mkdirSync(path.dirname(out), { recursive: true });
    git(from, "bundle", "create", "--quiet", out, `refs/heads/bmus/${ORDER}`, ...refs.filter((r) => refExists(from, r)));
    return { name: "app", base_ref: "main", bundle_path: out };
  }

  /** A checkout of a fresh order bundle with uncommitted work: a staged edit, an unstaged one, an untracked and an ignored file. */
  async function dirty(dir: string) {
    const { repo } = orderBundle(dir, ORDER);
    const [c] = await checkoutRepos(path.join(dir, "work"), ORDER, [repo]);
    const d = c!.dir;
    fs.writeFileSync(path.join(d, ".gitignore"), "*.log\n");
    git(d, "add", ".gitignore");
    git(d, "commit", "-q", "-m", "ignore logs");
    fs.writeFileSync(path.join(d, "a.txt"), "staged\n");
    git(d, "add", "a.txt");
    fs.writeFileSync(path.join(d, "a.txt"), "unstaged\n");
    fs.writeFileSync(path.join(d, "new.txt"), "untracked\n");
    fs.writeFileSync(path.join(d, "debug.log"), "ignored\n");
    return c!;
  }

  it("snapshots tracked and untracked changes, not ignored or huge ones, on the WIP ref, leaving the index and work tree as they were", async () => {
    const dir = tempDir();
    const c = await dirty(dir);
    fs.closeSync(fs.openSync(path.join(c.dir, "huge.bin"), "w"));
    fs.truncateSync(path.join(c.dir, "huge.bin"), WIP_MAX_FILE_BYTES + 1); // sparse: cheap to make
    const state = () => [git(c.dir, "status", "--porcelain", "--ignored"), git(c.dir, "ls-files", "--stage"), git(c.dir, "diff"), git(c.dir, "diff", "--cached")];
    const before = state();
    const tip = git(c.dir, "rev-parse", `bmus/${ORDER}`);
    // Without commit, it moves no ref.
    const read = await snapshotWip(c, ORDER, { commit: false });
    expect(read.commit).toBeUndefined();
    expect(refExists(c.dir, WIP)).toBe(false);
    const snap = await snapshotWip(c, ORDER);
    expect(state()).toEqual(before);
    expect(git(c.dir, "rev-parse", `bmus/${ORDER}`)).toBe(tip); // the branch takes no WIP commit
    expect(snap).toMatchObject({ tip, tree: read.tree, large: ["huge.bin"] });
    expect(snap.commit).toBe(git(c.dir, "rev-parse", WIP));
    expect(git(c.dir, "rev-parse", `${WIP}^`)).toBe(tip);
    expect(git(c.dir, "show", `${WIP}:a.txt`)).toBe("unstaged");
    expect(git(c.dir, "show", `${WIP}:new.txt`)).toBe("untracked");
    const files = git(c.dir, "ls-tree", "--name-only", WIP);
    expect(files).not.toContain("debug.log");
    expect(files).not.toContain("huge.bin");
    expect(snapshotNote("app", snap)).toEqual(["Repo app: left out of the snapshot of uncommitted work, over 100 MiB each: huge.bin."]);
    expect(fs.readdirSync(path.join(c.dir, ".git")).filter((f) => f.includes("bmus-wip"))).toEqual([]);
  });

  it("takes no WIP with HEAD off bmus/<order>, and says why", async () => {
    const dir = tempDir();
    const c = await dirty(dir);
    await snapshotWip(c, ORDER);
    git(c.dir, "stash", "-q");
    git(c.dir, "checkout", "-q", "-b", "other");
    git(c.dir, "stash", "pop", "-q");
    const snap = await snapshotWip(c, ORDER);
    expect(snap.skipped).toBe(`HEAD is other, not bmus/${ORDER}`);
    expect(refExists(c.dir, WIP)).toBe(false);
    expect(snapshotNote("app", snap)).toEqual([`Repo app: no snapshot of uncommitted work: HEAD is other, not bmus/${ORDER}.`]);
    git(c.dir, "checkout", "-q", "--detach");
    expect((await snapshotWip(c, ORDER)).skipped).toBe(`HEAD is detached, not bmus/${ORDER}`);
  });

  it("has no WIP ref and no WIP bundle with nothing uncommitted", async () => {
    const dir = tempDir();
    const c = await dirty(dir);
    await snapshotWip(c, ORDER);
    git(c.dir, "add", "-A");
    git(c.dir, "commit", "-q", "-m", "all of it");
    fs.writeFileSync(path.join(c.dir, "more.log"), "still ignored\n");
    const { repos } = await bundleCheckpoint(path.join(dir, "cp"), order([c]));
    expect(refExists(c.dir, WIP)).toBe(false);
    expect(repos[0]!.wip_bundle_path).toBeUndefined();
    expect(git(dir, "bundle", "list-heads", repos[0]!.bundle_path)).toBe(`${repos[0]!.commit} refs/heads/bmus/${ORDER}`);
  });

  it("bundles the WIP on its own, and a checkpoint from the previous one's commit, the full bundle beside it, also when only the WIP changed", async () => {
    const dir = tempDir();
    const c = await dirty(dir);
    const header = (f: string) => fs.readFileSync(f, "latin1").split("\n\n")[0]!;
    const first = await bundleCheckpoint(path.join(dir, "cp1"), order([c]));
    const r1 = first.repos[0]!;
    expect(r1.full_bundle_path).toBeUndefined();
    expect(git(dir, "bundle", "list-heads", r1.bundle_path)).toBe(`${r1.commit} refs/heads/bmus/${ORDER}`);
    expect(git(dir, "bundle", "list-heads", r1.wip_bundle_path!)).toBe(`${git(c.dir, "rev-parse", WIP)} ${WIP}`);
    expect(header(r1.wip_bundle_path!)).toContain(`-${r1.commit}`);
    // Only the work tree changed: the bundle holds the previous commit alone.
    fs.writeFileSync(path.join(c.dir, "more.txt"), "more\n");
    const second = await bundleCheckpoint(path.join(dir, "cp2"), order([c]), { app: r1.commit });
    const r2 = second.repos[0]!;
    expect(r2.commit).toBe(r1.commit);
    expect(r2.full_bundle_path).toBe(path.join(dir, "cp2", "app.full.bundle"));
    expect(header(r2.bundle_path)).toContain(`-${git(c.dir, "rev-parse", `${r1.commit}^`)}`);
    expect(header(r2.full_bundle_path!)).toContain(`-${c.base_commit}`);
    expect(second.key).not.toBe(first.key);
    // A commit since: it starts at the previous one.
    git(c.dir, "commit", "-q", "-m", "staged a");
    const r3 = (await bundleCheckpoint(path.join(dir, "cp3"), order([c]), { app: r2.commit })).repos[0]!;
    expect(git(c.dir, "rev-list", "--count", `${r2.commit}..${r3.commit}`)).toBe("1");
    expect(header(r3.bundle_path)).toContain(`-${git(c.dir, "rev-parse", `${r2.commit}^`)}`);
  });

  it("restores the shipped bundle's WIP on its tip with nothing staged, and says so in the note", async () => {
    const dir = tempDir();
    const c = await dirty(dir);
    await snapshotWip(c, ORDER);
    const [got] = await checkoutRepos(path.join(dir, "ship"), ORDER, [ship(c.dir, path.join(dir, "ship", "in", "app.bundle"))]);
    expect(got!.wip).toMatchObject({ from: "bundle", restored: true, files: ["a.txt", "new.txt"] });
    expect(fs.readFileSync(path.join(got!.dir, "a.txt"), "utf8")).toBe("unstaged\n");
    expect(fs.readFileSync(path.join(got!.dir, "new.txt"), "utf8")).toBe("untracked\n");
    expect(git(got!.dir, "diff", "--cached", "--name-only")).toBe(""); // nothing staged
    expect(git(got!.dir, "status", "--porcelain")).toBe("M a.txt\n?? new.txt");
    expect(git(got!.dir, "rev-parse", WIP)).toBe(git(c.dir, "rev-parse", WIP));
    const note = wipNote(ORDER, [got!]);
    expect(note).toHaveLength(1);
    expect(note[0]).toMatch(/^Uncommitted work was restored in .* from the shipped checkpoint\), unstaged; files: a\.txt, new\.txt\./);
  });

  it("keeps a WIP on another commit on the kept ref, which outlives the next snapshots and travels with later checkpoints", async () => {
    const dir = tempDir();
    const c = await dirty(dir);
    await snapshotWip(c, ORDER);
    const wip = git(c.dir, "rev-parse", WIP);
    git(c.dir, "commit", "-q", "-m", "moved on"); // the tip moves past the WIP's parent
    const [got] = await checkoutRepos(path.join(dir, "ship"), ORDER, [ship(c.dir, path.join(dir, "ship", "in", "app.bundle"))]);
    expect(got!.wip).toMatchObject({ commit: wip, restored: false });
    expect(got!.wip!.kept).toMatch(/^it was snapshotted on [0-9a-f]{12}, not on bmus\/ORD-0007's tip [0-9a-f]{12}$/);
    expect(git(got!.dir, "status", "--porcelain")).toBe("");
    expect(git(got!.dir, "rev-parse", KEPT)).toBe(wip);
    expect(refExists(got!.dir, WIP)).toBe(false);
    expect(wipNote(ORDER, [got!])[0]).toContain(`${KEPT} in `);
    expect(wipNote(ORDER, [got!])[0]).toContain("holds uncommitted work that was not restored: it was snapshotted on");
    // The run's first snapshot (the cadence's baseline, then a checkpoint) leaves it alone, and the checkpoint carries it.
    await workKey(order([got!]));
    fs.writeFileSync(path.join(got!.dir, "fresh.txt"), "fresh\n");
    const { repos } = await bundleCheckpoint(path.join(dir, "cp"), order([got!]));
    expect(git(got!.dir, "rev-parse", KEPT)).toBe(wip);
    const heads = git(dir, "bundle", "list-heads", repos[0]!.wip_bundle_path!);
    expect(heads).toContain(`${wip} ${KEPT}`);
    expect(heads).toContain(WIP);
    // And a later start gets it back from the shipped bundle.
    const [again] = await checkoutRepos(path.join(dir, "again"), ORDER, [ship(got!.dir, path.join(dir, "again", "in", "app.bundle"), [WIP, KEPT])]);
    expect(git(again!.dir, "rev-parse", KEPT)).toBe(wip);
  });

  it("takes the set-aside checkout's work only when the shipped checkpoint is the last one this beam emitted", async () => {
    const dir = tempDir();
    const c = await dirty(dir);
    // The beam emits a checkpoint; the bridge ships that one back.
    const cp = (await bundleCheckpoint(path.join(dir, "cp"), order([c]))).repos[0]!;
    const shipped = path.join(dir, "work", ORDER, "in", "app.bundle");
    fs.mkdirSync(path.dirname(shipped), { recursive: true });
    git(c.dir, "bundle", "create", "--quiet", shipped, `refs/heads/bmus/${ORDER}`, WIP);
    // Later work on this beam, after the checkpoint.
    fs.writeFileSync(path.join(c.dir, "later.txt"), "later\n");
    fs.renameSync(c.dir, previousCheckout(c.dir)); // as serve's clearStopped does
    const [got] = await checkoutRepos(path.join(dir, "work"), ORDER, [{ name: "app", base_ref: "main", bundle_path: shipped }]);
    expect(got!.wip).toMatchObject({ from: "checkout", restored: true });
    expect(got!.wip!.commit).not.toBe(git(got!.dir, "rev-parse", `${cp.commit}`)); // a fresh snapshot
    expect(fs.readFileSync(path.join(got!.dir, "later.txt"), "utf8")).toBe("later\n");
    expect(fs.existsSync(previousCheckout(c.dir))).toBe(false);
    expect(wipNote(ORDER, [got!])[0]).toContain("from the order's existing checkout on this beam");
  });

  it("takes the shipped WIP over a stale set-aside checkout's on the same tip", async () => {
    const dir = tempDir();
    const c = await dirty(dir);
    await bundleCheckpoint(path.join(dir, "cp"), order([c])); // this beam emitted WIP1
    // Elsewhere the order made WIP2 on the same tip, and the bridge ships it.
    const other = path.join(dir, "other");
    git(dir, "clone", "-q", "--branch", `bmus/${ORDER}`, c.dir, other);
    fs.writeFileSync(path.join(other, "elsewhere.txt"), "elsewhere\n");
    const wip2 = (await snapshotWip({ name: "app", dir: other, base_ref: "main", base_commit: c.base_commit }, ORDER)).commit!;
    const shipped = path.join(dir, "work", ORDER, "in", "app.bundle");
    fs.mkdirSync(path.dirname(shipped), { recursive: true });
    git(other, "bundle", "create", "--quiet", shipped, `refs/heads/bmus/${ORDER}`, WIP);
    fs.renameSync(c.dir, previousCheckout(c.dir));
    const [got] = await checkoutRepos(path.join(dir, "work"), ORDER, [{ name: "app", base_ref: "main", bundle_path: shipped }]);
    expect(got!.wip).toMatchObject({ commit: wip2, from: "bundle", restored: true, files: ["elsewhere.txt"] });
    expect(fs.existsSync(path.join(got!.dir, "new.txt"))).toBe(false);
    // This beam's own differing work is kept on the side, and named.
    const kept = git(got!.dir, "rev-parse", KEPT);
    expect(git(got!.dir, "show", `${kept}:new.txt`)).toBe("untracked");
    const note = wipNote(ORDER, [got!]).join("\n");
    expect(note).toContain(`this beam's own uncommitted work (${kept.slice(0, 12)}) differs from the shipped checkpoint's`);
    expect(note).toContain(`it is kept as ${KEPT} in ${got!.dir}`);
  });

  it("restores nothing when this beam's agent dropped the change the shipped checkpoint holds", async () => {
    const dir = tempDir();
    const c = await dirty(dir);
    await bundleCheckpoint(path.join(dir, "cp"), order([c])); // emitted with a WIP
    const shipped = path.join(dir, "work", ORDER, "in", "app.bundle");
    fs.mkdirSync(path.dirname(shipped), { recursive: true });
    git(c.dir, "bundle", "create", "--quiet", shipped, `refs/heads/bmus/${ORDER}`, WIP);
    git(c.dir, "checkout", "-q", "--", ".");
    git(c.dir, "reset", "-q", "--hard");
    git(c.dir, "clean", "-qfd");
    fs.renameSync(c.dir, previousCheckout(c.dir));
    const [got] = await checkoutRepos(path.join(dir, "work"), ORDER, [{ name: "app", base_ref: "main", bundle_path: shipped }]);
    expect(got!.wip).toBeUndefined();
    expect(git(got!.dir, "status", "--porcelain")).toBe("");
    expect(refExists(got!.dir, WIP)).toBe(false);
  });

  it("names a kept WIP on every start, and names one a newer kept WIP replaces", async () => {
    const dir = tempDir();
    const c = await dirty(dir);
    await snapshotWip(c, ORDER);
    const first = git(c.dir, "rev-parse", WIP);
    git(c.dir, "commit", "-q", "-m", "moved on");
    const [a] = await checkoutRepos(path.join(dir, "a"), ORDER, [ship(c.dir, path.join(dir, "a", "in", "app.bundle"))]);
    expect(git(a!.dir, "rev-parse", KEPT)).toBe(first);
    // The next start, with nothing new: the kept WIP is named again.
    const [b] = await checkoutRepos(path.join(dir, "b"), ORDER, [ship(a!.dir, path.join(dir, "b", "in", "app.bundle"), [WIP, KEPT])]);
    expect(wipNote(ORDER, [b!])).toEqual([`${KEPT} in ${b!.dir} (${first.slice(0, 12)}) holds uncommitted work kept from an earlier run; see it with git diff ${first.slice(0, 12)}^ ${KEPT}.`]);
    // A newer WIP that can't be restored replaces it, and the note says so.
    fs.writeFileSync(path.join(b!.dir, "second.txt"), "second\n");
    await snapshotWip(b!, ORDER);
    const second = git(b!.dir, "rev-parse", WIP);
    git(b!.dir, "commit", "-q", "--allow-empty", "-m", "moved on again");
    const [d] = await checkoutRepos(path.join(dir, "d"), ORDER, [ship(b!.dir, path.join(dir, "d", "in", "app.bundle"), [WIP, KEPT])]);
    expect(git(d!.dir, "rev-parse", KEPT)).toBe(second);
    expect(wipNote(ORDER, [d!]).join("\n")).toContain(`held ${first.slice(0, 12)}, uncommitted work kept from earlier; it now holds ${second.slice(0, 12)}`);
  });

  it("removes a set-aside checkout when the checkout fails", async () => {
    const dir = tempDir();
    const c = await dirty(dir);
    fs.renameSync(c.dir, previousCheckout(c.dir));
    await expect(checkoutRepos(path.join(dir, "work"), ORDER, [{ name: "app", base_ref: "main", bundle_path: path.join(dir, "gone.bundle") }])).rejects.toThrow();
    expect(fs.existsSync(previousCheckout(c.dir))).toBe(false);
  });
});
