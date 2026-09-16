package app_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/johnlanda/hop/internal/app"
	"github.com/johnlanda/hop/internal/domain/identity"
	"github.com/johnlanda/hop/internal/domain/run"
)

const (
	integrationHopPath = "/usr/local/bin/hop"
	integrationRefName = "refs/heads/hop/r1/integration"
)

// integrationFixture bundles a feature run with one completed implement
// task, its accepted result, and a fake git repository behind the
// CommandRunner.
type integrationFixture struct {
	tc        *testController
	fr        featureRun
	git       *fakeGitRepo
	base      string
	src       string
	taskID    identity.TaskID
	attemptID identity.AttemptID
	resultID  identity.ResultID
}

// newIntegrationFixture seeds the fixture. srcIsAncestor makes the source
// commit an ancestor of the integration head (the no-op shape) instead of
// a divergent child.
func newIntegrationFixture(t *testing.T, srcIsAncestor bool) *integrationFixture {
	t.Helper()
	tc := newTestController(defaultPolicy())
	fr := seedFeatureRun(t, tc, 2)
	tc.Store.repoByRoot["/repo"] = tc.Store.Runs[fr.RunID].value.RepositoryID
	snap := tc.Store.Snapshots[fr.RunID]
	snap.CheckArgv = []string{"/bin/hopcheck"}
	tc.Store.Snapshots[fr.RunID] = snap

	git := newFakeGitRepo("/usr/bin/git", integrationHopPath)
	tc.Commands.RunHook = git.Hook
	t.Cleanup(func() { git.requireNoDerefUpdates(t) })

	var base, src string
	if srcIsAncestor {
		src = git.newCommit("tree-src")
		base = git.newCommit("tree-base", src)
	} else {
		base = git.newCommit("tree-base")
		src = git.newCommit("tree-src", base)
	}
	git.setRef(integrationRefName, base)

	taskID := seedImplementTask(t, tc, fr.RunID, 1, "A", false, run.TaskCompleted)
	attemptID, err := identity.ParseAttemptID(tc.IDs.NewID())
	if err != nil {
		t.Fatalf("parse attempt id: %v", err)
	}
	attempt, err := run.NewAttempt(attemptID, taskID, 1, tc.Clock.Now())
	if err != nil {
		t.Fatalf("NewAttempt() error = %v", err)
	}
	attempt.State = run.AttemptCompleted
	tc.Store.Attempts[attemptID] = &entityRow[run.Attempt]{value: attempt, revision: 1}
	resultID, err := identity.ParseResultID(tc.IDs.NewID())
	if err != nil {
		t.Fatalf("parse result id: %v", err)
	}
	tc.Store.Results[attemptID] = run.Result{
		ID: resultID, AttemptID: attemptID, CommitOID: src, Summary: "done",
		ContentDigest: "d", Accepted: true, SubmittedAt: tc.Clock.Now(),
	}
	return &integrationFixture{tc: tc, fr: fr, git: git, base: base, src: src, taskID: taskID, attemptID: attemptID, resultID: resultID}
}

func (f *integrationFixture) drive(t *testing.T) app.IntegrationReport {
	t.Helper()
	report, err := f.tc.Controller.DriveIntegration(context.Background(), f.fr.Handle, integrationHopPath, []string{"PATH=/usr/bin"})
	if err != nil {
		t.Fatalf("DriveIntegration() error = %v", err)
	}
	return report
}

// driveUntil loops DriveIntegration until the integration reports
// wantState, failing on a block or on no convergence.
func (f *integrationFixture) driveUntil(t *testing.T, wantState string, maxRounds int) app.IntegrationReport {
	t.Helper()
	var report app.IntegrationReport
	for round := 0; round < maxRounds; round++ {
		report = f.drive(t)
		if report.Blocked != "" {
			t.Fatalf("DriveIntegration() blocked: %s", report.Blocked)
		}
		if report.State == wantState {
			return report
		}
	}
	t.Fatalf("integration did not reach %s in %d rounds; last report %+v", wantState, maxRounds, report)
	return report
}

// currentIntegrationRow reads the run's single integration row directly.
func (f *integrationFixture) currentIntegrationRow(t *testing.T) run.Integration {
	t.Helper()
	for _, row := range f.tc.Store.Integrations {
		if row.value.RunID == f.fr.RunID {
			return row.value
		}
	}
	t.Fatalf("no integration row recorded")
	return run.Integration{}
}

// opsOfKind lists the run's operations of one kind, any state.
func (f *integrationFixture) opsOfKind(kind app.OperationKind) []app.Operation {
	var out []app.Operation
	for _, op := range f.tc.Store.Operations { //nolint:gocritic // rangeValCopy: test helper over a small map.
		if op.RunID == f.fr.RunID && op.Kind == kind {
			out = append(out, op)
		}
	}
	return out
}

// payloadField reads one field of a persisted payload generically.
func payloadField(t *testing.T, payload any, key string) string {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	return fmt.Sprint(m[key])
}

// managerNotices lists controller info messages to the manager address.
func (f *integrationFixture) managerNotices() []run.Message {
	var out []run.Message
	for _, m := range f.tc.Store.Messages { //nolint:gocritic // rangeValCopy: test helper over a small map.
		if m.RunID == f.fr.RunID && m.Sender.Kind == run.PrincipalController && m.Recipient.Kind == run.AddressManager {
			out = append(out, m)
		}
	}
	return out
}

// seedIntegrationRow writes an integration row directly in the given
// state, returning its id.
func (f *integrationFixture) seedIntegrationRow(t *testing.T, state run.IntegrationState, mergeOID string) identity.IntegrationID {
	t.Helper()
	id, err := identity.ParseIntegrationID(f.tc.IDs.NewID())
	if err != nil {
		t.Fatalf("parse integration id: %v", err)
	}
	integ := run.NewIntegration(id, f.fr.RunID, f.taskID, f.resultID, f.src, f.base, f.tc.Clock.Now())
	integ.State = state
	integ.MergeCommitOID = mergeOID
	f.tc.Store.Integrations[id] = &entityRow[run.Integration]{value: integ, revision: 1}
	// The task follows the claim: integrating while the slot is held.
	f.tc.Store.Tasks[f.taskID].value.State = run.TaskIntegrating
	return id
}

// seedOperation writes an operation row directly with a JSON-generic
// payload (what a real store returns after persistence).
func (f *integrationFixture) seedOperation(t *testing.T, kind app.OperationKind, state app.OperationState, intent, evidence map[string]any) identity.OperationID {
	t.Helper()
	opID, err := identity.ParseOperationID(f.tc.IDs.NewID())
	if err != nil {
		t.Fatalf("parse operation id: %v", err)
	}
	op := app.Operation{
		ID: opID, RunID: f.fr.RunID, Generation: f.tc.Store.Leases[f.fr.RunID].lease.Generation,
		Kind: kind, State: state, CreatedAt: f.tc.Clock.Now(), UpdatedAt: f.tc.Clock.Now(),
	}
	op.Intent = intent
	if evidence != nil {
		op.ActEvidence = evidence
	}
	f.tc.Store.Operations[opID] = op
	return opID
}

func TestDriveIntegrationHappyPath(t *testing.T) {
	f := newIntegrationFixture(t, false)
	taskB := seedImplementTask(t, f.tc, f.fr.RunID, 2, "B", true, run.TaskPending)
	edge, err := run.NewTaskDependency(taskB, f.taskID, f.tc.Clock.Now())
	if err != nil {
		t.Fatalf("NewTaskDependency() error = %v", err)
	}
	f.tc.Store.TaskDependencies = append(f.tc.Store.TaskDependencies, edge)

	first := f.drive(t)
	if !first.Claimed || first.State != string(run.IntegrationMerging) {
		t.Fatalf("first round = %+v, want a fresh claim in merging", first)
	}
	if got := f.tc.Store.Tasks[f.taskID].value.State; got != run.TaskIntegrating {
		t.Fatalf("task state after claim = %s, want integrating", got)
	}

	report := f.driveUntil(t, string(run.IntegrationIntegrated), 6)
	if report.State != string(run.IntegrationIntegrated) {
		t.Fatalf("state = %s, want integrated", report.State)
	}

	head := f.git.ref(integrationRefName)
	if head == f.base {
		t.Fatalf("integration ref never moved off the pre-merge head")
	}
	parents := f.git.parentsOf(head)
	sortedParents := append([]string(nil), parents...)
	wantParents := []string{f.base, f.src}
	slices.Sort(sortedParents)
	slices.Sort(wantParents)
	if !slices.Equal(sortedParents, wantParents) {
		t.Fatalf("merge commit parents = %v, want {pre-merge head, source}", parents)
	}
	if got := f.tc.Store.Tasks[f.taskID].value.State; got != run.TaskIntegrated {
		t.Fatalf("task state = %s, want integrated", got)
	}
	if got := f.tc.Store.Tasks[taskB].value.State; got != run.TaskReady {
		t.Fatalf("dependent task state = %s, want ready (released in the integrating transaction)", got)
	}
	integ := f.currentIntegrationRow(t)
	if integ.MergeCommitOID != head {
		t.Fatalf("integration MergeCommitOID = %s, want the published head %s", integ.MergeCommitOID, head)
	}
	if notices := f.managerNotices(); len(notices) != 1 {
		t.Fatalf("manager notices = %d, want exactly one (committed with the integrated transition)", len(notices))
	}

	// The frozen merge argv: noninteractive, signing off, repo-local
	// hooks suppressed via core.hooksPath under the operation's artifact
	// dir (human decision Q5(a)).
	var mergeSpawn []string
	for _, call := range f.tc.Commands.Calls {
		if len(call.Argv) > 2 && call.Argv[1] == "check-exec" && strings.Contains(strings.Join(call.Argv, " "), " merge --no-ff --no-edit ") {
			mergeSpawn = call.Argv
		}
	}
	if mergeSpawn == nil {
		t.Fatalf("no merge spawn through the exec boundary was recorded")
	}
	joined := strings.Join(mergeSpawn, " ")
	for _, want := range []string{
		"-c user.name=hop", "-c user.email=hop@invalid", "-c core.editor=true",
		"-c commit.gpgsign=false", "-c merge.verifysignatures=false",
		"-c core.hooksPath=", "merge --no-ff --no-edit " + f.src,
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("merge spawn argv %q lacks %q", joined, want)
		}
	}
	hooksSeen := false
	for path := range f.tc.Artifacts.files {
		if strings.Contains(path, "/hooks/.keep") {
			hooksSeen = true
		}
	}
	if !hooksSeen {
		t.Errorf("the empty hooks directory was never created under the operation's artifact dir")
	}
}

func TestDriveIntegrationNoOpAdoption(t *testing.T) {
	t.Run("the ancestor no-op publishes nothing and checks the unchanged head", func(t *testing.T) {
		f := newIntegrationFixture(t, true)
		f.git.MergeOutcome = "no-op"

		f.drive(t) // claim
		f.drive(t) // merge -> no-op -> checking
		integ := f.currentIntegrationRow(t)
		if integ.State != run.IntegrationChecking || integ.MergeCommitOID != f.base {
			t.Fatalf("after no-op merge: state=%s candidate=%s, want checking on the unchanged head %s", integ.State, integ.MergeCommitOID, f.base)
		}
		report := f.driveUntil(t, string(run.IntegrationIntegrated), 3)
		if report.State != string(run.IntegrationIntegrated) {
			t.Fatalf("state = %s, want integrated", report.State)
		}
		if head := f.git.ref(integrationRefName); head != f.base {
			t.Fatalf("ref = %s, want the unchanged head %s (no publish ever runs for a no-op)", head, f.base)
		}
		if len(f.git.UpdateRefCalls) != 0 {
			t.Fatalf("update-ref was called %v; a no-op never publishes", f.git.UpdateRefCalls)
		}
		if ops := f.opsOfKind(app.OpIntegrationPublish); len(ops) != 0 {
			t.Fatalf("publish operations = %d, want none", len(ops))
		}
	})

	t.Run("an existing settled receipt for exactly the unchanged head satisfies the check", func(t *testing.T) {
		f := newIntegrationFixture(t, true)
		f.git.MergeOutcome = "no-op"
		f.seedOperation(t, app.OpCheckRun, app.OperationSucceeded, map[string]any{
			"integration_id":     "prior-integration",
			"subject_commit_oid": f.base,
			"subject_tree_oid":   "tree-base",
			"check_argv":         []any{"/bin/hopcheck"},
		}, nil)
		// The settled receipt needs its outcome recorded too.
		for id, op := range f.tc.Store.Operations {
			if op.Kind == app.OpCheckRun && op.State == app.OperationSucceeded {
				op.Outcome = map[string]any{"exit_code": float64(0)}
				f.tc.Store.Operations[id] = op
			}
		}

		f.drive(t) // claim
		f.drive(t) // merge -> no-op -> checking
		spawnsBefore := f.git.CheckExecCalls
		report := f.driveUntil(t, string(run.IntegrationIntegrated), 3)
		if report.State != string(run.IntegrationIntegrated) {
			t.Fatalf("state = %s, want integrated", report.State)
		}
		if f.git.CheckExecCalls != spawnsBefore {
			t.Fatalf("a fresh check execution was spawned despite the settled receipt for this exact head")
		}
	})
}

func TestDriveIntegrationConflict(t *testing.T) {
	t.Run("a conflict settles the integration and the task needs rework, evidence retained", func(t *testing.T) {
		f := newIntegrationFixture(t, false)
		f.git.MergeOutcome = "conflict"

		f.drive(t) // claim
		f.drive(t) // merge -> conflict

		integ := f.currentIntegrationRow(t)
		if integ.State != run.IntegrationConflicted {
			t.Fatalf("integration state = %s, want conflicted", integ.State)
		}
		if got := f.tc.Store.Tasks[f.taskID].value.State; got != run.TaskNeedsRework {
			t.Fatalf("task state = %s, want needs-rework with budget room", got)
		}
		if f.tc.Store.Tasks[f.taskID].value.MailboxClosed {
			t.Fatalf("mailbox closed on a retriable interruption; needs-rework leaves it OPEN")
		}
		if head := f.git.ref(integrationRefName); head != f.base {
			t.Fatalf("ref = %s, want untouched %s (a conflict never touches the ref)", head, f.base)
		}
		var evidence int
		for _, a := range f.tc.Store.Artifacts {
			if a.Kind == run.ArtifactCheckStdout || a.Kind == run.ArtifactCheckStderr {
				evidence++
			}
		}
		if evidence != 2 {
			t.Fatalf("retained evidence rows = %d, want stdout and stderr", evidence)
		}
		notices := f.managerNotices()
		if len(notices) != 1 {
			t.Fatalf("manager notices = %d, want one", len(notices))
		}
		body := f.tc.Artifacts.files[notices[0].BodyPath]
		if !strings.Contains(string(body), "conflicted") || !strings.Contains(string(body), "needs-rework") {
			t.Fatalf("notice body %q lacks the settlement facts", string(body))
		}
		// The conflicted scratch tree is retained as evidence.
		mergeOps := f.opsOfKind(app.OpIntegrationMerge)
		if len(mergeOps) != 1 {
			t.Fatalf("merge operations = %d, want one", len(mergeOps))
		}
		treePath := payloadField(t, mergeOps[0].Intent, "tree_path")
		if _, ok := f.git.worktrees[treePath]; !ok {
			t.Fatalf("the conflicted scratch tree at %s was removed; it is evidence until settlement", treePath)
		}
	})

	t.Run("at the exhausted retry limit the task fails and its mailbox closes with obligations journaled", func(t *testing.T) {
		f := newIntegrationFixture(t, false)
		f.git.MergeOutcome = "conflict"
		snap := f.tc.Store.Snapshots[f.fr.RunID]
		snap.Workflow.RetryLimit = 1
		f.tc.Store.Snapshots[f.fr.RunID] = snap

		// A pending obligation addressed to the failing task.
		msgID, err := identity.ParseMessageID(f.tc.IDs.NewID())
		if err != nil {
			t.Fatalf("parse message id: %v", err)
		}
		pending := run.NewInfo(msgID, f.fr.RunID, run.ControllerPrincipal(), run.TaskAddress(f.taskID), "", "/state/m", "d", 1, 1, f.tc.Clock.Now())
		f.tc.Store.Messages[msgID] = pending

		f.drive(t)
		f.drive(t)

		task := f.tc.Store.Tasks[f.taskID].value
		if task.State != run.TaskFailed {
			t.Fatalf("task state = %s, want failed at the exhausted limit (never parked needs-rework)", task.State)
		}
		if !task.MailboxClosed {
			t.Fatalf("mailbox open after failure settlement; failure closes admission in the same commit")
		}
		notices := f.managerNotices()
		if len(notices) != 1 {
			t.Fatalf("manager notices = %d, want one", len(notices))
		}
		body := string(f.tc.Artifacts.files[notices[0].BodyPath])
		if !strings.Contains(body, "orphaned obligations: "+msgID.String()) {
			t.Fatalf("notice body %q does not name the orphaned obligation %s", body, msgID)
		}
	})
}

func TestIntegrationMergeCrashColumns(t *testing.T) {
	mergeIntent := func(f *integrationFixture, integrationID identity.IntegrationID, treePath string) map[string]any {
		mergeArgv := []any{"/usr/bin/git", "-C", treePath, "merge", "--no-ff", "--no-edit", f.src}
		return map[string]any{
			"integration_id": integrationID.String(),
			"premerge_oid":   f.base,
			"source_oid":     f.src,
			"tree_path":      treePath,
			"hooks_path":     treePath + "-hooks",
			"merge_argv":     mergeArgv,
			"spawn_argv":     append([]any{integrationHopPath, "check-exec", "--op", "x", "--"}, mergeArgv...),
		}
	}

	t.Run("D2: a directory without recorded materialization completion is abandoned, never adopted", func(t *testing.T) {
		f := newIntegrationFixture(t, false)
		integrationID := f.seedIntegrationRow(t, run.IntegrationMerging, "")
		oldTree := "/state/runs/x/integrations/old-op/tree"
		opID := f.seedOperation(t, app.OpIntegrationMerge, app.OperationPending, mergeIntent(f, integrationID, oldTree), nil)
		// The preparer got far enough that HEAD already reads correctly —
		// exactly the window that must NOT be adopted.
		f.git.worktrees[oldTree] = f.base

		report := f.drive(t)
		if report.Blocked != "" {
			t.Fatalf("blocked = %q, want the failed settlement to unblock a fresh operation", report.Blocked)
		}
		old := f.tc.Store.Operations[opID]
		if old.State != app.OperationFailed {
			t.Fatalf("old operation state = %s, want failed (abandoned)", old.State)
		}
		mergeOps := f.opsOfKind(app.OpIntegrationMerge)
		if len(mergeOps) != 2 {
			t.Fatalf("merge operations = %d, want the abandoned one plus a FRESH operation", len(mergeOps))
		}
		for _, op := range mergeOps {
			if op.ID == opID {
				continue
			}
			freshTree := payloadField(t, op.Intent, "tree_path")
			if freshTree == oldTree {
				t.Fatalf("the fresh operation reuses the abandoned tree path %s", freshTree)
			}
		}
		for _, call := range f.tc.Commands.Calls {
			joined := strings.Join(call.Argv, " ")
			if strings.Contains(joined, "worktree add") && strings.Contains(joined, oldTree) {
				t.Fatalf("the abandoned directory was entered: %s", joined)
			}
			if strings.Contains(joined, " merge ") && strings.Contains(joined, oldTree) {
				t.Fatalf("a merge ran in the abandoned directory: %s", joined)
			}
		}
	})

	t.Run("no claim after materialization: bounded wait, then reconciling, never absence", func(t *testing.T) {
		f := newIntegrationFixture(t, false)
		integrationID := f.seedIntegrationRow(t, run.IntegrationMerging, "")
		tree := "/state/tree-materialized"
		opID := f.seedOperation(t, app.OpIntegrationMerge, app.OperationPending, mergeIntent(f, integrationID, tree),
			map[string]any{"tree_path": tree, "verified_head": f.base})
		f.git.worktrees[tree] = f.base

		report := f.drive(t)
		if report.Blocked == "" {
			t.Fatalf("want blocked within the bounded wait, got %+v", report)
		}
		if got := f.tc.Store.Operations[opID].State; got != app.OperationPending {
			t.Fatalf("operation state = %s, want still pending within the wait", got)
		}

		f.tc.Clock.Advance(app.CheckClaimDeadline + 1)
		if err := f.tc.Controller.Heartbeat(context.Background(), f.fr.Handle); err != nil {
			t.Fatalf("Heartbeat() error = %v", err)
		}
		report = f.drive(t)
		if report.Blocked == "" {
			t.Fatalf("want blocked past the wait, got %+v", report)
		}
		if got := f.tc.Store.Operations[opID].State; got != app.OperationReconciling {
			t.Fatalf("operation state = %s, want reconciling past the bounded wait", got)
		}
	})

	t.Run("claim present, group empty, tree proves M: settle with retirement evidence, adopt, publish", func(t *testing.T) {
		f := newIntegrationFixture(t, false)
		integrationID := f.seedIntegrationRow(t, run.IntegrationMerging, "")
		tree := "/state/tree-crashed"
		merged := f.git.newCommit("tree-merged", f.base, f.src)
		f.git.worktrees[tree] = merged
		opID := f.seedOperation(t, app.OpIntegrationMerge, app.OperationPending, mergeIntent(f, integrationID, tree),
			map[string]any{"tree_path": tree, "verified_head": f.base})
		f.tc.Store.CheckExecClaims[opID] = app.CheckExecClaim{OperationID: opID, PID: 4242, ClaimedAt: f.tc.Clock.Now()}
		f.tc.Groups.Processes[4242] = nil // observed empty

		report := f.drive(t)
		if report.Blocked != "" {
			t.Fatalf("blocked = %q, want adoption", report.Blocked)
		}
		op := f.tc.Store.Operations[opID]
		if op.State != app.OperationSucceeded || payloadField(t, op.Outcome, "merge_commit_oid") != merged {
			t.Fatalf("operation = %s outcome %v, want succeeded with the adopted M", op.State, op.Outcome)
		}
		if _, ok := f.tc.Store.CheckExecClaims[opID]; !ok {
			t.Fatalf("the old claim row was deleted; it is retained forever as history")
		}
		f.driveUntil(t, string(run.IntegrationIntegrated), 3)
		if head := f.git.ref(integrationRefName); head != merged {
			t.Fatalf("ref = %s, want the adopted M %s published", head, merged)
		}
	})

	t.Run("claim present, group still matched: signaled and outstanding, never adopted", func(t *testing.T) {
		f := newIntegrationFixture(t, false)
		integrationID := f.seedIntegrationRow(t, run.IntegrationMerging, "")
		tree := "/state/tree-live"
		intent := mergeIntent(f, integrationID, tree)
		opID := f.seedOperation(t, app.OpIntegrationMerge, app.OperationPending, intent,
			map[string]any{"tree_path": tree, "verified_head": f.base})
		f.tc.Store.CheckExecClaims[opID] = app.CheckExecClaim{OperationID: opID, PID: 5151, ClaimedAt: f.tc.Clock.Now()}
		argv := []string{"/usr/bin/git", "-C", tree, "merge", "--no-ff", "--no-edit", f.src}
		f.tc.Groups.Processes[5151] = []app.GroupProcess{{PID: 5151, Argv: argv}}

		report := f.drive(t)
		if report.Blocked == "" {
			t.Fatalf("want outstanding while the group lives, got %+v", report)
		}
		found := false
		for _, pid := range f.tc.Groups.Signaled {
			if pid == 5151 {
				found = true
			}
		}
		if !found {
			t.Fatalf("the matched group was never signaled")
		}
		if got := f.tc.Store.Operations[opID].State; got != app.OperationPending {
			t.Fatalf("operation state = %s, want pending while awaiting observed absence", got)
		}
	})

	t.Run("claim present, group empty, tree proves nothing: settle failed, fresh operation", func(t *testing.T) {
		f := newIntegrationFixture(t, false)
		integrationID := f.seedIntegrationRow(t, run.IntegrationMerging, "")
		tree := "/state/tree-garbage"
		stray := f.git.newCommit("tree-stray", f.src) // single parent: neither M nor the no-op
		f.git.worktrees[tree] = stray
		opID := f.seedOperation(t, app.OpIntegrationMerge, app.OperationPending, mergeIntent(f, integrationID, tree),
			map[string]any{"tree_path": tree, "verified_head": f.base})
		f.tc.Store.CheckExecClaims[opID] = app.CheckExecClaim{OperationID: opID, PID: 6161, ClaimedAt: f.tc.Clock.Now()}
		f.tc.Groups.Processes[6161] = nil

		report := f.drive(t)
		if report.Blocked != "" {
			t.Fatalf("blocked = %q, want the failed settlement to unblock a fresh operation", report.Blocked)
		}
		if got := f.tc.Store.Operations[opID].State; got != app.OperationFailed {
			t.Fatalf("operation state = %s, want failed", got)
		}
		if got := len(f.opsOfKind(app.OpIntegrationMerge)); got != 2 {
			t.Fatalf("merge operations = %d, want the settled one plus a fresh re-act", got)
		}
	})
}

func TestIntegrationPublishCrashColumns(t *testing.T) {
	seed := func(t *testing.T, f *integrationFixture) (string, identity.OperationID) {
		t.Helper()
		integrationID := f.seedIntegrationRow(t, run.IntegrationMerging, "")
		merged := f.git.newCommit("tree-merged", f.base, f.src)
		f.seedOperation(t, app.OpIntegrationMerge, app.OperationSucceeded, map[string]any{
			"integration_id": integrationID.String(), "premerge_oid": f.base, "source_oid": f.src,
			"tree_path": "/state/t", "merge_argv": []any{"x"}, "spawn_argv": []any{"y"},
		}, nil)
		for id, op := range f.tc.Store.Operations {
			if op.Kind == app.OpIntegrationMerge {
				op.Outcome = map[string]any{"result": "merged", "merge_commit_oid": merged}
				f.tc.Store.Operations[id] = op
			}
		}
		publishOp := f.seedOperation(t, app.OpIntegrationPublish, app.OperationPending, map[string]any{
			"integration_id": integrationID.String(), "ref": integrationRefName,
			"new_oid": merged, "expected_old_oid": f.base,
		}, nil)
		return merged, publishOp
	}

	t.Run("ref already at the candidate: adopt", func(t *testing.T) {
		f := newIntegrationFixture(t, false)
		merged, publishOp := seed(t, f)
		f.git.setRef(integrationRefName, merged)

		report := f.drive(t)
		if report.Blocked != "" {
			t.Fatalf("blocked = %q, want adoption", report.Blocked)
		}
		if got := f.tc.Store.Operations[publishOp].State; got != app.OperationSucceeded {
			t.Fatalf("publish state = %s, want succeeded by observation", got)
		}
		// The same round continues past the adoption: the combined check
		// runs and passes, so the integration lands integrated with the
		// ref still on the adopted candidate.
		if got := f.currentIntegrationRow(t).State; got != run.IntegrationIntegrated {
			t.Fatalf("integration state = %s, want integrated after adoption", got)
		}
		if head := f.git.ref(integrationRefName); head != merged {
			t.Fatalf("ref = %s, want the adopted candidate %s", head, merged)
		}
	})

	t.Run("ref still at the expected old value: re-act the same operation", func(t *testing.T) {
		f := newIntegrationFixture(t, false)
		merged, publishOp := seed(t, f)

		report := f.drive(t)
		if report.Blocked != "" {
			t.Fatalf("blocked = %q, want a retried CAS", report.Blocked)
		}
		if head := f.git.ref(integrationRefName); head != merged {
			t.Fatalf("ref = %s, want the candidate %s", head, merged)
		}
		if got := f.tc.Store.Operations[publishOp].State; got != app.OperationSucceeded {
			t.Fatalf("publish state = %s, want succeeded", got)
		}
		if got := len(f.opsOfKind(app.OpIntegrationPublish)); got != 1 {
			t.Fatalf("publish operations = %d; a re-act is the SAME operation, never a second intent", got)
		}
	})

	t.Run("ref at any other value: reconciling with the observed ref as evidence", func(t *testing.T) {
		f := newIntegrationFixture(t, false)
		_, publishOp := seed(t, f)
		stranger := f.git.newCommit("tree-stranger", f.base)
		f.git.setRef(integrationRefName, stranger)

		report := f.drive(t)
		if report.Blocked == "" {
			t.Fatalf("want blocked on the unresolved publish, got %+v", report)
		}
		op := f.tc.Store.Operations[publishOp]
		if op.State != app.OperationReconciling {
			t.Fatalf("publish state = %s, want reconciling", op.State)
		}
		if !strings.Contains(fmt.Sprint(op.Outcome), stranger) {
			t.Fatalf("outcome %v does not carry the observed ref", op.Outcome)
		}
	})
}

func TestIntegrationResetCrashColumns(t *testing.T) {
	seed := func(t *testing.T, f *integrationFixture) (identity.IntegrationID, string) {
		t.Helper()
		merged := f.git.newCommit("tree-merged", f.base, f.src)
		integrationID := f.seedIntegrationRow(t, run.IntegrationCheckFailed, merged)
		return integrationID, merged
	}
	resetIntent := func(f *integrationFixture, integrationID identity.IntegrationID, merged string) map[string]any {
		return map[string]any{
			"integration_id": integrationID.String(), "ref": integrationRefName,
			"rejected_oid": merged, "premerge_oid": f.base, "reason": "combined check failed",
		}
	}

	t.Run("R recorded and ref at R: adopt without recomputing", func(t *testing.T) {
		f := newIntegrationFixture(t, false)
		integrationID, merged := seed(t, f)
		rollback := f.git.newCommit("tree-base", merged)
		f.git.setRef(integrationRefName, rollback)
		opID := f.seedOperation(t, app.OpIntegrationReset, app.OperationPending, resetIntent(f, integrationID, merged),
			map[string]any{"rollback_oid": rollback, "parent_oid": merged})

		report := f.drive(t)
		if report.Blocked != "" {
			t.Fatalf("blocked = %q, want adoption", report.Blocked)
		}
		if f.git.CommitTreeCalls != 0 {
			t.Fatalf("commit-tree ran %d times; recovery compares the PERSISTED id only", f.git.CommitTreeCalls)
		}
		if got := f.tc.Store.Operations[opID].State; got != app.OperationSucceeded {
			t.Fatalf("reset state = %s, want succeeded", got)
		}
		if got := f.currentIntegrationRow(t).State; got != run.IntegrationRolledBack {
			t.Fatalf("integration state = %s, want rolled-back", got)
		}
		if got := f.tc.Store.Tasks[f.taskID].value.State; got != run.TaskNeedsRework {
			t.Fatalf("task state = %s, want needs-rework", got)
		}
	})

	t.Run("R recorded and ref at the rejected candidate: re-act step (ii) only", func(t *testing.T) {
		f := newIntegrationFixture(t, false)
		integrationID, merged := seed(t, f)
		f.git.setRef(integrationRefName, merged)
		rollback := f.git.newCommit("tree-base", merged)
		f.seedOperation(t, app.OpIntegrationReset, app.OperationPending, resetIntent(f, integrationID, merged),
			map[string]any{"rollback_oid": rollback, "parent_oid": merged})

		report := f.drive(t)
		if report.Blocked != "" {
			t.Fatalf("blocked = %q, want the completed reset", report.Blocked)
		}
		if f.git.CommitTreeCalls != 0 {
			t.Fatalf("commit-tree ran %d times; step (i) is never repeated once R is persisted", f.git.CommitTreeCalls)
		}
		if head := f.git.ref(integrationRefName); head != rollback {
			t.Fatalf("ref = %s, want the persisted R %s", head, rollback)
		}
	})

	t.Run("no R recorded and ref at the rejected candidate: re-act from step (i)", func(t *testing.T) {
		f := newIntegrationFixture(t, false)
		integrationID, merged := seed(t, f)
		f.git.setRef(integrationRefName, merged)
		opID := f.seedOperation(t, app.OpIntegrationReset, app.OperationPending, resetIntent(f, integrationID, merged), nil)

		report := f.drive(t)
		if report.Blocked != "" {
			t.Fatalf("blocked = %q, want the completed reset", report.Blocked)
		}
		if f.git.CommitTreeCalls != 1 {
			t.Fatalf("commit-tree ran %d times, want exactly one fresh R", f.git.CommitTreeCalls)
		}
		head := f.git.ref(integrationRefName)
		if head == merged || head == f.base {
			t.Fatalf("ref = %s; never-revisit requires a FRESH rollback commit, never a previous value", head)
		}
		if tree := f.git.treeOf(head); tree != "tree-base" {
			t.Fatalf("rollback commit tree = %s, want the validated pre-merge tree", tree)
		}
		if parents := f.git.parentsOf(head); len(parents) != 1 || parents[0] != merged {
			t.Fatalf("rollback commit parents = %v, want the rejected candidate kept reachable", parents)
		}
		evidence := payloadField(t, f.tc.Store.Operations[opID].ActEvidence, "rollback_oid")
		if evidence != head {
			t.Fatalf("persisted rollback OID %s != published %s; the OID is persisted BEFORE the CAS", evidence, head)
		}
	})
}

func TestIntegrationCheckFailureDrivesReset(t *testing.T) {
	f := newIntegrationFixture(t, false)
	f.git.CheckExitCode = 1

	f.drive(t) // claim
	f.drive(t) // merge
	f.drive(t) // publish -> checking
	f.drive(t) // combined check fails -> check-failed
	integ := f.currentIntegrationRow(t)
	if integ.State != run.IntegrationCheckFailed {
		t.Fatalf("integration state = %s, want check-failed", integ.State)
	}
	f.drive(t) // reset -> rolled-back

	integ = f.currentIntegrationRow(t)
	if integ.State != run.IntegrationRolledBack {
		t.Fatalf("integration state = %s, want rolled-back", integ.State)
	}
	head := f.git.ref(integrationRefName)
	if tree := f.git.treeOf(head); tree != "tree-base" {
		t.Fatalf("post-reset head tree = %s, want the pre-merge content", tree)
	}
	if parents := f.git.parentsOf(head); len(parents) != 1 || parents[0] != integ.MergeCommitOID {
		t.Fatalf("rollback parents = %v, want the rejected merge reachable", parents)
	}
	if got := f.tc.Store.Tasks[f.taskID].value.State; got != run.TaskNeedsRework {
		t.Fatalf("task state = %s, want needs-rework", got)
	}
}

func TestIntegrationCheckUnknownOutcome(t *testing.T) {
	seedChecking := func(t *testing.T, f *integrationFixture) (string, identity.OperationID) {
		t.Helper()
		merged := f.git.newCommit("tree-merged", f.base, f.src)
		f.git.setRef(integrationRefName, merged)
		integrationID := f.seedIntegrationRow(t, run.IntegrationChecking, merged)
		opID := f.seedOperation(t, app.OpCheckRun, app.OperationPending, map[string]any{
			"integration_id":     integrationID.String(),
			"subject_commit_oid": merged,
			"subject_tree_oid":   "tree-merged",
			"result_id":          f.resultID.String(),
			"check_argv":         []any{"/bin/hopcheck"},
			"spawn_argv":         []any{integrationHopPath, "check-exec", "--op", "o", "--", "/bin/hopcheck"},
		}, nil)
		f.tc.Store.CheckExecClaims[opID] = app.CheckExecClaim{OperationID: opID, PID: 7171, ClaimedAt: f.tc.Clock.Now()}
		f.tc.Groups.Processes[7171] = nil // confirmed absent
		return merged, opID
	}

	t.Run("unrepeatable: the integration settles check-failed and the reset runs", func(t *testing.T) {
		f := newIntegrationFixture(t, false)
		merged, opID := seedChecking(t, f)

		// One round applies the unknown rule (check-failed) and, having
		// settled it, drives the reset in the same pass.
		report := f.drive(t)
		if report.Blocked != "" {
			t.Fatalf("blocked = %q, want the unknown rule applied", report.Blocked)
		}
		op := f.tc.Store.Operations[opID]
		if op.State != app.OperationFailed || payloadField(t, op.Outcome, "unknown") != "true" {
			t.Fatalf("check op = %s outcome %v, want failed with unknown recorded", op.State, op.Outcome)
		}
		if report.State != string(run.IntegrationRolledBack) {
			report = f.driveUntil(t, string(run.IntegrationRolledBack), 2)
		}
		if got := f.currentIntegrationRow(t).State; got != run.IntegrationRolledBack {
			t.Fatalf("integration state = %s, want rolled-back", got)
		}
		if head := f.git.ref(integrationRefName); head == merged {
			t.Fatalf("ref still rests on the unvalidated candidate")
		}
	})

	t.Run("repeatable: a fresh execution against the same candidate head", func(t *testing.T) {
		f := newIntegrationFixture(t, false)
		snap := f.tc.Store.Snapshots[f.fr.RunID]
		snap.CheckRepeatable = true
		f.tc.Store.Snapshots[f.fr.RunID] = snap
		merged, opID := seedChecking(t, f)

		spawnsBefore := f.git.CheckExecCalls
		// One round applies the unknown rule (the integration stays
		// checking) and, unblocked, spawns the fresh execution against the
		// same candidate in the same pass; it passes.
		report := f.drive(t)
		if report.Blocked != "" {
			t.Fatalf("blocked = %q, want the unknown rule applied", report.Blocked)
		}
		if got := f.tc.Store.Operations[opID].State; got != app.OperationFailed {
			t.Fatalf("check op state = %s, want failed with unknown", got)
		}
		if f.git.CheckExecCalls != spawnsBefore+1 {
			t.Fatalf("check spawns = %d, want exactly one fresh execution", f.git.CheckExecCalls-spawnsBefore)
		}
		if got := f.currentIntegrationRow(t).State; got != run.IntegrationIntegrated {
			t.Fatalf("integration state = %s, want integrated; candidate %s", got, merged)
		}
	})
}

func TestIntegrationStopPrecedence(t *testing.T) {
	requestStop := func(f *integrationFixture) {
		row := f.tc.Store.Runs[f.fr.RunID]
		row.value = row.value.RequestStop(f.tc.Clock.Now())
		row.revision++
	}
	absentPanes := func(f *integrationFixture) {
		f.tc.Runtime.InspectPaneFn = func(string) (app.PaneProcess, error) {
			return app.PaneProcess{}, app.ErrPaneNotFound
		}
	}

	t.Run("stop mid-merge: the group is retired and the integration settles interrupted", func(t *testing.T) {
		f := newIntegrationFixture(t, false)
		integrationID := f.seedIntegrationRow(t, run.IntegrationMerging, "")
		tree := "/state/tree-live"
		argv := []any{"/usr/bin/git", "-C", tree, "merge", "--no-ff", "--no-edit", f.src}
		opID := f.seedOperation(t, app.OpIntegrationMerge, app.OperationPending, map[string]any{
			"integration_id": integrationID.String(), "premerge_oid": f.base, "source_oid": f.src,
			"tree_path": tree, "merge_argv": argv, "spawn_argv": append([]any{integrationHopPath, "check-exec", "--op", "o", "--"}, argv...),
		}, map[string]any{"tree_path": tree, "verified_head": f.base})
		f.tc.Store.CheckExecClaims[opID] = app.CheckExecClaim{OperationID: opID, PID: 8181, ClaimedAt: f.tc.Clock.Now()}
		liveArgv := []string{"/usr/bin/git", "-C", tree, "merge", "--no-ff", "--no-edit", f.src}
		f.tc.Groups.Processes[8181] = []app.GroupProcess{{PID: 8181, Argv: liveArgv}}
		f.git.worktrees[tree] = f.base
		requestStop(f)
		absentPanes(f)

		report, err := f.tc.Controller.DriveFeatureStop(context.Background(), f.fr.Handle)
		if err != nil {
			t.Fatalf("DriveFeatureStop() error = %v", err)
		}
		if report.Terminated {
			t.Fatalf("stop reported terminated while the merge group lives")
		}
		signaled := false
		for _, pid := range f.tc.Groups.Signaled {
			if pid == 8181 {
				signaled = true
			}
		}
		if !signaled {
			t.Fatalf("the merge group was never signaled")
		}

		// The group dies; the next round settles everything.
		f.tc.Groups.Processes[8181] = nil
		report, err = f.tc.Controller.DriveFeatureStop(context.Background(), f.fr.Handle)
		if err != nil {
			t.Fatalf("DriveFeatureStop() round 2 error = %v", err)
		}
		if !report.Terminated {
			t.Fatalf("stop not terminated after owned work observed absent: %+v", report)
		}
		if got := f.currentIntegrationRow(t).State; got != run.IntegrationInterrupted {
			t.Fatalf("integration state = %s, want interrupted (stop before a candidate was published)", got)
		}
		if head := f.git.ref(integrationRefName); head != f.base {
			t.Fatalf("ref = %s, want untouched %s", head, f.base)
		}
		if got := f.tc.Store.Tasks[f.taskID].value.State; got != run.TaskInterrupted {
			t.Fatalf("task state = %s, want interrupted", got)
		}
		if got := f.tc.Store.Runs[f.fr.RunID].value.State; got != run.RunStopped {
			t.Fatalf("run state = %s, want stopped", got)
		}
	})

	t.Run("stop completing a pending rollback: the reset CAS lands before stopped", func(t *testing.T) {
		f := newIntegrationFixture(t, false)
		merged := f.git.newCommit("tree-merged", f.base, f.src)
		f.git.setRef(integrationRefName, merged)
		integrationID := f.seedIntegrationRow(t, run.IntegrationCheckFailed, merged)
		rollback := f.git.newCommit("tree-base", merged)
		f.seedOperation(t, app.OpIntegrationReset, app.OperationPending, map[string]any{
			"integration_id": integrationID.String(), "ref": integrationRefName,
			"rejected_oid": merged, "premerge_oid": f.base, "reason": "combined check failed",
		}, map[string]any{"rollback_oid": rollback, "parent_oid": merged})
		requestStop(f)
		absentPanes(f)

		report, err := f.tc.Controller.DriveFeatureStop(context.Background(), f.fr.Handle)
		if err != nil {
			t.Fatalf("DriveFeatureStop() error = %v", err)
		}
		if !report.Terminated {
			t.Fatalf("stop not terminated: %+v", report)
		}
		if head := f.git.ref(integrationRefName); head != rollback {
			t.Fatalf("ref = %s, want the completed rollback %s before stopped", head, rollback)
		}
		if got := f.currentIntegrationRow(t).State; got != run.IntegrationRolledBack {
			t.Fatalf("integration state = %s, want rolled-back (COMPLETED, never fenced over)", got)
		}
		if got := f.tc.Store.Tasks[f.taskID].value.State; got != run.TaskInterrupted {
			t.Fatalf("task state = %s, want interrupted by stop precedence, not needs-rework", got)
		}
	})
}

func TestIntegrationBarrierUnmovedHeadWindow(t *testing.T) {
	seedPendingPublish := func(t *testing.T, f *integrationFixture) string {
		t.Helper()
		integrationID := f.seedIntegrationRow(t, run.IntegrationMerging, "")
		merged := f.git.newCommit("tree-merged", f.base, f.src)
		f.seedOperation(t, app.OpIntegrationPublish, app.OperationPending, map[string]any{
			"integration_id": integrationID.String(), "ref": integrationRefName,
			"new_oid": merged, "expected_old_oid": f.base,
		}, nil)
		return merged
	}
	requestStop := func(f *integrationFixture) {
		row := f.tc.Store.Runs[f.fr.RunID]
		row.value = row.value.RequestStop(f.tc.Clock.Now())
		row.revision++
	}
	absentPanes := func(f *integrationFixture) {
		f.tc.Runtime.InspectPaneFn = func(string) (app.PaneProcess, error) {
			return app.PaneProcess{}, app.ErrPaneNotFound
		}
	}

	t.Run("fencing lands first: A's late CAS dies at the ref store", func(t *testing.T) {
		f := newIntegrationFixture(t, false)
		merged := seedPendingPublish(t, f)
		requestStop(f)
		absentPanes(f)

		report, err := f.tc.Controller.DriveFeatureStop(context.Background(), f.fr.Handle)
		if err != nil {
			t.Fatalf("DriveFeatureStop() error = %v", err)
		}
		if !report.Terminated {
			t.Fatalf("stop not terminated: %+v", report)
		}
		fence := f.git.ref(integrationRefName)
		if fence == f.base || fence == merged {
			t.Fatalf("ref = %s, want a fresh fencing commit", fence)
		}
		if tree := f.git.treeOf(fence); tree != "tree-base" {
			t.Fatalf("fence tree = %s, want the CURRENT head's tree (content unchanged)", tree)
		}
		if parents := f.git.parentsOf(fence); len(parents) != 1 || parents[0] != f.base {
			t.Fatalf("fence parents = %v, want the current head", parents)
		}

		// A's late CAS, dispatched after the retirement: expected-old
		// burned, dead at the ref store.
		result := f.git.runGitArgv([]string{"update-ref", "--no-deref", integrationRefName, merged, f.base})
		if result.ExitCode == 0 {
			t.Fatalf("A's zombie publish CAS succeeded; the fencing rule must have burned its expected-old value")
		}
		if head := f.git.ref(integrationRefName); head != fence {
			t.Fatalf("ref moved to %s under the zombie CAS", head)
		}
	})

	t.Run("A's publish lands first: the stop path rolls the candidate back and A's outcome commit is fenced", func(t *testing.T) {
		f := newIntegrationFixture(t, false)
		merged := seedPendingPublish(t, f)
		// A's CAS wins the race before the retirement runs.
		if result := f.git.runGitArgv([]string{"update-ref", "--no-deref", integrationRefName, merged, f.base}); result.ExitCode != 0 {
			t.Fatalf("the zombie CAS could not land: %s", result.Stderr)
		}
		requestStop(f)
		absentPanes(f)

		report, err := f.tc.Controller.DriveFeatureStop(context.Background(), f.fr.Handle)
		if err != nil {
			t.Fatalf("DriveFeatureStop() error = %v", err)
		}
		if !report.Terminated {
			// The reset may take a second round after the adoption.
			report, err = f.tc.Controller.DriveFeatureStop(context.Background(), f.fr.Handle)
			if err != nil {
				t.Fatalf("DriveFeatureStop() round 2 error = %v", err)
			}
		}
		if !report.Terminated {
			t.Fatalf("stop not terminated: %+v", report)
		}
		head := f.git.ref(integrationRefName)
		if head == merged {
			t.Fatalf("the ref rests on the unvalidated candidate in a stopped run")
		}
		if tree := f.git.treeOf(head); tree != "tree-base" {
			t.Fatalf("post-rollback tree = %s, want the validated pre-merge content", tree)
		}
		if parents := f.git.parentsOf(head); len(parents) != 1 || parents[0] != merged {
			t.Fatalf("rollback parents = %v, want the rejected candidate reachable", parents)
		}
		if got := f.currentIntegrationRow(t).State; got != run.IntegrationRolledBack {
			t.Fatalf("integration state = %s, want rolled-back", got)
		}

		// A's outcome commit under its stale lease is fenced at the store.
		staleLease := app.Lease{Run: f.fr.RunID, ControllerID: "controller-A", Generation: 0, ExpiresAt: f.tc.Clock.Now()}
		uow, err := f.tc.Store.Begin(context.Background(), staleLease)
		if err != nil {
			t.Fatalf("Begin() error = %v", err)
		}
		if err := uow.Commit(); !errors.Is(err, app.ErrFenced) {
			t.Fatalf("stale outcome commit error = %v, want ErrFenced", err)
		}
	})
}

func TestIntegrationBarrierZombieAfterTakeover(t *testing.T) {
	f := newIntegrationFixture(t, false)
	f.git.CheckExitCode = 1 // B's combined check fails, so B rolls back and advances the branch.

	// Controller A crashed mid-merge: intent + materialization evidence +
	// claim recorded, the scratch tree already holds M, the group still
	// live.
	integrationID := f.seedIntegrationRow(t, run.IntegrationMerging, "")
	tree := "/state/tree-A"
	merged := f.git.newCommit("tree-merged", f.base, f.src)
	f.git.worktrees[tree] = merged
	argv := []any{"/usr/bin/git", "-C", tree, "merge", "--no-ff", "--no-edit", f.src}
	opID := f.seedOperation(t, app.OpIntegrationMerge, app.OperationPending, map[string]any{
		"integration_id": integrationID.String(), "premerge_oid": f.base, "source_oid": f.src,
		"tree_path": tree, "merge_argv": argv, "spawn_argv": append([]any{integrationHopPath, "check-exec", "--op", "o", "--"}, argv...),
	}, map[string]any{"tree_path": tree, "verified_head": f.base})
	f.tc.Store.CheckExecClaims[opID] = app.CheckExecClaim{OperationID: opID, PID: 9191, ClaimedAt: f.tc.Clock.Now()}
	liveArgv := []string{"/usr/bin/git", "-C", tree, "merge", "--no-ff", "--no-edit", f.src}
	f.tc.Groups.Processes[9191] = []app.GroupProcess{{PID: 9191, Argv: liveArgv}}

	// B takes over after A's lease expires. A's lease is the fixture's
	// generation-1 grant (seedFeatureRun's shape).
	aLease := app.Lease{Run: f.fr.RunID, ControllerID: "controller-1", Generation: 1, ExpiresAt: f.tc.Clock.Now().Add(leaseTTL)}
	f.tc.Clock.Advance(leaseTTL + 1)
	leaseB, err := f.tc.Store.AcquireLease(context.Background(), f.fr.RunID, "controller-B")
	if err != nil {
		t.Fatalf("AcquireLease(B) error = %v", err)
	}
	handleB := app.NewRunHandleForTest(f.fr.RunID, leaseB)

	driveB := func() app.IntegrationReport {
		report, driveErr := f.tc.Controller.DriveIntegration(context.Background(), handleB, integrationHopPath, nil)
		if driveErr != nil {
			t.Fatalf("DriveIntegration(B) error = %v", driveErr)
		}
		return report
	}

	// B retires A's scratch group by claim: signaled first, then adopted
	// once observed absent.
	report := driveB()
	if report.Blocked == "" {
		t.Fatalf("B did not block on A's live merge group")
	}
	signaled := false
	for _, pid := range f.tc.Groups.Signaled {
		if pid == 9191 {
			signaled = true
		}
	}
	if !signaled {
		t.Fatalf("A's scratch group was never signaled by claim")
	}
	f.tc.Groups.Processes[9191] = nil

	// B adopts M, publishes, fails the combined check, resets — the
	// branch has advanced twice past A's expected-old.
	for range 6 {
		report = driveB()
		if report.State == string(run.IntegrationRolledBack) {
			break
		}
		if report.Blocked != "" {
			t.Fatalf("B blocked: %s", report.Blocked)
		}
	}
	if report.State != string(run.IntegrationRolledBack) {
		t.Fatalf("B's integration state = %s, want rolled-back", report.State)
	}
	head := f.git.ref(integrationRefName)
	if head == f.base || head == merged {
		t.Fatalf("ref = %s, want B's fresh rollback commit", head)
	}

	// A resumes: its zombie publish CAS (expected-old = the original
	// pre-merge head) dies at the ref store under never-revisit.
	if result := f.git.runGitArgv([]string{"update-ref", "--no-deref", integrationRefName, merged, f.base}); result.ExitCode == 0 {
		t.Fatalf("A's zombie publish CAS succeeded against the advanced branch")
	}
	if result := f.git.runGitArgv([]string{"update-ref", "--no-deref", integrationRefName, f.base, merged}); result.ExitCode == 0 {
		t.Fatalf("A's zombie reset CAS succeeded against the advanced branch")
	}
	if got := f.git.ref(integrationRefName); got != head {
		t.Fatalf("ref moved to %s under a zombie CAS", got)
	}

	// A's store writes are fenced.
	uow, err := f.tc.Store.Begin(context.Background(), aLease)
	if err != nil {
		t.Fatalf("Begin(A) error = %v", err)
	}
	if err := uow.Commit(); !errors.Is(err, app.ErrFenced) {
		t.Fatalf("A's outcome commit error = %v, want ErrFenced", err)
	}
}

func TestIntegrationFenceRecoveryRow(t *testing.T) {
	// Crash after the fence CAS, before its outcome: decidable through
	// integration.fence's own row, never a fresh ambiguity.
	f := newIntegrationFixture(t, false)
	merged := f.git.newCommit("tree-merged", f.base, f.src)
	f.seedIntegrationRow(t, run.IntegrationMerging, "")
	publishOp := f.seedOperation(t, app.OpIntegrationPublish, app.OperationPending, map[string]any{
		"integration_id": "any", "ref": integrationRefName,
		"new_oid": merged, "expected_old_oid": f.base,
	}, nil)
	fence := f.git.newCommit("tree-base", f.base)
	f.git.setRef(integrationRefName, fence)
	fenceOp := f.seedOperation(t, app.OpIntegrationFence, app.OperationPending, map[string]any{
		"ref": integrationRefName, "observed_head_oid": f.base,
		"retired_operation_id": publishOp.String(), "retired_operation_kind": string(app.OpIntegrationPublish),
	}, map[string]any{"fence_oid": fence})

	report := f.drive(t)
	_ = report
	if got := f.tc.Store.Operations[fenceOp].State; got != app.OperationSucceeded {
		t.Fatalf("fence state = %s, want succeeded (ref == persisted F adopts)", got)
	}
	if got := f.tc.Store.Operations[publishOp].State; got != app.OperationFailed {
		t.Fatalf("retired publish state = %s, want failed (retired by the fence)", got)
	}
	if f.git.CommitTreeCalls != 0 {
		t.Fatalf("commit-tree ran %d times; adoption compares the persisted F only", f.git.CommitTreeCalls)
	}
}
