// The console's one store, fed by the change feed (#72): crew members,
// orders and hails keyed by id, the bridge's status and the recent
// captain's log. An order's progress is on its view (#141). A snapshot
// replaces everything; a log entry is appended; an entity view replaces
// the one with its id; a status replaces the status. There are no
// optimistic updates: actions post and wait for the feed (console plan,
// "Frontend").
//
// The store also queues the hails to notify about (#76): an open hail it
// hasn't seen, from a live hail event or a snapshot after the first (a
// reconnect). The app raises their notifications and
// acknowledges them with an "announced" action.
//
// Actions are this module's own shape, not the wire format: feed.ts maps
// the server-sent events onto them.

import type { BridgeStatus, CrewView, HailView, LogEntryView, OrderView, Snapshot } from "./api.js";
import { categoryCampaignTriage } from "./view.js";

/**
 * How many log entries the live feed keeps; older ones are dropped as new
 * ones arrive. Pages loaded with "Load older entries" raise the cap by
 * their size, so live entries don't drop what the captain scrolled back to.
 */
export const logLimit = 500;

export interface State {
  readonly crew: ReadonlyMap<string, CrewView>;
  readonly order: ReadonlyMap<string, OrderView>;
  readonly hail: ReadonlyMap<string, HailView>;
  /** Undefined until the first snapshot. */
  readonly status: BridgeStatus | undefined;
  /** Recent entries, oldest first, ids rising. */
  readonly log: readonly LogEntryView[];
  /** The newest log id applied. */
  readonly lastLogId: number;
  /** Whether log reaches back to the log's first entry. */
  readonly logStart: boolean;
  /** How many entries log keeps: logLimit plus the older pages loaded since the snapshot. */
  readonly logCap: number;
  /** Ids of hails newly opened by live events or a reconnect snapshot and not yet announced, oldest first. */
  readonly fresh: readonly string[];
  /** Whether a snapshot has been applied: the next one is a reconnect. */
  readonly loaded: boolean;
  /** The bridge's registered repo names, sorted, from the snapshot (#211). */
  readonly repos: readonly string[];
  /** The registered repos' URLs, without credentials, by name (#4). */
  readonly repoUrls: Readonly<Record<string, string>>;
}

export type Action =
  | { readonly type: "snapshot"; readonly snapshot: Snapshot }
  | { readonly type: "log"; readonly entry: LogEntryView }
  /** A page read with /api/log?before=<before>; ignored unless before is still the oldest entry's id. */
  | { readonly type: "older"; readonly before: number; readonly entries: readonly LogEntryView[]; readonly complete: boolean }
  | { readonly type: "status"; readonly status: BridgeStatus }
  | { readonly type: "crew"; readonly view: CrewView }
  | { readonly type: "order"; readonly view: OrderView }
  | { readonly type: "hail"; readonly view: HailView }
  /** The app raised notifications for these fresh hails. */
  | { readonly type: "announced"; readonly ids: readonly string[] };

export const emptyState: State = {
  crew: new Map(),
  order: new Map(),
  hail: new Map(),
  status: undefined,
  log: [],
  lastLogId: 0,
  logStart: false,
  logCap: logLimit,
  fresh: [],
  loaded: false,
  repos: [],
  repoUrls: {},
};

/** Returns the state after action. It never mutates state. */
export function reduce(state: State, action: Action): State {
  switch (action.type) {
    case "snapshot": {
      const snap = action.snapshot;
      const log = snap.log.slice(-logLimit);
      return {
        crew: byId(snap.crew),
        order: byId(snap.orders),
        hail: byId(snap.hails),
        status: snap.status,
        log,
        lastLogId: snap.last_log_id,
        logStart: log.length === 0 || log[0]?.id === 1,
        logCap: logLimit,
        // The first snapshot's hails are never fresh. A reconnect's open
        // hails the store hadn't seen opened while the feed was down.
        fresh: state.loaded ? withFresh(state, snap.hails ?? []) : state.fresh,
        loaded: true,
        repos: snap.repos ?? [],
        repoUrls: snap.repo_urls ?? {},
      };
    }
    case "log": {
      const e = action.entry;
      if (e.id <= state.lastLogId) return state; // already applied
      const log = [...state.log, e];
      return {
        ...state,
        log: log.length > state.logCap ? log.slice(-state.logCap) : log,
        lastLogId: e.id,
        logStart: state.logStart && log.length <= state.logCap,
      };
    }
    case "older": {
      // A snapshot since the request replaced the log: the page may not join it.
      const first = state.log[0]?.id;
      if (first === undefined || action.before !== first) return state;
      const older = action.entries.filter((e) => e.id < first);
      if (older.length === 0 && !action.complete) return state;
      const log = [...older, ...state.log];
      return {
        ...state,
        log,
        logStart: action.complete || log[0]?.id === 1,
        logCap: state.logCap + older.length,
      };
    }
    case "status":
      return { ...state, status: action.status };
    case "crew":
      return { ...state, crew: upsert(state.crew, action.view) };
    case "order":
      return { ...state, order: upsert(state.order, action.view) };
    case "hail": {
      const v = action.view;
      return { ...state, hail: upsert(state.hail, v), fresh: withFresh(state, [v]) };
    }
    case "announced": {
      const done = new Set(action.ids);
      return { ...state, fresh: state.fresh.filter((id) => !done.has(id)) };
    }
  }
}

/**
 * Returns state.fresh with the open hails among views that state doesn't
 * have yet appended, and a campaign triage newly routed to the captain
 * (#187): Command's answer didn't fit, and the captain now has to answer.
 */
function withFresh(state: State, views: readonly HailView[]): readonly string[] {
  const rerouted = (v: HailView) => {
    const was = state.hail.get(v.id);
    return was !== undefined && v.category === categoryCampaignTriage && was.route !== "captain" && v.route === "captain";
  };
  const add = views.filter((v) => v.status === "open" && (!state.hail.has(v.id) || rerouted(v)) && !state.fresh.includes(v.id)).map((v) => v.id);
  return add.length === 0 ? state.fresh : [...state.fresh, ...add];
}

function byId<T extends { readonly id: string }>(list: readonly T[] | null): Map<string, T> {
  return new Map((list ?? []).map((v) => [v.id, v]));
}

function upsert<T extends { readonly id: string }>(m: ReadonlyMap<string, T>, v: T): Map<string, T> {
  const next = new Map(m);
  next.set(v.id, v);
  return next;
}
