-- Owner: Run engine. The snapshots plan reads, the plan versions, their
-- confirmations, the runs of apply, the write journal and the mapping from
-- PagerDuty objects to JSM objects.
--
-- IDs, states, counts, hashes and times are kept in clear, so pause and the
-- menu can tell where a run stands without the data key; a request ID an API
-- answered with is cleaned first, as the run record has it. Everything else a
-- step, an API or the operator wrote (payloads, messages, reasons and
-- plan.json) is sealed with the store's field encryption, its texts cleaned.

-- One read of one side. finished_at is set once every step has read its
-- part: a read without it was cut short and is never used. problems holds
-- the messages of the steps whose read failed, sealed, so a later plan that
-- reuses the read with --only still carries its blockers.
CREATE TABLE snapshot_reads (
    id          INTEGER PRIMARY KEY,
    side        TEXT NOT NULL CHECK (side IN ('pagerduty', 'jsm')),
    started_at  TEXT NOT NULL,
    finished_at TEXT,
    complete    INTEGER NOT NULL DEFAULT 0,
    problems    BLOB
);

CREATE TABLE snapshot_objects (
    read_id   INTEGER NOT NULL REFERENCES snapshot_reads(id),
    step      TEXT NOT NULL,
    kind      TEXT NOT NULL,
    object_id TEXT NOT NULL,
    payload   BLOB NOT NULL,
    PRIMARY KEY (read_id, step, kind, object_id)
);

-- hash is the hash of the version's plan.json, which apply checks the file
-- against; content_hash names what the plan does, so plan can tell that
-- nothing changed. plan is a sealed copy of plan.json.
CREATE TABLE plan_versions (
    version        INTEGER PRIMARY KEY,
    hash           TEXT NOT NULL,
    content_hash   TEXT NOT NULL,
    created_at     TEXT NOT NULL,
    pagerduty_read INTEGER REFERENCES snapshot_reads(id),
    jsm_read       INTEGER REFERENCES snapshot_reads(id),
    calls          INTEGER NOT NULL,
    blockers       INTEGER NOT NULL,
    warnings       INTEGER NOT NULL,
    dir            TEXT NOT NULL,
    plan           BLOB NOT NULL
);

-- One confirmation per version. record is the confirmation with the reasons
-- given for each warning, sealed.
CREATE TABLE confirmations (
    plan_version INTEGER PRIMARY KEY REFERENCES plan_versions(version),
    confirmed_at TEXT NOT NULL,
    account_id   TEXT NOT NULL,
    via          TEXT NOT NULL,
    record       BLOB NOT NULL
);

-- written counts the items that have a final state, and failed those of
-- them that are not done. record is the run's run.md, relative to
-- artifacts/. jsm_read is the JSM read the run's lookups compare with. A run
-- with a call whose outcome is unknown is never completed: resume settles it
-- first.
CREATE TABLE runs (
    id           TEXT PRIMARY KEY,
    plan_version INTEGER NOT NULL REFERENCES confirmations(plan_version),
    state        TEXT NOT NULL CHECK (state IN ('running', 'paused', 'interrupted', 'completed')),
    started_at   TEXT NOT NULL,
    ended_at     TEXT,
    total        INTEGER NOT NULL,
    written      INTEGER NOT NULL DEFAULT 0,
    failed       INTEGER NOT NULL DEFAULT 0,
    record       TEXT NOT NULL,
    jsm_read     INTEGER NOT NULL REFERENCES snapshot_reads(id)
);

-- One row per item taken, and for an item that makes a call, one per
-- attempt. The row of a call is committed as 'sending' before the call is
-- made, so after a crash the row says the call may have reached JSM.
-- attempt is 0 for an item that makes no call.
CREATE TABLE journal (
    run_id      TEXT NOT NULL REFERENCES runs(id),
    seq         INTEGER NOT NULL,
    item_key    TEXT NOT NULL,
    attempt     INTEGER NOT NULL,
    state       TEXT NOT NULL CHECK (state IN ('sending', 'done', 'failed', 'unknown', 'declined')),
    started_at  TEXT NOT NULL,
    finished_at TEXT,
    status      INTEGER,
    request_id  TEXT,
    jsm_type    TEXT,
    jsm_id      TEXT,
    messages    BLOB,
    PRIMARY KEY (run_id, seq)
);
CREATE INDEX journal_by_item ON journal(run_id, item_key);

-- The JSM object each PagerDuty object was migrated to or linked with. A row
-- is written in the same transaction as the journal row that made it, so the
-- mapping never runs ahead of the journal.
CREATE TABLE mappings (
    pagerduty_type TEXT NOT NULL,
    pagerduty_id   TEXT NOT NULL,
    jsm_type       TEXT NOT NULL,
    jsm_id         TEXT NOT NULL,
    run_id         TEXT NOT NULL REFERENCES runs(id),
    item_key       TEXT NOT NULL,
    mapped_at      TEXT NOT NULL,
    PRIMARY KEY (pagerduty_type, pagerduty_id, jsm_type)
);
