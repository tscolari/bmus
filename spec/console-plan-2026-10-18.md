# Console integration plan

This plan builds on D9 in `initial-plan-2026-10-16.md` and the Phase 2 deliverable "the console wired to live data with SSE and notifications". It doesn't restate them. The design is the BMUS Bridge canvas (bridge console and ship's manifest), with the earlier prototype kept in `spec/ui/prototype-2026-10-16.html`. The console is Phase 2 work, tracked under the Phase 2 epic. The Phase 1 groundwork it needs is in `phase-1-plan-amendment-2026-10-18.md`.

## Shape

The console is one more client of the daemon, next to the CLI. It never touches the store. Every read and every action goes through the same `Daemon` methods the RPC server calls, so the CLI and the console can't disagree about what an action does or which checks it passes.

```
browser --HTTP/SSE--> internal/console --> Daemon methods <-- internal/bridge/rpc.go <-- CLI
                            ^
                            +-- change feed (captain's log appends, in-process)
```

`bmus bridge run` starts the console server alongside the socket server, with the same lifecycle (started after the tours re-attach, stopped first on shutdown). `--console 127.0.0.1:4096` sets the address. `--console off` disables it. A bind address that isn't a loopback address is refused at startup.

## Live data: the captain's log is the change feed

The console needs to know when anything changes. The captain's log already has a monotonic id. Phase 1 groundwork makes every state change write its log entry in the same transaction as the change, so one ordered stream covers crew, orders, hails and beams.

- `internal/store` gains a post-commit hook: after a transaction that appended log entries commits, the store hands the new entries to a `Feed`. The daemon is the store's only writer, so an in-process feed is complete. No polling.
- The `Feed` fans entries out to subscribers through bounded channels. A subscriber that falls behind is dropped, and its client reconnects and catches up from the log.
- SSE events carry the log id as their `id`. On reconnect the browser sends `Last-Event-ID` and the server replays `log WHERE id > ?` before going live. A gap wider than 10,000 entries sends a fresh `snapshot` instead.

Event types:

- `snapshot`: the whole console state (crew, open and away orders, open hails, status, the last 200 log entries) and the log id it reflects. Sent first on every connection.
- `log`: one captain's log entry.
- `crew`, `order`, `hail`: the current view of the entity a log entry names (by its `crew_id`, `order_id`, or a hail id in its payload). The server reads the view after the commit and sends it right behind the `log` event.
- `status`: condition and counters, sent when they change.

Views are the existing `CrewView`, `OrderView` and hail view types from `rpc.go`, moved to a shared `internal/bridge/views.go`. The console and the CLI's `--json` output then share one JSON shape, and the golden tests cover both.

I prefer pushing full entity views over pushing diffs. They are a few hundred bytes each, they make the client a plain cache keyed by id, and a lost event heals at the next one.

## HTTP API

Reads:

- `GET /api/state`: the `snapshot` body, for a page load without SSE.
- `GET /api/events`: the SSE stream.
- `GET /api/log?since=<id>&limit=<n>`: older log pages for scrolling back.

Actions, each a `POST` with a JSON body, each calling the `Daemon` method the matching RPC calls:

| Console control | Endpoint | Daemon call | Available |
|---|---|---|---|
| Issue order, Engage | `POST /api/orders` | `CreateOrder` | Phase 1 |
| Hail answer buttons | `POST /api/hails/{id}/answer` | `AnswerHail` | Phase 1 |
| Add crew (not in the mock; settings drawer) | `POST /api/crew` | `AddCrew` | Phase 1 |
| Belay order | `POST /api/orders/{id}/belay` | new | Phase 2 |
| Beam up (recall) | `POST /api/orders/{id}/recall` | new | Phase 2 |
| Withdraw, Route | `POST /api/orders/{id}/withdraw`, `/route` | new | Phase 2 |
| Re-energize beam | `POST /api/crew/{id}/reenergize` | new | Phase 2 |
| Standing orders switch | `PUT /api/standing-orders` | new | Phase 2 |

Errors reuse the RPC codes (`bad_request`, `not_found`, `unavailable`, `exists`, `hail_closed`) as `{"error": {"code", "message"}}` with matching HTTP statuses. A control whose backend isn't there yet is rendered disabled with a tooltip naming the missing capability, never hidden. The console can then ship before every Phase 2 backend item lands.

## Security

D9 sets the rules. This is how they land.

- **Token.** The daemon makes a random 256-bit token at start and writes it to `<home>/console.token`, mode 0600. A restart invalidates old sessions.
- **Getting in.** `bmus console` reads the token, prints `http://127.0.0.1:4096/#t=<token>` and opens it unless `--no-open`. The token rides in the URL fragment, which never reaches server logs or `Referer`. The page posts it once to `POST /api/session` and gets an `HttpOnly`, `SameSite=Strict`, path `/` cookie, then removes the fragment with `history.replaceState`. `bmus console` exits 3 when the bridge isn't running, like every other command.
- **Host and Origin.** Every request must have `Host` equal to the bound `127.0.0.1:<port>` or `localhost:<port>` (DNS rebinding). Every `POST`, `PUT` and `DELETE` must have an `Origin` equal to one of those. A missing `Origin` on a mutation is refused.
- **CSRF.** `POST /api/session` also returns a CSRF token. Mutations must send it in `X-BMUS-CSRF`. The cookie alone never authorizes a change.
- **Headers.** A strict `Content-Security-Policy` (`default-src 'self'`, no inline script, `connect-src 'self'`), `X-Frame-Options: DENY`, `Referrer-Policy: no-referrer`, `Cache-Control: no-store` on `/api`.
- **Untrusted text.** Hail questions, progress notes and report summaries come from agents that may have read hostile content. The UI renders them as text, never as HTML or markdown with links. Phase 3's "raw question next to Command's summary" builds on that.

The Phase 4 security review covers this surface. I'd still have a reviewer focused only on these checks when the server first lands, not wait until Phase 4.

## Frontend

The canvas is a design, not code. `web/` rebuilds it as an app.

- **Stack.** TypeScript and Preact, bundled by esbuild, the same toolchain as combadge. Preact keeps the bundle small, and the console's state is one cache fed by SSE, so a store library isn't needed. Fonts (Archivo Narrow, Space Mono, Syncopate) are vendored as woff2. No CDN, which the CSP forbids anyway.
- **Embedding.** `web/` builds into `internal/console/assets/`, embedded with `go:embed`. I think the built assets should be committed. `go install` then gives a working console without Node, and CI fails when `npm run build` changes the committed output. A missing build serves a page that says to run it, not a 404.
- **State.** One store keyed by entity id, fed by `snapshot` and the entity events. Components read from it. Actions post, then wait for the event to update the view. No optimistic updates: the log stays the single source of truth.
- **Derived values.** Condition comes from the server's `status` (see the Phase 1 amendment), never from the client. Stardate is client-side decoration from the clock.
- **Phone width.** The mock is desktop first. Below 900px panels stack in this order: hails, away missions, crew, orders, log.

## Notifications

D9 says hails raise OS notifications. The console uses the browser's Notification API: the page asks for permission once, from a button, and shows a notification for each new hail with the crew member and the question. The browser hands it to the OS. Notifications need the console tab open. Hails that arrive with no tab open stay in the log and in `bmus hails list`, and the "subspace hails" idea on the drawing board covers the rest later. A bridge-side `notify-send`/`osascript` notifier isn't worth its platform code yet.

## Testing

- **Go.** Handler tests for each auth rule (missing token, wrong `Host`, wrong `Origin`, missing CSRF, each with the exact status). Feed tests: one log entry per committed state change, replay from `Last-Event-ID`, a slow subscriber dropped, a gap that forces a snapshot.
- **Frontend.** vitest for the store reducer and the view helpers.
- **End to end.** Playwright with the preinstalled Chromium against a holodeck bridge: issue an order, watch it go away, answer its hail, see it done, with no reload. This is the "console matches the mock's behaviour" exit check in executable form, and it runs in CI.

## Work items (Phase 2, sub-issues of #55)

In order. Each line names what it waits on.

1. #71 Console server skeleton: listener, flags, lifecycle, token, session, Host/Origin/CSRF checks, headers, `bmus console`. Waits on nothing.
2. #72 Feed and SSE: store post-commit hook, `Feed`, `/api/events` with replay and snapshot, `/api/state`, `/api/log`. Waits on #69 and #71.
3. #73 Shared views and action endpoints for what exists: orders, hail answers, crew. Waits on 1.
4. #74 `web/` scaffold: esbuild, Preact, fonts, embedding, CI freshness check. Waits on nothing.
5. #75 Panels backed by Phase 1 data: header and condition, crew manifest, composer, hails, away missions, mission record, captain's log, transporter pads. Waits on #70, #72, #73 and #74.
6. #76 Notifications. Waits on 5.
7. #77 Playwright end-to-end test in CI. Waits on 5.
8. #78 Phase 2 controls (belay, recall, withdraw and route, re-energize, standing orders, open orders pool): each enabled as its backend issue lands. Waits on those issues.
