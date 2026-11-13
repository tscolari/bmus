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
    compute_status  TEXT NOT NULL DEFAULT ''
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
    source        TEXT NOT NULL DEFAULT ''
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

CREATE TABLE IF NOT EXISTS hails (
    id                TEXT PRIMARY KEY,
    order_id          TEXT NOT NULL REFERENCES orders(id),
    crew_id           TEXT NOT NULL REFERENCES crew(id),
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

CREATE INDEX IF NOT EXISTS orders_assignee_state ON orders (assignee, state);
-- An order's recent log lines, for Command's consults (#150).
CREATE INDEX IF NOT EXISTS log_order ON log (order_id, id);
-- Decided hails still inside their countermand window, for the console
-- (#153).
CREATE INDEX IF NOT EXISTS hails_countermand_until ON hails (countermand_until);
-- A split order's children (#155): open children counts and settleSplit.
CREATE INDEX IF NOT EXISTS orders_parent ON orders (parent_id, state);
