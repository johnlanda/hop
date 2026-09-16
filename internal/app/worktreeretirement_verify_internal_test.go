package app

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/johnlanda/hop/internal/domain/identity"
	"github.com/johnlanda/hop/internal/domain/run"
)

// TestVerifyRetirementCandidate covers candidate provenance: a row agreeing
// with its attempt's one succeeded worktree.create operation — typed or
// JSON round-tripped, with the outcome in act evidence or in the outcome —
// becomes a candidate on the full branch ref; every missing link or
// disagreeing fact is unverified, and no detail echoes a path.
func TestVerifyRetirementCandidate(t *testing.T) {
	const (
		root    = "/srv/repo"
		path    = "/worktrees/repo/hop-r3-t1a1"
		branch  = "hop/r3/t1a1"
		base    = "1111111111111111111111111111111111111111"
		runID   = identity.RunID("00000000-0000-4000-8000-000000000001")
		attempt = identity.AttemptID("00000000-0000-4000-8000-000000000002")
		other   = identity.AttemptID("00000000-0000-4000-8000-000000000003")
	)
	row := func() run.Worktree {
		w, err := run.NewAttemptWorktree("00000000-0000-4000-8000-000000000004", "00000000-0000-4000-8000-000000000005", runID, attempt, base, path, branch)
		if err != nil {
			t.Fatal(err)
		}
		return w
	}
	create := func(edit func(intent *attemptWorktreeCreateIntent, outcome *worktreeCreateOutcome, op *Operation)) Operation {
		intent := attemptWorktreeCreateIntent{RepositoryRoot: root, Branch: branch, BaseRef: base, AttemptID: attempt}
		outcome := worktreeCreateOutcome{Info: WorktreeInfo{WorkspaceID: "w3", Path: path, Branch: branch}, BaseCommit: base}
		op := Operation{ID: "00000000-0000-4000-8000-000000000006", RunID: runID, Kind: OpWorktreeCreate, State: OperationSucceeded}
		if edit != nil {
			edit(&intent, &outcome, &op)
		}
		op.Intent = intent
		if op.ActEvidence == nil && op.Outcome == nil {
			op.ActEvidence = outcome
		}
		return op
	}
	roundTrip := func(op Operation) Operation {
		for _, field := range []*any{&op.Intent, &op.ActEvidence, &op.Outcome} {
			if *field == nil {
				continue
			}
			raw, err := json.Marshal(*field)
			if err != nil {
				t.Fatal(err)
			}
			var generic any
			if err := json.Unmarshal(raw, &generic); err != nil {
				t.Fatal(err)
			}
			*field = generic
		}
		return op
	}
	want := attemptCheckout{RepositoryRoot: root, Path: path, Branch: "refs/heads/" + branch, BaseOID: base}

	accepted := []struct {
		name    string
		creates []Operation
	}{
		{"the assignment act's typed record", []Operation{create(nil)}},
		{"a JSON round-tripped record", []Operation{roundTrip(create(nil))}},
		{"the outcome recorded by a recovery", []Operation{roundTrip(create(func(_ *attemptWorktreeCreateIntent, outcome *worktreeCreateOutcome, op *Operation) {
			op.Outcome = *outcome
		}))}},
		{"beside another attempt's and unsettled records", []Operation{
			create(func(intent *attemptWorktreeCreateIntent, _ *worktreeCreateOutcome, _ *Operation) {
				intent.AttemptID = other
			}),
			create(func(_ *attemptWorktreeCreateIntent, _ *worktreeCreateOutcome, op *Operation) {
				op.State, op.Outcome = OperationFailed, "unrelated checkout"
			}),
			create(func(_ *attemptWorktreeCreateIntent, _ *worktreeCreateOutcome, op *Operation) {
				op.State = OperationReconciling
			}),
			create(nil),
		}},
	}
	for _, tc := range accepted {
		t.Run("candidate: "+tc.name, func(t *testing.T) {
			w := row()
			got, detail, ok := verifyRetirementCandidate(&w, tc.creates, root)
			if !ok || got != want || detail != "" {
				t.Fatalf("verifyRetirementCandidate = %+v, %q, %v; want %+v", got, detail, ok, want)
			}
		})
	}

	refused := []struct {
		name    string
		row     func(w *run.Worktree)
		creates []Operation
		root    string
	}{
		{name: "an unlinked row", row: func(w *run.Worktree) { w.AttemptID = "" }},
		{name: "a row without a base commit", row: func(w *run.Worktree) { w.BaseCommit = "" }},
		{name: "a row without a branch", row: func(w *run.Worktree) { w.Branch = "" }},
		{name: "a row naming a full ref", row: func(w *run.Worktree) { w.Branch = "refs/heads/" + branch }},
		{name: "a row without a path", row: func(w *run.Worktree) { w.Path = "" }},
		{name: "no worktree.create operation", creates: []Operation{}},
		{name: "only another attempt's operation", creates: []Operation{create(func(intent *attemptWorktreeCreateIntent, _ *worktreeCreateOutcome, _ *Operation) {
			intent.AttemptID = other
		})}},
		{name: "only an unsettled operation", creates: []Operation{create(func(_ *attemptWorktreeCreateIntent, _ *worktreeCreateOutcome, op *Operation) {
			op.State = OperationReconciling
		})}},
		{name: "another run's operation", creates: []Operation{create(func(_ *attemptWorktreeCreateIntent, _ *worktreeCreateOutcome, op *Operation) {
			op.RunID = "00000000-0000-4000-8000-000000000099"
		})}},
		{name: "another kind's operation", creates: []Operation{create(func(_ *attemptWorktreeCreateIntent, _ *worktreeCreateOutcome, op *Operation) {
			op.Kind = OpPaneOpen
		})}},
		{name: "two succeeded operations for the attempt", creates: []Operation{create(nil), create(nil)}},
		{name: "an undecodable outcome", creates: []Operation{create(func(_ *attemptWorktreeCreateIntent, _ *worktreeCreateOutcome, op *Operation) {
			op.Outcome = "worktree already adopted"
		})}},
		{name: "another repository root", root: "/srv/other"},
		{name: "another requested branch", creates: []Operation{create(func(intent *attemptWorktreeCreateIntent, _ *worktreeCreateOutcome, _ *Operation) {
			intent.Branch = "hop/r3/t9a1"
		})}},
		{name: "another reported branch", creates: []Operation{create(func(_ *attemptWorktreeCreateIntent, outcome *worktreeCreateOutcome, _ *Operation) {
			outcome.Info.Branch = ""
		})}},
		{name: "another requested base", creates: []Operation{create(func(intent *attemptWorktreeCreateIntent, _ *worktreeCreateOutcome, _ *Operation) {
			intent.BaseRef = "2222222222222222222222222222222222222222"
		})}},
		{name: "another verified base", creates: []Operation{create(func(_ *attemptWorktreeCreateIntent, outcome *worktreeCreateOutcome, _ *Operation) {
			outcome.BaseCommit = "2222222222222222222222222222222222222222"
		})}},
		{name: "another reported path", creates: []Operation{create(func(_ *attemptWorktreeCreateIntent, outcome *worktreeCreateOutcome, _ *Operation) {
			outcome.Info.Path = root
		})}},
	}
	for _, tc := range refused {
		t.Run("unverified: "+tc.name, func(t *testing.T) {
			w := row()
			if tc.row != nil {
				tc.row(&w)
			}
			creates := tc.creates
			if creates == nil {
				creates = []Operation{create(nil)}
			}
			repositoryRoot := tc.root
			if repositoryRoot == "" {
				repositoryRoot = root
			}
			got, detail, ok := verifyRetirementCandidate(&w, creates, repositoryRoot)
			if ok || got != (attemptCheckout{}) || detail == "" {
				t.Fatalf("verifyRetirementCandidate = %+v, %q, %v; want unverified with a detail", got, detail, ok)
			}
			if strings.Contains(detail, "/") {
				t.Fatalf("detail %q echoes a path", detail)
			}
		})
	}
}
