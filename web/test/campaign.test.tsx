// The campaign hails on the console (#187): LandCard, StalledCard, a
// campaign triage on HailCard, the open-orders line and the report's
// verdict. Agent and tracker text (issue titles, commit subjects, review
// summaries, finding titles and bodies, stall reasons) renders as text.
import { renderToString } from "preact-render-to-string";
import { describe, expect, it } from "vitest";
import type { CampaignLandContext, CampaignStalledContext, HailView, OrderReport } from "../src/api.js";
import { hailNotice, notifies } from "../src/notify.js";
import { HailCard, HailsPanel, LandCard, OpenOrdersPanel, StalledCard } from "../src/panels.js";
import { ReportView } from "../src/report.js";
import { emptyState, reduce } from "../src/store.js";
import {
  bridgeHailOrder,
  campaignOrderText,
  choiceDecision,
  dropPrompt,
  hailKind,
  landContext,
  parseTaskID,
  stalledContext,
  triageFindings,
  verdictText,
  verdictTone,
} from "../src/view.js";
import { hail, order, snapshot } from "./fixtures.js";

const hostile = `<script>alert(1)</script> **bold** [link](https://example.com) <img src=x onerror="alert(1)">`;
const escaped = "&lt;script>alert(1)&lt;/script> **bold** [link](https://example.com) &lt;img src=x onerror=&quot;alert(1)&quot;>";
const head = "3f2a9c1e0b7d44a1c0ffee5eed1e55c0de0b7d01";
const base = "8b1d0e2c4a55f00dfeedfacecafebeef01234567";

const land: CampaignLandContext = {
  campaign: "C-0001",
  task: "acme/app#12",
  issue_title: hostile,
  repo: "app",
  branch: "campaign/C-0001/acme-app-12",
  head: "ORD-0011",
  head_commit: head,
  review_base: base,
  commits: [
    { sha: "a".repeat(40), subject: "Add retries to the webhook sender" },
    { sha: "b".repeat(40), subject: hostile },
  ],
  commit_count: 5,
  rounds: 2,
  review: "ORD-0012",
  review_summary: `Retries are bounded.\n${hostile}`,
  passed: true,
};

function campaignHail(id: string, category: string, context: unknown, over: Partial<HailView> = {}): HailView {
  return hail(id, {
    order_id: "",
    crew_id: "",
    deliver_to: "campaign/C-0001/task/acme/app#12",
    category,
    question: `Land acme/app#12 of campaign C-0001: push ORD-0011 (${head.slice(0, 12)}) to campaign/C-0001/acme-app-12 on repo app, 5 commits over ${base.slice(0, 12)}, passed review in round 2?`,
    options: ["land", "retry", "drop"],
    context,
    route: "captain",
    ...over,
  });
}

const stalled: CampaignStalledContext = {
  campaign: "C-0001",
  task: "acme/app#12",
  issue_title: "Retry the webhook sender",
  code: "max_rounds",
  reason: `round 3 of 3 blocked ${hostile}`,
  round: 3,
  max_rounds: 3,
  head: "ORD-0013",
  head_commit: head,
  order: "ORD-0014",
  blocking: [
    { n: 1, severity: "high", title: hostile, body: `The writer ${hostile}`, path: "sender/retry.go", line: 41 },
    { n: 2, severity: "medium", title: "Backoff has no jitter", body: "" },
  ],
};

const landHail = campaignHail("H-0009", "campaign_land", land);
const stallHail = campaignHail("H-0010", "campaign_stalled", stalled, {
  question: "A task of campaign C-0001 stalled in round 3 of 3 on ORD-0014 (max_rounds): land ORD-0013 (3f2a9c1e0b7d) as it is, retry or drop? An option may be followed by a note: retry: <note>.",
});
const repoUrls = { app: "git@github.com:acme/app.git" };

describe("LandCard (#187)", () => {
  const html = renderToString(<LandCard hail={landHail} repoUrls={repoUrls} csrf="c" />);

  it("names the campaign and the task on its meta line, and links the ref to its issue", () => {
    expect(html).toContain(">H-0009 · Bridge · campaign C-0001 · task acme/app#12<");
    expect(html).toContain('href="https://github.com/acme/app/issues/12"');
  });

  it("shows the branch, head, review base and rounds", () => {
    expect(html).toContain("app: campaign/C-0001/acme-app-12");
    expect(html).toContain(`ORD-0011 at ${head.slice(0, 12)}`);
    expect(html).toContain(`>${base.slice(0, 12)}<`);
    expect(html).toContain("<dt>Rounds</dt>");
  });

  it("lists the commits with short ids, and how many more there are", () => {
    expect(html).toContain("5 commits over the review base");
    expect(html).toContain(`>${"a".repeat(12)}<`);
    expect(html).toContain("Add retries to the webhook sender");
    expect(html).toContain("and 3 more");
  });

  it("renders the issue title, commit subjects and the review's summary as text", () => {
    expect(html).not.toContain("<script>");
    expect(html).not.toContain("<img");
    expect(html).not.toContain("<strong>");
    // Title, one subject, and the summary.
    expect(html.split(escaped).length - 1).toBe(3);
    expect(html).toContain("Review ORD-0012 passed");
    expect(html).not.toContain("no passing review");
  });

  it("offers Land, Another round with a note and Drop behind a confirm", () => {
    expect(html).toContain(">Land<");
    expect(html).toContain(">Another round<");
    expect(html).toContain('<label for="hail-H-0009-note" class="camp-note-label">Note for another round</label>');
    expect(html).toContain('aria-describedby="hail-H-0009-note"');
    expect(html).toContain('aria-label="Drop the task of H-0009"');
    // The confirm's first step only.
    expect(html).not.toContain("without landing?");
  });

  it("says when the work has no passing review, and when there was none", () => {
    const blocked = renderToString(<LandCard hail={campaignHail("H-0011", "campaign_land", { ...land, passed: false })} repoUrls={{}} csrf="c" />);
    expect(blocked).toContain("No passing review: ORD-0012 was the last");
    expect(blocked).toContain("This work has no passing review.");
    // No registered github.com repo: the ref is text.
    expect(blocked).not.toContain("href=");
    const none = renderToString(<LandCard hail={campaignHail("H-0012", "campaign_land", { ...land, passed: false, review: "", review_summary: "" })} repoUrls={{}} csrf="c" />);
    expect(none).toContain("No review yet");
  });
});

describe("StalledCard (#187)", () => {
  const html = renderToString(<StalledCard hail={stallHail} repoUrls={{}} csrf="c" />);

  it("shows the code, the reason, the round of its max, the head and the order it stalled on", () => {
    expect(html).toContain(">H-0010 · Bridge · campaign C-0001 · task acme/app#12<");
    expect(html).toContain(">max_rounds<");
    expect(html).toContain(`round 3 of 3 blocked ${escaped}`);
    expect(html).toContain(">3 of 3<");
    expect(html).toContain(`ORD-0013 at ${head.slice(0, 12)}`);
    expect(html).toContain("<dt>Stalled on</dt>");
  });

  it("lists the blocking findings, numbered, with their place, all as text", () => {
    expect(html).toContain("Blocking findings: 2");
    expect(html).toContain("sender/retry.go:41");
    expect(html).toContain("Backoff has no jitter");
    expect(html).not.toContain("<script>");
    expect(html).not.toContain("<img");
    // The reason, a title and a body.
    expect(html.split(escaped).length - 1).toBe(3);
    expect(html).toContain('class="camp-finding sev-high"');
  });

  it("offers the hail's options: land, retry with a note, and drop", () => {
    expect(html).toContain(">Land as it is<");
    expect(html).toContain(">Retry<");
    expect(html).toContain("Note for retry");
    expect(html).toContain('aria-label="Drop the task of H-0010"');
  });

  it("has no land without a head", () => {
    const headless = campaignHail("H-0013", "campaign_stalled", { ...stalled, head: "", head_commit: "", blocking: [] }, { options: ["retry", "drop"] });
    const card = renderToString(<StalledCard hail={headless} repoUrls={{}} csrf="c" />);
    expect(card).not.toContain("Land as it is");
    expect(card).toContain(">none yet<");
    expect(card).not.toContain("Blocking findings");
    expect(card).toContain(">Retry<");
  });
});

describe("a campaign triage on HailCard (#187)", () => {
  const triage = campaignHail(
    "H-0014",
    "campaign_triage",
    {
      campaign: "C-0001",
      task: "acme/app#12",
      issue_title: "Retry the webhook sender",
      review: "ORD-0009",
      round: 1,
      findings: [
        { n: 1, severity: "medium", title: hostile, body: "b", path: "x.go", line: 3 },
        { n: 2, severity: "medium", title: "Second", body: "" },
      ],
    },
    { options: ["file", "fix", "drop"], question: "Triage 2 medium findings from ORD-0009's review in round 1: answer file, fix or drop for all of them, or a JSON object." },
  );
  const html = renderToString(<HailCard hail={triage} crew={undefined} order={undefined} csrf="c" />);

  it("lists the numbered mediums, offers the options and takes the JSON form in the decision field", () => {
    expect(html).toContain(">H-0014 · Bridge · campaign C-0001 · task acme/app#12<");
    expect(html).toContain("Medium findings: 2");
    expect(html).toContain(">file<");
    expect(html).toContain(">fix<");
    expect(html).toContain(">drop<");
    expect(html).toContain("Decision for H-0014, as a JSON object of finding numbers");
    expect(html).toContain('placeholder="{&quot;fix&quot;: [1], &quot;file&quot;: [2], &quot;drop&quot;: [3]}"');
    expect(html).not.toContain("<script>");
    // The context shows as the list, not as JSON.
    expect(html).not.toContain("&quot;findings&quot;");
    expect(html).not.toContain("Belay");
    expect(triageFindings(triage).map((f) => f.n)).toEqual([1, 2]);
  });
});

describe("campaign hails on the hails panel (#187)", () => {
  const s = reduce(emptyState, { type: "snapshot", snapshot: snapshot({ hails: [landHail, stallHail], repo_urls: repoUrls }) });
  const html = renderToString(<HailsPanel state={s} csrf="c" />);

  it("picks LandCard and StalledCard", () => {
    expect(hailKind(landHail)).toBe("land");
    expect(hailKind(stallHail)).toBe("stalled");
    expect(hailKind(campaignHail("H-1", "campaign_triage", {}))).toBe("bridge");
    expect(html).toContain('class="row hail camp land"');
    expect(html).toContain('class="row hail camp stalled"');
  });

  it("names the head of a land, the stalled order of a stall, else its head, and the review of a triage", () => {
    expect(bridgeHailOrder(landHail)).toBe("ORD-0011");
    expect(bridgeHailOrder(stallHail)).toBe("ORD-0014");
    expect(bridgeHailOrder(campaignHail("H-1", "campaign_stalled", { ...stalled, order: "" }))).toBe("ORD-0013");
    expect(bridgeHailOrder(campaignHail("H-1", "campaign_triage", { review: "ORD-0009" }))).toBe("ORD-0009");
  });

  it("notifies a landing and a stall from the bridge", () => {
    expect(hailNotice(s, landHail)).toEqual({ title: "Landing from the bridge · ORD-0011", body: landHail.question, tag: "H-0009" });
    expect(hailNotice(s, stallHail).title).toBe("Stall from the bridge · ORD-0014");
    expect(hailNotice(s, campaignHail("H-1", "campaign_triage", { review: "ORD-0009" })).title).toBe("Hail from the bridge · ORD-0009");
    expect(notifies(landHail)).toBe(true);
    expect(notifies(stallHail)).toBe(true);
  });
});

describe("campaign helpers (#187)", () => {
  it("builds retry's answer with a note on one line", () => {
    expect(choiceDecision("retry", "")).toBe("retry");
    expect(choiceDecision("retry", "   ")).toBe("retry");
    expect(choiceDecision("retry", "  the flaky\ttest\nis real ")).toBe("retry: the flaky test is real");
  });

  it("reads contexts leniently", () => {
    const c = landContext(campaignHail("H-1", "campaign_land", { commits: [{ subject: "no sha" }, { sha: "c".repeat(40) }], commit_count: "9", passed: "yes" }));
    expect(c.commits).toEqual([{ sha: "c".repeat(40), subject: "" }]);
    expect(c.commit_count).toBe(1);
    expect(c.passed).toBe(false);
    expect(c.task).toBe("");
    const st = stalledContext(campaignHail("H-1", "campaign_stalled", { blocking: [{ n: 1, title: "t" }, { title: "no number" }, "x"] }));
    expect(st.blocking).toEqual([{ n: 1, severity: "", title: "t", body: "" }]);
    expect(stalledContext(campaignHail("H-1", "campaign_stalled", null)).blocking).toEqual([]);
  });

  it("names a campaign order by kind, task ref and round", () => {
    expect(parseTaskID("campaign/C-0001/task/o/r#12")).toEqual({ campaign: "C-0001", ref: "o/r#12" });
    expect(parseTaskID("findings/ORD-0001")).toBeUndefined();
    expect(campaignOrderText(order("ORD-1", { kind: "review", task: "campaign/C-0001/task/o/r#12", round: 2 }))).toBe("review of o/r#12, round 2");
    expect(campaignOrderText(order("ORD-1", { task: "campaign/C-0001/task/o/r#12", round: 1 }))).toBe("work on o/r#12, round 1");
    expect(campaignOrderText(order("ORD-1", { task: "odd" }))).toBe("work on odd");
    expect(campaignOrderText(order("ORD-1"))).toBeUndefined();
  });

  it("asks before a drop", () => {
    expect(dropPrompt(land)).toBe("Drop acme/app#12 without landing? Campaign C-0001 ends with it.");
  });

  it("sums a verdict", () => {
    expect(verdictText([])).toBe("Passed: no findings");
    expect(verdictText([{ severity: "high" }, { severity: "low" }, { severity: "high" }])).toBe("Blocked: 2 high, 1 low");
    expect(verdictText([{ severity: "medium" }])).toBe("Findings: 1 medium");
    expect([verdictTone([]), verdictTone([{ severity: "high" }]), verdictTone([{ severity: "low" }])]).toEqual(["passed", "blocked", "findings"]);
  });
});

describe("the open orders card names a campaign order (#187)", () => {
  it("shows its kind, task ref and round", () => {
    const s = reduce(emptyState, {
      type: "snapshot",
      snapshot: snapshot({
        orders: [
          order("ORD-0012", { title: "Review o/r#12 (round 2)", kind: "review", task: "campaign/C-0001/task/o/r#12", round: 2, division: "science" }),
          order("ORD-0013", { title: "Plain" }),
        ],
      }),
    });
    const html = renderToString(<OpenOrdersPanel state={s} csrf="c" />);
    expect(html).toContain('<div class="mono order-task">review of o/r#12, round 2</div>');
    expect(html.split("order-task").length - 1).toBe(1);
  });
});

describe("the mission report shows a review's verdict (#187)", () => {
  const report = (verdict: OrderReport["report"]): OrderReport => ({
    order: {
      ...order("ORD-0012", { title: "Review o/r#12 (round 2)", state: "done", kind: "review", task: "campaign/C-0001/task/o/r#12", round: 2, repos: [] }),
      created_at: null,
      done_at: null,
    },
    crew: null,
    beams: [],
    report: verdict,
    pushes: [],
    cap: 0,
    repos: [],
  });

  it("lists the findings under the summary, as text", () => {
    const html = renderToString(
      <ReportView
        report={report({
          summary: "One must go.",
          artifacts: [],
          verdict: { findings: [{ severity: "high", title: hostile, body: hostile, path: "x.go", line: 3 }, { severity: "low", title: "Later", body: "" }] },
        })}
      />,
    );
    expect(html).toContain("<dt>Campaign</dt><dd>review of o/r#12, round 2</dd>");
    expect(html.indexOf("One must go.")).toBeLessThan(html.indexOf(">Verdict<"));
    expect(html).toContain('class="report-verdict blocked">Blocked: 1 high, 1 low<');
    expect(html).toContain("x.go:3");
    expect(html).not.toContain("<script>");
    expect(html.split(escaped).length - 1).toBe(2);
  });

  it("says a pass, and shows no verdict for a work order", () => {
    expect(renderToString(<ReportView report={report({ summary: "ok", artifacts: [], verdict: { findings: [] } })} />)).toContain("Passed: no findings");
    expect(renderToString(<ReportView report={report({ summary: "ok", artifacts: [] })} />)).not.toContain("Verdict");
  });
});
