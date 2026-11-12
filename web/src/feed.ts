// The feed client: GET /api/events as an EventSource, its events mapped
// onto store actions. The browser reconnects on its own with
// Last-Event-ID; the server then replays the entries since, or sends a
// fresh snapshot, which replaces the store. A stream the browser gives
// up on (a non-200 reply) is reopened after a pause, unless the session
// is gone.

import type { BridgeStatus, CrewView, HailView, LogEntryView, OrderView, Snapshot } from "./api.js";
import type { Action } from "./store.js";

/** The feed's connection, for the header's indicator. */
export type Link = "connecting" | "live" | "reconnecting" | "lost";

/** The parts of EventSource connectFeed uses, for tests to fake. */
export interface FeedSource {
  readonly readyState: number;
  onopen: ((ev: Event) => unknown) | null;
  onerror: ((ev: Event) => unknown) | null;
  addEventListener(type: string, listener: (ev: MessageEvent<string>) => void): void;
  close(): void;
}

export interface FeedEnv {
  readonly open: (url: string) => FeedSource;
  /** Whether the session cookie is still valid (GET /api/session). */
  readonly sessionValid: () => Promise<boolean>;
  readonly setTimeout: (f: () => void, ms: number) => unknown;
  readonly clearTimeout: (t: unknown) => void;
}

/** EventSource.CLOSED. */
const closed = 2;
/** How long to wait before reopening a stream the browser closed. */
export const reopenDelay = 3000;

/** The events whose data is one store action's payload. */
const events = ["snapshot", "log", "status", "crew", "order", "hail"] as const;

/** Returns the action for an event's type and data, or undefined if the data isn't JSON. */
export function actionFor(type: (typeof events)[number], data: string): Action | undefined {
  let v: unknown;
  try {
    v = JSON.parse(data);
  } catch {
    return undefined;
  }
  if (typeof v !== "object" || v === null) return undefined;
  switch (type) {
    case "snapshot":
      return { type, snapshot: v as Snapshot };
    case "log":
      return { type, entry: v as LogEntryView };
    case "status":
      return { type, status: v as BridgeStatus };
    case "crew":
      return { type, view: v as CrewView };
    case "order":
      return { type, view: v as OrderView };
    case "hail":
      return { type, view: v as HailView };
  }
}

/** Opens the feed and returns a function that closes it. */
export function connectFeed(env: FeedEnv, dispatch: (a: Action) => void, onLink: (l: Link) => void): () => void {
  let src: FeedSource | undefined;
  let timer: unknown;
  let stopped = false;

  const open = () => {
    const s = env.open("/api/events");
    src = s;
    s.onopen = () => onLink("live");
    s.onerror = () => {
      if (s.readyState !== closed) {
        onLink("reconnecting"); // the browser retries on its own
        return;
      }
      s.close();
      onLink("reconnecting");
      env.sessionValid().then(
        (ok) => {
          if (stopped) return;
          if (!ok) onLink("lost");
          else timer = env.setTimeout(open, reopenDelay);
        },
        () => {
          if (!stopped) timer = env.setTimeout(open, reopenDelay);
        },
      );
    };
    for (const type of events) {
      s.addEventListener(type, (ev) => {
        const a = actionFor(type, ev.data);
        if (a) dispatch(a);
      });
    }
  };

  onLink("connecting");
  open();
  return () => {
    stopped = true;
    env.clearTimeout(timer);
    src?.close();
  };
}

/** The browser's FeedEnv. */
export const browserFeedEnv: FeedEnv = {
  open: (url) => new EventSource(url, { withCredentials: true }),
  sessionValid: async () => (await fetch("/api/session", { credentials: "same-origin" })).status !== 401,
  setTimeout: (f, ms) => window.setTimeout(f, ms),
  clearTimeout: (t) => window.clearTimeout(t as number),
};
