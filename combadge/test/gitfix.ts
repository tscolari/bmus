// Git fixtures: repos and order bundles shaped as the bridge ships them (#51).
import { execFileSync } from "node:child_process";
import * as fs from "node:fs";
import * as path from "node:path";
import type { OrderRepo } from "../src/protocol.js";

/** Runs git in cwd with a fixed identity, returning trimmed stdout. */
export function git(cwd: string, ...args: string[]): string {
  return execFileSync("git", ["-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false", ...args], {
    cwd,
    encoding: "utf8",
    stdio: ["ignore", "pipe", "pipe"],
  }).trim();
}

/**
 * A source repo `<dir>/src-<name>` with main at two commits, and a bundle of
 * branch bmus/<order> at main with its full history, at
 * `<dir>/in/<name>.bundle`, as the bridge's ship-repos step writes it.
 */
export function orderBundle(dir: string, order: string, name = "app"): { repo: OrderRepo; commit: string } {
  const src = path.join(dir, `src-${name}`);
  fs.mkdirSync(src, { recursive: true });
  git(src, "init", "-q", "-b", "main");
  fs.writeFileSync(path.join(src, "a.txt"), "a\n");
  git(src, "add", "a.txt");
  git(src, "commit", "-q", "-m", "one");
  fs.writeFileSync(path.join(src, "a.txt"), "aa\n");
  git(src, "commit", "-q", "-am", "two");
  const commit = git(src, "rev-parse", "main");
  git(src, "branch", `bmus/${order}`, "main");
  const bundle = path.join(dir, "in", `${name}.bundle`);
  fs.mkdirSync(path.dirname(bundle), { recursive: true });
  git(src, "bundle", "create", "--quiet", bundle, `refs/heads/bmus/${order}`);
  return { repo: { name, base_ref: "main", bundle_path: bundle }, commit };
}

/** A dir holding only a `git` symlink: a PATH with git and nothing else. */
export function gitOnlyPath(dir: string): string {
  const bin = path.join(dir, "git-only");
  fs.mkdirSync(bin, { recursive: true });
  const real = execFileSync("sh", ["-c", "command -v git"], { encoding: "utf8" }).trim();
  fs.symlinkSync(real, path.join(bin, "git"));
  return bin;
}
