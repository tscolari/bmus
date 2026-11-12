import { renderToString } from "preact-render-to-string";
import { describe, expect, it } from "vitest";
import { announce, focusHail, hailNotice, notifiedClass, watchPermission, type NotifyEnv, type Permission, type PermissionsLike } from "../src/notify.js";
import { HailCard, NotifyReadout } from "../src/panels.js";
import { emptyState, reduce } from "../src/store.js";
import { crew, hail, order, snapshot } from "./fixtures.js";

function fakeEnv(permission: Permission) {
  const shown: { title: string; body: string; tag: string; click: () => void }[] = [];
  const env: NotifyEnv = {
    permission: () => permission,
    request: async () => permission,
    show: (title, opts, click) => shown.push({ title, ...opts, click }),
    watch: () => () => {},
  };
  return { env, shown };
}

const state = reduce(emptyState, {
  type: "snapshot",
  snapshot: snapshot({
    crew: [crew("sorell", { name: "Lt. Sorell" }), crew("tovan", { name: "" })],
    hails: [
      hail("H-0001", { crew_id: "sorell", order_id: "ORD-0001", question: "Run <b>x</b>?" }),
      hail("H-0002", { crew_id: "tovan", order_id: "ORD-0002" }),
      hail("H-0003", { status: "decided" }),
    ],
  }),
});

describe("hailNotice", () => {
  it("names the crew member and order; the question is the body as is", () => {
    expect(hailNotice(state, state.hail.get("H-0001")!)).toEqual({ title: "Hail from Lt. Sorell · ORD-0001", body: "Run <b>x</b>?", tag: "H-0001" });
  });

  it("falls back to the crew id", () => {
    expect(hailNotice(state, state.hail.get("H-0002")!).title).toBe("Hail from tovan · ORD-0002");
    expect(hailNotice(state, hail("H-0009", { crew_id: "ghost" })).title).toBe("Hail from ghost · ORD-0001");
  });
});

describe("announce", () => {
  it("shows one notification per open hail when granted, and clicking reports the hail", () => {
    const { env, shown } = fakeEnv("granted");
    const clicked: string[] = [];
    const ids = announce(env, state, ["H-0001", "H-0002", "H-0003", "H-0404"], (id) => clicked.push(id));
    expect(ids).toEqual(["H-0001", "H-0002", "H-0003", "H-0404"]);
    expect(shown.map((n) => n.tag)).toEqual(["H-0001", "H-0002"]);
    shown[1]?.click();
    expect(clicked).toEqual(["H-0002"]);
  });

  it("shows nothing unless granted, and still acknowledges the ids", () => {
    for (const p of ["default", "denied", "unsupported"] as const) {
      const { env, shown } = fakeEnv(p);
      expect(announce(env, state, ["H-0001"], () => {})).toEqual(["H-0001"]);
      expect(shown).toEqual([]);
    }
  });
});

describe("NotifyReadout", () => {
  const render = (p: Permission) => renderToString(<NotifyReadout permission={p} onNotify={() => {}} />);

  it("offers a button until the browser has been asked", () => {
    expect(render("default")).toContain('<button type="button" class="btn blue-o notify">Notify me</button>');
  });

  it("shows a quiet state when granted and a note when denied, with no button", () => {
    expect(render("granted")).toContain(">On</span>");
    expect(render("granted")).not.toContain("<button");
    expect(render("denied")).toContain("Blocked in the browser's site settings");
    expect(render("denied")).not.toContain("<button");
  });

  it("renders nothing without the API", () => {
    expect(render("unsupported")).toBe("");
  });
});

describe("HailCard", () => {
  it("has a stable id, takes focus and is labelled by its meta line and question", () => {
    const html = renderToString(<HailCard hail={hail("H-0001")} crew={crew("sorell")} order={order("ORD-0001")} csrf="c" />);
    expect(html).toContain('id="hail-H-0001"');
    expect(html).toContain('tabindex="-1"');
    expect(html).toContain('aria-labelledby="hail-H-0001-meta hail-H-0001-q"');
    expect(html).toContain('id="hail-H-0001-meta"');
    expect(html).toContain('id="hail-H-0001-q"');
  });
});

describe("focusHail", () => {
  it("focuses the card, marked as notified until it loses focus", () => {
    const calls: string[] = [];
    const classes = new Set<string>();
    let blur = () => {};
    const el = {
      classList: { add: (c: string) => classes.add(c), remove: (c: string) => classes.delete(c) },
      addEventListener: (_: string, f: () => void) => (blur = f),
      scrollIntoView: () => calls.push("scroll"),
      focus: () => calls.push(`focus ${[...classes].join(" ")}`),
    };
    const win = {
      focus: () => calls.push("window"),
      document: { getElementById: (id: string) => (id === "hail-H-0001" ? el : null) },
    } as unknown as Window;
    focusHail("H-0001", win);
    expect(calls).toEqual(["window", "scroll", `focus ${notifiedClass}`]);
    blur();
    expect(classes.size).toBe(0);
    focusHail("H-0404", win); // a card that's gone
    expect(calls.at(-1)).toBe("window");
  });
});

describe("watchPermission", () => {
  function fakePerms(state: PermissionState) {
    const listeners = new Set<() => void>();
    const status = {
      state,
      addEventListener: (_: string, f: () => void) => listeners.add(f),
      removeEventListener: (_: string, f: () => void) => listeners.delete(f),
    };
    const perms = { query: async () => status } as unknown as PermissionsLike;
    const set = (s: PermissionState) => {
      status.state = s;
      for (const f of listeners) f();
    };
    return { perms, set, listeners };
  }
  const tick = () => new Promise((r) => setTimeout(r, 0));

  it("reports changes, prompt as default, until stopped", async () => {
    const { perms, set, listeners } = fakePerms("prompt");
    const seen: Permission[] = [];
    const stop = watchPermission(perms, (p) => seen.push(p));
    await tick();
    expect(seen).toEqual([]);
    set("granted");
    set("denied");
    set("prompt");
    expect(seen).toEqual(["granted", "denied", "default"]);
    stop();
    expect(listeners.size).toBe(0);
  });

  it("is a no-op without the API or when the query fails", async () => {
    const seen: Permission[] = [];
    watchPermission(undefined, (p) => seen.push(p))();
    const rejects = { query: () => Promise.reject(new TypeError("bad name")) } as unknown as PermissionsLike;
    watchPermission(rejects, (p) => seen.push(p));
    const throws = {
      query: () => {
        throw new TypeError("bad name");
      },
    } as unknown as PermissionsLike;
    watchPermission(throws, (p) => seen.push(p));
    await tick();
    expect(seen).toEqual([]);
  });

  it("doesn't listen when stopped before the query answers", async () => {
    const { perms, listeners } = fakePerms("granted");
    watchPermission(perms, () => {})();
    await tick();
    expect(listeners.size).toBe(0);
  });
});
