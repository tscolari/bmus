import { describe, expect, it } from "vitest";
import { startSession, tokenFromHash, type SessionEnv } from "../src/session.js";

interface Call {
  readonly url: string;
  readonly init: RequestInit | undefined;
}

function fakeEnv(hash: string, status: number, body: unknown) {
  const calls: Call[] = [];
  const replaced: string[] = [];
  const env: SessionEnv = {
    location: { hash, pathname: "/", search: "?x=1" },
    history: { replaceState: (_data, _unused, url) => void replaced.push(String(url)) },
    fetch: async (url, init) => {
      calls.push({ url: String(url), init });
      return new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
    },
  };
  return { env, calls, replaced };
}

describe("tokenFromHash", () => {
  it("reads t from the fragment", () => {
    expect(tokenFromHash("#t=abc")).toBe("abc");
    expect(tokenFromHash("")).toBeUndefined();
    expect(tokenFromHash("#t=")).toBeUndefined();
    expect(tokenFromHash("#other=1")).toBeUndefined();
  });
});

describe("startSession", () => {
  it("posts the token once, removes the fragment and returns the CSRF token", async () => {
    const { env, calls, replaced } = fakeEnv("#t=tok", 200, { csrf: "c5rf" });
    await expect(startSession(env)).resolves.toEqual({ csrf: "c5rf" });
    expect(replaced).toEqual(["/?x=1"]);
    expect(calls).toHaveLength(1);
    expect(calls[0]?.url).toBe("/api/session");
    expect(calls[0]?.init?.method).toBe("POST");
    expect(new Headers(calls[0]?.init?.headers).get("Content-Type")).toBe("application/json");
    expect(calls[0]?.init?.credentials).toBe("same-origin");
    expect(JSON.parse(String(calls[0]?.init?.body))).toEqual({ token: "tok" });
  });

  it("resumes the cookie's session without a token", async () => {
    const { env, calls, replaced } = fakeEnv("", 200, { csrf: "c5rf" });
    await expect(startSession(env)).resolves.toEqual({ csrf: "c5rf" });
    expect(replaced).toHaveLength(0);
    expect(calls).toHaveLength(1);
    expect(calls[0]?.url).toBe("/api/session");
    expect(calls[0]?.init?.method).toBe("GET");
    expect(calls[0]?.init?.body).toBeUndefined();
    expect(calls[0]?.init?.credentials).toBe("same-origin");
  });

  it("returns undefined without a token or a valid session", async () => {
    const { env, calls, replaced } = fakeEnv("", 401, { error: { code: "unauthorized", message: "missing or invalid session" } });
    await expect(startSession(env)).resolves.toBeUndefined();
    expect(calls).toHaveLength(1);
    expect(replaced).toHaveLength(0);
  });

  it("rejects a resume that fails otherwise, or has no csrf token", async () => {
    const { env } = fakeEnv("", 403, { error: { code: "forbidden", message: "missing or invalid Origin" } });
    await expect(startSession(env)).rejects.toThrow("session: 403 missing or invalid Origin");
    const { env: empty } = fakeEnv("", 200, {});
    await expect(startSession(empty)).rejects.toThrow("session: response has no csrf token");
  });

  it("rejects a 200 without a csrf token", async () => {
    for (const body of [{}, { csrf: "" }]) {
      const { env } = fakeEnv("#t=tok", 200, body);
      await expect(startSession(env)).rejects.toThrow("session: response has no csrf token");
    }
  });

  it("removes the fragment even when the exchange fails", async () => {
    const { env, replaced } = fakeEnv("#t=bad", 401, { error: { code: "unauthorized", message: "invalid token" } });
    await expect(startSession(env)).rejects.toThrow("session: 401 invalid token");
    expect(replaced).toEqual(["/?x=1"]);
  });
});
