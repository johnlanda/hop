-- Migration 001: the complete Phase 2 schema. All IDs are canonical
-- lowercase UUID TEXT. All times are fixed-width canonical UTC TEXT with
-- nine fractional digits and a trailing Z, so equal-length lexical
-- comparison agrees with time order; code still compares parsed times for
-- every lease and expiry decision. Every mutable entity table carries
-- revision INTEGER for optimistic concurrency. Foreign keys are enforced by
-- the per-connection foreign_keys pragma.

CREATE TABLE repositories (
    id         TEXT PRIMARY KEY,
    root_path  TEXT NOT NULL,
    created_at TEXT NOT NULL,
    UNIQUE (root_path)
) STRICT;

CREATE TABLE runs (
    id                TEXT PRIMARY KEY,
    repository_id     TEXT NOT NULL REFERENCES repositories (id),
    seq               INTEGER NOT NULL,
    brief             TEXT NOT NULL,
    brief_digest      TEXT NOT NULL,
    state             TEXT NOT NULL,
    stop_requested_at TEXT,
    revision          INTEGER NOT NULL,
    created_at        TEXT NOT NULL,
    updated_at        TEXT NOT NULL,
    UNIQUE (repository_id, seq)
) STRICT;

CREATE TABLE run_snapshots (
    run_id            TEXT PRIMARY KEY REFERENCES runs (id),
    check_argv        TEXT NOT NULL,
    check_timeout_ms  INTEGER NOT NULL,
    check_repeatable  INTEGER NOT NULL,
    env_policy        TEXT NOT NULL,
    harness           TEXT NOT NULL,
    profile_dir       TEXT,
    state_root        TEXT NOT NULL,
    assignment_path   TEXT NOT NULL,
    assignment_digest TEXT NOT NULL,
    created_at        TEXT NOT NULL
) STRICT;

CREATE TABLE tasks (
    id                  TEXT PRIMARY KEY,
    run_id              TEXT NOT NULL REFERENCES runs (id),
    instructions_digest TEXT NOT NULL,
    state               TEXT NOT NULL,
    revision            INTEGER NOT NULL,
    updated_at          TEXT NOT NULL
) STRICT;

CREATE TABLE attempts (
    id         TEXT PRIMARY KEY,
    task_id    TEXT NOT NULL REFERENCES tasks (id),
    number     INTEGER NOT NULL,
    state      TEXT NOT NULL,
    revision   INTEGER NOT NULL,
    updated_at TEXT NOT NULL,
    UNIQUE (task_id, number)
) STRICT;

-- At most one active attempt per task: the seven non-terminal attempt
-- states of the section 5 table. A raced second reservation resolves
-- through this index, never through application-level checking alone.
CREATE UNIQUE INDEX attempts_one_active_per_task ON attempts (task_id)
    WHERE state IN ('reserved', 'launching', 'running', 'submitted', 'checking', 'reconciling', 'relaunching');

CREATE TABLE sessions (
    id                 TEXT PRIMARY KEY,
    run_id             TEXT NOT NULL REFERENCES runs (id),
    attempt_id         TEXT NOT NULL REFERENCES attempts (id),
    role               TEXT NOT NULL,
    harness            TEXT NOT NULL,
    native_session_ref TEXT,
    native_ref_source  TEXT,
    state              TEXT NOT NULL,
    revision           INTEGER NOT NULL,
    updated_at         TEXT NOT NULL
) STRICT;

-- At most one non-terminated session per attempt at any instant; lost and
-- terminated are the terminal session states.
CREATE UNIQUE INDEX sessions_one_current_per_attempt ON sessions (attempt_id)
    WHERE state NOT IN ('lost', 'terminated');

-- Append-only binding history: one row per launch incarnation plus
-- observed-restoration rows. creation_label deliberately has no unique
-- constraint: a Herdr-restored occupant keeps the original pane label, so
-- its observed-restoration row shares the label with the binding it will
-- supersede.
CREATE TABLE runtime_bindings (
    id                  TEXT PRIMARY KEY,
    session_id          TEXT NOT NULL REFERENCES sessions (id),
    incarnation_id      TEXT NOT NULL,
    server_socket_path  TEXT NOT NULL,
    server_instance     TEXT,
    workspace_id        TEXT NOT NULL,
    tab_id              TEXT NOT NULL,
    pane_id             TEXT NOT NULL,
    creation_label      TEXT NOT NULL,
    launch_kind         TEXT NOT NULL,
    occupant_evidence   TEXT,
    observed_at         TEXT NOT NULL,
    superseded          INTEGER NOT NULL,
    superseded_at       TEXT,
    superseded_evidence TEXT,
    UNIQUE (session_id, incarnation_id)
) STRICT;

CREATE TABLE launch_claims (
    incarnation_id      TEXT PRIMARY KEY,
    run_id              TEXT NOT NULL REFERENCES runs (id),
    attempt_id          TEXT NOT NULL REFERENCES attempts (id),
    executable          TEXT NOT NULL,
    argv_digest         TEXT NOT NULL,
    pid                 INTEGER NOT NULL,
    state               TEXT NOT NULL,
    error               TEXT,
    claimed_at          TEXT NOT NULL,
    settled_at          TEXT,
    settlement_evidence TEXT
) STRICT;

CREATE TABLE worktrees (
    id            TEXT PRIMARY KEY,
    repository_id TEXT NOT NULL REFERENCES repositories (id),
    run_id        TEXT NOT NULL REFERENCES runs (id),
    path          TEXT NOT NULL,
    branch        TEXT NOT NULL,
    state         TEXT NOT NULL,
    revision      INTEGER NOT NULL,
    created_at    TEXT NOT NULL,
    UNIQUE (path)
) STRICT;

CREATE TABLE results (
    id             TEXT PRIMARY KEY,
    attempt_id     TEXT NOT NULL REFERENCES attempts (id),
    commit_oid     TEXT NOT NULL,
    summary        TEXT NOT NULL,
    content_digest TEXT NOT NULL,
    accepted       INTEGER NOT NULL,
    submitted_at   TEXT NOT NULL,
    UNIQUE (attempt_id, content_digest)
) STRICT;

-- At most one accepted result per attempt; the accepted receipt is
-- immutable and never replaced.
CREATE UNIQUE INDEX results_one_accepted_per_attempt ON results (attempt_id)
    WHERE accepted = 1;

-- Submission receipts: claimed identities are plain text with no foreign
-- keys, so a malformed submission still leaves evidence. result_id names
-- the accepted result an accepted or duplicate receipt refers to.
CREATE TABLE result_submissions (
    id                     TEXT PRIMARY KEY,
    claimed_run_id         TEXT NOT NULL,
    claimed_task_id        TEXT NOT NULL,
    claimed_attempt_id     TEXT NOT NULL,
    claimed_incarnation_id TEXT NOT NULL,
    claimed_commit_oid     TEXT,
    claimed_summary        TEXT,
    content_digest         TEXT,
    result_id              TEXT,
    outcome                TEXT NOT NULL,
    detail                 TEXT NOT NULL,
    submitted_at           TEXT NOT NULL
) STRICT;

CREATE TABLE check_requests (
    result_id          TEXT PRIMARY KEY REFERENCES results (id),
    attempt_id         TEXT NOT NULL REFERENCES attempts (id),
    state              TEXT NOT NULL,
    created_at         TEXT NOT NULL,
    claimed_generation INTEGER
) STRICT;

CREATE TABLE artifacts (
    id         TEXT PRIMARY KEY,
    run_id     TEXT NOT NULL REFERENCES runs (id),
    result_id  TEXT REFERENCES results (id),
    kind       TEXT NOT NULL,
    path       TEXT NOT NULL,
    digest     TEXT NOT NULL,
    created_at TEXT NOT NULL
) STRICT;

CREATE TABLE transitions (
    id          TEXT PRIMARY KEY,
    entity_kind TEXT NOT NULL,
    entity_id   TEXT NOT NULL,
    from_state  TEXT NOT NULL,
    to_state    TEXT NOT NULL,
    reason      TEXT NOT NULL,
    generation  INTEGER,
    at          TEXT NOT NULL
) STRICT;

CREATE TABLE operations (
    id           TEXT PRIMARY KEY,
    run_id       TEXT NOT NULL REFERENCES runs (id),
    generation   INTEGER NOT NULL,
    kind         TEXT NOT NULL,
    state        TEXT NOT NULL,
    intent       TEXT NOT NULL,
    act_evidence TEXT,
    outcome      TEXT,
    created_at   TEXT NOT NULL,
    updated_at   TEXT NOT NULL
) STRICT;

-- The check exec boundary's durable pre-exec identity: hop check-exec
-- records its own pid (its process-group id) here before execve. A
-- worker-authority write, so it lives in its own table rather than in the
-- controller-written operations.act_evidence column.
CREATE TABLE check_exec_claims (
    operation_id TEXT PRIMARY KEY REFERENCES operations (id),
    pid          INTEGER NOT NULL,
    claimed_at   TEXT NOT NULL
) STRICT;

CREATE TABLE run_leases (
    run_id        TEXT PRIMARY KEY REFERENCES runs (id),
    controller_id TEXT NOT NULL,
    generation    INTEGER NOT NULL,
    state         TEXT NOT NULL,
    acquired_at   TEXT NOT NULL,
    heartbeat_at  TEXT NOT NULL,
    expires_at    TEXT NOT NULL
) STRICT;
