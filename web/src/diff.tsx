// The diff page (#104): a done order's work, opened from its mission record
// row at /diff/<order> in a new tab. It reads GET /api/orders/{id}/diff
// and shows each repo's unified diff, a section per repo and a header per
// file, added lines green and removed ones red. The diff is an agent's
// work: it is rendered as plain text children only, never as HTML.

import { useEffect, useState } from "preact/hooks";
import { orderDiff, type OrderDiff, type RepoDiff } from "./api.js";

/**
 * The most diff lines a repo's section renders: an agent can write a
 * million, and each is an element. The rest stays in Copy.
 */
export const maxRenderedLines = 50_000;

/** n bytes for a note: whole MiB or KiB where it divides, else bytes. */
export function formatBytes(n: number): string {
  if (n >= 1 << 20 && n % (1 << 20) === 0) return `${n / (1 << 20)} MiB`;
  if (n >= 1 << 10 && n % (1 << 10) === 0) return `${n / (1 << 10)} KiB`;
  return `${n} bytes`;
}

/** files cut to at most max lines in all, and how many lines were left out. */
export function capLines(files: readonly DiffFile[], max: number): { files: DiffFile[]; omitted: number } {
  const out: DiffFile[] = [];
  let left = max;
  let omitted = 0;
  for (const f of files) {
    if (left <= 0) {
      omitted += f.lines.length;
      continue;
    }
    const lines = f.lines.length > left ? f.lines.slice(0, left) : f.lines;
    omitted += f.lines.length - lines.length;
    left -= lines.length;
    out.push(lines === f.lines ? f : { ...f, lines });
  }
  return { files: out, omitted };
}

/** How a diff line reads: added, removed, a hunk header, a file's metadata (index, ---, +++, modes) or context. */
export type DiffLineKind = "add" | "del" | "hunk" | "meta" | "context";

export interface DiffLine {
  readonly kind: DiffLineKind;
  readonly text: string;
}

/** One file of a unified diff: its "diff --git" line, the path it names and its lines after that one. */
export interface DiffFile {
  readonly header: string;
  readonly path: string;
  readonly lines: readonly DiffLine[];
}

/**
 * Splits a unified diff into files at its "diff --git" lines. Before a
 * file's first "@@" its lines are metadata; after it a line's first
 * character says what it is. Text before the first "diff --git" line, if
 * any, is a file with an empty header.
 */
export function parseDiff(text: string): DiffFile[] {
  const files: { header: string; lines: DiffLine[] }[] = [];
  let cur: { header: string; lines: DiffLine[] } | undefined;
  let inHunk = false;
  const rows = text.split("\n");
  if (rows[rows.length - 1] === "") rows.pop();
  for (const row of rows) {
    if (row.startsWith("diff --git ")) {
      cur = { header: row, lines: [] };
      files.push(cur);
      inHunk = false;
      continue;
    }
    if (cur === undefined) {
      cur = { header: "", lines: [] };
      files.push(cur);
    }
    if (row.startsWith("@@")) {
      inHunk = true;
      cur.lines.push({ kind: "hunk", text: row });
    } else if (!inHunk) {
      cur.lines.push({ kind: "meta", text: row });
    } else if (row.startsWith("+")) {
      cur.lines.push({ kind: "add", text: row });
    } else if (row.startsWith("-")) {
      cur.lines.push({ kind: "del", text: row });
    } else if (row.startsWith("\\")) {
      cur.lines.push({ kind: "meta", text: row });
    } else {
      cur.lines.push({ kind: "context", text: row });
    }
  }
  return files.map((f) => ({ ...f, path: filePath(f.header, f.lines) }));
}

/** A file's path: the "+++ b/" line's, or the "--- a/" line's for a deleted file, or else the header's second path. */
function filePath(header: string, lines: readonly DiffLine[]): string {
  for (const l of lines) {
    if (l.kind !== "meta") break;
    if (l.text.startsWith("+++ b/")) return l.text.slice(6);
  }
  for (const l of lines) {
    if (l.kind !== "meta") break;
    if (l.text.startsWith("--- a/")) return l.text.slice(6);
  }
  const m = /^diff --git a\/.* b\/(.*)$/.exec(header);
  return m?.[1] ?? header.replace(/^diff --git /, "");
}

/** The raw unified diff of every repo, in order: what Copy puts on the clipboard. */
export function rawDiff(d: OrderDiff): string {
  return d.repos.map((r) => r.diff).join("");
}

/** Where a copy stands: not tried, done, or refused by the browser. */
export type CopyNote = "" | "copied" | "failed";

/** Writes text with clipboard; false when there is none or it refuses. */
export async function copyText(clipboard: Pick<Clipboard, "writeText"> | undefined, text: string): Promise<boolean> {
  try {
    if (clipboard === undefined) return false;
    await clipboard.writeText(text);
    return true;
  } catch {
    return false;
  }
}

/**
 * A small Copy button for text. Where the clipboard refuses, a note asks
 * to select the text instead, which stays on the page.
 */
export function CopyButton(props: { text: string; label: string; initial?: CopyNote }) {
  const [note, setNote] = useState<CopyNote>(props.initial ?? "");
  const copy = async () => {
    setNote((await copyText(navigator.clipboard, props.text)) ? "copied" : "failed");
  };
  return (
    <>
      <button type="button" class="btn ghost small" aria-label={props.label} onClick={() => void copy()}>
        Copy
      </button>
      {note === "copied" && (
        <span class="notice copy-note" role="status">
          Copied.
        </span>
      )}
      {note === "failed" && (
        <span class="error copy-note" role="alert">
          The clipboard refused. Select the text to copy.
        </span>
      )}
    </>
  );
}

/** One repo's section: its name and base, a truncation note, a block per file and, past maxLines, a note. */
function RepoSection(props: { repo: RepoDiff; branch: string; cap: number; maxLines: number }) {
  const r = props.repo;
  const parsed = parseDiff(r.diff);
  const { files, omitted } = capLines(parsed, props.maxLines);
  const hid = `diff-repo-${r.name}`;
  return (
    <section class="panel diff-repo" aria-labelledby={hid}>
      <h2 id={hid} class="sect">
        <span class="tab bg-blue" aria-hidden="true" />
        {r.name}
        <span class="count mono">
          {r.base.slice(0, 12)}...{props.branch}
        </span>
      </h2>
      {r.truncated && (
        <p class="error" role="note">
          This diff is cut at the bridge's {formatBytes(props.cap)} cap: it stops at its last whole line under it.
        </p>
      )}
      {parsed.length === 0 && <p class="empty">No changes.</p>}
      {files.map((f, i) => (
        <div class="diff-file" key={i}>
          <h3 class="diff-file-h mono" title={f.header}>
            {f.path === "" ? "(preamble)" : f.path}
          </h3>
          <div class="diff-body mono">
            <div class="diff-lines">
              {f.lines.map((l, j) => (
                <div class={`dl dl-${l.kind}`} key={j}>
                  {l.text}
                </div>
              ))}
            </div>
          </div>
        </div>
      ))}
      {omitted > 0 && (
        <p class="error" role="note">
          {omitted.toLocaleString("en-US")} more {omitted === 1 ? "line is" : "lines are"} not shown here. Copy still has them.
        </p>
      )}
    </section>
  );
}

/** A loaded diff: the order's id, title and branch, Copy, and a section per repo, each rendering at most maxLines lines. */
export function DiffView(props: { diff: OrderDiff; copy?: CopyNote; maxLines?: number }) {
  const d = props.diff;
  return (
    <>
      <header class="diff-head">
        <h1 class="diff-title">
          <span class="mono order-id">{d.id}</span> {d.title}
        </h1>
        <span class="mono diff-branch">{d.branch}</span>
        <CopyButton text={rawDiff(d)} label={`Copy the diff of ${d.id}`} {...(props.copy ? { initial: props.copy } : {})} />
      </header>
      {d.repos.length === 0 && <p class="empty">This order has no repos.</p>}
      {d.repos.map((r) => (
        <RepoSection key={r.name} repo={r} branch={d.branch} cap={d.cap} maxLines={props.maxLines ?? maxRenderedLines} />
      ))}
    </>
  );
}

type Load = { readonly kind: "loading" } | { readonly kind: "loaded"; readonly diff: OrderDiff } | { readonly kind: "failed"; readonly message: string };

/** The diff page for order id: it reads the diff once. */
export function DiffPage(props: { id: string }) {
  const [load, setLoad] = useState<Load>({ kind: "loading" });
  useEffect(() => {
    document.title = `Diff ${props.id} - BMUS Bridge`;
    orderDiff(props.id).then(
      (diff) => setLoad({ kind: "loaded", diff }),
      (err: unknown) => setLoad({ kind: "failed", message: err instanceof Error ? err.message : String(err) }),
    );
  }, [props.id]);
  return (
    <div class="console">
      <main class="frame">
        {load.kind === "loading" && <p class="mono gate">Reading the diff of {props.id}...</p>}
        {load.kind === "failed" && (
          <p class="error" role="alert">
            Could not read the diff of {props.id}: {load.message}
          </p>
        )}
        {load.kind === "loaded" && <DiffView diff={load.diff} />}
      </main>
    </div>
  );
}

/** The order id in a diff page's path, /diff/<order>; undefined for any other path. */
export function diffPageOrder(pathname: string): string | undefined {
  const m = /^\/diff\/([A-Za-z0-9-]+)$/.exec(pathname);
  return m?.[1];
}
