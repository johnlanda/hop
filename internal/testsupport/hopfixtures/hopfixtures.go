// Package hopfixtures seeds runs, tasks, attempts, sessions and bindings
// directly through the application's store port interfaces
// (app.StateStore, app.SubmissionStore, app.UnitOfWork and
// app.WorkflowRepositories) — never through `hop run`, which places a
// worker through Herdr. It exists solely for cmd/hop's real-binary
// grammar contract tests (docs/plan/phase-3-design.md section 11), which
// need a fixture state root behind the built hop binary but must never
// start a Herdr server: the only CLI bootstrap of a feature-mode run,
// `hop run --workflow feature`, places the manager through Herdr, so the
// fixture has to be built directly against the store.
//
// Every exported identity is a plain string and every exported parameter
// or return type is either a string or a type from internal/app: cmd/hop
// test files (composition code, forbidden from importing
// internal/domain/identity or internal/domain/run even in _test.go files
// — internal/arch_test.go's cmd/hop rule) call into this package and
// never see a domain type. This package itself is a categoryTestHelper
// package, whose architecture-checker category may depend on application
// and domain code only, never a driven adapter — so it never imports
// internal/adapters/sqlite either; cmd/hop's test files open the real
// adapter (which implements the Store interface below, among other
// ports) and pass the opened value in directly.
//
// One exception stays out of this package by the manager's own ruling:
// promoting a seeded run to feature mode requires writing the frozen
// app.WorkflowSnapshot into run_snapshots.workflow, a raw SQL write
// against the store file (the solo InitializeRun this package drives
// writes NULL there, and no port method sets it after the fact; the
// feature InitializeRun creates its own manager session and refuses the
// solo base this package seeds). That single write lives in cmd/hop's
// own test file, using database/sql directly, because composition code
// may import any standard package and the sqlite driver is registered by
// cmd/hop's own adapter import — see cmd/hop/grammarcontract_seed_test.go's
// freezeWorkflowSnapshot.
package hopfixtures

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"time"

	"github.com/johnlanda/hop/internal/app"
	"github.com/johnlanda/hop/internal/domain/identity"
	"github.com/johnlanda/hop/internal/domain/run"
)

// Store is the store capability every fixture in this package needs:
// unit-of-work access for entity creation and transitions
// (app.StateStore) and the launcher's pre-exec claim write
// (app.SubmissionStore). The real internal/adapters/sqlite Store
// implements both, among the other Phase 3 ports cmd/hop wires; pass it
// directly.
type Store interface {
	app.StateStore
	app.SubmissionStore
}

// uid renders a deterministic, canonical-shaped lowercase UUID from an
// integer seed: 8-4-4-4-12 hyphenated hex groups, matching
// internal/domain/identity's parse contract exactly.
func uid(seed int) string {
	return fmt.Sprintf("00000000-0000-4000-8000-%012d", seed)
}

// derivedUID renders a deterministic canonical-shaped UUID from an
// arbitrary string basis (an FNV-1a hash folded into uid's numeric
// form) — used for adapter-internal rows (the operation journal) this
// package mints without the caller choosing a seed for them, so a
// collision-free id still only needs the caller's OWN identity
// (already unique) as input.
func derivedUID(basis string) string {
	h := fnv.New64a()
	_, _ = h.Write([]byte(basis))
	return uid(int(h.Sum64() % 1_000_000_000))
}

// Entity-role offsets within one fixture's seed block. Callers space
// their own seeds a safe distance apart (multiples of 1000 comfortably
// clear every offset below plus this package's own internal per-call
// offsets) so two fixtures built in the same test never collide.
const (
	offRun = iota + 1
	offTask
	offAttempt
	offSession
	offWorktree
	offIncarnation
	offNativeRef
)

// Base is a run seeded at exactly InitializeRun's own initial state (run
// created, task pending, attempt reserved, session reserved) — the
// substrate for both a solo fixture (used as-is, or driven running) and a
// feature-mode one (its bootstrap task/attempt/session are simply left
// unused; freeze the workflow snapshot, then call SeedManager). Every
// field is a plain string.
type Base struct {
	RunID         string
	TaskID        string
	AttemptID     string
	SessionID     string
	IncarnationID string
}

// Initialize seeds a fresh Base run. stateRoot and repositoryRoot must
// already be resolved exactly as cmd/hop's own resolution would produce
// them — stateRoot is frozen into the run snapshot and compared for
// agreement against the launcher's own HOP_STATE_DIR by hop launch's
// environment validation, and repositoryRoot is the exact value
// cmd/hop's `-C` resolution (or its default working directory) must
// reproduce to find this run again.
func Initialize(ctx context.Context, store Store, stateRoot, repositoryRoot string, seed int, now time.Time) (Base, app.Lease, error) {
	runID := uid(seed + offRun)
	spec := app.NewRunSpec{
		RepositoryRoot:     repositoryRoot,
		RunID:              identity.RunID(runID),
		TaskID:             identity.TaskID(uid(seed + offTask)),
		AttemptID:          identity.AttemptID(uid(seed + offAttempt)),
		SessionID:          identity.SessionID(uid(seed + offSession)),
		WorktreeID:         identity.WorktreeID(uid(seed + offWorktree)),
		IncarnationID:      identity.IncarnationID(uid(seed + offIncarnation)),
		Brief:              "grammar contract fixture",
		BriefDigest:        "brief-digest",
		InstructionsDigest: "instructions-digest",
		Snapshot: app.RunSnapshot{
			CheckArgv:        []string{"sh", "-c", "true"},
			CheckTimeout:     10 * time.Minute,
			EnvPolicy:        app.EnvPolicy{Version: app.EnvPolicyVersion1, Harness: app.HarnessClaude},
			Harness:          app.HarnessClaude,
			StateRoot:        stateRoot,
			AssignmentPath:   stateRoot + "/runs/" + runID + "/artifacts/assignment.md",
			AssignmentDigest: "assignment-digest",
		},
		Harness:          run.HarnessClaude,
		NativeSessionRef: uid(seed + offNativeRef),
		ControllerID:     "grammar-contract-fixture",
		Now:              now,
	}
	_, lease, err := store.InitializeRun(ctx, spec)
	if err != nil {
		return Base{}, app.Lease{}, fmt.Errorf("hopfixtures: initialize run: %w", err)
	}
	return Base{
		RunID:         runID,
		TaskID:        spec.TaskID.String(),
		AttemptID:     spec.AttemptID.String(),
		SessionID:     spec.SessionID.String(),
		IncarnationID: spec.IncarnationID.String(),
	}, lease, nil
}

// withUOW opens a unit of work bound to lease, runs fn, and commits on
// success or rolls back on any error fn returns.
func withUOW(ctx context.Context, store Store, lease app.Lease, fn func(app.UnitOfWork) error) error {
	uow, err := store.Begin(ctx, lease)
	if err != nil {
		return fmt.Errorf("hopfixtures: begin unit of work: %w", err)
	}
	if err := fn(uow); err != nil {
		_ = uow.Rollback() //nolint:errcheck // best-effort: the original error is what matters.
		return err
	}
	if err := uow.Commit(); err != nil {
		return fmt.Errorf("hopfixtures: commit unit of work: %w", err)
	}
	return nil
}

// ErrBaseAlreadyLaunched is LaunchBase's refusal of a Base whose run has
// already left Initialize's created state.
var ErrBaseAlreadyLaunched = errors.New("hopfixtures: the base run has already been launched; LaunchBase is single-use per Base")

// LaunchBase drives a Base's run/task/attempt/session from Initialize's
// initial reserved states through to "launching" (run launching, task
// active, attempt launching, session launching) and commits a pending
// pane.open launch operation naming the session and incarnation — the
// state hop launch's PrepareSessionLaunchExec requires before it will
// even validate the pane environment, and the pre-binding currency
// evidence ClaimLaunch itself requires before any binding row exists
// (internal/adapters/sqlite's own documented contract: the controller
// commits this intent before dispatching the pane request).
//
// LaunchBase is single-use per Base, NOT idempotent: a second call — or
// RunBase after LaunchBase, since RunBase launches the Base itself — is
// refused with ErrBaseAlreadyLaunched before anything is written, because
// the run has already left created.
func LaunchBase(ctx context.Context, store Store, lease app.Lease, f Base, now time.Time) error { //nolint:gocritic // hugeParam: Base is a small fixture-identity value read once per call, never a hot loop; mirrors this codebase's own convention for domain-shaped DTOs passed by value.
	return withUOW(ctx, store, lease, func(uow app.UnitOfWork) error {
		current, _, getErr := uow.Runs().Get(ctx, identity.RunID(f.RunID))
		if getErr != nil {
			return fmt.Errorf("hopfixtures: get run: %w", getErr)
		}
		if current.State != run.RunCreated {
			return ErrBaseAlreadyLaunched
		}
		if err := launchRun(ctx, uow, f.RunID, now); err != nil {
			return err
		}
		if err := activateTask(ctx, uow, f.TaskID, now); err != nil {
			return err
		}
		if err := launchAttempt(ctx, uow, f.AttemptID, now); err != nil {
			return err
		}
		if err := launchSession(ctx, uow, f.SessionID, now); err != nil {
			return err
		}
		err := uow.Operations().Create(ctx, app.Operation{
			ID:         identity.OperationID(derivedUID(f.SessionID + "-launch-intent")),
			RunID:      identity.RunID(f.RunID),
			Generation: lease.Generation,
			Kind:       app.OpPaneOpen,
			State:      app.OperationPending,
			Intent: map[string]any{
				"session_id":     f.SessionID,
				"incarnation_id": f.IncarnationID,
				"creation_label": derivedUID(f.SessionID + "-creation-label"),
			},
			CreatedAt: now,
			UpdatedAt: now,
		})
		if err != nil {
			return fmt.Errorf("hopfixtures: create pending launch intent: %w", err)
		}
		return nil
	})
}

// fixtureSocketPath, fixtureServerInstance and fixturePID are the
// cosmetic runtime-binding and launch-claim values every fixture uses:
// nothing under test ever dials the socket path or inspects the pid
// beyond currency comparisons, so a fixed value is sufficient — except
// where a test deliberately chooses its own pid (SeedLaunchClaim) to
// prove a claim refusal.
const (
	fixtureSocketPath     = "/tmp/hop-fixture-herdr.sock"
	fixtureServerInstance = "fixture-server-instance"
	fixturePID            = 4242
)

// RunBase finishes what LaunchBase starts: a runtime binding for the
// session's incarnation, a settled (execed) launch claim, then the
// running lifecycle transitions (attempt running, session active, run
// running) — the state every worker-plumbing verb (hop msg/task/plan/
// review, hop result submit) needs from its caller's session. It calls
// LaunchBase itself first, so callers only ever need Initialize then
// RunBase to reach a fully running solo (or feature-mode bootstrap)
// session — and must not call LaunchBase themselves first
// (ErrBaseAlreadyLaunched).
func RunBase(ctx context.Context, store Store, lease app.Lease, f Base, now time.Time) error { //nolint:gocritic // hugeParam: Base is a small fixture-identity value read once per call, never a hot loop; mirrors this codebase's own convention for domain-shaped DTOs passed by value.
	if err := LaunchBase(ctx, store, lease, f, now); err != nil {
		return err
	}
	if err := withUOW(ctx, store, lease, func(uow app.UnitOfWork) error {
		return uow.Bindings().Create(ctx, run.NewRuntimeBinding(
			identity.SessionID(f.SessionID), identity.IncarnationID(f.IncarnationID),
			fixtureSocketPath, fixtureServerInstance, "fixture-workspace", "fixture-tab", "fixture-pane-"+f.SessionID,
			"fixture-label-"+f.SessionID, run.LaunchInitial, now,
		))
	}); err != nil {
		return fmt.Errorf("hopfixtures: create binding: %w", err)
	}
	if err := store.ClaimLaunch(ctx, app.LaunchClaim{
		IncarnationID: identity.IncarnationID(f.IncarnationID),
		RunID:         identity.RunID(f.RunID),
		SessionID:     identity.SessionID(f.SessionID),
		AttemptID:     identity.AttemptID(f.AttemptID),
		Executable:    "/opt/harness/fixture-claude",
		ArgvDigest:    "fixture-argv-digest",
		PID:           fixturePID,
		ClaimedAt:     now,
	}); err != nil {
		return fmt.Errorf("hopfixtures: claim launch: %w", err)
	}
	return withUOW(ctx, store, lease, func(uow app.UnitOfWork) error {
		if err := uow.LaunchClaims().Settle(ctx, identity.IncarnationID(f.IncarnationID), app.LaunchClaimSettlement{
			State: app.LaunchClaimExeced, PaneID: "fixture-pane-" + f.SessionID, PID: fixturePID,
			Executable: "/opt/harness/fixture-claude", ArgvMarker: f.IncarnationID, At: now,
		}); err != nil {
			return fmt.Errorf("hopfixtures: settle launch claim: %w", err)
		}
		if err := markAttemptRunning(ctx, uow, f.AttemptID, now); err != nil {
			return err
		}
		if err := activateSession(ctx, uow, f.SessionID, now); err != nil {
			return err
		}
		return markRunRunning(ctx, uow, f.RunID, now)
	})
}

// SeedLaunchClaim writes a launch claim directly (no lifecycle
// transitions), so a subsequent real `hop launch` exec against the same
// session/incarnation observes an existing claim under a DIFFERENT pid
// than its own process — cmd/hop's real-binary proof of hop launch's
// "a launch claim for this incarnation already exists with a different
// pid" refusal, reached without ever letting a launcher actually exec.
// Call this AFTER LaunchBase (or RunBase's own LaunchBase step) has put
// the session in the launching state a claim requires.
func SeedLaunchClaim(ctx context.Context, store Store, runID, sessionID, attemptID, incarnationID string, pid int, now time.Time) error {
	claim := app.LaunchClaim{
		IncarnationID: identity.IncarnationID(incarnationID),
		RunID:         identity.RunID(runID),
		SessionID:     identity.SessionID(sessionID),
		Executable:    "/opt/harness/fixture-claude",
		ArgvDigest:    "fixture-argv-digest",
		PID:           pid,
		ClaimedAt:     now,
	}
	if attemptID != "" {
		claim.AttemptID = identity.AttemptID(attemptID)
	}
	if err := store.ClaimLaunch(ctx, claim); err != nil {
		return fmt.Errorf("hopfixtures: seed launch claim: %w", err)
	}
	return nil
}

// SeedManager drives runID's run to running and creates a launched,
// active manager session with a current runtime binding — the identity
// every manager verb (hop task create/retry, hop plan close) and every
// messaging verb addressed to "manager" validates against. Call this
// AFTER freezing the run's workflow snapshot to feature mode (the
// cmd/hop-side raw SQL write); it does not itself touch run_snapshots.
func SeedManager(ctx context.Context, store Store, lease app.Lease, runID string, seed int, now time.Time) (managerID, managerIncarnation string, err error) {
	managerID = uid(seed + 1)
	managerIncarnation = uid(seed + 2)
	txErr := withUOW(ctx, store, lease, func(uow app.UnitOfWork) error {
		if err := launchRun(ctx, uow, runID, now); err != nil {
			return err
		}
		if err := markRunRunning(ctx, uow, runID, now); err != nil {
			return err
		}
		manager := run.NewManagerSession(identity.SessionID(managerID), identity.RunID(runID), run.HarnessClaude, now)
		manager, launchErr := manager.Launch(now)
		if launchErr != nil {
			return fmt.Errorf("hopfixtures: launch manager session: %w", launchErr)
		}
		if manager, launchErr = manager.ConfirmActive(now); launchErr != nil {
			return fmt.Errorf("hopfixtures: activate manager session: %w", launchErr)
		}
		if _, err := uow.Sessions().Create(ctx, manager); err != nil {
			return fmt.Errorf("hopfixtures: create manager session: %w", err)
		}
		binding := run.NewRuntimeBinding(
			identity.SessionID(managerID), identity.IncarnationID(managerIncarnation),
			fixtureSocketPath, fixtureServerInstance, "fixture-workspace-mgr", "fixture-tab-mgr", "fixture-pane-mgr",
			"fixture-label-mgr", run.LaunchInitial, now,
		)
		if err := uow.Bindings().Create(ctx, binding); err != nil {
			return fmt.Errorf("hopfixtures: create manager binding: %w", err)
		}
		return nil
	})
	if txErr != nil {
		return "", "", txErr
	}
	return managerID, managerIncarnation, nil
}

// SeedImplementTask creates one implement task through the controller's
// workflow index (bypassing PlanStore's own hop task create path, which
// is exercised separately through the real binary), in state (one of
// internal/domain/run's TaskState string values, e.g. "ready" or
// "active"), ready to bind a worker session to.
func SeedImplementTask(ctx context.Context, store Store, lease app.Lease, runID string, seq, seed int, title, state string, now time.Time) (taskID string, err error) {
	taskID = uid(seed)
	txErr := withUOW(ctx, store, lease, func(uow app.UnitOfWork) error {
		wf, wfErr := app.RequireWorkflowRepositories(uow, "hopfixtures.SeedImplementTask")
		if wfErr != nil {
			return fmt.Errorf("hopfixtures: %w", wfErr)
		}
		task := run.NewImplementTask(identity.TaskID(taskID), identity.RunID(runID), seq, title, "instructions-digest", false, now)
		task.State = run.TaskState(state)
		if _, createErr := wf.TaskIndex().Create(ctx, task); createErr != nil {
			return fmt.Errorf("hopfixtures: create implement task: %w", createErr)
		}
		return nil
	})
	if txErr != nil {
		return "", txErr
	}
	return taskID, nil
}

// SeedReviewTask creates one review task through the controller's
// workflow index, its subject frozen at creation, in state (a
// run.TaskState string value — TestSubmitReviewAcceptance-shaped
// fixtures want "active").
func SeedReviewTask(ctx context.Context, store Store, lease app.Lease, runID string, seq, seed int, subjectCommitOID, subjectTreeOID, state string, now time.Time) (taskID string, err error) {
	taskID = uid(seed)
	txErr := withUOW(ctx, store, lease, func(uow app.UnitOfWork) error {
		wf, wfErr := app.RequireWorkflowRepositories(uow, "hopfixtures.SeedReviewTask")
		if wfErr != nil {
			return fmt.Errorf("hopfixtures: %w", wfErr)
		}
		task := run.NewReviewTask(identity.TaskID(taskID), identity.RunID(runID), seq, subjectCommitOID, subjectTreeOID, now)
		task.State = run.TaskState(state)
		if _, createErr := wf.TaskIndex().Create(ctx, task); createErr != nil {
			return fmt.Errorf("hopfixtures: create review task: %w", createErr)
		}
		return nil
	})
	if txErr != nil {
		return "", txErr
	}
	return taskID, nil
}

// SeedChildSession reserves an attempt on taskID and creates a launched,
// active child session of managerID with role ("implementer" or
// "reviewer"), bound by a current runtime binding. The returned attempt
// is left in its freshly created "reserved" state; call RunAttempt to
// drive it running (SubmitReviewVerdict and CreateTask/RequestRetry's
// currency checks want a running attempt behind an active reviewer or
// implementer, exactly as internal/adapters/sqlite's own review fixture
// drives it).
func SeedChildSession(ctx context.Context, store Store, lease app.Lease, runID, taskID, managerID, role string, seed int, now time.Time) (sessionID, incarnationID, attemptID string, err error) {
	attemptID = uid(seed)
	sessionID = uid(seed + 1)
	incarnationID = uid(seed + 2)
	txErr := withUOW(ctx, store, lease, func(uow app.UnitOfWork) error {
		wf, wfErr := app.RequireWorkflowRepositories(uow, "hopfixtures.SeedChildSession")
		if wfErr != nil {
			return fmt.Errorf("hopfixtures: %w", wfErr)
		}
		attempts, listErr := wf.AttemptIndex().ByTask(ctx, identity.TaskID(taskID))
		if listErr != nil {
			return fmt.Errorf("hopfixtures: list attempts: %w", listErr)
		}
		attempt, attemptErr := run.NewAttempt(identity.AttemptID(attemptID), identity.TaskID(taskID), len(attempts)+1, now)
		if attemptErr != nil {
			return fmt.Errorf("hopfixtures: new attempt: %w", attemptErr)
		}
		if _, createErr := wf.AttemptIndex().Create(ctx, attempt); createErr != nil {
			return fmt.Errorf("hopfixtures: create attempt: %w", createErr)
		}
		manager, _, getErr := uow.Sessions().Get(ctx, identity.SessionID(managerID))
		if getErr != nil {
			return fmt.Errorf("hopfixtures: get manager session: %w", getErr)
		}
		session, sessionErr := run.NewChildSession(identity.SessionID(sessionID), identity.RunID(runID), identity.AttemptID(attemptID), run.Role(role), manager, run.HarnessClaude, now)
		if sessionErr != nil {
			return fmt.Errorf("hopfixtures: new child session: %w", sessionErr)
		}
		if session, sessionErr = session.Launch(now); sessionErr != nil {
			return fmt.Errorf("hopfixtures: launch child session: %w", sessionErr)
		}
		if session, sessionErr = session.ConfirmActive(now); sessionErr != nil {
			return fmt.Errorf("hopfixtures: activate child session: %w", sessionErr)
		}
		if _, createErr := uow.Sessions().Create(ctx, session); createErr != nil {
			return fmt.Errorf("hopfixtures: create child session: %w", createErr)
		}
		binding := run.NewRuntimeBinding(
			identity.SessionID(sessionID), identity.IncarnationID(incarnationID),
			fixtureSocketPath, fixtureServerInstance, "fixture-workspace-"+sessionID, "fixture-tab-"+sessionID, "fixture-pane-"+sessionID,
			"fixture-label-"+sessionID, run.LaunchInitial, now,
		)
		if err := uow.Bindings().Create(ctx, binding); err != nil {
			return fmt.Errorf("hopfixtures: create child binding: %w", err)
		}
		return nil
	})
	if txErr != nil {
		return "", "", "", txErr
	}
	return sessionID, incarnationID, attemptID, nil
}

// SeedInterruptedAttempt reserves the next attempt on taskID and records
// it straight in its terminal "interrupted" state (reserved ->
// interrupted is a legal transition): the terminal prior attempt a
// manager's hop task retry needs behind a needs-rework task. Seed the
// task itself with SeedImplementTask(..., "needs-rework", ...).
func SeedInterruptedAttempt(ctx context.Context, store Store, lease app.Lease, taskID string, seed int, now time.Time) (attemptID string, err error) {
	attemptID = uid(seed)
	txErr := withUOW(ctx, store, lease, func(uow app.UnitOfWork) error {
		wf, wfErr := app.RequireWorkflowRepositories(uow, "hopfixtures.SeedInterruptedAttempt")
		if wfErr != nil {
			return fmt.Errorf("hopfixtures: %w", wfErr)
		}
		attempts, listErr := wf.AttemptIndex().ByTask(ctx, identity.TaskID(taskID))
		if listErr != nil {
			return fmt.Errorf("hopfixtures: list attempts: %w", listErr)
		}
		attempt, attemptErr := run.NewAttempt(identity.AttemptID(attemptID), identity.TaskID(taskID), len(attempts)+1, now)
		if attemptErr != nil {
			return fmt.Errorf("hopfixtures: new attempt: %w", attemptErr)
		}
		interrupted, trErr := attempt.Interrupt(now)
		if trErr != nil {
			return fmt.Errorf("hopfixtures: interrupt attempt: %w", trErr)
		}
		if _, createErr := wf.AttemptIndex().Create(ctx, interrupted); createErr != nil {
			return fmt.Errorf("hopfixtures: create attempt: %w", createErr)
		}
		return nil
	})
	if txErr != nil {
		return "", txErr
	}
	return attemptID, nil
}

// RunAttempt drives attemptID from its freshly reserved state through
// launching to running (the two transitions SeedChildSession leaves
// undone).
func RunAttempt(ctx context.Context, store Store, lease app.Lease, attemptID string, now time.Time) error {
	return withUOW(ctx, store, lease, func(uow app.UnitOfWork) error {
		if err := launchAttempt(ctx, uow, attemptID, now); err != nil {
			return err
		}
		return markAttemptRunning(ctx, uow, attemptID, now)
	})
}

// SeedIntegratedTask seeds one implement task (sequence seq) of managerID's
// running feature run whose accepted result was integrated: the task
// active with a running implementer attempt (SeedChildSession,
// RunAttempt), the result accepted through store.SubmitResult (commitOID),
// then an integration row driven merging, checking and integrated over
// premergeOID and mergeOID, and the task left integrated — the validated
// integration head worktree retirement detects. Seeds occupy seed through
// seed+21.
func SeedIntegratedTask(ctx context.Context, store Store, lease app.Lease, runID, managerID string, seq, seed int, commitOID, premergeOID, mergeOID string, now time.Time) (taskID, attemptID string, err error) {
	taskID, err = SeedImplementTask(ctx, store, lease, runID, seq, seed, fmt.Sprintf("integrated task %d", seq), string(run.TaskActive), now)
	if err != nil {
		return "", "", err
	}
	_, incarnationID, attemptID, err := SeedChildSession(ctx, store, lease, runID, taskID, managerID, string(run.RoleImplementer), seed+10, now)
	if err != nil {
		return "", "", err
	}
	if runErr := RunAttempt(ctx, store, lease, attemptID, now); runErr != nil {
		return "", "", runErr
	}
	resultID := uid(seed + 20)
	outcome, err := store.SubmitResult(ctx, app.ResultSubmission{
		ID: identity.ResultID(resultID), RunID: identity.RunID(runID), TaskID: identity.TaskID(taskID),
		AttemptID: identity.AttemptID(attemptID), IncarnationID: identity.IncarnationID(incarnationID),
		CommitOID: commitOID, Summary: "fixture result", Digest: "fixture-digest-" + resultID,
	})
	if err != nil {
		return "", "", fmt.Errorf("hopfixtures: submit result: %w", err)
	}
	if outcome.Kind != app.SubmissionAccepted {
		return "", "", fmt.Errorf("hopfixtures: submit result: %s (%s)", outcome.Kind, outcome.Detail)
	}
	txErr := withUOW(ctx, store, lease, func(uow app.UnitOfWork) error {
		wf, wfErr := app.RequireWorkflowRepositories(uow, "hopfixtures.SeedIntegratedTask")
		if wfErr != nil {
			return fmt.Errorf("hopfixtures: %w", wfErr)
		}
		integration := run.NewIntegration(identity.IntegrationID(uid(seed+21)), identity.RunID(runID), identity.TaskID(taskID), identity.ResultID(resultID), commitOID, premergeOID, now)
		if _, createErr := wf.Integrations().Create(ctx, integration); createErr != nil {
			return fmt.Errorf("hopfixtures: create integration: %w", createErr)
		}
		checking, trErr := integration.EnterChecking(mergeOID, now)
		if trErr != nil {
			return fmt.Errorf("hopfixtures: integration checking: %w", trErr)
		}
		integrated, trErr := checking.Integrate(now)
		if trErr != nil {
			return fmt.Errorf("hopfixtures: integrate: %w", trErr)
		}
		if _, saveErr := wf.Integrations().Save(ctx, integrated, 1); saveErr != nil {
			return fmt.Errorf("hopfixtures: save integration: %w", saveErr)
		}
		task, revision, getErr := uow.Tasks().Get(ctx, identity.TaskID(taskID))
		if getErr != nil {
			return fmt.Errorf("hopfixtures: get task: %w", getErr)
		}
		task.State = run.TaskIntegrated
		if _, saveErr := uow.Tasks().Save(ctx, task, revision); saveErr != nil {
			return fmt.Errorf("hopfixtures: save integrated task: %w", saveErr)
		}
		return nil
	})
	if txErr != nil {
		return "", "", txErr
	}
	return taskID, attemptID, nil
}

// SeedAttemptWorktree records attemptID's worktree exactly as the
// assignment act does after Herdr's worktree.create succeeds: a succeeded
// worktree.create operation whose intent (repository root, requested
// branch, base commit, attempt) and act evidence (the reported path and
// branch, the verified base) are in the store's persisted JSON shape, and
// the worktree row linked to the attempt and base in the same unit of
// work. branch is the requested short name (hop/r<seq>/t<n>a<m>); path is
// the spelling Herdr reported. Seeds occupy seed and seed+1.
func SeedAttemptWorktree(ctx context.Context, store Store, lease app.Lease, runID, attemptID string, seed int, repositoryRoot, path, branch, baseOID string, now time.Time) (worktreeID string, err error) {
	worktreeID = uid(seed)
	txErr := withUOW(ctx, store, lease, func(uow app.UnitOfWork) error {
		current, _, getErr := uow.Runs().Get(ctx, identity.RunID(runID))
		if getErr != nil {
			return fmt.Errorf("hopfixtures: get run: %w", getErr)
		}
		if opErr := uow.Operations().Create(ctx, app.Operation{
			ID: identity.OperationID(uid(seed + 1)), RunID: identity.RunID(runID), Generation: lease.Generation,
			Kind: app.OpWorktreeCreate, State: app.OperationSucceeded,
			Intent: map[string]any{
				"repository_root": repositoryRoot, "branch": branch, "base_ref": baseOID, "attempt_id": attemptID,
			},
			ActEvidence: map[string]any{
				"info":        map[string]any{"WorkspaceID": "fixture-workspace-" + attemptID, "Path": path, "Branch": branch},
				"base_commit": baseOID,
			},
			CreatedAt: now, UpdatedAt: now,
		}); opErr != nil {
			return fmt.Errorf("hopfixtures: create worktree.create operation: %w", opErr)
		}
		worktree, wtErr := run.NewAttemptWorktree(identity.WorktreeID(worktreeID), current.RepositoryID, identity.RunID(runID), identity.AttemptID(attemptID), baseOID, path, branch)
		if wtErr != nil {
			return fmt.Errorf("hopfixtures: new attempt worktree: %w", wtErr)
		}
		if _, createErr := uow.Worktrees().Create(ctx, worktree); createErr != nil {
			return fmt.Errorf("hopfixtures: create attempt worktree: %w", createErr)
		}
		return nil
	})
	if txErr != nil {
		return "", txErr
	}
	return worktreeID, nil
}

// FinishRun drives runID from running to state — "completed" (through
// completing), "failed", or "stopped" (a stop request, then stopping) —
// and releases lease, leaving the run exactly as a finished controller
// does: terminal, with no controller holding it.
func FinishRun(ctx context.Context, store Store, lease app.Lease, runID, state string, now time.Time) error {
	if err := withUOW(ctx, store, lease, func(uow app.UnitOfWork) error {
		current, revision, err := uow.Runs().Get(ctx, identity.RunID(runID))
		if err != nil {
			return fmt.Errorf("hopfixtures: get run: %w", err)
		}
		var next run.Run
		switch run.RunState(state) {
		case run.RunCompleted:
			if next, err = current.EnterCompleting(now); err == nil {
				next, err = next.Complete(now)
			}
		case run.RunFailed:
			next, err = current.Fail(now)
		case run.RunStopped:
			next, err = current.RequestStop(now).MarkStopped(now)
		default:
			return fmt.Errorf("hopfixtures: %q is not a terminal run state", state)
		}
		if err != nil {
			return fmt.Errorf("hopfixtures: finish run: %w", err)
		}
		if _, err := uow.Runs().Save(ctx, next, revision); err != nil {
			return fmt.Errorf("hopfixtures: save finished run: %w", err)
		}
		return nil
	}); err != nil {
		return err
	}
	if err := store.ReleaseLease(ctx, lease); err != nil {
		return fmt.Errorf("hopfixtures: release lease: %w", err)
	}
	return nil
}

func launchRun(ctx context.Context, uow app.UnitOfWork, runID string, now time.Time) error {
	v, rev, err := uow.Runs().Get(ctx, identity.RunID(runID))
	if err != nil {
		return fmt.Errorf("hopfixtures: get run: %w", err)
	}
	next, err := v.Launch(now)
	if err != nil {
		return fmt.Errorf("hopfixtures: launch run: %w", err)
	}
	if _, err := uow.Runs().Save(ctx, next, rev); err != nil {
		return fmt.Errorf("hopfixtures: save run: %w", err)
	}
	return nil
}

func markRunRunning(ctx context.Context, uow app.UnitOfWork, runID string, now time.Time) error {
	v, rev, err := uow.Runs().Get(ctx, identity.RunID(runID))
	if err != nil {
		return fmt.Errorf("hopfixtures: get run: %w", err)
	}
	next, err := v.MarkRunning(now)
	if err != nil {
		return fmt.Errorf("hopfixtures: mark run running: %w", err)
	}
	if _, err := uow.Runs().Save(ctx, next, rev); err != nil {
		return fmt.Errorf("hopfixtures: save run: %w", err)
	}
	return nil
}

func activateTask(ctx context.Context, uow app.UnitOfWork, taskID string, now time.Time) error {
	v, rev, err := uow.Tasks().Get(ctx, identity.TaskID(taskID))
	if err != nil {
		return fmt.Errorf("hopfixtures: get task: %w", err)
	}
	next, err := v.Activate(now)
	if err != nil {
		return fmt.Errorf("hopfixtures: activate task: %w", err)
	}
	if _, err := uow.Tasks().Save(ctx, next, rev); err != nil {
		return fmt.Errorf("hopfixtures: save task: %w", err)
	}
	return nil
}

func launchAttempt(ctx context.Context, uow app.UnitOfWork, attemptID string, now time.Time) error {
	v, rev, err := uow.Attempts().Get(ctx, identity.AttemptID(attemptID))
	if err != nil {
		return fmt.Errorf("hopfixtures: get attempt: %w", err)
	}
	next, err := v.Launch(now)
	if err != nil {
		return fmt.Errorf("hopfixtures: launch attempt: %w", err)
	}
	if _, err := uow.Attempts().Save(ctx, next, rev); err != nil {
		return fmt.Errorf("hopfixtures: save attempt: %w", err)
	}
	return nil
}

func markAttemptRunning(ctx context.Context, uow app.UnitOfWork, attemptID string, now time.Time) error {
	v, rev, err := uow.Attempts().Get(ctx, identity.AttemptID(attemptID))
	if err != nil {
		return fmt.Errorf("hopfixtures: get attempt: %w", err)
	}
	next, err := v.MarkRunning(now)
	if err != nil {
		return fmt.Errorf("hopfixtures: mark attempt running: %w", err)
	}
	if _, err := uow.Attempts().Save(ctx, next, rev); err != nil {
		return fmt.Errorf("hopfixtures: save attempt: %w", err)
	}
	return nil
}

func launchSession(ctx context.Context, uow app.UnitOfWork, sessionID string, now time.Time) error {
	v, rev, err := uow.Sessions().Get(ctx, identity.SessionID(sessionID))
	if err != nil {
		return fmt.Errorf("hopfixtures: get session: %w", err)
	}
	next, err := v.Launch(now)
	if err != nil {
		return fmt.Errorf("hopfixtures: launch session: %w", err)
	}
	if _, err := uow.Sessions().Save(ctx, next, rev); err != nil {
		return fmt.Errorf("hopfixtures: save session: %w", err)
	}
	return nil
}

func activateSession(ctx context.Context, uow app.UnitOfWork, sessionID string, now time.Time) error {
	v, rev, err := uow.Sessions().Get(ctx, identity.SessionID(sessionID))
	if err != nil {
		return fmt.Errorf("hopfixtures: get session: %w", err)
	}
	next, err := v.ConfirmActive(now)
	if err != nil {
		return fmt.Errorf("hopfixtures: activate session: %w", err)
	}
	if _, err := uow.Sessions().Save(ctx, next, rev); err != nil {
		return fmt.Errorf("hopfixtures: save session: %w", err)
	}
	return nil
}
