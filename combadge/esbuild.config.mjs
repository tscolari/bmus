// Bundles src/main.ts into dist/combadge.mjs, one file runnable with plain
// `node` and no node_modules (plan §5). The process-level tests use
// buildCombadge to bundle into a temp dir.
//
// ESM rather than the plan's CommonJS: the .mjs extension makes Node load it
// as ESM wherever it is copied, without relying on package.json or syntax
// detection. The banner gives bundled CommonJS dependencies a require for
// Node builtins.
//
// Nothing is external: every dependency is inlined, the Agent SDK included
// (about 1.3 MiB of the bundle). Its native `claude` binary is the one thing
// a bundle can't carry, so the sdk harness points pathToClaudeCodeExecutable
// at the beam's own `claude`; the SDK only looks for its binary at query()
// time, not at load.
//
// The output is deterministic: esbuild writes no timestamps, and source
// comments name paths relative to this directory, not the absolute checkout.
import { build } from "esbuild";
import { fileURLToPath } from "node:url";

const root = fileURLToPath(new URL(".", import.meta.url));

/** Bundles combadge into outfile; returns esbuild's metafile. */
export async function buildCombadge(outfile) {
  const result = await build({
    absWorkingDir: root,
    entryPoints: ["src/main.ts"],
    outfile,
    bundle: true,
    platform: "node",
    target: "node22",
    format: "esm",
    banner: { js: 'import { createRequire } from "node:module"; const require = createRequire(import.meta.url);' },
    metafile: true,
    logLevel: "warning",
  });
  return result.metafile;
}

/** Prints the bundle's size and its largest inputs. */
function report(metafile) {
  const kb = (n) => `${(n / 1024).toFixed(1)} KiB`;
  for (const [file, out] of Object.entries(metafile.outputs)) {
    console.log(`${file}  ${kb(out.bytes)}`);
    const inputs = Object.entries(out.inputs).sort((a, b) => b[1].bytesInOutput - a[1].bytesInOutput);
    for (const [input, { bytesInOutput }] of inputs.slice(0, 5)) console.log(`  ${kb(bytesInOutput).padStart(10)}  ${input}`);
  }
}

if (process.argv[1] === fileURLToPath(import.meta.url)) {
  report(await buildCombadge("dist/combadge.mjs"));
}
