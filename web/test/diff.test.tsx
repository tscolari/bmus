import { renderToString } from "preact-render-to-string";
import { describe, expect, it } from "vitest";
import type { OrderDiff } from "../src/api.js";
import { CopyButton, copyText, diffPageOrder, DiffView, parseDiff, rawDiff } from "../src/diff.js";

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

const diff: OrderDiff = {
  id: "ORD-0007",
  title: "Add <b>retries</b>",
  branch: "bmus/ORD-0007",
  repos: [
    { name: "app", base, diff: appDiff, truncated: false },
    { name: "lib", base, diff: "", truncated: true },
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

describe("DiffView", () => {
  const html = renderToString(<DiffView diff={diff} />);

  it("heads the page with the order's id, title and branch, and a section per repo", () => {
    expect(html).toContain('<h1 class="diff-title"><span class="mono order-id">ORD-0007</span> Add &lt;b>retries&lt;/b></h1>');
    expect(html).toContain('<span class="mono diff-branch">bmus/ORD-0007</span>');
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

  it("copies the raw diff of every repo", () => {
    expect(rawDiff({ ...diff, repos: [diff.repos[0]!, { name: "lib", base, diff: "diff --git a/l b/l\n", truncated: false }] })).toBe(
      `${appDiff}diff --git a/l b/l\n`,
    );
    expect(html).toContain('<button type="button" class="btn ghost small" aria-label="Copy the diff of ORD-0007">Copy</button>');
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
    const failed = renderToString(<DiffView diff={diff} copy="failed" />);
    expect(failed).toContain('<span class="error copy-note" role="alert">The clipboard refused. Select the text to copy.</span>');
    expect(failed).toContain('<div class="dl dl-add">+&lt;script>alert(1)&lt;/script></div>');
  });
});

describe("diffPageOrder", () => {
  it("reads the order id from /diff/<order> only", () => {
    expect(diffPageOrder("/diff/ORD-0007")).toBe("ORD-0007");
    for (const p of ["/", "/diff/", "/diff/ORD-0007/", "/diff/a/b", "/diff/ORD%2F1", "/x/diff/ORD-1"]) {
      expect(diffPageOrder(p), p).toBeUndefined();
    }
  });
});
