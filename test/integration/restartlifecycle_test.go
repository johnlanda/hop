package integration

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/johnlanda/hop/internal/adapters/herdr"
	"github.com/johnlanda/hop/internal/app"
)

// orphanResumeRef is the native session reference this scenario reports to
// Herdr for every orchestrated pane, so Herdr builds its own deferred
// restore plan for them. It is deliberately a value HOP NEVER uses: HOP's
// own cold relaunch resumes a session's real recorded reference, so any
// process carrying THIS one is necessarily Herdr's own restore firing
// behind HOP's back, and never HOP's relaunch. That is what makes the
// no-orphan assertion decidable rather than a judgement about argv shapes.
const orphanResumeRef = "11111111-2222-4333-8444-555555555555"

// reportOrchestratedAgentSession records a native agent session against one
// of HOP's own panes through Herdr's own pane.report_agent_session — the
// call a real harness integration makes for a live conversation. Without it
// Herdr has nothing to restore and the orphan half of this scenario would
// assert the absence of something that was never armed.
func reportOrchestratedAgentSession(t *testing.T, server *testServer, paneID string) {
	t.Helper()
	server.call(t, "pane.report_agent_session", map[string]any{
		"pane_id":          paneID,
		"source":           "herdr:claude",
		"agent":            "claude",
		"agent_session_id": orphanResumeRef,
		"seq":              1,
	}, &struct{}{})
}

// requireNoOrphanResume fails if ANY process on the machine carries the
// orphan resume reference in its command line. The scan is a plain `ps`
// read — never a signal, and never keyed on a pid this test observed — and
// the reference is unique to this run, so a match is unambiguous evidence
// that Herdr restored an agent HOP does not know about.
func requireNoOrphanResume(t *testing.T, when string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ps", "-A", "-o", "pid=,args=").Output()
	if err != nil {
		t.Fatalf("ps -A -o pid=,args=: %v", err)
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.Contains(line, orphanResumeRef) {
			t.Fatalf("a process carrying the orphan resume reference exists %s: %q", when, strings.TrimSpace(line))
		}
	}
}

// orchestratedSession is one session HOP placed before the restart: what it
// was, and the pane and native reference it recorded.
type orchestratedSession struct {
	Role      string
	SessionID string
	PaneID    string
	NativeRef string
}

// sessionsBeforeRestart reads every non-terminal session of the run with a
// current placement, straight from the run's own store.
func sessionsBeforeRestart(t *testing.T, fx *featureRun) []orchestratedSession {
	t.Helper()
	rows := fx.scalar(t, fmt.Sprintf(
		"SELECT s.role || '|' || s.id || '|' || COALESCE(b.pane_id, '') || '|' || COALESCE(s.native_session_ref, '') "+
			"FROM sessions s LEFT JOIN runtime_bindings b ON b.session_id = s.id AND b.superseded_at IS NULL "+
			"WHERE s.run_id = '%s' AND s.state NOT IN ('terminated', 'lost') ORDER BY s.role, s.id;", fx.runID))
	var sessions []orchestratedSession
	for _, line := range strings.Split(rows, "\n") {
		fields := strings.Split(strings.TrimSpace(line), "|")
		if len(fields) != 4 || fields[2] == "" {
			continue
		}
		sessions = append(sessions, orchestratedSession{Role: fields[0], SessionID: fields[1], PaneID: fields[2], NativeRef: fields[3]})
	}
	return sessions
}

// requirePathResolvesClaudeToTheFixture proves, in a scratch pane of this
// server (never one HOP owns, whose input belongs to its agent), that a
// pane LOGIN shell resolves a bare `claude` to this test's own fixture
// principal. This scenario deliberately arms Herdr's deferred restore,
// which types a bare `claude` into a restored shell that no test controls,
// so if resolution went to the real harness installed on a developer
// machine this test would run it. The PATH prepend written into the
// temporary HOME is the arrangement; this is the check that it held.
func requirePathResolvesClaudeToTheFixture(t *testing.T, server *testServer, artifacts *artifactDir, runtime *herdr.Runtime) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), callTimeout)
	workspace, err := runtime.CreateWorkspace(ctx, app.WorkspaceRequest{
		Cwd: server.workDir(), Label: "hop-restart-pathcheck-" + newSpikeUUID(t),
	})
	cancel()
	if err != nil {
		t.Fatalf("CreateWorkspace for the PATH check: %v", err)
	}
	requirePaneShellResolves(t, server, artifacts, workspace.PaneID, "claude", filepath.Join(server.base, "bin", "claude"))
}

// TestRealProcessRestartClosesAndRelaunchesSessions is the restart-
// lifecycle scenario (docs/plan/phase-3-design.md sections 4 and 6): a
// feature run with a LIVE MANAGER and a live worker survives a real Herdr
// restart because HOP closes the panes it owns and cold-relaunches those
// sessions from their recorded native session references.
//
// Before the fix, every one of those sessions was unreconcilable forever:
// the placement's server lifetime was gone, so absence could never be
// concluded, the slot never freed and the run never progressed.
//
// The scenario arms Herdr's OWN deferred restore for each orchestrated pane
// first (pane.report_agent_session), so the thing HOP must prevent is
// actually armed: without it, "no orphan agent appeared" would assert the
// absence of something never set up. The reference reported is one HOP
// never uses, so any process carrying it is Herdr's restore and never HOP's
// relaunch.
//
// It asserts, each independently:
//   - every session placed before the restart is CLOSED — its recorded pane
//     answers nothing afterwards — and its row reaches a terminal state;
//   - each is relaunched as a SUCCESSOR bound to the SAME native session
//     reference, on the same attempt, with its own new session id;
//   - the run continues rather than stalling, and reaches completion;
//   - the MANAGER LINEAGE is intact: no replan, no second plan close, no
//     duplicate tasks, and the relaunched manager — identified by its own
//     new session id — performs a section 7 duty afterwards;
//   - no orphan resume exists at any point.
func TestRealProcessRestartClosesAndRelaunchesSessions(t *testing.T) {
	artifacts := newArtifactDir(t)
	server := prepareServer(t, artifacts)
	worker := buildFixtureWorker(t, artifacts)
	installFixtureWorkerAsClaudeStub(t, server, worker)
	// A restored pane's login shell runs path_helper, which puts the
	// system directories ahead of the inherited hermetic PATH, so the
	// fixture directory is re-prepended where a login shell will read it.
	// requirePathResolvesClaudeToTheFixture below proves it took.
	prepend := "PATH=\"" + filepath.Join(server.base, "bin") + ":$PATH\"\nexport PATH\n"
	for _, name := range []string{".profile", ".zprofile", ".zshrc"} {
		if err := os.WriteFile(filepath.Join(server.homeDir(), name), []byte(prepend), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	server.start(t)
	runtime := herdr.NewRuntime(server.socketPath)

	scratchDir := artifacts.dir(t, "fixture-scratch")
	repo := newFeatureFixtureRepo(t, artifacts, server, "repo", featureFixtureOptions{
		ScratchDir: scratchDir, ReviewerBehavior: "reviewer-approve",
		MaxWorkers: 1, RetryLimit: 3, MessageWaitTimeout: "3s", MessageAttentionAfter: "30s",
	})
	brief := fixtureManagerBrief(scratchDir,
		[]fixtureManagerTask{{Label: "t1", Title: "Implement t1", Behavior: "worker-hold"}},
		[]fixtureManagerAnswer{{Match: fixtureHoldMarker, Action: "relay"}},
		"",
	)
	fx := startFeatureRun(t, artifacts, server, repo, scratchDir, brief)

	// The worker holds at its own barrier, so it is provably alive and
	// cannot progress while the restart happens — no race against its own
	// submission.
	t1 := fx.requireTaskBySeq(t, 1)
	fx.requireTaskState(t, t1, "active")
	attemptID, _ := fx.currentAttempt(t, t1)
	workerSession := fx.sessionForAttempt(t, attemptID)
	managerSession := fx.managerSessionID(t)
	// The restart waits for the worker's SESSION to be active, not merely
	// its task: a task is active the moment it is ASSIGNED, while the
	// session stays launching until its launch claim is corroborated. A
	// restart in that window is a real case — the rule settles such a launch
	// rather than resuming it, since a pre-assigned native reference names
	// no transcript — but it is NOT this scenario's case, which is the
	// relaunch of a corroborated agent.
	fx.requireSessionState(t, workerSession, "active")
	fx.requireSessionState(t, managerSession, "active")
	fx.requirePane(t, workerSession)
	fx.requirePane(t, managerSession)

	requirePathResolvesClaudeToTheFixture(t, server, artifacts, runtime)

	// The plan as it stands before the restart: the lineage assertions
	// below are against these exact numbers.
	tasksBefore := fx.scalar(t, fmt.Sprintf("SELECT COUNT(*) FROM tasks WHERE run_id = '%s';", fx.runID))
	planClosesBefore := fx.scalar(t, planCloseQuery(fx.runID))

	placed := sessionsBeforeRestart(t, fx)
	if len(placed) < 2 {
		t.Fatalf("sessions placed before the restart = %+v, want at least the manager and the worker", placed)
	}
	for i := range placed {
		reportOrchestratedAgentSession(t, server, placed[i].PaneID)
	}
	requireNoOrphanResume(t, "before the restart")
	artifacts.save(t, "sessions-before-restart.txt", fmt.Sprintf("%+v\n", placed))

	// The real restart: a graceful stop, the shutdown save, and a fresh
	// server on the same roots.
	server.restart(t)

	// Every placed pane is closed: its recorded id answers nothing. This is
	// what the rule performs, and what was impossible before it.
	for i := range placed {
		session := &placed[i]
		if !waitUntilDeadline(featureRunTimeout, func() bool {
			ctx, cancel := context.WithTimeout(t.Context(), callTimeout)
			defer cancel()
			_, err := runtime.InspectPane(ctx, session.PaneID)
			return err != nil
		}) {
			t.Fatalf("the %s session's recorded pane %s still answers after the restart; it was never closed", session.Role, session.PaneID)
		}
		fx.requireSessionState(t, session.SessionID, "terminated", "lost")
	}

	// Each is relaunched as a successor on the same native reference, with
	// its own new session id.
	var relaunchedManager string
	for i := range placed {
		session := &placed[i]
		successor := successorOf(t, fx, session)
		if successor == session.SessionID {
			t.Fatalf("the %s session's successor is itself (%s)", session.Role, successor)
		}
		if ref := fx.scalar(t, fmt.Sprintf("SELECT COALESCE(native_session_ref, '') FROM sessions WHERE id = '%s';", successor)); ref != session.NativeRef {
			t.Errorf("the %s successor %s resumes %q, want the predecessor's own %q", session.Role, successor, ref, session.NativeRef)
		}
		if session.Role == roleManagerRow {
			relaunchedManager = successor
		}
	}

	// WHAT THE CLOSED PANE HELD, from the scrollback HOP itself captured
	// before each close. This is OBSERVED and logged, never asserted:
	// whether Herdr's restore wins the sub-second race before HOP's close
	// is genuinely racy, and a run where it does and a run where it does
	// not are both correct. What it establishes is that the hazard was
	// really armed rather than hypothetical — and it is the durable
	// evidence a process scan cannot give, since an orphan that has already
	// exited leaves nothing for `ps` to find.
	for i := range placed {
		if scrollback := restartCloseScrollback(t, fx, placed[i].PaneID); strings.Contains(scrollback, orphanResumeRef) {
			t.Logf("the %s session's pane had ALREADY been given Herdr's restored agent when HOP closed it: the close is what ended it", placed[i].Role)
		} else {
			t.Logf("the %s session's pane had not yet been given Herdr's restored agent when HOP closed it: the close prevented it", placed[i].Role)
		}
	}

	// WHICH RUNG carried each close, read from the journal the closes
	// themselves wrote: the scenario names the mechanism, not only the
	// outcome. Every restart close must record one of the two
	// identifications, and a close recording neither would mean a pane was
	// closed on its id alone.
	for i := range placed {
		rung := restartCloseIdentification(t, fx, placed[i].PaneID)
		if rung != "creation label" && rung != "restored harness occupant" {
			t.Errorf("the %s session's pane %s was closed with identification %q, want one of the two rungs", placed[i].Role, placed[i].PaneID, rung)
		}
		t.Logf("the %s session's pane %s was closed, identified by its %s", placed[i].Role, placed[i].PaneID, rung)
	}

	// MANAGER LAST, as OBSERVED: the journal's own ordering of the closes,
	// not the order the code intends.
	requireManagerClosedLast(t, fx, placed)

	requireNoOrphanResume(t, "after the restart and the relaunch")

	// The run continues rather than stalling: the relaunched worker sends
	// its barrier question again, and the RELAUNCHED MANAGER — its own new
	// session id — relays it to the human. That relay is the section 7 duty
	// this scenario requires of it, observed in the store rather than
	// inferred from the run finishing.
	workerSuccessor := successorOf(t, fx, sessionByRole(placed, roleImplementerRow))
	questionID := fx.relayedQuestionFor(t, workerSuccessor)
	sender := fx.scalar(t, fmt.Sprintf("SELECT COALESCE(sender_session_id, '') FROM messages WHERE id = '%s';", questionID))
	if sender != relaunchedManager {
		t.Errorf("the relayed question was sent by %q, want the RELAUNCHED manager %q", sender, relaunchedManager)
	}

	// The lineage is intact: the manager did not replan.
	if got := fx.scalar(t, fmt.Sprintf("SELECT COUNT(*) FROM tasks WHERE run_id = '%s';", fx.runID)); got != tasksBefore {
		t.Errorf("tasks after the relaunch = %s, want the same %s: a relaunched manager must not replan", got, tasksBefore)
	}
	if got := fx.scalar(t, planCloseQuery(fx.runID)); got != planClosesBefore {
		t.Errorf("accepted plan closes after the relaunch = %s, want the same %s: a relaunched manager must not close the plan again", got, planClosesBefore)
	}

	// Released, the run finishes the ordinary way.
	fx.answerHuman(t, questionID, "continue")
	fx.requireRunState(t, "completed")
	requireNoOrphanResume(t, "after the run completed")
	if got := fx.scalar(t, fmt.Sprintf("SELECT COUNT(*) FROM tasks WHERE run_id = '%s' AND kind = 'implement';", fx.runID)); got != "1" {
		t.Errorf("implement tasks at completion = %s, want exactly 1: no duplicate task survived the relaunch", got)
	}
}

// restartCloseIdentification reads, from the run's own operation journal,
// which rung identified a pane before the restart rule closed it. The
// intent is stored as JSON, so this scrapes the one field rather than
// decoding a shape this package cannot import.
func restartCloseIdentification(t *testing.T, fx *featureRun, paneID string) string {
	t.Helper()
	var rung string
	if !waitUntilDeadline(featureRunTimeout, func() bool {
		rung = strings.TrimSpace(fx.scalar(t, fmt.Sprintf(
			"SELECT json_extract(intent, '$.identified_by') FROM operations "+
				"WHERE kind = 'pane.close' AND json_extract(intent, '$.pane_id') = '%s' "+
				"AND json_extract(intent, '$.reason') = 'server-lifetime change';", paneID)))
		return rung != "" && rung != "NULL"
	}) {
		t.Fatalf("no server-restart close was journaled for pane %s (last answer %q)", paneID, rung)
	}
	return rung
}

// restartCloseScrollback reads the pane scrollback HOP captured before it
// closed a pane under the restart rule — the close procedure's own evidence
// capture, written to the run's artifact directory. Empty when the artifact
// is missing, which a caller must treat as "nothing observed" rather than
// as evidence of absence.
func restartCloseScrollback(t *testing.T, fx *featureRun, paneID string) string {
	t.Helper()
	opID := strings.TrimSpace(fx.scalar(t, fmt.Sprintf(
		"SELECT id FROM operations WHERE kind = 'pane.close' "+
			"AND json_extract(intent, '$.pane_id') = '%s' "+
			"AND json_extract(intent, '$.reason') = 'server-lifetime change';", paneID)))
	if opID == "" {
		return ""
	}
	content, err := os.ReadFile(filepath.Join(fx.stateDir, "runs", fx.runID, "artifacts", "pane-scrollback-"+opID+".txt")) //nolint:gosec // G304: the path is inside this test's own state directory.
	if err != nil {
		return ""
	}
	return string(content)
}

// requireManagerClosedLast asserts the OBSERVED ordering: every child
// session's restart close was journaled before the manager's, so a round
// that failed partway could not have moved the run's manager lineage
// first. The comparison is on the operations' own recorded instants, with
// the row id breaking a tie, exactly as the store orders them.
func requireManagerClosedLast(t *testing.T, fx *featureRun, placed []orchestratedSession) {
	t.Helper()
	closedAt := func(paneID string) string {
		return strings.TrimSpace(fx.scalar(t, fmt.Sprintf(
			"SELECT created_at || '|' || id FROM operations WHERE kind = 'pane.close' "+
				"AND json_extract(intent, '$.pane_id') = '%s' "+
				"AND json_extract(intent, '$.reason') = 'server-lifetime change';", paneID)))
	}
	var manager string
	for i := range placed {
		if placed[i].Role == roleManagerRow {
			manager = closedAt(placed[i].PaneID)
		}
	}
	if manager == "" {
		t.Fatal("no server-restart close was journaled for the manager's pane")
	}
	for i := range placed {
		if placed[i].Role == roleManagerRow {
			continue
		}
		child := closedAt(placed[i].PaneID)
		if child == "" || child >= manager {
			t.Errorf("the %s session's close (%s) was not journaled before the manager's (%s); the manager is reconciled LAST", placed[i].Role, child, manager)
		}
	}
	t.Logf("the manager's restart close was journaled last, at %s", manager)
}

// planCloseQuery counts the run's ACCEPTED plan closes — the authoritative
// record, since workflow_receipts holds exactly one accepted row per
// (run, verb, request id) and a second close would have to appear here.
func planCloseQuery(runID string) string {
	return fmt.Sprintf("SELECT COUNT(*) FROM workflow_receipts WHERE run_id = '%s' AND op = 'plan-close' AND outcome = 'accepted';", runID)
}

// Role values as the session rows record them.
const (
	roleManagerRow     = "manager"
	roleImplementerRow = "implementer"
)

// successorOf is the non-terminal session that replaced one closed by the
// restart rule: for a child, the other session of its attempt; for the
// manager, the run's current manager session.
func successorOf(t *testing.T, fx *featureRun, prior *orchestratedSession) string {
	t.Helper()
	query := fmt.Sprintf("SELECT id FROM sessions WHERE run_id = '%s' AND role = 'manager' AND state NOT IN ('terminated', 'lost');", fx.runID)
	if prior.Role != roleManagerRow {
		query = fmt.Sprintf("SELECT id FROM sessions WHERE run_id = '%s' AND id != '%s' AND attempt_id = (SELECT attempt_id FROM sessions WHERE id = '%s') AND state NOT IN ('terminated', 'lost');", fx.runID, prior.SessionID, prior.SessionID)
	}
	var successor string
	if !waitUntilDeadline(featureRunTimeout, func() bool {
		successor = strings.TrimSpace(fx.scalar(t, query))
		return successor != "" && !strings.Contains(successor, "\n")
	}) {
		t.Fatalf("the %s session %s was never succeeded by exactly one live session (last answer %q)", prior.Role, prior.SessionID, successor)
	}
	return successor
}

// sessionByRole picks one placed session by its role.
func sessionByRole(sessions []orchestratedSession, role string) *orchestratedSession {
	for i := range sessions {
		if sessions[i].Role == role {
			return &sessions[i]
		}
	}
	return nil
}
