import { afterEach, describe, expect, it, vi } from "vitest";
import { actionFor, connectFeed, reopenDelay, type FeedEnv, type FeedSource, type Link } from "../src/feed.js";
import type { Action } from "../src/store.js";

class FakeSource implements FeedSource {
  readyState = 0;
  onopen: ((ev: Event) => unknown) | null = null;
  onerror: ((ev: Event) => unknown) | null = null;
  closed = false;
  readonly listeners = new Map<string, (ev: MessageEvent<string>) => void>();
  constructor(readonly url: string) {}
  addEventListener(type: string, listener: (ev: MessageEvent<string>) => void) {
    this.listeners.set(type, listener);
  }
  close() {
    this.closed = true;
    this.readyState = 2;
  }
  emit(type: string, data: string) {
    this.listeners.get(type)?.({ data } as MessageEvent<string>);
  }
}

function fakeEnv(valid: boolean) {
  const sources: FakeSource[] = [];
  const timers: { f: () => void; ms: number }[] = [];
  const env: FeedEnv = {
    open: (url) => {
      const s = new FakeSource(url);
      sources.push(s);
      return s;
    },
    sessionValid: async () => valid,
    setTimeout: (f, ms) => timers.push({ f, ms }),
    clearTimeout: () => {},
  };
  return { env, sources, timers };
}

describe("actionFor", () => {
  it("maps each event to its action", () => {
    expect(actionFor("log", '{"id":3}')).toEqual({ type: "log", entry: { id: 3 } });
    expect(actionFor("order", '{"id":"ORD-0001"}')).toEqual({ type: "order", view: { id: "ORD-0001" } });
    expect(actionFor("status", '{"condition":"red"}')).toEqual({ type: "status", status: { condition: "red" } });
    expect(actionFor("snapshot", '{"last_log_id":0}')).toEqual({ type: "snapshot", snapshot: { last_log_id: 0 } });
  });

  it("drops data that isn't a JSON object", () => {
    expect(actionFor("log", "{nope")).toBeUndefined();
    expect(actionFor("log", "null")).toBeUndefined();
  });
});

describe("connectFeed", () => {
  it("dispatches events and reports the link", () => {
    const { env, sources } = fakeEnv(true);
    const got: Action[] = [];
    const links: Link[] = [];
    const stop = connectFeed(env, (a) => got.push(a), (l) => links.push(l));
    const src = sources[0] as FakeSource;
    expect(src.url).toBe("/api/events");
    src.onopen?.(new Event("open"));
    src.emit("log", '{"id":1}');
    src.emit("hail", '{"id":"H-0001"}');
    src.readyState = 0;
    src.onerror?.(new Event("error")); // the browser retries
    expect(got.map((a) => a.type)).toEqual(["log", "hail"]);
    expect(links).toEqual(["connecting", "live", "reconnecting"]);
    expect(sources).toHaveLength(1);
    stop();
    expect(src.closed).toBe(true);
  });

  it("reopens a closed stream after a pause while the session is valid", async () => {
    const { env, sources, timers } = fakeEnv(true);
    connectFeed(env, () => {}, () => {});
    const src = sources[0] as FakeSource;
    src.readyState = 2;
    src.onerror?.(new Event("error"));
    await Promise.resolve();
    expect(timers).toHaveLength(1);
    expect(timers[0]?.ms).toBe(reopenDelay);
    timers[0]?.f();
    expect(sources).toHaveLength(2);
  });

  it("reports a lost session instead of reopening", async () => {
    const { env, sources, timers } = fakeEnv(false);
    const links: Link[] = [];
    connectFeed(env, () => {}, (l) => links.push(l));
    const src = sources[0] as FakeSource;
    src.readyState = 2;
    src.onerror?.(new Event("error"));
    await Promise.resolve();
    expect(timers).toHaveLength(0);
    expect(links.at(-1)).toBe("lost");
  });
});

describe("connectFeed with a failing session check", () => {
  afterEach(() => {
    vi.useRealTimers();
  });

  it("reopens after the pause when the check rejects", async () => {
    vi.useFakeTimers();
    const sources: FakeSource[] = [];
    const env: FeedEnv = {
      open: (url) => {
        const s = new FakeSource(url);
        sources.push(s);
        return s;
      },
      sessionValid: () => Promise.reject(new Error("network down")),
      setTimeout: (f, ms) => setTimeout(f, ms),
      clearTimeout: (t) => clearTimeout(t as ReturnType<typeof setTimeout>),
    };
    const links: Link[] = [];
    connectFeed(env, () => {}, (l) => links.push(l));
    const src = sources[0] as FakeSource;
    src.readyState = 2;
    src.onerror?.(new Event("error"));
    await vi.advanceTimersByTimeAsync(reopenDelay - 1);
    expect(sources).toHaveLength(1);
    await vi.advanceTimersByTimeAsync(1);
    expect(sources).toHaveLength(2);
    expect(links).not.toContain("lost");
  });
});
