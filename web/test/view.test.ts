import { describe, expect, it } from "vitest";
import type { OrderView } from "../src/api.js";
import { emptyState, reduce, type State } from "../src/store.js";
import {
  awayOrders,
  belayPrompt,
  canCountermand,
  canPush,
  diffPath,
  forcePushPrompt,
  githubCompareURL,
  canTake,
  clockHM,
  conditionInfo,
  contextText,
  countermandableHails,
  crewOptionLabel,
  crewStatusInfo,
  currentOrder,
  decidedByLabel,
  divisionInfo,
  composerRepo,
  knownDivisions,
  logTag,
  logTime,
  manifest,
  missionRecord,
  openHails,
  openOrders,
  orderProgress,
  padInfo,
  priorityInfo,
  progressPct,
  pushedTo,
  pushPrompt,
  pushRemotes,
  queuedFor,
  routeArgs,
  routeValue,
  stardate,
  standingOrdersInfo,
  summaryOf,
} from "../src/view.js";
import { crew, entry, hail, order, snapshot, status } from "./fixtures.js";

const stateOf = (snap: Parameters<typeof snapshot>[0]): State => reduce(emptyState, { type: "snapshot", snapshot: snapshot(snap) });

describe("formatters", () => {
  it("stardate is the local year and day of the year", () => {
    expect(stardate(new Date(2026, 0, 1, 0, 0))).toBe("2026.001");
    expect(stardate(new Date(2026, 9, 28, 23, 59))).toBe("2026.301");
    expect(stardate(new Date(2024, 11, 31, 12, 0))).toBe("2024.366");
  });

  it("clock and log times are local", () => {
    expect(clockHM(new Date(2026, 9, 28, 9, 5, 7))).toBe("09:05");
    expect(logTime(new Date(2026, 9, 28, 9, 5, 7).toISOString())).toBe("09:05:07");
    expect(logTime("not a time")).toBe("--:--:--");
  });

  it("names conditions, divisions, statuses, priorities and log kinds", () => {
    expect(conditionInfo("red")).toEqual({ label: "RED", tone: "red" });
    expect(conditionInfo("yellow")).toEqual({ label: "YELLOW", tone: "gold" });
    expect(conditionInfo(undefined).label).toBe("UNKNOWN");
    expect(divisionInfo("ops")).toEqual({ label: "Operations", tone: "red" });
    expect(divisionInfo("engineering")).toEqual({ label: "Engineering", tone: "grey" });
    expect(divisionInfo("").label).toBe("No division");
    expect(crewStatusInfo("hailing")).toEqual({ label: "Hailing", tone: "blue", blink: true });
    expect(crewStatusInfo("on_deck").label).toBe("On deck");
    expect(priorityInfo(2).label).toBe("Red alert");
    expect(priorityInfo(0)).toEqual({ label: "Routine", tone: "slate" });
    expect(logTag("done")).toEqual({ tag: "BACK", tone: "green" });
    expect(logTag("fail").tag).toBe("LOST");
    expect(logTag("whatever").tag).toBe("WHATEV");
  });

  it("shows a hail's context as one line of JSON, or nothing", () => {
    expect(contextText(null)).toBe("");
    expect(contextText({})).toBe("");
    expect(contextText({ tool: "Bash", command: "make deploy" })).toBe('{"tool":"Bash","command":"make deploy"}');
  });

  it("reads a done entry's summary from its payload, else its text", () => {
    expect(summaryOf(entry(1, { kind: "done", text: "long text", payload: { summary: "short" } }))).toBe("short");
    expect(summaryOf(entry(1, { kind: "done", text: "long text", payload: null }))).toBe("long text");
  });
});

describe("panels' lists", () => {
  const s = stateOf({
    crew: [
      crew("tuvok", { division: "science", status: "away", current_beam: "bm-1" }),
      crew("data", { division: "ops" }),
      crew("gone", { division: "command", status: "dismissed" }),
      crew("worf", { division: "ops", status: "sickbay" }),
    ],
    orders: [
      order("ORD-0001", { state: "away", assignee: "tuvok" }),
      order("ORD-0002", { state: "pool", division: "ops" }),
      order("ORD-0003", { state: "directed", assignee: "data", priority: 2 }),
      order("ORD-0004", { state: "hailing", assignee: "x", priority: 1, repos: [{ name: "lib", base_ref: "main" }] }),
    ],
    hails: [hail("H-0002"), hail("H-0001"), hail("H-0003", { status: "decided" })],
  });

  it("the manifest leaves out the dismissed", () => {
    expect(manifest(s).map((c) => c.id)).toEqual(["data", "tuvok", "worf"]);
    expect(knownDivisions(s)).toEqual(["ops", "science"]);
  });

  it("orders split into open and away, red alerts first", () => {
    expect(openOrders(s).map((o) => o.id)).toEqual(["ORD-0003", "ORD-0002"]);
    expect(awayOrders(s).map((o) => o.id)).toEqual(["ORD-0004", "ORD-0001"]);
    expect(currentOrder(s, "tuvok")?.id).toBe("ORD-0001");
    expect(currentOrder(s, "data")).toBeUndefined();
    expect(queuedFor(s, "data")).toBe(1);
  });

  it("the composer sends a registered repo (#211)", () => {
    expect(composerRepo(["app", "lib"], "lib")).toBe("lib");
    expect(composerRepo(["app", "lib"], "")).toBe("app");
    expect(composerRepo(["app", "lib"], "gone")).toBe("app");
    expect(composerRepo([], "app")).toBe("");
  });

  it("open hails, oldest first", () => {
    expect(openHails(s).map((h) => h.id)).toEqual(["H-0001", "H-0002"]);
  });

  it("pads follow crew status", () => {
    expect(padInfo(crew("a", { status: "away", current_beam: "bm-1" }))).toEqual({ mode: "on", label: "bm-1", tone: "gold" });
    expect(padInfo(crew("a", { status: "sickbay" })).mode).toBe("lost");
    expect(padInfo(crew("a", { current_beam: "bm-2" })).label).toBe("standby");
    expect(padInfo(crew("a")).label).toBe("no beam");
  });
});

describe("progress (#141)", () => {
  it("comes from the order view, not the log: a reload shows a report older than the snapshot's log", () => {
    // The progress entry is long gone from the snapshot's 200 entries.
    const log = Array.from({ length: 200 }, (_, i) => entry(1000 + i, { kind: "bridge" }));
    let s = reduce(emptyState, {
      type: "snapshot",
      snapshot: snapshot({
        last_log_id: 1199,
        orders: [
          order("ORD-0001", { state: "away", assignee: "tuvok", progress_pct: 40, progress_note: "tests" }),
          order("ORD-0002", { state: "away" }),
        ],
        log,
      }),
    });
    const [first, second] = awayOrders(s);
    expect(orderProgress(first as OrderView)).toEqual({ pct: 40, note: "tests" });
    expect(orderProgress(second as OrderView)).toBeUndefined();
    // A later view replaces it; a log entry alone doesn't.
    s = reduce(s, { type: "log", entry: entry(1200, { order_id: "ORD-0001", payload: { seq: 9, pct: 90 } }) });
    expect(orderProgress(s.order.get("ORD-0001") as OrderView)?.pct).toBe(40);
    s = reduce(s, { type: "order", view: order("ORD-0001", { state: "away", progress_pct: 90 }) });
    expect(orderProgress(s.order.get("ORD-0001") as OrderView)).toEqual({ pct: 90, note: "" });
  });

  it("clamps the view's pct and ignores one that isn't a number", () => {
    expect(orderProgress(order("o", { progress_pct: 140 }))?.pct).toBe(100);
    expect(orderProgress(order("o", { progress_pct: -3 }))?.pct).toBe(0);
    expect(orderProgress(order("o", { progress_pct: 0 }))).toEqual({ pct: 0, note: "" });
    expect(orderProgress(order("o", { progress_pct: Number.NaN }))).toBeUndefined();
    expect(orderProgress(order("o", { progress_note: "note without pct" }))).toBeUndefined();
  });

  it("a log row's pct comes from its entry's payload", () => {
    const progress = (orderId: string, pct: unknown) => entry(1, { order_id: orderId, payload: { seq: 1, pct } });
    expect(progressPct(progress("o1", 42))).toBe(42);
    expect(progressPct(progress("o1", -3))).toBe(0);
    expect(progressPct(progress("", 42))).toBeUndefined();
    expect(progressPct(progress("o1", "42"))).toBeUndefined();
    expect(progressPct(progress("o1", Number.NaN))).toBeUndefined();
    expect(progressPct(entry(1, { order_id: "o1", payload: null }))).toBeUndefined();
    expect(progressPct(entry(1, { order_id: "o1", payload: [1, 2] }))).toBeUndefined();
  });
});

describe("missionRecord", () => {
  it("after a snapshot, finds a done or fail entry behind \"crew back on deck\"", () => {
    const s = stateOf({
      last_log_id: 6,
      log: [
        entry(1, { kind: "fail", order_id: "ORD-0001", crew_id: "worf", text: "beam lost" }),
        entry(2, { kind: "crew", order_id: "ORD-0001", crew_id: "worf", text: "crew back on deck" }),
        entry(3, { kind: "hail", order_id: "ORD-0002", crew_id: "data", text: "hail H-0001 decided: allow" }),
        entry(4, { kind: "done", order_id: "ORD-0002", crew_id: "data", text: "done (app: ...)", payload: { summary: "all green" } }),
        entry(5, { kind: "crew", order_id: "ORD-0002", crew_id: "data", text: "crew back on deck" }),
        entry(6, { kind: "crew", order_id: "ORD-0003", crew_id: "data", text: "crew back on deck" }),
      ],
    });
    expect(missionRecord(s).map((r) => [r.orderId, r.result, r.crewId, r.summary])).toEqual([
      ["ORD-0002", "Complete", "data", "all green"],
      ["ORD-0001", "Failed", "worf", "beam lost"],
    ]);
  });


  it("lists finished orders, the newest entry first", () => {
    let s = stateOf({
      last_log_id: 5,
      orders: [order("ORD-0003", { state: "away", assignee: "tuvok" })],
      log: [
        entry(1, { kind: "fail", order_id: "ORD-0001", crew_id: "worf", text: "beam lost" }),
        entry(2, { kind: "done", order_id: "ORD-0002", crew_id: "data", text: "done (app: ...)", payload: { summary: "all green" } }),
        entry(3, { kind: "order", order_id: "ORD-0003", text: "claimed" }),
        entry(4, { kind: "order", order_id: "ORD-0099", text: "unknown, not finished" }),
      ],
    });
    expect(missionRecord(s)).toEqual([
      { orderId: "ORD-0002", title: "", crewId: "data", result: "Complete", tone: "green", summary: "all green" },
      { orderId: "ORD-0001", title: "", crewId: "worf", result: "Failed", tone: "red", summary: "beam lost" },
    ]);

    // The feed then sends the order's view: its state decides, and its title shows.
    s = reduce(s, { type: "log", entry: entry(6, { kind: "order", order_id: "ORD-0003", text: "belayed" }) });
    s = reduce(s, { type: "order", view: order("ORD-0003", { state: "belayed", assignee: "tuvok" }) });
    s = reduce(s, { type: "order", view: order("ORD-0007", { state: "withdrawn" }) });
    const rows = missionRecord(s);
    expect(rows.map((r) => [r.orderId, r.result, r.title])).toEqual([
      ["ORD-0003", "Belayed", "title of ORD-0003"],
      ["ORD-0002", "Complete", ""],
      ["ORD-0001", "Failed", ""],
      ["ORD-0007", "Withdrawn", "title of ORD-0007"],
    ]);
    expect(missionRecord(s, 2)).toHaveLength(2);
  });

  it("after a reload, lists the belayed and withdrawn orders the snapshot carries (#78)", () => {
    const s = stateOf({
      last_log_id: 5,
      orders: [
        order("ORD-0001", { state: "belayed", assignee: "worf" }),
        order("ORD-0002", { state: "withdrawn", division: "ops" }),
        order("ORD-0003", { state: "done", assignee: "data" }),
        order("ORD-0004", { state: "pool", division: "ops" }),
      ],
      log: [
        entry(1, { kind: "order", order_id: "ORD-0001", crew_id: "worf", text: "belay requested" }),
        entry(2, { kind: "order", order_id: "ORD-0001", crew_id: "worf", text: "order belayed" }),
        entry(3, { kind: "done", order_id: "ORD-0003", crew_id: "data", text: "done", payload: { summary: "all green" } }),
        entry(4, { kind: "order", order_id: "ORD-0002", text: "order withdrawn: duplicate" }),
        entry(5, { kind: "order", order_id: "ORD-0004", text: "order created" }),
      ],
    });
    expect(missionRecord(s).map((r) => [r.orderId, r.result, r.title, r.crewId, r.summary])).toEqual([
      ["ORD-0002", "Withdrawn", "title of ORD-0002", "", ""],
      ["ORD-0003", "Complete", "title of ORD-0003", "data", "all green"],
      ["ORD-0001", "Belayed", "title of ORD-0001", "worf", ""],
    ]);
  });
});

describe("split orders (#155)", () => {
  it("lists a split order as waiting on its open children, until it settles", () => {
    let s = stateOf({
      last_log_id: 2,
      orders: [
        order("ORD-0001", { state: "split", assignee: "vance", open_children: 2 }),
        order("ORD-0002", { state: "pool", division: "ops", parent_id: "ORD-0001" }),
        order("ORD-0003", { state: "pool", division: "ops", parent_id: "ORD-0001" }),
      ],
      log: [
        entry(1, { kind: "order", order_id: "ORD-0001", crew_id: "vance", text: "order split into 2 children: ORD-0002, ORD-0003" }),
        entry(2, { kind: "done", order_id: "ORD-0001", crew_id: "vance", text: "split", payload: { summary: "rfc split" } }),
      ],
    });
    expect(missionRecord(s)).toEqual([
      { orderId: "ORD-0001", title: "title of ORD-0001", crewId: "vance", result: "Split: waiting on 2 children", tone: "blue", summary: "rfc split", splitChildren: 2 },
    ]);
    expect(openOrders(s).map((o) => o.id)).toEqual(["ORD-0002", "ORD-0003"]);

    s = reduce(s, { type: "order", view: order("ORD-0001", { state: "split", assignee: "vance", open_children: 1 }) });
    expect(missionRecord(s)[0]?.result).toBe("Split: waiting on 1 child");
    expect(missionRecord(s)[0]?.splitChildren).toBe(1);
    s = reduce(s, { type: "order", view: order("ORD-0001", { state: "done", assignee: "vance", open_children: 0 }) });
    expect(missionRecord(s)[0]?.result).toBe("Complete");
    expect(missionRecord(s)[0]?.splitChildren).toBeUndefined();
  });

  it("a split order the log no longer names is still listed", () => {
    const s = stateOf({ last_log_id: 0, orders: [order("ORD-0001", { state: "split", open_children: 3 })], log: [] });
    expect(missionRecord(s).map((r) => r.result)).toEqual(["Split: waiting on 3 children"]);
    expect(missionRecord(s)[0]?.splitChildren).toBe(3);
  });
});

describe("cascade belay (#146)", () => {
  it("the belay confirm says how many open child orders it reaches", () => {
    expect(belayPrompt("ORD-0001", 2)).toBe("Belays ORD-0001 and its 2 open child orders. Belay them all?");
    expect(belayPrompt("ORD-0001", 1)).toBe("Belays ORD-0001 and its 1 open child order. Belay them all?");
    expect(belayPrompt("ORD-0001", 0)).toBe("Belay ORD-0001? The order is abandoned.");
  });
});

describe("Phase 2 controls (#78)", () => {
  it("the route picker's value and args", () => {
    expect(routeValue(order("o", { state: "pool", division: "ops" }))).toBe("pool");
    expect(routeValue(order("o", { state: "directed", assignee: "worf" }))).toBe("worf");
    expect(routeArgs("pool")).toEqual({ assignee: "" });
    expect(routeArgs("worf")).toEqual({ assignee: "worf" });
  });

  it("route and the composer offer crew not in sickbay", () => {
    expect(canTake(crew("a"))).toBe(true);
    expect(canTake(crew("a", { status: "away" }))).toBe(true);
    expect(canTake(crew("a", { status: "sickbay" }))).toBe(false);
    expect(canTake(crew("a", { status: "dismissed" }))).toBe(false);
    expect(crewOptionLabel(crew("sorell", { division: "science", status: "hailing" }))).toBe("SORELL · Science (hailing)");
  });

  it("standing orders follow the status, unknown before it", () => {
    expect(standingOrdersInfo(undefined).on).toBeUndefined();
    expect(standingOrdersInfo(status({ standing_orders: true }))).toEqual({
      on: true,
      label: "on",
      hint: "Free crew take open orders from the pool, matching their division.",
    });
    expect(standingOrdersInfo(status())).toEqual({ on: false, label: "off", hint: "Crew only move on directed orders." });
  });
});

describe("countermand window (#153)", () => {
  const until = "2026-10-30T10:05:00Z";
  const before = new Date("2026-10-30T10:04:59Z");
  const at = new Date(until);
  const after = new Date("2026-10-30T10:05:01Z");
  const decided = (id: string, over: Parameters<typeof hail>[1] = {}) =>
    hail(id, { status: "decided", decided_by: "command", original_decided_by: "command", decision: "allow", countermand_until: until, ...over });

  it("is open on a Command or captain timeout decision until countermand_until, inclusive", () => {
    expect(canCountermand(decided("H-1"), before)).toBe(true);
    expect(canCountermand(decided("H-1"), at)).toBe(true);
    expect(canCountermand(decided("H-1"), after)).toBe(false);
    expect(canCountermand(decided("H-1", { decided_by: "policy", original_decided_by: "policy" }), before)).toBe(true);
    // Countermanded already: the window still runs.
    expect(canCountermand(decided("H-1", { decided_by: "captain", countermanded_at: "2026-10-30T10:01:00Z" }), before)).toBe(true);
  });

  it("is never open on an open hail, an auto_allow decision, the captain's own answer or a bad timestamp", () => {
    expect(canCountermand(hail("H-1", { countermand_until: until }), before)).toBe(false);
    const auto = hail("H-1", { status: "decided", decided_by: "policy", original_decided_by: "policy", route: "auto" });
    expect(canCountermand(auto, before)).toBe(false);
    expect(canCountermand(decided("H-1", { decided_by: "captain", original_decided_by: "captain" }), before)).toBe(false);
    expect(canCountermand(decided("H-1", { countermand_until: "soon" }), before)).toBe(false);
  });

  it("lists the hails inside their window, oldest first", () => {
    const s = stateOf({
      hails: [decided("H-0003"), hail("H-0002"), decided("H-0001"), decided("H-0004", { countermand_until: "2026-10-30T10:00:00Z" })],
    });
    expect(countermandableHails(s, before).map((h) => h.id)).toEqual(["H-0001", "H-0003"]);
    expect(countermandableHails(s, after)).toEqual([]);
  });

  it("names who decided", () => {
    expect(decidedByLabel(decided("H-1"))).toBe("Command");
    expect(decidedByLabel(decided("H-1", { decided_by: "policy" }))).toBe("the captain timeout");
    expect(decidedByLabel(decided("H-1", { decided_by: "captain", countermanded_at: until }))).toBe("you (countermanded)");
  });
});

describe("push (#4)", () => {
  it("is offered for a done order only", () => {
    expect(canPush(order("ORD-0001", { state: "done" }))).toBe(true);
    for (const state of ["pool", "directed", "away", "hailing", "split", "failed", "belayed", "withdrawn"]) {
      expect(canPush(order("ORD-0001", { state })), state).toBe(false);
    }
    expect(canPush(undefined)).toBe(false);
    expect(canPush(order("ORD-0001", { state: "done", repos: [] })), "no repos").toBe(false);
  });

  it("asks naming each remote and the target branch", () => {
    const s = stateOf({ repo_urls: { app: "https://example.com/o/app.git", lib: "git@example.com:o/lib.git" } });
    const one = pushRemotes(s, order("ORD-0007"));
    expect(one).toEqual([{ name: "app", url: "https://example.com/o/app.git" }]);
    expect(pushPrompt("ORD-0007", one, "bmus/ORD-0007")).toBe("Push bmus/ORD-0007 to https://example.com/o/app.git as bmus/ORD-0007?");
    const two = pushRemotes(s, order("ORD-0007", { repos: [{ name: "app", base_ref: "main" }, { name: "lib", base_ref: "main" }] }));
    expect(pushPrompt("ORD-0007", two, "feature")).toBe(
      "Push bmus/ORD-0007 to https://example.com/o/app.git (app) and git@example.com:o/lib.git (lib) as feature?",
    );
    expect(pushPrompt("ORD-0007", pushRemotes(stateOf({}), order("ORD-0007")), "x")).toBe("Push bmus/ORD-0007 to app's remote as x?");
    expect(forcePushPrompt("ORD-0007", one, "feature")).toBe(
      "Force push bmus/ORD-0007 to https://example.com/o/app.git as feature? The remote branch's commits that bmus/ORD-0007 doesn't have are overwritten.",
    );
    expect(forcePushPrompt("ORD-0007", two, "feature")).toBe(
      "Force push bmus/ORD-0007 to https://example.com/o/app.git (app) and git@example.com:o/lib.git (lib) as feature? " +
        "The remote branches' commits that bmus/ORD-0007 doesn't have are overwritten.",
    );
  });

  it("links a pull request for github.com remotes only", () => {
    const compare = "https://github.com/o/r/compare/bmus/ORD-0007?expand=1";
    for (const url of [
      "git@github.com:o/r.git",
      "git@github.com:o/r",
      "ssh://git@github.com/o/r.git",
      "ssh://github.com/o/r.git",
      "ssh://github.com:22/o/r.git",
      "https://github.com/o/r",
      "https://github.com/o/r.git",
      "https://github.com/o/r/",
      "https://GitHub.com/o/r.git",
    ]) {
      expect(githubCompareURL(url, "bmus/ORD-0007"), url).toBe(compare);
    }
    for (const url of [
      "https://github.com.example.com/o/r.git",
      "git@github.com.example.com:o/r.git",
      "ssh://git@github.com.example.com/o/r.git",
      "https://example.com/github.com/o/r.git",
      "https://github.com@example.com/o/r.git",
      "https://notgithub.com/o/r.git",
      "git@example.com:github.com/o/r.git",
      "http://github.com/o/r.git",
      "file:///srv/github.com/o/r.git",
      "/srv/git/github.com:o/r.git",
      "https://github.com/o/r/tree/main",
      "https://github.com/o",
      "https://github.com/o/r.git?x=1",
      "https://github.com:8443/o/r.git",
      "git@github.com:o/r/x.git",
      "",
    ]) {
      expect(githubCompareURL(url, "bmus/ORD-0007"), url).toBeUndefined();
    }
    expect(githubCompareURL("https://github.com/o/r.git", "feat/a b#c?d")).toBe("https://github.com/o/r/compare/feat/a%20b%23c%3Fd?expand=1");
  });

  it("the diff page's path is /diff/<order>, encoded", () => {
    expect(diffPath("ORD-0007")).toBe("/diff/ORD-0007");
    expect(diffPath("a/b?c")).toBe("/diff/a%2Fb%3Fc");
  });

  it("reads where an order was last pushed from its log", () => {
    const s = stateOf({
      last_log_id: 4,
      log: [
        entry(1, { kind: "order", order_id: "ORD-0007", text: "repo app pushed to bmus/ORD-0007 at 0123456789ab" }),
        entry(2, { kind: "order", order_id: "ORD-0007", text: "repo app: push to feature at 0123456789ab failed" }),
        entry(3, { kind: "order", order_id: "ORD-0007", text: "repo app pushed to feature at 0123456789ab (forced)" }),
        entry(4, { kind: "order", order_id: "ORD-0008", text: "repo app pushed to other at 0123456789ab" }),
      ],
    });
    expect(pushedTo(s, "ORD-0007")).toBe("feature");
    expect(pushedTo(s, "ORD-0008")).toBe("other");
    expect(pushedTo(s, "ORD-0009")).toBeUndefined();
  });
});
