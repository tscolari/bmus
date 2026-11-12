// Builds the console frontend into ../internal/console/assets, which the Go
// binary embeds and which is committed (console plan, "Frontend"). CI
// rebuilds and fails on any difference, so the output must be
// deterministic: no content hashes in file names, no timestamps, no source
// maps carrying absolute paths.
//
// Output: index.html (copied), app.js and app.css (bundled from
// src/main.tsx and the CSS it imports), fonts/*.woff2 (the latin subsets
// from the @fontsource packages, which esbuild copies for the url()s in
// app.css), each font's license next to them, and preact-LICENSE.txt
// (legalComments is "none", so the bundle carries no notice of its own). Everything in the output
// directory but .gitkeep is removed first, so a dropped file doesn't linger.
import { build } from "esbuild";
import { copyFile, mkdir, readdir, rm } from "node:fs/promises";
import { fileURLToPath } from "node:url";
import path from "node:path";

const root = fileURLToPath(new URL(".", import.meta.url));
const outdir = path.join(root, "..", "internal", "console", "assets");

// Font packages whose license is vendored as fonts/<package>-LICENSE.txt.
const fontPackages = ["archivo-narrow", "space-mono", "syncopate"];

/** Removes everything in dir but .gitkeep. */
async function clean(dir) {
  await mkdir(dir, { recursive: true });
  for (const name of await readdir(dir)) {
    if (name !== ".gitkeep") await rm(path.join(dir, name), { recursive: true, force: true });
  }
}

/** Builds the console frontend into outdir. */
export async function buildConsole() {
  await clean(outdir);
  await build({
    absWorkingDir: root,
    entryPoints: { app: "src/main.tsx" },
    outdir,
    bundle: true,
    minify: true,
    format: "esm",
    target: "es2022",
    jsx: "automatic",
    jsxImportSource: "preact",
    loader: { ".woff2": "file" },
    assetNames: "fonts/[name]",
    legalComments: "none",
    logLevel: "warning",
  });
  await copyFile(path.join(root, "index.html"), path.join(outdir, "index.html"));
  await copyFile(path.join(root, "node_modules", "preact", "LICENSE"), path.join(outdir, "preact-LICENSE.txt"));
  for (const pkg of fontPackages) {
    await copyFile(
      path.join(root, "node_modules", "@fontsource", pkg, "LICENSE"),
      path.join(outdir, "fonts", `${pkg}-LICENSE.txt`),
    );
  }
}

if (process.argv[1] === fileURLToPath(import.meta.url)) {
  await buildConsole();
}
