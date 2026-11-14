import { renderToString } from "preact-render-to-string";
import { afterEach, describe, expect, it, vi } from "vitest";
import { ApiError } from "../src/api.js";
import {
  AwayPanel,
  UsageLine,
  Composer,
  ConfirmAction,
  CountermandPanel,
  CrewPanel,
  HailCard,
  LogPanel,
  OpenOrdersPanel,
  clearPushResult,
  PUSH_RESULT_MS,
  PushAction,
  pushFailed,
  type PushPhase,
  RecordPanel,
  StandingOrdersSwitch,
  Stats,
  TransporterPanel,
} from "../src/panels.js";
import { emptyState, reduce } from "../src/store.js";
import { crew, entry, hail, order, snapshot, status } from "./fixtures.js";

const evil = `<img src=x onerror="alert(1)"> **bold** [link](https://example.com) https://example.com`;

describe("agent text renders as plain text", () => {
  it("a hail question with HTML in it", () => {
    const html = renderToString(
      <HailCard hail={hail("H-0001", { question: evil, context: { note: "<b>ctx</b>" } })} crew={crew("sorell")} order={order("ORD-0001")} csrf="c" />,
    );
    expect(html).not.toContain("<img");
    expect(html).not.toContain("<b>");
    expect(html).not.toContain("<a ");
    expect(html).toContain("&lt;img src=x onerror=");
    expect(html).toContain("**bold** [link](https://example.com) https://example.com");
  });

  it("progress notes, summaries and log text", () => {
    const s = reduce(emptyState, {
      type: "snapshot",
      snapshot: snapshot({
        last_log_id: 3,
        crew: [crew("sorell", { status: "away" })],
        orders: [order("ORD-0001", { state: "away", assignee: "sorell", progress_pct: 40, progress_note: evil })],
        log: [
          entry(1, { kind: "order", order_id: "ORD-0001", text: evil, payload: { seq: 1, pct: 40 } }),
          entry(2, { kind: "done", order_id: "ORD-0002", text: evil, payload: { summary: evil } }),
          entry(3, { kind: "bridge", text: "bad payload row", payload: null }),
        ],
      }),
    });
    const html = renderToString(
      <>
        <AwayPanel state={s} csrf="c" />
        <RecordPanel state={s} csrf="c" />
        <LogPanel state={s} dispatch={() => {}} />
      </>,
    );
    expect(html).not.toContain("<img");
    expect(html).not.toContain("<a ");
    expect(html.split("&lt;img").length - 1).toBe(4); // note, summary, two log rows
    expect(html).toContain("bad payload row");
    expect(html).toContain("40%");
  });
});

describe("Phase 2 controls (#78)", () => {
  const s = reduce(emptyState, {
    type: "snapshot",
    snapshot: snapshot({
      crew: [crew("sorell", { status: "away" }), crew("tovan", { status: "sickbay", name: "Ens. Tovan" }), crew("worf")],
      orders: [
        order("ORD-0001", { state: "away", assignee: "sorell" }),
        order("ORD-0002", { state: "directed", assignee: "worf", division: "ops" }),
        order("ORD-0003", { state: "directed", assignee: "worf" }),
      ],
    }),
  });

  it("are enabled, with no #78 tooltips left", () => {
    const html = renderToString(
      <>
        <CrewPanel state={s} csrf="c" target="pool" onDirect={() => {}} />
        <OpenOrdersPanel state={s} csrf="c" />
        <AwayPanel state={s} csrf="c" />
        <TransporterPanel state={s} csrf="c" />
        <HailCard hail={hail("H-0001")} crew={crew("sorell")} order={order("ORD-0001")} csrf="c" />
      </>,
    );
    expect(html).not.toContain("title=");
    expect(html).not.toContain("#78");
    expect(html).not.toContain("not wired");
    for (const name of [
      "Re-energize beam for Ens. Tovan",
      "Route ORD-0002",
      "Withdraw ORD-0002",
      "Beam SORELL up and return ORD-0001 to the pool",
      "Belay order ORD-0001",
    ]) {
      expect(html).toContain(`aria-label="${name}"`);
    }
  });

  it("a confirm action starts as its one button", () => {
    const html = renderToString(
      <ConfirmAction idBase="x" label="Withdraw" ariaLabel="Withdraw ORD-0002" prompt="Withdraw ORD-0002?" confirmLabel="Withdraw" run={async () => {}} />,
    );
    expect(html).toBe('<button type="button" class="btn ghost" aria-label="Withdraw ORD-0002">Withdraw</button>');
  });

  it("the route picker shows the order's route and offers no one in sickbay", () => {
    const html = renderToString(<OpenOrdersPanel state={s} csrf="c" />);
    expect(html).toMatch(/<option selected value="worf">WORF · No division \(on deck\)<\/option>/);
    expect(html).toMatch(/<option value="tovan" disabled>TOVAN/);
    expect(html).toMatch(/<option value="pool">Open pool/);
    // Nothing to send yet: the Route button is inert, but focusable.
    expect(html).toContain('<button type="button" class="btn ghost" aria-label="Route ORD-0002" aria-disabled="true">Route</button>');
  });

  it("an order without a division can't be routed to a pool", () => {
    const html = renderToString(<OpenOrdersPanel state={s} csrf="c" />);
    const row = html.slice(html.indexOf('id="route-ORD-0003"'));
    expect(row).toMatch(/^[^]*?<option value="pool" disabled>Open pool/);
    const other = html.slice(html.indexOf('id="route-ORD-0002"'), html.indexOf('id="route-ORD-0003"'));
    expect(other).toMatch(/<option value="pool">Open pool/);
  });

  it("the standing orders switch reads the status", () => {
    const on = renderToString(<StandingOrdersSwitch status={status({ standing_orders: true })} csrf="c" />);
    expect(on).toContain('role="switch"');
    expect(on).toContain('aria-checked="true"');
    expect(on).toContain('class="knob on"');
    expect(on).toContain('<span aria-hidden="true"> on</span>');
    expect(on).not.toContain("disabled");
    const off = renderToString(<StandingOrdersSwitch status={status()} csrf="c" />);
    expect(off).toContain('aria-checked="false"');
    expect(off).toContain("Crew only move on directed orders.");
    const unknown = renderToString(<StandingOrdersSwitch status={undefined} csrf="c" />);
    expect(unknown).toContain("disabled");
    expect(unknown).toContain('aria-checked="false"');
  });
});

describe("countermand control (#153)", () => {
  const decided = hail("H-0001", {
    status: "decided",
    decided_by: "command",
    original_decided_by: "command",
    decision: "allow",
    rationale: "deploys are routine",
    confidence: 0.9,
    countermand_until: "2026-10-30T10:05:00Z",
  });
  const s = reduce(emptyState, { type: "snapshot", snapshot: snapshot({ hails: [decided, hail("H-0002")], orders: [order("ORD-0001")] }) });

  it("shows only inside the window", () => {
    const inside = renderToString(<CountermandPanel state={s} csrf="c" now={new Date("2026-10-30T10:04:00Z")} />);
    expect(inside).toContain("Decided without you");
    expect(inside).toContain('aria-label="Countermand H-0001 with deny"');
    expect(inside).not.toContain("Countermand H-0001 with allow"); // the current decision
    expect(inside).not.toContain("H-0002"); // open: the hails panel has it
    expect(inside).toContain("Decided by Command: ");
    expect(inside).toContain("(confidence 90%)");
    expect(inside).toContain("deploys are routine");
    expect(renderToString(<CountermandPanel state={s} csrf="c" now={new Date("2026-10-30T10:05:01Z")} />)).toBe("");
  });

  it("says when the order has no live run, and not while it is away", () => {
    const now = new Date("2026-10-30T10:04:00Z");
    const done = renderToString(<CountermandPanel state={s} csrf="c" now={now} />); // ORD-0001 is in the pool here
    expect(done).toContain("No live run for ORD-0001");
    const away = reduce(emptyState, {
      type: "snapshot",
      snapshot: snapshot({ hails: [decided], orders: [order("ORD-0001", { state: "away", assignee: "sorell" })] }),
    });
    expect(renderToString(<CountermandPanel state={away} csrf="c" now={now} />)).not.toContain("No live run");
  });

  it("says a parked order is told on resume (#167)", () => {
    const now = new Date("2026-10-30T10:04:00Z");
    const parkedAt = { parked_at: "2026-10-30T10:02:00Z", assignee: "sorell" };
    const parked = reduce(emptyState, {
      type: "snapshot",
      snapshot: snapshot({ hails: [decided], orders: [order("ORD-0001", { state: "away", ...parkedAt })] }),
    });
    const html = renderToString(<CountermandPanel state={parked} csrf="c" now={now} />);
    expect(html).toContain("ORD-0001 is parked: the agent is told on resume.");
    expect(html).not.toContain("No live run");
    const recalled = reduce(emptyState, {
      type: "snapshot",
      snapshot: snapshot({ hails: [decided], orders: [order("ORD-0001", { state: "pool", parked_at: "2026-10-30T10:02:00Z" })] }),
    });
    const r = renderToString(<CountermandPanel state={recalled} csrf="c" now={now} />);
    expect(r).toMatch(/ORD-0001 is recalled: the agent is told only if .+ resumes it\./);
    expect(r).not.toContain("No live run");
  });

  it("doesn't promise the resume when the order is parked on another crew member (#167)", () => {
    // sorell hailed; the order was recalled, resumed on tuvok, and parked there.
    const elsewhere = reduce(emptyState, {
      type: "snapshot",
      snapshot: snapshot({
        hails: [decided],
        orders: [order("ORD-0001", { state: "away", assignee: "tuvok", parked_at: "2026-10-30T10:02:00Z" })],
      }),
    });
    const html = renderToString(<CountermandPanel state={elsewhere} csrf="c" now={new Date("2026-10-30T10:04:00Z")} />);
    expect(html).toMatch(/ORD-0001 is parked on .+: the agent is told only if .+ resumes it\./);
    expect(html).not.toContain("the agent is told on resume");
    expect(html).not.toContain("No live run");
  });

  it("asks without naming who decided, which reads badly after a countermand", () => {
    const again = hail("H-0001", { ...decided, decided_by: "captain", countermanded_at: "2026-10-30T10:01:00Z", decision: "deny" });
    const st = reduce(emptyState, { type: "snapshot", snapshot: snapshot({ hails: [again] }) });
    const html = renderToString(<CountermandPanel state={st} csrf="c" now={new Date("2026-10-30T10:04:00Z")} />);
    expect(html).toContain("Decided by you (countermanded): ");
    expect(html).not.toContain("you (countermanded)&#39;s");
    expect(html).toContain('aria-label="Countermand H-0001 with allow"');
  });

  it("offers a decision field on a free-text hail, its button inert until filled", () => {
    const text = hail("H-0003", { ...decided, id: "H-0003", options: [], decision: "use the bridge" });
    const st = reduce(emptyState, { type: "snapshot", snapshot: snapshot({ hails: [text] }) });
    const html = renderToString(<CountermandPanel state={st} csrf="c" now={new Date("2026-10-30T10:00:00Z")} />);
    expect(html).toContain('placeholder="Your decision"');
    expect(html).toMatch(/<button type="button" class="btn ghost" aria-label="Countermand H-0003" disabled>Countermand<\/button>/);
  });
});

describe("split orders (#155)", () => {
  it("the mission record shows a split order waiting on its open children", () => {
    const s = reduce(emptyState, {
      type: "snapshot",
      snapshot: snapshot({
        last_log_id: 1,
        orders: [order("ORD-0001", { title: "Split the RFC", state: "split", assignee: "vance", open_children: 2 })],
        log: [entry(1, { kind: "order", order_id: "ORD-0001", crew_id: "vance", text: "order split into 2 children: ORD-0002, ORD-0003" })],
      }),
    });
    const html = renderToString(<RecordPanel state={s} csrf="c" />);
    expect(html).toContain("Split the RFC");
    expect(html).toContain(`<span class="chip bg-blue on-blue">Split: waiting on 2 children</span>`);
    // #146: a split order can be belayed from its row, cascading.
    expect(html).toContain('aria-label="Belay ORD-0001 and its open child orders"');
  });

  it("only a split order's record row has a belay control (#146)", () => {
    const s = reduce(emptyState, {
      type: "snapshot",
      snapshot: snapshot({
        last_log_id: 1,
        orders: [order("ORD-0001", { state: "done", assignee: "vance" })],
        log: [entry(1, { kind: "done", order_id: "ORD-0001", crew_id: "vance", text: "done" })],
      }),
    });
    expect(renderToString(<RecordPanel state={s} csrf="c" />)).not.toContain("Belay");
  });
});

describe("order form repos (#211)", () => {
  const form = (repos: string[]) =>
    renderToString(<Composer state={reduce(emptyState, { type: "snapshot", snapshot: snapshot({ repos }) })} csrf="c" target="pool" onTarget={() => {}} />);

  it("with no repos, hides repo and base ref, disables Engage and says how to register one", () => {
    const html = form([]);
    expect(html).not.toContain('id="order-repo"');
    expect(html).not.toContain('id="order-ref"');
    expect(html).toMatch(/<button type="submit" class="btn gold engage" disabled aria-describedby="order-no-repos">/);
    expect(html).toContain('<p id="order-no-repos" class="hint grow-220">');
    expect(html).toContain("This bridge has no repos");
    expect(html).toContain("--repo &lt;name>=&lt;url>");
  });

  it("with one repo, a dropdown with it preselected", () => {
    const html = form(["app"]);
    expect(html).toContain('<label for="order-repo" class="lbl">Repo</label><select id="order-repo" class="field">');
    expect(html).toContain('<option selected value="app">app</option>');
    expect(html).not.toContain('type="text" list=');
    expect(html).toContain('<input id="order-ref" class="field" type="text" autocomplete="off" value="main"/>');
    expect(html).not.toContain("order-no-repos");
    expect(html).toMatch(/<button type="submit" class="btn gold engage">/);
  });

  it("with several repos, a dropdown of each, the first selected", () => {
    const html = form(["app", "lib", "web"]);
    expect(html).toContain('<option selected value="app">app</option><option value="lib">lib</option><option value="web">web</option>');
    expect(html).not.toContain("<datalist");
    expect(html).toContain('id="order-ref"');
  });
});

describe("push (#4)", () => {
  const record = (state: string, repos = true) =>
    reduce(emptyState, {
      type: "snapshot",
      snapshot: snapshot({
        last_log_id: 2,
        orders: [order("ORD-0007", { state, assignee: "vance", ...(repos ? {} : { repos: [] }) })],
        log: [
          entry(1, { kind: state === "done" ? "done" : "fail", order_id: "ORD-0007", crew_id: "vance", text: "over" }),
          entry(2, { kind: "order", order_id: "ORD-0007", text: "repo app pushed to bmus/ORD-0007 at 0123456789ab" }),
        ],
        repo_urls: { app: "https://example.com/o/app.git" },
      }),
    });

  it("a done order's record row offers Push and Mission report, and shows where it was pushed", () => {
    const html = renderToString(<RecordPanel state={record("done")} csrf="c" />);
    expect(html).toContain(`aria-label="Push ORD-0007's work to its remote"`);
    expect(html).toContain(
      '<a class="btn ghost small" href="/report/ORD-0007" target="_blank" rel="noopener noreferrer" aria-label="Open the mission report of ORD-0007">Mission report</a>',
    );
    expect(html).not.toContain("Copy diff command");
    expect(html).toContain("pushed to bmus/ORD-0007");
    expect(html).not.toContain("Force push");
  });

  it("no other state's row offers them", () => {
    for (const state of ["failed", "belayed", "withdrawn", "split"]) {
      const html = renderToString(<RecordPanel state={record(state)} csrf="c" />);
      expect(html, state).not.toContain(">Push</button>");
      expect(html, state).not.toContain("Mission report");
      expect(html, state).not.toContain("pushed to");
    }
  });

  it("a row is the id and text on top, then a footer: crew, status and the pushed marker on the left, the actions on the right", () => {
    const html = renderToString(<RecordPanel state={record("done")} csrf="c" />);
    expect(html).toMatch(
      /<div class="record-row"><div class="record-main"><span class="mono order-id">ORD-0007<\/span><span class="record-title">[^<]*<span class="record-sum">over<\/span><\/span><\/div><div class="record-foot"><div class="record-who"><span class="record-crew">VANCE<\/span><span class="chip bg-green on-green">Complete<\/span><span class="mono pushed-to">pushed to bmus\/ORD-0007<\/span><\/div><div class="record-acts">/,
    );
    const acts = html.slice(html.indexOf('<div class="record-acts">'));
    expect(acts).toMatch(/<div class="record-btns"><button type="button" class="btn gold small"[^>]*>Push<\/button><a class="btn ghost small"/);
    expect(acts).toContain('<button type="button" class="btn gold small" aria-label="Push ORD-0007\'s work to its remote">Push</button>');
    expect(acts).toContain('<a class="btn ghost small" href="/report/ORD-0007"');
    for (const state of ["failed", "belayed"]) {
      const other = renderToString(<RecordPanel state={record(state)} csrf="c" />);
      expect(other, state).toContain('<div class="record-foot"><div class="record-who"><span class="record-crew">VANCE</span>');
      expect(other, state).not.toContain("record-acts");
    }
  });

  it("a done order without repos offers neither", () => {
    const html = renderToString(<RecordPanel state={record("done", false)} csrf="c" />);
    expect(html).not.toContain(">Push</button>");
    expect(html).not.toContain("Mission report");
  });

  const remotes = [{ name: "app", url: "git@github.com:o/app.git" }];
  const run = () => Promise.resolve([]);

  it("its confirm names the remote and the branch, with a target branch field defaulting to bmus/<order>", () => {
    const html = renderToString(<PushAction orderId="ORD-0007" remotes={remotes} run={run} initial={{ kind: "confirm" }} />);
    expect(html).toContain('role="group" aria-labelledby="push-ORD-0007-q"');
    expect(html).toContain('<span id="push-ORD-0007-q" class="confirm-q">Push bmus/ORD-0007 to git@github.com:o/app.git as bmus/ORD-0007?</span>');
    expect(html).toContain('<label class="inline-lbl" for="push-ORD-0007-target">Target branch</label>');
    expect(html).toContain('id="push-ORD-0007-target" class="field push-target" value="bmus/ORD-0007"');
    expect(html).not.toContain("Force push");
  });

  it("shows a pending state while the push runs", () => {
    const html = renderToString(<PushAction orderId="ORD-0007" remotes={remotes} run={run} initial={{ kind: "pending", force: false }} />);
    expect(html).toContain('aria-busy="true"');
    expect(html).toMatch(/<button type="button" class="btn red" disabled>Pushing<\/button>/);
  });

  it("a 409 offers Force push; any other failure doesn't", () => {
    expect(pushFailed(new ApiError(409, "conflict", "push order failed: conflict"))).toEqual({
      phase: { kind: "rejected", message: "push order failed: conflict" },
      error: "",
    });
    expect(pushFailed(new ApiError(400, "bad_request", "order ORD-0007 is away"))).toEqual({ phase: { kind: "idle" }, error: "order ORD-0007 is away" });
    expect(pushFailed(new ApiError(504, "timeout", "push timed out"))).toEqual({ phase: { kind: "idle" }, error: "push timed out" });

    const rejected = renderToString(<PushAction orderId="ORD-0007" remotes={remotes} run={run} initial={{ kind: "rejected", message: "push order failed: conflict" }} />);
    expect(rejected).toContain('role="alert"');
    expect(rejected).toContain("The remote rejected the push: bmus/ORD-0007 has likely diverged");
    expect(rejected).toContain('class="btn ghost small" aria-label="Force push ORD-0007 to bmus/ORD-0007">Force push</button>');
    expect(rejected).not.toContain("are overwritten");
  });

  it("Force push asks again, saying the remote branch's commits are overwritten", () => {
    const html = renderToString(<PushAction orderId="ORD-0007" remotes={remotes} run={run} initial={{ kind: "force", message: "conflict" }} />);
    expect(html).toContain('role="group" aria-labelledby="push-ORD-0007-q-force"');
    expect(html).toContain(
      "Force push bmus/ORD-0007 to git@github.com:o/app.git as bmus/ORD-0007? The remote branch's commits that bmus/ORD-0007 doesn't have are overwritten.",
    );
    expect(html).toMatch(/<button type="button" class="btn red">Force push<\/button>/);
  });

  it("after success, each repo's branch and commit, and a pull request link for github.com only", () => {
    const done = renderToString(
      <PushAction
        orderId="ORD-0007"
        remotes={remotes}
        run={run}
        initial={{
          kind: "done",
          repos: [
            { name: "app", url: "git@github.com:o/app.git", branch: "bmus/ORD-0007", commit: "0123456789abcdef" },
            { name: "lib", url: "https://example.com/o/lib.git", branch: "bmus/ORD-0007", commit: "fedcba9876543210" },
          ],
        }}
      />,
    );
    expect(done).toContain('role="status"');
    expect(done).toContain('<li><span class="mono">app: bmus/ORD-0007 at 0123456789ab</span><span class="mono muted-id push-remote">git@github.com:o/app.git</span>');
    expect(done).toContain('<li><span class="mono">lib: bmus/ORD-0007 at fedcba987654</span><span class="mono muted-id push-remote">https://example.com/o/lib.git</span></li>');
    expect(done).toContain('<a href="https://github.com/o/app/compare/bmus/ORD-0007?expand=1" target="_blank" rel="noopener noreferrer">Open a pull request</a>');
    expect(done.split("Open a pull request").length - 1).toBe(1);
  });

  describe("the result clears itself after 15s (#139)", () => {
    afterEach(() => {
      vi.useRealTimers();
    });
    const done: PushPhase = { kind: "done", repos: [{ name: "app", url: "", branch: "bmus/ORD-0007", commit: "0123456789abcdef" }] };

    it("a push's result shows, is still there at 14.9s and gone at 15s", () => {
      vi.useFakeTimers();
      expect(PUSH_RESULT_MS).toBe(15_000);
      expect(renderToString(<PushAction orderId="ORD-0007" remotes={remotes} run={run} initial={done} />)).toContain("Pushed.");
      const clear = vi.fn();
      clearPushResult(done, clear);
      vi.advanceTimersByTime(14_900);
      expect(clear).not.toHaveBeenCalled();
      vi.advanceTimersByTime(100);
      expect(clear).toHaveBeenCalledTimes(1);
    });

    it("a new push restarts it, and the cleanup stops it", () => {
      vi.useFakeTimers();
      const clear = vi.fn();
      const stop = clearPushResult(done, clear);
      vi.advanceTimersByTime(10_000);
      stop();
      const stopAgain = clearPushResult({ ...done }, clear);
      vi.advanceTimersByTime(14_900);
      expect(clear).not.toHaveBeenCalled();
      vi.advanceTimersByTime(100);
      expect(clear).toHaveBeenCalledTimes(1);
      const stopped = vi.fn();
      clearPushResult(done, stopped)();
      stopAgain();
      vi.advanceTimersByTime(60_000);
      expect(stopped).not.toHaveBeenCalled();
    });

    it("a rejection or another phase doesn't clear itself", () => {
      vi.useFakeTimers();
      const clear = vi.fn();
      for (const phase of [{ kind: "rejected", message: "conflict" }, { kind: "force", message: "conflict" }, { kind: "idle" }, { kind: "confirm" }] as PushPhase[]) {
        clearPushResult(phase, clear);
      }
      vi.advanceTimersByTime(60_000);
      expect(clear).not.toHaveBeenCalled();
    });
  });
});

describe("beaming in (#106)", () => {
  const s = reduce(emptyState, {
    type: "snapshot",
    snapshot: snapshot({
      crew: [crew("sorell", { status: "beaming_in" }), crew("worf", { status: "beaming_in", current_beam: "bm-2" })],
      status: status({ crew: { beaming_in: 2, on_deck: 1, away: 0, hailing: 0, sickbay: 0 } }),
    }),
  });

  it("the crew card shows the chip and the beam as provisioning while there is none", () => {
    const html = renderToString(<CrewPanel state={s} csrf="c" target="pool" onDirect={() => {}} />);
    expect(html).toContain('<div class="crew-status tone-cream"><span class="light bg-cream blink" aria-hidden="true"></span>Beaming in</div>');
    expect(html).toContain("BEAM provisioning");
    expect(html).toContain("BEAM bm-2");
    expect(html).toContain("no orders until its combadge says hello");
  });

  it("the transporter pad reads provisioning", () => {
    const html = renderToString(<TransporterPanel state={s} csrf="c" />);
    expect(html.split('class="pad pad-beaming"').length - 1).toBe(2);
    expect(html).toContain('<div class="mono pad-lbl tone-cream">provisioning</div>');
  });

  it("the counters count it, and crew aboard includes it", () => {
    const html = renderToString(<Stats state={s} />);
    expect(html).toMatch(/Beaming in<\/span><\/div><span class="mono stat-val">2<\/span>/);
    expect(html).toMatch(/Crew aboard<\/span><\/div><span class="mono stat-val">3<\/span>/);
  });
});

describe("usage against budget on the away-missions card (#120)", () => {
  const s = reduce(emptyState, {
    type: "snapshot",
    snapshot: snapshot({
      crew: [crew("sorell", { status: "away" }), crew("worf", { status: "away" })],
      orders: [
        order("ORD-0001", {
          state: "away",
          assignee: "sorell",
          limit_tokens: 1_000_000,
          limit_turns: 150,
          budget_source: { tokens: "order", turns: "division" },
          usage: { input_tokens: 800_000, output_tokens: 20_000, turns: 42 },
        }),
        order("ORD-0002", { state: "away", assignee: "worf", usage: { input_tokens: 400_000, output_tokens: 20_000, turns: 9 } }),
      ],
    }),
  });
  const html = renderToString(<AwayPanel state={s} csrf="c" />);
  const card = (id: string) => html.slice(html.indexOf(`aria-label="${id} usage"`)).split('class="row-actions"')[0];

  it("shows each limited field against its limit, with a meter", () => {
    const c = card("ORD-0001");
    expect(c).toContain(">Budget<");
    expect(c).toContain("820k / 1M tokens");
    expect(c).toContain("42 / 150 turns");
    expect(c).toContain('class="mono usage-text tone-gold">820k / 1M tokens');
    expect(c).toContain('class="mono usage-text tone-green">42 / 150 turns');
    expect(c).toContain('role="meter" aria-label="ORD-0001 tokens used" aria-valuemin="0" aria-valuemax="1000000" aria-valuenow="820000"');
    expect(c).toContain('aria-label="ORD-0001 turns used"');
  });

  it("shows the usage alone without a budget", () => {
    const c = card("ORD-0002");
    expect(c).toContain(">No budget<");
    expect(c).toContain('class="mono usage-text tone-grey">420k tokens, 9 turns<');
    expect(c).not.toContain('role="meter"');
  });

  it("caps an overspent meter and turns it red", () => {
    const over = renderToString(
      <UsageLine order={order("ORD-0003", { limit_tokens: 1000, usage: { input_tokens: 1500, output_tokens: 0, turns: 3 } })} />,
    );
    expect(over).toContain('class="mono usage-text tone-red">1.5k / 1k tokens (over)<');
    expect(over).toContain('aria-valuenow="1000" aria-valuetext="1.5k / 1k tokens (over)"');
    expect(over).toContain('class="fill bg-red" style="width:100%;"');
    expect(over).toContain('class="mono usage-text tone-grey">3 turns');
  });
});

describe("order dependencies on the open-orders card (#142)", () => {
  it("shows a waiting order as waiting on its after, next to a normal one", () => {
    const s = reduce(emptyState, {
      type: "snapshot",
      snapshot: snapshot({
        crew: [crew("worf")],
        orders: [
          order("ORD-0001", { division: "ops" }),
          order("ORD-0002", { division: "ops", after: ["ORD-0001"], waiting_on: ["ORD-0001"] }),
          order("ORD-0003", { state: "directed", assignee: "worf", after: ["ORD-0001", "ORD-0002"], waiting_on: ["ORD-0001", "ORD-0002"] }),
        ],
      }),
    });
    const html = renderToString(<OpenOrdersPanel state={s} csrf="c" />);
    const rows = html.split('<div class="row"').slice(1);
    expect(rows).toHaveLength(3);
    expect(rows[0]).toContain('<span class="row-state">In the pool</span>');
    expect(rows[1]).toContain('<span class="row-state">In the pool, waiting on ORD-0001</span>');
    expect(rows[2]).toContain('<span class="row-state">Queued for WORF, waiting on ORD-0001, ORD-0002</span>');
  });

  it("drops the wait once the feed says its after is done", () => {
    let s = reduce(emptyState, {
      type: "snapshot",
      snapshot: snapshot({ orders: [order("ORD-0002", { division: "ops", after: ["ORD-0001"], waiting_on: ["ORD-0001"] })] }),
    });
    expect(renderToString(<OpenOrdersPanel state={s} csrf="c" />)).toContain("waiting on ORD-0001");
    s = reduce(s, { type: "order", view: order("ORD-0002", { division: "ops", after: ["ORD-0001"], waiting_on: [] }) });
    const html = renderToString(<OpenOrdersPanel state={s} csrf="c" />);
    expect(html).not.toContain("waiting on");
    expect(html).toContain('<span class="row-state">In the pool</span>');
  });
});
