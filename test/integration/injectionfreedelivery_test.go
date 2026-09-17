package integration

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/johnlanda/hop/internal/adapters/herdr"
)

// hostilePayload is seeded into every artifact-delivery channel this
// scenario proves byte-exact (the brief, a task's instructions, and a
// manager answer): ANSI escapes, a bracketed-paste open/close pair, the
// classic "y" + newline confirm-dialog payload, and control characters
// -- design section 11 scenario 7's exact hostile-content list. It
// carries no NUL byte: the brief travels as a single argv element to the
// real hop binary (execve, no shell), and a NUL cannot appear in one, so
// the SAME payload is reused unchanged for the other two channels
// (file content, where a NUL would in fact be safe) for one shared,
// simpler proof.
const hostilePayload = "\x1b[31mANSI-ESCAPE\x1b[0m" +
	"\x1b[200~BRACKETED-PASTE-BLOCK\x1b[201~" +
	"y\n" +
	"\x07\x08\x1bCONTROL-CHARS"

// terminalInputMethods is Herdr 0.9.0's complete real terminal-input
// surface (design section 7/11, S11-confirmed against
// repos/herdr/src/api/schema.rs): the only methods that could ever type
// into a live pane.
func terminalInputMethods() []string {
	return []string{
		"pane.send_text", "pane.send_keys", "pane.send_input",
		"agent.prompt", "agent.send_keys", "agent.start",
	}
}

// extractBriefSection extracts the exact text internal/app/templates.go's
// renderManagerAssignment interpolates for the brief -- between its fixed
// "## Brief\n\n" and "\n\n## Instructions" markers -- failing the test
// loudly if either marker is absent. Retyped from that renderer's own
// literal template text (never derived), so a drift in the template fails
// here rather than silently degrading this scenario's assertion to a
// loose substring check.
func extractBriefSection(t *testing.T, assignmentContent string) string {
	t.Helper()
	const startMarker = "## Brief\n\n"
	const endMarker = "\n\n## Instructions"
	i := strings.Index(assignmentContent, startMarker)
	if i < 0 {
		t.Fatalf("manager assignment does not contain the %q section marker; content:\n%s", startMarker, assignmentContent)
	}
	rest := assignmentContent[i+len(startMarker):]
	j := strings.Index(rest, endMarker)
	if j < 0 {
		t.Fatalf("manager assignment does not contain the %q section marker after Brief; content:\n%s", endMarker, assignmentContent)
	}
	return rest[:j]
}

// sha256HexOf returns the lowercase-hex SHA-256 digest of content,
// mirroring internal/app/usecase_run.go's sha256Hex exactly (retyped,
// never imported) -- cryptographic corroboration of byte-exactness on
// top of a direct content comparison, cross-checked against the
// messages.body_digest column the production write path itself computed.
func sha256HexOf(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

// TestRealProcessInjectionFreeDelivery is design section 11 scenario 7:
// the brief, a task's instructions and a manager answer are seeded with
// hostile bytes (ANSI escapes, a bracketed-paste open/close pair, "y"
// plus newline, and control characters); every fixture principal that
// receives one validates byte-exact delivery ITSELF -- its own
// independent os.ReadFile of the path it resolved (or was handed by hop
// msg next/wait), dumped as a digest or verbatim in its own observation
// file, never a copy this test handed it -- cross-checked here against
// the durable artifact file each channel actually is (this test's own
// separate read): the brief, against the manager's own assignment.md,
// dumped verbatim; the task instructions, against a feature-mode
// implementer's own digest of its independently-resolved instructions
// path (its own per-attempt assignment.md is a SEPARATE templated
// document that only names that path, so the digest -- a capability
// added to the shared fixture for this scenario, see writeObservation's
// instructionsPath/instructionsContent parameters and
// fixtureworker_test.go's own unit test -- is the byte-exact evidence
// for this channel, not the dump's assignment_content field); the
// answer, against the SAME worker's digest of the exact body file hop
// msg wait handed it (writeContentDigest, a capability likewise added
// for this scenario), plus the message's stored body_digest as
// independent cryptographic corroboration. The disposable server's own
// request log, captured at the S11-pinned elevated level for the whole
// run, proves zero requests to any of Herdr's six real terminal-input
// methods other than one identified positive control.
//
// The manager session here is deliberately UNSCRIPTED (its
// FIXTURE-BEHAVIOR directive is absent -- the entire brief IS the
// hostile payload, so nothing else may parse it): every manager verb
// this scenario needs (task create, plan close, the barrier answer) is
// driven directly, authenticated as this same live manager session --
// exactly as legitimate as the scripted fixture manager issuing them
// itself (messagingscenarios_helpers_test.go's managerEnv), and never
// typed pane input. This also sidesteps the scripted manager-feature
// grammar's own whitespace-delimited, underscore-substituted TASK/ANSWER
// encoding, which cannot carry genuinely hostile bytes losslessly.
func TestRealProcessInjectionFreeDelivery(t *testing.T) {
	start := time.Now()
	defer func() { t.Logf("TestRealProcessInjectionFreeDelivery wall time: %s", time.Since(start)) }()

	artifacts := newArtifactDir(t)
	server := prepareServer(t, artifacts)
	// The S11-pinned elevated capture level, set BEFORE the server ever
	// starts -- so the capture window's start precedes the first HOP
	// process by construction, never by luck: at the default level a
	// successful pane.* call is invisible, so a zero count there would
	// prove nothing.
	server.extraEnv = append(server.extraEnv, "HERDR_LOG=herdr=debug")
	worker := buildFixtureWorker(t, artifacts)
	installFixtureWorkerAsClaudeStub(t, server, worker)
	server.start(t)

	// A dedicated, test-owned pane for the positive control, created now
	// (on the test's own shared connection -- setup, not itself part of
	// the six forbidden methods) so it exists throughout the run and is
	// available once the run has finished.
	var controlWorkspace workspaceCreatedResponse
	server.call(t, "workspace.create", map[string]any{"cwd": server.workDir(), "focus": false}, &controlWorkspace)
	controlPane := controlWorkspace.RootPane.PaneID

	// Window-start bracket (S11's own bracketing technique): a distinct,
	// otherwise-unused method's own log line, issued before any HOP
	// process (hop run, and every fixture principal it launches) exists.
	server.call(t, "pane.list", nil, &struct{}{})

	scratchDir := artifacts.dir(t, "fixture-scratch")
	repo := newFeatureFixtureRepo(t, artifacts, server, "injection-repo", featureFixtureOptions{
		ScratchDir: scratchDir, ReviewerBehavior: "reviewer-approve",
		MaxWorkers: 1, RetryLimit: 3, MessageWaitTimeout: "3s", MessageAttentionAfter: "30s",
	})

	// Channel 1: the brief -- seeded ENTIRELY with hostile bytes, and
	// otherwise unscripted (no FIXTURE-BEHAVIOR directive at all: the
	// manager just dumps it byte-exact to its own observation file and
	// idles at the composer-like loop).
	brief := hostilePayload
	fx := startFeatureRun(t, artifacts, server, repo, scratchDir, brief)

	briefPath := filepath.Join(fx.stateDir, "runs", fx.runID, "artifacts", "assignment.md")
	rawAssignment, err := os.ReadFile(briefPath) //nolint:gosec // G304: a path this test computed itself from its own run state.
	if err != nil {
		t.Fatalf("read manager assignment %s: %v", briefPath, err)
	}
	if got := extractBriefSection(t, string(rawAssignment)); got != brief {
		t.Fatalf("delivered brief section is not byte-exact: got %q, want %q", got, brief)
	}
	managerObservationPath := filepath.Join(fx.stateDir, "runs", fx.runID, "artifacts", "manager-observed.txt")
	managerObs := waitForObservation(t, managerObservationPath)
	if managerObs.AssignmentContent != string(rawAssignment) {
		t.Errorf("the manager fixture's OWN read of its assignment (its observation dump) does not byte-exactly match the file this test independently read:\nprincipal observed: %q\nfile content:       %q", managerObs.AssignmentContent, string(rawAssignment))
	}

	managerEnv := fx.managerEnv(t)

	// Channel 2: a task's instructions -- a valid directive line
	// (worker-hold, so the worker sends a barrier question this scenario
	// answers directly, exercising channel 3 too) followed immediately by
	// the hostile payload. Created directly as the manager (never through
	// the scripted grammar, whose own TASK-line fields cannot carry it).
	instructionsPath := filepath.Join(scratchDir, "hostile-instructions.md")
	instructionsContent := "FIXTURE-BEHAVIOR: worker-hold " + scratchDir + "\n" + hostilePayload
	if err := os.WriteFile(instructionsPath, []byte(instructionsContent), 0o600); err != nil {
		t.Fatalf("write hostile instructions file: %v", err)
	}
	createResult := runManagerVerb(t, managerEnv, repo.Root, "task", "create", "--title", "hostile task", "--file", instructionsPath, "--request-id", newSpikeUUID(t))
	if createResult.ExitCode != 0 {
		t.Fatalf("hop task create (hostile instructions): exit=%d stdout=%q stderr=%q", createResult.ExitCode, createResult.Stdout, createResult.Stderr)
	}
	taskID := parseTaskID(t, createResult.Stdout)

	closeResult := runManagerVerb(t, managerEnv, repo.Root, "plan", "close", "--request-id", newSpikeUUID(t))
	if closeResult.ExitCode != 0 && !strings.HasPrefix(closeResult.FirstStdoutLine(), "duplicate") {
		t.Fatalf("hop plan close: exit=%d stdout=%q stderr=%q", closeResult.ExitCode, closeResult.Stdout, closeResult.Stderr)
	}

	fx.requireTaskState(t, taskID, "active")
	attemptID, _ := fx.currentAttempt(t, taskID)
	sessionID := fx.sessionForAttempt(t, attemptID)

	instructionsArtifactPath := fx.scalar(t, fmt.Sprintf("SELECT instructions_path FROM tasks WHERE id = '%s';", taskID))
	if instructionsArtifactPath == "" {
		t.Fatalf("task %s has no recorded instructions_path", taskID)
	}
	requireByteExactFile(t, instructionsArtifactPath, instructionsContent)
	// The worker fixture's OWN evidence for this channel: unlike the
	// manager's own assignment.md (dumped verbatim above), a feature-mode
	// implementer's per-attempt assignment.md is a SEPARATE templated
	// document (internal/app/templates.go's renderAssignment) that only
	// NAMES the task instructions file by path -- it never embeds its
	// content -- so writeObservation instead dumps a digest of the
	// worker's own independent read of that exact path
	// (instructions_path/instructions_sha256/instructions_bytes; see
	// fixtureworker_test.go's writeObservation and
	// fixtureprincipal_test.go's TestFixtureWorkerHoldBarrier for the
	// isolated unit coverage of this capability).
	workerObservationPath := filepath.Join(scratchDir, "worker-observed-"+attemptID+".txt")
	workerObs := waitForObservation(t, workerObservationPath)
	if workerObs.Fields["behavior"] != "worker-hold" {
		t.Errorf("worker observation dump behavior = %q, want %q", workerObs.Fields["behavior"], "worker-hold")
	}
	if workerObs.Fields["instructions_path"] != instructionsArtifactPath {
		t.Errorf("worker observation instructions_path = %q, want %q (the same path the store recorded)", workerObs.Fields["instructions_path"], instructionsArtifactPath)
	}
	if want := sha256HexOf(instructionsContent); workerObs.Fields["instructions_sha256"] != want {
		t.Errorf("worker's OWN observed instructions_sha256 = %q, want %q (sha256 of the exact hostile instructions content, computed from the worker's OWN independent read)", workerObs.Fields["instructions_sha256"], want)
	}
	if want := fmt.Sprintf("%d", len(instructionsContent)); workerObs.Fields["instructions_bytes"] != want {
		t.Errorf("worker's OWN observed instructions_bytes = %q, want %q", workerObs.Fields["instructions_bytes"], want)
	}

	// Channel 3: a manager answer -- seeded entirely with hostile bytes,
	// answering the worker-hold barrier question directly (never relayed
	// to a human here: this scenario is proving delivery bytes, not the
	// relay chain).
	questionID := fx.sessionQuestionTo(t, sessionID, "manager")
	answerPath := filepath.Join(scratchDir, "hostile-answer.md")
	if err := os.WriteFile(answerPath, []byte(hostilePayload), 0o600); err != nil {
		t.Fatalf("write hostile answer file: %v", err)
	}
	answerResult := runManagerVerb(t, managerEnv, repo.Root, "msg", "send", "--kind", "answer", "--reply-to", questionID, "--file", answerPath, "--request-id", newSpikeUUID(t))
	if answerResult.ExitCode != 0 {
		t.Fatalf("hop msg send --kind answer (hostile body): exit=%d stdout=%q stderr=%q", answerResult.ExitCode, answerResult.Stdout, answerResult.Stderr)
	}
	answerID := parseSentID(t, answerResult.Stdout)

	answerBodyPath, answerBodyDigest := "", ""
	row := fx.scalar(t, fmt.Sprintf("SELECT body_path || '|' || body_digest FROM messages WHERE id = '%s';", answerID))
	if parts := strings.SplitN(row, "|", 2); len(parts) == 2 {
		answerBodyPath, answerBodyDigest = parts[0], parts[1]
	}
	if answerBodyPath == "" {
		t.Fatalf("no message row found for the accepted answer %s", answerID)
	}
	requireByteExactFile(t, answerBodyPath, hostilePayload)
	if want := sha256HexOf(hostilePayload); answerBodyDigest != want {
		t.Errorf("stored body_digest for answer %s = %s, want %s (sha256 of the exact hostile bytes)", answerID, answerBodyDigest, want)
	}
	if !waitUntilDeadline(featureRunTimeout, func() bool {
		return messageDeliveredToSession(t, fx, answerID, sessionID)
	}) {
		t.Fatalf("answer %s was never delivered to session %s", answerID, sessionID)
	}
	if !waitUntilDeadline(featureRunTimeout, func() bool { return fx.messageAcked(t, answerID) }) {
		t.Fatalf("answer %s was never acknowledged", answerID)
	}
	// The worker fixture's OWN evidence for this channel: worker-hold
	// dumps a digest of the exact body file hop msg wait handed it for
	// the matching (reply-to == its own barrier question) answer --
	// writeContentDigest, a capability added for this scenario (see
	// fixtureworker_test.go and TestFixtureWorkerHoldBarrier's isolated
	// unit coverage) -- since every existing behavior otherwise
	// discards a fetched body's content once acked.
	answerObs := waitForObservation(t, filepath.Join(scratchDir, "answer-observed-"+attemptID+".txt"))
	if answerObs.Fields["id"] != answerID {
		t.Errorf("worker's own answer observation id = %q, want %q", answerObs.Fields["id"], answerID)
	}
	if want := sha256HexOf(hostilePayload); answerObs.Fields["sha256"] != want {
		t.Errorf("worker's OWN observed answer sha256 = %q, want %q (sha256 of the exact hostile bytes, computed from the worker's OWN independent read)", answerObs.Fields["sha256"], want)
	}
	if want := fmt.Sprintf("%d", len(hostilePayload)); answerObs.Fields["bytes"] != want {
		t.Errorf("worker's OWN observed answer bytes = %q, want %q", answerObs.Fields["bytes"], want)
	}

	// The run completes normally, proving nothing about the hostile
	// content broke the pipeline.
	fx.requireTaskState(t, taskID, "integrated")
	fx.requireRunState(t, "completed")
	waitForControllerExit(t, fx.controller, 30*time.Second)

	// The positive control: pane.send_text through a SEPARATE connection
	// (exactly as a production subprocess connects), located by method
	// name, asserted present -- proving the elevated capture level and
	// the log's own correlation mechanics actually work, so a zero count
	// below is meaningful and not an artifact of a broken capture.
	controlMarker := "hop-injection-free-control-" + newSpikeUUID(t)
	controlClient := herdr.NewClient(server.socketPath)
	if err := controlClient.Call(testContext(t), "pane.send_text", map[string]any{"pane_id": controlPane, "text": controlMarker + "\n"}, nil); err != nil {
		t.Fatalf("positive control pane.send_text: %v", err)
	}

	// Window-end bracket.
	server.call(t, "tab.list", nil, &struct{}{})

	logPath := filepath.Join(filepath.Dir(server.socketPath), "herdr-server.log")
	var content string
	if !waitUntil(func() bool {
		data, readErr := os.ReadFile(logPath) //nolint:gosec // G304: fixed path derived from this test's own isolated socket directory.
		if readErr != nil {
			return false
		}
		content = string(data)
		return strings.Contains(content, `method="tab.list"`)
	}) {
		t.Fatalf("herdr-server.log at %s never recorded the window-end marker (tab.list); last content:\n%s", logPath, content)
	}
	artifacts.save(t, "herdr-server-log.txt", content)

	lines := strings.Split(content, "\n")
	startIdx, ok := firstLineIndex(lines, `method="pane.list"`)
	if !ok {
		t.Fatalf("herdr-server.log never recorded the window-start marker (pane.list)")
	}
	endIdx, ok := firstLineIndex(lines, `method="tab.list"`)
	if !ok {
		t.Fatalf("herdr-server.log never recorded the window-end marker (tab.list)")
	}
	if endIdx <= startIdx {
		t.Fatalf("window-end line %d is not after window-start line %d; the bracket is not usable for scoping a scan", endIdx, startIdx)
	}
	// Production never calls pane.list/tab.list today (verified by
	// grep), which is what makes firstLineIndex's FIRST-occurrence bracket
	// safe -- but that safety is silent and would change meaning without
	// warning if production ever did call either method. Asserting each
	// marker's call occurred EXACTLY ONCE across the whole capture makes
	// that assumption an executed check, not just a cited fact -- one API
	// call logs TWO lines (its own start and complete), both sharing the
	// same method="..." text, so the start and complete events are
	// counted separately rather than as raw method-matching lines (which
	// would always be even, never 1, for a method called exactly once).
	for _, m := range []string{"pane.list", "tab.list"} {
		if n := countLinesContainingAll(lines, `event="api.request.start"`, `method="`+m+`"`); n != 1 {
			t.Fatalf("herdr-server.log contains %d api.request.start line(s) naming method=%q, want exactly 1", n, m)
		}
		if n := countLinesContainingAll(lines, `event="api.request.complete"`, `method="`+m+`"`); n != 1 {
			t.Fatalf("herdr-server.log contains %d api.request.complete line(s) naming method=%q, want exactly 1", n, m)
		}
	}

	// S11's own finding: a request-log line never carries the pane/agent
	// target or any request parameter, so the control is located by
	// method + event alone -- unique here since production never calls
	// pane.send_text at all and this test issues it exactly once. Both its
	// start and complete records are located and excluded from the scan
	// below BY INDEX, never by string equality (two distinct lines in the
	// window can be byte-identical, e.g. two requests logged in the same
	// millisecond against the same method).
	controlStartIdx := requireLineIndex(t, lines, startIdx, endIdx, `method="pane.send_text"`, `event="api.request.start"`)
	controlCompleteIdx := requireLineIndex(t, lines, startIdx, endIdx, `method="pane.send_text"`, `event="api.request.complete"`)
	t.Logf("positive control located: start=%s complete=%s", lines[controlStartIdx], lines[controlCompleteIdx])

	requireNoUnexpectedTerminalInputRequests(t, lines, startIdx, endIdx, controlStartIdx, controlCompleteIdx)
	requireEveryRequestPaired(t, lines, startIdx, endIdx)
}

// messageDeliveredToSession reports whether a delivery row exists for
// message messageID addressed to sessionID specifically (never merely to
// its lineage), the same authority hop msg ack itself requires.
func messageDeliveredToSession(t *testing.T, fx *featureRun, messageID, sessionID string) bool {
	t.Helper()
	return fx.scalar(t, fmt.Sprintf("SELECT count(*) FROM message_deliveries WHERE message_id = '%s' AND session_id = '%s';", messageID, sessionID)) != "0"
}

// requireNoUnexpectedTerminalInputRequests asserts that within
// lines[startIdx+1:endIdx] no api.request.start, api.request.complete or
// api.request.fail entry exists for any of terminalInputMethods, other than
// the two identified positive-control lines (excluded BY INDEX, never by
// string equality). Scanning "start" as well as "complete"/"fail" is load
// bearing: a forbidden wait-mode agent.prompt call can leave only a
// start record in the window (its completion lands only when the wait
// resolves, possibly long after the window closes, or never on some error
// paths) -- a completion-only scan would miss exactly that regression.
func requireNoUnexpectedTerminalInputRequests(t *testing.T, lines []string, startIdx, endIdx, controlStartIdx, controlCompleteIdx int) {
	t.Helper()
	var violations []string
	for i := startIdx + 1; i < endIdx; i++ {
		if i == controlStartIdx || i == controlCompleteIdx {
			continue
		}
		line := lines[i]
		isRequestEvent := strings.Contains(line, `event="api.request.start"`) ||
			strings.Contains(line, `event="api.request.complete"`) ||
			strings.Contains(line, `event="api.request.fail"`)
		if !isRequestEvent {
			continue
		}
		for _, m := range terminalInputMethods() {
			if strings.Contains(line, `method="`+m+`"`) {
				violations = append(violations, line)
			}
		}
	}
	if len(violations) > 0 {
		t.Fatalf("herdr-server.log records %d unexpected terminal-input request start/completion/failure(s) other than the identified positive control:\n%s",
			len(violations), strings.Join(violations, "\n"))
	}
}

// apiLogEventPattern, apiLogMethodPattern and apiLogRequestIDPattern extract
// one herdr-server.log request-log line's event/method/request_id fields
// (S11's plain-text format: space-separated key="value" fields, order not
// guaranteed).
var (
	apiLogEventPattern     = regexp.MustCompile(`event="([^"]*)"`)
	apiLogMethodPattern    = regexp.MustCompile(`method="([^"]*)"`)
	apiLogRequestIDPattern = regexp.MustCompile(`request_id="([^"]*)"`)
)

// apiLogLineFields returns one request-log line's event, method and
// request_id fields, and false if any of the three is absent (a line this
// scenario does not need to correlate, e.g. a non-request log line).
func apiLogLineFields(line string) (event, method, requestID string, ok bool) {
	em := apiLogEventPattern.FindStringSubmatch(line)
	mm := apiLogMethodPattern.FindStringSubmatch(line)
	rm := apiLogRequestIDPattern.FindStringSubmatch(line)
	if em == nil || mm == nil || rm == nil {
		return "", "", "", false
	}
	return em[1], mm[1], rm[1], true
}

// requireEveryRequestPaired asserts that every api.request.start line
// within lines[startIdx+1:endIdx] has a matching api.request.complete or
// api.request.fail line (same method and request_id) later in the window.
// This is a capture-integrity check, independent of the six-method scan
// above: a dropped or truncated completion (a log rotation mid-run, S11's
// 5 MiB/retained=0 limit) would otherwise leave a forbidden call's start
// record silently unpaired without failing loudly, and would make the
// window-scoped zero-violation count above less trustworthy. Pairing is
// counted per (method, request_id) key rather than matched line-for-line,
// since request_id is a per-connection counter (S11): two DIFFERENT
// connections can log the same id for the same method, but the counts
// still balance per key as long as every start eventually resolves.
func requireEveryRequestPaired(t *testing.T, lines []string, startIdx, endIdx int) {
	t.Helper()
	type reqKey struct{ method, requestID string }
	unpaired := map[reqKey]int{}
	for i := startIdx + 1; i < endIdx; i++ {
		event, method, requestID, ok := apiLogLineFields(lines[i])
		if !ok {
			continue
		}
		key := reqKey{method, requestID}
		switch event {
		case "api.request.start":
			unpaired[key]++
		case "api.request.complete", "api.request.fail":
			if unpaired[key] > 0 {
				unpaired[key]--
			}
		}
	}
	for key, count := range unpaired {
		if count > 0 {
			t.Errorf("herdr-server.log has %d api.request.start line(s) for method=%q request_id=%q with no paired completion or failure before the end bracket", count, key.method, key.requestID)
		}
	}
}
