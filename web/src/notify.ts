// Hail notifications (#76, console plan "Notifications"): the browser's
// Notification API hands them to the OS. The page asks for permission only
// from a button, never on load. The store says which hails are fresh (a
// live event opened them); announce raises one notification each. Question
// text goes in the body, which the API renders as plain text.

import type { HailView } from "./api.js";
import type { State } from "./store.js";
import { bridgeHailOrder, hailKind } from "./view.js";

/** The page's notification permission, or "unsupported" without the API. */
export type Permission = NotificationPermission | "unsupported";

/** The parts of the Notification API the console uses, for tests to fake. */
export interface NotifyEnv {
  readonly permission: () => Permission;
  readonly request: () => Promise<Permission>;
  readonly show: (title: string, opts: { body: string; tag: string }, onClick: () => void) => void;
  /** Calls onChange when the permission changes outside the page; returns a function that stops. */
  readonly watch: (onChange: (p: Permission) => void) => () => void;
}

/**
 * A notification's title and body for a hail: the crew member and order,
 * then the question. A bridge-raised hail (#152) has no crew member: the
 * title names the bridge and the order it is about, the findings review
 * as a review, a campaign's land and stall hails (#187) as a landing and
 * a stall: the head the land pushes, the order the task stalled on.
 */
export function hailNotice(state: State, h: HailView): { title: string; body: string; tag: string } {
  const kind = hailKind(h);
  if (kind !== "crew") {
    const order = bridgeHailOrder(h);
    const what = bridgeTitles[kind] ?? "Hail from the bridge";
    return { title: order ? `${what} · ${order}` : what, body: h.question, tag: h.id };
  }
  const c = state.crew.get(h.crew_id);
  return { title: `Hail from ${c?.name || h.crew_id} · ${h.order_id}`, body: h.question, tag: h.id };
}

/** A bridge hail's notification title by its kind (hailKind). */
const bridgeTitles: Partial<Record<ReturnType<typeof hailKind>, string>> = {
  review: "Review from the bridge",
  land: "Landing from the bridge",
  stalled: "Stall from the bridge",
};

/** Whether a hail is the captain's to answer, so worth a notification: Command's advisory triage (#155) never is. */
export function notifies(h: HailView): boolean {
  return hailKind(h) !== "triage";
}

/**
 * Raises a notification for each of ids still open in state, when
 * permission is granted, and returns ids: the caller acknowledges them
 * either way, so a hail is announced at most once.
 */
export function announce(env: NotifyEnv, state: State, ids: readonly string[], onClick: (hailId: string) => void): readonly string[] {
  if (env.permission() !== "granted") return ids;
  for (const id of ids) {
    const h = state.hail.get(id);
    if (!h || h.status !== "open" || !notifies(h)) continue;
    const n = hailNotice(state, h);
    env.show(n.title, { body: n.body, tag: n.tag }, () => onClick(id));
  }
  return ids;
}

/** The id of a hail's card in the hails panel. */
export function hailCardId(hailId: string): string {
  return `hail-${hailId}`;
}

/** The class a card has while focused from a notification; only then does it show an outline. */
export const notifiedClass = "notified";

/** Brings the window forward and moves focus to a hail's card. */
export function focusHail(hailId: string, win: Window = window) {
  win.focus();
  const el = win.document.getElementById(hailCardId(hailId));
  if (!el) return;
  el.classList.add(notifiedClass);
  el.addEventListener("blur", () => el.classList.remove(notifiedClass), { once: true });
  el.scrollIntoView({ block: "center" });
  el.focus();
}

/** The parts of navigator.permissions watchPermission uses, for tests to fake. */
export interface PermissionsLike {
  query(desc: { name: "notifications" }): Promise<Pick<PermissionStatus, "state" | "addEventListener" | "removeEventListener">>;
}

/**
 * Calls onChange with the notification permission each time
 * permissions reports it changed (the captain changed it in the site
 * settings). A no-op without the Permissions API or when the query fails.
 */
export function watchPermission(perms: PermissionsLike | undefined, onChange: (p: Permission) => void): () => void {
  let stopped = false;
  let stop = () => {};
  try {
    perms?.query({ name: "notifications" }).then(
      (status) => {
        if (stopped) return;
        const changed = () => onChange(status.state === "prompt" ? "default" : status.state);
        status.addEventListener("change", changed);
        stop = () => status.removeEventListener("change", changed);
      },
      () => {},
    );
  } catch {
    // no Permissions API for notifications
  }
  return () => {
    stopped = true;
    stop();
  };
}

/** The browser's NotifyEnv. */
export const browserNotifyEnv: NotifyEnv = {
  permission: () => (typeof Notification === "undefined" ? "unsupported" : Notification.permission),
  request: async () => (typeof Notification === "undefined" ? "unsupported" : Notification.requestPermission()),
  show: (title, opts, onClick) => {
    let n: Notification;
    try {
      n = new Notification(title, opts);
    } catch {
      return; // e.g. a browser that only notifies from a service worker
    }
    n.onclick = () => {
      onClick();
      n.close();
    };
  },
  watch: (onChange) => watchPermission(typeof navigator === "undefined" ? undefined : navigator.permissions, onChange),
};
