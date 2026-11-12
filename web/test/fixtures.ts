// Views for tests, with every field the bridge sends; overrides set the
// ones a test cares about.

import type { BridgeStatus, CrewView, HailView, LogEntryView, OrderView, Snapshot } from "../src/api.js";

export function crew(id: string, over: Partial<CrewView> = {}): CrewView {
  return {
    id,
    name: id,
    rank: "",
    division: "",
    spec: "",
    status: "on_deck",
    current_beam: "",
    tour_no: 1,
    dossier_path: "",
    harness: "",
    harness_source: "default",
    model: "",
    model_source: "default",
    ...over,
  };
}

export function order(id: string, over: Partial<OrderView> = {}): OrderView {
  return {
    id,
    title: `title of ${id}`,
    body: "",
    division: "",
    priority: 0,
    state: "pool",
    assignee: "",
    parent_id: "",
    repos: [{ name: "app", base_ref: "main" }],
    branch: "",
    budget_tokens: 0,
    created_by: "captain",
    model: "",
    open_children: 0,
    ...over,
  };
}

export function hail(id: string, over: Partial<HailView> = {}): HailView {
  return {
    id,
    order_id: "ORD-0001",
    crew_id: "sorell",
    category: "tool_approval",
    question: "Run make deploy?",
    options: ["allow", "deny"],
    context: null,
    route: "",
    status: "open",
    decided_by: "",
    decision: "",
    rationale: "",
    ...over,
  };
}

export function entry(id: number, over: Partial<LogEntryView> = {}): LogEntryView {
  return { id, ts: "2026-10-28T09:00:00Z", kind: "order", crew_id: "", order_id: "", text: `entry ${id}`, payload: null, ...over };
}

export function status(over: Partial<BridgeStatus> = {}): BridgeStatus {
  return {
    condition: "green",
    reasons: [],
    info: [],
    crew: { on_deck: 0, away: 0, hailing: 0, sickbay: 0 },
    orders: { open: 0, away: 0 },
    hails_open: 0,
    beams_live: 0,
    last_log_id: 0,
    console: "127.0.0.1:4096",
    standing_orders: false,
    ...over,
  };
}

export function snapshot(over: Partial<Snapshot> = {}): Snapshot {
  return { last_log_id: 0, crew: [], orders: [], hails: [], status: status(), log: [], repos: [], repo_urls: {}, ...over };
}
