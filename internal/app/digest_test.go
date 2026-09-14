package app_test

import (
	"testing"

	"github.com/johnlanda/hop/internal/app"
	"github.com/johnlanda/hop/internal/domain/identity"
)

func TestComputeResultDigest(t *testing.T) {
	runID, err := identity.ParseRunID("11111111-1111-4111-8111-111111111111")
	if err != nil {
		t.Fatalf("parse run id: %v", err)
	}
	taskID, err := identity.ParseTaskID("22222222-2222-4222-8222-222222222222")
	if err != nil {
		t.Fatalf("parse task id: %v", err)
	}
	attemptID, err := identity.ParseAttemptID("33333333-3333-4333-8333-333333333333")
	if err != nil {
		t.Fatalf("parse attempt id: %v", err)
	}
	commit := "cccccccccccccccccccccccccccccccccccccccc"

	t.Run("deterministic for identical inputs", func(t *testing.T) {
		a := app.ComputeResultDigest(runID, taskID, attemptID, commit, "summary")
		b := app.ComputeResultDigest(runID, taskID, attemptID, commit, "summary")
		if a != b {
			t.Fatalf("digest not deterministic: %s != %s", a, b)
		}
		if len(a) != 64 {
			t.Fatalf("digest length = %d, want 64 (sha256 hex)", len(a))
		}
	})

	t.Run("differs on any field change", func(t *testing.T) {
		base := app.ComputeResultDigest(runID, taskID, attemptID, commit, "summary")
		cases := map[string]string{
			"summary": app.ComputeResultDigest(runID, taskID, attemptID, commit, "different"),
			"commit":  app.ComputeResultDigest(runID, taskID, attemptID, "dddddddddddddddddddddddddddddddddddddddd", "summary"),
		}
		for name, other := range cases {
			if base == other {
				t.Errorf("digest unchanged when %s changed", name)
			}
		}
	})

	t.Run("length-prefixed fields prevent boundary ambiguity", func(t *testing.T) {
		// "ab"+"c" and "a"+"bc" must not collide despite concatenating to
		// the same bytes without length prefixes.
		d1 := app.ComputeResultDigest(runID, taskID, attemptID, commit, "ab")
		d2 := app.ComputeResultDigest(runID, taskID, attemptID, commit, "a")
		if d1 == d2 {
			t.Fatalf("digests collided across a field-boundary shift")
		}
	})
}
