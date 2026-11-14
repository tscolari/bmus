-- Domain tables for the bridge (spec/initial-plan-2026-10-16.md §7).
-- Workflow state lives in DBOS's own tables in the same file; these hold the
-- domain records. Timestamps are written by the application as UTC time.Time.

CREATE TABLE IF NOT EXISTS crew (
    id           TEXT PRIMARY KEY,
    name         TEXT NOT NULL,
    rank         TEXT NOT NULL DEFAULT '',
    division     TEXT NOT NULL DEFAULT '',
    spec         TEXT NOT NULL DEFAULT '',
    status       TEXT NOT NULL DEFAULT 'on_deck',
    current_beam TEXT NOT NULL DEFAULT '',
    tour_no      INTEGER NOT NULL DEFAULT 0,
    dossier_path TEXT NOT NULL DEFAULT '',
    harness      TEXT NOT NULL DEFAULT '', -- empty: unset, serve's default (sdk)
    model        TEXT NOT NULL DEFAULT '' -- empty: the harness's default (#58)
);

CREATE TABLE IF NOT EXISTS beams (
    uuid            TEXT PRIMARY KEY,
    alias           TEXT NOT NULL,
    crew_id         TEXT NOT NULL REFERENCES crew(id),
    tour_no         INTEGER NOT NULL DEFAULT 0, -- the crew member's tour that created the beam (#97)
    region          TEXT NOT NULL DEFAULT '',
    created_at      TIMESTAMP NOT NULL,
    expires_at      TIMESTAMP NOT NULL,
    egress_mode     TEXT NOT NULL DEFAULT '',
    allowed_domains TEXT NOT NULL DEFAULT '[]',
    compute_status  TEXT NOT NULL DEFAULT '',
    -- Set once the beam is removed or found gone; NULL while it may still
    -- exist (#111). A removal that failed leaves it NULL for reconcile.
    removed_at      TIMESTAMP,
    labels          TEXT NOT NULL DEFAULT '{}' -- the provider labels, a JSON object (#111)
);

CREATE TABLE IF NOT EXISTS orders (
    id            TEXT PRIMARY KEY,
    title         TEXT NOT NULL,
    body          TEXT NOT NULL DEFAULT '',
    division      TEXT NOT NULL DEFAULT '',
    priority      INTEGER NOT NULL DEFAULT 0,
    state         TEXT NOT NULL,
    assignee      TEXT NOT NULL DEFAULT '',
    parent_id     TEXT NOT NULL DEFAULT '',
    base_ref      TEXT NOT NULL DEFAULT '', -- unused since order_repos (#51); kept for older databases
    branch        TEXT NOT NULL DEFAULT '',
    budget_tokens INTEGER NOT NULL DEFAULT 0,
    created_by    TEXT NOT NULL DEFAULT '',
    model         TEXT NOT NULL DEFAULT '', -- empty: the assignee's model (#58)
    parked_at     TIMESTAMP, -- set while the order's mission is parked (#100)
    -- The latest progress report of the order's running mission (#141):
    -- NULL pct for none. tour and seq order the reports.
    progress_pct  REAL,
    progress_note TEXT NOT NULL DEFAULT '',
    progress_tour INTEGER NOT NULL DEFAULT 0,
    progress_seq  INTEGER NOT NULL DEFAULT 0,
    -- Where the order came from, free-form: tracker:<ref> for a tracker
    -- item whose text the bridge put in the body (#54). Empty for none.
    source        TEXT NOT NULL DEFAULT '',
    -- 0 for unlimited, as budget_tokens. budget_extended counts the
    -- extensions the captain granted (#111).
    budget_turns    INTEGER NOT NULL DEFAULT 0,
    budget_extended INTEGER NOT NULL DEFAULT 0
);

-- The repos an order works on (#51), in the order's list order (pos from
-- 0). A table rather than a column on orders, so a database made before it
-- gains it through CREATE TABLE IF NOT EXISTS.
CREATE TABLE IF NOT EXISTS order_repos (
    order_id TEXT NOT NULL REFERENCES orders(id),
    pos      INTEGER NOT NULL,
    name     TEXT NOT NULL,
    base_ref TEXT NOT NULL,
    PRIMARY KEY (order_id, pos),
    UNIQUE (order_id, name)
);

-- The orders an order waits on (#142): it is claimed only once each has
-- ended done. A table, so a database made before it gains it through
-- CREATE TABLE IF NOT EXISTS.
CREATE TABLE IF NOT EXISTS order_after (
    order_id TEXT NOT NULL REFERENCES orders(id),
    after_id TEXT NOT NULL REFERENCES orders(id),
    PRIMARY KEY (order_id, after_id)
);

-- An order's usage as each beam that ran it reported it (#111): the
-- beam's running totals for the order, the latest seq's. The order's usage
-- is the sum over its rows.
CREATE TABLE IF NOT EXISTS order_usage (
    order_id      TEXT NOT NULL REFERENCES orders(id),
    beam          TEXT NOT NULL,
    input_tokens  INTEGER NOT NULL DEFAULT 0,
    output_tokens INTEGER NOT NULL DEFAULT 0,
    turns         INTEGER NOT NULL DEFAULT 0,
    seq           INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (order_id, beam)
);

-- A hail from a beam has an order and a crew member and no deliver_to. A
-- hail a bridge workflow raises (#151) has neither: deliver_to is that
-- workflow's id. Older databases are refused (requiredColumns).
CREATE TABLE IF NOT EXISTS hails (
    id                TEXT PRIMARY KEY,
    order_id          TEXT REFERENCES orders(id),
    crew_id           TEXT REFERENCES crew(id),
    deliver_to        TEXT,
    category          TEXT NOT NULL DEFAULT '',
    question          TEXT NOT NULL,
    options           TEXT NOT NULL DEFAULT '[]',
    context           TEXT NOT NULL DEFAULT '{}',
    route             TEXT NOT NULL DEFAULT 'captain',
    status            TEXT NOT NULL DEFAULT 'open',
    decided_by        TEXT NOT NULL DEFAULT '',
    decision          TEXT NOT NULL DEFAULT '',
    rationale         TEXT NOT NULL DEFAULT '',
    confidence        REAL,
    countermand_until TIMESTAMP,
    -- #147: created_at is NULL on hails from before it. decided_at and
    -- original_decided_by are set when the hail is first decided (closed);
    -- a countermand (#153) leaves them alone and sets countermanded_at.
    created_at          TIMESTAMP,
    decided_at          TIMESTAMP,
    countermanded_at    TIMESTAMP,
    original_decided_by TEXT NOT NULL DEFAULT '',
    -- The option the captain's last countermand picked, NULL for a
    -- free-text one or none (#54).
    countermand_option  INTEGER
);

-- The budget extensions the captain granted (#119), one per budget hail
-- answered extend: orders.budget_extended counts them, and the hail id
-- keys each, so a re-run of the step that grants one adds nothing.
CREATE TABLE IF NOT EXISTS budget_extensions (
    hail_id    TEXT PRIMARY KEY REFERENCES hails(id),
    order_id   TEXT NOT NULL REFERENCES orders(id),
    granted_at TIMESTAMP NOT NULL
);

-- Domains an egress hail allowed for an order (#111). Every beam that runs
-- the order gets them.
CREATE TABLE IF NOT EXISTS order_egress (
    order_id TEXT NOT NULL REFERENCES orders(id),
    domain   TEXT NOT NULL,
    hail_id  TEXT NOT NULL REFERENCES hails(id),
    PRIMARY KEY (order_id, domain)
);

-- A checkpoint's seq is the beam's message seq, so a resumed order's next
-- beam numbers from 1 again: the beam is part of the key (#100).
CREATE TABLE IF NOT EXISTS checkpoints (
    order_id        TEXT NOT NULL REFERENCES orders(id),
    beam            TEXT NOT NULL,
    seq             INTEGER NOT NULL,
    "commit"        TEXT NOT NULL DEFAULT '', -- unused since checkpoint_repos (#53); kept for older databases
    bundle_path     TEXT NOT NULL DEFAULT '', -- unused since checkpoint_repos (#53); kept for older databases
    transcript_path TEXT NOT NULL DEFAULT '',
    handover_path   TEXT NOT NULL DEFAULT '',
    created_at      TIMESTAMP NOT NULL,
    PRIMARY KEY (order_id, beam, seq)
);

-- A checkpoint's bundles, one per repo of the order (#53), in the
-- checkpoint_ready's list order (pos from 0). A table for the same reason as
-- order_repos.
CREATE TABLE IF NOT EXISTS checkpoint_repos (
    order_id    TEXT NOT NULL,
    beam        TEXT NOT NULL,
    seq         INTEGER NOT NULL,
    pos         INTEGER NOT NULL,
    name        TEXT NOT NULL,
    "commit"    TEXT NOT NULL,
    bundle_path TEXT NOT NULL,
    PRIMARY KEY (order_id, beam, seq, pos),
    FOREIGN KEY (order_id, beam, seq) REFERENCES checkpoints(order_id, beam, seq)
);

-- Append-only: the code never issues UPDATE or DELETE against this table.
CREATE TABLE IF NOT EXISTS log (
    id       INTEGER PRIMARY KEY AUTOINCREMENT,
    ts       TIMESTAMP NOT NULL,
    kind     TEXT NOT NULL,
    crew_id  TEXT NOT NULL DEFAULT '',
    order_id TEXT NOT NULL DEFAULT '',
    text     TEXT NOT NULL DEFAULT '',
    payload  TEXT NOT NULL DEFAULT '{}'
);

-- Keyed by tour as well as crew member: each tour runs its own beam, whose
-- outbox seq starts again at 1.
CREATE TABLE IF NOT EXISTS inbox (
    crew_id     TEXT NOT NULL,
    tour_no     INTEGER NOT NULL,
    seq         INTEGER NOT NULL,
    received_at TIMESTAMP NOT NULL,
    PRIMARY KEY (crew_id, tour_no, seq)
);

-- Bridge-wide settings the captain changes at run time, e.g.
-- standing_orders ("on" or "off") and command_guidance (#147).
-- Also the database's nonce, made once by Open (#206).
CREATE TABLE IF NOT EXISTS settings (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

-- The backup workflow's runs (#111). finished_at is NULL while one runs
-- or after a crash in it; error is empty for a backup that passed its check.
CREATE TABLE IF NOT EXISTS backups (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    started_at  TIMESTAMP NOT NULL,
    finished_at TIMESTAMP,
    dir         TEXT NOT NULL,
    bytes       INTEGER NOT NULL DEFAULT 0,
    error       TEXT NOT NULL DEFAULT ''
);

-- Findings a crew member records while it works on an order (#151). The
-- bridge files them as tracker issues once the order's mission ends.
CREATE TABLE IF NOT EXISTS findings (
    order_id     TEXT NOT NULL REFERENCES orders(id),
    key          TEXT NOT NULL,
    crew_id      TEXT NOT NULL,          -- who filed it
    title        TEXT NOT NULL,
    body         TEXT NOT NULL DEFAULT '',
    labels       TEXT NOT NULL DEFAULT '[]',
    severity     TEXT NOT NULL DEFAULT '',
    blocked_by   TEXT NOT NULL DEFAULT '[]',
    received_at  TIMESTAMP NOT NULL,
    state        TEXT NOT NULL DEFAULT 'new', -- new, dropped, filed, failed, kept, discarded, restored
    why          TEXT NOT NULL DEFAULT '',    -- Command's reason for dropping it, or the filing error
    duplicate_of TEXT NOT NULL DEFAULT '',
    ref          TEXT NOT NULL DEFAULT '',    -- its issue, once created
    PRIMARY KEY (order_id, key)
);

CREATE INDEX IF NOT EXISTS orders_assignee_state ON orders (assignee, state);
-- An order's recent log lines, for Command's consults (#150).
CREATE INDEX IF NOT EXISTS log_order ON log (order_id, id);
-- Decided hails still inside their countermand window, for the console
-- (#153).
CREATE INDEX IF NOT EXISTS hails_countermand_until ON hails (countermand_until);
-- A split order's children (#155): open children counts and settleSplit.
CREATE INDEX IF NOT EXISTS orders_parent ON orders (parent_id, state);
-- An order's dependents (#142): the cascade when it ends failed, belayed
-- or withdrawn.
CREATE INDEX IF NOT EXISTS order_after_after ON order_after (after_id);
-- One hail per bridge raiser and category (#151): a re-run of the raising
-- step finds the first.
CREATE UNIQUE INDEX IF NOT EXISTS hails_deliver_to ON hails (deliver_to, category) WHERE deliver_to IS NOT NULL;
