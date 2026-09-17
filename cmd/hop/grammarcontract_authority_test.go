package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/johnlanda/hop/internal/app"
	"github.com/johnlanda/hop/internal/testsupport/hopfixtures"
)

// TestGrammarContractResultSubmitSupersededIncarnationIsStale drives, through
// the real binary, a worker whose binding was superseded with no successor
// while a message is queued to its task. `hop result submit` prints the
// final `stale: …` line and exits 1, on the first submission and on a
// retry — never the drain line, which would loop a caller whose own
// `hop msg next` is refused.
func TestGrammarContractResultSubmitSupersededIncarnationIsStale(t *testing.T) {
	f := newFeatureManager(t, 10200, defaultMessageWait)
	worker := f.addActiveChild(t, 10300, "implement", "", "", true)
	requireSentOnly(t, "msg send (manager info to the task)", execHop(t, f.env(nil), f.StateRoot, "msg", "send", "--to", "task:"+worker.TaskID, "--kind", "info", "--body", "read this first"))
	if err := hopfixtures.SupersedeBinding(context.Background(), f.store, f.lease, worker.SessionID, time.Now().UTC()); err != nil {
		t.Fatalf("supersede worker binding: %v", err)
	}

	for _, attempt := range []string{"first", "retried"} {
		result := execHop(t, worker.env(f, nil), f.StateRoot, "result", "submit", "--summary", "implemented the fixture change", "--commit", strings.Repeat("c", 40))
		if result.ExitCode != exitFailure || !strings.HasPrefix(result.Stdout, "stale: ") || strings.Count(result.Stdout, "\n") != 1 || result.Stderr != "" {
			t.Fatalf("result submit (%s): exit=%d stdout=%q stderr=%q, want exit %d and exactly one `stale: ` line", attempt, result.ExitCode, result.Stdout, result.Stderr, exitFailure)
		}
		if strings.HasPrefix(result.Stdout, app.GrammarTransientPrefix) {
			t.Fatalf("result submit (%s): stdout = %q, a stale caller must never get a retry line", attempt, result.Stdout)
		}
	}

	next := execHop(t, worker.env(f, nil), f.StateRoot, "msg", "next")
	if next.ExitCode != exitFailure || next.Stdout != "" || !strings.HasPrefix(next.Stderr, "hop msg next: ") {
		t.Fatalf("msg next (superseded worker): exit=%d stdout=%q stderr=%q, want a refusal on stderr only", next.ExitCode, next.Stdout, next.Stderr)
	}
}
