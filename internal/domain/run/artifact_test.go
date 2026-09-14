package run_test

import (
	"testing"

	"github.com/johnlanda/hop/internal/domain/run"
)

func TestNewArtifact(t *testing.T) {
	a := run.NewArtifact(testArtifactID, testRunID, run.ArtifactAssignment, "runs/r1/artifacts/assignment.md", "digest-1")

	if a.ResultID != nil {
		t.Fatalf("NewArtifact ResultID = %v, want nil", a.ResultID)
	}
	if a.ID != testArtifactID || a.RunID != testRunID || a.Kind != run.ArtifactAssignment {
		t.Fatalf("NewArtifact did not preserve its identity inputs: %+v", a)
	}
	if a.Path != "runs/r1/artifacts/assignment.md" || a.Digest != "digest-1" {
		t.Fatalf("NewArtifact did not preserve its path/digest inputs: %+v", a)
	}
}

func TestNewResultArtifact(t *testing.T) {
	a := run.NewResultArtifact(testArtifactID, testRunID, testResultID, run.ArtifactCheckStdout, "runs/r1/checks/op1/stdout", "digest-2")

	if a.ResultID == nil || *a.ResultID != testResultID {
		t.Fatalf("NewResultArtifact ResultID = %v, want %v", a.ResultID, testResultID)
	}
	if a.Kind != run.ArtifactCheckStdout {
		t.Fatalf("NewResultArtifact Kind = %s, want %s", a.Kind, run.ArtifactCheckStdout)
	}
}
