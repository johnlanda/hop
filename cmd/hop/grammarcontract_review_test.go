package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/johnlanda/hop/internal/app"
)

// initFixtureGitRepo creates a minimal git repository with one commit at
// a fresh realDir, returning the repository root and that commit's
// object id. hop review submit's fixtures need a REAL, on-disk git
// repository: SubmitReviewVerdict resolves --subject's tree object id by
// running `git rev-parse <subject>^{tree}` against the run's recorded
// repository root (confirmed by reading internal/app/usecase_review.go),
// before ever reaching ReviewStore. The repo's own HOME and author/
// committer identity are isolated from the operator's real git
// configuration; nothing here touches ~/.gitconfig.
func initFixtureGitRepo(t *testing.T) (repoRoot, commitOID string) {
	t.Helper()
	dir := realDir(t)
	gitHome := t.TempDir()
	runGit := func(args ...string) string {
		t.Helper()
		cmd := exec.CommandContext(context.Background(), "git", args...) //nolint:gosec // G204: a fixed git invocation against this test's own throwaway repository.
		cmd.Dir = dir
		cmd.Env = []string{
			"PATH=" + os.Getenv("PATH"),
			"HOME=" + gitHome,
			"GIT_AUTHOR_NAME=hop-fixture", "GIT_AUTHOR_EMAIL=hop-fixture@example.invalid",
			"GIT_COMMITTER_NAME=hop-fixture", "GIT_COMMITTER_EMAIL=hop-fixture@example.invalid",
		}
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return strings.TrimSpace(string(out))
	}
	runGit("init", "-q")
	runGit("config", "commit.gpgsign", "false")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("hop fixture\n"), 0o600); err != nil {
		t.Fatalf("write fixture repo file: %v", err)
	}
	runGit("add", "README.md")
	runGit("commit", "-q", "-m", "fixture commit")
	return dir, runGit("rev-parse", "HEAD")
}

// commitFixtureChange adds one more commit to the repository at dir
// (initFixtureGitRepo's own isolated identity), returning its object id —
// used only to build a SECOND, genuinely resolvable commit distinct from
// the review task's frozen subject, so a subject-mismatch refusal is
// reached through a real `git rev-parse` success, not a resolution
// failure (which the app layer classifies as malformed instead).
func commitFixtureChange(t *testing.T, dir string) string {
	t.Helper()
	gitHome := t.TempDir()
	runGit := func(args ...string) string {
		t.Helper()
		cmd := exec.CommandContext(context.Background(), "git", args...) //nolint:gosec // G204: a fixed git invocation against this test's own throwaway repository.
		cmd.Dir = dir
		cmd.Env = []string{
			"PATH=" + os.Getenv("PATH"),
			"HOME=" + gitHome,
			"GIT_AUTHOR_NAME=hop-fixture", "GIT_AUTHOR_EMAIL=hop-fixture@example.invalid",
			"GIT_COMMITTER_NAME=hop-fixture", "GIT_COMMITTER_EMAIL=hop-fixture@example.invalid",
		}
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return strings.TrimSpace(string(out))
	}
	if err := os.WriteFile(filepath.Join(dir, "second.md"), []byte("second change\n"), 0o600); err != nil {
		t.Fatalf("write second fixture repo file: %v", err)
	}
	runGit("add", "second.md")
	runGit("commit", "-q", "-m", "second fixture commit")
	return runGit("rev-parse", "HEAD")
}

// gitRevParseTree resolves subject's tree object id in the repo at dir —
// the same computation SubmitReviewVerdict performs server-side, used
// here only to build a review fixture's frozen SubjectTreeOID.
func gitRevParseTree(t *testing.T, dir, subject string) string {
	t.Helper()
	cmd := exec.CommandContext(context.Background(), "git", "rev-parse", subject+"^{tree}") //nolint:gosec // G204: a fixed git invocation against this test's own throwaway repository.
	cmd.Dir = dir
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir()}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git rev-parse %s^{tree}: %v\n%s", subject, err, out)
	}
	return strings.TrimSpace(string(out))
}

// writeReasonsFile writes a readable reasons file under dir, returning
// its absolute path.
func writeReasonsFile(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "reasons.md")
	if err := os.WriteFile(path, []byte("looks good"), 0o600); err != nil {
		t.Fatalf("write reasons fixture: %v", err)
	}
	return path
}

// TestGrammarContractReviewSubmitUsage covers every hop review submit
// usage-error branch (--verdict/--subject/--reasons-file required) and
// hop review's own dispatch usage (no or unknown subcommand); all
// validated before any store is opened, so no fixture is needed beyond a
// readable reasons file.
func TestGrammarContractReviewSubmitUsage(t *testing.T) {
	dir := freshStateDir(t)
	reasons := writeReasonsFile(t, dir)
	cases := []struct {
		name string
		args []string
	}{
		{"missing verdict", []string{"review", "submit", "--subject", strings.Repeat("a", 40), "--reasons-file", reasons}},
		{"missing subject", []string{"review", "submit", "--verdict", "approve", "--reasons-file", reasons}},
		{"missing reasons-file", []string{"review", "submit", "--verdict", "approve", "--subject", strings.Repeat("a", 40)}},
		{"unexpected argument", []string{"review", "submit", "--verdict", "approve", "--subject", strings.Repeat("a", 40), "--reasons-file", reasons, "extra"}},
		{"dispatch: no subcommand", []string{"review"}},
		{"dispatch: unknown subcommand", []string{"review", "bogus"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result := execHop(t, map[string]string{"HOP_STATE_DIR": dir}, dir, tc.args...)
			if result.ExitCode != exitUsage {
				t.Fatalf("exit = %d, want %d (usage); stdout=%q stderr=%q", result.ExitCode, exitUsage, result.Stdout, result.Stderr)
			}
		})
	}
}

// TestGrammarContractReviewSubmitReasonsFileUnreadable proves an
// unreadable --reasons-file is a command failure, not a usage error.
func TestGrammarContractReviewSubmitReasonsFileUnreadable(t *testing.T) {
	dir := freshStateDir(t)
	result := execHop(t, map[string]string{"HOP_STATE_DIR": dir}, dir, "review", "submit", "--verdict", "approve", "--subject", strings.Repeat("a", 40), "--reasons-file", filepath.Join(dir, "does-not-exist.md"))
	if result.ExitCode != exitFailure {
		t.Fatalf("exit = %d, want %d (failure); stdout=%q stderr=%q", result.ExitCode, exitFailure, result.Stdout, result.Stderr)
	}
	if result.Stdout != "" {
		t.Errorf("unreadable reasons file wrote to stdout: %q", result.Stdout)
	}
}

// TestGrammarContractReviewSubmitMalformed drives every hop review
// submit `refused: malformed` shape reachable with zero fixture seeding
// (SubmitReviewVerdict's own parse/bounds validation runs entirely
// before any store call): malformed HOP_* identities, an invalid
// verdict, and a subject that does not resolve as a git object in the
// current working directory (used as the repository root here — no
// store is ever reached, so which repository is irrelevant to this
// shape).
func TestGrammarContractReviewSubmitMalformed(t *testing.T) {
	dir := freshStateDir(t)
	reasons := writeReasonsFile(t, dir)
	validEnv := map[string]string{
		"HOP_STATE_DIR":      dir,
		"HOP_RUN_ID":         testUUID(1),
		"HOP_TASK_ID":        testUUID(2),
		"HOP_ATTEMPT_ID":     testUUID(3),
		"HOP_SESSION_ID":     testUUID(4),
		"HOP_INCARNATION_ID": testUUID(5),
	}
	want := app.GrammarRefusalLine(app.GrammarReasonMalformed)

	for _, key := range []string{"HOP_RUN_ID", "HOP_TASK_ID", "HOP_ATTEMPT_ID", "HOP_SESSION_ID", "HOP_INCARNATION_ID"} {
		t.Run(key+"_malformed", func(t *testing.T) {
			env := map[string]string{}
			for k, v := range validEnv {
				env[k] = v
			}
			env[key] = malformedUUID
			result := execHop(t, env, dir, "review", "submit", "--verdict", "approve", "--subject", strings.Repeat("a", 40), "--reasons-file", reasons)
			if got := result.FirstStdoutLine(); got != want {
				t.Errorf("first line = %q, want %q; stdout=%q stderr=%q", got, want, result.Stdout, result.Stderr)
			}
			if result.ExitCode != exitFailure {
				t.Errorf("exit = %d, want %d", result.ExitCode, exitFailure)
			}
		})
	}

	t.Run("invalid verdict", func(t *testing.T) {
		result := execHop(t, validEnv, dir, "review", "submit", "--verdict", "maybe", "--subject", strings.Repeat("a", 40), "--reasons-file", reasons)
		if got := result.FirstStdoutLine(); got != want {
			t.Errorf("first line = %q, want %q; stdout=%q stderr=%q", got, want, result.Stdout, result.Stderr)
		}
	})
}

// TestGrammarContractReviewSubmitSubjectDoesNotResolve proves the
// `refused: malformed` shape for a --subject that does not resolve as a
// git object: unlike the parse/bounds failures above, this check runs
// AFTER LoadFrozenRun succeeds (confirmed empirically: against a
// nonexistent run it is a plain "load frozen run" command failure, not a
// refusal line — see usecase_review.go's call order), so it needs a real
// seeded run.
func TestGrammarContractReviewSubmitSubjectDoesNotResolve(t *testing.T) {
	rf := newReviewFixture(t, 7600)
	reasons := writeReasonsFile(t, rf.StateRoot)

	result := rf.submit(t, "approve", "not-a-commit", reasons)
	want := app.GrammarRefusalLine(app.GrammarReasonMalformed)
	if got := result.FirstStdoutLine(); got != want {
		t.Errorf("first line = %q, want %q; stdout=%q stderr=%q", got, want, result.Stdout, result.Stderr)
	}
	if result.ExitCode != exitFailure {
		t.Errorf("exit = %d, want %d", result.ExitCode, exitFailure)
	}
}

// reviewFixture seeds a running feature-mode run with a review task and
// its reviewer session, subject frozen to a real git commit.
type reviewFixture struct {
	*featureManager
	reviewer      childSession
	repoRoot      string
	subjectCommit string
	subjectTree   string
}

// newReviewFixture builds a review fixture: a real git repository with
// one commit, a feature-mode run rooted there, and a review task whose
// subject is frozen to that commit.
func newReviewFixture(t *testing.T, seed int) reviewFixture {
	t.Helper()
	repoRoot, commitOID := initFixtureGitRepo(t)
	treeOID := gitRevParseTree(t, repoRoot, commitOID)
	f := newFeatureManagerAt(t, seed, defaultMessageWait, repoRoot)
	reviewer := f.addReviewTask(t, seed+100, commitOID, treeOID)
	return reviewFixture{featureManager: f, reviewer: reviewer, repoRoot: repoRoot, subjectCommit: commitOID, subjectTree: treeOID}
}

// submit runs hop review submit as the fixture's reviewer session.
func (rf reviewFixture) submit(t *testing.T, verdict, subject, reasons string) hopResult { //nolint:gocritic // hugeParam: reviewFixture is a small fixture value read once per call, never a hot loop; mirrors this codebase's own convention for domain-shaped DTOs passed by value.
	t.Helper()
	return execHop(t, rf.reviewer.env(rf.featureManager, nil), rf.StateRoot, "review", "submit", "--verdict", verdict, "--subject", subject, "--reasons-file", reasons)
}

// TestGrammarContractReviewSubmitAccepted drives hop review submit's
// accepted, duplicate (an identical resubmission) and conflicting (a
// different verdict against the same subject) outcomes.
func TestGrammarContractReviewSubmitAccepted(t *testing.T) {
	rf := newReviewFixture(t, 7000)
	reasons := writeReasonsFile(t, rf.StateRoot)

	accepted := rf.submit(t, "approve", rf.subjectCommit, reasons)
	if !strings.HasPrefix(accepted.FirstStdoutLine(), "verdict accepted ") {
		t.Fatalf("accepted first line = %q, want a \"verdict accepted \" prefix; stdout=%q stderr=%q", accepted.FirstStdoutLine(), accepted.Stdout, accepted.Stderr)
	}
	if accepted.ExitCode != exitOK {
		t.Errorf("accepted exit = %d, want %d", accepted.ExitCode, exitOK)
	}
	reviewID := strings.TrimPrefix(accepted.FirstStdoutLine(), "verdict accepted ")

	duplicate := rf.submit(t, "approve", rf.subjectCommit, reasons)
	if want := app.GrammarVerdictDuplicateLine(reviewID); duplicate.FirstStdoutLine() != want {
		t.Errorf("duplicate first line = %q, want %q; stdout=%q stderr=%q", duplicate.FirstStdoutLine(), want, duplicate.Stdout, duplicate.Stderr)
	}

	conflicting := rf.submit(t, "reject", rf.subjectCommit, reasons)
	want := app.GrammarRefusalLine(app.GrammarReasonConflicting)
	if got := conflicting.FirstStdoutLine(); got != want {
		t.Errorf("conflicting first line = %q, want %q; stdout=%q stderr=%q", got, want, conflicting.Stdout, conflicting.Stderr)
	}
	if conflicting.ExitCode != exitFailure {
		t.Errorf("conflicting exit = %d, want %d", conflicting.ExitCode, exitFailure)
	}
}

// TestGrammarContractReviewSubmitStale drives hop review submit's
// `refused: stale` shape two ways: a subject that resolves as a real git
// commit but does not match the review task's frozen subject, and a
// caller session that is not the review task's reviewer.
func TestGrammarContractReviewSubmitStale(t *testing.T) {
	rf := newReviewFixture(t, 7200)
	reasons := writeReasonsFile(t, rf.StateRoot)
	want := app.GrammarRefusalLine(app.GrammarReasonStale)

	t.Run("subject mismatch", func(t *testing.T) {
		otherCommit := commitFixtureChange(t, rf.repoRoot)
		result := rf.submit(t, "approve", otherCommit, reasons)
		if got := result.FirstStdoutLine(); got != want {
			t.Errorf("first line = %q, want %q; stdout=%q stderr=%q", got, want, result.Stdout, result.Stderr)
		}
	})

	t.Run("non-reviewer caller", func(t *testing.T) {
		env := rf.reviewer.env(rf.featureManager, map[string]string{
			"HOP_SESSION_ID":     rf.ManagerID,
			"HOP_INCARNATION_ID": rf.ManagerIncarnation,
		})
		result := execHop(t, env, rf.StateRoot, "review", "submit", "--verdict", "approve", "--subject", rf.subjectCommit, "--reasons-file", reasons)
		if got := result.FirstStdoutLine(); got != want {
			t.Errorf("first line = %q, want %q; stdout=%q stderr=%q", got, want, result.Stdout, result.Stderr)
		}
	})
}

// TestGrammarContractReviewSubmitTransient proves hop review submit's
// fixed transient retry line when the reviewer session has an undelivered
// message queued (section 5's drain-before-submit mailbox rule): the
// store's own Detail goes to stderr, never the stdout protocol line.
func TestGrammarContractReviewSubmitTransient(t *testing.T) {
	rf := newReviewFixture(t, 7400)
	reasons := writeReasonsFile(t, rf.StateRoot)

	send := execHop(t, rf.env(nil), rf.StateRoot, "msg", "send", "--to", "task:"+rf.reviewer.TaskID, "--kind", "info", "--body", "fyi")
	if send.ExitCode != exitOK {
		t.Fatalf("msg send (queue a message): exit=%d stdout=%q stderr=%q", send.ExitCode, send.Stdout, send.Stderr)
	}

	result := rf.submit(t, "approve", rf.subjectCommit, reasons)
	if want := app.GrammarTransientUndeliveredLine; result.FirstStdoutLine() != want {
		t.Errorf("first line = %q, want %q; stdout=%q stderr=%q", result.FirstStdoutLine(), want, result.Stdout, result.Stderr)
	}
	if result.ExitCode != exitFailure {
		t.Errorf("exit = %d, want %d", result.ExitCode, exitFailure)
	}
}
