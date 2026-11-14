import { renderToString } from "preact-render-to-string";
import { describe, expect, it } from "vitest";
import type { OrderReport } from "../src/api.js";
import { baseText, BudgetFacts, capLines, CopyButton, copyText, formatBytes, parseDiff, rawDiff, reportPageOrder, reportTime, ReportView } from "../src/report.js";
import { crew, order } from "./fixtures.js";

const base = "0123456789abcdef0123456789abcdef01234567";

const appDiff = [
  "diff --git a/holodeck.txt b/holodeck.txt",
  "index 1111111..2222222 100644",
  "--- a/holodeck.txt",
  "+++ b/holodeck.txt",
  "@@ -1,2 +1,2 @@",
  " kept",
  "-old line",
  "+<script>alert(1)</script>",
  "--- not a header: a removed line",
  "\\ No newline at end of file",
  "diff --git a/gone.txt b/gone.txt",
  "deleted file mode 100644",
  "index 3333333..0000000",
  "--- a/gone.txt",
  "+++ /dev/null",
  "@@ -1 +0,0 @@",
  "-bye",
  "",
].join("\n");

const diff: OrderReport = {
  order: {
    ...order("ORD-0007", {
      title: "Add <b>retries</b>",
      state: "done",
      priority: 1,
      division: "science",
      body: "Fix it.\n<script>alert('body')</script>",
      repos: [{ name: "app", base_ref: "main" }, { name: "lib", base_ref: "v1.2" }],
    }),
    created_at: "2026-11-12T09:30:00Z",
    done_at: "2026-11-12T10:05:59Z",
  },
  crew: crew("sorell", { name: "Lt. Sorell", rank: "Lieutenant", division: "science" }),
  beams: ["holodeck-1", "holodeck-2"],
  report: { summary: "Retries in.\n<script>alert('summary')</script>", artifacts: ["notes.md", "<script>alert('artifact')</script>"] },
  pushes: [{ repo: "app", branch: "bmus/ORD-0007", commit: "0123456789abcdef", force: false, at: "2026-11-12T11:00:00Z" }],
  cap: 2 << 20,
  repos: [
    { name: "app", base, diff: appDiff, truncated: false, jettisoned: false },
    { name: "lib", base, diff: "", truncated: true, jettisoned: false },
  ],
};

describe("parseDiff", () => {
  it("splits files at their diff --git lines and reads each line's kind", () => {
    const files = parseDiff(appDiff);
    expect(files.map((f) => [f.header, f.path])).toEqual([
      ["diff --git a/holodeck.txt b/holodeck.txt", "holodeck.txt"],
      ["diff --git a/gone.txt b/gone.txt", "gone.txt"],
    ]);
    expect(files[0]?.lines.map((l) => l.kind)).toEqual(["meta", "meta", "meta", "hunk", "context", "del", "add", "del", "meta"]);
    expect(files[0]?.lines[6]).toEqual({ kind: "add", text: "+<script>alert(1)</script>" });
    expect(files[1]?.lines.map((l) => l.kind)).toEqual(["meta", "meta", "meta", "meta", "hunk", "del"]);
  });

  it("takes the header's path without +++ or --- lines, and keeps text before the first file", () => {
    expect(parseDiff("diff --git a/x.bin b/y.bin\nBinary files a/x.bin and b/y.bin differ\n").map((f) => f.path)).toEqual(["y.bin"]);
    const files = parseDiff("stray\ndiff --git a/a b/a\n");
    expect(files.map((f) => [f.header, f.path, f.lines.length])).toEqual([
      ["", "", 1],
      ["diff --git a/a b/a", "a", 0],
    ]);
    expect(parseDiff("")).toEqual([]);
  });
});

describe("ReportView", () => {
  const html = renderToString(<ReportView report={diff} />);

  it("heads the page MISSION REPORT with the order's id and title", () => {
    expect(html).toContain('<h1 class="gate-title">MISSION REPORT</h1><p class="report-order"><span class="mono order-id">ORD-0007</span> Add &lt;b>retries&lt;/b></p>');
  });

  it("shows the order: state, priority, division, repos with their base ref, times, branch and body", () => {
    expect(html).toContain('<section class="panel report-panel report-wide" aria-labelledby="report-order-h">');
    expect(html).toContain("<dt>State</dt><dd>done</dd>");
    expect(html).toContain("<dt>Priority</dt><dd>Yellow alert</dd>");
    expect(html).toContain("<dt>Division</dt><dd>Science</dd>");
    expect(html).toContain('<li class="mono">app @ main</li><li class="mono">lib @ v1.2</li>');
    expect(html).toContain('<dt>Branch</dt><dd><span class="mono">bmus/ORD-0007</span></dd>');
    expect(html).toContain("<dt>Created</dt><dd>2026-11-12 09:30 UTC</dd>");
    expect(html).toContain("<dt>Done</dt><dd>2026-11-12 10:05 UTC</dd>");
    expect(html).toContain(`<pre class="report-pre">Fix it.\n&lt;script>alert('body')&lt;/script></pre>`);
  });

  it("shows a base naming another order's work with the commit it started from (#143)", () => {
    const based = renderToString(
      <ReportView report={{ ...diff, order: { ...diff.order, repos: [{ name: "app", base_ref: "order:ORD-0001" }, { name: "lib", base_ref: "v1.2" }] } }} />,
    );
    expect(based).toContain('<li class="mono">app @ order:ORD-0001 (0123456789ab)</li><li class="mono">lib @ v1.2</li>');
  });

  it("shows the crew member, the beams, the report and the pushes", () => {
    expect(html).toContain('<dt>Name</dt><dd>Lt. Sorell</dd><dt>Id</dt><dd><span class="mono">sorell</span></dd><dt>Rank</dt><dd>Lieutenant</dd><dt>Division</dt><dd>Science</dd>');
    expect(html).toContain('<li class="mono">holodeck-1</li><li class="mono">holodeck-2</li>');
    expect(html).toContain(`<p class="report-summary">Retries in.\n&lt;script>alert('summary')&lt;/script></p>`);
    expect(html).toContain(`<li class="mono">notes.md</li><li class="mono">&lt;script>alert('artifact')&lt;/script></li>`);
    expect(html).toContain('<span class="mono">app: bmus/ORD-0007 at 0123456789ab</span>, 2026-11-12 11:00 UTC');
  });

  it("escapes every text from the order and the agent, and sets no style", () => {
    expect(html).not.toContain("<script>");
    expect(html).not.toContain("<b>");
    expect(html).not.toContain("style=");
  });

  it("shows unknown where the bridge has no record, and no pushes panel without pushes", () => {
    const bare = renderToString(
      <ReportView
        report={{ ...diff, order: { ...diff.order, division: "", created_at: null, done_at: null }, crew: null, beams: [], report: null, pushes: [] }}
      />,
    );
    expect(bare).toContain("<dt>Division</dt><dd>unknown</dd>");
    expect(bare).toContain("<dt>Created</dt><dd>unknown</dd><dt>Done</dt><dd>unknown</dd>");
    expect(bare).toContain('<p class="empty">Crew member: unknown.</p>');
    expect(bare).toContain('<p class="empty">Beam: unknown.</p>');
    expect(bare).toContain('<p class="empty">Report: unknown.</p>');
    expect(bare).not.toContain("Pushes");
    const blankCrew = renderToString(<ReportView report={{ ...diff, crew: crew("x", { name: "", rank: "", division: "" }) }} />);
    expect(blankCrew).toContain("<dt>Name</dt><dd>unknown</dd>");
    expect(blankCrew).toContain("<dt>Rank</dt><dd>unknown</dd>");
    expect(reportTime("not a time")).toBe("unknown");
  });

  it("then the diff, with a section per repo and a header per file", () => {
    expect(html.indexOf("Diff</h2>")).toBeGreaterThan(html.indexOf("report-summary"));
    expect(html).toContain('<section class="panel diff-repo" aria-labelledby="diff-repo-app">');
    expect(html).toContain(`${base.slice(0, 12)}...bmus/ORD-0007`);
    expect(html).toContain('<h3 class="diff-file-h mono" title="diff --git a/holodeck.txt b/holodeck.txt">holodeck.txt</h3>');
    expect(html).toContain('<h3 class="diff-file-h mono" title="diff --git a/gone.txt b/gone.txt">gone.txt</h3>');
  });

  it("colours added and removed lines, and escapes the diff's text", () => {
    expect(html).toContain('<div class="dl dl-add">+&lt;script>alert(1)&lt;/script></div>');
    expect(html).toContain('<div class="dl dl-del">-old line</div>');
    expect(html).toContain('<div class="dl dl-hunk">@@ -1,2 +1,2 @@</div>');
    expect(html).toContain('<div class="dl dl-context"> kept</div>');
    expect(html).not.toContain("<script>");
    expect(html).not.toContain("<b>");
    expect(html).not.toContain("style=");
  });

  it("notes a truncated repo with the cap, and an empty one", () => {
    expect(html).toContain("This diff is cut at the bridge's 2 MiB cap");
    expect(html.split("cut at the bridge's").length - 1).toBe(1);
    expect(html).toContain('<p class="empty">No changes.</p>');
  });

  it("marks a jettisoned repo's branch as gone, with no diff and no error (#108)", () => {
    const jettisoned = renderToString(
      <ReportView report={{ ...diff, repos: [diff.repos[0]!, { name: "lib", base, diff: "", truncated: false, jettisoned: true }] }} />,
    );
    expect(jettisoned).toContain('<span class="count mono">branch jettisoned</span>');
    expect(jettisoned).toContain("Branch jettisoned: <span class=\"mono\">bmus/ORD-0007</span> was deleted from the bridge's clone of lib");
    expect(jettisoned).not.toContain("No changes.");
    expect(jettisoned).not.toContain('role="alert"');
    expect(html).not.toContain("jettisoned");
  });

  it("copies the raw diff of every repo", () => {
    expect(rawDiff({ ...diff, repos: [diff.repos[0]!, { name: "lib", base, diff: "diff --git a/l b/l\n", truncated: false, jettisoned: false }] })).toBe(
      `${appDiff}diff --git a/l b/l\n`,
    );
    expect(html).toContain('<button type="button" class="btn ghost small" aria-label="Copy the diff of ORD-0007">Copy</button>');
  });
});

describe("rendered lines", () => {
  it("caps the lines across a repo's files and counts the rest", () => {
    const files = parseDiff(appDiff);
    expect(capLines(files, 100)).toEqual({ files, omitted: 0 });
    const cut = capLines(files, 10);
    expect(cut.files.map((f) => [f.path, f.lines.length])).toEqual([
      ["holodeck.txt", 9],
      ["gone.txt", 1],
    ]);
    expect(cut.omitted).toBe(5);
    expect(capLines(files, 9).files.map((f) => f.path)).toEqual(["holodeck.txt"]);
    expect(capLines(files, 9).omitted).toBe(6);
  });

  it("notes the lines left out, which Copy still has", () => {
    const html = renderToString(<ReportView report={diff} maxLines={4} />);
    expect(html).toContain('<p class="error" role="note">11 more lines are not shown here. Copy still has them.</p>');
    expect(html).not.toContain("dl-add");
    expect(html.match(/class="dl /g)).toHaveLength(4);
    expect(renderToString(<ReportView report={diff} />)).not.toContain("not shown here");
  });

  it("names the cap from the reply", () => {
    expect(formatBytes(2 << 20)).toBe("2 MiB");
    expect(formatBytes(64 << 10)).toBe("64 KiB");
    expect(formatBytes(1000)).toBe("1000 bytes");
    expect(renderToString(<ReportView report={{ ...diff, cap: 512 << 10 }} />)).toContain("cut at the bridge's 512 KiB cap");
  });
});

describe("copy", () => {
  it("writes the text, and fails without a clipboard or when it refuses", async () => {
    const written: string[] = [];
    expect(await copyText({ writeText: async (t) => void written.push(t) }, "diff")).toBe(true);
    expect(written).toEqual(["diff"]);
    expect(await copyText({ writeText: () => Promise.reject(new Error("denied")) }, "diff")).toBe(false);
    expect(await copyText(undefined, "diff")).toBe(false);
  });

  it("says when it copied, and asks to select the text when the clipboard refused", () => {
    expect(renderToString(<CopyButton text="x" label="Copy x" />)).not.toContain("copy-note");
    expect(renderToString(<CopyButton text="x" label="Copy x" initial="copied" />)).toContain('<span class="notice copy-note" role="status">Copied.</span>');
    const failed = renderToString(<ReportView report={diff} copy="failed" />);
    expect(failed).toContain('<span class="error copy-note" role="alert">The clipboard refused. Select the text to copy.</span>');
    expect(failed).toContain('<div class="dl dl-add">+&lt;script>alert(1)&lt;/script></div>');
  });
});

describe("baseText", () => {
  it("adds the commit to an order: base only, and only when the report has it", () => {
    const diffs = diff.repos;
    expect(baseText({ name: "app", base_ref: "main" }, diffs)).toBe("main");
    expect(baseText({ name: "app", base_ref: "order:ORD-0001" }, diffs)).toBe("order:ORD-0001 (0123456789ab)");
    expect(baseText({ name: "web", base_ref: "order:ORD-0001" }, diffs)).toBe("order:ORD-0001");
    expect(baseText({ name: "app", base_ref: "order:ORD-0001" }, [{ name: "app", base: "", diff: "", truncated: false, jettisoned: true }])).toBe(
      "order:ORD-0001",
    );
  });
});

describe("reportPageOrder", () => {
  it("reads the order id from /report/<order> only", () => {
    expect(reportPageOrder("/report/ORD-0007")).toBe("ORD-0007");
    for (const p of ["/", "/report/", "/report/ORD-0007/", "/report/a/b", "/report/ORD%2F1", "/x/report/ORD-1", "/diff/ORD-0007"]) {
      expect(reportPageOrder(p), p).toBeUndefined();
    }
  });
});

describe("the report's budget (#120)", () => {
  const usage = { input_tokens: 800_000, output_tokens: 20_000, turns: 42 };

  it("shows each field against its limit, with its source, a meter and the extensions", () => {
    const html = renderToString(
      <BudgetFacts
        order={order("ORD-0001", {
          state: "done",
          budget_tokens: 500_000,
          limit_tokens: 1_000_000,
          limit_turns: 150,
          budget_source: { tokens: "order", turns: "division" },
          budget_extended: 1,
          usage,
        })}
      />,
    );
    expect(html).toContain('<span class="mono tone-gold">820k / 1M tokens</span> <span class="report-src">order budget</span>');
    expect(html).toContain('<span class="mono tone-green">42 / 150 turns</span> <span class="report-src">division budget</span>');
    expect(html).toContain('role="meter" aria-label="ORD-0001 tokens used" aria-valuemin="0" aria-valuemax="1000000" aria-valuenow="820000"');
    expect(html).toContain("<dt>Extensions</dt><dd>1: each limit is 2 times its budget</dd>");
    expect(html).not.toContain("No budget");
  });

  it("marks a field over its limit, and one without a limit", () => {
    const html = renderToString(
      <BudgetFacts order={order("ORD-0002", { limit_tokens: 700_000, budget_source: { tokens: "division", turns: "default" }, usage })} />,
    );
    expect(html).toContain('<span class="mono tone-red">820k / 700k tokens (over)</span> <span class="report-src">division budget</span>');
    expect(html).toContain('class="fill bg-red" style="width:100%;"');
    expect(html).toContain('<span class="mono tone-grey">42 turns</span> <span class="report-src">no limit</span>');
    expect(html).toContain("<dt>Extensions</dt><dd>none</dd>");
  });

  it("says No budget and shows the usage without one", () => {
    const html = renderToString(<BudgetFacts order={order("ORD-0003", { usage })} />);
    expect(html).toContain("No budget.");
    expect(html).toContain('<dt>Tokens</dt><dd><span class="mono">820k tokens</span></dd><dt>Turns</dt><dd><span class="mono">42 turns</span></dd>');
    expect(html).not.toContain('role="meter"');
    expect(html).not.toContain("Extensions");
  });

  it("sits in its own panel on the page", () => {
    const html = renderToString(<ReportView report={{ ...diff, order: { ...diff.order, usage } }} />);
    expect(html).toContain('aria-labelledby="report-budget-h"');
    expect(html).toContain("No budget.");
  });
});
