-- Migration 003: the Phase 3 manager/workers/messages schema
-- (docs/plan/phase-3-design.md section 4; the design text's "002" — 002
-- landed as the trust-seeding column). Additive means DATA- and
-- BEHAVIOR-preserving for Phase 2 rows, not ALTER-only: relaxing a NOT
-- NULL column in a STRICT table rebuilds the table (create new, copy,
-- drop, rename). The migrator runs this file on a DEDICATED connection
-- with foreign_keys OFF set before BEGIN, validates the launch-claim
-- backfill in Go before executing it, and runs PRAGMA foreign_key_check
-- before commit; 001 and 002 are untouched.

-- ---------------------------------------------------------------------
-- New tables (created first: the rebuilt check_requests references
-- integrations, and retry_requests references the rebuilt sessions by
-- its final name).
-- ---------------------------------------------------------------------

CREATE TABLE task_dependencies (
    task_id         TEXT NOT NULL REFERENCES tasks (id),
    prerequisite_id TEXT NOT NULL REFERENCES tasks (id),
    created_at      TEXT NOT NULL,
    UNIQUE (task_id, prerequisite_id)
) STRICT;

-- Immutable message envelopes. recipient_address is the canonical
-- rendered form ("manager", "human", "task:<uuid>"); enqueue_seq is the
-- durable per-(run, recipient) FIFO authority assigned inside the send
-- transaction. request_id here is informational only — the request key's
-- authority is message_receipts, so two runs or two verbs may carry the
-- same ID.
CREATE TABLE messages (
    id                TEXT PRIMARY KEY,
    run_id            TEXT NOT NULL REFERENCES runs (id),
    sender_kind       TEXT NOT NULL,
    sender_session_id TEXT REFERENCES sessions (id),
    recipient_address TEXT NOT NULL,
    kind              TEXT NOT NULL,
    reply_to          TEXT REFERENCES messages (id),
    relayed_from      TEXT REFERENCES messages (id),
    enqueue_seq       INTEGER NOT NULL,
    request_id        TEXT,
    body_path         TEXT NOT NULL,
    body_digest       TEXT NOT NULL,
    body_bytes        INTEGER NOT NULL,
    created_at        TEXT NOT NULL,
    UNIQUE (run_id, recipient_address, enqueue_seq)
) STRICT;

-- One accepted answer per question.
CREATE UNIQUE INDEX messages_one_answer_per_question ON messages (reply_to)
    WHERE kind = 'answer';

-- Append-only delivery history: every fetch that served the message,
-- re-serves included.
CREATE TABLE message_deliveries (
    id             TEXT PRIMARY KEY,
    message_id     TEXT NOT NULL REFERENCES messages (id),
    session_id     TEXT NOT NULL REFERENCES sessions (id),
    incarnation_id TEXT NOT NULL,
    delivered_at   TEXT NOT NULL
) STRICT;

-- At most one acknowledgement; insertion is the acknowledgement.
-- session_id and incarnation_id are NULL for a human ack bundled into an
-- answer's acceptance (hop answer).
CREATE TABLE message_acks (
    message_id     TEXT PRIMARY KEY REFERENCES messages (id),
    session_id     TEXT REFERENCES sessions (id),
    incarnation_id TEXT,
    acked_at       TEXT NOT NULL
) STRICT;

-- The messaging analogue of result_submissions: claimed identities are
-- plain text with no foreign keys, so a refused or malformed request
-- still leaves evidence. Every outcome leaves a receipt EXCEPT an empty
-- fetch, which is deliberately receipt-free (a 1s poll loop must not
-- grow the store); a successful serve's evidence is its delivery row.
CREATE TABLE message_receipts (
    id                     TEXT PRIMARY KEY,
    run_id                 TEXT NOT NULL,
    op                     TEXT NOT NULL,
    claimed_session_id     TEXT,
    claimed_incarnation_id TEXT,
    claimed_message_id     TEXT,
    claimed_recipient      TEXT,
    request_id             TEXT,
    request_digest         TEXT,
    outcome                TEXT NOT NULL,
    created_entity_id      TEXT,
    detail                 TEXT NOT NULL,
    at                     TEXT NOT NULL
) STRICT;

-- ONE authoritative acceptance per (run, verb, request ID); refused and
-- malformed observations are ordinary additional rows outside the key.
CREATE UNIQUE INDEX message_receipts_accepted_request
    ON message_receipts (run_id, op, request_id)
    WHERE outcome = 'accepted' AND request_id IS NOT NULL;

-- Accepted review verdicts, immutable once accepted.
CREATE TABLE reviews (
    id                 TEXT PRIMARY KEY,
    run_id             TEXT NOT NULL REFERENCES runs (id),
    task_id            TEXT NOT NULL REFERENCES tasks (id),
    attempt_id         TEXT NOT NULL REFERENCES attempts (id),
    subject_commit_oid TEXT NOT NULL,
    subject_tree_oid   TEXT NOT NULL,
    verdict            TEXT NOT NULL,
    reasons_path       TEXT NOT NULL,
    reasons_digest     TEXT NOT NULL,
    submitted_at       TEXT NOT NULL,
    UNIQUE (attempt_id, reasons_digest)
) STRICT;

-- One accepted verdict per attempt; an idempotent duplicate returns the
-- accepted row instead of inserting.
CREATE UNIQUE INDEX reviews_one_verdict_per_attempt ON reviews (attempt_id);

-- Review submission receipts, as for results: claimed ids plain text.
CREATE TABLE review_submissions (
    id                         TEXT PRIMARY KEY,
    claimed_run_id             TEXT NOT NULL,
    claimed_task_id            TEXT NOT NULL,
    claimed_attempt_id         TEXT NOT NULL,
    claimed_incarnation_id     TEXT NOT NULL,
    claimed_subject_commit_oid TEXT,
    claimed_verdict            TEXT,
    reasons_digest             TEXT,
    review_id                  TEXT,
    outcome                    TEXT NOT NULL,
    detail                     TEXT NOT NULL,
    submitted_at               TEXT NOT NULL
) STRICT;

-- Serial integration attempts. operation_id names the journal operation
-- currently driving the row, evidence only.
CREATE TABLE integrations (
    id                TEXT PRIMARY KEY,
    run_id            TEXT NOT NULL REFERENCES runs (id),
    task_id           TEXT NOT NULL REFERENCES tasks (id),
    result_id         TEXT NOT NULL REFERENCES results (id),
    source_commit_oid TEXT NOT NULL,
    premerge_head_oid TEXT NOT NULL,
    merge_commit_oid  TEXT,
    state             TEXT NOT NULL,
    operation_id      TEXT REFERENCES operations (id),
    revision          INTEGER NOT NULL,
    created_at        TEXT NOT NULL,
    updated_at        TEXT NOT NULL,
    UNIQUE (task_id, result_id)
) STRICT;

-- Serial integration is store-enforced, not just scheduled: at most one
-- non-terminal integration per run (merging, checking, check-failed;
-- integrated, conflicted, rolled-back and interrupted are terminal).
CREATE UNIQUE INDEX integrations_one_current_per_run ON integrations (run_id)
    WHERE state IN ('merging', 'checking', 'check-failed');

-- Manager retry bookkeeping: PlanStore.RequestRetry writes the pending
-- row (the attempt itself is reserved in the same transaction); the
-- controller's assignment pass marks it consumed.
CREATE TABLE retry_requests (
    id                   TEXT PRIMARY KEY,
    task_id              TEXT NOT NULL REFERENCES tasks (id),
    requested_by_session TEXT NOT NULL REFERENCES sessions (id),
    request_id           TEXT,
    reason               TEXT NOT NULL,
    state                TEXT NOT NULL,
    created_at           TEXT NOT NULL
) STRICT;

CREATE UNIQUE INDEX retry_requests_one_pending_per_task ON retry_requests (task_id)
    WHERE state = 'pending';

-- Manager plan-verb receipts (task-create, task-retry, plan-close):
-- append-only, one authoritative acceptance per (run, verb, request ID).
CREATE TABLE workflow_receipts (
    id                     TEXT PRIMARY KEY,
    run_id                 TEXT NOT NULL,
    op                     TEXT NOT NULL,
    request_id             TEXT,
    request_digest         TEXT,
    claimed_session_id     TEXT,
    claimed_incarnation_id TEXT,
    claimed_task_id        TEXT,
    outcome                TEXT NOT NULL,
    created_entity_id      TEXT,
    created_entity_seq     INTEGER,
    detail                 TEXT NOT NULL,
    at                     TEXT NOT NULL
) STRICT;

CREATE UNIQUE INDEX workflow_receipts_accepted_request
    ON workflow_receipts (run_id, op, request_id)
    WHERE outcome = 'accepted' AND request_id IS NOT NULL;

-- ---------------------------------------------------------------------
-- Additive columns on landed tables (defaults ARE the solo backfill).
-- ---------------------------------------------------------------------

-- The durable plan flag: set by ClosePlan, cleared by an accepted
-- CreateTask; NULL means solo (no plan) or plan open.
ALTER TABLE runs ADD COLUMN plan_closed_at TEXT;

-- The frozen feature-mode policy JSON; NULL means solo (Phase 2 rows).
ALTER TABLE run_snapshots ADD COLUMN workflow TEXT;

-- Per-attempt worktrees; Phase 2 rows keep NULL for both (their base
-- remains readable from the worktree.create operation intent).
ALTER TABLE worktrees ADD COLUMN attempt_id TEXT REFERENCES attempts (id);
ALTER TABLE worktrees ADD COLUMN base_commit TEXT;

-- Task extensions. Existing solo rows mean: kind implement, seq 1, no
-- title (the solo assignment lives in the run snapshot), no instructions
-- file, no retries, no frozen subject, mailbox never closed.
ALTER TABLE tasks ADD COLUMN kind TEXT NOT NULL DEFAULT 'implement';
ALTER TABLE tasks ADD COLUMN seq INTEGER NOT NULL DEFAULT 1;
ALTER TABLE tasks ADD COLUMN title TEXT NOT NULL DEFAULT '';
ALTER TABLE tasks ADD COLUMN instructions_path TEXT NOT NULL DEFAULT '';
ALTER TABLE tasks ADD COLUMN retry_count INTEGER NOT NULL DEFAULT 0;
ALTER TABLE tasks ADD COLUMN subject_commit_oid TEXT;
ALTER TABLE tasks ADD COLUMN subject_tree_oid TEXT;
ALTER TABLE tasks ADD COLUMN mailbox_closed_at TEXT;
ALTER TABLE tasks ADD COLUMN created_at TEXT NOT NULL DEFAULT '';
UPDATE tasks SET created_at = updated_at;

CREATE UNIQUE INDEX tasks_run_seq_unique ON tasks (run_id, seq);

-- ---------------------------------------------------------------------
-- Rebuild: sessions — attempt_id relaxed to NULL (a manager session has
-- none), parent_session_id added (one-level delegation is validated in
-- the application transaction that creates a child; SQLite cannot
-- express the cross-row rule declaratively). Rows are copied verbatim
-- with parent_session_id NULL (solo workers have no manager).
-- ---------------------------------------------------------------------

CREATE TABLE sessions_new (
    id                 TEXT PRIMARY KEY,
    run_id             TEXT NOT NULL REFERENCES runs (id),
    attempt_id         TEXT REFERENCES attempts (id),
    parent_session_id  TEXT REFERENCES sessions (id),
    role               TEXT NOT NULL,
    harness            TEXT NOT NULL,
    native_session_ref TEXT,
    native_ref_source  TEXT,
    state              TEXT NOT NULL,
    revision           INTEGER NOT NULL,
    updated_at         TEXT NOT NULL
) STRICT;

INSERT INTO sessions_new (id, run_id, attempt_id, parent_session_id, role, harness, native_session_ref, native_ref_source, state, revision, updated_at)
SELECT id, run_id, attempt_id, NULL, role, harness, native_session_ref, native_ref_source, state, revision, updated_at FROM sessions;

DROP TABLE sessions;
ALTER TABLE sessions_new RENAME TO sessions;

-- Preserved from 001: at most one non-terminated session per attempt
-- (NULL attempt ids are distinct in SQLite unique indexes, so
-- attempt-less sessions never collide here).
CREATE UNIQUE INDEX sessions_one_current_per_attempt ON sessions (attempt_id)
    WHERE state NOT IN ('lost', 'terminated');

-- New: one current (non-terminal) manager session per run.
CREATE UNIQUE INDEX sessions_one_manager_per_run ON sessions (run_id)
    WHERE role = 'manager' AND state NOT IN ('lost', 'terminated');

-- ---------------------------------------------------------------------
-- Rebuild: launch_claims — session_id NOT NULL added, attempt_id relaxed
-- to NULL (legal only for a claim whose session has no attempt,
-- application-validated inside ClaimLaunch's transaction). session_id
-- backfills from runtime_bindings by incarnation_id; a claim with NO
-- binding row backfills from the HISTORICAL launch intent — the
-- pane.open/launch.send operation whose intent JSON carries this claim's
-- incarnation_id, whose session_id field is authoritative. The migrator
-- validates in Go, BEFORE this SQL executes, that every claim resolves
-- unambiguously (exactly one binding session, or no binding and exactly
-- one intent) and fails the migration naming the claim otherwise, so the
-- COALESCE below never guesses.
-- ---------------------------------------------------------------------

CREATE TABLE launch_claims_new (
    incarnation_id      TEXT PRIMARY KEY,
    run_id              TEXT NOT NULL REFERENCES runs (id),
    session_id          TEXT NOT NULL REFERENCES sessions (id),
    attempt_id          TEXT REFERENCES attempts (id),
    executable          TEXT NOT NULL,
    argv_digest         TEXT NOT NULL,
    pid                 INTEGER NOT NULL,
    state               TEXT NOT NULL,
    error               TEXT,
    claimed_at          TEXT NOT NULL,
    settled_at          TEXT,
    settlement_evidence TEXT,
    seed_evidence       TEXT
) STRICT;

INSERT INTO launch_claims_new (incarnation_id, run_id, session_id, attempt_id, executable, argv_digest, pid, state, error, claimed_at, settled_at, settlement_evidence, seed_evidence)
SELECT
    lc.incarnation_id,
    lc.run_id,
    COALESCE(
        (SELECT rb.session_id FROM runtime_bindings rb WHERE rb.incarnation_id = lc.incarnation_id LIMIT 1),
        (SELECT json_extract(o.intent, '$.session_id') FROM operations o
          WHERE o.kind IN ('pane.open', 'launch.send')
            AND json_extract(o.intent, '$.incarnation_id') = lc.incarnation_id
          LIMIT 1)
    ),
    lc.attempt_id,
    lc.executable, lc.argv_digest, lc.pid, lc.state, lc.error,
    lc.claimed_at, lc.settled_at, lc.settlement_evidence, lc.seed_evidence
FROM launch_claims lc;

DROP TABLE launch_claims;
ALTER TABLE launch_claims_new RENAME TO launch_claims;

-- ---------------------------------------------------------------------
-- Rebuild: check_requests — the typed check subject (section 8). The
-- Phase 2 rows are result subjects re-keyed with id = result_id (already
-- a UUID, deterministic).
-- ---------------------------------------------------------------------

CREATE TABLE check_requests_new (
    id                 TEXT PRIMARY KEY,
    subject_kind       TEXT NOT NULL,
    result_id          TEXT REFERENCES results (id),
    attempt_id         TEXT REFERENCES attempts (id),
    integration_id     TEXT REFERENCES integrations (id),
    state              TEXT NOT NULL,
    created_at         TEXT NOT NULL,
    claimed_generation INTEGER,
    CHECK ((subject_kind = 'result' AND result_id IS NOT NULL AND integration_id IS NULL)
        OR (subject_kind = 'integration' AND integration_id IS NOT NULL AND result_id IS NULL))
) STRICT;

INSERT INTO check_requests_new (id, subject_kind, result_id, attempt_id, integration_id, state, created_at, claimed_generation)
SELECT result_id, 'result', result_id, attempt_id, NULL, state, created_at, claimed_generation FROM check_requests;

DROP TABLE check_requests;
ALTER TABLE check_requests_new RENAME TO check_requests;

CREATE UNIQUE INDEX check_requests_one_per_result ON check_requests (result_id)
    WHERE result_id IS NOT NULL;
CREATE UNIQUE INDEX check_requests_one_per_integration ON check_requests (integration_id)
    WHERE integration_id IS NOT NULL;
