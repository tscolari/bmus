// The session exchange (D9, #71): `bmus console` opens the page with the
// per-start token in the URL fragment, `#t=<token>`. The page removes the
// fragment from the URL and history, posts the token once to
// POST /api/session, and gets back an HttpOnly session cookie and the CSRF
// token mutations send in X-BMUS-CSRF. A page loaded without the fragment
// (a reload) asks GET /api/session for its cookie's CSRF token (#73).

export interface Session {
  readonly csrf: string;
}

/** Returns the token in a `#t=<token>` fragment, or undefined. */
export function tokenFromHash(hash: string): string | undefined {
  const token = new URLSearchParams(hash.replace(/^#/, "")).get("t");
  return token || undefined;
}

/** The parts of window startSession uses, for tests to fake. */
export interface SessionEnv {
  readonly location: Pick<Location, "hash" | "pathname" | "search">;
  readonly history: Pick<History, "replaceState">;
  readonly fetch: typeof fetch;
}

/**
 * Exchanges the fragment's token for a session. The fragment is removed
 * before the request, so the token never stays in the address bar or
 * history, even if the request fails. Without a token it resumes the
 * session the cookie names, if any. Returns undefined when there is
 * neither a token nor a valid session.
 */
export async function startSession(env: SessionEnv): Promise<Session | undefined> {
  const token = tokenFromHash(env.location.hash);
  if (token === undefined) return resumeSession(env);
  env.history.replaceState(null, "", env.location.pathname + env.location.search);
  const resp = await env.fetch("/api/session", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ token }),
    credentials: "same-origin",
  });
  return sessionFrom(resp);
}

/**
 * Reads the CSRF token of the session the cookie names, with
 * GET /api/session. Returns undefined on a 401: no cookie, or one from
 * before the bridge restarted.
 */
async function resumeSession(env: SessionEnv): Promise<Session | undefined> {
  const resp = await env.fetch("/api/session", { method: "GET", credentials: "same-origin" });
  if (resp.status === 401) return undefined;
  return sessionFrom(resp);
}

/** Reads a /api/session response's CSRF token, or throws. */
async function sessionFrom(resp: Response): Promise<Session> {
  const body: unknown = await resp.json().catch(() => undefined);
  if (!resp.ok) throw new Error(`session: ${resp.status} ${errorMessage(body)}`);
  const csrf = (body as { csrf?: unknown } | undefined)?.csrf;
  if (typeof csrf !== "string" || csrf === "") throw new Error("session: response has no csrf token");
  return { csrf };
}

/** Returns the message of the console's {"error": {...}} body, if any. */
function errorMessage(body: unknown): string {
  const message = (body as { error?: { message?: unknown } } | undefined)?.error?.message;
  return typeof message === "string" ? message : "";
}
