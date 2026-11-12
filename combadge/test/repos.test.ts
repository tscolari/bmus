// checkoutRepos (#51): an order's bundles become checkouts on bmus/<order>.
import * as fs from "node:fs";
import * as path from "node:path";
import { afterEach, describe, expect, it } from "vitest";
import { execFileSync } from "node:child_process";
import { bundleWork, checkoutRepos } from "../src/repos.js";
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
