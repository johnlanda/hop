package integration

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// This file is the reusable base for every Phase 3 feature-mode real-process
// scenario (slice 7a's FeatureRunEndToEnd/WorkerInterruption/
// ReviewerRejection and, on this tip, slices 7b/7c's messaging and
// controller/stop/integration scenarios): a feature-mode fixture repository
// builder, a `hop run --workflow feature` starter mirroring resultsubmit_
// test.go's solo fixtureRun, and read-only store polling helpers. Every wait
// is a bounded poll against the store (never a sleep); every barrier release
// goes through a real one-shot `hop answer`, never typed pane input.

// featureRunTimeout bounds a feature-mode scenario's own polls: launch
// corroboration, concurrent worker scheduling, serial integration and
// review all add real wall-clock cost on top of the Phase 2 baseline, so
// this is deliberately generous — a timeout's own failure message always
// names what it was still waiting for, never just "timeout".
const featureRunTimeout = 3 * time.Minute

// featureConfigTOML renders a feature-mode .herdr-orchestrator/config.toml:
// the Phase 2 [check]/[worker] tables fixtureConfigTOML already renders,
// plus [workflow] mode=feature, [workers]/[retry] bounds, [messages]
// timeouts (kept short so a scenario's manager/worker/reviewer idle-poll
// loops advance quickly under test) and the three [roles.*] instruction
// paths — relative to .herdr-orchestrator/ itself (internal/adapters/
// config's own resolution rule), matching newFeatureFixtureRepo's file
// layout exactly.
func featureConfigTOML(checkCommand []string, maxWorkers, retryLimit int, waitTimeout, attentionAfter string) string {
	quoted := make([]string, len(checkCommand))
	for i, arg := range checkCommand {
		quoted[i] = strconv.Quote(arg)
	}
	return "[check]\n" +
		"command = [" + strings.Join(quoted, ", ") + "]\n" +
		"timeout = \"30s\"\n" +
		"\n[worker]\nharness = \"claude\"\n" +
		"\n[workflow]\nmode = \"feature\"\n" +
		fmt.Sprintf("\n[workers]\nmax = %d\n", maxWorkers) +
		fmt.Sprintf("\n[retry]\nmax_attempts = %d\n", retryLimit) +
		fmt.Sprintf("\n[messages]\nwait_timeout = %q\nattention_after = %q\n", waitTimeout, attentionAfter) +
		"\n[roles.manager]\ninstructions = \"roles/manager.md\"\n" +
		"\n[roles.implementer]\ninstructions = \"roles/implementer.md\"\n" +
		"\n[roles.reviewer]\ninstructions = \"roles/reviewer.md\"\nharness = \"claude\"\n"
}

// featureFixtureOptions configures newFeatureFixtureRepo. ScratchDir is the
// test-owned directory every fixture principal in the run writes its
// observation dumps, authored message bodies and one-shot markers into
// (design directive: never under HOP_STATE_DIR) — baked into the frozen
// reviewer role content here, and threaded to every worker by the manager
// script's own scratch-dir argument at scenario-build time.
type featureFixtureOptions struct {
	ScratchDir             string
	ReviewerBehavior       string // "reviewer-approve" | "reviewer-reject-once"
	MaxWorkers, RetryLimit int
	MessageWaitTimeout     string // Go duration string, e.g. "3s"
	MessageAttentionAfter  string
}

// newFeatureFixtureRepo builds a feature-mode fixture repository: the same
// deterministic check.sh/CHECK_RESULT contract as newFixtureRepo, plus the
// three frozen role instruction files under .herdr-orchestrator/roles/ (the
// manager's and implementer's are generic — their own behavior travels
// through the brief and the per-task instructions file instead — and the
// reviewer's carries its FIXTURE-BEHAVIOR directive directly, since a
// review task's own assignment carries no manager-authored free text at
// all) and the feature-mode config.toml. server is passed to
// registerWorktreeCleanup exactly as newFixtureRepo's is.
func newFeatureFixtureRepo(t *testing.T, artifacts *artifactDir, server *testServer, name string, opts featureFixtureOptions) *fixtureRepo { //nolint:gocritic // hugeParam: featureFixtureOptions is a one-shot scenario-build options struct, constructed once per test; a pointer would only complicate every call site.
	t.Helper()
	repo := initFixtureRepo(t, artifacts, name)
	repo.writeFile(t, "hello.go", trivialSourceFile, 0o644)
	repo.writeFile(t, checkScriptName, checkScriptSource, 0o755)
	repo.writeFile(t, checkResultFile, "pass\n", 0o644)
	repo.writeFile(t, ".herdr-orchestrator/roles/manager.md", "# Manager role\n\nCoordinate the run through the hop verbs the worker protocol reference quotes.\n", 0o644)
	repo.writeFile(t, ".herdr-orchestrator/roles/implementer.md", "# Implementer role\n\nImplement your assigned task and commit your work.\n", 0o644)
	repo.writeFile(t, ".herdr-orchestrator/roles/reviewer.md", "FIXTURE-BEHAVIOR: "+opts.ReviewerBehavior+" "+opts.ScratchDir+"\n", 0o644)
	repo.writeFile(t, configRelPath, featureConfigTOML([]string{"sh", checkScriptName}, opts.MaxWorkers, opts.RetryLimit, opts.MessageWaitTimeout, opts.MessageAttentionAfter), 0o644)
	repo.Base = repo.commit(t, "initial commit")
	registerWorktreeCleanup(t, server, repo)
	return repo
}

// featureRun is one live feature-mode run: the started controller plus the
// read-only store/CLI helpers every scenario built on this harness shares.
// Mirrors resultsubmit_test.go's solo fixtureRun.
type featureRun struct {
	artifacts  *artifactDir
	server     *testServer
	repo       *fixtureRepo
	stateDir   string
	scratchDir string
	env        []string

	controller     *serverProcess
	controllerName string
	label, runID   string
}

// startFeatureRun runs `hop run --workflow feature` against repo with
// brief as the run's positional brief argument, returning once the
// controller has printed its "started" line.
func startFeatureRun(t *testing.T, artifacts *artifactDir, server *testServer, repo *fixtureRepo, scratchDir, brief string) *featureRun {
	t.Helper()
	stateDir := artifacts.dir(t, "state")
	env := server.hopEnviron(stateDir)

	sp := server.startHopController(t, stateDir, "run", "run", "-C", repo.Root, "--workflow", "feature", brief)
	started := waitForControllerLog(t, artifacts, "run", "started")
	label, runID := extractRunID(t, started)

	return &featureRun{
		artifacts: artifacts, server: server, repo: repo, stateDir: stateDir, scratchDir: scratchDir,
		env: env, controller: sp, controllerName: "run", label: label, runID: runID,
	}
}

// dbPath is this run's own throwaway hop.db.
func (f *featureRun) dbPath() string { return filepath.Join(f.stateDir, "hop.db") }

// scalar runs one read-only query against this run's store.
func (f *featureRun) scalar(t *testing.T, query string) string {
	t.Helper()
	return querySQLite(t, f.dbPath(), query)
}

// runState reads the run's own current state.
func (f *featureRun) runState(t *testing.T) string {
	t.Helper()
	return f.scalar(t, fmt.Sprintf("SELECT state FROM runs WHERE id = '%s';", f.runID))
}

// requireRunState polls runState until it is one of want, bounded by
// featureRunTimeout, failing with the controller's captured stdout on
// timeout — mirroring resultsubmit_test.go's fixtureRun.requireRunState.
func (f *featureRun) requireRunState(t *testing.T, want ...string) {
	t.Helper()
	var state string
	reached := waitUntilDeadline(featureRunTimeout, func() bool {
		state = f.runState(t)
		return slices.Contains(want, state)
	})
	if !reached {
		t.Fatalf("run %s ended %q, want one of %v after %s\ncontroller stdout:\n%s", f.runID, state, want, featureRunTimeout, readControllerLog(t, f.artifacts, f.controllerName))
	}
}

// taskState reads one task's current state.
func (f *featureRun) taskState(t *testing.T, taskID string) string {
	t.Helper()
	return f.scalar(t, fmt.Sprintf("SELECT state FROM tasks WHERE id = '%s';", taskID))
}

// requireTaskState polls taskState until it is one of want, bounded by
// featureRunTimeout, naming the task id on timeout.
func (f *featureRun) requireTaskState(t *testing.T, taskID string, want ...string) {
	t.Helper()
	var state string
	reached := waitUntilDeadline(featureRunTimeout, func() bool {
		state = f.taskState(t, taskID)
		return slices.Contains(want, state)
	})
	if !reached {
		t.Fatalf("task %s ended %q, want one of %v after %s", taskID, state, want, featureRunTimeout)
	}
}

// reconciledSessionID returns the id of the first session belonging to
// this run that the transitions journal has ever recorded going to
// "reconciling", or "" if none has, regardless of the transition's own
// recorded reason. The journal is append-only, so one query answers this
// regardless of when it is asked. Since docs/plan/phase-3-design.md
// section 6's live launch corroboration, a session that reconciles under
// the live launch-corroboration reason IS later revisited by
// CorroborateSessionLaunches and may settle on its own — see
// evaluateReconcilingGuard, which applies that distinction; every OTHER
// reconciling transition remains a durable, silent wedge, since nothing
// ever revisits it. A caller that wants the reason-aware, bounded rule
// should use requireTaskStateNeverReconciling instead: this raw check
// stays useful only where ANY reconciling transition at all, legitimate
// or not, would already indict the narrower window the caller is
// independently proving (workerlaunchvanish_test.go's own use, ahead of
// the point that window is even meant to close).
func (f *featureRun) reconciledSessionID(t *testing.T) string {
	t.Helper()
	return f.scalar(t, fmt.Sprintf(
		"SELECT entity_id FROM transitions WHERE entity_kind = 'session' AND to_state = 'reconciling' "+
			"AND entity_id IN (SELECT id FROM sessions WHERE run_id = '%s') ORDER BY at LIMIT 1;",
		f.runID))
}

// reconcilingLiveLaunchCorroborationReason is RETYPED from
// internal/app/usecase_sessioncorroborate.go's own exported
// TransitionReasonLaunchCorroboration literal — deliberately never
// imported (this package's own AGENTS.md: expected values are retyped,
// never derived, and its principals cannot import internal/app in the
// first place). This guard is an INDEPENDENT check on production's own
// reason text: retyped, a drift in production's literal fails this guard
// instead of silently following it.
const reconcilingLiveLaunchCorroborationReason = "launch corroboration: another process on the pane carries the launch identity; re-inspected every pass"

// reconcilingLiveBound is how long a session may stay "reconciling" under
// the live launch-corroboration reason (docs/plan/phase-3-design.md
// section 6: a transient fork-window classification a later clean
// observation settles on its own) before this guard treats it as a wedge
// instead.
const reconcilingLiveBound = 30 * time.Second

// reconcileTransition is one recorded transition either into or out of
// "reconciling" for one session, as read from the transitions journal.
// Reason is populated only for an entry (a transition INTO reconciling);
// an exit's own to_state carries no rule this guard cares about.
type reconcileTransition struct {
	SessionID, At, Reason string
}

// parseReconcileTransitions parses rows of "|"-joined fields ("session_id
// | at" for an exit list, "session_id | at | reason" for an entry list)
// as f.scalar renders a multi-row SELECT (one row per line), in the
// journal's own oldest-first order.
func parseReconcileTransitions(t *testing.T, rows string, withReason bool) []reconcileTransition {
	t.Helper()
	fields := 2
	if withReason {
		fields = 3
	}
	var out []reconcileTransition
	for _, row := range strings.Split(rows, "\n") {
		if row == "" {
			continue
		}
		parts := strings.SplitN(row, "|", fields)
		if len(parts) != fields {
			t.Fatalf("unparseable reconciling transition row %q (want %d fields)", row, fields)
		}
		entry := reconcileTransition{SessionID: parts[0], At: parts[1]}
		if withReason {
			entry.Reason = parts[2]
		}
		out = append(out, entry)
	}
	return out
}

// sessionReconcileEntries lists, oldest first, every transition any
// session of this run has ever made INTO "reconciling", with the reason
// each one was recorded under.
func (f *featureRun) sessionReconcileEntries(t *testing.T) []reconcileTransition {
	t.Helper()
	rows := f.scalar(t, fmt.Sprintf(
		"SELECT entity_id || '|' || at || '|' || reason FROM transitions WHERE entity_kind = 'session' AND to_state = 'reconciling' "+
			"AND entity_id IN (SELECT id FROM sessions WHERE run_id = '%s') ORDER BY at, rowid;",
		f.runID))
	return parseReconcileTransitions(t, rows, true)
}

// sessionReconcileExits lists, oldest first, every transition any session
// of this run has ever made OUT of "reconciling" (to whatever state:
// active via settlement, or terminated via a stop or a failure sweep —
// this guard only cares that the session left, not where it went).
func (f *featureRun) sessionReconcileExits(t *testing.T) []reconcileTransition {
	t.Helper()
	rows := f.scalar(t, fmt.Sprintf(
		"SELECT entity_id || '|' || at FROM transitions WHERE entity_kind = 'session' AND from_state = 'reconciling' "+
			"AND entity_id IN (SELECT id FROM sessions WHERE run_id = '%s') ORDER BY at, rowid;",
		f.runID))
	return parseReconcileTransitions(t, rows, false)
}

// evaluateReconcilingGuard applies docs/plan/phase-3-design.md section 6's
// live launch-corroboration rule to entries (every transition a run's
// sessions have ever made into "reconciling", oldest first) and exits
// (every transition out of it, oldest first), evaluated fresh against the
// journal rather than a state snapshot — the journal alone can tell a
// session that already resolved from one still open, which a single
// sessions.state read cannot. now is the caller's own wall-clock read,
// compared against the journal's canonical RFC3339Nano timestamps.
//
// Pairs each entry with the next unconsumed exit for the SAME session at
// or after that entry's own timestamp (FIFO, since neither list can
// interleave two entries for one session without an intervening exit —
// markSessionReconciling is idempotent while a session stays reconciling,
// and the entry that mismatches its allowed reason is caught, and this
// function stops, before a second entry for that session is ever
// examined).
//
// Returns violation naming the first rule broken, if any: an entry
// recorded under any reason but reconcilingLiveLaunchCorroborationReason
// is a wedge at once, whether or not it ever left; a legitimate entry
// that stays reconciling past reconcilingLiveBound is a wedge too,
// whether it exceeded the bound before leaving or is still open past it
// now — the run itself completing first proves nothing on its own (a
// session that stayed reconciling for the run's entire remaining life is
// exactly the wedge this guard exists to catch, not an exemption from it).
//
// Otherwise, pending reports whether any legitimate entry is still open
// AND still inside its bound: NOT a pass (a still-open episode could yet
// become a permanent wedge, or run out its own clock, before its next
// observation) and NOT a failure (a legitimate transient reconciliation
// in progress) — the caller must wait out the remainder of the bound and
// re-evaluate rather than deciding either way from this one read.
func evaluateReconcilingGuard(entries, exits []reconcileTransition, now time.Time) (violation string, pending bool, err error) {
	exitsBySession := make(map[string][]string, len(exits))
	for _, exit := range exits {
		exitsBySession[exit.SessionID] = append(exitsBySession[exit.SessionID], exit.At)
	}
	consumed := make(map[string]int, len(entries))
	for _, entry := range entries {
		if entry.Reason != reconcilingLiveLaunchCorroborationReason {
			return fmt.Sprintf("session %s entered reconciling with reason %q, not the live launch-corroboration reason %q; any other reconciling transition is a wedge",
				entry.SessionID, entry.Reason, reconcilingLiveLaunchCorroborationReason), false, nil
		}
		enteredAt, parseErr := time.Parse(time.RFC3339Nano, entry.At)
		if parseErr != nil {
			return "", false, fmt.Errorf("parse session %s reconciling entry timestamp %q: %w", entry.SessionID, entry.At, parseErr)
		}

		var leftAt string
		var leftTime time.Time
		found := false
		candidates := exitsBySession[entry.SessionID]
		for i := consumed[entry.SessionID]; i < len(candidates); i++ {
			consumed[entry.SessionID] = i + 1
			exitTime, parseErr := time.Parse(time.RFC3339Nano, candidates[i])
			if parseErr != nil {
				return "", false, fmt.Errorf("parse session %s reconciling exit timestamp %q: %w", entry.SessionID, candidates[i], parseErr)
			}
			// Defensive: production's markSessionReconciling is idempotent
			// while a session stays reconciling, so entries and exits for
			// one session strictly alternate and an exit can never precede
			// its own entry — this comparison holds on every real exit the
			// journal can produce. Kept as a guard against clock skew
			// rather than assumed: if it ever failed, treating the entry
			// as still open (never matched) is the safe direction, a false
			// failure rather than a false pass.
			if !exitTime.Before(enteredAt) {
				leftAt, leftTime, found = candidates[i], exitTime, true
				break
			}
		}

		if found {
			if leftTime.Sub(enteredAt) > reconcilingLiveBound {
				return fmt.Sprintf("session %s stayed reconciling for %s (entered %s, left %s), exceeding the %s bound",
					entry.SessionID, leftTime.Sub(enteredAt), entry.At, leftAt, reconcilingLiveBound), false, nil
			}
			continue
		}

		elapsed := now.Sub(enteredAt)
		if elapsed > reconcilingLiveBound {
			return fmt.Sprintf("session %s has been reconciling since %s (%s ago) with no transition out of it yet, exceeding the %s bound",
				entry.SessionID, entry.At, elapsed, reconcilingLiveBound), false, nil
		}
		pending = true
	}
	return "", pending, nil
}

// reconcilingGuard reads this run's own transitions journal and applies
// evaluateReconcilingGuard against the current wall clock.
func (f *featureRun) reconcilingGuard(t *testing.T) (violation string, pending bool) {
	t.Helper()
	entries := f.sessionReconcileEntries(t)
	if len(entries) == 0 {
		return "", false
	}
	exits := f.sessionReconcileExits(t)
	violation, pending, err := evaluateReconcilingGuard(entries, exits, time.Now())
	if err != nil {
		t.Fatalf("evaluate reconciling guard for run %s: %v", f.runID, err)
	}
	return violation, pending
}

// requireTaskStateNeverReconciling is requireTaskState, additionally
// enforcing docs/plan/phase-3-design.md section 6's reconciling rule while
// it waits: a session may enter "reconciling" only under the live
// launch-corroboration reason, and must leave it within reconcilingLiveBound
// — any other reconciling transition, or one that overruns the bound,
// fails at once rather than only after the full featureRunTimeout. A
// still-open, still-in-bound reconciliation is neither a pass nor a
// failure (evaluateReconcilingGuard's pending result): reaching one of
// want while such an episode is open does NOT end the wait, since letting
// the task's own state decide it would pass a run that completes while a
// session is still silently wedged — the exact failure mode this guard
// exists to catch. sawWant LATCHES the instant want is observed rather
// than re-testing taskState only once no episode is pending: want may be
// a state the task only passes through (never rests in), and a pending
// episode can span exactly the polls that would have observed it, so
// re-deriving "reached want" from a LATER poll's state would silently
// miss it — the wait would then burn the full featureRunTimeout and fail
// naming whatever state the task moved on to, not the reconciliation that
// actually caused the miss.
func (f *featureRun) requireTaskStateNeverReconciling(t *testing.T, taskID string, want ...string) {
	t.Helper()
	var state, violation string
	sawWant := false
	reached := waitUntilDeadline(featureRunTimeout, func() bool {
		var pending bool
		violation, pending = f.reconcilingGuard(t)
		if violation == "" {
			state = f.taskState(t, taskID)
			sawWant = sawWant || slices.Contains(want, state)
		}
		return reconcilingWaitStop(violation, pending, sawWant)
	})
	if violation != "" {
		t.Fatalf("task %s: %s", taskID, violation)
	}
	if !reached {
		t.Fatalf("task %s ended %q, want one of %v after %s", taskID, state, want, featureRunTimeout)
	}
}

// reconcilingWaitStop is requireTaskStateNeverReconciling's own per-poll
// decision, pulled out as a pure function so the poll loop itself — never
// exercised by evaluateReconcilingGuard's own tests, which drive the
// guard directly rather than the loop wrapped around it — has something
// to test directly (TestReconcilingWaitStop): violation (from
// reconcilingGuard) ends the wait at once, before sawWant is ever
// consulted, since a wedge must fail regardless of whatever want state
// happened to be observed already; a still-open pending episode never
// ends the wait, whatever sawWant holds; otherwise the wait ends once
// sawWant is true — sawWant, not the CURRENT state, since want may be a
// state the caller's task only passes through rather than rests in, and
// a pending episode can span exactly the polls that would have observed
// it.
func reconcilingWaitStop(violation string, pending, sawWant bool) bool {
	if violation != "" {
		return true
	}
	if pending {
		return false
	}
	return sawWant
}

// currentAttempt returns taskID's most recent (highest-numbered) attempt.
func (f *featureRun) currentAttempt(t *testing.T, taskID string) (attemptID string, number int) {
	t.Helper()
	row := f.scalar(t, fmt.Sprintf("SELECT id || '|' || number FROM attempts WHERE task_id = '%s' ORDER BY number DESC LIMIT 1;", taskID))
	parts := strings.SplitN(row, "|", 2)
	if len(parts) != 2 {
		t.Fatalf("no attempt found for task %s", taskID)
	}
	n, err := strconv.Atoi(parts[1])
	if err != nil {
		t.Fatalf("attempt number %q for task %s does not parse: %v", parts[1], taskID, err)
	}
	return parts[0], n
}

// attemptCount reports how many attempts taskID has reserved so far.
func (f *featureRun) attemptCount(t *testing.T, taskID string) int {
	t.Helper()
	row := f.scalar(t, fmt.Sprintf("SELECT count(*) FROM attempts WHERE task_id = '%s';", taskID))
	n, err := strconv.Atoi(row)
	if err != nil {
		t.Fatalf("attempt count %q for task %s does not parse: %v", row, taskID, err)
	}
	return n
}

// attemptState reads one attempt's current state.
func (f *featureRun) attemptState(t *testing.T, attemptID string) string {
	t.Helper()
	return f.scalar(t, fmt.Sprintf("SELECT state FROM attempts WHERE id = '%s';", attemptID))
}

// requireAttemptState polls attemptState until it is one of want, bounded
// by featureRunTimeout, naming the attempt id on timeout.
func (f *featureRun) requireAttemptState(t *testing.T, attemptID string, want ...string) {
	t.Helper()
	var state string
	reached := waitUntilDeadline(featureRunTimeout, func() bool {
		state = f.attemptState(t, attemptID)
		return slices.Contains(want, state)
	})
	if !reached {
		t.Fatalf("attempt %s ended %q, want one of %v after %s", attemptID, state, want, featureRunTimeout)
	}
}

// sessionForAttempt resolves an attempt's current (non-terminal) session,
// falling back to its most recent session regardless of state so a caller
// can still observe a session's terminal row after retirement.
func (f *featureRun) sessionForAttempt(t *testing.T, attemptID string) string {
	t.Helper()
	id := f.scalar(t, fmt.Sprintf("SELECT id FROM sessions WHERE attempt_id = '%s' AND state NOT IN ('lost','terminated') ORDER BY rowid DESC LIMIT 1;", attemptID))
	if id == "" {
		id = f.scalar(t, fmt.Sprintf("SELECT id FROM sessions WHERE attempt_id = '%s' ORDER BY rowid DESC LIMIT 1;", attemptID))
	}
	if id == "" {
		t.Fatalf("no session found for attempt %s", attemptID)
	}
	return id
}

// managerSessionID resolves the run's current manager session.
func (f *featureRun) managerSessionID(t *testing.T) string {
	t.Helper()
	id := f.scalar(t, fmt.Sprintf("SELECT id FROM sessions WHERE run_id = '%s' AND role = 'manager' AND state NOT IN ('lost','terminated') ORDER BY rowid DESC LIMIT 1;", f.runID))
	if id == "" {
		t.Fatalf("no live manager session found for run %s", f.runID)
	}
	return id
}

// sessionState reads one session's current state.
func (f *featureRun) sessionState(t *testing.T, sessionID string) string {
	t.Helper()
	return f.scalar(t, fmt.Sprintf("SELECT state FROM sessions WHERE id = '%s';", sessionID))
}

// requireSessionState polls sessionState until it is one of want, bounded
// by featureRunTimeout, naming the session id on timeout.
func (f *featureRun) requireSessionState(t *testing.T, sessionID string, want ...string) {
	t.Helper()
	var state string
	reached := waitUntilDeadline(featureRunTimeout, func() bool {
		state = f.sessionState(t, sessionID)
		return slices.Contains(want, state)
	})
	if !reached {
		t.Fatalf("session %s ended %q, want one of %v after %s", sessionID, state, want, featureRunTimeout)
	}
}

// paneForSession resolves a session's most recently observed pane
// binding, if any.
func (f *featureRun) paneForSession(t *testing.T, sessionID string) (paneID string, ok bool) {
	t.Helper()
	paneID = f.scalar(t, fmt.Sprintf("SELECT pane_id FROM runtime_bindings WHERE session_id = '%s' ORDER BY observed_at DESC, rowid DESC LIMIT 1;", sessionID))
	return paneID, paneID != ""
}

// requirePane polls paneForSession until a binding is observed, bounded
// by featureRunTimeout.
func (f *featureRun) requirePane(t *testing.T, sessionID string) string {
	t.Helper()
	var paneID string
	reached := waitUntilDeadline(featureRunTimeout, func() bool {
		var found bool
		paneID, found = f.paneForSession(t, sessionID)
		return found
	})
	if !reached {
		t.Fatalf("session %s has no bound pane after %s", sessionID, featureRunTimeout)
	}
	return paneID
}

// requireManagerFirstAgentTokens polls agent.list (f.server.agentTokens)
// until at least one HOP-ordered agent is published, then asserts the
// manager pane sorts first (assertManagerFirst, presentation_test.go).
// PublishRunPresentation runs as a step of the SAME scheduling pass that
// assigns tasks (cmd/hop's runFeatureSchedulingPass), not inside the
// assignment transaction itself, so a caller that has only observed a
// task/session state must poll here rather than assume tokens are already
// published. A timeout with the run otherwise healthy is deliberately
// distinguished as a presentation-publishing gap, not a harness timing
// issue this bounded poll would otherwise paper over.
func (f *featureRun) requireManagerFirstAgentTokens(t *testing.T, managerPaneID string) {
	t.Helper()
	var tokens map[string]map[string]string
	reached := waitUntilDeadline(featureRunTimeout, func() bool {
		tokens = f.server.agentTokens(t)
		for _, tk := range tokens {
			if tk["hop_order"] != "" {
				return true
			}
		}
		return false
	})
	if !reached {
		t.Fatalf("no HOP-ordered agent observed via agent.list for run %s after %s (a presentation-publishing gap, not a timing issue, if the run is otherwise healthy)", f.runID, featureRunTimeout)
	}
	assertManagerFirst(t, tokens, managerPaneID)
}

// killSession ends attemptID's own launched process by asking it to kill
// ITSELF, then waits for its pane to close (a layout.apply command pane
// has no shell, S6). This is never a raw OS signal to an externally
// OBSERVED pid: the test does not own that pid's wait/reap lifecycle, so
// nothing pins it between an
// observation (e.g. via pane.process_info) and a signal — Herdr could
// reap the process and the OS could recycle its pid before the test's
// own signal call ran, killing an unrelated process instead. Writing the
// control file watchForSelfKill polls for (fixtureWorkerSource, under
// the run's own scratch directory — never HOP_STATE_DIR, never pane
// input) asks the VERIFIED process to kill itself, closing that window
// entirely: no pid the test never independently re-verifies is ever
// signaled.
func (f *featureRun) killSession(t *testing.T, sessionID, attemptID string) {
	t.Helper()
	paneID := f.requirePane(t, sessionID)
	controlPath := filepath.Join(f.scratchDir, "self-kill-"+attemptID)
	tmp := controlPath + ".tmp"
	if err := os.WriteFile(tmp, []byte("FIXTURE-SELF-KILL\n"), 0o600); err != nil {
		t.Fatalf("write self-kill control file for attempt %s: %v", attemptID, err)
	}
	if err := os.Rename(tmp, controlPath); err != nil {
		t.Fatalf("rename self-kill control file into place for attempt %s: %v", attemptID, err)
	}
	if !waitUntil(func() bool { return !f.server.paneExists(t, paneID) }) {
		t.Fatalf("pane %s still exists after attempt %s's self-kill control file was written", paneID, attemptID)
	}
}

// relayedQuestionFor finds the human-addressed question whose relay chain
// (relayed_from) traces back to a question originSessionID itself sent —
// matched by the relay chain, never by delivery/creation order, since more
// than one task's barrier can be held (and relayed) concurrently. deadline
// bounds the wait for the manager to have relayed it yet; the failure
// names the originating session so it is clear which barrier was still
// pending.
func (f *featureRun) relayedQuestionFor(t *testing.T, originSessionID string) (questionID string) {
	t.Helper()
	ok := waitUntilDeadline(featureRunTimeout, func() bool {
		id := f.scalar(t, fmt.Sprintf(
			"SELECT h.id FROM messages h JOIN messages orig ON h.relayed_from = orig.id "+
				"WHERE h.run_id = '%s' AND h.recipient_address = 'human' AND orig.sender_session_id = '%s' LIMIT 1;",
			f.runID, originSessionID))
		if id == "" {
			return false
		}
		questionID = id
		return true
	})
	if !ok {
		t.Fatalf("no relayed human question observed for the barrier originating from session %s within %s", originSessionID, featureRunTimeout)
	}
	return questionID
}

// answerHuman drives `hop answer` as a separate one-shot human process,
// releasing questionID with an inline body — the ONLY thing that lets a
// test control a worker-hold barrier's release timing, since no
// production code path ever types into a pane.
func (f *featureRun) answerHuman(t *testing.T, questionID, body string) {
	t.Helper()
	result := runHop(t, f.env, f.repo.Root, "answer", "-C", f.repo.Root, "-run", f.runID, "--body", body, questionID)
	if result.ExitCode != 0 {
		t.Fatalf("hop answer %s: exit=%d stdout=%q stderr=%q", questionID, result.ExitCode, result.Stdout, result.Stderr)
	}
}

// messageBodyContent reads one message's body artifact content by id.
func (f *featureRun) messageBodyContent(t *testing.T, messageID string) string {
	t.Helper()
	path := f.scalar(t, fmt.Sprintf("SELECT body_path FROM messages WHERE id = '%s' AND run_id = '%s';", messageID, f.runID))
	if path == "" {
		t.Fatalf("no message %s found for run %s", messageID, f.runID)
	}
	content, err := os.ReadFile(path) //nolint:gosec // G304: a path read from this test's own throwaway store.
	if err != nil {
		t.Fatalf("read message %s body %s: %v", messageID, path, err)
	}
	return string(content)
}

// requireManagerNoticeFirstLine polls every info message addressed to
// "manager" for one whose body's exact first line equals want — the
// manager directive's own requirement (assert the EXACT rendered notice
// line read from the store, so a renderer format drift fails here, not
// merely at the fixture's looser parse) — and returns its message id.
// Checks every notice, not just the newest, since more than one may have
// landed by the time this observes them.
func (f *featureRun) requireManagerNoticeFirstLine(t *testing.T, deadline time.Duration, want string) (messageID string) {
	t.Helper()
	ok := waitUntilDeadline(deadline, func() bool {
		rows := f.scalar(t, fmt.Sprintf(
			"SELECT id || '|' || body_path FROM messages WHERE run_id = '%s' AND recipient_address = 'manager' AND kind = 'info' ORDER BY enqueue_seq;",
			f.runID))
		for _, row := range strings.Split(rows, "\n") {
			if row == "" {
				continue
			}
			parts := strings.SplitN(row, "|", 2)
			if len(parts) != 2 {
				continue
			}
			content, err := os.ReadFile(parts[1])
			if err != nil {
				continue
			}
			firstLine, _, _ := strings.Cut(string(content), "\n")
			if firstLine == want {
				messageID = parts[0]
				return true
			}
		}
		return false
	})
	if !ok {
		t.Fatalf("no manager info notice with first line %q observed for run %s within %s", want, f.runID, deadline)
	}
	return messageID
}

// worktreeForAttempt reads one attempt's own worktree row: path, branch
// and its recorded base commit (empty for a Phase 2 row, never expected
// here since every feature attempt gets its own). ok is false when no
// worktree row exists yet: worktree creation (usecase_schedule.go's
// AssignReadyTasks, createAttemptWorktree — a real git/herdr act) commits
// AFTER, and separately from, the transaction that first moves the task
// to active/the attempt to launching, so a caller that has only observed
// the task state must poll (requireWorktreeForAttempt), never assume the
// row already exists.
func (f *featureRun) worktreeForAttempt(t *testing.T, attemptID string) (path, branch, baseCommit string, ok bool) {
	t.Helper()
	row := f.scalar(t, fmt.Sprintf("SELECT path || '|' || branch || '|' || ifnull(base_commit,'') FROM worktrees WHERE attempt_id = '%s';", attemptID))
	parts := strings.SplitN(row, "|", 3)
	if len(parts) != 3 {
		return "", "", "", false
	}
	return parts[0], parts[1], parts[2], true
}

// requireWorktreeForAttempt polls worktreeForAttempt until its row exists,
// bounded by featureRunTimeout, naming the attempt id on timeout.
func (f *featureRun) requireWorktreeForAttempt(t *testing.T, attemptID string) (path, branch, baseCommit string) {
	t.Helper()
	var ok bool
	reached := waitUntilDeadline(featureRunTimeout, func() bool {
		path, branch, baseCommit, ok = f.worktreeForAttempt(t, attemptID)
		return ok
	})
	if !reached {
		t.Fatalf("no worktree row observed for attempt %s after %s", attemptID, featureRunTimeout)
	}
	return path, branch, baseCommit
}

// integrationForTask reads taskID's most recently created integration row
// (state and, once published, its merge commit); ok is false when no
// integration has been claimed for the task yet.
func (f *featureRun) integrationForTask(t *testing.T, taskID string) (state, mergeCommit string, ok bool) {
	t.Helper()
	row := f.scalar(t, fmt.Sprintf("SELECT state || '|' || ifnull(merge_commit_oid,'') FROM integrations WHERE task_id = '%s' ORDER BY rowid DESC LIMIT 1;", taskID))
	parts := strings.SplitN(row, "|", 2)
	if len(parts) != 2 {
		return "", "", false
	}
	return parts[0], parts[1], true
}

// requireIntegrationState polls integrationForTask until its state is one
// of want, bounded by featureRunTimeout.
func (f *featureRun) requireIntegrationState(t *testing.T, taskID string, want ...string) {
	t.Helper()
	var state string
	reached := waitUntilDeadline(featureRunTimeout, func() bool {
		var ok bool
		state, _, ok = f.integrationForTask(t, taskID)
		return ok && slices.Contains(want, state)
	})
	if !reached {
		t.Fatalf("integration for task %s ended %q, want one of %v after %s", taskID, state, want, featureRunTimeout)
	}
}

// integrationHead reads the run's current integration branch head (the
// most recently integrated task's merge commit, or the frozen base commit
// before any task has integrated).
func (f *featureRun) integrationHead(t *testing.T) string {
	t.Helper()
	head := f.scalar(t, fmt.Sprintf(
		"SELECT merge_commit_oid FROM integrations WHERE run_id = '%s' AND state = 'integrated' ORDER BY updated_at DESC, rowid DESC LIMIT 1;",
		f.runID))
	if head != "" {
		return head
	}
	return f.repo.Base
}

// requireCombinedCheckPassed asserts a PERSISTED, successful combined
// check (a check.run operation — usecase_integration.go's
// integrationCheckIntent/checkRunOutcome payloads — state succeeded,
// exit code 0) whose subject commit is exactly candidateCommit: the
// actual guard evidence design section 8's EvaluateReadiness requires,
// independent of integration.state == "integrated" alone. A production
// regression that marked an integration integrated without ever
// persisting this evidence would otherwise satisfy every other check in
// this suite, which would otherwise infer check evidence from
// integration state alone.
func (f *featureRun) requireCombinedCheckPassed(t *testing.T, candidateCommit string) {
	t.Helper()
	row := f.scalar(t, fmt.Sprintf(
		`SELECT count(*) FROM operations WHERE run_id = '%s' AND kind = 'check.run' AND state = 'succeeded' AND intent LIKE '%%"subject_commit_oid":%q%%' AND outcome LIKE '%%"exit_code":0%%';`,
		f.runID, candidateCommit))
	n, err := strconv.Atoi(row)
	if err != nil {
		t.Fatalf("combined-check evidence count %q for candidate %s does not parse: %v", row, candidateCommit, err)
	}
	if n == 0 {
		t.Errorf("no persisted successful combined-check operation (kind=check.run, state=succeeded, exit_code=0) found for candidate commit %s (run %s)", candidateCommit, f.runID)
	}
}

// requireCombinedCheckFailed is requireCombinedCheckPassed's negative
// sibling: asserts a PERSISTED, failed combined check (kind=check.run,
// state=failed) whose subject commit is exactly candidateCommit and
// whose outcome records exitCode — the actual guard evidence a scenario
// proving a combined-check failure needs, rather than inferring the
// failure from task/integration states and the check script alone.
func (f *featureRun) requireCombinedCheckFailed(t *testing.T, candidateCommit string, exitCode int) {
	t.Helper()
	row := f.scalar(t, fmt.Sprintf(
		`SELECT count(*) FROM operations WHERE run_id = '%s' AND kind = 'check.run' AND state = 'failed' AND intent LIKE '%%"subject_commit_oid":%q%%' AND outcome LIKE '%%"exit_code":%d%%';`,
		f.runID, candidateCommit, exitCode))
	n, err := strconv.Atoi(row)
	if err != nil {
		t.Fatalf("combined-check failure evidence count %q for candidate %s does not parse: %v", row, candidateCommit, err)
	}
	if n == 0 {
		t.Errorf("no persisted failed combined-check operation (kind=check.run, state=failed, exit_code=%d) found for candidate commit %s (run %s)", exitCode, candidateCommit, f.runID)
	}
}

// reviewVerdict reads reviewTaskID's accepted verdict, if any.
func (f *featureRun) reviewVerdict(t *testing.T, reviewTaskID string) (verdict string, ok bool) {
	t.Helper()
	verdict = f.scalar(t, fmt.Sprintf("SELECT verdict FROM reviews WHERE task_id = '%s';", reviewTaskID))
	return verdict, verdict != ""
}

// planClosed reports whether the run's plan flag is currently set.
func (f *featureRun) planClosed(t *testing.T) bool {
	t.Helper()
	return f.scalar(t, fmt.Sprintf("SELECT CASE WHEN plan_closed_at IS NULL THEN '' ELSE '1' END FROM runs WHERE id = '%s';", f.runID)) == "1"
}

// reviewTaskID finds the run's review-kind task, if one has been created
// yet.
func (f *featureRun) reviewTaskID(t *testing.T) (taskID string, ok bool) {
	t.Helper()
	taskID = f.scalar(t, fmt.Sprintf("SELECT id FROM tasks WHERE run_id = '%s' AND kind = 'review' ORDER BY rowid DESC LIMIT 1;", f.runID))
	return taskID, taskID != ""
}

// requireReviewTask polls reviewTaskID until one exists, bounded by
// featureRunTimeout.
func (f *featureRun) requireReviewTask(t *testing.T) string {
	t.Helper()
	var taskID string
	reached := waitUntilDeadline(featureRunTimeout, func() bool {
		var ok bool
		taskID, ok = f.reviewTaskID(t)
		return ok
	})
	if !reached {
		t.Fatalf("no review task observed for run %s after %s", f.runID, featureRunTimeout)
	}
	return taskID
}

// requireReviewTaskOtherThan polls for a review-kind task distinct from
// excludeTaskID, bounded by featureRunTimeout — used after a reject
// verdict, where a first review task already exists and the caller needs
// to observe the SECOND one a fix task's integration creates, never
// re-observing the first.
func (f *featureRun) requireReviewTaskOtherThan(t *testing.T, excludeTaskID string) string {
	t.Helper()
	var taskID string
	reached := waitUntilDeadline(featureRunTimeout, func() bool {
		id := f.scalar(t, fmt.Sprintf(
			"SELECT id FROM tasks WHERE run_id = '%s' AND kind = 'review' AND id != '%s' ORDER BY rowid DESC LIMIT 1;",
			f.runID, excludeTaskID))
		if id == "" {
			return false
		}
		taskID = id
		return true
	})
	if !reached {
		t.Fatalf("no review task other than %s observed for run %s after %s", excludeTaskID, f.runID, featureRunTimeout)
	}
	return taskID
}

// requireImplementTaskAfter polls for an implement-kind task created
// after afterTaskID's own row (by rowid, the store's own monotonic
// insertion order), bounded by featureRunTimeout. Task seq numbers are
// shared with review tasks (EnsureReviewTask mints maxSeq+1), so "the
// next seq after a review task" can select a LATER review task instead
// of the fix task a rejected verdict causes the manager to plan — this
// selects by KIND and creation order instead, never by seq alone.
func (f *featureRun) requireImplementTaskAfter(t *testing.T, afterTaskID string) string {
	t.Helper()
	afterRowID := f.scalar(t, fmt.Sprintf("SELECT rowid FROM tasks WHERE id = '%s';", afterTaskID))
	if afterRowID == "" {
		t.Fatalf("no task %s found to select a later implement task after", afterTaskID)
	}
	var taskID string
	reached := waitUntilDeadline(featureRunTimeout, func() bool {
		id := f.scalar(t, fmt.Sprintf(
			"SELECT id FROM tasks WHERE run_id = '%s' AND kind = 'implement' AND rowid > %s ORDER BY rowid LIMIT 1;",
			f.runID, afterRowID))
		if id == "" {
			return false
		}
		taskID = id
		return true
	})
	if !reached {
		t.Fatalf("no implement task created after task %s observed for run %s after %s", afterTaskID, f.runID, featureRunTimeout)
	}
	return taskID
}

// transitionCount counts recorded transitions for one entity between two
// states, used to assert a guard row exists without over-specifying its
// exact timestamp or generation.
func (f *featureRun) transitionCount(t *testing.T, entityKind, entityID, fromState, toState string) int {
	t.Helper()
	row := f.scalar(t, fmt.Sprintf(
		"SELECT count(*) FROM transitions WHERE entity_kind = '%s' AND entity_id = '%s' AND from_state = '%s' AND to_state = '%s';",
		entityKind, entityID, fromState, toState))
	n, err := strconv.Atoi(row)
	if err != nil {
		t.Fatalf("transition count %q does not parse: %v", row, err)
	}
	return n
}

// transitionAt returns the timestamp of the FIRST recorded transition of
// one entity into toState ("" if none yet) — the fixed-width canonical
// UTC format sqlite.go's timeLayout uses, so lexical string comparison
// agrees with time order (the schema's own documented property). Used to
// prove causal ordering directly from the journal (e.g. a dependent
// task's pending->ready transition landing no earlier than its
// prerequisite's own ...->integrated transition) instead of a racy
// snapshot poll.
func (f *featureRun) transitionAt(t *testing.T, entityKind, entityID, toState string) string {
	t.Helper()
	return f.scalar(t, fmt.Sprintf(
		"SELECT at FROM transitions WHERE entity_kind = '%s' AND entity_id = '%s' AND to_state = '%s' ORDER BY at LIMIT 1;",
		entityKind, entityID, toState))
}

// requireTransitionAt polls transitionAt until it is non-empty, bounded by
// featureRunTimeout, and returns it.
func (f *featureRun) requireTransitionAt(t *testing.T, entityKind, entityID, toState string) string {
	t.Helper()
	var at string
	reached := waitUntilDeadline(featureRunTimeout, func() bool {
		at = f.transitionAt(t, entityKind, entityID, toState)
		return at != ""
	})
	if !reached {
		t.Fatalf("no %s %s transition to %q observed after %s", entityKind, entityID, toState, featureRunTimeout)
	}
	return at
}

// waitForTaskBySeq polls for a task with the given store-reported seq to
// exist, bounded by deadline — the manager's own task-creation calls take
// a real store round trip each, so a caller cannot assume a task exists
// the instant the run starts.
func (f *featureRun) waitForTaskBySeq(t *testing.T, seq int, deadline time.Duration) (taskID string, ok bool) {
	t.Helper()
	ok = waitUntilDeadline(deadline, func() bool {
		taskID = f.scalar(t, fmt.Sprintf("SELECT id FROM tasks WHERE run_id = '%s' AND seq = %d;", f.runID, seq))
		return taskID != ""
	})
	return taskID, ok
}

// requireTaskBySeq is waitForTaskBySeq bounded by featureRunTimeout,
// failing the test on timeout.
func (f *featureRun) requireTaskBySeq(t *testing.T, seq int) string {
	t.Helper()
	taskID, ok := f.waitForTaskBySeq(t, seq, featureRunTimeout)
	if !ok {
		t.Fatalf("no task with seq %d observed for run %s after %s", seq, f.runID, featureRunTimeout)
	}
	return taskID
}

// integrationWindow is one task's integration row, its start (created_at)
// and settlement (updated_at) timestamps.
type integrationWindow struct {
	TaskID, State, CreatedAt, UpdatedAt string
}

// allIntegrationWindows lists every integration row for the run, oldest
// first, for a serial-order check independent of any single poll's
// snapshot: the store's own partial unique index enforces that at most
// one integration is ever non-terminal at a time, but a real-process
// scenario proves it empirically from the evidence too — each window's
// settlement (UpdatedAt) must not be later than the next window's start
// (CreatedAt).
func (f *featureRun) allIntegrationWindows(t *testing.T) []integrationWindow {
	t.Helper()
	rows := f.scalar(t, fmt.Sprintf(
		"SELECT task_id || '|' || state || '|' || created_at || '|' || updated_at FROM integrations WHERE run_id = '%s' ORDER BY created_at, rowid;",
		f.runID))
	var windows []integrationWindow
	for _, row := range strings.Split(rows, "\n") {
		if row == "" {
			continue
		}
		parts := strings.SplitN(row, "|", 4)
		if len(parts) != 4 {
			t.Fatalf("unparseable integration row %q", row)
		}
		windows = append(windows, integrationWindow{TaskID: parts[0], State: parts[1], CreatedAt: parts[2], UpdatedAt: parts[3]})
	}
	return windows
}

// launchClaimPID reports one session's newest recorded launch claim pid,
// if any — needs no live pane, so it remains usable for provenance
// assertions after a session has already retired.
func (f *featureRun) launchClaimPID(t *testing.T, sessionID string) (pid int, ok bool) {
	t.Helper()
	row := f.scalar(t, fmt.Sprintf("SELECT pid FROM launch_claims WHERE session_id = '%s' ORDER BY claimed_at DESC, rowid DESC LIMIT 1;", sessionID))
	if row == "" {
		return 0, false
	}
	n, err := strconv.Atoi(row)
	if err != nil {
		t.Fatalf("launch claim pid %q for session %s does not parse: %v", row, sessionID, err)
	}
	return n, true
}

// claimState reads one session's newest recorded launch claim state
// ("exec_pending", "execed", "exec_failed"). A caller that has already
// polled the session into "active" (settleSessionExeced settles the
// claim and activates the session in the SAME transaction) can read this
// directly rather than poll it again — used to assert explicitly, not
// merely infer, that a claim has settled before a scenario deliberately
// kills its own process (design section 11 scenario 4: a worker killed
// mid-attempt means AFTER its own launch settled, never a race against
// the controller's own corroboration).
func (f *featureRun) claimState(t *testing.T, sessionID string) string {
	t.Helper()
	return f.scalar(t, fmt.Sprintf("SELECT state FROM launch_claims WHERE session_id = '%s' ORDER BY claimed_at DESC, rowid DESC LIMIT 1;", sessionID))
}

// claimTimestamps reads one session's newest launch claim's own recorded
// claimed_at and settled_at (settled_at "" before settlement).
func (f *featureRun) claimTimestamps(t *testing.T, sessionID string) (claimedAt, settledAt string) {
	t.Helper()
	row := f.scalar(t, fmt.Sprintf("SELECT claimed_at || '|' || ifnull(settled_at,'') FROM launch_claims WHERE session_id = '%s' ORDER BY claimed_at DESC, rowid DESC LIMIT 1;", sessionID))
	parts := strings.SplitN(row, "|", 2)
	if len(parts) != 2 || parts[0] == "" {
		t.Fatalf("no launch claim recorded yet for session %s", sessionID)
	}
	return parts[0], parts[1]
}

// requireClaimSettled polls until sessionID's launch claim reaches
// "execed" and its session reaches "active", bounded by featureRunTimeout
// — a deliberate check (LAUNCH-2/PRES-1 triage) of whether corroboration
// settles a live harness blocked in its own idle loop (a worker-hold
// worker's `hop msg wait`), not only one whose foreground has already
// moved on by the time it happens to be inspected. Returns the
// settlement latency computed from the claim's own recorded
// claimed_at/settled_at timestamps (this suite's canonical fixed-width
// UTC format, parsed here rather than compared lexically since an actual
// duration, not merely an order, is what this check answers) — never
// wall-clock time this poll happened to notice it in. On timeout, the
// failure names the session (the claim's own key) and how long it had
// already been unsettled.
func (f *featureRun) requireClaimSettled(t *testing.T, sessionID string) time.Duration {
	t.Helper()
	reached := waitUntilDeadline(featureRunTimeout, func() bool {
		return f.claimState(t, sessionID) == "execed" && f.sessionState(t, sessionID) == "active"
	})
	claimedAt, settledAt := f.claimTimestamps(t, sessionID)
	claimedTime, err := time.Parse(time.RFC3339Nano, claimedAt)
	if err != nil {
		t.Fatalf("parse launch claim claimed_at %q for session %s: %v", claimedAt, sessionID, err)
	}
	if !reached {
		t.Fatalf("session %s's launch claim never reached execed within %s (claimed at %s, still unsettled after %s)",
			sessionID, featureRunTimeout, claimedAt, time.Since(claimedTime))
	}
	settledTime, err := time.Parse(time.RFC3339Nano, settledAt)
	if err != nil {
		t.Fatalf("parse launch claim settled_at %q for session %s: %v", settledAt, sessionID, err)
	}
	return settledTime.Sub(claimedTime)
}

// resultCount counts result rows submitted for one attempt (0 for an
// attempt that was interrupted before ever submitting).
func (f *featureRun) resultCount(t *testing.T, attemptID string) int {
	t.Helper()
	row := f.scalar(t, fmt.Sprintf("SELECT count(*) FROM results WHERE attempt_id = '%s';", attemptID))
	n, err := strconv.Atoi(row)
	if err != nil {
		t.Fatalf("result count %q for attempt %s does not parse: %v", row, attemptID, err)
	}
	return n
}

// requireSerialIntegrationOrder asserts allIntegrationWindows never
// overlap: each window's settlement precedes the next window's start.
func (f *featureRun) requireSerialIntegrationOrder(t *testing.T) {
	t.Helper()
	windows := f.allIntegrationWindows(t)
	for i := 1; i < len(windows); i++ {
		if windows[i-1].UpdatedAt > windows[i].CreatedAt {
			t.Errorf("integrations overlapped: task %s settled at %s, after task %s's own integration started at %s; windows=%+v",
				windows[i-1].TaskID, windows[i-1].UpdatedAt, windows[i].TaskID, windows[i].CreatedAt, windows)
		}
	}
}

// TestReconcilingGuardBounds proves evaluateReconcilingGuard's own rule in
// isolation, against synthetic transitions-journal rows: no herdr binary,
// no fixture principal, no real controller — only the rule's own
// arithmetic over the journal's canonical RFC3339Nano timestamps.
func TestReconcilingGuardBounds(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	at := func(d time.Duration) string { return base.Add(d).Format(time.RFC3339Nano) }
	const otherReason = "resume: evidence ambiguous"

	cases := []struct {
		name          string
		entries       []reconcileTransition
		exits         []reconcileTransition
		now           time.Time
		wantViolation bool
		wantPending   bool
	}{
		{
			name: "no entries: neither a violation nor pending",
			now:  base,
		},
		{
			name:          "wrong reason fails at once, before any bound is even considered",
			entries:       []reconcileTransition{{SessionID: "s1", At: at(0), Reason: otherReason}},
			now:           base,
			wantViolation: true,
		},
		{
			name:    "left within the bound: no violation, not pending",
			entries: []reconcileTransition{{SessionID: "s1", At: at(0), Reason: reconcilingLiveLaunchCorroborationReason}},
			exits:   []reconcileTransition{{SessionID: "s1", At: at(10 * time.Second)}},
			now:     base.Add(20 * time.Second),
		},
		{
			name:          "left, but only after exceeding the bound: a violation",
			entries:       []reconcileTransition{{SessionID: "s1", At: at(0), Reason: reconcilingLiveLaunchCorroborationReason}},
			exits:         []reconcileTransition{{SessionID: "s1", At: at(31 * time.Second)}},
			now:           base.Add(40 * time.Second),
			wantViolation: true,
		},
		{
			name:        "still open, 5s elapsed: pending, not yet decidable",
			entries:     []reconcileTransition{{SessionID: "s1", At: at(0), Reason: reconcilingLiveLaunchCorroborationReason}},
			now:         base.Add(5 * time.Second),
			wantPending: true,
		},
		{
			name:          "still open, clock advanced past the bound with no exit yet: a violation",
			entries:       []reconcileTransition{{SessionID: "s1", At: at(0), Reason: reconcilingLiveLaunchCorroborationReason}},
			now:           base.Add(31 * time.Second),
			wantViolation: true,
		},
		{
			name: "a second session's wrong reason is still caught after the first resolved cleanly",
			entries: []reconcileTransition{
				{SessionID: "s1", At: at(0), Reason: reconcilingLiveLaunchCorroborationReason},
				{SessionID: "s2", At: at(1 * time.Second), Reason: otherReason},
			},
			exits:         []reconcileTransition{{SessionID: "s1", At: at(2 * time.Second)}},
			now:           base.Add(3 * time.Second),
			wantViolation: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			violation, pending, err := evaluateReconcilingGuard(tc.entries, tc.exits, tc.now)
			if err != nil {
				t.Fatalf("evaluateReconcilingGuard: %v", err)
			}
			if (violation != "") != tc.wantViolation {
				t.Fatalf("violation = %q, want non-empty=%v", violation, tc.wantViolation)
			}
			if pending != tc.wantPending {
				t.Fatalf("pending = %v, want %v", pending, tc.wantPending)
			}
		})
	}

	t.Run("still open at 5s (pending), then leaves at 12s: the SAME episode ends up a pass", func(t *testing.T) {
		entries := []reconcileTransition{{SessionID: "s1", At: at(0), Reason: reconcilingLiveLaunchCorroborationReason}}

		violation, pending, err := evaluateReconcilingGuard(entries, nil, base.Add(5*time.Second))
		if err != nil {
			t.Fatalf("evaluateReconcilingGuard (still open): %v", err)
		}
		if violation != "" || !pending {
			t.Fatalf("at 5s: violation = %q, pending = %v, want empty violation and pending=true", violation, pending)
		}

		exits := []reconcileTransition{{SessionID: "s1", At: at(12 * time.Second)}}
		violation, pending, err = evaluateReconcilingGuard(entries, exits, base.Add(13*time.Second))
		if err != nil {
			t.Fatalf("evaluateReconcilingGuard (left at 12s): %v", err)
		}
		if violation != "" || pending {
			t.Fatalf("after leaving at 12s: violation = %q, pending = %v, want empty violation and pending=false", violation, pending)
		}
	})
}

// TestReconcilingWaitStop proves requireTaskStateNeverReconciling's own
// poll-loop decision (reconcilingWaitStop) directly, over scripted poll
// sequences — evaluateReconcilingGuard's own tests (above) call the
// guard, never the loop built around it, so the loop's own sequencing
// (whether a want state observed while an episode is pending is
// remembered once the episode clears) needs its own coverage. No herdr
// needed.
func TestReconcilingWaitStop(t *testing.T) {
	want := []string{"active"}

	// poll is one scripted reconcilingGuard/taskState observation. drive
	// replays requireTaskStateNeverReconciling's own loop shape over a
	// sequence of polls: violation short-circuits before state or sawWant
	// are touched (mirroring the real loop skipping the taskState read on
	// a violation); otherwise state updates and sawWant latches whether
	// want has EVER been seen; reconcilingWaitStop decides whether to
	// stop, and drive stops iterating the instant it does, exactly as
	// waitUntilDeadline's own predicate loop would (a real poll loop
	// never calls the predicate again after it returns true).
	type poll struct {
		violation string
		pending   bool
		state     string
	}
	drive := func(polls []poll) (stopped bool) {
		sawWant := false
		for _, p := range polls {
			violation := p.violation
			state := ""
			if violation == "" {
				state = p.state
				sawWant = sawWant || slices.Contains(want, state)
			}
			if reconcilingWaitStop(violation, p.pending, sawWant) {
				return true
			}
		}
		return false
	}

	cases := []struct {
		name        string
		polls       []poll
		wantStopped bool
	}{
		{
			name: "want observed while pending, then the task leaves want and the episode closes: stops",
			polls: []poll{
				{pending: true, state: "active"},
				{pending: true, state: "active"},
				{state: "completed"},
				{state: "integrated"},
			},
			wantStopped: true,
		},
		{
			name: "want never observed, episode closes: keeps waiting",
			polls: []poll{
				{pending: true, state: "ready"},
				{state: "ready"},
			},
			wantStopped: false,
		},
		{
			name: "violation stops immediately, regardless of sawWant",
			polls: []poll{
				{violation: "resume: evidence ambiguous", state: "ready"},
			},
			wantStopped: true,
		},
		{
			name: "pending with want observed: does not stop while the episode is open",
			polls: []poll{
				{pending: true, state: "active"},
			},
			wantStopped: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if stopped := drive(tc.polls); stopped != tc.wantStopped {
				t.Fatalf("stopped = %v, want %v", stopped, tc.wantStopped)
			}
		})
	}

	// The first case above stops only because sawWant LATCHES "active"
	// across the two pending polls. stateOnlyStop decides from the
	// CURRENT poll's own state instead of a latched observation — the
	// shape requireTaskStateNeverReconciling's loop would take without
	// the latch — and driving the identical sequence through it must NOT
	// stop, or this table is not actually covering what the latch is for.
	t.Run("without latching sawWant, the first case above never stops", func(t *testing.T) {
		polls := []poll{
			{pending: true, state: "active"},
			{pending: true, state: "active"},
			{state: "completed"},
			{state: "integrated"},
		}
		stateOnlyStop := func(violation string, pending bool, state string) bool {
			if violation != "" {
				return true
			}
			if pending {
				return false
			}
			return slices.Contains(want, state)
		}
		for _, p := range polls {
			if stateOnlyStop(p.violation, p.pending, p.state) {
				t.Fatal("the state-only decision unexpectedly stopped; this sequence no longer demonstrates what latching sawWant is for")
			}
		}
	})
}
