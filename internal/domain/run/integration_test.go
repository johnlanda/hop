package run_test

import (
	"errors"
	"testing"
	"time"

	"github.com/johnlanda/hop/internal/domain/run"
)

// integrationStates enumerates every Integration state. It is a function,
// not a package-level slice, only to keep this shared fixture out of the
// global-variable set the project's lint configuration restricts.
func integrationStates() []run.IntegrationState {
	return []run.IntegrationState{
		run.IntegrationMerging, run.IntegrationChecking, run.IntegrationIntegrated,
		run.IntegrationConflicted, run.IntegrationCheckFailed, run.IntegrationRolledBack,
		run.IntegrationInterrupted,
	}
}

// integrationValidTransitions is section 5's Integration table, transcribed
// independently of the production transitionTable.
func integrationValidTransitions() map[[2]run.IntegrationState]bool {
	return map[[2]run.IntegrationState]bool{
		{run.IntegrationMerging, run.IntegrationChecking}:       true,
		{run.IntegrationMerging, run.IntegrationConflicted}:     true,
		{run.IntegrationChecking, run.IntegrationIntegrated}:    true,
		{run.IntegrationChecking, run.IntegrationCheckFailed}:   true,
		{run.IntegrationCheckFailed, run.IntegrationRolledBack}: true,
		{run.IntegrationMerging, run.IntegrationInterrupted}:    true,
		{run.IntegrationChecking, run.IntegrationRolledBack}:    true,
	}
}

func TestIntegrationTransitions(t *testing.T) {
	steps := []struct {
		name string
		to   run.IntegrationState
		fn   func(run.Integration, time.Time) (run.Integration, error)
	}{
		{"Conflict", run.IntegrationConflicted, run.Integration.Conflict},
		{"Integrate", run.IntegrationIntegrated, run.Integration.Integrate},
		{"FailCheck", run.IntegrationCheckFailed, run.Integration.FailCheck},
		{"RollBack", run.IntegrationRolledBack, run.Integration.RollBack},
		{"Interrupt", run.IntegrationInterrupted, run.Integration.Interrupt},
		{"EnterChecking", run.IntegrationChecking, func(i run.Integration, now time.Time) (run.Integration, error) {
			return i.EnterChecking("merge-oid", now)
		}},
	}

	valid := integrationValidTransitions()
	for _, step := range steps {
		for _, from := range integrationStates() {
			t.Run(step.name+"/"+string(from)+"_to_"+string(step.to), func(t *testing.T) {
				integration := run.Integration{ID: testIntegrationID, RunID: testRunID, TaskID: testTaskID, State: from}

				got, err := step.fn(integration, epoch())

				if valid[[2]run.IntegrationState{from, step.to}] {
					if err != nil {
						t.Fatalf("%s from %s: unexpected error: %v", step.name, from, err)
					}
					if got.State != step.to {
						t.Fatalf("%s from %s: State = %s, want %s", step.name, from, got.State, step.to)
					}
					if !got.UpdatedAt.Equal(epoch()) {
						t.Fatalf("%s from %s: UpdatedAt = %v, want %v", step.name, from, got.UpdatedAt, epoch())
					}
					return
				}
				if err == nil {
					t.Fatalf("%s from %s: got %+v, want ErrInvalidTransition", step.name, from, got)
				}
				if !errors.Is(err, run.ErrInvalidTransition) {
					t.Fatalf("%s from %s: error = %v, want wrapping ErrInvalidTransition", step.name, from, err)
				}
				if got.State != from {
					t.Fatalf("%s from %s: State = %s, want unchanged", step.name, from, got.State)
				}
			})
		}
	}
}

// TestIntegrationEnterCheckingRecordsMergeCommit proves EnterChecking
// records the candidate object ID it is given — a fresh merge commit, or,
// for the ancestor/no-op outcome, the unchanged head passed through
// unchanged.
func TestIntegrationEnterCheckingRecordsMergeCommit(t *testing.T) {
	t.Run("ordinary merge", func(t *testing.T) {
		integration := run.NewIntegration(testIntegrationID, testRunID, testTaskID, testResultID, "source-oid", "premerge-oid", epoch())

		got, err := integration.EnterChecking("merge-commit-oid", later())
		if err != nil {
			t.Fatalf("EnterChecking: unexpected error: %v", err)
		}
		if got.MergeCommitOID != "merge-commit-oid" {
			t.Fatalf("EnterChecking: MergeCommitOID = %q, want merge-commit-oid", got.MergeCommitOID)
		}
	})

	t.Run("no-op outcome carries the unchanged head", func(t *testing.T) {
		integration := run.NewIntegration(testIntegrationID, testRunID, testTaskID, testResultID, "source-oid", "premerge-oid", epoch())

		got, err := integration.EnterChecking("premerge-oid", later())
		if err != nil {
			t.Fatalf("EnterChecking: unexpected error: %v", err)
		}
		if got.MergeCommitOID != "premerge-oid" {
			t.Fatalf("EnterChecking(no-op): MergeCommitOID = %q, want premerge-oid", got.MergeCommitOID)
		}
	})

	t.Run("invalid source leaves MergeCommitOID untouched", func(t *testing.T) {
		integration := run.Integration{ID: testIntegrationID, State: run.IntegrationIntegrated}

		got, err := integration.EnterChecking("merge-commit-oid", later())
		if !errors.Is(err, run.ErrInvalidTransition) {
			t.Fatalf("EnterChecking from integrated: error = %v, want ErrInvalidTransition", err)
		}
		if got.MergeCommitOID != "" {
			t.Fatalf("EnterChecking from integrated: MergeCommitOID = %q, want untouched", got.MergeCommitOID)
		}
	})
}

func TestNewIntegration(t *testing.T) {
	integration := run.NewIntegration(testIntegrationID, testRunID, testTaskID, testResultID, "source-oid", "premerge-oid", epoch())

	if integration.State != run.IntegrationMerging {
		t.Fatalf("NewIntegration State = %s, want merging", integration.State)
	}
	if integration.SourceCommitOID != "source-oid" || integration.PremergeHeadOID != "premerge-oid" {
		t.Fatalf("NewIntegration did not preserve its inputs: %+v", integration)
	}
	if !integration.CreatedAt.Equal(epoch()) || !integration.UpdatedAt.Equal(epoch()) {
		t.Fatalf("NewIntegration timestamps = %+v, want %v", integration, epoch())
	}
}
