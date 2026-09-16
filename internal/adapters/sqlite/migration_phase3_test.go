package sqlite_test

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/johnlanda/hop/internal/adapters/sqlite"
)

// v2ts is the fixed timestamp every populated pre-003 fixture row carries.
const v2ts = "2026-09-02T00:00:00.000000000Z"

// buildV2Store executes the 001 schema plus 002's column verbatim on a raw
// connection and records schema_migrations at version 2, then applies the
// given fixture statements: a genuine pre-Phase-3 store, byte-identical to
// what a Phase 2 binary left behind.
func buildV2Store(t *testing.T, root string, fixture []string) {
	t.Helper()
	schema, err := os.ReadFile(filepath.Join("migrations", "001_initial_schema.sql"))
	if err != nil {
		t.Fatalf("read 001 schema: %v", err)
	}
	seed, err := os.ReadFile(filepath.Join("migrations", "002_launch_claim_seed_evidence.sql"))
	if err != nil {
		t.Fatalf("read 002 schema: %v", err)
	}
	raw, err := sql.Open("sqlite", "file:"+filepath.Join(root, "hop.db")+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	statements := []string{
		string(schema),
		string(seed),
		`CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY, applied_at TEXT NOT NULL) STRICT`,
		`INSERT INTO schema_migrations (version, applied_at) VALUES (1, '` + v2ts + `'), (2, '` + v2ts + `')`,
	}
	statements = append(statements, fixture...)
	for _, stmt := range statements {
		if _, execErr := raw.ExecContext(t.Context(), stmt); execErr != nil {
			t.Fatalf("build v2 store: %v\nstatement: %.120s", execErr, stmt)
		}
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
}

// fixtureIDs are the deterministic identities the populated fixtures share.
const (
	fxRepo        = "aaaaaaaa-aaaa-4aaa-8aaa-00000000000a"
	fxRun         = "aaaaaaaa-aaaa-4aaa-8aaa-00000000000b"
	fxTask        = "aaaaaaaa-aaaa-4aaa-8aaa-00000000000c"
	fxAttempt     = "aaaaaaaa-aaaa-4aaa-8aaa-00000000000d"
	fxSession     = "aaaaaaaa-aaaa-4aaa-8aaa-00000000000e"
	fxIncarnation = "aaaaaaaa-aaaa-4aaa-8aaa-00000000000f"
	fxBinding     = "aaaaaaaa-aaaa-4aaa-8aaa-000000000010"
	fxResult      = "aaaaaaaa-aaaa-4aaa-8aaa-000000000011"
	fxOperation   = "aaaaaaaa-aaaa-4aaa-8aaa-000000000012"
	fxOperation2  = "aaaaaaaa-aaaa-4aaa-8aaa-000000000013"
	fxWorktree    = "aaaaaaaa-aaaa-4aaa-8aaa-000000000014"
)

// coreV2Fixture is the shared repository/run/task/attempt/session chain in
// the given states.
func coreV2Fixture(runState, taskState, attemptState, sessionState string) []string {
	return []string{
		`INSERT INTO repositories (id, root_path, created_at) VALUES ('` + fxRepo + `', '/repos/phase3', '` + v2ts + `')`,
		`INSERT INTO runs (id, repository_id, seq, brief, brief_digest, state, stop_requested_at, revision, created_at, updated_at)
		 VALUES ('` + fxRun + `', '` + fxRepo + `', 1, 'phase3 brief', 'brief-digest', '` + runState + `', NULL, 2, '` + v2ts + `', '` + v2ts + `')`,
		`INSERT INTO run_snapshots (run_id, check_argv, check_timeout_ms, check_repeatable, env_policy, harness, profile_dir, state_root, assignment_path, assignment_digest, created_at)
		 VALUES ('` + fxRun + `', '["sh","check.sh"]', 60000, 0, '{"Version":"v1"}', 'claude', NULL, '/state/root', '/state/root/assignment.md', 'assignment-digest', '` + v2ts + `')`,
		`INSERT INTO tasks (id, run_id, instructions_digest, state, revision, updated_at)
		 VALUES ('` + fxTask + `', '` + fxRun + `', 'instructions-digest', '` + taskState + `', 3, '` + v2ts + `')`,
		`INSERT INTO attempts (id, task_id, number, state, revision, updated_at)
		 VALUES ('` + fxAttempt + `', '` + fxTask + `', 1, '` + attemptState + `', 4, '` + v2ts + `')`,
		`INSERT INTO sessions (id, run_id, attempt_id, role, harness, native_session_ref, native_ref_source, state, revision, updated_at)
		 VALUES ('` + fxSession + `', '` + fxRun + `', '` + fxAttempt + `', 'worker', 'claude', 'native-ref-1', 'assigned', '` + sessionState + `', 5, '` + v2ts + `')`,
		`INSERT INTO run_leases (run_id, controller_id, generation, state, acquired_at, heartbeat_at, expires_at)
		 VALUES ('` + fxRun + `', 'controller-a', 7, 'released', '` + v2ts + `', '` + v2ts + `', '` + v2ts + `')`,
	}
}

// tableSnapshot reads every row of a table as rendered text, ordered by
// rowid, for byte-for-byte preservation comparisons across the migration.
func tableSnapshot(t *testing.T, db *sql.DB, table string, columns ...string) []string {
	t.Helper()
	rows, err := db.QueryContext(t.Context(),
		`SELECT `+strings.Join(columns, ", ")+` FROM `+table+` ORDER BY rowid`) //nolint:gosec // G202: test-only snapshot over fixed table and column names, never external input.
	if err != nil {
		t.Fatalf("snapshot %s: %v", table, err)
	}
	defer rows.Close() //nolint:errcheck // fully-iterated read cursor.
	var out []string
	for rows.Next() {
		values := make([]sql.NullString, len(columns))
		dest := make([]any, len(columns))
		for i := range values {
			dest[i] = &values[i]
		}
		if err := rows.Scan(dest...); err != nil {
			t.Fatalf("scan %s snapshot: %v", table, err)
		}
		fields := make([]string, len(values))
		for i, v := range values {
			if !v.Valid {
				fields[i] = "<NULL>"
				continue
			}
			fields[i] = v.String
		}
		out = append(out, strings.Join(fields, "|"))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate %s snapshot: %v", table, err)
	}
	return out
}

// requireEqualRows compares before/after snapshots.
func requireEqualRows(t *testing.T, table string, before, after []string) {
	t.Helper()
	if len(before) != len(after) {
		t.Fatalf("%s rows changed across the migration: %d before, %d after", table, len(before), len(after))
	}
	for i := range before {
		if before[i] != after[i] {
			t.Fatalf("%s row %d changed across the migration:\n before %s\n after  %s", table, i, before[i], after[i])
		}
	}
}

// openRaw opens a plain read handle on the fixture database.
func openRaw(t *testing.T, root string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(root, "hop.db")+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close raw handle: %v", err)
		}
	})
	return db
}

// requireCleanFKCheck asserts PRAGMA foreign_key_check reports nothing.
func requireCleanFKCheck(t *testing.T, db *sql.DB) {
	t.Helper()
	rows, err := db.QueryContext(t.Context(), `PRAGMA foreign_key_check`)
	if err != nil {
		t.Fatalf("foreign_key_check: %v", err)
	}
	defer rows.Close() //nolint:errcheck // fully-iterated read cursor.
	for rows.Next() {
		var (
			table, parent string
			rowid, fkid   sql.NullInt64
		)
		if err := rows.Scan(&table, &rowid, &parent, &fkid); err != nil {
			t.Fatalf("scan foreign_key_check: %v", err)
		}
		t.Errorf("foreign key violation after migration: %s rowid %d references missing %s", table, rowid.Int64, parent)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate foreign_key_check: %v", err)
	}
}

// TestMigration003ActiveStore upgrades a populated pre-003 store frozen
// mid-launch: a running run whose claim settled through a binding, plus a
// second exec_pending claim window is exercised by the dedicated
// claim-without-binding test below. Every rebuild preserves rows
// byte-for-byte where unchanged, the claim gains the binding's session,
// solo session role/attempt survive, task defaults land, and the store
// opens with a clean foreign_key_check.
func TestMigration003ActiveStore(t *testing.T) {
	root := t.TempDir()
	fixture := coreV2Fixture("running", "active", "running", "active")
	fixture = append(fixture,
		`INSERT INTO runtime_bindings (id, session_id, incarnation_id, server_socket_path, server_instance, workspace_id, tab_id, pane_id, creation_label, launch_kind, occupant_evidence, observed_at, superseded, superseded_at, superseded_evidence)
		 VALUES ('`+fxBinding+`', '`+fxSession+`', '`+fxIncarnation+`', '/tmp/hop.sock', 'srv-1', 'ws-1', 'tab-1', 'pane-1', 'label-1', 'pane', '{"label":"label-1","argv_marker":"m","pid":42}', '`+v2ts+`', 0, NULL, NULL)`,
		`INSERT INTO launch_claims (incarnation_id, run_id, attempt_id, executable, argv_digest, pid, state, error, claimed_at, settled_at, settlement_evidence, seed_evidence)
		 VALUES ('`+fxIncarnation+`', '`+fxRun+`', '`+fxAttempt+`', '/opt/harness/claude', 'argv-digest', 42, 'execed', NULL, '`+v2ts+`', '`+v2ts+`', 'settled', 'workspace trust seeded for /wt')`,
		`INSERT INTO operations (id, run_id, generation, kind, state, intent, act_evidence, outcome, created_at, updated_at)
		 VALUES ('`+fxOperation+`', '`+fxRun+`', 7, 'pane.open', 'succeeded', '{"session_id":"`+fxSession+`","incarnation_id":"`+fxIncarnation+`"}', NULL, '{"pane_id":"pane-1"}', '`+v2ts+`', '`+v2ts+`')`,
		`INSERT INTO worktrees (id, repository_id, run_id, path, branch, state, revision, created_at)
		 VALUES ('`+fxWorktree+`', '`+fxRepo+`', '`+fxRun+`', '/wt/r1', 'hop/r1', 'active', 1, '`+v2ts+`')`,
	)
	buildV2Store(t, root, fixture)

	raw := openRaw(t, root)
	sessionCols := []string{"id", "run_id", "attempt_id", "role", "harness", "native_session_ref", "native_ref_source", "state", "revision", "updated_at"}
	claimCols := []string{"incarnation_id", "run_id", "attempt_id", "executable", "argv_digest", "pid", "state", "error", "claimed_at", "settled_at", "settlement_evidence", "seed_evidence"}
	taskCols := []string{"id", "run_id", "instructions_digest", "state", "revision", "updated_at"}
	bindingCols := []string{"id", "session_id", "incarnation_id", "server_socket_path", "server_instance", "workspace_id", "tab_id", "pane_id", "creation_label", "launch_kind", "occupant_evidence", "observed_at", "superseded", "superseded_at", "superseded_evidence"}
	sessionsBefore := tableSnapshot(t, raw, "sessions", sessionCols...)
	claimsBefore := tableSnapshot(t, raw, "launch_claims", claimCols...)
	tasksBefore := tableSnapshot(t, raw, "tasks", taskCols...)
	bindingsBefore := tableSnapshot(t, raw, "runtime_bindings", bindingCols...)

	store := openStoreAt(t, root, newFakeClock())

	// The unchanged columns of every rebuilt table survive byte-for-byte.
	requireEqualRows(t, "sessions", sessionsBefore, tableSnapshot(t, raw, "sessions", sessionCols...))
	requireEqualRows(t, "launch_claims", claimsBefore, tableSnapshot(t, raw, "launch_claims", claimCols...))
	requireEqualRows(t, "tasks", tasksBefore, tableSnapshot(t, raw, "tasks", taskCols...))
	requireEqualRows(t, "runtime_bindings", bindingsBefore, tableSnapshot(t, raw, "runtime_bindings", bindingCols...))

	// The claim gained the binding's session; the session gained a NULL
	// parent; the task gained the solo defaults.
	var claimSession string
	if err := sqlite.WriteDB(store).QueryRowContext(t.Context(),
		`SELECT session_id FROM launch_claims WHERE incarnation_id = ?`, fxIncarnation,
	).Scan(&claimSession); err != nil || claimSession != fxSession {
		t.Fatalf("backfilled claim session = %q, %v; want the binding's session", claimSession, err)
	}
	var parent sql.NullString
	if err := sqlite.WriteDB(store).QueryRowContext(t.Context(),
		`SELECT parent_session_id FROM sessions WHERE id = ?`, fxSession,
	).Scan(&parent); err != nil || parent.Valid {
		t.Fatalf("solo session parent = %v (%v), want NULL", parent, err)
	}
	var kind, title, instructionsPath, createdAt string
	var seq, retryCount int64
	if err := sqlite.WriteDB(store).QueryRowContext(t.Context(),
		`SELECT kind, seq, title, instructions_path, retry_count, created_at FROM tasks WHERE id = ?`, fxTask,
	).Scan(&kind, &seq, &title, &instructionsPath, &retryCount, &createdAt); err != nil {
		t.Fatalf("read task defaults: %v", err)
	}
	if kind != "implement" || seq != 1 || title != "" || instructionsPath != "" || retryCount != 0 || createdAt != v2ts {
		t.Fatalf("task backfill = kind %q seq %d title %q path %q retries %d created %q; want implement/1/''/''/0 with created_at copied from updated_at", kind, seq, title, instructionsPath, retryCount, createdAt)
	}
	requireCleanFKCheck(t, raw)
}

// TestMigration003CompletedAndFailedStores upgrades populated terminal
// stores: check requests are re-keyed (id = result_id,
// subject_kind='result', integration_id NULL) with values preserved, and
// results/receipts survive untouched.
func TestMigration003CompletedAndFailedStores(t *testing.T) {
	cases := []struct {
		name                                       string
		runState, taskState, attemptState, session string
		requestState                               string
	}{
		{name: "completed", runState: "completed", taskState: "completed", attemptState: "completed", session: "terminated", requestState: "settled"},
		{name: "failed", runState: "failed", taskState: "failed", attemptState: "failed", session: "terminated", requestState: "settled"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			fixture := coreV2Fixture(tc.runState, tc.taskState, tc.attemptState, tc.session)
			fixture = append(fixture,
				`INSERT INTO runtime_bindings (id, session_id, incarnation_id, server_socket_path, server_instance, workspace_id, tab_id, pane_id, creation_label, launch_kind, occupant_evidence, observed_at, superseded, superseded_at, superseded_evidence)
				 VALUES ('`+fxBinding+`', '`+fxSession+`', '`+fxIncarnation+`', '/tmp/hop.sock', NULL, 'ws-1', 'tab-1', 'pane-1', 'label-1', 'pane', NULL, '`+v2ts+`', 1, '`+v2ts+`', 'retired')`,
				`INSERT INTO launch_claims (incarnation_id, run_id, attempt_id, executable, argv_digest, pid, state, error, claimed_at, settled_at, settlement_evidence, seed_evidence)
				 VALUES ('`+fxIncarnation+`', '`+fxRun+`', '`+fxAttempt+`', '/opt/harness/claude', 'argv-digest', 42, 'execed', NULL, '`+v2ts+`', '`+v2ts+`', 'settled', NULL)`,
				`INSERT INTO results (id, attempt_id, commit_oid, summary, content_digest, accepted, submitted_at)
				 VALUES ('`+fxResult+`', '`+fxAttempt+`', 'commit-1', 'done', 'digest-1', 1, '`+v2ts+`')`,
				`INSERT INTO check_requests (result_id, attempt_id, state, created_at, claimed_generation)
				 VALUES ('`+fxResult+`', '`+fxAttempt+`', '`+tc.requestState+`', '`+v2ts+`', 7)`,
				`INSERT INTO result_submissions (id, claimed_run_id, claimed_task_id, claimed_attempt_id, claimed_incarnation_id, claimed_commit_oid, claimed_summary, content_digest, result_id, outcome, detail, submitted_at)
				 VALUES ('aaaaaaaa-aaaa-4aaa-8aaa-000000000020', '`+fxRun+`', '`+fxTask+`', '`+fxAttempt+`', '`+fxIncarnation+`', 'commit-1', 'done', 'digest-1', '`+fxResult+`', 'accepted', '', '`+v2ts+`')`,
			)
			buildV2Store(t, root, fixture)

			raw := openRaw(t, root)
			requestCols := []string{"result_id", "attempt_id", "state", "created_at", "claimed_generation"}
			resultCols := []string{"id", "attempt_id", "commit_oid", "summary", "content_digest", "accepted", "submitted_at"}
			receiptCols := []string{"id", "claimed_run_id", "claimed_task_id", "claimed_attempt_id", "claimed_incarnation_id", "claimed_commit_oid", "claimed_summary", "content_digest", "result_id", "outcome", "detail", "submitted_at"}
			requestsBefore := tableSnapshot(t, raw, "check_requests", requestCols...)
			resultsBefore := tableSnapshot(t, raw, "results", resultCols...)
			receiptsBefore := tableSnapshot(t, raw, "result_submissions", receiptCols...)

			openStoreAt(t, root, newFakeClock())

			requireEqualRows(t, "check_requests", requestsBefore, tableSnapshot(t, raw, "check_requests", requestCols...))
			requireEqualRows(t, "results", resultsBefore, tableSnapshot(t, raw, "results", resultCols...))
			requireEqualRows(t, "result_submissions", receiptsBefore, tableSnapshot(t, raw, "result_submissions", receiptCols...))

			var id, subjectKind string
			var integrationID sql.NullString
			if err := raw.QueryRowContext(t.Context(),
				`SELECT id, subject_kind, integration_id FROM check_requests WHERE result_id = ?`, fxResult,
			).Scan(&id, &subjectKind, &integrationID); err != nil {
				t.Fatalf("read re-keyed check request: %v", err)
			}
			if id != fxResult || subjectKind != "result" || integrationID.Valid {
				t.Fatalf("re-keyed check request = id %q subject %q integration %v; want id=result_id, subject_kind='result', integration NULL", id, subjectKind, integrationID)
			}
			requireCleanFKCheck(t, raw)
		})
	}
}

// TestMigration003ClaimWithoutBinding upgrades a store whose exec_pending
// claim raced the controller's pane.open outcome: no binding row exists,
// and the claim's session backfills from the HISTORICAL launch intent —
// the operation whose intent JSON carries the claim's incarnation, whose
// session_id field is authoritative.
func TestMigration003ClaimWithoutBinding(t *testing.T) {
	root := t.TempDir()
	fixture := coreV2Fixture("launching", "active", "launching", "launching")
	fixture = append(fixture,
		`INSERT INTO operations (id, run_id, generation, kind, state, intent, act_evidence, outcome, created_at, updated_at)
		 VALUES ('`+fxOperation+`', '`+fxRun+`', 7, 'pane.open', 'pending', '{"session_id":"`+fxSession+`","incarnation_id":"`+fxIncarnation+`","creation_label":"label-1"}', NULL, NULL, '`+v2ts+`', '`+v2ts+`')`,
		`INSERT INTO launch_claims (incarnation_id, run_id, attempt_id, executable, argv_digest, pid, state, error, claimed_at, settled_at, settlement_evidence, seed_evidence)
		 VALUES ('`+fxIncarnation+`', '`+fxRun+`', '`+fxAttempt+`', '/opt/harness/claude', 'argv-digest', 42, 'exec_pending', NULL, '`+v2ts+`', NULL, NULL, NULL)`,
	)
	buildV2Store(t, root, fixture)

	store := openStoreAt(t, root, newFakeClock())

	var claimSession string
	if err := sqlite.WriteDB(store).QueryRowContext(t.Context(),
		`SELECT session_id FROM launch_claims WHERE incarnation_id = ?`, fxIncarnation,
	).Scan(&claimSession); err != nil || claimSession != fxSession {
		t.Fatalf("intent-backfilled claim session = %q, %v; want the historical intent's session", claimSession, err)
	}
	requireCleanFKCheck(t, openRaw(t, root))
}

// TestMigration003AmbiguousBackfillRefused proves real ambiguity is
// surfaced, never guessed away: a claim matching no binding and no intent,
// one matching two intents, and one whose only intent carries no session
// each fail the migration NAMING the claim, and the store is left
// unmigrated (version 2, old schema intact) for a human to resolve.
func TestMigration003AmbiguousBackfillRefused(t *testing.T) {
	intent := func(op, incarnation, sessionJSON string) string {
		return `INSERT INTO operations (id, run_id, generation, kind, state, intent, act_evidence, outcome, created_at, updated_at)
		 VALUES ('` + op + `', '` + fxRun + `', 7, 'pane.open', 'pending', '{` + sessionJSON + `"incarnation_id":"` + incarnation + `"}', NULL, NULL, '` + v2ts + `', '` + v2ts + `')`
	}
	cases := []struct {
		name    string
		fixture []string
		want    string
	}{
		{
			name:    "no binding and no intent",
			fixture: nil,
			want:    "matches no binding row and no historical launch intent",
		},
		{
			name: "more than one intent",
			fixture: []string{
				intent(fxOperation, fxIncarnation, `"session_id":"`+fxSession+`",`),
				intent(fxOperation2, fxIncarnation, `"session_id":"`+fxSession+`",`),
			},
			want: "2 historical launch intents",
		},
		{
			name:    "intent without a session identity",
			fixture: []string{intent(fxOperation, fxIncarnation, ``)},
			want:    "carries no session identity",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			fixture := coreV2Fixture("launching", "active", "launching", "launching")
			fixture = append(fixture,
				`INSERT INTO launch_claims (incarnation_id, run_id, attempt_id, executable, argv_digest, pid, state, error, claimed_at, settled_at, settlement_evidence, seed_evidence)
				 VALUES ('`+fxIncarnation+`', '`+fxRun+`', '`+fxAttempt+`', '/opt/harness/claude', 'argv-digest', 42, 'exec_pending', NULL, '`+v2ts+`', NULL, NULL, NULL)`,
			)
			fixture = append(fixture, tc.fixture...)
			buildV2Store(t, root, fixture)

			_, err := sqlite.Open(t.Context(), root, sqlite.Options{Clock: newFakeClock()})

			if err == nil {
				t.Fatal("Open migrated a store whose claim backfill is ambiguous")
			}
			if !strings.Contains(err.Error(), fxIncarnation) {
				t.Fatalf("refusal does not name the claim: %v", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("refusal reason = %v, want it to contain %q", err, tc.want)
			}
			raw := openRaw(t, root)
			var version int
			if err := raw.QueryRowContext(t.Context(), `SELECT MAX(version) FROM schema_migrations`).Scan(&version); err != nil || version != 2 {
				t.Fatalf("store version after refusal = %d (%v), want 2 with nothing applied", version, err)
			}
			var sessionCol int
			if err := raw.QueryRowContext(t.Context(),
				`SELECT COUNT(*) FROM pragma_table_info('launch_claims') WHERE name = 'session_id'`,
			).Scan(&sessionCol); err != nil || sessionCol != 0 {
				t.Fatalf("launch_claims gained session_id despite the refusal (%d, %v); the rebuild must roll back whole", sessionCol, err)
			}
		})
	}
}

// TestMigration003FromEmpty proves a fresh store carries the complete
// Phase 3 surface: every new table, the rebuilt shapes, and the new
// partial unique indexes.
func TestMigration003FromEmpty(t *testing.T) {
	store := openStoreAt(t, t.TempDir(), newFakeClock())

	for _, index := range []string{
		"sessions_one_current_per_attempt",
		"sessions_one_manager_per_run",
		"messages_one_answer_per_question",
		"message_receipts_accepted_request",
		"reviews_one_verdict_per_attempt",
		"integrations_one_current_per_run",
		"retry_requests_one_pending_per_task",
		"workflow_receipts_accepted_request",
		"check_requests_one_per_result",
		"check_requests_one_per_integration",
		"tasks_run_seq_unique",
	} {
		n := countRows(t, store, `SELECT COUNT(*) FROM sqlite_schema WHERE type = 'index' AND name = ?`, index)
		if n != 1 {
			t.Errorf("index %q does not exist after migration", index)
		}
	}
	for column, table := range map[string]string{
		"plan_closed_at":    "runs",
		"workflow":          "run_snapshots",
		"attempt_id":        "worktrees",
		"base_commit":       "worktrees",
		"parent_session_id": "sessions",
		"session_id":        "launch_claims",
		"subject_kind":      "check_requests",
		"mailbox_closed_at": "tasks",
	} {
		var n int
		if err := sqlite.WriteDB(store).QueryRowContext(t.Context(),
			fmt.Sprintf(`SELECT COUNT(*) FROM pragma_table_info('%s') WHERE name = ?`, table), column,
		).Scan(&n); err != nil || n != 1 {
			t.Errorf("column %s.%s missing after migration (%d, %v)", table, column, n, err)
		}
	}
}

// TestMigration003ForeignKeyCheckRefused proves the rebuild's
// foreign_key_check gate: a claim whose one historical intent names a
// SESSION THAT DOES NOT EXIST passes the ambiguity validation (the
// intent's session_id field is authoritative and present) but the
// rebuilt launch_claims row then references a missing session — the
// migration fails on the check, before commit, and the store is left
// unmigrated.
func TestMigration003ForeignKeyCheckRefused(t *testing.T) {
	root := t.TempDir()
	fixture := coreV2Fixture("launching", "active", "launching", "launching")
	const ghostSession = "aaaaaaaa-aaaa-4aaa-8aaa-00000000dead"
	fixture = append(fixture,
		`INSERT INTO operations (id, run_id, generation, kind, state, intent, act_evidence, outcome, created_at, updated_at)
		 VALUES ('`+fxOperation+`', '`+fxRun+`', 7, 'pane.open', 'pending', '{"session_id":"`+ghostSession+`","incarnation_id":"`+fxIncarnation+`"}', NULL, NULL, '`+v2ts+`', '`+v2ts+`')`,
		`INSERT INTO launch_claims (incarnation_id, run_id, attempt_id, executable, argv_digest, pid, state, error, claimed_at, settled_at, settlement_evidence, seed_evidence)
		 VALUES ('`+fxIncarnation+`', '`+fxRun+`', '`+fxAttempt+`', '/opt/harness/claude', 'argv-digest', 42, 'exec_pending', NULL, '`+v2ts+`', NULL, NULL, NULL)`,
	)
	buildV2Store(t, root, fixture)

	_, err := sqlite.Open(t.Context(), root, sqlite.Options{Clock: newFakeClock()})

	if err == nil {
		t.Fatal("Open migrated a store whose backfilled claim references a missing session")
	}
	if !strings.Contains(err.Error(), "foreign key violations") || !strings.Contains(err.Error(), "launch_claims") {
		t.Fatalf("refusal = %v, want the foreign_key_check gate naming launch_claims", err)
	}
	raw := openRaw(t, root)
	var version int
	if scanErr := raw.QueryRowContext(t.Context(), `SELECT MAX(version) FROM schema_migrations`).Scan(&version); scanErr != nil || version != 2 {
		t.Fatalf("store version after the refusal = %d (%v), want 2 with nothing committed", version, scanErr)
	}
}
