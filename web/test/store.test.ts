import { describe, expect, it } from "vitest";
import { emptyState, logLimit, reduce, type Action, type State } from "../src/store.js";
import { crew, entry, hail, order, snapshot, status } from "./fixtures.js";

const apply = (s: State, ...actions: Action[]) => actions.reduce(reduce, s);

describe("reduce: snapshot", () => {
  it("replaces every kind, the status and the log", () => {
    let s = apply(emptyState, {
      type: "snapshot",
      snapshot: snapshot({
        last_log_id: 3,
        crew: [crew("c1"), crew("c2")],
        orders: [order("o1")],
        hails: [hail("h1")],
        status: status({ condition: "yellow", reasons: ["c1 hailing"] }),
        log: [entry(2), entry(3)],
        repos: ["app", "lib"],
      }),
    });
    expect([...s.crew.keys()]).toEqual(["c1", "c2"]);
    expect(s.repos).toEqual(["app", "lib"]);
    expect(s.order.get("o1")?.title).toBe("title of o1");
    expect(s.hail.has("h1")).toBe(true);
    expect(s.status?.condition).toBe("yellow");
    expect(s.log.map((e) => e.id)).toEqual([2, 3]);
    expect(s.lastLogId).toBe(3);
    expect(s.logStart).toBe(false);

    s = apply(s, { type: "snapshot", snapshot: snapshot({ last_log_id: 9, crew: [crew("c3")], log: [entry(1)] }) });
    expect([...s.crew.keys()]).toEqual(["c3"]);
    expect(s.order.size).toBe(0);
    expect(s.hail.size).toBe(0);
    expect(s.log.map((e) => e.id)).toEqual([1]);
    expect(s.lastLogId).toBe(9);
    expect(s.logStart).toBe(true);
    expect(s.repos).toEqual([]);
  });

  it("of an empty log starts at the log's start", () => {
    const s = apply(emptyState, { type: "snapshot", snapshot: snapshot() });
    expect(s.logStart).toBe(true);
    expect(s.lastLogId).toBe(0);
    expect(s.status?.condition).toBe("green");
  });

  it("on reconnection replaces what came before", () => {
    let s = apply(
      emptyState,
      { type: "snapshot", snapshot: snapshot({ last_log_id: 2, orders: [order("o1", { state: "away" })], log: [entry(2)] }) },
      { type: "hail", view: hail("h1") },
      { type: "log", entry: entry(3) },
    );
    s = apply(s, { type: "snapshot", snapshot: snapshot({ last_log_id: 7, orders: [order("o2")], log: [entry(7)] }) });
    expect([...s.order.keys()]).toEqual(["o2"]);
    expect(s.hail.size).toBe(0);
    expect(s.log.map((e) => e.id)).toEqual([7]);
    expect(s.lastLogId).toBe(7);
  });
});

describe("reduce: log", () => {
  it("appends entries and moves the last id", () => {
    const s = apply(emptyState, { type: "snapshot", snapshot: snapshot({ last_log_id: 1, log: [entry(1)] }) }, { type: "log", entry: entry(2) });
    expect(s.log.map((e) => e.id)).toEqual([1, 2]);
    expect(s.lastLogId).toBe(2);
  });

  it("ignores an entry at or below the last id", () => {
    const s = apply(emptyState, { type: "snapshot", snapshot: snapshot({ last_log_id: 5, log: [entry(5)] }) });
    expect(reduce(s, { type: "log", entry: entry(5) })).toBe(s);
    expect(reduce(s, { type: "log", entry: entry(4) })).toBe(s);
  });

  it(`keeps the last ${logLimit} entries`, () => {
    let s = apply(emptyState, { type: "snapshot", snapshot: snapshot({ last_log_id: 1, log: [entry(1)] }) });
    expect(s.logStart).toBe(true);
    for (let id = 2; id <= logLimit + 10; id++) s = reduce(s, { type: "log", entry: entry(id) });
    expect(s.log).toHaveLength(logLimit);
    expect(s.log[0]?.id).toBe(11);
    expect(s.log.at(-1)?.id).toBe(logLimit + 10);
    expect(s.logStart).toBe(false);
  });

  it("keeps an entry with a null payload", () => {
    const s = apply(emptyState, { type: "snapshot", snapshot: snapshot() }, { type: "log", entry: entry(1, { order_id: "o1", payload: null }) });
    expect(s.log).toHaveLength(1);
  });
});

describe("reduce: older", () => {
  const base = () => apply(emptyState, { type: "snapshot", snapshot: snapshot({ last_log_id: 12, log: [entry(10), entry(11), entry(12)] }) });

  it("prepends a page of older entries", () => {
    const s = reduce(base(), { type: "older", before: 10, entries: [entry(8), entry(9)], complete: false });
    expect(s.log.map((e) => e.id)).toEqual([8, 9, 10, 11, 12]);
    expect(s.logStart).toBe(false);
    expect(s.lastLogId).toBe(12);
  });

  it("drops entries it already has and marks the start", () => {
    const s = reduce(base(), { type: "older", before: 10, entries: [entry(9), entry(10)], complete: true });
    expect(s.log.map((e) => e.id)).toEqual([9, 10, 11, 12]);
    expect(s.logStart).toBe(true);
  });

  it("ignores a page asked for before a snapshot replaced the log", () => {
    // The page was asked for below 10; a snapshot then moved the oldest entry to 40.
    const s = apply(base(), { type: "snapshot", snapshot: snapshot({ last_log_id: 41, log: [entry(40), entry(41)] }) });
    expect(reduce(s, { type: "older", before: 10, entries: [entry(8), entry(9)], complete: true })).toBe(s);
    expect(reduce(emptyState, { type: "older", before: 10, entries: [entry(8)], complete: true })).toBe(emptyState);
  });

  it("keeps loaded pages when live entries arrive", () => {
    let s = apply(emptyState, {
      type: "snapshot",
      snapshot: snapshot({ last_log_id: 1000, log: Array.from({ length: logLimit }, (_, i) => entry(501 + i)) }),
    });
    s = reduce(s, { type: "older", before: 501, entries: Array.from({ length: 200 }, (_, i) => entry(301 + i)), complete: false });
    expect(s.log).toHaveLength(logLimit + 200);
    expect(s.logCap).toBe(logLimit + 200);
    s = reduce(s, { type: "log", entry: entry(1001) });
    expect(s.log[0]?.id).toBe(302); // one dropped, not the page
    expect(s.log).toHaveLength(logLimit + 200);
    // A snapshot resets the cap.
    s = reduce(s, { type: "snapshot", snapshot: snapshot({ last_log_id: 1001, log: [entry(1001)] }) });
    expect(s.logCap).toBe(logLimit);
  });

  it("an empty page that isn't the start changes nothing", () => {
    const s = base();
    expect(reduce(s, { type: "older", before: 10, entries: [], complete: false })).toBe(s);
  });
});

describe("reduce: views and status", () => {
  it("upserts views by id within their kind", () => {
    const s = apply(
      emptyState,
      { type: "crew", view: crew("c1") },
      { type: "crew", view: crew("c1", { status: "away" }) },
      { type: "order", view: order("c1") },
      { type: "hail", view: hail("h1") },
      { type: "hail", view: hail("h1", { status: "decided" }) },
    );
    expect(s.crew.get("c1")?.status).toBe("away");
    expect(s.crew.size).toBe(1);
    expect(s.order.get("c1")?.id).toBe("c1");
    expect(s.hail.get("h1")?.status).toBe("decided");
  });

  it("replaces the status", () => {
    const s = apply(emptyState, { type: "status", status: status({ condition: "red", reasons: ["c1 in sickbay"] }) });
    expect(s.status?.condition).toBe("red");
    expect(reduce(s, { type: "status", status: status() }).status?.condition).toBe("green");
  });

  it("standing orders come and go with the status (#78)", () => {
    const s = apply(emptyState, { type: "snapshot", snapshot: snapshot({ status: status({ standing_orders: true }) }) });
    expect(s.status?.standing_orders).toBe(true);
    expect(reduce(s, { type: "status", status: status() }).status?.standing_orders).toBe(false);
  });

  it("keeps a snapshot's finished orders (#78)", () => {
    const s = apply(emptyState, {
      type: "snapshot",
      snapshot: snapshot({ orders: [order("o1", { state: "away" }), order("o2", { state: "belayed" }), order("o3", { state: "withdrawn" })] }),
    });
    expect([...s.order.values()].map((o) => [o.id, o.state])).toEqual([
      ["o1", "away"],
      ["o2", "belayed"],
      ["o3", "withdrawn"],
    ]);
  });

  it("never mutates the previous state", () => {
    const before = apply(emptyState, { type: "snapshot", snapshot: snapshot({ last_log_id: 1, crew: [crew("c1")], log: [entry(1)] }) });
    reduce(before, { type: "crew", view: crew("c2") });
    reduce(before, { type: "log", entry: entry(2) });
    reduce(before, { type: "older", before: 1, entries: [], complete: true });
    expect([...before.crew.keys()]).toEqual(["c1"]);
    expect(before.log).toHaveLength(1);
    expect(emptyState.crew.size).toBe(0);
    expect(emptyState.log).toHaveLength(0);
  });
});

describe("reduce: fresh hails", () => {
  const snap = (...hails: ReturnType<typeof hail>[]): Action => ({ type: "snapshot", snapshot: snapshot({ hails }) });

  it("ignores the first snapshot's hails", () => {
    const s = apply(emptyState, snap(hail("h1"), hail("h2")));
    expect(s.fresh).toEqual([]);
  });

  it("reports a live new open hail once", () => {
    let s = apply(emptyState, snap(hail("h1")), { type: "hail", view: hail("h2") });
    expect(s.fresh).toEqual(["h2"]);
    s = apply(s, { type: "announced", ids: ["h2"] });
    expect(s.fresh).toEqual([]);
    expect(s.hail.has("h2")).toBe(true);
  });

  it("doesn't report a re-sent view of a known hail", () => {
    let s = apply(emptyState, snap(hail("h1")), { type: "hail", view: hail("h1") });
    expect(s.fresh).toEqual([]);
    s = apply(s, { type: "hail", view: hail("h2") }, { type: "hail", view: hail("h2") });
    expect(s.fresh).toEqual(["h2"]);
    s = apply(s, { type: "announced", ids: ["h2"] }, { type: "hail", view: hail("h2", { context: { more: 1 } }) });
    expect(s.fresh).toEqual([]);
  });

  it("doesn't report a hail first seen decided or timed out", () => {
    const s = apply(
      emptyState,
      snap(),
      { type: "hail", view: hail("h1", { status: "decided" }) },
      { type: "hail", view: hail("h2", { status: "timed_out" }) },
    );
    expect(s.fresh).toEqual([]);
  });

  it("keeps hails queued before an announcement, in order", () => {
    let s = apply(emptyState, snap(), { type: "hail", view: hail("h1") }, { type: "hail", view: hail("h2") });
    expect(s.fresh).toEqual(["h1", "h2"]);
    s = apply(s, { type: "announced", ids: ["h1"] }, { type: "hail", view: hail("h3") });
    expect(s.fresh).toEqual(["h2", "h3"]);
  });

  it("a reconnect snapshot doesn't report the hails the store knows", () => {
    let s = apply(emptyState, snap(hail("h1")), { type: "hail", view: hail("h2") }, { type: "announced", ids: ["h2"] });
    s = apply(s, snap(hail("h1"), hail("h2")));
    expect(s.fresh).toEqual([]);
  });

  it("a reconnect snapshot reports a hail opened while the feed was down, once", () => {
    let s = apply(emptyState, snap(hail("h1")));
    expect(s.loaded).toBe(true);
    s = apply(s, snap(hail("h1"), hail("h2"), hail("h3", { status: "decided" })));
    expect(s.fresh).toEqual(["h2"]);
    // Neither a re-sent view nor another snapshot reports it again.
    s = apply(s, { type: "hail", view: hail("h2") }, snap(hail("h1"), hail("h2")));
    expect(s.fresh).toEqual(["h2"]);
    s = apply(s, { type: "announced", ids: ["h2"] }, snap(hail("h2")));
    expect(s.fresh).toEqual([]);
  });
});
