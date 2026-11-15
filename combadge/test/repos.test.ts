// checkoutRepos (#51): an order's bundles become checkouts on bmus/<order>.
import * as fs from "node:fs";
import * as path from "node:path";
import { afterEach, describe, expect, it } from "vitest";
import { execFileSync } from "node:child_process";
import { bundleWork, checkoutRepos, previousCheckout, snapshotWip, wipNote, wipRef } from "../src/repos.js";
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

  it("snapshots tracked and untracked changes, not ignored ones, on the WIP ref, leaving the index and work tree as they were", async () => {
    const dir = tempDir();
    const c = await dirty(dir);
    const state = () => [git(c.dir, "status", "--porcelain", "--ignored"), git(c.dir, "ls-files", "--stage"), git(c.dir, "diff"), git(c.dir, "diff", "--cached")];
    const before = state();
    const tip = git(c.dir, "rev-parse", `bmus/${ORDER}`);
    const snap = await snapshotWip(c, ORDER);
    expect(state()).toEqual(before);
    expect(git(c.dir, "rev-parse", `bmus/${ORDER}`)).toBe(tip); // the branch takes no WIP commit
    expect(snap.tip).toBe(tip);
    expect(snap.commit).toBe(git(c.dir, "rev-parse", WIP));
    expect(git(c.dir, "rev-parse", `${WIP}^`)).toBe(tip);
    expect(git(c.dir, "show", `${WIP}:a.txt`)).toBe("unstaged");
    expect(git(c.dir, "show", `${WIP}:new.txt`)).toBe("untracked");
    expect(git(c.dir, "ls-tree", "--name-only", WIP)).not.toContain("debug.log");
    expect(fs.readdirSync(path.join(c.dir, ".git")).filter((f) => f.includes("bmus-wip"))).toEqual([]);
  });

  it("has no WIP ref with nothing uncommitted, and the bundle carries none", async () => {
    const dir = tempDir();
    const c = await dirty(dir);
    await snapshotWip(c, ORDER);
    git(c.dir, "add", "-A");
    git(c.dir, "commit", "-q", "-m", "all of it");
    fs.writeFileSync(path.join(c.dir, "more.log"), "still ignored\n");
    const snap = await snapshotWip(c, ORDER);
    expect(snap.commit).toBeUndefined();
    expect(() => git(c.dir, "rev-parse", "--verify", "--quiet", WIP)).toThrow();
    const b = await bundleWork(c, ORDER, path.join(dir, "cp", "app.bundle"), { wip: true });
    expect(git(dir, "bundle", "list-heads", b.bundle_path)).toBe(`${b.commit} refs/heads/bmus/${ORDER}`);
  });

  it("bundles a checkpoint from the previous one's commit, with the full bundle beside it, and only the full one without commits past it", async () => {
    const dir = tempDir();
    const c = await dirty(dir);
    const first = git(c.dir, "rev-parse", `bmus/${ORDER}`);
    git(c.dir, "commit", "-q", "-m", "staged a");
    await snapshotWip(c, ORDER);
    const b = await bundleWork(c, ORDER, path.join(dir, "cp2", "app.bundle"), { wip: true, since: first });
    expect(b.full_bundle_path).toBe(path.join(dir, "cp2", "app.full.bundle"));
    expect(git(dir, "bundle", "list-heads", b.bundle_path)).toContain(WIP);
    // The incremental one needs the previous commit; the full one only the base.
    expect(execFileSync("git", ["bundle", "verify", b.bundle_path], { cwd: c.dir, encoding: "utf8", stdio: ["ignore", "pipe", "pipe"] })).toBeDefined();
    const header = (f: string) => fs.readFileSync(f, "latin1").split("\n\n")[0]!;
    expect(header(b.bundle_path)).toContain(`-${first}`);
    expect(header(b.full_bundle_path!)).toContain(`-${c.base_commit}`);
    // Nothing committed since: just the full bundle.
    const again = await bundleWork(c, ORDER, path.join(dir, "cp3", "app.bundle"), { wip: true, since: b.commit });
    expect(again.full_bundle_path).toBeUndefined();
    expect(header(again.bundle_path)).toContain(`-${c.base_commit}`);
  });

  it("restores the shipped bundle's WIP on its tip with nothing staged, and says so in the note", async () => {
    const dir = tempDir();
    const c = await dirty(dir);
    await snapshotWip(c, ORDER);
    // The bridge ships bmus/<order> and the WIP ref with their history.
    const shipped = path.join(dir, "ship", "in", "app.bundle");
    fs.mkdirSync(path.dirname(shipped), { recursive: true });
    git(c.dir, "bundle", "create", "--quiet", shipped, `refs/heads/bmus/${ORDER}`, WIP);
    const [got] = await checkoutRepos(path.join(dir, "ship"), ORDER, [{ name: "app", base_ref: "main", bundle_path: shipped }]);
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

  it("keeps a WIP on another commit as the ref only, and a WIP over a dirty work tree too", async () => {
    const dir = tempDir();
    const c = await dirty(dir);
    await snapshotWip(c, ORDER);
    const wip = git(c.dir, "rev-parse", WIP);
    git(c.dir, "commit", "-q", "-m", "moved on"); // the tip moves past the WIP's parent
    const shipped = path.join(dir, "ship", "in", "app.bundle");
    fs.mkdirSync(path.dirname(shipped), { recursive: true });
    git(c.dir, "bundle", "create", "--quiet", shipped, `refs/heads/bmus/${ORDER}`, WIP);
    const [got] = await checkoutRepos(path.join(dir, "ship"), ORDER, [{ name: "app", base_ref: "main", bundle_path: shipped }]);
    expect(got!.wip).toMatchObject({ commit: wip, restored: false });
    expect(got!.wip!.kept).toMatch(/^it was snapshotted on [0-9a-f]{12}, not on bmus\/ORD-0007's tip [0-9a-f]{12}$/);
    expect(git(got!.dir, "status", "--porcelain")).toBe("");
    expect(git(got!.dir, "rev-parse", WIP)).toBe(wip);
    expect(wipNote(ORDER, [got!])[0]).toContain("holds uncommitted work that was not restored: it was snapshotted on");

    // A kept checkout (a re-delivery) whose work tree has changes of its own keeps them.
    git(c.dir, "reset", "-q", "--soft", "HEAD~1");
    await snapshotWip(c, ORDER);
    const [kept] = await checkoutRepos(path.join(dir, "work"), ORDER, [{ name: "app", base_ref: "main", bundle_path: path.join(dir, "none.bundle") }]);
    expect(kept!.wip).toMatchObject({ from: "checkout", restored: false, kept: "the work tree already had uncommitted changes" });
  });

  it("prefers the existing checkout's WIP, set aside by a stop, over the bundle's", async () => {
    const dir = tempDir();
    const c = await dirty(dir);
    await snapshotWip(c, ORDER);
    const shipped = path.join(dir, "work", ORDER, "in", "app.bundle");
    fs.mkdirSync(path.dirname(shipped), { recursive: true });
    git(c.dir, "bundle", "create", "--quiet", shipped, `refs/heads/bmus/${ORDER}`, WIP);
    // Later work on this beam, snapshotted after the bundle the bridge has.
    fs.writeFileSync(path.join(c.dir, "later.txt"), "later\n");
    await snapshotWip(c, ORDER);
    const local = git(c.dir, "rev-parse", WIP);
    fs.renameSync(c.dir, previousCheckout(c.dir)); // as serve's clearStopped does
    const [got] = await checkoutRepos(path.join(dir, "work"), ORDER, [{ name: "app", base_ref: "main", bundle_path: shipped }]);
    expect(got!.wip).toMatchObject({ commit: local, from: "checkout", restored: true });
    expect(fs.readFileSync(path.join(got!.dir, "later.txt"), "utf8")).toBe("later\n");
    expect(fs.existsSync(previousCheckout(c.dir))).toBe(false);
    expect(wipNote(ORDER, [got!])[0]).toContain("from the order's existing checkout on this beam");
  });
});
