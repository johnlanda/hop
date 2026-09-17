package sqlite_test

import (
	"database/sql"
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

// seedClaim builds the fixture's launch claim carrying evidence, otherwise
// identical to claimLaunch's.
func (f *fixture) seedClaim(evidence string) app.LaunchClaim {
	return app.LaunchClaim{
		IncarnationID: f.spec.IncarnationID,
		RunID:         f.spec.RunID,
		AttemptID:     f.spec.AttemptID,
		Executable:    "/opt/harness/claude",
		ArgvDigest:    "argv-digest",
		PID:           fixturePID,
		SeedEvidence:  evidence,
	}
}

// claimSeedEvidence reads the fixture claim row's persisted seed evidence
// and whether the column is NULL (the pre-migration row shape).
func claimSeedEvidence(t *testing.T, f *fixture) (value string, isNull bool) {
	t.Helper()
	var v sql.NullString
	if err := sqlite.WriteDB(f.store).QueryRowContext(t.Context(),
		`SELECT seed_evidence FROM launch_claims WHERE incarnation_id = ?`, f.spec.IncarnationID.String(),
	).Scan(&v); err != nil {
		t.Fatalf("read seed evidence: %v", err)
	}
	return v.String, !v.Valid
}

// loadClaimRow reads the fixture claim's invocation identity fields.
func loadClaimRow(t *testing.T, f *fixture) app.LaunchClaim {
	t.Helper()
	var (
		executable, argvDigest, state string
		pid                           int
	)
	if err := sqlite.WriteDB(f.store).QueryRowContext(t.Context(),
		`SELECT executable, argv_digest, pid, state FROM launch_claims WHERE incarnation_id = ?`, f.spec.IncarnationID.String(),
	).Scan(&executable, &argvDigest, &pid, &state); err != nil {
		t.Fatalf("read launch claim row: %v", err)
	}
	return app.LaunchClaim{Executable: executable, ArgvDigest: argvDigest, PID: pid, State: app.LaunchClaimState(state)}
}

// TestClaimLaunch proves the pre-exec claim contract: same-pid idempotence
// with the seed-evidence refresh, different-pid rejection (including raced
// from two handles), the settled-history and incompatible-retry refusals,
// the non-current incarnation refusal, and the stop refusal.
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
		if got, null := claimSeedEvidence(t, f); null || got != fixtureSeedEvidence {
			t.Fatalf("seed evidence after an identical retry = %q (null %t), want it unchanged", got, null)
		}
	})

	t.Run("a same-pid retry refreshes NULL seed evidence", func(t *testing.T) {
		f := newFixture(t)
		f.launchAttempt(t)
		f.createBinding(t)
		// A claim with empty evidence persists NULL — the shape of every
		// row written before the seed-evidence migration.
		if err := f.store.ClaimLaunch(t.Context(), f.seedClaim("")); err != nil {
			t.Fatalf("ClaimLaunch: %v", err)
		}
		if _, null := claimSeedEvidence(t, f); !null {
			t.Fatal("the empty-evidence claim did not persist NULL")
		}

		if err := f.store.ClaimLaunch(t.Context(), f.seedClaim(fixtureSeedEvidence)); err != nil {
			t.Fatalf("ClaimLaunch retry: %v", err)
		}

		if got, null := claimSeedEvidence(t, f); null || got != fixtureSeedEvidence {
			t.Fatalf("seed evidence after the retry = %q (null %t), want the refreshed value", got, null)
		}
		claim := loadClaimRow(t, f)
		if claim.Executable != "/opt/harness/claude" || claim.ArgvDigest != "argv-digest" || claim.PID != fixturePID || claim.State != app.LaunchClaimExecPending {
			t.Fatalf("claim identity after the evidence refresh = %+v, want it untouched", claim)
		}
	})

	t.Run("a same-pid retry records a changed seed outcome", func(t *testing.T) {
		f := newFixture(t)
		f.launchAttempt(t)
		f.createBinding(t)
		if err := f.store.ClaimLaunch(t.Context(), f.seedClaim("workspace trust not seeded: profile config absent")); err != nil {
			t.Fatalf("ClaimLaunch: %v", err)
		}

		if err := f.store.ClaimLaunch(t.Context(), f.seedClaim(fixtureSeedEvidence)); err != nil {
			t.Fatalf("ClaimLaunch retry: %v", err)
		}

		if got, null := claimSeedEvidence(t, f); null || got != fixtureSeedEvidence {
			t.Fatalf("seed evidence after the changed-outcome retry = %q (null %t), want the new outcome", got, null)
		}
	})

	t.Run("a settled claim refuses a same-pid retry", func(t *testing.T) {
		f := newFixture(t)
		f.launchAttempt(t)
		f.createBinding(t)
		f.claimLaunch(t)
		if err := f.store.SettleLaunchFailure(t.Context(), f.spec.IncarnationID, "exec failed"); err != nil {
			t.Fatalf("SettleLaunchFailure: %v", err)
		}

		err := f.store.ClaimLaunch(t.Context(), f.seedClaim(fixtureSeedEvidence))

		if err == nil || !strings.Contains(err.Error(), "settled") {
			t.Fatalf("err = %v, want the settled-history refusal", err)
		}
		if got, null := claimSeedEvidence(t, f); null || got != fixtureSeedEvidence {
			t.Fatalf("settled claim's evidence = %q (null %t); settled history must stay untouched", got, null)
		}
	})

	t.Run("an incompatible same-pid retry is refused", func(t *testing.T) {
		f := newFixture(t)
		f.launchAttempt(t)
		f.createBinding(t)
		f.claimLaunch(t)

		incompatible := f.seedClaim(fixtureSeedEvidence)
		incompatible.ArgvDigest = "a-differently-composed-argv"
		err := f.store.ClaimLaunch(t.Context(), incompatible)

		if err == nil || !strings.Contains(err.Error(), "different executable or argv") {
			t.Fatalf("err = %v, want the incompatible-retry refusal", err)
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

	t.Run("claim before binding with matching intent is accepted", func(t *testing.T) {
		f := newFixture(t)
		f.launchAttempt(t)
		f.createLaunchIntent(t, f.spec.SessionID, f.spec.IncarnationID)
		// No binding row yet: the launcher raced the controller's pane.open
		// outcome write.

		f.claimLaunch(t)

		if n := countRows(t, f.store, `SELECT COUNT(*) FROM launch_claims`); n != 1 {
			t.Fatalf("claims after the pre-binding claim = %d, want 1", n)
		}
	})

	t.Run("stale incarnation before binding is refused", func(t *testing.T) {
		f := newFixture(t)
		f.launchAttempt(t)
		f.createLaunchIntent(t, f.spec.SessionID, f.spec.IncarnationID)

		err := f.store.ClaimLaunch(t.Context(), app.LaunchClaim{
			IncarnationID: identity.IncarnationID(uid(6205)),
			RunID:         f.spec.RunID,
			AttemptID:     f.spec.AttemptID,
			Executable:    "/opt/harness/claude",
			ArgvDigest:    "argv-digest",
			PID:           fixturePID,
		})

		if err == nil {
			t.Fatal("a pre-binding claim whose incarnation is not the pending intent's was accepted")
		}
	})

	t.Run("a binding and a differing pending intent fail closed", func(t *testing.T) {
		f := newFixture(t)
		f.launchAttempt(t)
		f.createBinding(t)
		other := identity.IncarnationID(uid(6206))
		f.createLaunchIntent(t, f.spec.SessionID, other)

		// The intent fallback never overrides an existing current binding,
		// and the disagreement retires the binding's own incarnation too:
		// the launch context refuses exactly this state.
		for _, incarnation := range []identity.IncarnationID{other, f.spec.IncarnationID} {
			err := f.store.ClaimLaunch(t.Context(), app.LaunchClaim{
				IncarnationID: incarnation,
				RunID:         f.spec.RunID,
				AttemptID:     f.spec.AttemptID,
				Executable:    "/opt/harness/claude",
				ArgvDigest:    "argv-digest",
				PID:           fixturePID,
			})
			if err == nil || !strings.Contains(err.Error(), "current identity") {
				t.Fatalf("ClaimLaunch(%s) under a disagreeing binding and intent = %v; want the currency refusal", incarnation, err)
			}
		}
		if n := countRows(t, f.store, `SELECT COUNT(*) FROM launch_claims WHERE run_id = ?`, f.spec.RunID.String()); n != 0 {
			t.Fatalf("claims after the refusals = %d, want 0", n)
		}
	})

	t.Run("superseded binding retires the intent fallback", func(t *testing.T) {
		f := newFixture(t)
		f.launchAttempt(t)
		f.createBinding(t)
		f.createLaunchIntent(t, f.spec.SessionID, f.spec.IncarnationID)
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

		err := f.store.ClaimLaunch(t.Context(), app.LaunchClaim{
			IncarnationID: f.spec.IncarnationID,
			RunID:         f.spec.RunID,
			AttemptID:     f.spec.AttemptID,
			Executable:    "/opt/harness/claude",
			ArgvDigest:    "argv-digest",
			PID:           fixturePID,
		})

		if err == nil {
			t.Fatal("a retired incarnation reclaimed through the intent fallback after supersession")
		}
	})

	t.Run("attempt of another run is refused before and after the owner stops", func(t *testing.T) {
		clock := newFakeClock()
		store := openStoreAt(t, t.TempDir(), clock)
		specA := newSpec("/repos/alpha", 1*specStride, clock.Now())
		specB := newSpec("/repos/beta", 2*specStride, clock.Now())
		_, leaseA, err := store.InitializeRun(t.Context(), specA)
		if err != nil {
			t.Fatalf("InitializeRun A: %v", err)
		}
		if _, _, err := store.InitializeRun(t.Context(), specB); err != nil {
			t.Fatalf("InitializeRun B: %v", err)
		}
		fA := &fixture{store: store, clock: clock, spec: specA, lease: leaseA}
		fA.launchAttempt(t)
		fA.createBinding(t)
		mixed := app.LaunchClaim{
			RunID:         specB.RunID, // B's run with A's attempt and incarnation
			AttemptID:     specA.AttemptID,
			IncarnationID: specA.IncarnationID,
			Executable:    "/opt/harness/claude",
			ArgvDigest:    "argv-digest",
			PID:           fixturePID,
		}

		if err := store.ClaimLaunch(t.Context(), mixed); err == nil {
			t.Fatal("a mixed run/attempt tuple was accepted while the owner is unstopped")
		}
		if err := store.RequestStop(t.Context(), specA.RunID); err != nil {
			t.Fatalf("RequestStop A: %v", err)
		}
		if err := store.ClaimLaunch(t.Context(), mixed); err == nil {
			t.Fatal("a mixed run/attempt tuple bypassed the owning run's stop request")
		}
		if n := countRows(t, store, `SELECT COUNT(*) FROM launch_claims`); n != 0 {
			t.Fatalf("launch claims after refused mixed tuples = %d, want 0", n)
		}
	})

	t.Run("existing claim must agree on run and attempt", func(t *testing.T) {
		clock := newFakeClock()
		store := openStoreAt(t, t.TempDir(), clock)
		specA := newSpec("/repos/alpha", 1*specStride, clock.Now())
		specB := newSpec("/repos/alpha", 2*specStride, clock.Now())
		_, leaseA, err := store.InitializeRun(t.Context(), specA)
		if err != nil {
			t.Fatalf("InitializeRun A: %v", err)
		}
		_, leaseB, err := store.InitializeRun(t.Context(), specB)
		if err != nil {
			t.Fatalf("InitializeRun B: %v", err)
		}
		fA := &fixture{store: store, clock: clock, spec: specA, lease: leaseA}
		fA.launchAttempt(t)
		fA.createBinding(t)
		fA.claimLaunch(t)
		// Same pid, same incarnation, but claiming B's run and attempt: the
		// rewrite is not idempotent, it is a disagreement.
		fB := &fixture{store: store, clock: clock, spec: specB, lease: leaseB}
		fB.launchAttempt(t)
		fB.createBinding(t)

		err = store.ClaimLaunch(t.Context(), app.LaunchClaim{
			RunID:         specB.RunID,
			AttemptID:     specB.AttemptID,
			IncarnationID: specA.IncarnationID, // A's already-claimed incarnation
			Executable:    "/opt/harness/claude",
			ArgvDigest:    "argv-digest",
			PID:           fixturePID,
		})

		if err == nil {
			t.Fatal("an existing claim was rewritten onto a different run and attempt")
		}
	})

	t.Run("replacement session does not revive a retired incarnation", func(t *testing.T) {
		f := newFixture(t)
		f.launchAttempt(t)
		f.createBinding(t)
		f.createLaunchIntent(t, f.spec.SessionID, f.spec.IncarnationID) // old intent, still pending
		newSessionID := identity.SessionID(uid(6301))
		f.inUOW(t, func(uow app.UnitOfWork) {
			binding, ok, err := uow.Bindings().Current(t.Context(), f.spec.SessionID)
			if err != nil || !ok {
				t.Fatalf("current binding: %v (found %t)", err, ok)
			}
			superseded, err := binding.Supersede("positive retirement evidence", f.clock.Now())
			if err != nil {
				t.Fatalf("supersede: %v", err)
			}
			if err := uow.Bindings().Save(t.Context(), superseded); err != nil {
				t.Fatalf("save superseded binding: %v", err)
			}
			saveSession(t, uow, f.spec.SessionID, func(v run.Session) (run.Session, error) { return v.Reconcile(f.clock.Now()) })
			saveSession(t, uow, f.spec.SessionID, func(v run.Session) (run.Session, error) { return v.Terminate(f.clock.Now()) })
			if _, err := uow.Sessions().Create(t.Context(), run.NewSession(newSessionID, f.spec.RunID, f.spec.AttemptID, run.HarnessClaude, f.clock.Now())); err != nil {
				t.Fatalf("create replacement session: %v", err)
			}
		})

		// The replacement session has zero bindings and the OLD launch
		// intent is still the run's newest pending one: the retired
		// incarnation must not claim through it.
		err := f.store.ClaimLaunch(t.Context(), app.LaunchClaim{
			IncarnationID: f.spec.IncarnationID,
			RunID:         f.spec.RunID,
			AttemptID:     f.spec.AttemptID,
			Executable:    "/opt/harness/claude",
			ArgvDigest:    "argv-digest",
			PID:           fixturePID,
		})
		if err == nil {
			t.Fatal("a retired incarnation claimed through the old session's still-pending intent")
		}

		// Once the controller commits the NEW session's launch intent, only
		// the new incarnation claims.
		newIncarnation := identity.IncarnationID(uid(6302))
		f.createLaunchIntent(t, newSessionID, newIncarnation)
		if err := f.store.ClaimLaunch(t.Context(), app.LaunchClaim{
			IncarnationID: f.spec.IncarnationID,
			RunID:         f.spec.RunID,
			AttemptID:     f.spec.AttemptID,
			Executable:    "/opt/harness/claude",
			ArgvDigest:    "argv-digest",
			PID:           fixturePID,
		}); err == nil {
			t.Fatal("the retired incarnation claimed even after the new intent was committed")
		}
		if err := f.store.ClaimLaunch(t.Context(), app.LaunchClaim{
			IncarnationID: newIncarnation,
			RunID:         f.spec.RunID,
			AttemptID:     f.spec.AttemptID,
			Executable:    "/opt/harness/claude",
			ArgvDigest:    "argv-digest",
			PID:           fixturePID,
		}); err != nil {
			t.Fatalf("the new session's incarnation could not claim through its intent: %v", err)
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

// TestWriteContentionWithHeldImmediateTransaction proves two writers
// contending on the same database resolve through the busy handling: a
// worker-authority write started while another connection holds an
// immediate transaction still commits once that transaction ends. The
// handshake only orders start-then-release; whether the contender actually
// blocked or raced ahead, the outcome is the same committed stop request.
func TestWriteContentionWithHeldImmediateTransaction(t *testing.T) {
	clock := newFakeClock()
	root := t.TempDir()
	storeA := openStoreAt(t, root, clock)
	storeB := openStoreAt(t, root, clock)
	spec := newSpec("/repos/alpha", specStride, clock.Now())
	if _, _, err := storeA.InitializeRun(t.Context(), spec); err != nil {
		t.Fatalf("InitializeRun: %v", err)
	}
	heldTx, err := sqlite.WriteDB(storeA).BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatalf("hold immediate transaction: %v", err)
	}

	started := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		close(started)
		done <- storeB.RequestStop(t.Context(), spec.RunID)
	}()
	<-started
	if rbErr := heldTx.Rollback(); rbErr != nil {
		t.Fatalf("release held transaction: %v", rbErr)
	}

	if stopErr := <-done; stopErr != nil {
		t.Fatalf("contending RequestStop: %v", stopErr)
	}
	detail, err := storeA.LoadRunStatus(t.Context(), spec.RunID)
	if err != nil {
		t.Fatalf("LoadRunStatus: %v", err)
	}
	if detail.State != run.RunStopping {
		t.Fatalf("run state after the contended stop = %s, want stopping", detail.State)
	}
}

// TestSubmitResultRacedAcrossHandles proves the acceptance transaction
// arbitration: two handles racing the same content commit exactly one
// accepted result, one duplicate acknowledgment, two receipts and one
// check request.
func TestSubmitResultRacedAcrossHandles(t *testing.T) {
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
	f.claimLaunch(t)
	f.settleClaimExeced(t)
	f.markRunning(t)

	var (
		start sync.WaitGroup
		done  sync.WaitGroup
	)
	start.Add(1)
	outcomes := make([]app.SubmissionOutcome, 2)
	errs := make([]error, 2)
	done.Add(2)
	for i, submitter := range []*sqlite.Store{storeA, storeB} {
		go func() {
			defer done.Done()
			start.Wait()
			outcomes[i], errs[i] = submitter.SubmitResult(t.Context(), f.submission(8700+i, "digest-raced"))
		}()
	}
	start.Done()
	done.Wait()

	accepted, duplicate := 0, 0
	for i, err := range errs {
		if err != nil {
			t.Fatalf("raced submission %d: %v", i, err)
		}
		switch outcomes[i].Kind {
		case app.SubmissionAccepted:
			accepted++
		case app.SubmissionDuplicate:
			duplicate++
		default:
			t.Fatalf("raced submission %d outcome = %s, want accepted or duplicate", i, outcomes[i].Kind)
		}
	}
	if accepted != 1 || duplicate != 1 {
		t.Fatalf("raced outcomes: %d accepted, %d duplicate; want exactly 1 and 1", accepted, duplicate)
	}
	if outcomes[0].ResultID != outcomes[1].ResultID {
		t.Fatalf("raced outcomes name results %s and %s, want the same accepted result", outcomes[0].ResultID, outcomes[1].ResultID)
	}
	if n := countRows(t, storeA, `SELECT COUNT(*) FROM results`); n != 1 {
		t.Fatalf("results after the race = %d, want 1", n)
	}
	if n := countRows(t, storeA, `SELECT COUNT(*) FROM result_submissions`); n != 2 {
		t.Fatalf("receipts after the race = %d, want 2", n)
	}
	if n := countRows(t, storeA, `SELECT COUNT(*) FROM check_requests`); n != 1 {
		t.Fatalf("check requests after the race = %d, want 1", n)
	}
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
