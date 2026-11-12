// View helpers: what the panels show, derived from the store. Pure
// functions, no DOM. Tones name the design's palette; app.css maps each
// to its colour (tone-<name> for text, bg-<name> for fills).

import type { BridgeStatus, CrewView, HailView, LogEntryView, OrderRouteArgs, OrderView } from "./api.js";
import type { State } from "./store.js";

export type Tone = "green" | "gold" | "blue" | "red" | "cream" | "grey" | "slate";

const pad2 = (n: number) => String(n).padStart(2, "0");

/** The stardate: the local year and day of the year, "2026.301". Decoration, from the clock. */
export function stardate(d: Date): string {
  const day = (Date.UTC(d.getFullYear(), d.getMonth(), d.getDate()) - Date.UTC(d.getFullYear(), 0, 0)) / 86400000;
  return `${d.getFullYear()}.${String(day).padStart(3, "0")}`;
}

/** Local "HH:MM". */
export function clockHM(d: Date): string {
  return `${pad2(d.getHours())}:${pad2(d.getMinutes())}`;
}

/** A log entry's local time, "HH:MM:SS"; "--:--:--" for a timestamp that doesn't parse. */
export function logTime(ts: string): string {
  const d = new Date(ts);
  if (Number.isNaN(d.getTime())) return "--:--:--";
  return `${pad2(d.getHours())}:${pad2(d.getMinutes())}:${pad2(d.getSeconds())}`;
}

/** The condition's label and tone; an unknown condition shows as itself, grey. */
export function conditionInfo(condition: string | undefined): { label: string; tone: Tone } {
  switch (condition) {
    case "green":
      return { label: "GREEN", tone: "green" };
    case "yellow":
      return { label: "YELLOW", tone: "gold" };
    case "red":
      return { label: "RED", tone: "red" };
    default:
      return { label: condition ? condition.toUpperCase() : "UNKNOWN", tone: "grey" };
  }
}

const divisions: Record<string, { label: string; tone: Tone }> = {
  command: { label: "Command", tone: "gold" },
  science: { label: "Science", tone: "blue" },
  ops: { label: "Operations", tone: "red" },
  operations: { label: "Operations", tone: "red" },
};

/** A division's label and tone. The manifest's three have their colours; others are grey. */
export function divisionInfo(division: string): { label: string; tone: Tone } {
  const known = divisions[division];
  if (known) return known;
  if (division === "") return { label: "No division", tone: "grey" };
  return { label: division.charAt(0).toUpperCase() + division.slice(1), tone: "grey" };
}

/** A crew member's status: its label, tone and whether its light blinks. */
export function crewStatusInfo(status: string): { label: string; tone: Tone; blink: boolean } {
  switch (status) {
    case "on_deck":
      return { label: "On deck", tone: "green", blink: false };
    case "away":
      return { label: "Away", tone: "gold", blink: false };
    case "hailing":
      return { label: "Hailing", tone: "blue", blink: true };
    case "sickbay":
      return { label: "Sickbay", tone: "red", blink: true };
    case "dismissed":
      return { label: "Dismissed", tone: "grey", blink: false };
    default:
      return { label: status, tone: "grey", blink: false };
  }
}

/** The alert levels, in order, with order.create's priority values. */
export const priorities = [
  { value: 0, key: "routine", label: "Routine" },
  { value: 1, key: "yellow", label: "Yellow alert" },
  { value: 2, key: "red", label: "Red alert" },
] as const;

/** A priority's label and chip tone. */
export function priorityInfo(priority: number): { label: string; tone: Tone } {
  if (priority >= 2) return { label: "Red alert", tone: "red" };
  if (priority === 1) return { label: "Yellow alert", tone: "gold" };
  return { label: "Routine", tone: "slate" };
}

/** A log entry kind's tag and tone. */
export function logTag(kind: string): { tag: string; tone: Tone } {
  switch (kind) {
    case "beam":
      return { tag: "BEAM", tone: "gold" };
    case "order":
      return { tag: "ORDER", tone: "cream" };
    case "hail":
      return { tag: "HAIL", tone: "blue" };
    case "done":
      return { tag: "BACK", tone: "green" };
    case "fail":
      return { tag: "LOST", tone: "red" };
    case "crew":
      return { tag: "CREW", tone: "cream" };
    case "bridge":
      return { tag: "BRIDGE", tone: "grey" };
    default:
      return { tag: kind.toUpperCase().slice(0, 6), tone: "grey" };
  }
}

/** A crew member's short name, as the design prints it: the id in capitals. */
export function shortName(crewId: string): string {
  return crewId.toUpperCase();
}

/** Crew members on the manifest: everyone but the dismissed, by id. */
export function manifest(state: State): CrewView[] {
  return [...state.crew.values()].filter((c) => c.status !== "dismissed").sort((a, b) => cmp(a.id, b.id));
}

const byPriority = (a: OrderView, b: OrderView) => b.priority - a.priority || cmp(a.id, b.id);

/** Orders not yet claimed (pool and directed), red alerts first. */
export function openOrders(state: State): OrderView[] {
  return [...state.order.values()].filter((o) => o.state === "pool" || o.state === "directed").sort(byPriority);
}

/**
 * An order's latest progress, from its view (#141): the pct clamped to
 * 0-100 and the note ("" without one). Undefined when the order carries
 * no report.
 */
export function orderProgress(o: OrderView): { pct: number; note: string } | undefined {
  const pct = clampPct(o.progress_pct);
  return pct === undefined ? undefined : { pct, note: o.progress_note ?? "" };
}

/**
 * Returns the percentage a progress entry reports, for the log's rows: an
 * entry naming an order whose payload has a numeric pct (the comms pump's
 * progress entries, `{"seq", "pct"}`), clamped to 0-100. Undefined for any
 * other entry, a null payload included.
 */
export function progressPct(e: LogEntryView): number | undefined {
  if (e.order_id === "" || typeof e.payload !== "object" || e.payload === null) return undefined;
  return clampPct((e.payload as { pct?: unknown }).pct);
}

function clampPct(pct: unknown): number | undefined {
  if (typeof pct !== "number" || !Number.isFinite(pct)) return undefined;
  return Math.min(100, Math.max(0, pct));
}

/** Orders on a mission (away and hailing), red alerts first. */
export function awayOrders(state: State): OrderView[] {
  return [...state.order.values()].filter((o) => o.state === "away" || o.state === "hailing").sort(byPriority);
}

/** The order a crew member is on, if any: an away or hailing order assigned to it. */
export function currentOrder(state: State, crewId: string): OrderView | undefined {
  return awayOrders(state).find((o) => o.assignee === crewId);
}

/** How many directed orders wait in a crew member's queue. */
export function queuedFor(state: State, crewId: string): number {
  let n = 0;
  for (const o of state.order.values()) if (o.state === "directed" && o.assignee === crewId) n++;
  return n;
}

/** Open hails, oldest first. */
export function openHails(state: State) {
  return [...state.hail.values()].filter((h) => h.status === "open").sort((a, b) => cmp(a.id, b.id));
}

/**
 * Whether the captain may still countermand h at now (#153): a decision
 * made without them (Command's, or a captain timeout's default_option)
 * whose countermand_until hasn't passed. The bridge decides in the end:
 * the console's clock may be off, and it only hides the control.
 */
export function canCountermand(h: HailView, now: Date): boolean {
  if (h.status !== "decided" || h.countermand_until === undefined) return false;
  if (h.original_decided_by !== "command" && h.original_decided_by !== "policy") return false;
  const until = Date.parse(h.countermand_until);
  return Number.isFinite(until) && now.getTime() <= until;
}

/** Decided hails the captain may still countermand at now, oldest first. */
export function countermandableHails(state: State, now: Date): HailView[] {
  return [...state.hail.values()].filter((h) => canCountermand(h, now)).sort((a, b) => cmp(a.id, b.id));
}

/** Who decided a hail, as the countermand card says it. A policy decision with a window is the captain timeout's. */
export function decidedByLabel(h: HailView): string {
  switch (h.decided_by) {
    case "command":
      return "Command";
    case "policy":
      return "the captain timeout";
    case "captain":
      return h.countermanded_at ? "you (countermanded)" : "you";
    default:
      return h.decided_by;
  }
}

/** A crew member as the composer's and the route picker's options show them: "SORELL · Science (on deck)". */
export function crewOptionLabel(c: CrewView): string {
  return `${shortName(c.id)} · ${divisionInfo(c.division).label} (${crewStatusInfo(c.status).label.toLowerCase()})`;
}

/** Whether an order can be sent to c: route and order.create refuse a crew member in sickbay (or dismissed). */
export function canTake(c: CrewView): boolean {
  return c.status !== "sickbay" && c.status !== "dismissed";
}

/** The route picker's value for an unclaimed order: the crew member it is directed to, else "pool". */
export function routeValue(o: OrderView): string {
  return o.state === "directed" && o.assignee !== "" ? o.assignee : "pool";
}

/**
 * order.route's args for a pick of the route picker: "pool" clears the
 * assignee, returning the order to its division's pool; a crew id
 * directs it to them. The division is kept either way, as `bmus order
 * route --assignee` keeps it.
 */
export function routeArgs(pick: string): OrderRouteArgs {
  return { assignee: pick === "pool" ? "" : pick };
}

/** The standing orders switch: on is undefined until the first status. */
export function standingOrdersInfo(status: BridgeStatus | undefined): { on: boolean | undefined; label: string; hint: string } {
  if (status === undefined || typeof status.standing_orders !== "boolean") {
    return { on: undefined, label: "", hint: "Free crew take open orders from the pool, matching their division, while standing orders are on." };
  }
  return status.standing_orders
    ? { on: true, label: "on", hint: "Free crew take open orders from the pool, matching their division." }
    : { on: false, label: "off", hint: "Crew only move on directed orders." };
}

/** Divisions to offer for a pool order: those of the crew on the manifest, sorted. */
export function knownDivisions(state: State): string[] {
  const set = new Set<string>();
  for (const c of state.crew.values()) if (c.division !== "" && c.status !== "dismissed") set.add(c.division);
  return [...set].sort(cmp);
}

/**
 * The repo the composer sends (#211): chosen when the bridge registers it,
 * else the first registered one; "" when the bridge registers none.
 */
export function composerRepo(repos: readonly string[], chosen: string): string {
  return repos.includes(chosen) ? chosen : (repos[0] ?? "");
}

/** A row of the mission record. */
export interface RecordRow {
  readonly orderId: string;
  readonly title: string;
  readonly crewId: string;
  readonly result: string;
  readonly tone: Tone;
  /** The done entry's summary or the fail entry's text: plain text. */
  readonly summary: string;
  /** A split order's open child orders, which a belay of it reaches (#146); undefined for any other. */
  readonly splitChildren?: number;
}

/**
 * The belay confirm's prompt for order id (#146): a belay cascades to the
 * order's open child orders, and says how many.
 */
export function belayPrompt(id: string, openChildren: number): string {
  if (openChildren <= 0) return `Belay ${id}? The order is abandoned.`;
  const n = openChildren === 1 ? "its 1 open child order" : `its ${openChildren} open child orders`;
  return `Belays ${id} and ${n}. Belay them all?`;
}

const terminal: Record<string, { result: string; tone: Tone }> = {
  done: { result: "Complete", tone: "green" },
  failed: { result: "Failed", tone: "red" },
  belayed: { result: "Belayed", tone: "grey" },
  withdrawn: { result: "Withdrawn", tone: "grey" },
};

/**
 * An order's mission record result: a finished order's, or a split one's
 * (#155): its mission is over, and it waits on its open child orders.
 * Undefined for an order still to run or running.
 */
export function recordResult(o: OrderView): { result: string; tone: Tone } | undefined {
  if (o.state === "split") {
    const n = o.open_children;
    return { result: `Split: waiting on ${n} ${n === 1 ? "child" : "children"}`, tone: "blue" };
  }
  return terminal[o.state];
}

/**
 * The mission record: finished orders, and split ones waiting on their
 * children (recordResult), most recent first, at most limit.
 * An order the store has a view of is decided by its state, placed at its
 * newest entry in the recent log. The feed sends terminal views, and a
 * snapshot carries the finished orders its log names (#78): after a
 * reload, belayed and withdrawn orders, which have no done or fail entry,
 * are still listed. An order without a view is decided by its newest done
 * or fail entry: a mission's done entry is followed by "crew back on deck"
 * (kind crew), so the newest entry of any kind won't do. Finished orders
 * the store has with no entry left in the log come after.
 */
export function missionRecord(state: State, limit = 6): RecordRow[] {
  const rows: RecordRow[] = [];
  const seen = new Set<string>();
  const summaries = new Map<string, string>();
  for (const e of state.log) {
    if (e.order_id !== "" && (e.kind === "done" || e.kind === "fail")) summaries.set(e.order_id, summaryOf(e));
  }
  for (let i = state.log.length - 1; i >= 0 && rows.length < limit; i--) {
    const e = state.log[i] as LogEntryView;
    if (e.order_id === "" || seen.has(e.order_id)) continue;
    const o = state.order.get(e.order_id);
    let t: { result: string; tone: Tone } | undefined;
    if (o) {
      seen.add(e.order_id); // the view decides, finished or not
      t = recordResult(o);
    } else if (e.kind === "done" || e.kind === "fail") {
      seen.add(e.order_id);
      t = terminal[e.kind === "done" ? "done" : "failed"];
    }
    if (!t) continue;
    rows.push({
      orderId: e.order_id,
      title: o?.title ?? "",
      crewId: o?.assignee || e.crew_id,
      result: t.result,
      tone: t.tone,
      summary: summaries.get(e.order_id) ?? "",
      ...splitChildren(o),
    });
  }
  const rest = [...state.order.values()].filter((o) => !seen.has(o.id) && recordResult(o)).sort((a, b) => cmp(b.id, a.id));
  for (const o of rest) {
    if (rows.length >= limit) break;
    const t = recordResult(o) as { result: string; tone: Tone };
    rows.push({ orderId: o.id, title: o.title, crewId: o.assignee, result: t.result, tone: t.tone, summary: "", ...splitChildren(o) });
  }
  return rows;
}

/** A record row's splitChildren: a split order's open children. */
function splitChildren(o: OrderView | undefined): { splitChildren?: number } {
  return o?.state === "split" ? { splitChildren: o.open_children } : {};
}

/** An entry's summary: its payload's summary (a done entry's report), else its text. */
export function summaryOf(e: LogEntryView): string {
  const s = typeof e.payload === "object" && e.payload !== null ? (e.payload as { summary?: unknown }).summary : undefined;
  return typeof s === "string" ? s : e.text;
}

/** A transporter pad's look for a crew member. */
export function padInfo(c: CrewView): { mode: "standby" | "on" | "hail" | "lost"; label: string; tone: Tone } {
  switch (c.status) {
    case "away":
      return { mode: "on", label: c.current_beam || "energizing", tone: "gold" };
    case "hailing":
      return { mode: "hail", label: "hailing", tone: "blue" };
    case "sickbay":
      return { mode: "lost", label: "no signal", tone: "red" };
    default:
      return { mode: "standby", label: c.current_beam ? "standby" : "no beam", tone: "grey" };
  }
}

/** A hail's context as one line of text, or "" for none. */
export function contextText(context: unknown): string {
  if (context === null || context === undefined) return "";
  if (typeof context === "object" && Object.keys(context).length === 0) return "";
  try {
    return JSON.stringify(context);
  } catch {
    return "";
  }
}

function cmp(a: string, b: string): number {
  return a < b ? -1 : a > b ? 1 : 0;
}

/* Push (#4) ---------------------------------------------------------------- */

/** Whether the console offers Push for o: only a done order's work can be pushed, and only an order with repos has any. */
export function canPush(o: OrderView | undefined): o is OrderView {
  return o?.state === "done" && o.repos.length > 0;
}

/** An order's branch in the bridge's clone, and a push's default target. */
export function orderBranch(id: string): string {
  return `bmus/${id}`;
}

/** A repo of an order and its registered URL, without credentials; "" when the snapshot has none. */
export interface PushRemote {
  readonly name: string;
  readonly url: string;
}

/** The remotes a push of o sends its work to: its repos' registered URLs. */
export function pushRemotes(state: State, o: OrderView): PushRemote[] {
  return o.repos.map((r) => ({ name: r.name, url: state.repoUrls[r.name] ?? "" }));
}

/**
 * The push confirm's prompt: "Push bmus/ORD-0007 to <url> as <target>?",
 * each repo's URL named, and the repo's name too when there are several.
 */
export function pushPrompt(id: string, remotes: readonly PushRemote[], target: string): string {
  return `Push ${orderBranch(id)} to ${remoteNames(remotes)} as ${target}?`;
}

/** remotes named for a prompt: each URL, with its repo's name when there are several. */
function remoteNames(remotes: readonly PushRemote[]): string {
  const where = remotes.map((r) => {
    const url = r.url || `${r.name}'s remote`;
    return remotes.length > 1 && r.url ? `${url} (${r.name})` : url;
  });
  return where.join(" and ") || "its remote";
}

/**
 * The second confirm, offered only after the remote rejected a push
 * (409): it names every remote the force push overwrites, as pushPrompt
 * does.
 */
export function forcePushPrompt(id: string, remotes: readonly PushRemote[], target: string): string {
  const what = remotes.length > 1 ? `The remote branches' commits` : `The remote branch's commits`;
  return `Force push ${orderBranch(id)} to ${remoteNames(remotes)} as ${target}? ${what} that ${orderBranch(id)} doesn't have are overwritten.`;
}

/** What a rejected push (409) says: the target has likely diverged, and force is the way past it. */
export function pushRejectedText(target: string, message: string): string {
  return `The remote rejected the push: ${target} has likely diverged from this order's work. ${message}`;
}

const githubName = /^[A-Za-z0-9_.-]+$/;

/**
 * The GitHub "open a pull request" link for a push of branch to url, or
 * undefined unless url is a github.com repo: git@github.com:o/r(.git),
 * ssh://[git@]github.com/o/r(.git) or https://github.com/o/r(.git). The
 * host must be github.com itself, so a look-alike such as
 * github.com.example.com gets no link. The branch is URL-encoded segment
 * by segment, keeping its slashes.
 */
export function githubCompareURL(url: string, branch: string): string | undefined {
  let path: string | undefined;
  const scp = /^(?:[^@/:]+@)?github\.com:(.*)$/i.exec(url);
  if (scp && !url.includes("://")) {
    path = scp[1];
  } else {
    let u: URL;
    try {
      u = new URL(url);
    } catch {
      return undefined;
    }
    if (!["https:", "ssh:"].includes(u.protocol) || u.hostname.toLowerCase() !== "github.com" || u.search !== "" || u.hash !== "") return undefined;
    if (u.protocol === "https:" && (u.port !== "" || u.username !== "" || u.password !== "")) return undefined;
    path = u.pathname.replace(/^\//, "");
  }
  const parts = (path ?? "").replace(/\/$/, "").split("/");
  if (parts.length !== 2) return undefined;
  const owner = parts[0] ?? "";
  const repo = (parts[1] ?? "").replace(/\.git$/, "");
  if (!githubName.test(owner) || !githubName.test(repo) || repo === "." || repo === "..") return undefined;
  const ref = branch.split("/").map(encodeURIComponent).join("/");
  return `https://github.com/${owner}/${repo}/compare/${ref}?expand=1`;
}

/** s quoted for a POSIX shell: as it is when it holds only safe characters, else in single quotes. */
export function shellQuote(s: string): string {
  if (/^[A-Za-z0-9_/.,:@%+=-]+$/.test(s)) return s;
  return `'${s.replaceAll("'", `'\\''`)}'`;
}

/**
 * The "Copy diff command" text for done order o: for each repo with a
 * base commit, `git -C '<home>/repos/<name>' diff <base>...bmus/<order>`,
 * one per line. The clone's path is always single-quoted. A base that
 * isn't a full commit id is skipped, and an order id outside
 * [A-Za-z0-9-] gives none. Undefined when there is nothing to diff, or no
 * home yet.
 */
export function diffCommand(home: string, o: OrderView): string | undefined {
  if (home === "" || !/^[A-Za-z0-9-]+$/.test(o.id)) return undefined;
  const lines = o.repos.flatMap((r) => {
    const base = o.base_commits?.[r.name];
    if (base === undefined || !/^[0-9a-f]{40,64}$/.test(base)) return [];
    const dir = `'${`${home.replace(/\/+$/, "")}/repos/${r.name}`.replaceAll("'", `'\\''`)}'`;
    return [`git -C ${dir} diff ${shellQuote(`${base}...${orderBranch(o.id)}`)}`];
  });
  return lines.length === 0 ? undefined : lines.join("\n");
}

/** The branch order id's work was last pushed to, from its "repo <name> pushed to <branch> at <commit>" entries in the log; undefined if none. */
export function pushedTo(state: State, id: string): string | undefined {
  for (let i = state.log.length - 1; i >= 0; i--) {
    const e = state.log[i] as LogEntryView;
    if (e.order_id !== id || e.kind !== "order") continue;
    const m = /^repo \S+ pushed to (\S+) at \S+/.exec(e.text);
    if (m) return m[1];
  }
  return undefined;
}
