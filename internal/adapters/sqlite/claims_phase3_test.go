package sqlite_test

import (
	"database/sql"
	"strings"
	"testing"

	"github.com/johnlanda/hop/internal/adapters/sqlite"
	"github.com/johnlanda/hop/internal/app"
	"github.com/johnlanda/hop/internal/domain/identity"
	"github.com/johnlanda/hop/internal/domain/run"
)

// createLaunchIntentFor commits a pending pane.open operation whose intent
// names session and incarnation — the controller's pre-dispatch write the
// session-keyed claim fallback reads.
func createLaunchIntentFor(t *testing.T, f *featureFixture, opN int, session identity.SessionID, incarnation identity.IncarnationID) {
	t.Helper()
	f.inUOW(t, func(uow app.UnitOfWork) {
		err := uow.Operations().Create(t.Context(), app.Operation{
			ID: identity.OperationID(uid(opN)), RunID: f.spec.RunID, Generation: f.lease.Generation,
			Kind: app.OpPaneOpen, State: app.OperationPending,
			Intent: map[string]any{
				"session_id":     session.String(),
				"incarnation_id": incarnation.String(),
				"creation_label": uid(opN + 1),
			},
			CreatedAt: f.clock.Now(), UpdatedAt: f.clock.Now(),
		})
		if err != nil {
			t.Fatalf("create launch intent: %v", err)
		}
	})
}

// pendingChildSession reserves an attempt and creates a LAUNCHING child
// session with NO binding — the pre-binding claim window.
func pendingChildSession(t *testing.T, f *featureFixture, task identity.TaskID, n int) (identity.SessionID, identity.AttemptID) {
	t.Helper()
	attemptID := identity.AttemptID(uid(n))
	sessionID := identity.SessionID(uid(n + 1))
	now := f.clock.Now()
	manager := f.managerSessionValue(t)
	f.inUOW(t, func(uow app.UnitOfWork) {
		wf := workflowRepos(t, uow)
		attempt, attemptErr := run.NewAttempt(attemptID, task, 1, now)
		if attemptErr != nil {
			t.Fatalf("new attempt: %v", attemptErr)
		}
		if _, err := wf.AttemptIndex().Create(t.Context(), attempt); err != nil {
			t.Fatalf("create attempt: %v", err)
		}
		session, err := run.NewChildSession(sessionID, f.spec.RunID, attemptID, run.RoleImplementer, manager, run.HarnessClaude, now)
		if err != nil {
			t.Fatalf("new child session: %v", err)
		}
		if session, err = session.Launch(now); err != nil {
			t.Fatalf("launch child session: %v", err)
		}
		if _, err := uow.Sessions().Create(t.Context(), session); err != nil {
			t.Fatalf("create child session: %v", err)
		}
	})
	return sessionID, attemptID
}

// claimFor builds a launch claim for the given identities.
func claimFor(f *featureFixture, incarnation identity.IncarnationID, session identity.SessionID, attempt identity.AttemptID, pid int) app.LaunchClaim {
	return app.LaunchClaim{
		IncarnationID: incarnation, RunID: f.spec.RunID, SessionID: session, AttemptID: attempt,
		Executable: "/opt/harness/claude", ArgvDigest: "argv-digest", PID: pid,
	}
}

// TestClaimLaunchSessionKeyedIntents is the B2 vector: two sessions with
// concurrently PENDING launch intents each validate their own claim
// independently — another session's later intent can no longer invalidate
// an earlier claim's currency, which the Phase 2 run-newest lookup would
// have done.
func TestClaimLaunchSessionKeyedIntents(t *testing.T) {
	f := newFeatureFixture(t)
	taskB := f.createFeatureTask(t, 7701, 2, run.TaskActive)
	taskC := f.createFeatureTask(t, 7702, 3, run.TaskActive)
	sessionB, attemptB := pendingChildSession(t, f, taskB, 7703)
	sessionC, attemptC := pendingChildSession(t, f, taskC, 7706)
	incarnationB := identity.IncarnationID(uid(7709))
	incarnationC := identity.IncarnationID(uid(7710))
	createLaunchIntentFor(t, f, 7711, sessionB, incarnationB)
	f.clock.Advance(1)
	// C's intent is the RUN's newest pending launch operation; B's claim
	// must still validate against B's own.
	createLaunchIntentFor(t, f, 7713, sessionC, incarnationC)

	if err := f.store.ClaimLaunch(t.Context(), claimFor(f, incarnationB, sessionB, attemptB, 111)); err != nil {
		t.Fatalf("ClaimLaunch(B under the older intent) = %v; the session-keyed lookup must validate it", err)
	}
	if err := f.store.ClaimLaunch(t.Context(), claimFor(f, incarnationC, sessionC, attemptC, 222)); err != nil {
		t.Fatalf("ClaimLaunch(C) = %v", err)
	}
	for _, tc := range []struct {
		incarnation identity.IncarnationID
		session     identity.SessionID
	}{{incarnationB, sessionB}, {incarnationC, sessionC}} {
		var storedSession string
		if err := sqlite.WriteDB(f.store).QueryRowContext(t.Context(),
			`SELECT session_id FROM launch_claims WHERE incarnation_id = ?`, tc.incarnation.String(),
		).Scan(&storedSession); err != nil || storedSession != tc.session.String() {
			t.Fatalf("claim %s session = %q, %v; want %s", tc.incarnation, storedSession, err, tc.session)
		}
	}

	// A claim whose incarnation matches ANOTHER session's intent is
	// refused: the claimed session's own newest intent decides.
	wrong := claimFor(f, incarnationC, sessionB, attemptB, 333)
	wrong.IncarnationID = identity.IncarnationID(uid(7715))
	if err := f.store.ClaimLaunch(t.Context(), wrong); err == nil || !strings.Contains(err.Error(), "current identity") {
		t.Fatalf("ClaimLaunch(foreign incarnation) = %v, want the currency refusal", err)
	}
}

// TestClaimLaunchByAttemptResolvesSession proves the Phase 2 caller shape
// (attempt only, no SessionID) still claims: the store resolves the
// attempt's current session exactly as the currency read path does and
// records it on the row.
func TestClaimLaunchByAttemptResolvesSession(t *testing.T) {
	f := newFeatureFixture(t)
	taskB := f.createFeatureTask(t, 7721, 2, run.TaskActive)
	sessionB, attemptB := pendingChildSession(t, f, taskB, 7722)
	incarnation := identity.IncarnationID(uid(7725))
	createLaunchIntentFor(t, f, 7726, sessionB, incarnation)

	claim := claimFor(f, incarnation, "", attemptB, 444)
	if err := f.store.ClaimLaunch(t.Context(), claim); err != nil {
		t.Fatalf("ClaimLaunch(by attempt) = %v", err)
	}
	var storedSession string
	if err := sqlite.WriteDB(f.store).QueryRowContext(t.Context(),
		`SELECT session_id FROM launch_claims WHERE incarnation_id = ?`, incarnation.String(),
	).Scan(&storedSession); err != nil || storedSession != sessionB.String() {
		t.Fatalf("resolved claim session = %q, %v; want the attempt's current session", storedSession, err)
	}
}

// TestClaimLaunchManagerSession proves the attempt-less claim shape: a
// manager session's launcher claims by session id alone, the row carrying
// a NULL attempt.
func TestClaimLaunchManagerSession(t *testing.T) {
	clock := newFakeClock()
	store := openStoreAt(t, t.TempDir(), clock)
	spec := newSpec("/repos/manager-claim", specStride, clock.Now())
	lease := initLegacyFeatureRun(t, store, &spec)
	f := &featureFixture{fixture: &fixture{store: store, clock: clock, spec: spec, lease: lease}}
	managerID := identity.SessionID(uid(7731))
	incarnation := identity.IncarnationID(uid(7732))
	now := clock.Now()
	// The manager session is created LAUNCHING with no binding — the
	// launcher is the pane's own command and claims first.
	f.inUOW(t, func(uow app.UnitOfWork) {
		manager := run.NewManagerSession(managerID, spec.RunID, run.HarnessClaude, now)
		manager, launchErr := manager.Launch(now)
		if launchErr != nil {
			t.Fatal(launchErr)
		}
		if _, err := uow.Sessions().Create(t.Context(), manager); err != nil {
			t.Fatalf("create manager session: %v", err)
		}
	})
	createLaunchIntentFor(t, f, 7733, managerID, incarnation)

	claim := app.LaunchClaim{
		IncarnationID: incarnation, RunID: spec.RunID, SessionID: managerID,
		Executable: "/opt/harness/claude", ArgvDigest: "argv-digest", PID: 555,
	}
	if err := store.ClaimLaunch(t.Context(), claim); err != nil {
		t.Fatalf("ClaimLaunch(manager) = %v", err)
	}
	var attemptID sql.NullString
	if err := sqlite.WriteDB(store).QueryRowContext(t.Context(),
		`SELECT attempt_id FROM launch_claims WHERE incarnation_id = ?`, incarnation.String(),
	).Scan(&attemptID); err != nil || attemptID.Valid {
		t.Fatalf("manager claim attempt = %v, %v; want NULL", attemptID, err)
	}

	// A terminal session never claims.
	f.inUOW(t, func(uow app.UnitOfWork) {
		saveSession(t, uow, managerID, func(v run.Session) (run.Session, error) { return v.Reconcile(now) })
		saveSession(t, uow, managerID, func(v run.Session) (run.Session, error) { return v.Terminate(now) })
	})
	late := claim
	late.IncarnationID = identity.IncarnationID(uid(7734))
	if err := store.ClaimLaunch(t.Context(), late); err == nil || !strings.Contains(err.Error(), "terminal") {
		t.Fatalf("ClaimLaunch(terminal session) = %v, want the terminal refusal", err)
	}
}

// TestClaimCheckExecAcceptsMergeOperations proves the generalized exec
// boundary's claim kinds: a pending integration.merge operation of the
// current generation claims exactly like check.run, and every other kind
// is refused.
func TestClaimCheckExecAcceptsMergeOperations(t *testing.T) {
	f := newFeatureFixture(t)
	mergeOp := identity.OperationID(uid(7741))
	paneOp := identity.OperationID(uid(7742))
	f.inUOW(t, func(uow app.UnitOfWork) {
		for _, op := range []app.Operation{
			{
				ID: mergeOp, RunID: f.spec.RunID, Generation: f.lease.Generation, Kind: app.OpIntegrationMerge, State: app.OperationPending,
				Intent: map[string]any{"merge_argv": []string{"/usr/bin/git", "merge"}}, CreatedAt: f.clock.Now(), UpdatedAt: f.clock.Now(),
			},
			{
				ID: paneOp, RunID: f.spec.RunID, Generation: f.lease.Generation, Kind: app.OpPaneOpen, State: app.OperationPending,
				Intent: map[string]any{"session_id": uid(1)}, CreatedAt: f.clock.Now(), UpdatedAt: f.clock.Now(),
			},
		} {
			if err := uow.Operations().Create(t.Context(), op); err != nil {
				t.Fatalf("create operation: %v", err)
			}
		}
	})

	if err := f.store.ClaimCheckExec(t.Context(), mergeOp, 4321); err != nil {
		t.Fatalf("ClaimCheckExec(integration.merge) = %v; want the merge execution claimable", err)
	}
	// Idempotent same-pid rewrite; different pid refused.
	if err := f.store.ClaimCheckExec(t.Context(), mergeOp, 4321); err != nil {
		t.Fatalf("ClaimCheckExec(same pid retry) = %v", err)
	}
	if err := f.store.ClaimCheckExec(t.Context(), mergeOp, 9999); err == nil {
		t.Fatal("ClaimCheckExec(different pid) accepted; want refused")
	}
	if err := f.store.ClaimCheckExec(t.Context(), paneOp, 4321); err == nil || !strings.Contains(err.Error(), "not a pending check or merge execution") {
		t.Fatalf("ClaimCheckExec(pane.open) = %v, want the kind refusal", err)
	}
}
