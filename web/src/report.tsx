// The mission report (#104): a done order, opened from its mission record
// row at /report/<order> in a new tab. It reads GET
// /api/orders/{id}/report and shows the order, the crew member and beams
// that ran it, the crew's report, its pushes, then each repo's unified
// diff, a section per repo and a header per file, added lines green and
// removed ones red. The order's body, the report and the diff come from
// the captain and the agent: they are rendered as plain text children
// only, never as HTML.

import { Fragment, type ComponentChildren } from "preact";
import { useEffect, useState } from "preact/hooks";
import { orderReport, type CrewView, type OrderReport, type OrderView, type RepoDiff } from "./api.js";
import { UsageMeter } from "./panels.js";
import { budgetSourceText, divisionInfo, extensionsText, orderBranch, orderUsage, priorityInfo } from "./view.js";

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
export function rawDiff(d: { readonly repos: readonly RepoDiff[] }): string {
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

/**
 * One repo's section: its name and base, a truncation note, a block per
 * file and, past maxLines, a note. A jettisoned repo's section says the
 * branch is gone instead (#108).
 */
function RepoSection(props: { repo: RepoDiff; branch: string; cap: number; maxLines: number }) {
  const r = props.repo;
  const hid = `diff-repo-${r.name}`;
  if (r.jettisoned) {
    return (
      <section class="panel diff-repo" aria-labelledby={hid}>
        <h2 id={hid} class="sect">
          <span class="tab bg-blue" aria-hidden="true" />
          {r.name}
          <span class="count mono">branch jettisoned</span>
        </h2>
        <p class="empty" role="note">
          Branch jettisoned: <span class="mono">{props.branch}</span> was deleted from the bridge's clone of {r.name} by{" "}
          <span class="mono">bmus jettison</span>, and its diff with it.
        </p>
      </section>
    );
  }
  const parsed = parseDiff(r.diff);
  const { files, omitted } = capLines(parsed, props.maxLines);
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

/** What shows for a field the bridge has no record of. */
export const unknown = "unknown";

/** A timestamp as "YYYY-MM-DD HH:MM UTC"; unknown when missing or unparsable. */
export function reportTime(ts: string | null | undefined): string {
  if (!ts) return unknown;
  const d = new Date(ts);
  if (Number.isNaN(d.getTime())) return unknown;
  return `${d.toISOString().slice(0, 16).replace("T", " ")} UTC`;
}

/** A report panel: a heading and its content. */
function Panel(props: { id: string; title: string; class?: string; children: ComponentChildren }) {
  return (
    <section class={`panel report-panel${props.class ? ` ${props.class}` : ""}`} aria-labelledby={props.id}>
      <h2 id={props.id} class="sect">
        <span class="tab bg-gold" aria-hidden="true" />
        {props.title}
      </h2>
      {props.children}
    </section>
  );
}

/** Label and value pairs; an empty value shows as unknown. */
function Facts(props: { rows: readonly (readonly [string, ComponentChildren])[] }) {
  return (
    <dl class="facts">
      {props.rows.map(([k, v]) => (
        <Fragment key={k}>
          <dt>{k}</dt>
          <dd>{v === "" || v === undefined || v === null ? unknown : v}</dd>
        </Fragment>
      ))}
    </dl>
  );
}

function CrewFacts(props: { crew: CrewView | null }) {
  const c = props.crew;
  if (c === null) return <p class="empty">Crew member: {unknown}.</p>;
  return (
    <Facts
      rows={[
        ["Name", c.name],
        ["Id", <span class="mono">{c.id}</span>],
        ["Rank", c.rank],
        ["Division", c.division === "" ? "" : divisionInfo(c.division).label],
      ]}
    />
  );
}

/**
 * The order's final usage against its limits (#120): each field's usage,
 * its limit's source and a meter, and the extensions granted; without a
 * budget, "No budget" and the usage.
 */
export function BudgetFacts(props: { order: OrderView }) {
  const o = props.order;
  const { budgeted, fields } = orderUsage(o);
  const source = (unit: string) => (unit === "tokens" ? o.budget_source?.tokens : o.budget_source?.turns);
  const label = (unit: string) => (unit === "tokens" ? "Tokens" : "Turns");
  if (!budgeted) {
    return (
      <>
        <p class="empty report-nobudget">No budget.</p>
        <Facts rows={fields.map((f) => [label(f.unit), <span class="mono">{f.text}</span>] as const)} />
      </>
    );
  }
  return (
    <Facts
      rows={[
        ...fields.map(
          (f) =>
            [
              label(f.unit),
              <div class="report-usage">
                <span class={`mono tone-${f.tone}`}>{f.text}</span>{" "}
                <span class="report-src">{f.limit > 0 ? budgetSourceText(source(f.unit)) : "no limit"}</span>
                <UsageMeter id={o.id} field={f} />
              </div>,
            ] as const,
        ),
        ["Extensions", extensionsText(o.budget_extended)] as const,
      ]}
    />
  );
}

/** A loaded mission report: the header, the order, crew, beam, report and pushes, then the diff with Copy, each repo rendering at most maxLines lines. */
export function ReportView(props: { report: OrderReport; copy?: CopyNote; maxLines?: number }) {
  const r = props.report;
  const o = r.order;
  const branch = orderBranch(o.id);
  return (
    <>
      <header class="report-head">
        <h1 class="gate-title">MISSION REPORT</h1>
        <p class="report-order">
          <span class="mono order-id">{o.id}</span> {o.title}
        </p>
      </header>
      <Panel id="report-order-h" title="Order" class="report-wide">
        <Facts
          rows={[
            ["State", o.state],
            ["Priority", priorityInfo(o.priority).label],
            ["Division", o.division === "" ? "" : divisionInfo(o.division).label],
            [
              "Repos",
              o.repos.length === 0 ? (
                "none"
              ) : (
                <ul class="report-list">
                  {o.repos.map((repo) => (
                    <li key={repo.name} class="mono">
                      {repo.name} @ {repo.base_ref}
                    </li>
                  ))}
                </ul>
              ),
            ],
            ["Branch", <span class="mono">{branch}</span>],
            ["Created", reportTime(o.created_at)],
            ["Done", reportTime(o.done_at)],
          ]}
        />
        <h3 class="report-sub">Body</h3>
        {o.body === "" ? <p class="empty">No body.</p> : <pre class="report-pre">{o.body}</pre>}
      </Panel>
      <div class="report-grid">
        <Panel id="report-crew-h" title="Crew">
          <CrewFacts crew={r.crew} />
        </Panel>
        <Panel id="report-beam-h" title="Beam">
          {r.beams.length === 0 ? (
            <p class="empty">Beam: {unknown}.</p>
          ) : (
            <ul class="report-list">
              {r.beams.map((b) => (
                <li key={b} class="mono">
                  {b}
                </li>
              ))}
            </ul>
          )}
        </Panel>
      </div>
      <Panel id="report-budget-h" title="Budget" class="report-wide">
        <BudgetFacts order={o} />
      </Panel>
      <Panel id="report-report-h" title="Report" class="report-wide">
        {r.report === null ? (
          <p class="empty">Report: {unknown}.</p>
        ) : (
          <>
            {r.report.summary === "" ? <p class="empty">No summary.</p> : <p class="report-summary">{r.report.summary}</p>}
            <h3 class="report-sub">Artifacts</h3>
            {r.report.artifacts.length === 0 ? (
              <p class="empty">None.</p>
            ) : (
              <ul class="report-list">
                {r.report.artifacts.map((a, i) => (
                  <li key={i} class="mono">
                    {a}
                  </li>
                ))}
              </ul>
            )}
          </>
        )}
      </Panel>
      {r.pushes.length > 0 && (
        <Panel id="report-pushes-h" title="Pushes" class="report-wide">
          <ul class="report-list">
            {r.pushes.map((p, i) => (
              <li key={i}>
                <span class="mono">
                  {p.repo}: {p.branch} at {p.commit.slice(0, 12)}
                </span>
                {p.force && " (forced)"}, {reportTime(p.at)}
              </li>
            ))}
          </ul>
        </Panel>
      )}
      <div class="report-diff-head">
        <h2 class="sect">
          <span class="tab bg-blue" aria-hidden="true" />
          Diff
        </h2>
        <CopyButton text={rawDiff(r)} label={`Copy the diff of ${o.id}`} {...(props.copy ? { initial: props.copy } : {})} />
      </div>
      {r.repos.length === 0 && <p class="empty">This order has no repos.</p>}
      {r.repos.map((repo) => (
        <RepoSection key={repo.name} repo={repo} branch={branch} cap={r.cap} maxLines={props.maxLines ?? maxRenderedLines} />
      ))}
    </>
  );
}

type Load = { readonly kind: "loading" } | { readonly kind: "loaded"; readonly report: OrderReport } | { readonly kind: "failed"; readonly message: string };

/** The mission report page for order id: it reads the report once. */
export function ReportPage(props: { id: string }) {
  const [load, setLoad] = useState<Load>({ kind: "loading" });
  useEffect(() => {
    document.title = `Mission report ${props.id} - BMUS Bridge`;
    orderReport(props.id).then(
      (report) => setLoad({ kind: "loaded", report }),
      (err: unknown) => setLoad({ kind: "failed", message: err instanceof Error ? err.message : String(err) }),
    );
  }, [props.id]);
  return (
    <div class="console">
      <main class="frame">
        {load.kind === "loading" && <p class="mono gate">Reading the mission report of {props.id}...</p>}
        {load.kind === "failed" && (
          <p class="error" role="alert">
            Could not read the mission report of {props.id}: {load.message}
          </p>
        )}
        {load.kind === "loaded" && <ReportView report={load.report} />}
      </main>
    </div>
  );
}

/** The order id in a mission report's path, /report/<order>; undefined for any other path. */
export function reportPageOrder(pathname: string): string | undefined {
  const m = /^\/report\/([A-Za-z0-9-]+)$/.exec(pathname);
  return m?.[1];
}
