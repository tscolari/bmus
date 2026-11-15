import { renderToString } from "preact-render-to-string";
import { describe, expect, it } from "vitest";
import type { HailView, ReviewContext } from "../src/api.js";
import { hailNotice, notifies } from "../src/notify.js";
import { HailCard, HailsPanel, ReviewCard, TriageCard } from "../src/panels.js";
import { emptyState, reduce } from "../src/store.js";
import {
  bridgeHailOrder,
  discardAllPrompt,
  hailKind,
  issueURL,
  reviewAllDecision,
  reviewContext,
  reviewDecision,
  reviewSummary,
  triageRationale,
} from "../src/view.js";
import { crew, hail, order, snapshot } from "./fixtures.js";

const hostile = `<script>alert(1)</script> **bold** [link](https://example.com) <img src=x onerror="alert(1)">`;

const context: ReviewContext = {
  order: "ORD-0001",
  issues: [
    { ref: "acme/app#2", key: "a", title: "Snapshot writer leaks a handle" },
    { ref: "acme/app#3", key: "b", title: hostile },
    { ref: "acme/app#4", key: "c", title: "Compactor holds the lock" },
  ],
  dropped: [
    { key: "noise", title: "Typo in a comment", why: hostile },
    { key: "dup", title: "TestSnapshot is flaky", why: hostile, duplicate_of: "acme/app#1" },
  ],
  failed: [{ key: "lost", title: "Lost finding", error: "github: POST /repos/acme/app/issues: 502 Bad Gateway" }],
};

function bridgeHail(id: string, over: Partial<HailView> = {}): HailView {
  return hail(id, {
    order_id: "",
    crew_id: "",
    deliver_to: "findings/ORD-0001",
    category: "tracker_review",
    question: "Review 3 issues filed from ORD-0001's findings (2 dropped, 1 failed): keep, discard, or answer with the refs to discard and the keys to restore.",
    options: ["keep", "discard"],
    context,
    route: "captain",
    ...over,
  });
}

const review = bridgeHail("H-0007");
const triage = bridgeHail("H-0006", { category: "findings_triage", options: [], question: "Triage 5 findings of ORD-0001?", context: { order: "ORD-0001", title: "Fix the snapshots" }, route: "command" });

describe("ReviewCard (#157)", () => {
  const html = renderToString(<ReviewCard hail={review} repoUrls={{ app: "git@github.com:acme/app.git" }} csrf="c" />);

  it("names the bridge and the order whose findings it reviews, with the question", () => {
    expect(html).toContain("H-0007 · Bridge · findings of ORD-0001");
    expect(html).toContain("Review 3 issues filed from ORD-0001's findings");
  });

  it("lists each issue with a labelled Discard box, and each dropped or failed finding with a Restore box and its reason", () => {
    expect(html).toContain("Filed: 3 issues");
    expect(html).toContain("Not filed: 3");
    expect(html.split('type="checkbox"').length - 1).toBe(6);
    for (const [i, ref] of ["acme/app#2", "acme/app#3", "acme/app#4"].entries()) {
      expect(html).toContain(`<label for="hail-H-0007-d${i}" class="review-verb">Discard<span class="sr-only"> ${ref}</span></label>`);
    }
    for (const [i, key] of ["noise", "dup", "lost"].entries()) {
      expect(html).toContain(`<label for="hail-H-0007-r${i}" class="review-verb">Restore<span class="sr-only"> ${key}</span></label>`);
    }
    expect(html).toContain(">Dropped by Command<");
    expect(html).toContain("Failed: github: POST /repos/acme/app/issues: 502 Bad Gateway");
    expect(html).toContain("Dropped: duplicate of ");
    // Command's rationale, the same on both dropped rows, shows once.
    expect(html.split("Command: ").length - 1).toBe(1);
    // Rows are numbered on through the lists, as bmus hails review numbers them.
    expect([...html.matchAll(/<span class="review-n mono" aria-hidden="true">(\d+)<\/span>/g)].map((m) => m[1])).toEqual(["1", "2", "3", "4", "5", "6"]);
    // Nothing ticked: Apply keeps everything.
    expect(html).toContain("Apply keeps 3, discards 0, restores 0");
  });

  it("offers Keep all and Apply, and Discard all only as a confirm's first step", () => {
    const buttons = [...html.matchAll(/<button[^>]*>([^<]*)<\/button>/g)].map((m) => m[0]);
    expect(buttons).toHaveLength(3);
    expect(buttons[0]).toContain(">Keep all<");
    expect(buttons[1]).toContain(">Apply<");
    expect(buttons[1]).toContain('aria-describedby="hail-H-0007-sum"');
    // ConfirmAction's ask button: it sends nothing, its click shows the prompt.
    expect(buttons[2]).toContain('class="btn ghost"');
    expect(buttons[2]).toContain('aria-label="Discard all 3 issues from H-0007"');
    expect(buttons[2]).toContain(">Discard all<");
    expect(html).not.toContain("Close all 3 issues as not planned?");
    expect(html).toContain('role="status" aria-live="polite"');
  });

  it("has no Discard all without issues", () => {
    const none = renderToString(<ReviewCard hail={bridgeHail("H-0009", { context: { ...context, issues: [] } })} repoUrls={{}} csrf="c" />);
    expect(none).not.toContain("Discard all");
    expect(none).not.toContain("Filed:");
    expect(none).toContain("Keep all");
  });

  it("renders titles and reasons as text, never HTML or markdown", () => {
    expect(html).not.toContain("<script");
    expect(html).not.toContain("<img");
    expect(html).toContain("&lt;script>alert(1)&lt;/script> **bold** [link](https://example.com) &lt;img src=x onerror=");
    // Two places: the issue's title and Command's rationale.
    expect(html.split("&lt;script>").length - 1).toBe(2);
    expect(html).not.toContain('href="https://example.com"');
  });

  it("links a ref to its issue only for a registered github.com repo", () => {
    expect(html).toContain('<a class="mono" href="https://github.com/acme/app/issues/2" target="_blank" rel="noopener noreferrer">acme/app#2</a>');
    expect(html).toContain('href="https://github.com/acme/app/issues/1"');
    const plain = renderToString(<ReviewCard hail={review} repoUrls={{ app: "/srv/git/app.git" }} csrf="c" />);
    expect(plain).not.toContain("<a ");
    expect(plain).toContain('<span class="mono">acme/app#2</span>');
  });
});

describe("reviewDecision", () => {
  const rc = context;
  const set = (...v: string[]) => new Set(v);

  it("is keep with nothing ticked, never a bare discard", () => {
    expect(reviewDecision(rc, set(), set())).toBe("keep");
    expect(reviewSummary(rc, set(), set())).toBe("Apply keeps 3, discards 0, restores 0");
  });

  it("names the refs ticked in the context's order", () => {
    expect(reviewDecision(rc, set("acme/app#4", "acme/app#2"), set())).toBe("discard acme/app#2 acme/app#4");
  });

  it("names every ref when every issue is ticked, so it can't reach an issue the card didn't show", () => {
    expect(reviewDecision(rc, set("acme/app#4", "acme/app#3", "acme/app#2"), set())).toBe("discard acme/app#2 acme/app#3 acme/app#4");
  });

  it("adds the keys to restore in the context's order, dropped then failed, after keep or discard", () => {
    expect(reviewDecision(rc, set(), set("lost", "noise"))).toBe("keep; restore noise lost");
    expect(reviewDecision(rc, set("acme/app#3"), set("dup"))).toBe("discard acme/app#3; restore dup");
    expect(reviewSummary(rc, set("acme/app#3"), set("dup"))).toBe("Apply keeps 2, discards 1, restores 1");
  });

  it("Keep all and Discard all carry the restores ticked, and are their option alone without any", () => {
    expect(reviewAllDecision(rc, "keep", set())).toBeUndefined();
    expect(reviewAllDecision(rc, "discard", set())).toBeUndefined();
    expect(reviewAllDecision(rc, "keep", set("lost", "dup"))).toBe("keep; restore dup lost");
    expect(reviewAllDecision(rc, "discard", set("noise"))).toBe("discard; restore noise");
    expect(discardAllPrompt(rc, set())).toBe("Close all 3 issues as not planned?");
    expect(discardAllPrompt(rc, set("lost", "noise"))).toBe("Close all 3 issues as not planned and restore noise, lost?");
  });

  it("shows Command's rationale once", () => {
    expect(triageRationale(rc)).toEqual([hostile]);
    expect(triageRationale({ ...rc, dropped: [{ key: "x", title: "", why: "" }] })).toEqual([]);
  });

  it("ignores values the review doesn't hold", () => {
    expect(reviewDecision(rc, set("acme/app#9"), set("ghost"))).toBe("keep");
    expect(reviewDecision({ ...rc, issues: [] }, set("acme/app#2"), set())).toBe("keep");
  });
});

describe("reviewContext", () => {
  it("reads a context leniently: lists missing or malformed are empty, entries without a ref or key left out", () => {
    const rc = reviewContext(bridgeHail("H-0001", { context: { order: "ORD-0002", issues: [{ ref: "#1", title: 5 }, { key: "x" }, "junk"], dropped: null } }));
    expect(rc).toEqual({ order: "ORD-0002", issues: [{ ref: "#1", key: "", title: "" }], dropped: [], failed: [] });
    expect(reviewContext(bridgeHail("H-0001", { context: null }))).toEqual({ order: "ORD-0001", issues: [], dropped: [], failed: [] });
  });
});

describe("issueURL", () => {
  const urls = { app: "https://github.com/acme/app.git", lib: "git@example.com:acme/lib.git" };
  it("links owner/name#N of a registered github.com repo, in any case", () => {
    expect(issueURL("acme/app#12", urls)).toBe("https://github.com/acme/app/issues/12");
    expect(issueURL("Acme/App#12", urls)).toBe("https://github.com/Acme/App/issues/12");
  });
  it("leaves anything else as text", () => {
    expect(issueURL("#12", urls)).toBeUndefined();
    expect(issueURL("acme/other#12", urls)).toBeUndefined();
    expect(issueURL("acme/lib#12", urls)).toBeUndefined();
    expect(issueURL("acme/app#12 x", urls)).toBeUndefined();
    expect(issueURL("acme/app#0", urls)).toBeUndefined();
    expect(issueURL("javascript:alert(1)//acme/app#1", urls)).toBeUndefined();
    // A dot owner or name is no GitHub repo.
    expect(issueURL("./app#1", { a: "git@github.com:./app.git" })).toBeUndefined();
    expect(issueURL("acme/..#1", { a: "git@github.com:acme/...git" })).toBeUndefined();
  });
});

describe("Command's triage (#157)", () => {
  it("shows what Command is doing, with no controls", () => {
    const html = renderToString(<TriageCard hail={triage} />);
    expect(html).toContain("Command is triaging ORD-0001's findings");
    expect(html).toContain("H-0006 · Bridge · findings of ORD-0001");
    expect(html).not.toContain("<button");
    expect(html).not.toContain("<input");
    expect(html).not.toContain("Triage 5 findings");
  });

  it("raises no notification: the captain has nothing to answer", () => {
    expect(notifies(triage)).toBe(false);
    expect(notifies(review)).toBe(true);
  });
});

describe("bridge hails on the hails panel (#157)", () => {
  const generic = bridgeHail("H-0005", { category: "budget", context: { order: "ORD-0003" }, options: ["extend", "stop"], question: "Extend?" });
  const s = reduce(emptyState, {
    type: "snapshot",
    snapshot: snapshot({
      crew: [crew("sorell", { name: "Lt. Sorell" })],
      orders: [order("ORD-0002", { state: "hailing", assignee: "sorell" })],
      hails: [generic, triage, review, hail("H-0008", { order_id: "ORD-0002", crew_id: "sorell" })],
    }),
  });
  const html = renderToString(<HailsPanel state={s} csrf="c" />);

  it("picks each hail's card", () => {
    expect(hailKind(generic)).toBe("bridge");
    expect(hailKind(triage)).toBe("triage");
    expect(hailKind(review)).toBe("review");
    expect(hailKind(s.hail.get("H-0008")!)).toBe("crew");
    expect(html).toContain("Command is triaging ORD-0001's findings");
    expect(html).toContain("Keep all");
    // Only the crew member's hail can belay its order.
    expect(html.split(">Belay order<").length - 1).toBe(1);
    expect(html).toContain('aria-label="Belay order ORD-0002"');
  });

  it("a bridge hail's card has no belay, and its meta line names the bridge and the order", () => {
    const card = renderToString(<HailCard hail={generic} crew={undefined} order={undefined} csrf="c" />);
    expect(card).not.toContain("Belay");
    expect(card).toContain("H-0005 · Bridge · ORD-0003");
    expect(card).toContain(">extend<");
    const bare = renderToString(<HailCard hail={bridgeHail("H-0004", { category: "budget", deliver_to: "other/x", context: null })} crew={undefined} order={undefined} csrf="c" />);
    expect(bare).toContain(">H-0004 · Bridge<");
  });

  it("a crew member's hail keeps its belay", () => {
    const card = renderToString(<HailCard hail={hail("H-0001")} crew={crew("sorell")} order={order("ORD-0001")} csrf="c" />);
    expect(card).toContain("Belay order");
    expect(card).toContain("H-0001 · SORELL · title of ORD-0001");
  });

  it("the order comes from the context, else a findings/<order> deliver_to", () => {
    expect(bridgeHailOrder(bridgeHail("H-0001", { context: null }))).toBe("ORD-0001");
    expect(bridgeHailOrder(bridgeHail("H-0001", { context: null, deliver_to: "raiser/1" }))).toBe("");
  });

  it("notifications name the bridge and the order", () => {
    expect(hailNotice(s, review)).toEqual({ title: "Review from the bridge · ORD-0001", body: review.question, tag: "H-0007" });
    expect(hailNotice(s, generic).title).toBe("Hail from the bridge · ORD-0003");
    expect(hailNotice(s, bridgeHail("H-0004", { category: "budget", deliver_to: "x", context: null })).title).toBe("Hail from the bridge");
  });
});
