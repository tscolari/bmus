// The console's panels, after the BMUS Bridge design's "Bridge console"
// artboard. Every value comes from the store; actions post and the views
// update from the feed, never optimistically. Text from agents (hail questions and context,
// progress notes, summaries) is rendered as plain text children only:
// never as HTML or markdown, and never linkified.

import type { ComponentChildren } from "preact";
import { useEffect, useRef, useState } from "preact/hooks";
import {
  answerHail,
  belayOrder,
  countermandHail,
  createOrder,
  logBefore,
  pushOrder,
  recallOrder,
  reenergizeCrew,
  routeOrder,
  setStandingOrders,
  withdrawOrder,
  ApiError,
  type BridgeStatus,
  type CampaignFinding,
  type CrewView,
  type HailView,
  type OrderView,
  type PushedRepo,
} from "./api.js";
import type { Link } from "./feed.js";
import { hailCardId, type Permission } from "./notify.js";
import type { Action, State } from "./store.js";
import {
  awayOrders,
  belayPrompt,
  bridgeHailOrder,
  campaignHailMeta,
  campaignOrderText,
  canPush,
  choiceDecision,
  commitsText,
  findingPlace,
  isCampaignHail,
  landContext,
  optionIndex,
  dropPrompt,
  shortCommit,
  stalledContext,
  triageFindings,
  canTake,
  clockHM,
  countermandableHails,
  decidedByLabel,
  conditionInfo,
  reportPath,
  forcePushPrompt,
  githubCompareURL,
  contextText,
  crewOptionLabel,
  crewStatusInfo,
  currentOrder,
  divisionInfo,
  composerRepo,
  categoryCampaignTriage,
  hailKind,
  issuesText,
  issueURL,
  knownDivisions,
  logTag,
  logTime,
  manifest,
  missionRecord,
  openHails,
  openOrders,
  openOrderState,
  orderBranch,
  orderProgress,
  orderUsage,
  padInfo,
  priorities,
  priorityInfo,
  progressPct,
  pushedTo,
  pushPrompt,
  pushRejectedText,
  pushRemotes,
  queuedFor,
  discardAllPrompt,
  restoreKeys,
  reviewAllDecision,
  reviewContext,
  reviewNumbers,
  reviewDecision,
  reviewSummary,
  triageRationale,
  routeArgs,
  routeValue,
  shortName,
  standingOrdersInfo,
  stardate,
  type PushRemote,
  type Tone,
  type UsageField,
} from "./view.js";

/** The page of older entries "Load older entries" asks for. */
export const olderPage = 200;

/** The id of the composer's order field, which "Direct order" focuses. */
const orderFieldId = "order-text";

function errorText(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}

/** The sections' heading: a coloured tab and the title. */
function Heading(props: { id: string; tone: Tone; blink?: boolean; focusable?: boolean; onBlur?: () => void; children: ComponentChildren }) {
  return (
    <h2 id={props.id} class="sect" {...(props.focusable && { tabIndex: -1, onBlur: props.onBlur })}>
      <span class={`tab bg-${props.tone}${props.blink ? " blink" : ""}`} aria-hidden="true" />
      {props.children}
    </h2>
  );
}

function Light(props: { tone: Tone; blink: boolean }) {
  return <span class={`light bg-${props.tone}${props.blink ? " blink" : ""}`} aria-hidden="true" />;
}

function Bar(props: { pct: number | undefined; tone: Tone; label: string }) {
  const pct = props.pct ?? 0;
  return (
    <div class="bar" role="progressbar" aria-label={props.label} aria-valuemin={0} aria-valuemax={100} aria-valuenow={props.pct}>
      <div class={`fill bg-${props.tone}`} style={{ width: `${pct}%` }} />
    </div>
  );
}

function pctText(pct: number | undefined): string {
  return pct === undefined ? "--" : `${Math.round(pct)}%`;
}

type ConfirmPhase = "idle" | "confirm" | "pending" | "sent";

/**
 * An action that can't be undone, behind a confirm step (#78): the button
 * asks, then the prompt with a confirm and a cancel button replaces it,
 * the confirm button focused. Cancel or Escape puts the first button back,
 * focused. A refusal shows inline and puts it back too. Once sent, the
 * button stays disabled and focus moves to the "Sent" note, so a keyboard
 * user keeps their place: the feed updates the view.
 */
export function ConfirmAction(props: {
  idBase: string;
  label: string;
  ariaLabel: string;
  prompt: string;
  confirmLabel: string;
  run: () => Promise<unknown>;
  /** Not askable yet (e.g. a field it needs is empty), or, in the confirm step, not confirmable now. */
  disabled?: boolean;
  /** The first button's class; "btn ghost" by default. */
  askClass?: string;
}) {
  const [phase, setPhase] = useState<ConfirmPhase>("idle");
  const [error, setError] = useState("");
  const askRef = useRef<HTMLButtonElement>(null);
  const confirmRef = useRef<HTMLButtonElement>(null);
  const sentRef = useRef<HTMLParagraphElement>(null);
  const refocus = useRef(false);
  useEffect(() => {
    if (phase === "confirm") confirmRef.current?.focus();
    if (phase === "sent") sentRef.current?.focus();
    if (phase === "idle" && refocus.current) {
      refocus.current = false;
      askRef.current?.focus();
    }
  }, [phase]);
  const back = () => {
    refocus.current = true;
    setPhase("idle");
  };
  const go = async () => {
    setPhase("pending");
    setError("");
    try {
      await props.run();
      setPhase("sent");
    } catch (err) {
      setError(errorText(err));
      back();
    }
  };
  const promptId = `${props.idBase}-q`;
  return (
    <>
      {phase === "idle" || phase === "sent" ? (
        <button
          ref={askRef}
          type="button"
          class={props.askClass ?? "btn ghost"}
          aria-label={props.ariaLabel}
          disabled={phase === "sent" || props.disabled === true}
          onClick={() => {
            setError("");
            setPhase("confirm");
          }}
        >
          {props.label}
        </button>
      ) : (
        <span
          class="confirm"
          role="group"
          aria-labelledby={promptId}
          onKeyDown={(e) => {
            if (e.key === "Escape" && phase === "confirm") {
              e.preventDefault();
              back();
            }
          }}
        >
          <span id={promptId} class="confirm-q">
            {props.prompt}
          </span>
          <button ref={confirmRef} type="button" class="btn red" disabled={phase === "pending" || props.disabled === true} onClick={() => void go()}>
            {phase === "pending" ? "Sending" : props.confirmLabel}
          </button>
          <button type="button" class="btn ghost" disabled={phase === "pending"} onClick={back}>
            Cancel
          </button>
        </span>
      )}
      {phase === "sent" && (
        <p ref={sentRef} class="notice act-msg" role="status" tabIndex={-1}>
          Sent. Waiting for the bridge.
        </p>
      )}
      {error && (
        <p class="error act-msg" role="alert">
          {error}
        </p>
      )}
    </>
  );
}

/* Header ---------------------------------------------------------------- */

const linkLabels: Record<Link, { label: string; tone: Tone }> = {
  connecting: { label: "Connecting", tone: "grey" },
  live: { label: "Live", tone: "green" },
  reconnecting: { label: "Reconnecting", tone: "gold" },
  lost: { label: "Session lost", tone: "red" },
};

export function Header(props: { state: State; link: Link; now: Date; permission: Permission; onNotify: () => void }) {
  const cond = conditionInfo(props.state.status?.condition);
  const reasons = props.state.status?.reasons ?? [];
  const link = linkLabels[props.link];
  return (
    <header class="top">
      <div class="brand">
        <svg width="58" height="58" viewBox="0 0 58 58" aria-hidden="true">
          <circle cx="29" cy="29" r="26" fill="none" stroke="#E8B23A" stroke-width="3" />
          <path d="M17 30 L29 18 L41 30" fill="none" stroke="#E8B23A" stroke-width="4" stroke-linecap="square" />
          <path d="M17 41 L29 29 L41 41" fill="none" stroke="#E8B23A" stroke-width="4" stroke-linecap="square" />
        </svg>
        <div>
          <div class="mono kicker">U.S.S. BEAMS · NCC-4096 · BRIDGE</div>
          <h1>BEAM ME UP, SCOTTY</h1>
        </div>
      </div>
      <div class="readouts">
        <div class="readout" role="status" aria-live="polite">
          <span class="readout-lbl">LINK</span>
          <span class={`mono readout-val tone-${link.tone}`}>
            <Light tone={link.tone} blink={props.link === "reconnecting"} /> {link.label}
          </span>
        </div>
        <NotifyReadout permission={props.permission} onNotify={props.onNotify} />
        <div class="readout">
          <span class="readout-lbl">STARDATE</span>
          <span class="mono readout-val">
            {stardate(props.now)} · {clockHM(props.now)}
          </span>
        </div>
        <div class={`condition bg-${cond.tone}`} role="status" aria-live="polite" title={reasons.join("; ") || undefined}>
          <span class="condition-lbl">CONDITION</span>
          <span class="condition-val">{cond.label}</span>
          {reasons.length > 0 && <span class="condition-why">{reasons.join(", ")}</span>}
        </div>
      </div>
    </header>
  );
}

/**
 * Hail notifications: a button while the browser hasn't been asked, a
 * quiet state once granted, a note when denied. Nothing without the API.
 */
export function NotifyReadout(props: { permission: Permission; onNotify: () => void }) {
  if (props.permission === "unsupported") return null;
  return (
    <div class="readout">
      <span class="readout-lbl">HAIL ALERTS</span>
      {props.permission === "default" && (
        <button type="button" class="btn blue-o notify" onClick={props.onNotify}>
          Notify me
        </button>
      )}
      {props.permission === "granted" && <span class="mono readout-val tone-blue">On</span>}
      {props.permission === "denied" && <span class="readout-note">Blocked in the browser's site settings</span>}
    </div>
  );
}

const lampCount = 28;

export function Stats(props: { state: State }) {
  const s = props.state.status;
  const stats: { label: string; value: number | undefined; tone: Tone }[] = [
    {
      label: "Crew aboard",
      value: s && s.crew.beaming_in + s.crew.on_deck + s.crew.away + s.crew.hailing + s.crew.sickbay,
      tone: "cream",
    },
    { label: "Beaming in", value: s?.crew.beaming_in, tone: "cream" },
    { label: "On deck", value: s?.crew.on_deck, tone: "green" },
    { label: "Away", value: s?.crew.away, tone: "gold" },
    { label: "Hailing", value: s?.crew.hailing, tone: "blue" },
    { label: "Sickbay", value: s?.crew.sickbay, tone: "red" },
    { label: "Open orders", value: s?.orders.open, tone: "grey" },
  ];
  return (
    <div class="stats">
      {stats.map((st) => (
        <div class="stat" key={st.label}>
          <div class="stat-head">
            <Light tone={st.tone} blink={false} />
            <span class="stat-lbl">{st.label}</span>
          </div>
          <span class="mono stat-val">{st.value ?? "-"}</span>
        </div>
      ))}
      <div class="lamps" aria-hidden="true">
        {Array.from({ length: lampCount }, (_, i) => (
          <span class="lamp" key={i} />
        ))}
      </div>
    </div>
  );
}

/* Crew manifest --------------------------------------------------------- */

export function CrewPanel(props: { state: State; csrf: string; target: string; onDirect: (crewId: string) => void }) {
  const crew = manifest(props.state);
  return (
    <section class="area-crew crew-col" aria-labelledby="crew-h">
      <Heading id="crew-h" tone="gold">
        Crew manifest
      </Heading>
      {crew.length === 0 && <p class="empty">No crew aboard. Add one with bmus crew add.</p>}
      {crew.map((c) => (
        <CrewCard key={c.id} state={props.state} csrf={props.csrf} crew={c} selected={props.target === c.id} onDirect={props.onDirect} />
      ))}
    </section>
  );
}

function CrewCard(props: { state: State; csrf: string; crew: CrewView; selected: boolean; onDirect: (crewId: string) => void }) {
  const { state, crew: c } = props;
  const div = divisionInfo(c.division);
  const st = crewStatusInfo(c.status);
  const order = currentOrder(state, c.id);
  const queued = queuedFor(state, c.id);
  const onMission = c.status === "away" || c.status === "hailing";
  const pct = order ? orderProgress(order)?.pct : undefined;
  let task = "Standing by for orders";
  if (c.status === "sickbay") task = "Lost contact with beam";
  else if (order) task = order.title;
  else if (c.status === "beaming_in") task = "Beaming in: no orders until its combadge says hello";
  return (
    <article class={props.selected ? "crew sel" : "crew"} aria-label={c.name || c.id}>
      <div class="crew-head">
        <div class={`avatar bg-${div.tone}`} aria-hidden="true">
          {shortName(c.id).charAt(0)}
        </div>
        <div class="crew-who">
          <div class="crew-name">{c.name || c.id}</div>
          <div class="crew-role">
            {c.rank ? `${c.rank} · ` : ""}
            {div.label}
          </div>
          {c.spec && <div class="crew-spec">{c.spec}</div>}
        </div>
        <div class={`crew-status tone-${st.tone}`}>
          <Light tone={st.tone} blink={st.blink} />
          {st.label}
        </div>
      </div>
      <div class="crew-task">
        {order && c.status !== "sickbay" && <span class="mono muted-id">{order.id}</span>} {task}
      </div>
      {onMission && order && <Bar pct={pct} tone={st.tone} label={`${order.id} progress`} />}
      <div class="crew-foot">
        <span class="mono muted-id">
          BEAM {c.current_beam || (c.status === "beaming_in" ? "provisioning" : "none")}
          {queued > 0 ? ` · ${queued} queued` : ""}
        </span>
        {c.status === "sickbay" ? (
          <ReenergizeButton crew={c} csrf={props.csrf} />
        ) : (
          <button type="button" class="btn ghost" aria-label={`Direct the next order to ${c.name || c.id}`} onClick={() => props.onDirect(c.id)}>
            Direct order
          </button>
        )}
      </div>
    </article>
  );
}

/**
 * Re-energize for a crew member in sickbay (#78): a new tour and beam,
 * which resume its parked order. Not destructive, so no confirm step.
 */
function ReenergizeButton(props: { crew: CrewView; csrf: string }) {
  const c = props.crew;
  const [pending, setPending] = useState(false);
  const [error, setError] = useState("");
  const [notice, setNotice] = useState("");
  const run = async () => {
    if (pending) return;
    setPending(true);
    setError("");
    setNotice("");
    try {
      const res = await reenergizeCrew(props.csrf, c.id);
      setNotice(res.outcome === "under_way" ? "Re-energize is already under way." : "Fresh beam requested.");
    } catch (err) {
      setError(errorText(err));
    } finally {
      setPending(false);
    }
  };
  return (
    <>
      <button
        type="button"
        class="btn red"
        aria-label={`Re-energize beam for ${c.name || c.id}`}
        aria-disabled={pending || undefined}
        aria-busy={pending || undefined}
        onClick={() => void run()}
      >
        {pending ? "Energizing" : "Re-energize beam"}
      </button>
      {notice && (
        <p class="notice act-msg" role="status">
          {notice}
        </p>
      )}
      {error && (
        <p class="error act-msg" role="alert">
          {error}
        </p>
      )}
    </>
  );
}

/* Composer -------------------------------------------------------------- */

export function Composer(props: { state: State; csrf: string; target: string; onTarget: (t: string) => void }) {
  const { state, target } = props;
  const crew = manifest(state);
  const divisions = knownDivisions(state);
  // The bridge's registered repos (#211): an order may name only these.
  const repos = state.repos;
  const [title, setTitle] = useState("");
  const [division, setDivision] = useState("");
  const [priority, setPriority] = useState(0);
  const [repo, setRepo] = useState("");
  const [baseRef, setBaseRef] = useState("main");
  const [pending, setPending] = useState(false);
  const [error, setError] = useState("");
  const [notice, setNotice] = useState("");

  const targetCrew = target === "pool" ? undefined : state.crew.get(target);
  const poolDivision = division || divisions[0] || "";
  const repoName = composerRepo(repos, repo);

  const issue = async () => {
    if (pending || repoName === "") return;
    setPending(true);
    setError("");
    setNotice("");
    try {
      const o = await createOrder(props.csrf, {
        title: title.trim(),
        assignee: targetCrew ? targetCrew.id : "",
        ...(targetCrew ? {} : { division: poolDivision }),
        priority,
        repos: [{ name: repoName, base_ref: baseRef.trim() }],
      });
      setTitle("");
      setNotice(`${o.id} issued.`);
      props.onTarget("pool");
    } catch (err) {
      setError(errorText(err));
    } finally {
      setPending(false);
    }
  };

  let hint: string;
  if (targetCrew) {
    hint = `Directed to ${targetCrew.name || targetCrew.id}. It waits in their queue until they are back on deck.`;
  } else {
    const div = poolDivision ? `the ${divisionInfo(poolDivision).label} division` : "a division";
    switch (standingOrdersInfo(state.status).on) {
      case true:
        hint = `Goes to the open pool for ${div}. The first free crew member there takes it, red alerts first.`;
        break;
      case false:
        hint = `Goes to the open pool for ${div}. Standing orders are off: it sits there until you route it.`;
        break;
      default:
        hint = `Goes to the open pool for ${div}. A free crew member there takes it when standing orders are on, red alerts first.`;
    }
  }

  return (
    <section class="panel area-orders" aria-labelledby="issue-h">
      <Heading id="issue-h" tone="gold">
        Issue order
      </Heading>
      <form
        onSubmit={(e) => {
          e.preventDefault();
          void issue();
        }}
      >
        <label for={orderFieldId} class="lbl">
          Order
        </label>
        <input
          id={orderFieldId}
          class="field"
          type="text"
          autocomplete="off"
          value={title}
          onInput={(e) => setTitle(e.currentTarget.value)}
          placeholder="Add retries to the notifications webhook sender"
        />
        <div class="composer-row">
          <div class="grow-220">
            <label for="order-target" class="lbl">
              Send to
            </label>
            <select id="order-target" class="field" value={target} onChange={(e) => props.onTarget(e.currentTarget.value)}>
              <option value="pool">Open pool (any free crew)</option>
              {crew.map((c) => (
                <option key={c.id} value={c.id} disabled={!canTake(c)}>
                  {crewOptionLabel(c)}
                </option>
              ))}
            </select>
          </div>
          <div class="grow-160">
            <label for="order-div" class="lbl">
              Division
            </label>
            {targetCrew || divisions.length > 0 ? (
              <select
                id="order-div"
                class="field"
                value={targetCrew ? targetCrew.division : poolDivision}
                disabled={!!targetCrew}
                onChange={(e) => setDivision(e.currentTarget.value)}
              >
                {(targetCrew ? [targetCrew.division] : divisions).map((d) => (
                  <option key={d} value={d}>
                    {divisionInfo(d).label}
                  </option>
                ))}
              </select>
            ) : (
              <input id="order-div" class="field" type="text" value={division} onInput={(e) => setDivision(e.currentTarget.value)} />
            )}
          </div>
          <div>
            <span id="prio-lbl" class="lbl">
              Alert
            </span>
            <div role="group" aria-labelledby="prio-lbl" class="segs">
              {priorities.map((p) => (
                <button
                  type="button"
                  key={p.key}
                  class={priority === p.value ? `seg on-${p.key}` : "seg"}
                  aria-pressed={priority === p.value}
                  onClick={() => setPriority(p.value)}
                >
                  {p.label}
                </button>
              ))}
            </div>
          </div>
        </div>
        <div class="composer-row">
          {repos.length > 0 ? (
            <>
              <div class="grow-220">
                <label for="order-repo" class="lbl">
                  Repo
                </label>
                <select id="order-repo" class="field" value={repoName} onChange={(e) => setRepo(e.currentTarget.value)}>
                  {repos.map((r) => (
                    <option key={r} value={r}>
                      {r}
                    </option>
                  ))}
                </select>
              </div>
              <div class="grow-160">
                <label for="order-ref" class="lbl">
                  Base ref
                </label>
                <input id="order-ref" class="field" type="text" autocomplete="off" value={baseRef} onInput={(e) => setBaseRef(e.currentTarget.value)} />
              </div>
            </>
          ) : (
            // An order needs a registered repo: with none, every Engage would be refused.
            <p id="order-no-repos" class="hint grow-220">
              This bridge has no repos, so it can't take orders. Restart it with <code>--repo &lt;name&gt;=&lt;url&gt;</code>.
            </p>
          )}
          <button
            type="submit"
            class="btn gold engage"
            disabled={pending || repos.length === 0}
            aria-describedby={repos.length === 0 ? "order-no-repos" : undefined}
          >
            {pending ? "Engaging" : "Engage"}
          </button>
        </div>
      </form>
      <p class="hint">{hint}</p>
      {error && (
        <p class="error" role="alert">
          {error}
        </p>
      )}
      {notice && !error && (
        <p class="notice" role="status">
          {notice}
        </p>
      )}
    </section>
  );
}

/** Focuses the composer's order field (after "Direct order"). */
export function focusComposer(doc: Document = document) {
  const el = doc.getElementById(orderFieldId);
  el?.focus();
}

/* Hails ----------------------------------------------------------------- */

/**
 * The open hails. When the card that holds focus leaves (its hail was
 * answered, here or elsewhere), focus moves to the card now in its place,
 * else the one before it, else the panel's heading, which stays with "All
 * hails answered." until focus leaves it: a keyboard user keeps their
 * place in the panel.
 */
export function HailsPanel(props: { state: State; csrf: string }) {
  const hails = openHails(props.state);
  const ids = hails.map((h) => h.id);
  const focused = useRef("");
  const [cleared, setCleared] = useState(false);
  useEffect(() => {
    const gone = focused.current;
    if (gone === "" || ids.includes(gone)) return;
    focused.current = "";
    const active = document.activeElement;
    if (active !== null && active !== document.body) return; // focus went elsewhere
    const next = hails.find((h) => h.id > gone) ?? hails[hails.length - 1];
    if (next) document.getElementById(hailCardId(next.id))?.focus();
    else setCleared(true);
  }, [ids.join(" ")]);
  useEffect(() => {
    if (cleared && ids.length > 0) setCleared(false);
    else if (cleared) document.getElementById("hail-h")?.focus();
  }, [cleared, ids.length]);
  if (hails.length === 0 && !cleared) return null;
  return (
    <section class="panel hails area-hails" aria-labelledby="hail-h">
      <Heading id="hail-h" tone="blue" blink={hails.length > 0} focusable onBlur={() => setCleared(false)}>
        Hailing frequencies open
      </Heading>
      {hails.length === 0 && <p class="notice">All hails answered.</p>}
      <div
        class="stack"
        onFocusIn={(e) => {
          const card = (e.target as HTMLElement).closest?.(".row.hail");
          if (card?.id) focused.current = card.id.slice(hailCardId("").length);
        }}
      >
        {hails.map((h) => {
          switch (hailKind(h)) {
            case "review":
              return <ReviewCard key={h.id} hail={h} repoUrls={props.state.repoUrls} csrf={props.csrf} />;
            case "triage":
              return <TriageCard key={h.id} hail={h} />;
            case "land":
              return <LandCard key={h.id} hail={h} repoUrls={props.state.repoUrls} csrf={props.csrf} />;
            case "stalled":
              return <StalledCard key={h.id} hail={h} repoUrls={props.state.repoUrls} csrf={props.csrf} />;
            default:
              return (
                <HailCard
                  key={h.id}
                  hail={h}
                  crew={props.state.crew.get(h.crew_id)}
                  order={props.state.order.get(h.order_id)}
                  repoUrls={props.state.repoUrls}
                  csrf={props.csrf}
                />
              );
          }
        })}
      </div>
    </section>
  );
}

/**
 * One open hail and its answer buttons. Question and context are the crew
 * member's text, shown as text. A bridge-raised hail (#152) has no order
 * of its own to belay, so it has no belay control, and its meta line
 * names the bridge and the order it is about instead of a crew member.
 */
export function HailCard(props: {
  hail: HailView;
  crew: CrewView | undefined;
  order: OrderView | undefined;
  csrf: string;
  repoUrls?: Readonly<Record<string, string>>;
}) {
  const { hail: h } = props;
  const [pending, setPending] = useState(false);
  const [sent, setSent] = useState(false);
  const [error, setError] = useState("");
  const [decision, setDecision] = useState("");
  // A campaign triage (#187) shows its numbered mediums for the JSON form,
  // and takes a decision beside its options.
  const triage = h.deliver_to !== undefined && h.category === categoryCampaignTriage;
  const findings = triage ? triageFindings(h) : [];
  const ctx = triage ? "" : contextText(h.context);

  const answer = async (args: { option?: number; decision?: string }) => {
    if (pending || sent) return;
    setPending(true);
    setError("");
    try {
      await answerHail(props.csrf, h.id, args);
      setSent(true); // the feed closes the hail
    } catch (err) {
      setError(errorText(err));
    } finally {
      setPending(false);
    }
  };

  const busy = pending || sent;
  const cardId = hailCardId(h.id);
  const bridge = hailKind(h) !== "crew";
  const meta = bridge
    ? bridgeHailMeta(h)
    : [h.id, `${shortName(h.crew_id)}${props.crew?.current_beam ? ` on ${props.crew.current_beam}` : ""}`, props.order?.title ?? h.order_id];
  return (
    <div id={cardId} class="row hail" tabIndex={-1} role="group" aria-labelledby={`${cardId}-meta ${cardId}-q`} aria-busy={busy}>
      <div id={`${cardId}-meta`} class="mono hail-meta">
        {meta.join(" · ")}
      </div>
      {triage && <CampaignIssue hail={h} repoUrls={props.repoUrls ?? {}} />}
      <p id={`${cardId}-q`} class="hail-q">
        "{h.question}"
      </p>
      {ctx && <p class="mono hail-ctx">{ctx}</p>}
      {triage && <FindingList findings={findings} label="Medium findings" />}
      <div class="btns">
        {h.options.map((opt, i) => (
          <button type="button" key={i} class={i === 0 ? "btn blue" : "btn blue-o"} disabled={busy} onClick={() => void answer({ option: i })}>
            {opt}
          </button>
        ))}
        {(h.options.length === 0 || triage) && (
          <form
            class="decide"
            onSubmit={(e) => {
              e.preventDefault();
              if (decision.trim() !== "") void answer({ decision: decision.trim() });
            }}
          >
            <label for={`decide-${h.id}`} class="sr-only">
              {triage ? `Decision for ${h.id}, as a JSON object of finding numbers` : `Decision for ${h.id}`}
            </label>
            <input
              id={`decide-${h.id}`}
              class="field mono"
              type="text"
              placeholder={triage ? '{"fix": [1], "file": [2], "drop": [3]}' : "Your decision"}
              value={decision}
              disabled={busy}
              onInput={(e) => setDecision(e.currentTarget.value)}
            />
            <button type="submit" class="btn blue" disabled={busy}>
              Answer
            </button>
          </form>
        )}
        {!bridge && (
          <ConfirmAction
            idBase={`belay-${h.id}`}
            label="Belay order"
            ariaLabel={`Belay order ${h.order_id}`}
            prompt={belayPrompt(h.order_id, props.order?.open_children ?? 0)}
            confirmLabel="Belay"
            run={() => belayOrder(props.csrf, h.order_id)}
          />
        )}
      </div>
      {sent && <p class="notice">Answer sent. Waiting for the bridge.</p>}
      {error && (
        <p class="error" role="alert">
          {error}
        </p>
      )}
    </div>
  );
}

/** A bridge hail's meta line: its id, the bridge, and the order it is about (whose findings, for the findings hails), when it names one. */
function bridgeHailMeta(h: HailView): string[] {
  if (isCampaignHail(h)) return campaignHailMeta(h);
  const order = bridgeHailOrder(h);
  if (!order) return [h.id, "Bridge"];
  return [h.id, "Bridge", hailKind(h) === "bridge" ? order : `findings of ${order}`];
}

/**
 * Command's advisory triage of an order's findings (#155): the captain
 * has nothing to answer (the bridge refuses it), so it shows what is
 * happening and no controls. The review comes as its own hail.
 */
export function TriageCard(props: { hail: HailView }) {
  const { hail: h } = props;
  const cardId = hailCardId(h.id);
  const order = bridgeHailOrder(h);
  return (
    <div id={cardId} class="row hail triage" tabIndex={-1} role="group" aria-labelledby={`${cardId}-meta ${cardId}-q`}>
      <div id={`${cardId}-meta`} class="mono hail-meta">
        {bridgeHailMeta(h).join(" · ")}
      </div>
      <p id={`${cardId}-q`} class="triage-q">
        <Light tone="blue" blink />
        {order ? `Command is triaging ${order}'s findings` : "Command is triaging an order's findings"}
      </p>
    </div>
  );
}

type ReviewSent = "" | "answer" | "confirmed";

/**
 * The findings review (#156, #157): the issues filed from an order's
 * findings, each with a Discard box, and the findings triage dropped or
 * filing failed, each with a Restore box and its reason. Rows are
 * numbered as Command's triage numbers the findings (reviewNumbers), as
 * `bmus hails review` does. Command's rationale shows once, above
 * the findings it dropped.
 *
 * Keep all keeps every issue and Discard all discards every one, behind a
 * confirm; either also restores the findings ticked. Apply sends the
 * answer the boxes make (reviewDecision): nothing ticked keeps everything,
 * discards name their refs, and every issue ticked asks first as Discard
 * all does. Without restores, Keep all and Discard all send option 0 and
 * 1. One answer at a time: every control waits on a pending one. Titles,
 * reasons and errors are the agent's or Command's text, shown as text; a
 * ref links to its issue only where issueURL can tell the address.
 */
export function ReviewCard(props: { hail: HailView; repoUrls: Readonly<Record<string, string>>; csrf: string }) {
  const { hail: h } = props;
  const rc = reviewContext(h);
  const [discard, setDiscard] = useState<ReadonlySet<string>>(new Set());
  const [restore, setRestore] = useState<ReadonlySet<string>>(new Set());
  const [pending, setPending] = useState(false);
  const [sent, setSent] = useState<ReviewSent>("");
  const [error, setError] = useState("");
  const resultRef = useRef<HTMLParagraphElement>(null);
  useEffect(() => {
    if (sent === "answer") resultRef.current?.focus();
  }, [sent]);

  const answer = async (args: { option?: number; decision?: string }) => {
    if (pending || sent) return;
    setPending(true);
    setError("");
    try {
      await answerHail(props.csrf, h.id, args);
      setSent("answer"); // the feed closes the hail
    } catch (err) {
      setError(errorText(err));
    } finally {
      setPending(false);
    }
  };
  // A confirm's run: ConfirmAction shows its refusal and its "Sent" note.
  const confirmed = async (args: { option?: number; decision?: string }) => {
    setPending(true);
    setError("");
    try {
      await answerHail(props.csrf, h.id, args);
      setSent("confirmed");
    } finally {
      setPending(false);
    }
  };
  const toggle = (set: ReadonlySet<string>, put: (s: ReadonlySet<string>) => void, v: string, on: boolean) => {
    const next = new Set(set);
    if (on) next.add(v);
    else next.delete(v);
    put(next);
  };

  const busy = pending || sent !== "";
  const cardId = hailCardId(h.id);
  const sumId = `${cardId}-sum`;
  const findings = [
    ...rc.dropped.map((f) => ({ key: f.key, title: f.title, kind: "dropped" as const, dup: f.duplicate_of, error: "" })),
    ...rc.failed.map((f) => ({ key: f.key, title: f.title, kind: "failed" as const, dup: undefined, error: f.error })),
  ];
  const rationale = triageRationale(rc);
  const numbers = reviewNumbers(rc);
  const keys = restoreKeys(rc, restore);
  const keepDecision = reviewAllDecision(rc, "keep", restore);
  const discardDecision = reviewAllDecision(rc, "discard", restore);
  const allTicked = rc.issues.length > 0 && rc.issues.every((i) => discard.has(i.ref));
  const ref = (r: string) => {
    const url = issueURL(r, props.repoUrls);
    return url ? (
      <a class="mono" href={url} target="_blank" rel="noopener noreferrer">
        {r}
      </a>
    ) : (
      <span class="mono">{r}</span>
    );
  };
  return (
    <div id={cardId} class="row hail review" tabIndex={-1} role="group" aria-labelledby={`${cardId}-meta ${cardId}-q`} aria-busy={busy}>
      <div id={`${cardId}-meta`} class="mono hail-meta">
        {bridgeHailMeta(h).join(" · ")}
      </div>
      <p id={`${cardId}-q`} class="hail-q">
        "{h.question}"
      </p>
      {rc.issues.length > 0 && (
        <fieldset class="review-set" disabled={busy}>
          <legend class="review-legend">Filed: {issuesText(rc.issues.length)}</legend>
          <ul class="review-list">
            {rc.issues.map((is, i) => {
              const id = `${cardId}-d${i}`;
              return (
                <li key={is.ref} class={`review-item discard${discard.has(is.ref) ? " ticked" : ""}`}>
                  <span class="review-n mono" aria-hidden="true">
                    {numbers.get(is.key)}
                  </span>
                  <input
                    id={id}
                    type="checkbox"
                    class="review-box"
                    checked={discard.has(is.ref)}
                    aria-describedby={`${id}-t`}
                    onChange={(e) => toggle(discard, setDiscard, is.ref, e.currentTarget.checked)}
                  />
                  <label for={id} class="review-verb">
                    Discard<span class="sr-only"> {is.ref}</span>
                  </label>
                  <span class="review-ref">{ref(is.ref)}</span>
                  <span id={`${id}-t`} class="review-title">
                    {is.title}
                  </span>
                </li>
              );
            })}
          </ul>
        </fieldset>
      )}
      {findings.length > 0 && (
        <fieldset class="review-set" disabled={busy}>
          <legend class="review-legend">Not filed: {findings.length}</legend>
          {rationale.map((r, i) => (
            <p key={i} class="mono review-rationale">
              Command: {r}
            </p>
          ))}
          <ul class="review-list">
            {findings.map((f, i) => {
              const id = `${cardId}-r${i}`;
              return (
                <li key={f.key} class={`review-item restore${restore.has(f.key) ? " ticked" : ""}`}>
                  <span class="review-n mono" aria-hidden="true">
                    {numbers.get(f.key)}
                  </span>
                  <input
                    id={id}
                    type="checkbox"
                    class="review-box"
                    checked={restore.has(f.key)}
                    aria-describedby={`${id}-t ${id}-w`}
                    onChange={(e) => toggle(restore, setRestore, f.key, e.currentTarget.checked)}
                  />
                  <label for={id} class="review-verb">
                    Restore<span class="sr-only"> {f.key}</span>
                  </label>
                  <span class="review-ref mono">{f.key}</span>
                  <span id={`${id}-t`} class="review-title">
                    {f.title}
                  </span>
                  <span id={`${id}-w`} class={`review-why mono${f.kind === "failed" ? " failed" : ""}`}>
                    {f.kind === "failed" ? `Failed: ${f.error}` : f.dup !== undefined ? <>Dropped: duplicate of {ref(f.dup)}</> : "Dropped by Command"}
                  </span>
                </li>
              );
            })}
          </ul>
        </fieldset>
      )}
      <p id={sumId} class="mono hail-ctx">
        {reviewSummary(rc, discard, restore)}
      </p>
      <div class="btns">
        <button
          type="button"
          class="btn blue"
          disabled={busy}
          aria-label={keys.length > 0 ? `Keep all and restore ${keys.join(", ")}` : undefined}
          onClick={() => void answer(keepDecision ? { decision: keepDecision } : { option: 0 })}
        >
          Keep all
        </button>
        {allTicked ? (
          <ConfirmAction
            key="apply-all"
            idBase={`${cardId}-apply`}
            label="Apply"
            ariaLabel={`Apply: ${reviewSummary(rc, discard, restore).replace(/^Apply /, "")}`}
            askClass="btn blue-o"
            prompt={discardAllPrompt(rc, restore)}
            confirmLabel="Apply"
            disabled={busy}
            run={() => confirmed({ decision: reviewDecision(rc, discard, restore) })}
          />
        ) : (
          <button
            type="button"
            class="btn blue-o"
            disabled={busy}
            aria-describedby={sumId}
            onClick={() => void answer({ decision: reviewDecision(rc, discard, restore) })}
          >
            Apply
          </button>
        )}
        {rc.issues.length > 0 && (
          <ConfirmAction
            idBase={`${cardId}-all`}
            label="Discard all"
            ariaLabel={`Discard all ${issuesText(rc.issues.length)} from ${h.id}`}
            prompt={discardAllPrompt(rc, restore)}
            confirmLabel="Discard all"
            disabled={busy}
            run={() => confirmed(discardDecision ? { decision: discardDecision } : { option: 1 })}
          />
        )}
      </div>
      <div role="status" aria-live="polite">
        {sent === "answer" && (
          <p ref={resultRef} class="notice" tabIndex={-1}>
            Answer sent. Waiting for the bridge.
          </p>
        )}
      </div>
      {error && (
        <p class="error" role="alert">
          {error}
        </p>
      )}
    </div>
  );
}

/* Campaign hails (#187) ---------------------------------------------------- */

/**
 * One answer at a time for a hail card: answer sends and shows "Answer
 * sent" (focused, in a live region); confirmed is a ConfirmAction's run,
 * which shows its own note and refusal. While either is pending or sent,
 * busy holds.
 */
function useHailAnswer(csrf: string, hailId: string) {
  const [pending, setPending] = useState(false);
  const [sent, setSent] = useState<ReviewSent>("");
  const [error, setError] = useState("");
  const resultRef = useRef<HTMLParagraphElement>(null);
  useEffect(() => {
    if (sent === "answer") resultRef.current?.focus();
  }, [sent]);
  const answer = async (args: { option?: number; decision?: string }) => {
    if (pending || sent) return;
    setPending(true);
    setError("");
    try {
      await answerHail(csrf, hailId, args);
      setSent("answer"); // the feed closes the hail
    } catch (err) {
      setError(errorText(err));
    } finally {
      setPending(false);
    }
  };
  const confirmed = async (args: { option?: number; decision?: string }) => {
    setPending(true);
    setError("");
    try {
      await answerHail(csrf, hailId, args);
      setSent("confirmed");
    } finally {
      setPending(false);
    }
  };
  return { busy: pending || sent !== "", sent, error, resultRef, answer, confirmed };
}

/** The result line and refusal under a hail card's buttons. */
function AnswerResult(props: { sent: ReviewSent; error: string; resultRef: { current: HTMLParagraphElement | null } }) {
  return (
    <>
      <div role="status" aria-live="polite">
        {props.sent === "answer" && (
          <p ref={props.resultRef} class="notice" tabIndex={-1}>
            Answer sent. Waiting for the bridge.
          </p>
        )}
      </div>
      {props.error && (
        <p class="error" role="alert">
          {props.error}
        </p>
      )}
    </>
  );
}

/** A campaign hail's task: its ref, linked where issueURL can tell the address, and the issue's title as text. */
function CampaignIssue(props: { hail: HailView; repoUrls: Readonly<Record<string, string>> }) {
  const c = landContext(props.hail);
  if (c.task === "" && c.issue_title === "") return null;
  const url = c.task ? issueURL(c.task, props.repoUrls) : undefined;
  return (
    <p class="camp-issue">
      {c.task &&
        (url ? (
          <a class="mono camp-ref" href={url} target="_blank" rel="noopener noreferrer">
            {c.task}
          </a>
        ) : (
          <span class="mono camp-ref">{c.task}</span>
        ))}
      {c.issue_title && <span class="camp-title">{c.issue_title}</span>}
    </p>
  );
}

/** A review's numbered findings: number, severity, title, place and body, all as text. */
export function FindingList(props: { findings: readonly CampaignFinding[]; label: string }) {
  if (props.findings.length === 0) return null;
  return (
    <section class="camp-findings" aria-label={props.label}>
      <h3 class="camp-sub">
        {props.label}: {props.findings.length}
      </h3>
      <ol class="camp-list">
        {props.findings.map((f) => (
          <li key={f.n} class={`camp-finding sev-${f.severity === "high" || f.severity === "medium" || f.severity === "low" ? f.severity : "other"}`}>
            <span class="review-n mono">{f.n}</span>
            <span class="camp-sev mono">{f.severity}</span>
            <span class="camp-ftitle">{f.title}</span>
            {findingPlace(f) && <span class="camp-place mono">{findingPlace(f)}</span>}
            {f.body && <p class="camp-body">{f.body}</p>}
          </li>
        ))}
      </ol>
    </section>
  );
}

/**
 * A land or stall hail's options: the land option alone, retry with a
 * note field (`retry: <note>`, or the option alone with no note), and
 * drop behind a confirm. An option the hail lacks has no control: a stall
 * with no head offers no land.
 */
function CampaignChoices(props: {
  hail: HailView;
  cardId: string;
  labels: { land: string; retry: string };
  dropPrompt: string;
  answer: ReturnType<typeof useHailAnswer>;
}) {
  const { hail: h, cardId, answer: a } = props;
  const [note, setNote] = useState("");
  const land = optionIndex(h, "land");
  const retry = optionIndex(h, "retry");
  const drop = optionIndex(h, "drop");
  const noteId = `${cardId}-note`;
  return (
    <div class="camp-actions">
      {retry >= 0 && (
        <div class="camp-note">
          <label for={noteId} class="camp-note-label">
            Note for {props.labels.retry.toLowerCase()}
          </label>
          <input
            id={noteId}
            class="field"
            type="text"
            placeholder="Optional: what the next round should know"
            value={note}
            disabled={a.busy}
            onInput={(e) => setNote(e.currentTarget.value)}
          />
        </div>
      )}
      <div class="btns">
        {land >= 0 && (
          <button type="button" class="btn blue" disabled={a.busy} onClick={() => void a.answer({ option: land })}>
            {props.labels.land}
          </button>
        )}
        {retry >= 0 && (
          <button
            type="button"
            class="btn blue-o"
            disabled={a.busy}
            aria-describedby={noteId}
            onClick={() => {
              const d = choiceDecision("retry", note);
              void a.answer(d === "retry" ? { option: retry } : { decision: d });
            }}
          >
            {props.labels.retry}
          </button>
        )}
        {drop >= 0 && (
          <ConfirmAction
            idBase={`${cardId}-drop`}
            label="Drop"
            ariaLabel={`Drop the task of ${h.id}`}
            prompt={props.dropPrompt}
            confirmLabel="Drop"
            disabled={a.busy}
            run={() => a.confirmed({ option: drop })}
          />
        )}
      </div>
    </div>
  );
}

/**
 * A campaign_land hail (#187): the task's ref and issue title, where the
 * head lands (repo and branch), the head and review base, the commits
 * over the base and the last review's summary. A land Command decided on
 * a stall has no passing review (#185), and the card says so above the
 * summary. "Land" sends option 0, "Another round" retry with the note,
 * and "Drop" asks first. Every subject, title and summary is agent or
 * tracker text, rendered as text.
 */
export function LandCard(props: { hail: HailView; repoUrls: Readonly<Record<string, string>>; csrf: string }) {
  const { hail: h } = props;
  const c = landContext(h);
  const a = useHailAnswer(props.csrf, h.id);
  const cardId = hailCardId(h.id);
  const more = c.commit_count - c.commits.length;
  return (
    <div id={cardId} class="row hail camp land" tabIndex={-1} role="group" aria-labelledby={`${cardId}-meta ${cardId}-q`} aria-busy={a.busy}>
      <div id={`${cardId}-meta`} class="mono hail-meta">
        {campaignHailMeta(h).join(" · ")}
      </div>
      <CampaignIssue hail={h} repoUrls={props.repoUrls} />
      <p id={`${cardId}-q`} class="hail-q">
        "{h.question}"
      </p>
      <dl class="facts camp-facts">
        <dt>Branch</dt>
        <dd class="mono">
          {c.repo && `${c.repo}: `}
          {c.branch}
        </dd>
        <dt>Head</dt>
        <dd class="mono">
          {c.head}
          {c.head_commit && ` at ${shortCommit(c.head_commit)}`}
        </dd>
        <dt>Review base</dt>
        <dd class="mono">{shortCommit(c.review_base)}</dd>
        {c.rounds > 0 && (
          <>
            <dt>Rounds</dt>
            <dd class="mono">{c.rounds}</dd>
          </>
        )}
      </dl>
      <section class="camp-commits" aria-label="Commits">
        <h3 class="camp-sub">{commitsText(c.commit_count)} over the review base</h3>
        <ul class="camp-list">
          {c.commits.map((cm) => (
            <li key={cm.sha} class="camp-commit">
              <span class="mono camp-sha">{shortCommit(cm.sha)}</span>
              <span class="camp-subject">{cm.subject}</span>
            </li>
          ))}
        </ul>
        {more > 0 && <p class="mono hail-ctx">and {more} more</p>}
      </section>
      <section class={`camp-review${c.passed ? " passed" : " unpassed"}`} aria-label="Review">
        <h3 class="camp-sub">
          {c.passed ? `Review ${c.review} passed` : c.review ? `No passing review: ${c.review} was the last` : "No review yet"}
        </h3>
        {!c.passed && <p class="camp-warn">This work has no passing review.</p>}
        {c.review_summary !== "" && <p class="camp-summary">{c.review_summary}</p>}
      </section>
      <CampaignChoices
        hail={h}
        cardId={cardId}
        labels={{ land: "Land", retry: "Another round" }}
        dropPrompt={dropPrompt(c)}
        answer={a}
      />
      <AnswerResult sent={a.sent} error={a.error} resultRef={a.resultRef} />
    </div>
  );
}

/**
 * A campaign_stalled hail (#187): the task, the stall's code and reason,
 * the round of its max, the head and the order it stalled on, and the
 * last review's blocking findings, then the hail's options: land (only
 * with a head), retry with a note and drop behind a confirm.
 */
export function StalledCard(props: { hail: HailView; repoUrls: Readonly<Record<string, string>>; csrf: string }) {
  const { hail: h } = props;
  const c = stalledContext(h);
  const a = useHailAnswer(props.csrf, h.id);
  const cardId = hailCardId(h.id);
  return (
    <div id={cardId} class="row hail camp stalled" tabIndex={-1} role="group" aria-labelledby={`${cardId}-meta ${cardId}-q`} aria-busy={a.busy}>
      <div id={`${cardId}-meta`} class="mono hail-meta">
        {campaignHailMeta(h).join(" · ")}
      </div>
      <CampaignIssue hail={h} repoUrls={props.repoUrls} />
      <p id={`${cardId}-q`} class="hail-q">
        "{h.question}"
      </p>
      <p class="camp-reason">
        {c.code && <span class="chip-o tone-red mono">{c.code}</span>}
        {c.reason && <span class="camp-why">{c.reason}</span>}
      </p>
      <dl class="facts camp-facts">
        <dt>Round</dt>
        <dd class="mono">{c.max_rounds > 0 ? `${c.round} of ${c.max_rounds}` : c.round}</dd>
        <dt>Head</dt>
        <dd class="mono">{c.head ? `${c.head}${c.head_commit ? ` at ${shortCommit(c.head_commit)}` : ""}` : "none yet"}</dd>
        {c.order && (
          <>
            <dt>Stalled on</dt>
            <dd class="mono">{c.order}</dd>
          </>
        )}
      </dl>
      <FindingList findings={c.blocking} label="Blocking findings" />
      <CampaignChoices
        hail={h}
        cardId={cardId}
        labels={{ land: "Land as it is", retry: "Retry" }}
        dropPrompt={dropPrompt(c)}
        answer={a}
      />
      <AnswerResult sent={a.sent} error={a.error} resultRef={a.resultRef} />
    </div>
  );
}

/* Countermand ----------------------------------------------------------- */

/**
 * Decided hails the captain may still countermand (#153): Command's
 * answers and captain timeouts' default options, until countermand_until.
 * now is the console's clock, which already ticks for the header: a card
 * leaves at the first tick past its window, and the bridge refuses a late
 * countermand meanwhile.
 */
export function CountermandPanel(props: { state: State; csrf: string; now: Date }) {
  const hails = countermandableHails(props.state, props.now);
  if (hails.length === 0) return null;
  return (
    <section class="panel area-hails" aria-labelledby="countermand-h">
      <Heading id="countermand-h" tone="gold">
        Decided without you
      </Heading>
      <div class="stack">
        {hails.map((h) => (
          <CountermandCard key={h.id} hail={h} order={props.state.order.get(h.order_id)} csrf={props.csrf} />
        ))}
      </div>
    </section>
  );
}

/**
 * One decided hail and its countermand control, after #78's confirm
 * pattern: a confirm per option other than the current decision, or a
 * decision field for a free-text hail, and an optional reason. Each
 * control is keyed on the decision, so a countermand the feed brings back
 * resets it.
 */
export function CountermandCard(props: { hail: HailView; order: OrderView | undefined; csrf: string }) {
  const { hail: h } = props;
  const [reason, setReason] = useState("");
  const [decision, setDecision] = useState("");
  // The bridge's note on the last countermand sent from here (no live run):
  // kept on the card, as the confirm control resets once the feed brings
  // the new decision.
  const [note, setNote] = useState("");
  const send = async (a: { option?: number; decision?: string }) => {
    const res = await countermandHail(props.csrf, h.id, args(a));
    setNote(res.note ?? "");
  };
  const cardId = `countermand-${h.id}`;
  const by = decidedByLabel(h);
  const until = h.countermand_until ? clockHM(new Date(h.countermand_until)) : "";
  const args = (a: { option?: number; decision?: string }) => ({ ...a, ...(reason.trim() !== "" && { reason: reason.trim() }) });
  const meta = [h.id, shortName(h.crew_id), props.order?.title ?? h.order_id];
  // An order the console doesn't hold is a finished one (the snapshot has every open and away order).
  const o = props.order;
  const active = o !== undefined && (o.state === "away" || o.state === "hailing");
  // A parked order's mission holds the correction for the resume (#167). It
  // reaches the hailer only if the hailer resumes it: an order parked on
  // another crew member, or recalled (unclaimed, parked), may never be.
  const recalled = o?.parked_at !== undefined && (o.state === "pool" || o.state === "directed");
  const parkedAny = active && o.parked_at !== undefined;
  const parked = parkedAny && o.assignee === h.crew_id;
  const parkedElsewhere = parkedAny && !parked;
  const live = active && !parkedAny;
  const hailer = shortName(h.crew_id);
  const after = live
    ? "The agent is told to change course."
    : parked
      ? "Parked: the agent is told on resume."
      : parkedElsewhere
        ? `Parked on ${shortName(o.assignee)}: the agent is told only if ${hailer} resumes it.`
        : recalled
          ? `Recalled: the agent is told only if ${hailer} resumes it.`
          : "No live run: the agent won't be told.";
  return (
    <div id={cardId} class="row hail" role="group" aria-labelledby={`${cardId}-meta ${cardId}-q`}>
      <div id={`${cardId}-meta`} class="mono hail-meta">
        {meta.join(" · ")}
      </div>
      <p id={`${cardId}-q`} class="hail-q">
        "{h.question}"
      </p>
      <p class="decided-by">
        Decided by {by}: <span class="mono">{h.decision}</span>
        {h.confidence !== undefined && h.decided_by === "command" && ` (confidence ${Math.round(h.confidence * 100)}%)`}
      </p>
      {h.rationale && <p class="mono hail-ctx">{h.rationale}</p>}
      <p class="mono hail-ctx">Countermand until {until}</p>
      {parked && <p class="notice">{h.order_id} is parked: the agent is told on resume.</p>}
      {parkedElsewhere && (
        <p class="notice">
          {h.order_id} is parked on {shortName(o.assignee)}: the agent is told only if {hailer} resumes it.
        </p>
      )}
      {recalled && (
        <p class="notice">
          {h.order_id} is recalled: the agent is told only if {hailer} resumes it.
        </p>
      )}
      {!live && !parkedAny && !recalled && <p class="notice">No live run for {h.order_id}: a countermand is recorded, but the agent won't be told.</p>}
      <div class="decide">
        <label for={`${cardId}-reason`} class="sr-only">
          Reason for countermanding {h.id}
        </label>
        <input
          id={`${cardId}-reason`}
          class="field"
          type="text"
          placeholder="Reason (optional)"
          value={reason}
          onInput={(e) => setReason(e.currentTarget.value)}
        />
      </div>
      <div class="btns">
        {h.options.map((opt, i) =>
          opt === h.decision ? null : (
            <ConfirmAction
              key={`${h.decision}-${i}`}
              idBase={`${cardId}-${i}`}
              label={`Countermand: ${opt}`}
              ariaLabel={`Countermand ${h.id} with ${opt}`}
              prompt={`Replace "${h.decision}" with "${opt}"? ${after}`}
              confirmLabel="Countermand"
              run={() => send({ option: i })}
            />
          ),
        )}
        {h.options.length === 0 && (
          <>
            <label for={`${cardId}-decision`} class="sr-only">
              New decision for {h.id}
            </label>
            <input
              id={`${cardId}-decision`}
              class="field"
              type="text"
              placeholder="Your decision"
              value={decision}
              onInput={(e) => setDecision(e.currentTarget.value)}
            />
            <ConfirmAction
              key={h.decision}
              idBase={`${cardId}-text`}
              label="Countermand"
              ariaLabel={`Countermand ${h.id}`}
              prompt={`Replace "${h.decision}"? ${after}`}
              confirmLabel="Countermand"
              disabled={decision.trim() === ""}
              run={() => send({ decision: decision.trim() })}
            />
          </>
        )}
      </div>
      {note && (
        <p class="notice" role="status">
          Countermanded. {note}.
        </p>
      )}
    </div>
  );
}

/* Open orders ----------------------------------------------------------- */

export function OpenOrdersPanel(props: { state: State; csrf: string }) {
  const orders = openOrders(props.state);
  return (
    <section class="panel area-open" aria-labelledby="open-h">
      <Heading id="open-h" tone="cream">
        Open orders <span class="mono count">{orders.length}</span>
      </Heading>
      <div class="stack">
        {orders.map((o) => {
          const p = priorityInfo(o.priority);
          const d = divisionInfo(o.division);
          return (
            <div class="row" key={o.id}>
              <div class="row-head">
                <span class="mono order-id">{o.id}</span>
                <span class={`chip bg-${p.tone} on-${p.tone}`}>{p.label}</span>
                {o.division && <span class={`chip-o tone-${d.tone}`}>{d.label}</span>}
                <span class="row-state">{openOrderState(o)}</span>
              </div>
              <div class="order-title">{o.title}</div>
              {campaignOrderText(o) && <div class="mono order-task">{campaignOrderText(o)}</div>}
              <div class="row-actions">
                <RouteControl key={routeValue(o)} state={props.state} order={o} csrf={props.csrf} />
                <ConfirmAction
                  idBase={`withdraw-${o.id}`}
                  label="Withdraw"
                  ariaLabel={`Withdraw ${o.id}`}
                  prompt={`Withdraw ${o.id}? Nobody will take it.`}
                  confirmLabel="Withdraw"
                  run={() => withdrawOrder(props.csrf, o.id)}
                />
              </div>
            </div>
          );
        })}
        {orders.length === 0 && <p class="empty">No open orders. The pool is clear.</p>}
      </div>
    </section>
  );
}

/**
 * The route picker of an unclaimed order (#78): the order's division's
 * pool (not for an order without a division, which route refuses) or a
 * crew member on the manifest (not one in sickbay, whom route refuses).
 * While a route is out, and once one is sent, the controls are
 * aria-disabled rather than disabled, so focus stays on them. The pick goes out with the Route button, not on change: a
 * keyboard user can browse the options. Its parent keys it by the order's
 * route, so a routed order starts from its new one.
 */
function RouteControl(props: { state: State; order: OrderView; csrf: string }) {
  const o = props.order;
  const current = routeValue(o);
  const [pick, setPick] = useState(current);
  const [pending, setPending] = useState(false);
  // Set once a route is sent: the button stays inert until the feed
  // brings the new route, which remounts this control.
  const [sent, setSent] = useState(false);
  const [error, setError] = useState("");
  const crew = manifest(props.state);
  const inert = pending || sent || pick === current;
  const route = async () => {
    if (inert) return;
    setPending(true);
    setError("");
    try {
      await routeOrder(props.csrf, o.id, routeArgs(pick));
      setSent(true);
    } catch (err) {
      setError(errorText(err));
    } finally {
      setPending(false);
    }
  };
  const selId = `route-${o.id}`;
  return (
    <>
      <label for={selId} class="inline-lbl">
        Route to
      </label>
      <select
        id={selId}
        class="field route"
        value={pick}
        aria-disabled={pending || sent || undefined}
        onChange={(e) => {
          if (pending || sent) e.currentTarget.value = pick;
          else setPick(e.currentTarget.value);
        }}
      >
        <option value="pool" disabled={o.division === "" && current !== "pool"}>
          Open pool (any free crew)
        </option>
        {crew.map((c) => (
          <option key={c.id} value={c.id} disabled={!canTake(c) && c.id !== current}>
            {crewOptionLabel(c)}
          </option>
        ))}
      </select>
      <button
        type="button"
        class="btn ghost"
        aria-label={`Route ${o.id}`}
        aria-disabled={inert || undefined}
        aria-busy={pending || undefined}
        onClick={() => void route()}
      >
        {pending ? "Routing" : "Route"}
      </button>
      {error && (
        <p class="error act-msg" role="alert">
          {error}
        </p>
      )}
    </>
  );
}

/* Away missions --------------------------------------------------------- */

/**
 * A field of an order's usage against its limit (#120), as a thin meter:
 * the away-missions card's and the mission report's. Capped at full past
 * the limit; the text says how far.
 */
export function UsageMeter(props: { id: string; field: UsageField }) {
  const f = props.field;
  if (f.pct === undefined) return null;
  return (
    <div
      class="bar thin"
      role="meter"
      aria-label={`${props.id} ${f.unit} used`}
      aria-valuemin={0}
      aria-valuemax={f.limit}
      aria-valuenow={Math.min(f.used, f.limit)}
      aria-valuetext={f.text}
    >
      <div class={`fill bg-${f.tone}`} style={{ width: `${f.pct}%` }} />
    </div>
  );
}

/**
 * An order's usage against its budget (#120), on its away-missions card:
 * each field with a limit as "420k / 1M tokens" over a thin meter, green,
 * gold from 80% and red from 100%; without a budget, the usage alone.
 */
export function UsageLine(props: { order: OrderView }) {
  const { budgeted, fields } = orderUsage(props.order);
  const id = props.order.id;
  return (
    <div class="usage" role="group" aria-label={`${id} usage`}>
      <span class="inline-lbl">{budgeted ? "Budget" : "No budget"}</span>
      {budgeted ? (
        fields.map((f) => (
          <div class="usage-field" key={f.unit}>
            <span class={`mono usage-text tone-${f.tone}`}>{f.text}</span>
            <UsageMeter id={id} field={f} />
          </div>
        ))
      ) : (
        <span class="mono usage-text tone-grey">{fields.map((f) => f.text).join(", ")}</span>
      )}
    </div>
  );
}

export function AwayPanel(props: { state: State; csrf: string }) {
  const orders = awayOrders(props.state);
  return (
    <section class="panel area-away" aria-labelledby="away-h">
      <Heading id="away-h" tone="gold">
        Away missions <span class="mono count">{orders.length}</span>
      </Heading>
      <div class="stack">
        {orders.map((o) => {
          const p = priorityInfo(o.priority);
          const c = props.state.crew.get(o.assignee);
          const hailing = o.state === "hailing";
          const tone: Tone = hailing ? "blue" : "gold";
          const prog = orderProgress(o);
          return (
            <div class="row" key={o.id}>
              <div class="row-head">
                <span class="mono order-id">{o.id}</span>
                <span class={`chip bg-${p.tone} on-${p.tone}`}>{p.label}</span>
                <span class={`away-crew tone-${divisionInfo(c?.division ?? "").tone}`}>{shortName(o.assignee)}</span>
                {c?.current_beam && <span class="mono muted-id">{c.current_beam}</span>}
                <span class={`row-state tone-${tone}`}>
                  <Light tone={tone} blink={hailing} />
                  {hailing ? "Hailing" : "Away"}
                </span>
              </div>
              <div class="order-title">{o.title}</div>
              {prog && prog.note && <div class="progress-note">{prog.note}</div>}
              <UsageLine order={o} />
              <div class="row-actions">
                <div class="grow">
                  <Bar pct={prog?.pct} tone={tone} label={`${o.id} progress`} />
                </div>
                <span class="mono pct">{pctText(prog?.pct)}</span>
                <ConfirmAction
                  idBase={`recall-${o.id}`}
                  label="Beam up"
                  ariaLabel={`Beam ${shortName(o.assignee)} up and return ${o.id} to the pool`}
                  prompt={`Beam ${shortName(o.assignee)} up? ${o.id} goes back to the pool from its last checkpoint.`}
                  confirmLabel="Beam up"
                  run={() => recallOrder(props.csrf, o.id)}
                />
              </div>
            </div>
          );
        })}
        {orders.length === 0 && <p class="empty">Nobody is on an away mission.</p>}
      </div>
    </section>
  );
}

/* Mission record -------------------------------------------------------- */

export function RecordPanel(props: { state: State; csrf: string }) {
  const rows = missionRecord(props.state);
  return (
    <section class="panel area-record" aria-labelledby="rec-h">
      <Heading id="rec-h" tone="green">
        Mission record
      </Heading>
      <div class="record">
        {rows.map((r) => {
          const o = props.state.order.get(r.orderId);
          const pushed = canPush(o) ? pushedTo(props.state, o.id) : undefined;
          return (
            <div class="record-row" key={r.orderId}>
              <div class="record-main">
                <span class="mono order-id">{r.orderId}</span>
                <span class="record-title">
                  {r.title || r.summary}
                  {r.title && r.summary && <span class="record-sum">{r.summary}</span>}
                </span>
              </div>
              <div class="record-foot">
                <div class="record-who">
                  {r.crewId && <span class="record-crew">{shortName(r.crewId)}</span>}
                  <span class={`chip bg-${r.tone} on-${r.tone}`}>{r.result}</span>
                  {pushed && <span class="mono pushed-to">pushed to {pushed}</span>}
                </div>
                {(canPush(o) || r.splitChildren !== undefined) && (
                  <div class="record-acts">
                    {canPush(o) && <DoneOrderActions state={props.state} order={o} csrf={props.csrf} />}
                    {r.splitChildren !== undefined && (
                      <ConfirmAction
                        idBase={`belay-${r.orderId}`}
                        label="Belay"
                        ariaLabel={`Belay ${r.orderId} and its open child orders`}
                        prompt={belayPrompt(r.orderId, r.splitChildren)}
                        confirmLabel="Belay"
                        run={() => belayOrder(props.csrf, r.orderId)}
                      />
                    )}
                  </div>
                )}
              </div>
            </div>
          );
        })}
        {rows.length === 0 && <p class="empty">No missions finished yet.</p>}
      </div>
    </section>
  );
}

/**
 * A done order's controls (#4): Push and "Mission report", which opens its
 * report in a new tab (#104). The pushed marker sits by the status chip
 * (#139).
 */
function DoneOrderActions(props: { state: State; order: OrderView; csrf: string }) {
  const o = props.order;
  return (
    <div class="record-btns">
      <PushAction orderId={o.id} remotes={pushRemotes(props.state, o)} run={(args) => pushOrder(props.csrf, o.id, args)} />
      <a class="btn ghost small" href={reportPath(o.id)} target="_blank" rel="noopener noreferrer" aria-label={`Open the mission report of ${o.id}`}>
        Mission report
      </a>
    </div>
  );
}

/**
 * Where a push stands: asked for (confirm), sending, done with its repos,
 * rejected by the remote (a 409, which offers a force push), or a force
 * push asked for (force).
 */
export type PushPhase =
  | { readonly kind: "idle" }
  | { readonly kind: "confirm" }
  | { readonly kind: "pending"; readonly force: boolean }
  | { readonly kind: "done"; readonly repos: readonly PushedRepo[] }
  | { readonly kind: "rejected"; readonly message: string }
  | { readonly kind: "force"; readonly message: string };

/**
 * A push's phase once it failed with err: a 409 is a rejection (a
 * diverged target), which a force push may overwrite; anything else is an
 * error, shown, with Push offered again.
 */
export function pushFailed(err: unknown): { phase: PushPhase; error: string } {
  if (err instanceof ApiError && err.status === 409) return { phase: { kind: "rejected", message: err.message }, error: "" };
  return { phase: { kind: "idle" }, error: errorText(err) };
}

/** How long a push's "Pushed." result shows before it clears itself (#139). */
export const PUSH_RESULT_MS = 15_000;

/**
 * Starts the timer that clears a push's result: for a done phase, clear
 * runs after ms; any other phase, a rejection or an error included, has
 * none. Returns the timer's cleanup, for an effect.
 */
export function clearPushResult(phase: PushPhase, clear: () => void, ms = PUSH_RESULT_MS): () => void {
  if (phase.kind !== "done") return () => {};
  const t = setTimeout(clear, ms);
  return () => clearTimeout(t);
}

/**
 * Push for a done order (#4), after ConfirmAction's pattern: Push asks,
 * naming the remotes and the target branch, which a field may change
 * (default bmus/<order>); its confirm button is focused, and Cancel or
 * Escape puts Push back, focused. While the push runs both buttons are
 * disabled. On success each repo's branch and commit show, with a pull
 * request link for a github.com remote. A 409 shows the rejection and a
 * separate "Force push" button behind its own confirm, which says the
 * remote's commits are overwritten. No force control shows before that.
 * The result clears itself after PUSH_RESULT_MS, and Push takes the focus
 * if the result had it (#139); the pushed marker by the status chip stays.
 */
export function PushAction(props: {
  orderId: string;
  remotes: readonly PushRemote[];
  run: (args: { target_branch?: string; force?: boolean }) => Promise<PushedRepo[]>;
  /** The starting phase, for tests. */
  initial?: PushPhase;
}) {
  const id = props.orderId;
  const [phase, setPhase] = useState<PushPhase>(props.initial ?? { kind: "idle" });
  const [error, setError] = useState("");
  const [target, setTarget] = useState(orderBranch(id));
  const askRef = useRef<HTMLButtonElement>(null);
  const confirmRef = useRef<HTMLButtonElement>(null);
  const forceAskRef = useRef<HTMLButtonElement>(null);
  const resultRef = useRef<HTMLDivElement>(null);
  const refocus = useRef(false);
  useEffect(() => {
    if (phase.kind === "confirm" || phase.kind === "force") confirmRef.current?.focus();
    if (phase.kind === "done") resultRef.current?.focus();
    if (phase.kind === "rejected" && refocus.current) {
      refocus.current = false;
      forceAskRef.current?.focus();
    }
    if (phase.kind === "idle" && refocus.current) {
      refocus.current = false;
      askRef.current?.focus();
    }
  }, [phase]);
  useEffect(
    () =>
      clearPushResult(phase, () => {
        if (resultRef.current?.contains(document.activeElement)) refocus.current = true;
        setPhase({ kind: "idle" });
      }),
    [phase],
  );
  const back = () => {
    refocus.current = true;
    setPhase(phase.kind === "force" ? { kind: "rejected", message: phase.message } : { kind: "idle" });
  };
  const branch = target.trim() || orderBranch(id);
  const go = async (force: boolean) => {
    setPhase({ kind: "pending", force });
    setError("");
    try {
      const args = branch === orderBranch(id) ? {} : { target_branch: branch };
      const repos = await props.run(force ? { ...args, force: true } : args);
      setPhase({ kind: "done", repos });
    } catch (err) {
      const next = pushFailed(err);
      refocus.current = true;
      setError(next.error);
      setPhase(next.phase);
    }
  };
  const promptId = `push-${id}-q`;
  const fieldId = `push-${id}-target`;
  const asking = phase.kind === "confirm" || (phase.kind === "pending" && !phase.force);
  const forcing = phase.kind === "force" || (phase.kind === "pending" && phase.force);
  const onKey = (e: KeyboardEvent) => {
    if (e.key === "Escape" && (phase.kind === "confirm" || phase.kind === "force")) {
      e.preventDefault();
      back();
    }
  };
  return (
    <>
      {!asking && !forcing && (
        <button
          ref={askRef}
          type="button"
          class="btn gold small"
          aria-label={`Push ${id}'s work to its remote`}
          onClick={() => {
            setError("");
            setPhase({ kind: "confirm" });
          }}
        >
          Push
        </button>
      )}
      {asking && (
        <span class="confirm" role="group" aria-labelledby={promptId} aria-busy={phase.kind === "pending"} onKeyDown={onKey}>
          <label class="inline-lbl" for={fieldId}>
            Target branch
          </label>
          <input
            id={fieldId}
            class="field push-target"
            value={target}
            disabled={phase.kind === "pending"}
            onInput={(e) => setTarget((e.currentTarget as HTMLInputElement).value)}
          />
          <span id={promptId} class="confirm-q">
            {pushPrompt(id, props.remotes, branch)}
          </span>
          <button ref={confirmRef} type="button" class="btn red" disabled={phase.kind === "pending"} onClick={() => void go(false)}>
            {phase.kind === "pending" ? "Pushing" : "Push"}
          </button>
          <button type="button" class="btn ghost" disabled={phase.kind === "pending"} onClick={back}>
            Cancel
          </button>
        </span>
      )}
      {(phase.kind === "rejected" || phase.kind === "force") && (
        <p class="error act-msg" role="alert">
          {pushRejectedText(branch, phase.message)}
        </p>
      )}
      {phase.kind === "rejected" && (
        <button ref={forceAskRef} type="button" class="btn ghost small" aria-label={`Force push ${id} to ${branch}`} onClick={() => setPhase({ kind: "force", message: phase.message })}>
          Force push
        </button>
      )}
      {forcing && (
        <span class="confirm" role="group" aria-labelledby={`${promptId}-force`} aria-busy={phase.kind === "pending"} onKeyDown={onKey}>
          <span id={`${promptId}-force`} class="confirm-q">
            {forcePushPrompt(id, props.remotes, branch)}
          </span>
          <button ref={confirmRef} type="button" class="btn red" disabled={phase.kind === "pending"} onClick={() => void go(true)}>
            {phase.kind === "pending" ? "Pushing" : "Force push"}
          </button>
          <button type="button" class="btn ghost" disabled={phase.kind === "pending"} onClick={back}>
            Cancel
          </button>
        </span>
      )}
      {phase.kind === "done" && (
        <div ref={resultRef} class="notice act-msg push-result" role="status" tabIndex={-1}>
          Pushed.
          <ul>
            {phase.repos.map((r) => {
              const pr = githubCompareURL(r.url, r.branch);
              return (
                <li key={r.name}>
                  <span class="mono">
                    {r.name}: {r.branch} at {r.commit.slice(0, 12)}
                  </span>
                  {r.url && <span class="mono muted-id push-remote">{r.url}</span>}
                  {pr && (
                    <>
                      {" "}
                      <a href={pr} target="_blank" rel="noopener noreferrer">
                        Open a pull request
                      </a>
                    </>
                  )}
                </li>
              );
            })}
          </ul>
        </div>
      )}
      {error && (
        <p class="error act-msg" role="alert">
          {error}
        </p>
      )}
    </>
  );
}

/* Transporter room ------------------------------------------------------ */

export function TransporterPanel(props: { state: State; csrf: string }) {
  const crew = manifest(props.state);
  return (
    <section class="panel area-transporter" aria-labelledby="tr-h">
      <Heading id="tr-h" tone="red">
        Transporter room
      </Heading>
      <div class="pads">
        {crew.map((c) => {
          const p = padInfo(c);
          return (
            <div class={`pad pad-${p.mode}`} key={c.id}>
              <div class="beamcol" aria-hidden="true" />
              <div class="disc" aria-hidden="true" />
              <div class="pad-text">
                <div class="pad-name">{shortName(c.id)}</div>
                <div class={`mono pad-lbl tone-${p.tone}`}>{p.label}</div>
              </div>
            </div>
          );
        })}
        {crew.length === 0 && <p class="empty">No pads in use.</p>}
      </div>
      <p class="hint">One Teleport Beam per crew member. A lit pad is a live beam.</p>
      <StandingOrdersSwitch status={props.state.status} csrf={props.csrf} />
    </section>
  );
}

/**
 * The standing orders switch (#78): its state is the status's
 * standing_orders, and a press PUTs the other value; the status event then
 * flips it. Disabled until the first status; aria-disabled while a press
 * is out, so it keeps focus. The visible "on"/"off" is hidden from screen
 * readers, which read aria-checked.
 */
export function StandingOrdersSwitch(props: { status: BridgeStatus | undefined; csrf: string }) {
  const info = standingOrdersInfo(props.status);
  const [pending, setPending] = useState(false);
  const [error, setError] = useState("");
  const toggle = async () => {
    if (info.on === undefined || pending) return;
    setPending(true);
    setError("");
    try {
      await setStandingOrders(props.csrf, !info.on);
    } catch (err) {
      setError(errorText(err));
    } finally {
      setPending(false);
    }
  };
  return (
    <>
      <button
        type="button"
        class="toggle"
        role="switch"
        aria-checked={info.on === true}
        aria-busy={pending || undefined}
        aria-disabled={pending || undefined}
        disabled={info.on === undefined}
        onClick={() => void toggle()}
      >
        <span class={info.on ? "knob on" : "knob"} aria-hidden="true" />
        Standing orders
        {info.label && <span aria-hidden="true"> {info.label}</span>}
      </button>
      <p class="hint">{info.hint}</p>
      {error && (
        <p class="error" role="alert">
          {error}
        </p>
      )}
    </>
  );
}

/* Captain's log --------------------------------------------------------- */

export function LogPanel(props: { state: State; dispatch: (a: Action) => void }) {
  const { state } = props;
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState("");
  const entries = [...state.log].reverse();
  const first = state.log[0]?.id;

  const older = async () => {
    if (loading || first === undefined) return;
    setLoading(true);
    setError("");
    try {
      const page = await logBefore(first, olderPage);
      props.dispatch({ type: "older", before: first, entries: page.entries, complete: page.entries.length < olderPage });
    } catch (err) {
      setError(errorText(err));
    } finally {
      setLoading(false);
    }
  };

  return (
    <section class="panel area-log" aria-labelledby="log-h">
      <Heading id="log-h" tone="blue">
        Captain's log
      </Heading>
      <ol class="log" tabIndex={0} aria-label="Captain's log, newest first">
        {entries.map((e) => {
          const t = logTag(e.kind);
          const who = [e.crew_id ? shortName(e.crew_id) : "", e.order_id].filter(Boolean).join(" ");
          const pct = progressPct(e);
          return (
            <li key={e.id}>
              <span class="log-t">{logTime(e.ts)}</span>
              <span class={`log-tag tone-${t.tone}`}>{t.tag}</span>
              <span class="log-text">
                {who && <span class="log-who">{who} </span>}
                {e.text}
                {pct !== undefined && <span class="log-who"> ({Math.round(pct)}%)</span>}
              </span>
            </li>
          );
        })}
        {entries.length === 0 && <li class="empty">The log is empty.</li>}
      </ol>
      {!state.logStart && first !== undefined && (
        <button type="button" class="btn ghost more" disabled={loading} onClick={() => void older()}>
          {loading ? "Loading" : "Load older entries"}
        </button>
      )}
      {error && (
        <p class="error" role="alert">
          {error}
        </p>
      )}
    </section>
  );
}
