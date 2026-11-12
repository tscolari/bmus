import { useEffect, useReducer, useRef, useState } from "preact/hooks";
import { browserFeedEnv, connectFeed, type Link } from "./feed.js";
import {
  AwayPanel,
  Composer,
  CountermandPanel,
  CrewPanel,
  focusComposer,
  HailsPanel,
  Header,
  LogPanel,
  OpenOrdersPanel,
  RecordPanel,
  Stats,
  TransporterPanel,
} from "./panels.js";
import { announce, browserNotifyEnv, focusHail, type Permission } from "./notify.js";
import { startSession } from "./session.js";
import { emptyState, reduce } from "./store.js";

type SessionState =
  | { readonly kind: "starting" }
  | { readonly kind: "ready"; readonly csrf: string }
  | { readonly kind: "no-session" }
  | { readonly kind: "failed"; readonly message: string };

/** How often the stardate clock ticks. */
const clockTick = 15_000;

/** The console's root: the session exchange, then the bridge console. */
export function App() {
  const [session, setSession] = useState<SessionState>({ kind: "starting" });
  useEffect(() => {
    startSession(window).then(
      (s) => setSession(s ? { kind: "ready", csrf: s.csrf } : { kind: "no-session" }),
      (err: unknown) => setSession({ kind: "failed", message: err instanceof Error ? err.message : String(err) }),
    );
  }, []);
  if (session.kind === "ready") return <Console csrf={session.csrf} />;
  return (
    <main class="console">
      <h1 class="gate-title">BEAM ME UP, SCOTTY</h1>
      <p class="mono gate">{statusText(session)}</p>
    </main>
  );
}

function statusText(s: Exclude<SessionState, { kind: "ready" }>): string {
  switch (s.kind) {
    case "starting":
      return "Starting session...";
    case "no-session":
      return "No session. Open the console with `bmus console`.";
    case "failed":
      return `Could not start a session: ${s.message}. Run \`bmus console\` again.`;
  }
}

/**
 * The bridge console. Columns as in the design (crew; orders and
 * missions; transporter room and log); below 900px app.css stacks the
 * panels in the plan's order: hails, away missions, crew, orders, log.
 */
function Console(props: { csrf: string }) {
  const [state, dispatch] = useReducer(reduce, emptyState);
  const [link, setLink] = useState<Link>("connecting");
  const [target, setTarget] = useState("pool");
  const [now, setNow] = useState(() => new Date());
  useEffect(() => connectFeed(browserFeedEnv, dispatch, setLink), []);
  useEffect(() => {
    const t = window.setInterval(() => setNow(new Date()), clockTick);
    return () => window.clearInterval(t);
  }, []);
  const [permission, setPermission] = useState<Permission>(() => browserNotifyEnv.permission());
  useEffect(() => browserNotifyEnv.watch(setPermission), []);
  const asking = useRef(false);
  const askPermission = () => {
    if (asking.current) return;
    asking.current = true;
    browserNotifyEnv
      .request()
      .then(setPermission, () => setPermission(browserNotifyEnv.permission()))
      .finally(() => (asking.current = false));
  };
  // Fresh hails: notify (when granted), then drop them from the queue.
  useEffect(() => {
    if (state.fresh.length === 0) return;
    dispatch({ type: "announced", ids: announce(browserNotifyEnv, state, state.fresh, focusHail) });
  }, [state.fresh]);
  const direct = (crewId: string) => {
    setTarget(crewId);
    focusComposer();
  };
  return (
    <div class="console">
      <div class="frame">
        <Header state={state} link={link} now={now} permission={permission} onNotify={askPermission} />
        {link === "lost" && (
          <p class="error banner" role="alert">
            The session ended (the bridge restarted?). Open the console again with `bmus console`.
          </p>
        )}
        <div class="stripe" aria-hidden="true" />
        <Stats state={state} />
        <main class="board">
          <CrewPanel state={state} csrf={props.csrf} target={target} onDirect={direct} />
          <div class="col col-mid">
            <Composer state={state} csrf={props.csrf} target={target} onTarget={setTarget} />
            <HailsPanel state={state} csrf={props.csrf} />
            <CountermandPanel state={state} csrf={props.csrf} now={now} />
            <OpenOrdersPanel state={state} csrf={props.csrf} />
            <AwayPanel state={state} csrf={props.csrf} />
            <RecordPanel state={state} csrf={props.csrf} />
          </div>
          <div class="col col-side">
            <TransporterPanel state={state} csrf={props.csrf} />
            <LogPanel state={state} dispatch={dispatch} />
          </div>
        </main>
      </div>
    </div>
  );
}
