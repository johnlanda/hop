package sqlite_test

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/johnlanda/hop/internal/adapters/sqlite"
	"github.com/johnlanda/hop/internal/app"
	"github.com/johnlanda/hop/internal/domain/identity"
	"github.com/johnlanda/hop/internal/domain/run"
)

// fakeClock is a hand-advanced clock shared across store handles, so lease
// expiry is decided by explicit Advance calls instead of sleeps.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 9, 14, 10, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// openStoreAt opens a store handle on root, closed by t.Cleanup. Separate
// handles onto the same root stand in for separate processes.
func openStoreAt(t *testing.T, root string, clock app.Clock) *sqlite.Store {
	t.Helper()
	store, err := sqlite.Open(t.Context(), root, sqlite.Options{Clock: clock})
	if err != nil {
		t.Fatalf("open store at %s: %v", root, err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})
	return store
}

// uid renders a deterministic canonical lowercase UUID from a test-chosen
// number, so identities are readable in failures.
func uid(n int) string {
	return fmt.Sprintf("00000000-0000-4000-8000-%012d", n)
}

// Identity offsets within one spec's uid block.
const (
	offRun = iota + 1
	offTask
	offAttempt
	offSession
	offWorktree
	offIncarnation
	offNativeRef
	specStride = 100
)

// newSpec builds a complete NewRunSpec whose identities occupy the uid
// block starting at base (use multiples of specStride).
func newSpec(repoRoot string, base int, now time.Time) app.NewRunSpec {
	return app.NewRunSpec{
		RepositoryRoot:     repoRoot,
		RunID:              identity.RunID(uid(base + offRun)),
		TaskID:             identity.TaskID(uid(base + offTask)),
		AttemptID:          identity.AttemptID(uid(base + offAttempt)),
		SessionID:          identity.SessionID(uid(base + offSession)),
		WorktreeID:         identity.WorktreeID(uid(base + offWorktree)),
		IncarnationID:      identity.IncarnationID(uid(base + offIncarnation)),
		Brief:              "make the fixture pass",
		BriefDigest:        "brief-digest",
		InstructionsDigest: "instructions-digest",
		Snapshot: app.RunSnapshot{
			CheckArgv:        []string{"sh", "check.sh"},
			CheckTimeout:     10 * time.Minute,
			CheckRepeatable:  false,
			EnvPolicy:        app.EnvPolicy{Version: app.EnvPolicyVersion1, Harness: app.HarnessClaude},
			Harness:          "claude",
			StateRoot:        "/state/root",
			AssignmentPath:   "/state/root/runs/" + uid(base+offRun) + "/artifacts/assignment.md",
			AssignmentDigest: "assignment-digest",
		},
		Harness:          run.HarnessClaude,
		NativeSessionRef: uid(base + offNativeRef),
		ControllerID:     "controller-a",
		Now:              now,
	}
}

// fixture is one initialized run with its store, clock and lease. opSeq
// numbers the operations the fixture's helpers create.
type fixture struct {
	store *sqlite.Store
	clock *fakeClock
	spec  app.NewRunSpec
	lease app.Lease
	opSeq int
}

// newFixture opens a fresh store in a temp root and initializes one run.
func newFixture(t *testing.T) *fixture {
	t.Helper()
	clock := newFakeClock()
	store := openStoreAt(t, t.TempDir(), clock)
	spec := newSpec("/repos/alpha", specStride, clock.Now())
	_, lease, err := store.InitializeRun(t.Context(), spec)
	if err != nil {
		t.Fatalf("InitializeRun: %v", err)
	}
	return &fixture{store: store, clock: clock, spec: spec, lease: lease}
}

// inUOW runs fn inside one committed unit of work under the fixture lease.
func (f *fixture) inUOW(t *testing.T, fn func(uow app.UnitOfWork)) {
	t.Helper()
	uow, err := f.store.Begin(t.Context(), f.lease)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	fn(uow)
	if err := uow.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
}

// createBinding records the fixture incarnation's runtime binding, making
// it the attempt's current identity.
func (f *fixture) createBinding(t *testing.T) {
	t.Helper()
	f.inUOW(t, func(uow app.UnitOfWork) {
		binding := run.NewRuntimeBinding(
			f.spec.SessionID, f.spec.IncarnationID,
			"/tmp/herdr.sock", "server-instance-1", "workspace-1", "tab-1", "pane-1",
			uid(9001), run.LaunchInitial, f.clock.Now(),
		)
		if err := uow.Bindings().Create(t.Context(), binding); err != nil {
			t.Fatalf("create binding: %v", err)
		}
	})
}

// launchAttempt journals the launch: run launching, task active, attempt
// launching, session launching.
func (f *fixture) launchAttempt(t *testing.T) {
	t.Helper()
	now := f.clock.Now()
	f.inUOW(t, func(uow app.UnitOfWork) {
		ctx := t.Context()
		saveRun(t, uow, f.spec.RunID, func(v run.Run) (run.Run, error) { return v.Launch(now) })
		task, revision, err := uow.Tasks().Get(ctx, f.spec.TaskID)
		if err != nil {
			t.Fatalf("get task: %v", err)
		}
		next, err := task.Activate(now)
		if err != nil {
			t.Fatalf("activate task: %v", err)
		}
		if _, err := uow.Tasks().Save(ctx, next, revision); err != nil {
			t.Fatalf("save task: %v", err)
		}
		saveAttempt(t, uow, f.spec.AttemptID, func(v run.Attempt) (run.Attempt, error) { return v.Launch(now) })
		saveSession(t, uow, f.spec.SessionID, func(v run.Session) (run.Session, error) { return v.Launch(now) })
	})
}

// fixturePID is the launcher pid every fixture claim records.
const fixturePID = 111

// createLaunchIntent commits a pending pane.open launch operation whose
// intent JSON carries the session and incarnation under the documented
// "session_id" and "incarnation_id" keys — the controller's pre-dispatch
// write that ClaimLaunch's pre-binding currency rule reads.
func (f *fixture) createLaunchIntent(t *testing.T, session identity.SessionID, incarnation identity.IncarnationID) {
	t.Helper()
	f.opSeq++
	opID := identity.OperationID(uid(9500 + f.opSeq))
	f.inUOW(t, func(uow app.UnitOfWork) {
		err := uow.Operations().Create(t.Context(), app.Operation{
			ID:         opID,
			RunID:      f.spec.RunID,
			Generation: f.lease.Generation,
			Kind:       app.OpPaneOpen,
			State:      app.OperationPending,
			Intent: map[string]any{
				"session_id":     session.String(),
				"incarnation_id": incarnation.String(),
				"creation_label": uid(9001),
			},
			CreatedAt: f.clock.Now(),
			UpdatedAt: f.clock.Now(),
		})
		if err != nil {
			t.Fatalf("create launch intent: %v", err)
		}
	})
}

// createMalformedLaunchIntent commits a pending pane.open launch operation
// whose intent JSON names session under "session_id" but carries a
// non-UUID string under "incarnation_id", so the store's parse of the
// intent identity fails closed.
func (f *fixture) createMalformedLaunchIntent(t *testing.T, session identity.SessionID, rawIncarnation string) {
	t.Helper()
	f.opSeq++
	opID := identity.OperationID(uid(9600 + f.opSeq))
	f.inUOW(t, func(uow app.UnitOfWork) {
		err := uow.Operations().Create(t.Context(), app.Operation{
			ID:         opID,
			RunID:      f.spec.RunID,
			Generation: f.lease.Generation,
			Kind:       app.OpPaneOpen,
			State:      app.OperationPending,
			Intent: map[string]any{
				"session_id":     session.String(),
				"incarnation_id": rawIncarnation,
				"creation_label": uid(9001),
			},
			CreatedAt: f.clock.Now(),
			UpdatedAt: f.clock.Now(),
		})
		if err != nil {
			t.Fatalf("create malformed launch intent: %v", err)
		}
	})
}

// fixtureSeedEvidence is the workspace-trust seed evidence claimLaunch
// records, asserted round-tripped by the read-store tests.
const fixtureSeedEvidence = "workspace trust seeded for /worktrees/fixture"

// claimLaunch writes the incarnation's exec_pending launch claim under
// fixturePID.
func (f *fixture) claimLaunch(t *testing.T) {
	t.Helper()
	err := f.store.ClaimLaunch(t.Context(), app.LaunchClaim{
		IncarnationID: f.spec.IncarnationID,
		RunID:         f.spec.RunID,
		AttemptID:     f.spec.AttemptID,
		Executable:    "/opt/harness/claude",
		ArgvDigest:    "argv-digest",
		PID:           fixturePID,
		SeedEvidence:  fixtureSeedEvidence,
	})
	if err != nil {
		t.Fatalf("ClaimLaunch: %v", err)
	}
}

// settleClaimExeced settles the incarnation's claim to execed through the
// controller-side repository, corroborated at fixturePID.
func (f *fixture) settleClaimExeced(t *testing.T) {
	t.Helper()
	f.inUOW(t, func(uow app.UnitOfWork) {
		err := uow.LaunchClaims().Settle(t.Context(), f.spec.IncarnationID, app.LaunchClaimSettlement{
			State:      app.LaunchClaimExeced,
			PaneID:     "pane-1",
			PID:        fixturePID,
			Executable: "/opt/harness/claude",
			ArgvMarker: f.spec.IncarnationID.String(),
			At:         f.clock.Now(),
		})
		if err != nil {
			t.Fatalf("settle claim execed: %v", err)
		}
	})
}

// markRunning applies the post-settlement lifecycle transitions: attempt
// running, session active, run running.
func (f *fixture) markRunning(t *testing.T) {
	t.Helper()
	now := f.clock.Now()
	f.inUOW(t, func(uow app.UnitOfWork) {
		saveAttempt(t, uow, f.spec.AttemptID, func(v run.Attempt) (run.Attempt, error) { return v.MarkRunning(now) })
		saveSession(t, uow, f.spec.SessionID, func(v run.Session) (run.Session, error) { return v.ConfirmActive(now) })
		saveRun(t, uow, f.spec.RunID, func(v run.Run) (run.Run, error) { return v.MarkRunning(now) })
	})
}

// runningFixture drives a fresh fixture all the way to a running worker:
// binding created, claim settled execed, lifecycle transitions applied.
func runningFixture(t *testing.T) *fixture {
	t.Helper()
	f := newFixture(t)
	f.launchAttempt(t)
	f.createBinding(t)
	f.claimLaunch(t)
	f.settleClaimExeced(t)
	f.markRunning(t)
	return f
}

// submission builds a ResultSubmission against the fixture's identities.
func (f *fixture) submission(resultN int, digest string) app.ResultSubmission {
	return app.ResultSubmission{
		ID:            identity.ResultID(uid(resultN)),
		RunID:         f.spec.RunID,
		TaskID:        f.spec.TaskID,
		AttemptID:     f.spec.AttemptID,
		IncarnationID: f.spec.IncarnationID,
		CommitOID:     strings.Repeat("a", 40),
		Summary:       "implemented the fixture change",
		Digest:        digest,
	}
}

// saveRun applies transition to the run and saves it inside uow.
func saveRun(t *testing.T, uow app.UnitOfWork, id identity.RunID, transition func(run.Run) (run.Run, error)) {
	t.Helper()
	v, revision, err := uow.Runs().Get(t.Context(), id)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	next, err := transition(v)
	if err != nil {
		t.Fatalf("transition run: %v", err)
	}
	if _, err := uow.Runs().Save(t.Context(), next, revision); err != nil {
		t.Fatalf("save run: %v", err)
	}
}

// saveAttempt applies transition to the attempt and saves it inside uow.
func saveAttempt(t *testing.T, uow app.UnitOfWork, id identity.AttemptID, transition func(run.Attempt) (run.Attempt, error)) {
	t.Helper()
	v, revision, err := uow.Attempts().Get(t.Context(), id)
	if err != nil {
		t.Fatalf("get attempt: %v", err)
	}
	next, err := transition(v)
	if err != nil {
		t.Fatalf("transition attempt: %v", err)
	}
	if _, err := uow.Attempts().Save(t.Context(), next, revision); err != nil {
		t.Fatalf("save attempt: %v", err)
	}
}

// saveTask applies transition to the task and saves it inside uow.
func saveTask(t *testing.T, uow app.UnitOfWork, id identity.TaskID, transition func(run.Task) (run.Task, error)) {
	t.Helper()
	v, revision, err := uow.Tasks().Get(t.Context(), id)
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	next, err := transition(v)
	if err != nil {
		t.Fatalf("transition task: %v", err)
	}
	if _, err := uow.Tasks().Save(t.Context(), next, revision); err != nil {
		t.Fatalf("save task: %v", err)
	}
}

// saveSession applies transition to the session and saves it inside uow.
func saveSession(t *testing.T, uow app.UnitOfWork, id identity.SessionID, transition func(run.Session) (run.Session, error)) {
	t.Helper()
	v, revision, err := uow.Sessions().Get(t.Context(), id)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	next, err := transition(v)
	if err != nil {
		t.Fatalf("transition session: %v", err)
	}
	if _, err := uow.Sessions().Save(t.Context(), next, revision); err != nil {
		t.Fatalf("save session: %v", err)
	}
}

// countRows counts table rows matching a WHERE clause through raw SQL.
func countRows(t *testing.T, store *sqlite.Store, query string, args ...any) int {
	t.Helper()
	var n int
	if err := sqlite.WriteDB(store).QueryRowContext(t.Context(), query, args...).Scan(&n); err != nil {
		t.Fatalf("count rows (%s): %v", query, err)
	}
	return n
}
