// The console's HTTP API as the page sees it: the bridge's JSON views
// (internal/bridge/views.go, status.go), the feed's snapshot
// (internal/console/events.go) and the action bodies (the RPC args in
// internal/bridge/rpc.go). Field names follow the Go JSON tags exactly.

/** A crew member: bridge.CrewView. */
export interface CrewView {
  readonly id: string;
  readonly name: string;
  readonly rank: string;
  readonly division: string;
  readonly spec: string;
  /** beaming_in (#106), on_deck, away, hailing, sickbay or dismissed. */
  readonly status: string;
  readonly current_beam: string;
  readonly tour_no: number;
  readonly dossier_path: string;
  readonly harness: string;
  readonly harness_source: string;
  readonly model: string;
  readonly model_source: string;
}

/** A repo of an order: bridge.OrderRepo. */
export interface OrderRepo {
  readonly name: string;
  readonly base_ref: string;
}

/** An order: bridge.OrderView. */
export interface OrderView {
  readonly id: string;
  readonly title: string;
  readonly body: string;
  readonly division: string;
  /** 0 routine, 1 yellow, 2 red. */
  readonly priority: number;
  /** pool, directed, away, hailing, split, done, failed, belayed or withdrawn. */
  readonly state: string;
  readonly assignee: string;
  readonly parent_id: string;
  readonly repos: readonly OrderRepo[];
  readonly branch: string;
  /** The order's own token budget; 0 for none (its division's applies). */
  readonly budget_tokens: number;
  /** The order's own turn budget, the same way. */
  readonly budget_turns: number;
  /** The current token limit (#120): the resolved budget times 1 + budget_extended; 0 for none. */
  readonly limit_tokens: number;
  /** The current turn limit, the same way. */
  readonly limit_turns: number;
  /** Where each resolved field comes from: order, division or default (none). */
  readonly budget_source: BudgetSource;
  /** The extensions the captain granted. */
  readonly budget_extended: number;
  /** What the order's beams spent, summed. */
  readonly usage: OrderUsage;
  readonly created_by: string;
  readonly model: string;
  /** Where the order came from, free-form: tracker:<ref> for a tracker item (#54); absent for none. */
  readonly source?: string;
  /** The mission's latest progress report (#141); absent without one. */
  readonly progress_pct?: number;
  /** That report's note; absent when it has none. */
  readonly progress_note?: string;
  /** Child orders (parent_id this order) not yet ended: a split order waits on them (#155). */
  readonly open_children: number;
  /** The orders this one waits on (#142): no claim takes it until each is done. */
  readonly after: readonly string[];
  /** Those of after not done yet. */
  readonly waiting_on: readonly string[];
  /** Set while its mission is parked, waiting for a resume (#167); absent otherwise. */
  readonly parked_at?: string;
}

/** bridge.BudgetSource. */
export interface BudgetSource {
  readonly tokens: string;
  readonly turns: string;
}

/** An order's usage, summed over its beams: bridge.UsageView. */
export interface OrderUsage {
  readonly input_tokens: number;
  readonly output_tokens: number;
  readonly turns: number;
}

/** A hail: bridge.HailView. */
export interface HailView {
  readonly id: string;
  readonly order_id: string;
  readonly crew_id: string;
  /** The bridge workflow that raised the hail (#152), whose order_id and crew_id are empty; absent for a hail from a beam. */
  readonly deliver_to?: string;
  readonly category: string;
  readonly question: string;
  readonly options: readonly string[];
  /** Free-form JSON from the crew member, or null. */
  readonly context: unknown;
  readonly route: string;
  /** open, decided or timed_out. */
  readonly status: string;
  readonly decided_by: string;
  readonly decision: string;
  readonly rationale: string;
  /** RFC 3339, UTC; absent on a hail from before #147. */
  readonly created_at?: string;
  /** RFC 3339, UTC; absent until decided. */
  readonly decided_at?: string;
  /** Who decided it first (#147), which a countermand leaves alone; absent until decided. */
  readonly original_decided_by?: string;
  /** Command's confidence, on a hail Command decided. */
  readonly confidence?: number;
  /** RFC 3339, UTC: until when the captain may countermand the decision (#153); absent when they can't. */
  readonly countermand_until?: string;
  /** RFC 3339, UTC: when the captain last countermanded it; absent if never. */
  readonly countermanded_at?: string;
}

/** A captain's log entry: bridge.LogEntryView. */
export interface LogEntryView {
  readonly id: number;
  /** RFC 3339, UTC. */
  readonly ts: string;
  /** beam, crew, order, hail, done, fail or bridge. */
  readonly kind: string;
  readonly crew_id: string;
  readonly order_id: string;
  readonly text: string;
  /** Any JSON; null when the stored payload isn't valid JSON. */
  readonly payload: unknown;
}

/** The bridge's condition and counters: bridge.BridgeStatus. */
export interface BridgeStatus {
  /** green, yellow or red. */
  readonly condition: string;
  readonly reasons: readonly string[];
  /** Facts that never change the condition, e.g. "regs.yaml not configured" (#149). */
  readonly info: readonly string[];
  readonly crew: {
    /** Crew members whose tour is provisioning a beam or waiting for its hello (#106). */
    readonly beaming_in: number;
    readonly on_deck: number;
    readonly away: number;
    readonly hailing: number;
    readonly sickbay: number;
  };
  readonly orders: { readonly open: number; readonly away: number };
  readonly hails_open: number;
  readonly beams_live: number;
  readonly last_log_id: number;
  readonly console: string;
  /** Whether standing orders are on (#78). */
  readonly standing_orders: boolean;
}

/** The snapshot event's body, and GET /api/state's: console.Snapshot. */
export interface Snapshot {
  readonly last_log_id: number;
  readonly crew: readonly CrewView[];
  /** Pool, directed, away and hailing, and the finished orders log names. */
  readonly orders: readonly OrderView[];
  /** Open hails, and decided ones still inside their countermand window (#153). */
  readonly hails: readonly HailView[];
  readonly status: BridgeStatus;
  /** The last 200 entries up to last_log_id, oldest first. */
  readonly log: readonly LogEntryView[];
  /** The repo names registered with bridge run --repo, sorted: the repos an order may name (#211). Fixed for the bridge's lifetime. */
  readonly repos: readonly string[];
  /** The registered repos' URLs without credentials, by name: where a push sends an order's work (#4). */
  readonly repo_urls: Readonly<Record<string, string>>;
}

/** GET /api/log's body. */
export interface LogPage {
  readonly entries: readonly LogEntryView[];
  readonly last_id: number;
}

/** POST /api/orders's body: bridge.OrderCreateArgs. */
export interface OrderCreateArgs {
  readonly title: string;
  readonly body?: string;
  readonly division?: string;
  readonly assignee: string;
  readonly priority?: number;
  readonly repos: readonly OrderRepo[];
  readonly budget_tokens?: number;
  readonly model?: string;
  /** Free-form; tracker:<ref> puts the tracker item's text in the body, and its title when title is empty (#54). */
  readonly source?: string;
}

/** POST /api/hails/{id}/answer's body: hails.answer's option and decision. */
export interface HailAnswerArgs {
  readonly option?: number;
  readonly decision?: string;
}

/** POST /api/hails/{id}/countermand's body: hails.countermand's option, decision and reason. */
export interface HailCountermandArgs {
  readonly option?: number;
  readonly decision?: string;
  readonly reason?: string;
}

/** POST /api/orders/{id}/route's body: order.route's assignee and division; absent keeps, empty clears. */
export interface OrderRouteArgs {
  readonly assignee?: string;
  readonly division?: string;
}

/** POST /api/orders/{id}/push's body: order.push's target branch (default bmus/<order>) and force. */
export interface OrderPushArgs {
  readonly target_branch?: string;
  readonly force?: boolean;
}

/** A repo POST /api/orders/{id}/push pushed: bridge.PushedRepo. url has no credentials. */
export interface PushedRepo {
  readonly name: string;
  readonly url: string;
  readonly branch: string;
  readonly commit: string;
}

/**
 * One repo's diff in GET /api/orders/{id}/report: bridge.RepoDiff. diff is
 * unified, from base to the work; truncated says it stops at the bridge's
 * cap; jettisoned says `bmus jettison` deleted the order's branch from the
 * bridge's clone (#108), and diff is then empty.
 */
export interface RepoDiff {
  readonly name: string;
  readonly base: string;
  readonly diff: string;
  readonly truncated: boolean;
  readonly jettisoned: boolean;
}

/** A push of the order's work in GET /api/orders/{id}/report: bridge.ReportPush. */
export interface ReportPush {
  readonly repo: string;
  readonly branch: string;
  readonly commit: string;
  readonly force: boolean;
  readonly at: string;
}

/** GET /api/orders/{id}/report's reply: bridge.OrderReport, a done order's mission report (#104). null and [] where the bridge has no record. */
export interface OrderReport {
  /** The order's view, with when it was created and done. */
  readonly order: OrderView & { readonly created_at: string | null; readonly done_at: string | null };
  readonly crew: CrewView | null;
  /** The beams the order ran on, oldest first. */
  readonly beams: readonly string[];
  readonly report: { readonly summary: string; readonly artifacts: readonly string[] } | null;
  readonly pushes: readonly ReportPush[];
  /** The bytes each repo's diff was capped at. */
  readonly cap: number;
  readonly repos: readonly RepoDiff[];
}

/** POST /api/crew/{id}/reenergize's reply: bridge.CrewActionResult. */
export interface CrewActionResult extends CrewView {
  /** done, requested or under_way. */
  readonly outcome: string;
}

/** PUT /api/standing-orders's body and reply: standing_orders.set's. */
export interface StandingOrders {
  readonly on: boolean;
}

/** The CSRF header every mutation carries. */
export const csrfHeader = "X-BMUS-CSRF";

/** An action the server refused, with its error body's code and message. */
export class ApiError extends Error {
  constructor(
    readonly status: number,
    readonly code: string,
    message: string,
  ) {
    super(message);
    this.name = "ApiError";
  }
}

/** Posts body as JSON with the CSRF token and returns the parsed reply, or throws an ApiError. */
export function postJSON<T>(url: string, csrf: string, body: unknown, f: typeof fetch = fetch): Promise<T> {
  return sendJSON<T>("POST", url, csrf, body, f);
}

/** Sends body as JSON with method and the CSRF token and returns the parsed reply, or throws an ApiError. */
export async function sendJSON<T>(method: string, url: string, csrf: string, body: unknown, f: typeof fetch = fetch): Promise<T> {
  const resp = await f(url, {
    method,
    headers: { "Content-Type": "application/json", [csrfHeader]: csrf },
    body: JSON.stringify(body),
    credentials: "same-origin",
  });
  return replyOf<T>(resp);
}

/** GETs url and returns the parsed reply, or throws an ApiError. */
export async function getJSON<T>(url: string, f: typeof fetch = fetch): Promise<T> {
  return replyOf<T>(await f(url, { credentials: "same-origin" }));
}

async function replyOf<T>(resp: Response): Promise<T> {
  const body: unknown = await resp.json().catch(() => undefined);
  if (!resp.ok) {
    const err = (body as { error?: { code?: unknown; message?: unknown } } | undefined)?.error;
    const code = typeof err?.code === "string" ? err.code : "";
    const message = typeof err?.message === "string" && err.message !== "" ? err.message : `HTTP ${resp.status}`;
    throw new ApiError(resp.status, code, message);
  }
  return body as T;
}

/** Creates an order; the new order arrives through the feed. */
export function createOrder(csrf: string, args: OrderCreateArgs): Promise<OrderView> {
  return postJSON<OrderView>("/api/orders", csrf, args);
}

/** Answers a hail; the decided hail arrives through the feed. */
export function answerHail(csrf: string, hailId: string, args: HailAnswerArgs): Promise<HailView> {
  return postJSON<HailView>(`/api/hails/${encodeURIComponent(hailId)}/answer`, csrf, args);
}

/** POST /api/hails/{id}/countermand's reply: the hail's view, and a note when its order has no live run. */
export interface HailCountermandResult extends HailView {
  readonly note?: string;
}

/** Countermands a decision made without the captain; the hail's new view arrives through the feed. */
export function countermandHail(csrf: string, hailId: string, args: HailCountermandArgs): Promise<HailCountermandResult> {
  return postJSON<HailCountermandResult>(`/api/hails/${encodeURIComponent(hailId)}/countermand`, csrf, args);
}

const orderPath = (id: string, action: string) => `/api/orders/${encodeURIComponent(id)}/${action}`;

/** Belays an order; its mission settles it and the feed brings the change. */
export function belayOrder(csrf: string, id: string): Promise<OrderView> {
  return postJSON<OrderView>(orderPath(id, "belay"), csrf, {});
}

/** Recalls an order to its division's pool; the mission does it at its next checkpoint. */
export function recallOrder(csrf: string, id: string): Promise<OrderView> {
  return postJSON<OrderView>(orderPath(id, "recall"), csrf, {});
}

/** Withdraws an unclaimed order. */
export function withdrawOrder(csrf: string, id: string): Promise<OrderView> {
  return postJSON<OrderView>(orderPath(id, "withdraw"), csrf, {});
}

/** Routes an unclaimed order to a crew member or a division's pool. */
export function routeOrder(csrf: string, id: string, args: OrderRouteArgs): Promise<OrderView> {
  return postJSON<OrderView>(orderPath(id, "route"), csrf, args);
}

/** Pushes a done order's work to its repos' remotes; a diverged target is an ApiError with status 409. */
export function pushOrder(csrf: string, id: string, args: OrderPushArgs): Promise<PushedRepo[]> {
  return postJSON<PushedRepo[]>(orderPath(id, "push"), csrf, args);
}

/** Reads a done order's mission report, its diff included. */
export function orderReport(id: string, f: typeof fetch = fetch): Promise<OrderReport> {
  return getJSON<OrderReport>(orderPath(id, "report"), f);
}

/** Gives a crew member in sickbay a new tour and beam. */
export function reenergizeCrew(csrf: string, id: string): Promise<CrewActionResult> {
  return postJSON<CrewActionResult>(`/api/crew/${encodeURIComponent(id)}/reenergize`, csrf, {});
}

/** Turns standing orders on or off; the status event brings the change. */
export function setStandingOrders(csrf: string, on: boolean): Promise<StandingOrders> {
  return sendJSON<StandingOrders>("PUT", "/api/standing-orders", csrf, { on });
}

/** Reads the newest limit log entries below before, oldest first. */
export function logBefore(before: number, limit: number): Promise<LogPage> {
  return getJSON<LogPage>(`/api/log?before=${before}&limit=${limit}`);
}
