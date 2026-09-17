package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/johnlanda/hop/internal/app"
	"github.com/johnlanda/hop/internal/testsupport/hopfixtures"
)

// addActiveChild seeds one active task of kind ("implement" or "review")
// with a bound child session whose attempt is running — or, with running
// false, left launching with no settled launch claim (the early-submission
// window).
func (f *featureManager) addActiveChild(t *testing.T, seed int, kind, subjectCommitOID, subjectTreeOID string, running bool) childSession {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	var (
		taskID string
		role   string
		err    error
	)
	switch kind {
	case "implement":
		role = "implementer"
		taskID, err = hopfixtures.SeedImplementTask(ctx, f.store, f.lease, f.RunID, seed, seed, "submit a result", "active", now)
	case "review":
		role = "reviewer"
		taskID, err = hopfixtures.SeedReviewTask(ctx, f.store, f.lease, f.RunID, seed, seed, subjectCommitOID, subjectTreeOID, "active", now)
	default:
		t.Fatalf("unknown task kind %q", kind)
	}
	if err != nil {
		t.Fatalf("seed %s task: %v", kind, err)
	}
	sessionID, incarnationID, attemptID, err := hopfixtures.SeedChildSession(ctx, f.store, f.lease, f.RunID, taskID, f.ManagerID, role, seed+10, now)
	if err != nil {
		t.Fatalf("seed child session: %v", err)
	}
	advance := hopfixtures.LaunchAttempt
	if running {
		advance = hopfixtures.RunAttempt
	}
	if err := advance(ctx, f.store, f.lease, attemptID, now); err != nil {
		t.Fatalf("advance child attempt: %v", err)
	}
	return childSession{TaskID: taskID, AttemptID: attemptID, SessionID: sessionID, IncarnationID: incarnationID}
}

// requireTransientOnly proves an invocation printed exactly one retry line
// on stdout, exited 1 and wrote exactly one diagnostic line to stderr under
// verb's prefix.
func requireTransientOnly(t *testing.T, label, verb string, result hopResult, line string) {
	t.Helper()
	if result.Stdout != line+"\n" || result.ExitCode != exitFailure {
		t.Fatalf("%s: exit=%d stdout=%q stderr=%q, want exit %d and stdout exactly %q", label, result.ExitCode, result.Stdout, result.Stderr, exitFailure, line+"\n")
	}
	stderrLines := strings.Split(strings.TrimSuffix(result.Stderr, "\n"), "\n")
	if len(stderrLines) != 1 || !strings.HasPrefix(stderrLines[0], verb+": ") {
		t.Fatalf("%s: stderr = %q, want one %q diagnostic line", label, result.Stderr, verb+": ")
	}
}

// TestGrammarContractResultSubmitTransientReasons drives hop result
// submit's two retryable first lines through the real binary, each chosen
// by the store's typed reason: in a feature run, a message queued to the
// worker's task makes the submission print exactly the drain line — and
// once the worker fetches and acks it, the identical resubmission is
// accepted — while a worker whose attempt is still launching with no
// settled claim prints exactly the not-running line.
func TestGrammarContractResultSubmitTransientReasons(t *testing.T) {
	commit := strings.Repeat("c", 40)
	submit := func(t *testing.T, f *featureManager, worker childSession) hopResult {
		t.Helper()
		return execHop(t, worker.env(f, nil), f.StateRoot, "result", "submit", "--summary", "implemented the fixture change", "--commit", commit)
	}

	t.Run("undelivered messages", func(t *testing.T) {
		f := newFeatureManager(t, 9600, defaultMessageWait)
		worker := f.addActiveChild(t, 9700, "implement", "", "", true)
		infoID := requireSentOnly(t, "msg send (manager info to the task)", execHop(t, f.env(nil), f.StateRoot, "msg", "send", "--to", "task:"+worker.TaskID, "--kind", "info", "--body", "read this first"))

		first := submit(t, f, worker)
		requireTransientOnly(t, "result submit with a queued message", "hop result submit", first, "transient: undelivered messages; drain with hop msg next, ack, then resubmit")
		if first.Stdout != app.GrammarTransientUndeliveredLine+"\n" {
			t.Fatalf("stdout = %q, want the grammar constant %q", first.Stdout, app.GrammarTransientUndeliveredLine)
		}

		next := execHop(t, worker.env(f, nil), f.StateRoot, "msg", "next")
		if want := app.GrammarMessageLine(infoID, "info", f.ManagerID, "", "", ""); next.FirstStdoutLine() != want {
			t.Fatalf("msg next first line = %q, want %q; stderr=%q", next.FirstStdoutLine(), want, next.Stderr)
		}
		if ack := execHop(t, worker.env(f, nil), f.StateRoot, "msg", "ack", infoID); ack.Stdout != app.GrammarAckAcceptedLine(infoID)+"\n" {
			t.Fatalf("msg ack: exit=%d stdout=%q stderr=%q", ack.ExitCode, ack.Stdout, ack.Stderr)
		}

		accepted := submit(t, f, worker)
		resultID, found := strings.CutPrefix(strings.TrimSuffix(accepted.Stdout, "\n"), "accepted ")
		if !found || !uuidShape.MatchString(resultID) || accepted.ExitCode != exitOK || accepted.Stderr != "" {
			t.Fatalf("resubmit after drain: exit=%d stdout=%q stderr=%q, want exactly one accepted <result-uuid> line", accepted.ExitCode, accepted.Stdout, accepted.Stderr)
		}
	})

	// The not-running line was the only line this command ever printed;
	// this subtest guards the typed-reason plumbing that now selects it.
	t.Run("attempt not yet running", func(t *testing.T) {
		f := newFeatureManager(t, 9800, defaultMessageWait)
		worker := f.addActiveChild(t, 9900, "implement", "", "", false)

		for _, attempt := range []string{"first", "retried while still launching"} {
			result := submit(t, f, worker)
			requireTransientOnly(t, "result submit ("+attempt+")", "hop result submit", result, app.GrammarTransientNotRunningLine)
		}
	})
}

// TestGrammarContractReviewSubmitNotRunning proves hop review submit's
// not-running retry line through the real binary: a reviewer whose review
// attempt is still launching with no settled claim, over an EMPTY mailbox,
// prints exactly `transient: attempt not yet running; retry` — never the
// drain line, which would send it draining a mailbox that holds nothing —
// with the store's detail on stderr.
func TestGrammarContractReviewSubmitNotRunning(t *testing.T) {
	repoRoot, commitOID := initFixtureGitRepo(t)
	treeOID := gitRevParseTree(t, repoRoot, commitOID)
	f := newFeatureManagerAt(t, 10000, defaultMessageWait, repoRoot)
	reviewer := f.addActiveChild(t, 10100, "review", commitOID, treeOID, false)
	reasons := writeReasonsFile(t, f.StateRoot)

	for _, attempt := range []string{"first", "retried while still launching"} {
		result := execHop(t, reviewer.env(f, nil), f.StateRoot, "review", "submit", "--verdict", "approve", "--subject", commitOID, "--reasons-file", reasons)
		requireTransientOnly(t, "review submit ("+attempt+")", "hop review submit", result, "transient: attempt not yet running; retry")
		if !strings.Contains(result.Stderr, "attempt not yet running") {
			t.Fatalf("review submit (%s): stderr = %q, want the store's not-running detail", attempt, result.Stderr)
		}
	}
	none := execHop(t, reviewer.env(f, nil), f.StateRoot, "msg", "next")
	if none.Stdout != app.GrammarMsgNoneLine+"\n" {
		t.Fatalf("msg next (reviewer) = %q, want the empty-queue line: the mailbox held nothing to drain", none.Stdout)
	}
}
