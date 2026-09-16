package integration

import (
	"fmt"
	"testing"
)

// The Phase 3 role-prompt mirrors, reproducing internal/app/
// usecase_sessionlaunch.go's renderReviewerInitialPrompt,
// renderReviewerContinuationPrompt, renderManagerInitialPrompt and
// renderManagerContinuationPrompt byte for byte, exactly as
// testAssignmentPrompt / testContinuationPrompt (fixtureworker_test.go)
// mirror the two solo shapes: those functions are unexported, and the
// fixture principals (slice 7) are standalone generated programs that
// cannot import internal/app, so a scenario asserts against these
// mirrors. internal/app's TestGoldenRolePrompts pins the rendered side
// against the same golden bytes, and TestRolePromptMirrors below pins
// this side, so a drift in either place fails a test before the two can
// desynchronize silently.

// testReviewerInitialPrompt mirrors renderReviewerInitialPrompt.
func testReviewerInitialPrompt(assignmentPath, hopPath string) string {
	return fmt.Sprintf("Read your review assignment at %s and evaluate the frozen subject it names. "+
		"When your review is complete, submit your verdict by running: %s review submit --verdict <approve|reject> --subject <commit-oid> --reasons-file <absolute path>. "+
		"If the first output line begins with \"transient\", wait briefly and run the exact same command again.",
		assignmentPath, hopPath)
}

// testReviewerContinuationPrompt mirrors renderReviewerContinuationPrompt.
func testReviewerContinuationPrompt(assignmentPath, hopPath string) string {
	return fmt.Sprintf("You were relaunched after an interruption; your restored session may show earlier, unfinished work. "+
		"Re-read your review assignment at %s and continue it. "+
		"When your review is complete, submit your verdict by running: %s review submit --verdict <approve|reject> --subject <commit-oid> --reasons-file <absolute path>. "+
		"If the first output line begins with \"transient\", wait briefly and run the exact same command again.",
		assignmentPath, hopPath)
}

// testManagerInitialPrompt mirrors renderManagerInitialPrompt.
func testManagerInitialPrompt(assignmentPath, rolePath, cribPath, hopPath string) string {
	return fmt.Sprintf("You are this run's manager. Read your assignment at %s, your role instructions at %s and the worker protocol reference at %s before doing anything else; together they are your complete instructions. "+
		"Coordinate the run only through the hop verbs the protocol reference quotes, and poll for messages by running: %s msg wait.",
		assignmentPath, rolePath, cribPath, hopPath)
}

// testManagerContinuationPrompt mirrors renderManagerContinuationPrompt.
func testManagerContinuationPrompt(assignmentPath, rolePath, cribPath, hopPath string) string {
	return fmt.Sprintf("You were relaunched after an interruption; your restored session may show earlier, unfinished work. "+
		"Re-read your assignment at %s, your role instructions at %s and the worker protocol reference at %s, then continue coordinating the run. "+
		"Poll for messages by running: %s msg wait.",
		assignmentPath, rolePath, cribPath, hopPath)
}

// TestRolePromptMirrors pins the four mirrors against the same golden
// literals internal/app's TestGoldenRolePrompts pins the renderers
// against — retyped here, never derived — so editing a mirror without
// its renderer (or the shared golden) fails immediately. No herdr binary
// is needed.
func TestRolePromptMirrors(t *testing.T) {
	const (
		a   = "/state/runs/r/artifacts/assignment.md"
		ro  = "/state/runs/r/artifacts/roles/manager.md"
		cr  = "/state/runs/r/artifacts/worker-protocol.md"
		hop = "/usr/local/bin/hop"
	)
	golden := []struct {
		name string
		got  string
		want string
	}{
		{
			"reviewer initial",
			testReviewerInitialPrompt(a, hop),
			"Read your review assignment at " + a + " and evaluate the frozen subject it names. " +
				"When your review is complete, submit your verdict by running: " + hop + " review submit --verdict <approve|reject> --subject <commit-oid> --reasons-file <absolute path>. " +
				"If the first output line begins with \"transient\", wait briefly and run the exact same command again.",
		},
		{
			"reviewer continuation",
			testReviewerContinuationPrompt(a, hop),
			"You were relaunched after an interruption; your restored session may show earlier, unfinished work. " +
				"Re-read your review assignment at " + a + " and continue it. " +
				"When your review is complete, submit your verdict by running: " + hop + " review submit --verdict <approve|reject> --subject <commit-oid> --reasons-file <absolute path>. " +
				"If the first output line begins with \"transient\", wait briefly and run the exact same command again.",
		},
		{
			"manager initial",
			testManagerInitialPrompt(a, ro, cr, hop),
			"You are this run's manager. Read your assignment at " + a + ", your role instructions at " + ro + " and the worker protocol reference at " + cr + " before doing anything else; together they are your complete instructions. " +
				"Coordinate the run only through the hop verbs the protocol reference quotes, and poll for messages by running: " + hop + " msg wait.",
		},
		{
			"manager continuation",
			testManagerContinuationPrompt(a, ro, cr, hop),
			"You were relaunched after an interruption; your restored session may show earlier, unfinished work. " +
				"Re-read your assignment at " + a + ", your role instructions at " + ro + " and the worker protocol reference at " + cr + ", then continue coordinating the run. " +
				"Poll for messages by running: " + hop + " msg wait.",
		},
	}
	for _, tc := range golden {
		t.Run(tc.name, func(t *testing.T) {
			if tc.got != tc.want {
				t.Errorf("mirror rendered:\n%s\ngolden:\n%s", tc.got, tc.want)
			}
		})
	}
}
