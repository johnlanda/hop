package sqlite_test

import (
	"strings"
	"sync"
	"testing"

	"github.com/johnlanda/hop/internal/adapters/sqlite"
	"github.com/johnlanda/hop/internal/app"
	"github.com/johnlanda/hop/internal/domain/identity"
	"github.com/johnlanda/hop/internal/domain/run"
)

// lastReceipt reads the newest result_submissions row for a run.
func lastReceipt(t *testing.T, store *sqlite.Store, runID identity.RunID) (outcome, detail, resultID string) {
	t.Helper()
	var resultIDValue *string
	err := sqlite.WriteDB(store).QueryRowContext(t.Context(),
		`SELECT outcome, detail, result_id FROM result_submissions WHERE claimed_run_id = ? ORDER BY submitted_at DESC, rowid DESC LIMIT 1`,
		runID.String(),
	).Scan(&outcome, &detail, &resultIDValue)
	if err != nil {
		t.Fatalf("read last receipt: %v", err)
	}
	if resultIDValue != nil {
		resultID = *resultIDValue
	}
	return outcome, detail, resultID
}

// receiptCount counts a run's receipts with a given outcome.
func receiptCount(t *testing.T, store *sqlite.Store, runID identity.RunID, outcome app.SubmissionOutcomeKind) int {
	t.Helper()
	return countRows(t, store,
		`SELECT COUNT(*) FROM result_submissions WHERE claimed_run_id = ? AND outcome = ?`,
		runID.String(), string(outcome),
	)
}

// TestSubmitResultAccepted proves the atomic handoff of an ordinary
// acceptance: the result row, the accepted receipt, the unique check
// request, and the attempt/task transitions — all committed together.
func TestSubmitResultAccepted(t *testing.T) {
	f := runningFixture(t)

	outcome, err := f.store.SubmitResult(t.Context(), f.submission(8001, "digest-1"))
	if err != nil {
		t.Fatalf("SubmitResult: %v", err)
	}
	if outcome.Kind != app.SubmissionAccepted || outcome.ResultID != identity.ResultID(uid(8001)) {
		t.Fatalf("outcome = %+v, want accepted with the submitted result id", outcome)
	}
	recorded, _, resultID := lastReceipt(t, f.store, f.spec.RunID)
	if recorded != string(app.SubmissionAccepted) || resultID != uid(8001) {
		t.Fatalf("receipt = (%s, %s), want (accepted, %s)", recorded, resultID, uid(8001))
	}
	if n := countRows(t, f.store, `SELECT COUNT(*) FROM check_requests WHERE result_id = ?`, uid(8001)); n != 1 {
		t.Fatalf("check requests for the accepted result = %d, want 1", n)
	}
	f.inUOW(t, func(uow app.UnitOfWork) {
		attempt, _, err := uow.Attempts().Get(t.Context(), f.spec.AttemptID)
		if err != nil {
			t.Fatalf("get attempt: %v", err)
		}
		if attempt.State != run.AttemptSubmitted {
			t.Fatalf("attempt state = %s, want submitted", attempt.State)
		}
		task, _, err := uow.Tasks().Get(t.Context(), f.spec.TaskID)
		if err != nil {
			t.Fatalf("get task: %v", err)
		}
		if task.State != run.TaskChecking {
			t.Fatalf("task state = %s, want checking", task.State)
		}
	})
	transitions := countRows(t, f.store, `SELECT COUNT(*) FROM transitions WHERE entity_id IN (?, ?) AND generation IS NULL`,
		f.spec.AttemptID.String(), f.spec.TaskID.String())
	if transitions != 2 {
		t.Fatalf("acceptance transition evidence rows = %d, want 2 (attempt, task), generation NULL", transitions)
	}
}

// TestSubmitResultEarlyAcceptance proves the early-submission atomic
// handoff: a launching attempt with a settled (execed) claim accepts, and
// the same transaction moves the run launching → running.
func TestSubmitResultEarlyAcceptance(t *testing.T) {
	f := newFixture(t)
	f.launchAttempt(t)
	f.createBinding(t)
	f.claimLaunch(t)
	f.settleClaimExeced(t)
	// No markRunning: the attempt is still launching when the fast worker
	// submits.

	outcome, err := f.store.SubmitResult(t.Context(), f.submission(8002, "digest-1"))
	if err != nil {
		t.Fatalf("SubmitResult: %v", err)
	}
	if outcome.Kind != app.SubmissionAccepted {
		t.Fatalf("outcome = %+v, want accepted", outcome)
	}
	f.inUOW(t, func(uow app.UnitOfWork) {
		runV, _, err := uow.Runs().Get(t.Context(), f.spec.RunID)
		if err != nil {
			t.Fatalf("get run: %v", err)
		}
		if runV.State != run.RunRunning {
			t.Fatalf("run state after early acceptance = %s, want running", runV.State)
		}
		attempt, _, err := uow.Attempts().Get(t.Context(), f.spec.AttemptID)
		if err != nil {
			t.Fatalf("get attempt: %v", err)
		}
		if attempt.State != run.AttemptSubmitted {
			t.Fatalf("attempt state = %s, want submitted", attempt.State)
		}
	})
	runTransitions := countRows(t, f.store,
		`SELECT COUNT(*) FROM transitions WHERE entity_id = ? AND to_state = 'running' AND generation IS NULL`,
		f.spec.RunID.String())
	if runTransitions != 1 {
		t.Fatalf("run transition evidence rows = %d, want 1", runTransitions)
	}
}

// TestSubmitResultTransient proves a launching attempt with an unsettled
// (exec_pending) claim returns transient with a receipt and no state
// change.
func TestSubmitResultTransient(t *testing.T) {
	f := newFixture(t)
	f.launchAttempt(t)
	f.createBinding(t)
	f.claimLaunch(t)
	// The claim stays exec_pending.

	outcome, err := f.store.SubmitResult(t.Context(), f.submission(8003, "digest-1"))
	if err != nil {
		t.Fatalf("SubmitResult: %v", err)
	}
	if outcome.Kind != app.SubmissionTransient {
		t.Fatalf("outcome = %+v, want transient", outcome)
	}
	if n := receiptCount(t, f.store, f.spec.RunID, app.SubmissionTransient); n != 1 {
		t.Fatalf("transient receipts = %d, want 1", n)
	}
	if n := countRows(t, f.store, `SELECT COUNT(*) FROM results`); n != 0 {
		t.Fatalf("results after a transient submission = %d, want 0", n)
	}
	f.inUOW(t, func(uow app.UnitOfWork) {
		attempt, _, err := uow.Attempts().Get(t.Context(), f.spec.AttemptID)
		if err != nil {
			t.Fatalf("get attempt: %v", err)
		}
		if attempt.State != run.AttemptLaunching {
			t.Fatalf("attempt state after transient = %s, want launching unchanged", attempt.State)
		}
	})
}

// TestSubmitResultDuplicate proves an equal-digest resubmission is
// idempotent in every state, including after the run completes.
func TestSubmitResultDuplicate(t *testing.T) {
	f := runningFixture(t)
	first, err := f.store.SubmitResult(t.Context(), f.submission(8004, "digest-1"))
	if err != nil {
		t.Fatalf("first SubmitResult: %v", err)
	}

	t.Run("immediate retry", func(t *testing.T) {
		outcome, err := f.store.SubmitResult(t.Context(), f.submission(8005, "digest-1"))
		if err != nil {
			t.Fatalf("duplicate SubmitResult: %v", err)
		}
		if outcome.Kind != app.SubmissionDuplicate || outcome.ResultID != first.ResultID {
			t.Fatalf("outcome = %+v, want duplicate naming result %s", outcome, first.ResultID)
		}
		if n := countRows(t, f.store, `SELECT COUNT(*) FROM results`); n != 1 {
			t.Fatalf("results after a duplicate = %d, want 1", n)
		}
	})

	t.Run("retry after terminal state", func(t *testing.T) {
		now := f.clock.Now()
		f.inUOW(t, func(uow app.UnitOfWork) {
			saveAttempt(t, uow, f.spec.AttemptID, func(v run.Attempt) (run.Attempt, error) { return v.EnterChecking(now) })
			saveAttempt(t, uow, f.spec.AttemptID, func(v run.Attempt) (run.Attempt, error) { return v.Complete(now) })
			saveTask(t, uow, f.spec.TaskID, func(v run.Task) (run.Task, error) { return v.Complete(now) })
			saveRun(t, uow, f.spec.RunID, func(v run.Run) (run.Run, error) { return v.EnterCompleting(now) })
			saveRun(t, uow, f.spec.RunID, func(v run.Run) (run.Run, error) { return v.Complete(now) })
		})

		outcome, err := f.store.SubmitResult(t.Context(), f.submission(8006, "digest-1"))
		if err != nil {
			t.Fatalf("duplicate after completion: %v", err)
		}
		if outcome.Kind != app.SubmissionDuplicate || outcome.ResultID != first.ResultID {
			t.Fatalf("outcome after completion = %+v, want duplicate naming result %s", outcome, first.ResultID)
		}
	})
}

// TestSubmitResultConflicting proves a different digest for an attempt with
// an accepted result is rejected without disturbing the accepted result.
func TestSubmitResultConflicting(t *testing.T) {
	f := runningFixture(t)
	first, err := f.store.SubmitResult(t.Context(), f.submission(8007, "digest-1"))
	if err != nil {
		t.Fatalf("first SubmitResult: %v", err)
	}

	outcome, err := f.store.SubmitResult(t.Context(), f.submission(8008, "digest-2"))
	if err != nil {
		t.Fatalf("conflicting SubmitResult: %v", err)
	}
	if outcome.Kind != app.SubmissionConflicting {
		t.Fatalf("outcome = %+v, want conflicting", outcome)
	}
	f.inUOW(t, func(uow app.UnitOfWork) {
		accepted, err := uow.Results().Accepted(t.Context(), f.spec.AttemptID)
		if err != nil {
			t.Fatalf("accepted result: %v", err)
		}
		if accepted == nil || accepted.ID != first.ResultID || accepted.ContentDigest != "digest-1" {
			t.Fatalf("accepted result after conflict = %+v, want the original untouched", accepted)
		}
	})
	if n := receiptCount(t, f.store, f.spec.RunID, app.SubmissionConflicting); n != 1 {
		t.Fatalf("conflicting receipts = %d, want 1", n)
	}
}

// TestSubmitResultStale proves the stale rejections: a non-current
// incarnation, and a run with a stop request.
func TestSubmitResultStale(t *testing.T) {
	t.Run("incarnation not current", func(t *testing.T) {
		f := runningFixture(t)
		sub := f.submission(8009, "digest-1")
		sub.IncarnationID = identity.IncarnationID(uid(6001)) // a retired incarnation

		outcome, err := f.store.SubmitResult(t.Context(), sub)
		if err != nil {
			t.Fatalf("SubmitResult: %v", err)
		}
		if outcome.Kind != app.SubmissionStale {
			t.Fatalf("outcome = %+v, want stale", outcome)
		}
		if n := receiptCount(t, f.store, f.spec.RunID, app.SubmissionStale); n != 1 {
			t.Fatalf("stale receipts = %d, want 1", n)
		}
	})

	t.Run("superseded binding", func(t *testing.T) {
		f := runningFixture(t)
		f.inUOW(t, func(uow app.UnitOfWork) {
			binding, ok, err := uow.Bindings().Current(t.Context(), f.spec.SessionID)
			if err != nil || !ok {
				t.Fatalf("current binding: %v (found %t)", err, ok)
			}
			superseded, err := binding.Supersede("observed replacement occupant", f.clock.Now())
			if err != nil {
				t.Fatalf("supersede: %v", err)
			}
			if err := uow.Bindings().Save(t.Context(), superseded); err != nil {
				t.Fatalf("save superseded binding: %v", err)
			}
		})

		outcome, err := f.store.SubmitResult(t.Context(), f.submission(8010, "digest-1"))
		if err != nil {
			t.Fatalf("SubmitResult: %v", err)
		}
		if outcome.Kind != app.SubmissionStale {
			t.Fatalf("outcome with a superseded binding = %+v, want stale", outcome)
		}
	})

	t.Run("stop requested", func(t *testing.T) {
		f := runningFixture(t)
		if err := f.store.RequestStop(t.Context(), f.spec.RunID); err != nil {
			t.Fatalf("RequestStop: %v", err)
		}

		outcome, err := f.store.SubmitResult(t.Context(), f.submission(8011, "digest-1"))
		if err != nil {
			t.Fatalf("SubmitResult: %v", err)
		}
		if outcome.Kind != app.SubmissionStale {
			t.Fatalf("outcome after stop = %+v, want stale (stop precedence)", outcome)
		}
	})
}

// TestSubmitResultMalformed proves the in-transaction existence and
// agreement checks record malformed receipts with the claimed values.
func TestSubmitResultMalformed(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(f *fixture, sub *app.ResultSubmission)
	}{
		{
			name: "run does not exist",
			mutate: func(_ *fixture, sub *app.ResultSubmission) {
				sub.RunID = identity.RunID(uid(6101))
			},
		},
		{
			name: "task does not belong to run",
			mutate: func(_ *fixture, sub *app.ResultSubmission) {
				sub.TaskID = identity.TaskID(uid(6102))
			},
		},
		{
			name: "attempt does not exist",
			mutate: func(_ *fixture, sub *app.ResultSubmission) {
				sub.AttemptID = identity.AttemptID(uid(6103))
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := runningFixture(t)
			sub := f.submission(8012, "digest-1")
			tc.mutate(f, &sub)

			outcome, err := f.store.SubmitResult(t.Context(), sub)
			if err != nil {
				t.Fatalf("SubmitResult: %v", err)
			}
			if outcome.Kind != app.SubmissionMalformed {
				t.Fatalf("outcome = %+v, want malformed", outcome)
			}
			receipts := countRows(t, f.store,
				`SELECT COUNT(*) FROM result_submissions WHERE claimed_run_id = ? AND outcome = 'malformed'`,
				sub.RunID.String(),
			)
			if receipts != 1 {
				t.Fatalf("malformed receipts claiming run %s = %d, want 1", sub.RunID, receipts)
			}
		})
	}
}

// TestRecordMalformed proves the step 1 receipt path stores the claimed
// values as plain text, truncated to the field limit, with no result row.
func TestRecordMalformed(t *testing.T) {
	f := newFixture(t)
	oversized := strings.Repeat("x", app.ClaimedSubmissionFieldLimit+100)

	outcome, err := f.store.RecordMalformed(t.Context(), app.ClaimedSubmission{
		RunID:         "not-a-uuid",
		TaskID:        "also-not-a-uuid",
		AttemptID:     "",
		IncarnationID: "zzz",
		CommitOID:     "shortsha",
		Summary:       oversized,
		Detail:        "commit is not a 40-hex object id",
	})
	if err != nil {
		t.Fatalf("RecordMalformed: %v", err)
	}
	if outcome.Kind != app.SubmissionMalformed {
		t.Fatalf("outcome = %+v, want malformed", outcome)
	}
	var summary string
	err = sqlite.WriteDB(f.store).QueryRowContext(t.Context(),
		`SELECT claimed_summary FROM result_submissions WHERE claimed_run_id = 'not-a-uuid'`,
	).Scan(&summary)
	if err != nil {
		t.Fatalf("read malformed receipt: %v", err)
	}
	if len(summary) != app.ClaimedSubmissionFieldLimit {
		t.Fatalf("stored claimed summary length = %d, want truncated to %d", len(summary), app.ClaimedSubmissionFieldLimit)
	}
}

// TestClaimLaunch proves the pre-exec claim contract: same-pid idempotence,
// different-pid rejection (including raced from two handles), the
// non-current incarnation refusal, and the stop refusal.
func TestClaimLaunch(t *testing.T) {
	t.Run("same pid is idempotent", func(t *testing.T) {
		f := newFixture(t)
		f.launchAttempt(t)
		f.createBinding(t)
		f.claimLaunch(t)

		f.claimLaunch(t)

		if n := countRows(t, f.store, `SELECT COUNT(*) FROM launch_claims`); n != 1 {
			t.Fatalf("claims after an idempotent rewrite = %d, want 1", n)
		}
	})

	t.Run("different pid is rejected", func(t *testing.T) {
		f := newFixture(t)
		f.launchAttempt(t)
		f.createBinding(t)
		f.claimLaunch(t)

		err := f.store.ClaimLaunch(t.Context(), app.LaunchClaim{
			IncarnationID: f.spec.IncarnationID,
			RunID:         f.spec.RunID,
			AttemptID:     f.spec.AttemptID,
			Executable:    "/opt/harness/claude",
			ArgvDigest:    "argv-digest",
			PID:           222,
		})

		if err == nil {
			t.Fatal("a second launcher's claim by a different pid was accepted")
		}
	})

	t.Run("different pid raced from two handles", func(t *testing.T) {
		clock := newFakeClock()
		root := t.TempDir()
		storeA := openStoreAt(t, root, clock)
		storeB := openStoreAt(t, root, clock)
		spec := newSpec("/repos/alpha", specStride, clock.Now())
		_, lease, err := storeA.InitializeRun(t.Context(), spec)
		if err != nil {
			t.Fatalf("InitializeRun: %v", err)
		}
		f := &fixture{store: storeA, clock: clock, spec: spec, lease: lease}
		f.launchAttempt(t)
		f.createBinding(t)
		claim := func(pid int) app.LaunchClaim {
			return app.LaunchClaim{
				IncarnationID: spec.IncarnationID,
				RunID:         spec.RunID,
				AttemptID:     spec.AttemptID,
				Executable:    "/opt/harness/claude",
				ArgvDigest:    "argv-digest",
				PID:           pid,
			}
		}
		var (
			start sync.WaitGroup
			done  sync.WaitGroup
		)
		start.Add(1)
		errs := make([]error, 2)
		done.Add(2)
		go func() {
			defer done.Done()
			start.Wait()
			errs[0] = storeA.ClaimLaunch(t.Context(), claim(111))
		}()
		go func() {
			defer done.Done()
			start.Wait()
			errs[1] = storeB.ClaimLaunch(t.Context(), claim(222))
		}()
		start.Done()
		done.Wait()

		succeeded := 0
		for _, err := range errs {
			if err == nil {
				succeeded++
			}
		}
		if succeeded != 1 {
			t.Fatalf("raced claims succeeded %d times, want exactly 1 (errors: %v)", succeeded, errs)
		}
		if n := countRows(t, storeA, `SELECT COUNT(*) FROM launch_claims`); n != 1 {
			t.Fatalf("claims after the race = %d, want 1", n)
		}
	})

	t.Run("non-current incarnation is refused", func(t *testing.T) {
		f := newFixture(t)
		f.launchAttempt(t)
		f.createBinding(t)

		err := f.store.ClaimLaunch(t.Context(), app.LaunchClaim{
			IncarnationID: identity.IncarnationID(uid(6201)),
			RunID:         f.spec.RunID,
			AttemptID:     f.spec.AttemptID,
			Executable:    "/opt/harness/claude",
			ArgvDigest:    "argv-digest",
			PID:           111,
		})

		if err == nil {
			t.Fatal("a claim for a non-current incarnation was accepted")
		}
	})

	t.Run("stopping run is refused", func(t *testing.T) {
		f := newFixture(t)
		f.launchAttempt(t)
		f.createBinding(t)
		if err := f.store.RequestStop(t.Context(), f.spec.RunID); err != nil {
			t.Fatalf("RequestStop: %v", err)
		}

		err := f.store.ClaimLaunch(t.Context(), app.LaunchClaim{
			IncarnationID: f.spec.IncarnationID,
			RunID:         f.spec.RunID,
			AttemptID:     f.spec.AttemptID,
			Executable:    "/opt/harness/claude",
			ArgvDigest:    "argv-digest",
			PID:           111,
		})

		if err == nil {
			t.Fatal("a claim against a stopping run was accepted")
		}
	})
}

// TestSettleLaunchFailure proves the launcher's error path: exec_pending
// settles to exec_failed, a repeat is idempotent, and an execed claim
// refuses.
func TestSettleLaunchFailure(t *testing.T) {
	f := newFixture(t)
	f.launchAttempt(t)
	f.createBinding(t)
	f.claimLaunch(t)

	if err := f.store.SettleLaunchFailure(t.Context(), f.spec.IncarnationID, "execve: no such file"); err != nil {
		t.Fatalf("SettleLaunchFailure: %v", err)
	}
	if err := f.store.SettleLaunchFailure(t.Context(), f.spec.IncarnationID, "retry"); err != nil {
		t.Fatalf("idempotent SettleLaunchFailure: %v", err)
	}
	f.inUOW(t, func(uow app.UnitOfWork) {
		claim, ok, err := uow.LaunchClaims().Get(t.Context(), f.spec.IncarnationID)
		if err != nil || !ok {
			t.Fatalf("get claim: %v (found %t)", err, ok)
		}
		if claim.State != app.LaunchClaimExecFailed || claim.Error != "execve: no such file" {
			t.Fatalf("claim = %+v, want exec_failed with the first recorded reason", claim)
		}
		err = uow.LaunchClaims().Settle(t.Context(), f.spec.IncarnationID, app.LaunchClaimSettlement{
			State: app.LaunchClaimExeced, At: f.clock.Now(),
		})
		if err == nil {
			t.Fatal("an exec_failed claim settled to execed")
		}
	})
}

// TestClaimCheckExec proves the check exec boundary's claim contract.
func TestClaimCheckExec(t *testing.T) {
	newOp := func(t *testing.T, f *fixture, opN int, generation int64, kind app.OperationKind, state app.OperationState) identity.OperationID {
		t.Helper()
		opID := identity.OperationID(uid(opN))
		f.inUOW(t, func(uow app.UnitOfWork) {
			err := uow.Operations().Create(t.Context(), app.Operation{
				ID: opID, RunID: f.spec.RunID, Generation: generation,
				Kind: kind, State: state,
				Intent:    map[string]any{"tree_oid": "abc"},
				CreatedAt: f.clock.Now(), UpdatedAt: f.clock.Now(),
			})
			if err != nil {
				t.Fatalf("create operation: %v", err)
			}
		})
		return opID
	}

	t.Run("pending check of the current generation is claimed", func(t *testing.T) {
		f := newFixture(t)
		opID := newOp(t, f, 7101, f.lease.Generation, app.OpCheckRun, app.OperationPending)

		if err := f.store.ClaimCheckExec(t.Context(), opID, 4242); err != nil {
			t.Fatalf("ClaimCheckExec: %v", err)
		}
		if err := f.store.ClaimCheckExec(t.Context(), opID, 4242); err != nil {
			t.Fatalf("idempotent rewrite by the same pid: %v", err)
		}
		if err := f.store.ClaimCheckExec(t.Context(), opID, 9999); err == nil {
			t.Fatal("a claim by a different pid was accepted")
		}
	})

	t.Run("stale generation is refused", func(t *testing.T) {
		f := newFixture(t)
		opID := newOp(t, f, 7102, f.lease.Generation, app.OpCheckRun, app.OperationPending)
		if err := f.store.ReleaseLease(t.Context(), f.lease); err != nil {
			t.Fatalf("release: %v", err)
		}
		if _, err := f.store.AcquireLease(t.Context(), f.spec.RunID, "controller-b"); err != nil {
			t.Fatalf("reacquire: %v", err)
		}

		if err := f.store.ClaimCheckExec(t.Context(), opID, 4242); err == nil {
			t.Fatal("a claim against a prior-generation operation was accepted")
		}
	})

	t.Run("wrong kind is refused", func(t *testing.T) {
		f := newFixture(t)
		opID := newOp(t, f, 7103, f.lease.Generation, app.OpPaneOpen, app.OperationPending)

		if err := f.store.ClaimCheckExec(t.Context(), opID, 4242); err == nil {
			t.Fatal("a claim against a non-check operation was accepted")
		}
	})

	t.Run("settled operation is refused", func(t *testing.T) {
		f := newFixture(t)
		opID := newOp(t, f, 7104, f.lease.Generation, app.OpCheckRun, app.OperationSucceeded)

		if err := f.store.ClaimCheckExec(t.Context(), opID, 4242); err == nil {
			t.Fatal("a claim against a settled operation was accepted")
		}
	})
}

// TestRequestStopMonotonic proves the stop request is monotonic and
// idempotent, moves the run to stopping, and leaves exactly one
// generation-NULL transition row.
func TestRequestStopMonotonic(t *testing.T) {
	f := runningFixture(t)

	if err := f.store.RequestStop(t.Context(), f.spec.RunID); err != nil {
		t.Fatalf("first RequestStop: %v", err)
	}
	if err := f.store.RequestStop(t.Context(), f.spec.RunID); err != nil {
		t.Fatalf("repeated RequestStop: %v", err)
	}

	f.inUOW(t, func(uow app.UnitOfWork) {
		runV, _, err := uow.Runs().Get(t.Context(), f.spec.RunID)
		if err != nil {
			t.Fatalf("get run: %v", err)
		}
		if !runV.StopRequested || runV.State != run.RunStopping {
			t.Fatalf("run after stop requests = %+v, want stop requested and stopping", runV)
		}
	})
	transitions := countRows(t, f.store,
		`SELECT COUNT(*) FROM transitions WHERE entity_id = ? AND to_state = 'stopping' AND generation IS NULL`,
		f.spec.RunID.String())
	if transitions != 1 {
		t.Fatalf("stop transition rows = %d, want exactly 1 despite the repeat", transitions)
	}
}
