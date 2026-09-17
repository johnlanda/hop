package integration

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// This file drives the fixture principal's new manager/reviewer/worker-hold
// dispatch logic directly (no herdr, no real hop binary), exactly as
// fixtureworker_test.go's existing TestFixtureWorker* tests drive the
// solo-worker behaviors: the compiled principal against a fake `hop` stub,
// asserting parsing and dispatch decisions from the principal's own stdout
// and the stub's invocation log. The real-process scenarios
// (featurerun_test.go, workerinterruption_test.go, reviewerrejection_test.go)
// exercise the same logic end to end against the real binary and a real
// server; this file isolates the pure decision logic the way a unit test
// would if the embedded program were importable, which it is not (it is a
// standalone generated module, compiled fresh per test like the fixture
// worker always has been).

// fixtureHoldMarker mirrors the identically named constant inside
// fixtureWorkerSource (the embedded program's own package-level constant,
// invisible to this outer test package since it lives only in generated
// program text) — retyped here, never derived, so this test's scripted
// bodies use literally the same token the compiled principal itself
// recognizes.
const fixtureHoldMarker = "FIXTURE-HOLD-BARRIER"

// fakeHopSource is a minimal, scriptable stand-in for the real hop binary:
// every worker-plumbing verb except msg wait/next returns a fixed,
// deterministic success line (task create's own line increments a
// caller-provided counter file so dependency ids are distinguishable), and
// every invocation's argv is appended to a log file the test reads back —
// sufficient to prove the fixture manager's/reviewer's/worker's own
// decisions (what it called, with what arguments, in what order) without
// needing a real store. msg wait/next instead serves the next block of a
// test-authored script (blocks separated by a line reading exactly "---"),
// advancing a persistent index file, so a test can hand the principal
// exactly the message sequence a scenario would.
const fakeHopSource = `package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

func main() {
	args := os.Args[1:]
	if logFile := os.Getenv("FAKE_HOP_LOG"); logFile != "" {
		f, err := os.OpenFile(logFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err == nil {
			fmt.Fprintln(f, strings.Join(args, "\t"))
			f.Close()
		}
	}
	if len(args) > 0 && args[0] == "status" {
		if os.Getenv("FAKE_HOP_STATUS_REJECTED") == "1" {
			fmt.Println("run r1 aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa\n  state:         running\n  guard shortfall: verdict-rejected")
		} else {
			fmt.Println("run r1 aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa\n  state:         running")
		}
		return
	}
	verb := ""
	switch {
	case len(args) >= 2:
		verb = args[0] + " " + args[1]
	case len(args) == 1:
		verb = args[0]
	}
	switch verb {
	case "result submit":
		fmt.Println("accepted 77777777-7777-4777-8777-777777777777")
	case "task create":
		if os.Getenv("FAKE_HOP_TASK_CREATE_REFUSED") == "1" {
			fmt.Println("refused: dependency-cycle")
			os.Exit(1)
		}
		if transientCallsLeft("FAKE_HOP_TASK_CREATE_TRANSIENT_COUNT", "FAKE_HOP_TASK_CREATE_TRANSIENT_COUNTER_FILE") {
			fmt.Println("transient: run is launching; retry")
			os.Exit(1)
		}
		n := nextCounter(os.Getenv("FAKE_HOP_TASK_COUNTER_FILE"))
		fmt.Printf("task 00000000-0000-4000-8000-%012d t%d created\n", n, n)
	case "task retry":
		fmt.Println("retry accepted t0 attempt 2")
	case "plan close":
		fmt.Println("plan closed")
	case "msg send":
		fmt.Println("sent 99999999-9999-4999-8999-999999999999")
	case "msg ack":
		id := ""
		if len(args) >= 3 {
			id = args[2]
		}
		fmt.Println("acknowledged " + id)
	case "review submit":
		fmt.Println("verdict accepted 88888888-8888-4888-8888-888888888888")
	case "msg wait", "msg next":
		printScripted()
	default:
		fmt.Println("refused: not-found")
		os.Exit(1)
	}
}

func nextCounter(path string) int {
	data, _ := os.ReadFile(path)
	n, _ := strconv.Atoi(strings.TrimSpace(string(data)))
	n++
	_ = os.WriteFile(path, []byte(strconv.Itoa(n)), 0o600)
	return n
}

// transientCallsLeft reports whether this call should still return
// "transient:" for the caller's own verb: countEnv names how many leading
// calls stay transient, tracked durably in counterFileEnv's own file
// (absent/unreadable counts as zero seen so far) so a SEPARATE fresh
// process (the fixture principal's own retry, its own exec.Command) sees
// the same running count. Both env vars empty/absent means "never
// transient" (the ordinary success path for every existing test).
func transientCallsLeft(countEnv, counterFileEnv string) bool {
	target := os.Getenv(countEnv)
	if target == "" {
		return false
	}
	n, err := strconv.Atoi(target)
	if err != nil || n <= 0 {
		return false
	}
	counterFile := os.Getenv(counterFileEnv)
	data, _ := os.ReadFile(counterFile)
	seen, _ := strconv.Atoi(strings.TrimSpace(string(data)))
	if seen >= n {
		return false
	}
	_ = os.WriteFile(counterFile, []byte(strconv.Itoa(seen+1)), 0o600)
	return true
}

func printScripted() {
	scriptPath := os.Getenv("FAKE_HOP_MSG_SCRIPT")
	idxPath := os.Getenv("FAKE_HOP_MSG_INDEX")
	content, err := os.ReadFile(scriptPath)
	if err != nil {
		time.Sleep(50 * time.Millisecond)
		fmt.Println("none: no message within 3s; run hop msg wait again")
		return
	}
	blocks := strings.Split(string(content), "\n---\n")
	idxData, _ := os.ReadFile(idxPath)
	idx, _ := strconv.Atoi(strings.TrimSpace(string(idxData)))
	if idx >= len(blocks) {
		time.Sleep(50 * time.Millisecond)
		fmt.Println("none: no message within 3s; run hop msg wait again")
		return
	}
	fmt.Print(blocks[idx])
	_ = os.WriteFile(idxPath, []byte(strconv.Itoa(idx+1)), 0o600)
}
`

// buildFakeHopStub compiles fakeHopSource once for the calling test into a
// temporary module under the test's artifact directory, mirroring
// buildFixtureWorker's own build shape.
func buildFakeHopStub(t *testing.T, artifacts *artifactDir) string {
	t.Helper()
	src := artifacts.dir(t, "fake-hop-src")
	if err := os.WriteFile(filepath.Join(src, "go.mod"), []byte("module fakehop\n\ngo 1.21\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "main.go"), []byte(fakeHopSource), 0o600); err != nil {
		t.Fatal(err)
	}
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Fatalf("the go tool is required to build the fake hop stub: %v", err)
	}
	binDir := artifacts.dir(t, "fake-hop-bin")
	out := filepath.Join(binDir, "fakehop")
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	build := exec.CommandContext(ctx, goBin, "build", "-o", out, ".") //nolint:gosec // G204: the go tool builds this test's own generated fixture module.
	build.Dir = src
	if combined, buildErr := build.CombinedOutput(); buildErr != nil {
		t.Fatalf("go build fake hop stub: %v\n%s", buildErr, combined)
	}
	return out
}

// fakeMessageBlock renders one hop msg wait/next scripted response block,
// matching internal/app/grammar.go's GrammarMessageLine/GrammarBodyLine
// shape (optional fields omitted when empty, exactly as the real grammar
// renders them). relay-of is omitted: no scenario in this file scripts a
// question the manager itself relayed being re-served to it.
func fakeMessageBlock(id, kind, from, replyTo, origin, bodyPath string) string {
	line := "message " + id + " kind=" + kind + " from=" + from
	if replyTo != "" {
		line += " reply-to=" + replyTo
	}
	if origin != "" {
		line += " origin=" + origin
	}
	return line + "\nbody: " + bodyPath + "\nack: hop msg ack " + id + "\n"
}

// writeFakeHopMsgScript joins blocks with the fake hop stub's block
// delimiter and writes both the script and a fresh index file (starting at
// 0), returning the two paths as FAKE_HOP_MSG_SCRIPT/FAKE_HOP_MSG_INDEX
// values.
func writeFakeHopMsgScript(t *testing.T, dir string, blocks []string) (scriptPath, indexPath string) {
	t.Helper()
	scriptPath = filepath.Join(dir, "msg-script.txt")
	if err := os.WriteFile(scriptPath, []byte(strings.Join(blocks, "\n---\n")), 0o600); err != nil {
		t.Fatal(err)
	}
	indexPath = filepath.Join(dir, "msg-index.txt")
	if err := os.WriteFile(indexPath, []byte("0"), 0o600); err != nil {
		t.Fatal(err)
	}
	return scriptPath, indexPath
}

// newFakeHopTaskCounter writes a fresh task-create counter file starting
// at 0, returning its path as the FAKE_HOP_TASK_COUNTER_FILE value.
func newFakeHopTaskCounter(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "task-counter.txt")
	if err := os.WriteFile(path, []byte("0"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// readFakeHopLog reads a fake-hop-stub invocation log, tolerating absence
// (no verb was ever invoked).
func readFakeHopLog(t *testing.T, path string) string {
	t.Helper()
	content, err := os.ReadFile(path) //nolint:gosec // G304: a path this test constructed itself, under its own artifact directory.
	if err != nil {
		if os.IsNotExist(err) {
			return ""
		}
		t.Fatal(err)
	}
	return string(content)
}

// logHasLine reports whether the fake-hop invocation log carries a line
// starting with prefix and ending with suffix — used where a fresh
// --request-id (newRequestID, a different value on every logical call)
// sits between two fixed pieces of one invocation's argv, so neither an
// exact line match nor a single Contains substring pins the whole line.
func logHasLine(log, prefix, suffix string) bool {
	for _, line := range strings.Split(log, "\n") {
		if strings.HasPrefix(line, prefix) && strings.HasSuffix(line, suffix) {
			return true
		}
	}
	return false
}

// countLogLinesWithPrefix counts the fake-hop invocation log's own lines
// starting with prefix — used in place of an exact full-line count once a
// verb's own trailing --request-id (a fresh value per call) makes an
// exact line match brittle.
func countLogLinesWithPrefix(log, prefix string) int {
	n := 0
	for _, line := range strings.Split(log, "\n") {
		if strings.HasPrefix(line, prefix) {
			n++
		}
	}
	return n
}

// TestFixtureManagerScriptDispatch drives the compiled fixture principal as
// a manager-feature manager (HOP_ROLE=manager) against the fake hop stub,
// proving the manager-feature behavior's complete dispatch in isolation:
// scripted task creation with a dependency (t2's --depends-on carries t1's
// OWN reported id, not a guessed one), plan close, a scripted question
// relayed to the human (fixtureHoldMarker's ANSWER rule), a human answer
// forwarded back through the relay chain using only the envelope's origin
// field, a needs-rework notice triggering hop task retry against the
// correct task id, and an unrecognized info notice (the review verdict's
// own notice carries no marker at all — the fixture reviewer's reasons
// text is opaque prose) triggering a hop status check that names the
// verdict-rejected guard shortfall (STATUS-1, scripted via
// FAKE_HOP_STATUS_REJECTED), which plans a fix task and a second plan
// close.
func TestFixtureManagerScriptDispatch(t *testing.T) {
	artifacts := newArtifactDir(t)
	principal := buildFixtureWorker(t, artifacts)
	fakeHop := buildFakeHopStub(t, artifacts)

	stateDir := artifacts.dir(t, "state")
	scratchDir := artifacts.dir(t, "manager-scratch")
	const runID = "d1d1d1d1-d1d1-4d1d-8d1d-d1d1d1d1d1d1"
	assignmentPath := filepath.Join(stateDir, "runs", runID, "artifacts", "assignment.md")
	if err := os.MkdirAll(filepath.Dir(assignmentPath), 0o700); err != nil {
		t.Fatal(err)
	}

	brief := fixtureManagerBrief(
		scratchDir,
		[]fixtureManagerTask{
			{Label: "t1", Title: "Implement t1", Behavior: "worker-implement"},
			{Label: "t2", Title: "Implement t2", Behavior: "worker-implement", DependsOn: []string{"t1"}},
		},
		[]fixtureManagerAnswer{{Match: fixtureHoldMarker, Action: "relay"}},
		"worker-implement",
	)
	assignmentContent := "# HOP Manager Assignment\n\nRun: " + runID + "\n\n## Brief\n\n" + brief + "\n## Instructions\n"
	if err := os.WriteFile(assignmentPath, []byte(assignmentContent), 0o600); err != nil {
		t.Fatal(err)
	}

	scriptDir := artifacts.dir(t, "manager-script")
	bodyDir := artifacts.dir(t, "manager-bodies")
	writeBody := func(name, content string) string {
		path := filepath.Join(bodyDir, name)
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	holdBody := writeBody("hold-question.txt", fixtureHoldMarker+"\n")
	humanAnswerBody := writeBody("human-answer.txt", "release the barrier\n")
	needsReworkBody := writeBody("needs-rework.txt", "task t1 needs-rework\nreason: worker exited without an accepted result\n")
	// The review-verdict notice's own body is opaque reviewer prose in
	// production (sqlite/review.go's persistVerdictAcceptance uses the
	// reviewer's own reasons artifact verbatim, with no marker at all);
	// its content here is deliberately unremarkable to prove the manager
	// does not key off it — detection is via hop status only.
	rejectNoticeBody := writeBody("verdict-notice.txt", "fixture reviewer reasons: reject\n")

	blocks := []string{
		fakeMessageBlock("msg-hold-question", "question", "worker-session-1", "", "", holdBody),
		fakeMessageBlock("msg-human-answer", "answer", "human", "msg-relay-question", "msg-hold-question", humanAnswerBody),
		fakeMessageBlock("msg-needs-rework", "info", "controller", "", "", needsReworkBody),
		fakeMessageBlock("msg-reject-verdict", "info", "controller", "", "", rejectNoticeBody),
	}
	scriptPath, indexPath := writeFakeHopMsgScript(t, scriptDir, blocks)
	counterPath := newFakeHopTaskCounter(t, scriptDir)
	logPath := filepath.Join(scriptDir, "log.txt")

	// Resolved through symlinks (macOS's /var -> /private/var) to match
	// what the manager's own os.Getwd() returns inside the child process,
	// exactly like cmd.Dir being a symlinked path never changes the
	// resolved cwd a process observes.
	cwd, err := filepath.EvalSymlinks(artifacts.dir(t, "manager-cwd"))
	if err != nil {
		t.Fatal(err)
	}
	const rolePath, cribPath = "/state/runs/r/artifacts/roles/manager.md", "/state/runs/r/artifacts/worker-protocol.md"
	prompt := testManagerInitialPrompt(assignmentPath, rolePath, cribPath, fakeHop)

	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, principal, "--session-id", "22222222-2222-2222-2222-222222222222", prompt) //nolint:gosec // G204: fixed test-owned binary and arguments.
	cmd.Dir = cwd
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOP_STATE_DIR=" + stateDir,
		"HOP_RUN_ID=" + runID,
		"HOP_SESSION_ID=33333333-3333-3333-3333-333333333333",
		"HOP_INCARNATION_ID=44444444-4444-4444-4444-444444444444",
		"HOP_ROLE=manager",
		"FAKE_HOP_MSG_SCRIPT=" + scriptPath,
		"FAKE_HOP_MSG_INDEX=" + indexPath,
		"FAKE_HOP_TASK_COUNTER_FILE=" + counterPath,
		"FAKE_HOP_LOG=" + logPath,
		// STATUS-1 has not landed (defect id reported and confirmed);
		// this scripts the fake hop's status output to carry the
		// verdict-rejected shortfall so the manager's own hop-status
		// polling branch is exercised in isolation ahead of it.
		"FAKE_HOP_STATUS_REJECTED=1",
	}
	var out strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &out
	// The manager never exits on its own (per-attempt retirement is what a
	// real scenario proves terminates it); this test bounds it with a
	// context deadline instead, exactly like waiting out an intentionally
	// endless composer-idle loop, and asserts on everything it did before
	// the deadline killed it.
	_ = cmd.Run() //nolint:errcheck // the context deadline killing this intentionally endless manager is the expected outcome, asserted on its captured stdout below, never on this error.

	stdout := out.String()
	for _, want := range []string{
		"FIXTURE-MANAGER-READY",
		"FIXTURE-TASK-CREATED label=[t1] id=[00000000-0000-4000-8000-000000000001]",
		"FIXTURE-TASK-CREATED label=[t2] id=[00000000-0000-4000-8000-000000000002]",
		"FIXTURE-PLAN-CLOSED",
		"FIXTURE-RELAYED question=[msg-hold-question]",
		"FIXTURE-FORWARDED origin=[msg-hold-question]",
		"FIXTURE-RETRIED label=[t1] result=[retry accepted t0 attempt 2]",
		"FIXTURE-STATUS-CHECKED rejected=[true]",
		// fixCounter starts at len(script.Tasks) (2: t1, t2), so the first
		// fix task's own local label is "fix3", not "fix1" — a manager-side
		// bookkeeping label distinct from the store's own t<seq> numbering.
		"FIXTURE-FIX-TASK-CREATED label=[fix3] id=[00000000-0000-4000-8000-000000000003]",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("manager stdout missing %q; got:\n%s", want, stdout)
		}
	}

	log := readFakeHopLog(t, logPath)
	for _, want := range []string{
		"task\tcreate\t--title\tImplement t1\t--file\t",
		"task\tcreate\t--title\tImplement t2\t--file\t",
		"--depends-on\t00000000-0000-4000-8000-000000000001",
		"plan\tclose",
		"msg\tsend\t--to\thuman\t--kind\tquestion\t--relay-of\tmsg-hold-question\t--file\t" + holdBody,
		"msg\tsend\t--kind\tanswer\t--reply-to\tmsg-hold-question\t--file\t" + humanAnswerBody,
		"task\tretry\t--reason\tfixture retry after interruption\t--request-id\t",
		"status\t-C\t" + cwd + "\t-run\t" + runID,
		"task\tcreate\t--title\tfix from reject\t--file\t",
	} {
		if !strings.Contains(log, want) {
			t.Errorf("fake hop invocation log missing %q; got:\n%s", want, log)
		}
	}
	for _, id := range []string{"msg-hold-question", "msg-human-answer", "msg-needs-rework", "msg-reject-verdict"} {
		if !strings.Contains(log, "msg\tack\t"+id) {
			t.Errorf("fake hop invocation log missing an ack of %s; got:\n%s", id, log)
		}
	}
	// The retry call's own --request-id sits BEFORE the positional task id
	// (task retry's one positional argument), so the retried task id is
	// asserted as the line's own suffix rather than assuming adjacency to
	// --reason.
	if !logHasLine(log, "task\tretry\t--reason\tfixture retry after interruption\t--request-id\t", "\t00000000-0000-4000-8000-000000000001") {
		t.Errorf("fake hop invocation log missing a task retry of the expected task id; got:\n%s", log)
	}
	// The fix task's own plan close (the second one, after the reject
	// notice) must also appear: two "plan close" lines total. Each now
	// carries its own --request-id suffix (runHopCLIRetryable/newRequestID),
	// so lines are matched by prefix rather than an exact "plan close\n" line.
	if got := countLogLinesWithPrefix(log, "plan\tclose"); got != 2 {
		t.Errorf("plan close invocation count = %d, want 2 (the initial close and the post-fix-task reclose); log:\n%s", got, log)
	}
}

// requestIDFromLogLine extracts the value following a "--request-id"
// token in one fake-hop invocation log line, "" if absent.
func requestIDFromLogLine(t *testing.T, line string) string {
	t.Helper()
	fields := strings.Split(line, "\t")
	for i, f := range fields {
		if f == "--request-id" && i+1 < len(fields) {
			return fields[i+1]
		}
	}
	return ""
}

// managerScriptDispatchFixture bundles managerScriptDispatchFixture's setup
// (a struct rather than many return values, so the two tests that build it
// stay a plain field-access away from a single build call).
type managerScriptDispatchFixture struct {
	principal, fakeHop, stateDir, runID, cwd, logPath, counterPath, assignmentPath string
}

// buildManagerScriptDispatchFixture builds the common one-task manager
// fixture TestFixtureManagerRetriesTransientTaskCreate and
// TestFixtureManagerNeverRetriesRefused each drive with a different
// FAKE_HOP_TASK_CREATE_* script: a single "t1" task, no answers, no fix
// behavior — only hop task create's own outcome is under test here.
func buildManagerScriptDispatchFixture(t *testing.T, artifacts *artifactDir) managerScriptDispatchFixture {
	t.Helper()
	fx := managerScriptDispatchFixture{
		principal: buildFixtureWorker(t, artifacts),
		fakeHop:   buildFakeHopStub(t, artifacts),
		stateDir:  artifacts.dir(t, "state"),
		runID:     "a1a1a1a1-a1a1-4a1a-8a1a-a1a1a1a1a1a1",
	}
	scratchDir := artifacts.dir(t, "manager-scratch")
	fx.assignmentPath = filepath.Join(fx.stateDir, "runs", fx.runID, "artifacts", "assignment.md")
	if err := os.MkdirAll(filepath.Dir(fx.assignmentPath), 0o700); err != nil {
		t.Fatal(err)
	}
	brief := fixtureManagerBrief(scratchDir, []fixtureManagerTask{{Label: "t1", Title: "Implement t1", Behavior: "worker-implement"}}, nil, "")
	assignmentContent := "# HOP Manager Assignment\n\nRun: " + fx.runID + "\n\n## Brief\n\n" + brief + "\n## Instructions\n"
	if err := os.WriteFile(fx.assignmentPath, []byte(assignmentContent), 0o600); err != nil {
		t.Fatal(err)
	}

	scriptDir := artifacts.dir(t, "manager-script")
	fx.counterPath = newFakeHopTaskCounter(t, scriptDir)
	fx.logPath = filepath.Join(scriptDir, "log.txt")

	cwd, err := filepath.EvalSymlinks(artifacts.dir(t, "manager-cwd"))
	if err != nil {
		t.Fatal(err)
	}
	fx.cwd = cwd
	return fx
}

// TestFixtureManagerRetriesTransientTaskCreate proves the fixture
// principal's generalized transient-retry behavior (runHopCLIRetryable):
// a "transient: ..." first line from hop task create is retried, backing
// off fixtureRetryInterval, until it eventually succeeds — needed because
// LAUNCH-1's production fix returns exactly this shape for a manager verb
// racing its own launch corroboration while the run is still launching or
// resuming (design section 7/8) — and the retry reuses the IDENTICAL
// --request-id on every attempt of the same logical call, never minting a
// fresh one per retry, so the retry is idempotent server-side.
func TestFixtureManagerRetriesTransientTaskCreate(t *testing.T) {
	artifacts := newArtifactDir(t)
	fx := buildManagerScriptDispatchFixture(t, artifacts)
	transientCounterPath := filepath.Join(filepath.Dir(fx.logPath), "transient-counter.txt")

	const rolePath, cribPath = "/state/runs/a1/artifacts/roles/manager.md", "/state/runs/a1/artifacts/worker-protocol.md"
	prompt := testManagerInitialPrompt(fx.assignmentPath, rolePath, cribPath, fx.fakeHop)

	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, fx.principal, "--session-id", "22222222-2222-2222-2222-222222222222", prompt) //nolint:gosec // G204: fixed test-owned binary and arguments.
	cmd.Dir = fx.cwd
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOP_STATE_DIR=" + fx.stateDir,
		"HOP_RUN_ID=" + fx.runID,
		"HOP_SESSION_ID=33333333-3333-3333-3333-333333333333",
		"HOP_INCARNATION_ID=44444444-4444-4444-4444-444444444444",
		"HOP_ROLE=manager",
		"FAKE_HOP_TASK_COUNTER_FILE=" + fx.counterPath,
		"FAKE_HOP_LOG=" + fx.logPath,
		// No FAKE_HOP_MSG_SCRIPT: an unset script path makes every msg
		// wait/next return "none: ..." (printScripted's own os.ReadFile
		// failure branch), exactly the idle-forever behavior this test
		// needs once planning finishes — the context deadline is what
		// ends the manager, mirroring TestFixtureManagerScriptDispatch.
		"FAKE_HOP_TASK_CREATE_TRANSIENT_COUNT=1",
		"FAKE_HOP_TASK_CREATE_TRANSIENT_COUNTER_FILE=" + transientCounterPath,
	}
	var out strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &out
	_ = cmd.Run() //nolint:errcheck // the context deadline killing this intentionally endless manager is the expected outcome, asserted on its captured stdout below, never on this error.

	stdout := out.String()
	for _, want := range []string{
		"FIXTURE-MANAGER-READY",
		"FIXTURE-TASK-CREATED label=[t1] id=[00000000-0000-4000-8000-000000000001]",
		"FIXTURE-PLAN-CLOSED",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("manager stdout missing %q (the transient retry must eventually succeed); got:\n%s", want, stdout)
		}
	}

	log := readFakeHopLog(t, fx.logPath)
	var createLines []string
	for _, line := range strings.Split(log, "\n") {
		if strings.HasPrefix(line, "task\tcreate\t--title\tImplement t1\t") {
			createLines = append(createLines, line)
		}
	}
	if len(createLines) != 2 {
		t.Fatalf("task create invocation count for t1 = %d, want 2 (one transient, one accepted); log:\n%s", len(createLines), log)
	}
	firstID, secondID := requestIDFromLogLine(t, createLines[0]), requestIDFromLogLine(t, createLines[1])
	if firstID == "" {
		t.Fatalf("first task create attempt carries no --request-id; log line: %q", createLines[0])
	}
	if firstID != secondID {
		t.Errorf("task create retry used request ids %q then %q, want the SAME id reused on retry (idempotency depends on it)", firstID, secondID)
	}
}

// TestFixtureManagerNeverRetriesRefused proves runHopCLIRetryable never
// retries a hard "refused:" line — only a literal "transient:" prefix is
// ever retryable (design section 7: a refusal is not something the
// protocol tells the caller to retry). The manager must call hop task
// create EXACTLY once and then fail its own run loudly (fatalf), never
// looping the way a transient response does.
func TestFixtureManagerNeverRetriesRefused(t *testing.T) {
	artifacts := newArtifactDir(t)
	fx := buildManagerScriptDispatchFixture(t, artifacts)

	const rolePath, cribPath = "/state/runs/a1/artifacts/roles/manager.md", "/state/runs/a1/artifacts/worker-protocol.md"
	prompt := testManagerInitialPrompt(fx.assignmentPath, rolePath, cribPath, fx.fakeHop)

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, fx.principal, "--session-id", "22222222-2222-2222-2222-222222222222", prompt) //nolint:gosec // G204: fixed test-owned binary and arguments.
	cmd.Dir = fx.cwd
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOP_STATE_DIR=" + fx.stateDir,
		"HOP_RUN_ID=" + fx.runID,
		"HOP_SESSION_ID=33333333-3333-3333-3333-333333333333",
		"HOP_INCARNATION_ID=44444444-4444-4444-4444-444444444444",
		"HOP_ROLE=manager",
		"FAKE_HOP_TASK_COUNTER_FILE=" + fx.counterPath,
		"FAKE_HOP_LOG=" + fx.logPath,
		"FAKE_HOP_TASK_CREATE_REFUSED=1",
	}
	var out strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &out
	runErr := cmd.Run()
	if runErr == nil {
		t.Fatalf("manager exited 0 despite hop task create being refused; want a fatal non-zero exit; stdout/stderr:\n%s", out.String())
	}

	stdout := out.String()
	if strings.Contains(stdout, "FIXTURE-TASK-CREATED") {
		t.Errorf("manager reported a created task despite the refusal; got:\n%s", stdout)
	}
	if !strings.Contains(stdout, "fixture principal: manager script: hop task create for t1 was refused: refused: dependency-cycle") {
		t.Errorf("manager output does not name the refusal it fataled on; got:\n%s", stdout)
	}

	log := readFakeHopLog(t, fx.logPath)
	if got := countLogLinesWithPrefix(log, "task\tcreate\t--title\tImplement t1\t"); got != 1 {
		t.Errorf("task create invocation count for t1 = %d, want exactly 1 (a refused: line must never be retried); log:\n%s", got, log)
	}
}

// TestFixtureReviewerRejectOnce drives the compiled fixture principal as a
// reviewer-reject-once reviewer (HOP_ROLE=reviewer) TWICE against the fake
// hop stub, sharing the same HOP_STATE_DIR/HOP_RUN_ID across both
// invocations exactly as two separate review tasks' reviewer sessions
// would: the first invocation must reject and durably record its one-shot
// marker, and the second — a fresh process, as a real second review task's
// reviewer session always is — must find the marker and approve.
func TestFixtureReviewerRejectOnce(t *testing.T) {
	artifacts := newArtifactDir(t)
	principal := buildFixtureWorker(t, artifacts)
	fakeHop := buildFakeHopStub(t, artifacts)

	stateDir := artifacts.dir(t, "state")
	scratchDir := artifacts.dir(t, "reviewer-scratch")
	const runID = "e2e2e2e2-e2e2-4e2e-8e2e-e2e2e2e2e2e2"
	rolePath := filepath.Join(stateDir, "runs", runID, "artifacts", "roles", "reviewer.md")
	if err := os.MkdirAll(filepath.Dir(rolePath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rolePath, []byte("FIXTURE-BEHAVIOR: reviewer-reject-once "+scratchDir+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	runReviewerOnce := func(attemptID string) (verdict string) {
		t.Helper()
		assignmentPath := filepath.Join(stateDir, "runs", runID, "attempts", attemptID, "assignment.md")
		if err := os.MkdirAll(filepath.Dir(assignmentPath), 0o700); err != nil {
			t.Fatal(err)
		}
		content := "# HOP Review Assignment\n\nCommit: cccccccccccccccccccccccccccccccccccccccc\nTree: dddddddddddddddddddddddddddddddddddddddd\n"
		if err := os.WriteFile(assignmentPath, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		prompt := testReviewerInitialPrompt(assignmentPath, fakeHop)

		dir := artifacts.dir(t, "reviewer-"+attemptID)
		logPath := filepath.Join(dir, "log.txt")
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, principal, "--session-id", "55555555-5555-5555-5555-555555555555", prompt) //nolint:gosec // G204: fixed test-owned binary and arguments.
		cmd.Dir = artifacts.dir(t, "reviewer-cwd-"+attemptID)
		cmd.Env = []string{
			"PATH=" + os.Getenv("PATH"),
			"HOP_STATE_DIR=" + stateDir,
			"HOP_RUN_ID=" + runID,
			"HOP_SESSION_ID=66666666-6666-6666-6666-666666666666",
			"HOP_INCARNATION_ID=77777777-7777-7777-7777-777777777777",
			"HOP_TASK_ID=88888888-8888-8888-8888-888888888888",
			"HOP_ATTEMPT_ID=" + attemptID,
			"HOP_ROLE=reviewer",
			"FAKE_HOP_LOG=" + logPath,
		}
		var out strings.Builder
		cmd.Stdout = &out
		cmd.Stderr = &out
		cmd.Stdin = strings.NewReader("FIXTURE-QUIT\n")
		if err := cmd.Run(); err != nil {
			t.Fatalf("run fixture reviewer: %v\nstdout/stderr:\n%s", err, out.String())
		}
		if !strings.Contains(out.String(), "FIXTURE-REVIEWER-READY") {
			t.Fatalf("reviewer did not report ready; output:\n%s", out.String())
		}
		log := readFakeHopLog(t, logPath)
		switch {
		case strings.Contains(log, "--verdict\treject"):
			return "reject"
		case strings.Contains(log, "--verdict\tapprove"):
			return "approve"
		default:
			t.Fatalf("no review submit verdict found in fake hop log:\n%s", log)
			return ""
		}
	}

	if verdict := runReviewerOnce("attempt-1111111111111111111111111111111"); verdict != "reject" {
		t.Errorf("first reviewer-reject-once invocation verdict = %q, want reject", verdict)
	}
	if verdict := runReviewerOnce("attempt-2222222222222222222222222222222"); verdict != "approve" {
		t.Errorf("second reviewer-reject-once invocation (marker already recorded) verdict = %q, want approve", verdict)
	}
}

// TestFixtureWorkerHoldBarrier drives the compiled fixture principal as a
// feature-mode worker-hold implementer (HOP_ROLE=implementer) against the
// fake hop stub: it must send exactly one question carrying
// fixtureHoldMarker, wait for a reply-to-matching answer (never accepting
// an unrelated delivered message as its release), ack it, drain its
// mailbox, and only then submit — proving the barrier logic in isolation
// from any real messaging store.
func TestFixtureWorkerHoldBarrier(t *testing.T) {
	artifacts := newArtifactDir(t)
	worker := buildFixtureWorker(t, artifacts)
	fakeHop := buildFakeHopStub(t, artifacts)
	repo := newFixtureRepo(t, artifacts, nil, "repo")

	stateDir := artifacts.dir(t, "state")
	scratchDir := artifacts.dir(t, "worker-hold-scratch")
	const (
		runID     = "f3f3f3f3-f3f3-4f3f-8f3f-f3f3f3f3f3f3"
		taskID    = "11111111-2222-4333-8444-555555555555"
		attemptID = "66666666-7777-4888-8999-aaaaaaaaaaaa"
	)
	assignmentPath := filepath.Join(stateDir, "runs", runID, "attempts", attemptID, "assignment.md")
	if err := os.MkdirAll(filepath.Dir(assignmentPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(assignmentPath, []byte("# HOP Task Assignment\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	instructionsPath := filepath.Join(stateDir, "runs", runID, "tasks", taskID+".md")
	if err := os.MkdirAll(filepath.Dir(instructionsPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(instructionsPath, []byte("FIXTURE-BEHAVIOR: worker-hold "+scratchDir+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	scriptDir := artifacts.dir(t, "worker-hold-script")
	answerBody := filepath.Join(scriptDir, "answer-body.txt")
	if err := os.WriteFile(answerBody, []byte("released\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The barrier release's reply-to must equal the fixed id the fake hop
	// stub's own "msg send" always returns ("sent 99999999-...") — the
	// worker extracts the question id from THAT response, never a guess.
	blocks := []string{fakeMessageBlock("msg-answer", "answer", "manager-session", "99999999-9999-4999-8999-999999999999", "", answerBody)}
	scriptPath, indexPath := writeFakeHopMsgScript(t, scriptDir, blocks)
	logPath := filepath.Join(scriptDir, "log.txt")

	prompt := testAssignmentPrompt(assignmentPath, fakeHop)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, worker, "--session-id", "99999999-8888-4777-8666-555555555555", prompt) //nolint:gosec // G204: fixed test-owned binary and arguments.
	cmd.Dir = repo.Root
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOP_STATE_DIR=" + stateDir,
		"HOP_RUN_ID=" + runID,
		"HOP_TASK_ID=" + taskID,
		"HOP_ATTEMPT_ID=" + attemptID,
		"HOP_INCARNATION_ID=cccccccc-cccc-4ccc-8ccc-cccccccccccc",
		"HOP_ROLE=implementer",
		"FAKE_HOP_MSG_SCRIPT=" + scriptPath,
		"FAKE_HOP_MSG_INDEX=" + indexPath,
		"FAKE_HOP_LOG=" + logPath,
	}
	var out strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &out
	cmd.Stdin = strings.NewReader("FIXTURE-QUIT\n")
	if err := cmd.Run(); err != nil {
		t.Fatalf("run fixture worker-hold: %v\nstdout/stderr:\n%s", err, out.String())
	}

	stdout := out.String()
	for _, want := range []string{
		"FIXTURE-WORKER-READY",
		"FIXTURE-HOLD-SENT question=[99999999-9999-4999-8999-999999999999]",
		"FIXTURE-HOLD-RELEASED answer=[msg-answer]",
		"FIXTURE-SUBMIT-RESULT",
		"FIXTURE-WORKER-IDLE",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("worker-hold stdout missing %q; got:\n%s", want, stdout)
		}
	}
	log := readFakeHopLog(t, logPath)
	if !strings.Contains(log, "msg\tsend\t--to\tmanager\t--kind\tquestion\t--file\t") {
		t.Errorf("fake hop invocation log missing the barrier question send; got:\n%s", log)
	}
	if !strings.Contains(log, "msg\tack\tmsg-answer") {
		t.Errorf("fake hop invocation log missing the release answer's ack; got:\n%s", log)
	}
	if !strings.Contains(log, "result\tsubmit\t") {
		t.Errorf("fake hop invocation log missing the post-release result submit; got:\n%s", log)
	}

	head := repo.git(t, "rev-parse", "HEAD^{commit}")
	if head == repo.Base {
		t.Error("worker-hold did not commit a change before submitting")
	}
}
