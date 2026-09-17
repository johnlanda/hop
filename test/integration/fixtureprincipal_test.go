package integration

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
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

// fixtureEvidenceInconsistentQuestionBody mirrors the identically named
// constant inside fixtureWorkerSource: the fixed body of the question the
// manager sends to human when hop status names the evidence-inconsistent
// shortfall on a notice it does not otherwise recognize.
const fixtureEvidenceInconsistentQuestionBody = "HOP's recorded evidence about this run's current head is inconsistent; please inspect the run."

// preForwardBarrierControlFile and preForwardBarrierObservedFile mirror
// the identically named constants inside fixtureWorkerSource: the opt-in
// control file a test creates under the manager's own scratch directory
// to enable RelayedQuestion's deterministic pre-forward barrier, and the
// fixed name of the observation the barrier dumps before blocking.
const (
	preForwardBarrierControlFile  = "manager-pre-forward-barrier"
	preForwardBarrierObservedFile = "manager-pre-forward-observed.txt"
)

// fakeHopSource is a minimal, scriptable stand-in for the real hop binary
// — the handwritten-fake law (design's review brief): it validates each
// supported verb's real argv/context contract before ever returning a
// scripted outcome, exactly as cmd/hop's own flag parsing and HOP_* env
// resolution do (required/forbidden flags, positional argument counts,
// UUID-shaped caller identities, readable body files), rejecting a
// missing, unknown or extra argument the same way the real CLI's own
// usage errors would rather than silently accepting it. It is NOT a
// store: dependency graphs, run/task state and workflow legality are
// never modeled — only the CLI-level contract every scripted outcome
// sits behind. A successful, syntactically-legible call still returns a
// fixed, deterministic line (task create's own line increments a
// caller-provided counter file so dependency ids are distinguishable),
// and every invocation's argv is appended to a log file the test reads
// back. msg wait/next instead serve the next block of a test-authored
// script (blocks separated by a line reading exactly "---"), advancing a
// persistent index file, so a test can hand the principal exactly the
// message sequence a scenario would; next and wait render DISTINCT
// empty-queue lines, per design section 7's grammar, once the script is
// exhausted or absent. Message ids used inside test-authored
// fakeMessageBlock scripts are canonical lowercase UUIDs (Astra pass 2 F5:
// real AckMessage/SendMessage call identity.ParseMessageID on ack targets
// and on --reply-to/--relay-of, so a label like "msg-hold-question" would
// succeed against this fake but never against the real binary), validated
// here the same way as the caller identities (HOP_RUN_ID/HOP_SESSION_ID/
// HOP_TASK_ID/HOP_ATTEMPT_ID/HOP_INCARNATION_ID) and the task ids this
// fake itself mints (task create's own id, --depends-on, task retry's
// positional argument).
const fakeHopSource = `package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
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
		runStatus(args[1:])
		return
	}
	verb, rest := "", args
	switch {
	case len(args) >= 2:
		verb, rest = args[0]+" "+args[1], args[2:]
	case len(args) == 1:
		verb, rest = args[0], args[1:]
	}
	switch verb {
	case "result submit":
		runResultSubmit(rest)
	case "task create":
		runTaskCreate(rest)
	case "task retry":
		runTaskRetry(rest)
	case "plan close":
		runPlanClose(rest)
	case "msg send":
		runMsgSend(rest)
	case "msg next":
		runMsgFetch(rest, false)
	case "msg wait":
		runMsgFetch(rest, true)
	case "msg ack":
		runMsgAck(rest)
	case "review submit":
		runReviewSubmit(rest)
	default:
		refuse("not-found")
	}
}

// refuse renders the grammar's refusal line and exits 1 — a rejected but
// syntactically legible request, distinct from a usage error.
func refuse(token string) {
	fmt.Println("refused: " + token)
	os.Exit(1)
}

// usageFail renders a usage complaint to stderr and exits 2, mirroring
// the real CLI's own flag/argument-count convention: a malformed
// invocation (missing, unknown or extra argument, or missing/invalid
// required context) is a caller bug, never a scripted store outcome.
func usageFail(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "fake hop: "+format+"\n", a...)
	os.Exit(2)
}

// parseFlags parses args against fs, exiting 2 on any parse error
// (unknown flag, malformed value) exactly like the real CLI's own
// flag.ContinueOnError + explicit exit convention, and returns the
// remaining positional arguments.
func parseFlags(fs *flag.FlagSet, args []string) []string {
	fs.SetOutput(os.Stderr)
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}
	return fs.Args()
}

// requireEnv fails the invocation (usage, exit 2) if any of names is
// empty, returning every value for convenience.
func requireEnv(names ...string) map[string]string {
	values := map[string]string{}
	for _, n := range names {
		v := os.Getenv(n)
		if v == "" {
			usageFail("required %s is not set", n)
		}
		values[n] = v
	}
	return values
}

// requireCallerContext validates the caller-identity contract every
// manager and message verb shares (HOP_RUN_ID/HOP_SESSION_ID/
// HOP_INCARNATION_ID, every one UUID-shaped, plus an absolute
// HOP_STATE_DIR — cmd/hop's requireWorkerStateRoot, which every message
// command calls, refuses a missing or relative value rather than
// resolving a default) — task create/retry, plan close and msg
// send/next/wait/ack all resolve their caller this same way, regardless
// of role (manager, worker or reviewer).
func requireCallerContext() {
	env := requireEnv("HOP_RUN_ID", "HOP_SESSION_ID", "HOP_INCARNATION_ID", "HOP_STATE_DIR")
	requireUUID("HOP_RUN_ID", env["HOP_RUN_ID"])
	requireUUID("HOP_SESSION_ID", env["HOP_SESSION_ID"])
	requireUUID("HOP_INCARNATION_ID", env["HOP_INCARNATION_ID"])
	if !filepath.IsAbs(env["HOP_STATE_DIR"]) {
		usageFail("HOP_STATE_DIR is not an absolute path")
	}
}

func requireUUID(label, value string) string {
	if !isUUIDShape(value) {
		usageFail("%s = %q is not UUID-shaped", label, value)
	}
	return value
}

func isUUIDShape(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if c != '-' {
				return false
			}
			continue
		}
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// requireReadableFile fails the invocation if path is empty or cannot be
// read — the fake's own counterpart of the real CLI's file-first
// protocol (task create's --file, msg send's --file, review submit's
// --reasons-file all read their body before any scripted outcome).
func requireReadableFile(label, path string) []byte {
	if path == "" {
		usageFail("%s is required", label)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		usageFail("%s %q is not readable: %v", label, path, err)
	}
	return body
}

// runResultSubmit validates hop result submit's real contract:
// --summary/--commit required, no positional arguments, a worker context
// (HOP_RUN_ID/HOP_TASK_ID/HOP_ATTEMPT_ID/HOP_INCARNATION_ID, every one
// UUID-shaped — no HOP_SESSION_ID, matching cmd/hop's own SubmitResult
// call).
func runResultSubmit(args []string) {
	fs := flag.NewFlagSet("result submit", flag.ContinueOnError)
	summary := fs.String("summary", "", "")
	commit := fs.String("commit", "", "")
	rest := parseFlags(fs, args)
	if len(rest) > 0 {
		usageFail("result submit: unexpected argument %q", rest[0])
	}
	if *summary == "" {
		usageFail("result submit: --summary is required")
	}
	if *commit == "" {
		usageFail("result submit: --commit is required")
	}
	env := requireEnv("HOP_RUN_ID", "HOP_TASK_ID", "HOP_ATTEMPT_ID", "HOP_INCARNATION_ID")
	requireUUID("HOP_RUN_ID", env["HOP_RUN_ID"])
	requireUUID("HOP_TASK_ID", env["HOP_TASK_ID"])
	requireUUID("HOP_ATTEMPT_ID", env["HOP_ATTEMPT_ID"])
	requireUUID("HOP_INCARNATION_ID", env["HOP_INCARNATION_ID"])
	if transientCallsLeft("FAKE_HOP_RESULT_SUBMIT_UNDELIVERED_COUNT", "FAKE_HOP_RESULT_SUBMIT_UNDELIVERED_COUNTER_FILE") {
		fmt.Println("transient: undelivered messages; drain with hop msg next, ack, then resubmit")
		os.Exit(1)
	}
	fmt.Println("accepted 77777777-7777-4777-8777-777777777777")
}

// runTaskCreate validates hop task create's real contract:
// --title/--file required, --depends-on repeatable (each value
// UUID-shaped — every dependency this fake itself ever mints is), no
// positional arguments, manager context. Validation runs BEFORE any
// scripted refused/transient outcome, matching the real CLI's own
// usage-before-business-logic order.
func runTaskCreate(args []string) {
	fs := flag.NewFlagSet("task create", flag.ContinueOnError)
	title := fs.String("title", "", "")
	file := fs.String("file", "", "")
	var deps stringListFlag
	fs.Var(&deps, "depends-on", "")
	fs.String("request-id", "", "")
	rest := parseFlags(fs, args)
	if len(rest) > 0 {
		usageFail("task create: unexpected argument %q", rest[0])
	}
	if *title == "" {
		usageFail("task create: --title is required")
	}
	requireReadableFile("--file", *file)
	for _, dep := range deps {
		requireUUID("--depends-on", dep)
	}
	requireCallerContext()

	if os.Getenv("FAKE_HOP_TASK_CREATE_REFUSED") == "1" {
		refuse("dependency-cycle")
	}
	if transientCallsLeft("FAKE_HOP_TASK_CREATE_TRANSIENT_COUNT", "FAKE_HOP_TASK_CREATE_TRANSIENT_COUNTER_FILE") {
		fmt.Println("transient: run not yet running; retry")
		os.Exit(1)
	}
	n := nextCounter(os.Getenv("FAKE_HOP_TASK_COUNTER_FILE"))
	fmt.Printf("task 00000000-0000-4000-8000-%012d t%d created\n", n, n)
}

// runTaskRetry validates hop task retry's real contract: --reason
// required, exactly one positional task-id (UUID-shaped), manager
// context.
func runTaskRetry(args []string) {
	fs := flag.NewFlagSet("task retry", flag.ContinueOnError)
	reason := fs.String("reason", "", "")
	fs.String("request-id", "", "")
	rest := parseFlags(fs, args)
	if len(rest) != 1 {
		usageFail("task retry: exactly one task-id argument is required")
	}
	if *reason == "" {
		usageFail("task retry: --reason is required")
	}
	requireUUID("task-id", rest[0])
	requireCallerContext()
	fmt.Println("retry accepted t0 attempt 2")
}

// runPlanClose validates hop plan close's real contract: no positional
// arguments, manager context.
func runPlanClose(args []string) {
	fs := flag.NewFlagSet("plan close", flag.ContinueOnError)
	fs.String("request-id", "", "")
	rest := parseFlags(fs, args)
	if len(rest) > 0 {
		usageFail("plan close: unexpected argument %q", rest[0])
	}
	requireCallerContext()
	fmt.Println("plan closed")
}

// isSupportedMessageKind mirrors internal/app/usecase_message.go's
// parseMessageKind: exactly question, info or answer.
func isSupportedMessageKind(s string) bool {
	switch s {
	case "question", "info", "answer":
		return true
	default:
		return false
	}
}

// isSupportedAddress mirrors internal/app/usecase_message.go's
// parseAddress: manager, human, or task:<uuid>.
func isSupportedAddress(s string) bool {
	switch {
	case s == "manager", s == "human":
		return true
	case strings.HasPrefix(s, "task:"):
		return isUUIDShape(strings.TrimPrefix(s, "task:"))
	default:
		return false
	}
}

// runMsgSend validates hop msg send's real contract: --kind required and
// one of question/info/answer; --to required (and a recognized address)
// unless --kind answer (forbidden then); --reply-to required for --kind
// answer (forbidden otherwise), and — like --relay-of — UUID-shaped when
// supplied, mirroring identity.ParseMessageID; exactly one of --file/
// --body, the resulting body non-empty (the real use case's own
// MessageMalformed rule); no positional arguments; a session context
// (every sender role — manager, worker, reviewer — resolves the same
// way).
func runMsgSend(args []string) {
	fs := flag.NewFlagSet("msg send", flag.ContinueOnError)
	to := fs.String("to", "", "")
	kind := fs.String("kind", "", "")
	replyTo := fs.String("reply-to", "", "")
	relayOf := fs.String("relay-of", "", "")
	file := fs.String("file", "", "")
	body := fs.String("body", "", "")
	fs.String("request-id", "", "")
	rest := parseFlags(fs, args)
	supplied := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { supplied[f.Name] = true })
	if len(rest) > 0 {
		usageFail("msg send: unexpected argument %q", rest[0])
	}
	if *kind == "" {
		usageFail("msg send: --kind is required")
	}
	if !isSupportedMessageKind(*kind) {
		usageFail("msg send: --kind %q is not one of question, info, answer", *kind)
	}
	if *kind == "answer" {
		if supplied["to"] {
			usageFail("msg send: --to is forbidden for --kind answer")
		}
		if *replyTo == "" {
			usageFail("msg send: --reply-to is required for --kind answer")
		}
	} else {
		if *to == "" {
			usageFail("msg send: --to is required")
		}
		if !isSupportedAddress(*to) {
			usageFail("msg send: --to %q is not a recognized address (manager, human, task:<uuid>)", *to)
		}
		if supplied["reply-to"] {
			usageFail("msg send: --reply-to is only valid for --kind answer")
		}
	}
	if supplied["reply-to"] {
		requireUUID("--reply-to", *replyTo)
	}
	if supplied["relay-of"] {
		requireUUID("--relay-of", *relayOf)
	}
	if supplied["file"] == supplied["body"] {
		usageFail("msg send: exactly one of --file and --body is required")
	}
	var bodyBytes []byte
	if supplied["file"] {
		bodyBytes = requireReadableFile("--file", *file)
	} else {
		bodyBytes = []byte(*body)
	}
	if len(bodyBytes) == 0 {
		usageFail("msg send: body is empty")
	}
	requireCallerContext()
	fmt.Println("sent 99999999-9999-4999-8999-999999999999")
}

// runMsgFetch validates hop msg next/wait's real contract: no positional
// arguments, a session context, and — for wait — a real duration
// --timeout that a nonsense value fails to parse (flag.Duration exits 2
// on a bad value exactly like every other malformed flag, mirroring the
// real CLI's own duration flag). waitVerb selects between the two verbs'
// DISTINCT empty-queue lines (design section 7's grammar: next's is the
// fixed "none: no queued message"; wait's names its own timeout) once
// the scripted message sequence is exhausted or absent.
func runMsgFetch(args []string, waitVerb bool) {
	fs := flag.NewFlagSet("msg fetch", flag.ContinueOnError)
	var timeout *time.Duration
	if waitVerb {
		timeout = fs.Duration("timeout", 3*time.Second, "")
	}
	rest := parseFlags(fs, args)
	if len(rest) > 0 {
		usageFail("msg next/wait: unexpected argument %q", rest[0])
	}
	requireCallerContext()
	if printScripted() {
		return
	}
	if waitVerb {
		time.Sleep(50 * time.Millisecond)
		fmt.Println("none: no message within " + timeout.String() + "; run hop msg wait again")
		return
	}
	fmt.Println("none: no queued message")
}

// runMsgAck validates hop msg ack's real contract: exactly one
// positional message-id, UUID-shaped (identity.ParseMessageID's
// contract), a session context.
func runMsgAck(args []string) {
	fs := flag.NewFlagSet("msg ack", flag.ContinueOnError)
	rest := parseFlags(fs, args)
	if len(rest) != 1 {
		usageFail("msg ack: exactly one message-id argument is required")
	}
	requireUUID("message-id", rest[0])
	requireCallerContext()
	fmt.Println("acknowledged " + rest[0])
}

// runReviewSubmit validates hop review submit's real contract:
// --verdict (approve|reject)/--subject/--reasons-file all required (the
// reasons file readable), no positional arguments, the reviewer context
// (HOP_RUN_ID/HOP_TASK_ID/HOP_ATTEMPT_ID/HOP_SESSION_ID/
// HOP_INCARNATION_ID, every one UUID-shaped).
func runReviewSubmit(args []string) {
	fs := flag.NewFlagSet("review submit", flag.ContinueOnError)
	verdict := fs.String("verdict", "", "")
	subject := fs.String("subject", "", "")
	reasonsFile := fs.String("reasons-file", "", "")
	rest := parseFlags(fs, args)
	if len(rest) > 0 {
		usageFail("review submit: unexpected argument %q", rest[0])
	}
	if *verdict != "approve" && *verdict != "reject" {
		usageFail("review submit: --verdict must be approve or reject")
	}
	if *subject == "" {
		usageFail("review submit: --subject is required")
	}
	requireReadableFile("--reasons-file", *reasonsFile)
	env := requireEnv("HOP_RUN_ID", "HOP_TASK_ID", "HOP_ATTEMPT_ID", "HOP_SESSION_ID", "HOP_INCARNATION_ID")
	for label, v := range env {
		requireUUID(label, v)
	}
	fmt.Println("verdict accepted 88888888-8888-4888-8888-888888888888")
}

// runStatus validates hop status's own minimal contract as this fake's
// only non-worker-plumbing verb: -C and -run required. Its guard-shortfall
// line reproduces STATUS-1's real rendering verbatim
// (internal/app/grammar.go's GrammarVerdictRejectedLine/GrammarShortfallLine,
// cmd/hop/statuscmd.go's featureDetailLines): FAKE_HOP_STATUS_SHORTFALL
// selects "verdict-rejected" (rendered from FAKE_HOP_STATUS_REVIEW/
// FAKE_HOP_STATUS_SUBJECT/FAKE_HOP_STATUS_REASONS) or
// "evidence-inconsistent" (value-free); absent or any other value renders
// no shortfall line at all — the current head has no unmet verdict guard.
func runStatus(args []string) {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	repoDir := fs.String("C", "", "")
	runArg := fs.String("run", "", "")
	rest := parseFlags(fs, args)
	if len(rest) > 0 {
		usageFail("status: unexpected argument %q", rest[0])
	}
	if *repoDir == "" {
		usageFail("status: -C is required")
	}
	if *runArg == "" {
		usageFail("status: -run is required")
	}
	fmt.Println("run r1 aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa\n  state:         running")
	switch os.Getenv("FAKE_HOP_STATUS_SHORTFALL") {
	case "verdict-rejected":
		fmt.Println("  shortfall: verdict-rejected review=" + os.Getenv("FAKE_HOP_STATUS_REVIEW") +
			" subject=" + os.Getenv("FAKE_HOP_STATUS_SUBJECT") + " reasons=" + os.Getenv("FAKE_HOP_STATUS_REASONS"))
	case "evidence-inconsistent":
		fmt.Println("  shortfall: evidence-inconsistent")
	}
}

// stringListFlag is a repeatable string flag (--depends-on), mirroring
// cmd/hop's own stringList.
type stringListFlag []string

func (s *stringListFlag) String() string { return strings.Join(*s, ",") }
func (s *stringListFlag) Set(v string) error {
	*s = append(*s, v)
	return nil
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

// printScripted serves the next block of the test-authored msg script,
// if one is configured and has a block left; false when there is nothing
// to deliver (no script configured, or the script is exhausted), leaving
// the caller to render its own verb-specific empty-queue line.
func printScripted() bool {
	scriptPath := os.Getenv("FAKE_HOP_MSG_SCRIPT")
	idxPath := os.Getenv("FAKE_HOP_MSG_INDEX")
	content, err := os.ReadFile(scriptPath)
	if err != nil {
		return false
	}
	blocks := strings.Split(string(content), "\n---\n")
	idxData, _ := os.ReadFile(idxPath)
	idx, _ := strconv.Atoi(strings.TrimSpace(string(idxData)))
	if idx >= len(blocks) {
		return false
	}
	fmt.Print(blocks[idx])
	_ = os.WriteFile(idxPath, []byte(strconv.Itoa(idx+1)), 0o600)
	return true
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
// text is opaque prose) triggering a hop status check whose verdict-rejected
// guard shortfall (STATUS-1, scripted via FAKE_HOP_STATUS_SHORTFALL) names
// a reasons path equal to this exact notice's own body path, which plans a
// fix task and a second plan close.
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

	const (
		msgHoldQuestionID  = "55555555-5555-4555-8555-555555555555"
		msgHumanAnswerID   = "66666666-6666-4666-8666-666666666666"
		msgNeedsReworkID   = "eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee"
		msgRejectVerdictID = "ffffffff-ffff-4fff-8fff-ffffffffffff"
		// msgRelayedQuestionID is the fixed id the fake hop stub's own "msg
		// send" always returns ("sent 99999999-..."): the human-facing
		// relay question's reply-to must equal it, exactly as the real
		// manager extracts the question id from that CLI response rather
		// than guessing one.
		msgRelayedQuestionID = "99999999-9999-4999-8999-999999999999"
	)
	blocks := []string{
		fakeMessageBlock(msgHoldQuestionID, "question", "worker-session-1", "", "", holdBody),
		fakeMessageBlock(msgHumanAnswerID, "answer", "human", msgRelayedQuestionID, msgHoldQuestionID, humanAnswerBody),
		fakeMessageBlock(msgNeedsReworkID, "info", "controller", "", "", needsReworkBody),
		fakeMessageBlock(msgRejectVerdictID, "info", "controller", "", "", rejectNoticeBody),
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
		// Scripts the fake hop's status output to carry STATUS-1's real
		// verdict-rejected shortfall line, its reasons path equal to
		// rejectNoticeBody — the exact body path the scripted reject
		// notice above carries — so the manager's own correlation rule
		// (STATUS-1's manager verdict channel) matches this notice and
		// plans a fix from it.
		"FAKE_HOP_STATUS_SHORTFALL=verdict-rejected",
		"FAKE_HOP_STATUS_REVIEW=dddddddd-dddd-4ddd-8ddd-dddddddddddd",
		"FAKE_HOP_STATUS_SUBJECT=cccccccccccccccccccccccccccccccccccccccc",
		"FAKE_HOP_STATUS_REASONS=" + rejectNoticeBody,
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
		"FIXTURE-RELAYED question=[" + msgHoldQuestionID + "]",
		"FIXTURE-FORWARDED origin=[" + msgHoldQuestionID + "]",
		"FIXTURE-RETRIED label=[t1] result=[retry accepted t0 attempt 2]",
		"FIXTURE-STATUS-CHECKED matched=[true]",
		// fixCounter starts at len(script.Tasks) (2: t1, t2), so the first
		// fix task's own local label is "fix3", not "fix1" — a manager-side
		// bookkeeping label distinct from the store's own t<seq> numbering.
		"FIXTURE-FIX-TASK-CREATED label=[fix3] id=[00000000-0000-4000-8000-000000000003] review=[dddddddd-dddd-4ddd-8ddd-dddddddddddd]",
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
		"msg\tsend\t--to\thuman\t--kind\tquestion\t--relay-of\t" + msgHoldQuestionID + "\t--file\t" + holdBody,
		"msg\tsend\t--kind\tanswer\t--reply-to\t" + msgHoldQuestionID + "\t--file\t" + humanAnswerBody,
		"task\tretry\t--reason\tfixture retry after interruption\t--request-id\t",
		"status\t-C\t" + cwd + "\t-run\t" + runID,
		"task\tcreate\t--title\tfix from reject\t--file\t",
	} {
		if !strings.Contains(log, want) {
			t.Errorf("fake hop invocation log missing %q; got:\n%s", want, log)
		}
	}
	for _, id := range []string{msgHoldQuestionID, msgHumanAnswerID, msgNeedsReworkID, msgRejectVerdictID} {
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

// TestFixtureManagerResumeSkipsReplanning proves a cold-relaunched
// manager (a resume-shaped invocation, exactly [<exe> --resume
// <native-ref> <continuation prompt>]) never re-runs its own script's
// task-creation/plan-close block: it re-reads the IDENTICAL assignment
// (the same brief a resumed run's own frozen state never changes), and
// each hop task create call mints a FRESH --request-id per invocation
// (newRequestID), so request-id idempotency alone would not catch a
// blind resumed re-run of the same script -- the manager itself must
// know not to re-plan. Drives the compiled fixture directly (no herdr)
// twice against the SAME state root and fake-hop invocation log: first
// launch plans (one task create, one plan close); the resumed relaunch
// must add neither.
func TestFixtureManagerResumeSkipsReplanning(t *testing.T) {
	artifacts := newArtifactDir(t)
	principal := buildFixtureWorker(t, artifacts)
	fakeHop := buildFakeHopStub(t, artifacts)

	stateDir := artifacts.dir(t, "state")
	scratchDir := artifacts.dir(t, "manager-scratch")
	const runID = "c2c2c2c2-c2c2-4c2c-8c2c-c2c2c2c2c2c2"
	assignmentPath := filepath.Join(stateDir, "runs", runID, "artifacts", "assignment.md")
	if err := os.MkdirAll(filepath.Dir(assignmentPath), 0o700); err != nil {
		t.Fatal(err)
	}
	brief := fixtureManagerBrief(scratchDir, []fixtureManagerTask{{Label: "t1", Title: "Implement t1", Behavior: "worker-implement"}}, nil, "")
	assignmentContent := "# HOP Manager Assignment\n\nRun: " + runID + "\n\n## Brief\n\n" + brief + "\n## Instructions\n"
	if err := os.WriteFile(assignmentPath, []byte(assignmentContent), 0o600); err != nil {
		t.Fatal(err)
	}

	scriptDir := artifacts.dir(t, "manager-script")
	counterPath := newFakeHopTaskCounter(t, scriptDir)
	logPath := filepath.Join(scriptDir, "log.txt")
	// No FAKE_HOP_MSG_SCRIPT: every hop msg wait call answers
	// "none: ..." immediately (runMsgFetch's own unscripted default), so
	// both launches just idle-poll harmlessly until their own context
	// deadline, exactly like TestFixtureManagerScriptDispatch bounds an
	// intentionally endless manager.

	cwd, err := filepath.EvalSymlinks(artifacts.dir(t, "manager-cwd"))
	if err != nil {
		t.Fatal(err)
	}
	const rolePath, cribPath = "/state/runs/r/artifacts/roles/manager.md", "/state/runs/r/artifacts/worker-protocol.md"
	baseEnv := []string{
		"PATH=" + os.Getenv("PATH"),
		"HOP_STATE_DIR=" + stateDir,
		"HOP_RUN_ID=" + runID,
		"HOP_SESSION_ID=33333333-3333-3333-3333-333333333333",
		"HOP_INCARNATION_ID=44444444-4444-4444-4444-444444444444",
		"HOP_ROLE=manager",
		"FAKE_HOP_TASK_COUNTER_FILE=" + counterPath,
		"FAKE_HOP_LOG=" + logPath,
	}
	run := func(args ...string) string {
		ctx, cancel := context.WithTimeout(t.Context(), 1*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, principal, args...) //nolint:gosec // G204: fixed test-owned binary and arguments.
		cmd.Dir = cwd
		cmd.Env = baseEnv
		var out strings.Builder
		cmd.Stdout = &out
		cmd.Stderr = &out
		_ = cmd.Run() //nolint:errcheck // bounded by the context deadline; the manager never exits on its own (per-attempt retirement is what a real scenario proves terminates it).
		return out.String()
	}

	firstPrompt := testManagerInitialPrompt(assignmentPath, rolePath, cribPath, fakeHop)
	firstStdout := run("--session-id", "22222222-2222-2222-2222-222222222222", firstPrompt)
	if !strings.Contains(firstStdout, "FIXTURE-TASK-CREATED label=[t1]") || !strings.Contains(firstStdout, "FIXTURE-PLAN-CLOSED") {
		t.Fatalf("first (non-resumed) launch did not plan; stdout:\n%s", firstStdout)
	}
	logAfterFirst := readFakeHopLog(t, logPath)
	if got := countLogLinesWithPrefix(logAfterFirst, "task\tcreate"); got != 1 {
		t.Fatalf("task create count after the first launch = %d, want 1; log:\n%s", got, logAfterFirst)
	}
	if got := countLogLinesWithPrefix(logAfterFirst, "plan\tclose"); got != 1 {
		t.Fatalf("plan close count after the first launch = %d, want 1; log:\n%s", got, logAfterFirst)
	}

	const nativeRef = "88888888-8888-4888-8888-888888888888"
	continuationPrompt := testManagerContinuationPrompt(assignmentPath, rolePath, cribPath, fakeHop)
	secondStdout := run("--resume", nativeRef, continuationPrompt)
	if strings.Contains(secondStdout, "FIXTURE-TASK-CREATED") || strings.Contains(secondStdout, "FIXTURE-PLAN-CLOSED") {
		t.Errorf("resumed launch re-planned; stdout:\n%s", secondStdout)
	}
	if !strings.Contains(secondStdout, "FIXTURE-MANAGER-READY") {
		t.Errorf("resumed launch did not even report ready; stdout:\n%s", secondStdout)
	}
	logAfterSecond := readFakeHopLog(t, logPath)
	if got := countLogLinesWithPrefix(logAfterSecond, "task\tcreate"); got != 1 {
		t.Errorf("task create count after the resumed relaunch = %d, want still 1 (no re-plan); log:\n%s", got, logAfterSecond)
	}
	if got := countLogLinesWithPrefix(logAfterSecond, "plan\tclose"); got != 1 {
		t.Errorf("plan close count after the resumed relaunch = %d, want still 1 (no re-plan); log:\n%s", got, logAfterSecond)
	}
}

// preForwardBarrierAnswerBody is the fixed content of the scripted human
// answer TestFixtureManagerPreForwardBarrier's fixture delivers, reused
// both when writing the body file and when computing the barrier
// observation's expected digest.
const preForwardBarrierAnswerBody = "release the barrier\n"

// preForwardBarrierFixture is the shared setup for
// TestFixtureManagerPreForwardBarrier's enabled/disabled subtests: one
// worker-hold-shaped relay question, scripted exactly like
// TestFixtureManagerScriptDispatch's own hold/relay pair, and one scripted
// human answer to it — ready to drive the compiled manager principal
// directly (no herdr).
type preForwardBarrierFixture struct {
	principal, cwd, scratchDir, logPath string
	prompt                              string
	env                                 []string
	msgHoldQuestionID, msgHumanAnswerID string
}

func buildPreForwardBarrierFixture(t *testing.T, artifacts *artifactDir) preForwardBarrierFixture {
	t.Helper()
	principal := buildFixtureWorker(t, artifacts)
	fakeHop := buildFakeHopStub(t, artifacts)

	stateDir := artifacts.dir(t, "state")
	scratchDir := artifacts.dir(t, "manager-scratch")
	const runID = "b3b3b3b3-b3b3-4b3b-8b3b-b3b3b3b3b3b3"
	assignmentPath := filepath.Join(stateDir, "runs", runID, "artifacts", "assignment.md")
	if err := os.MkdirAll(filepath.Dir(assignmentPath), 0o700); err != nil {
		t.Fatal(err)
	}
	brief := fixtureManagerBrief(scratchDir,
		[]fixtureManagerTask{{Label: "t1", Title: "Implement t1", Behavior: "worker-implement"}},
		[]fixtureManagerAnswer{{Match: fixtureHoldMarker, Action: "relay"}},
		"")
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
	humanAnswerBody := writeBody("human-answer.txt", preForwardBarrierAnswerBody)

	const (
		msgHoldQuestionID = "77777777-7777-4777-8777-777777777777"
		msgHumanAnswerID  = "88888888-8888-4888-8888-888888888888"
		// msgRelayedQuestionID is the fixed id the fake hop stub's own "msg
		// send" always returns.
		msgRelayedQuestionID = "99999999-9999-4999-8999-999999999999"
	)
	blocks := []string{
		fakeMessageBlock(msgHoldQuestionID, "question", "worker-session-1", "", "", holdBody),
		fakeMessageBlock(msgHumanAnswerID, "answer", "human", msgRelayedQuestionID, msgHoldQuestionID, humanAnswerBody),
	}
	scriptPath, indexPath := writeFakeHopMsgScript(t, scriptDir, blocks)
	counterPath := newFakeHopTaskCounter(t, scriptDir)
	logPath := filepath.Join(scriptDir, "log.txt")

	cwd, err := filepath.EvalSymlinks(artifacts.dir(t, "manager-cwd"))
	if err != nil {
		t.Fatal(err)
	}
	const rolePath, cribPath = "/state/runs/r/artifacts/roles/manager.md", "/state/runs/r/artifacts/worker-protocol.md"
	prompt := testManagerInitialPrompt(assignmentPath, rolePath, cribPath, fakeHop)

	return preForwardBarrierFixture{
		principal: principal, cwd: cwd, scratchDir: scratchDir, logPath: logPath, prompt: prompt,
		env: []string{
			"PATH=" + os.Getenv("PATH"),
			"HOP_STATE_DIR=" + stateDir,
			"HOP_RUN_ID=" + runID,
			"HOP_SESSION_ID=aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa",
			"HOP_INCARNATION_ID=bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb",
			"HOP_ROLE=manager",
			"FAKE_HOP_MSG_SCRIPT=" + scriptPath,
			"FAKE_HOP_MSG_INDEX=" + indexPath,
			"FAKE_HOP_TASK_COUNTER_FILE=" + counterPath,
			"FAKE_HOP_LOG=" + logPath,
		},
		msgHoldQuestionID: msgHoldQuestionID, msgHumanAnswerID: msgHumanAnswerID,
	}
}

// runPreForwardBarrierManager runs fx's compiled manager principal to
// completion or the context deadline, whichever comes first — the manager
// never exits on its own, exactly like TestFixtureManagerScriptDispatch's
// own intentionally endless drive — and returns its captured stdout.
func runPreForwardBarrierManager(t *testing.T, fx preForwardBarrierFixture) string { //nolint:gocritic // hugeParam: preForwardBarrierFixture is a one-shot per-subtest fixture struct; a pointer would only complicate the two call sites.
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, fx.principal, "--session-id", "22222222-2222-2222-2222-222222222222", fx.prompt) //nolint:gosec // G204: fixed test-owned binary and arguments.
	cmd.Dir = fx.cwd
	cmd.Env = fx.env
	var out strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &out
	_ = cmd.Run() //nolint:errcheck // the context deadline ending this intentionally endless manager is the expected outcome, asserted on its captured stdout below.
	return out.String()
}

// TestFixtureManagerPreForwardBarrier proves RelayedQuestion's opt-in
// pre-forward barrier (design section 11 scenario 2) in isolation, for
// both the disabled path (every existing scenario's own shape: absent,
// the manager forwards a fetched human answer immediately, unaffected)
// and the enabled path (present, the manager's first incarnation blocks
// after fetching the answer and before forwarding it, dumping an
// observation naming what it fetched but never reaching the forward or
// the ack).
func TestFixtureManagerPreForwardBarrier(t *testing.T) {
	t.Run("Disabled", testFixtureManagerPreForwardBarrierDisabled)
	t.Run("Enabled", testFixtureManagerPreForwardBarrierEnabled)
}

func testFixtureManagerPreForwardBarrierDisabled(t *testing.T) {
	artifacts := newArtifactDir(t)
	fx := buildPreForwardBarrierFixture(t, artifacts)
	// No control file created: preForwardBarrierEnabled must report false,
	// and every existing scenario's own behavior (forward immediately)
	// must be unchanged.

	stdout := runPreForwardBarrierManager(t, fx)
	if !strings.Contains(stdout, "FIXTURE-FORWARDED origin=["+fx.msgHoldQuestionID+"]") {
		t.Errorf("manager stdout missing the forward with the barrier control file absent; got:\n%s", stdout)
	}
	if strings.Contains(stdout, "FIXTURE-PRE-FORWARD-BARRIER") {
		t.Errorf("manager stdout shows the pre-forward barrier firing with no control file present; got:\n%s", stdout)
	}
	if _, err := os.Stat(filepath.Join(fx.scratchDir, preForwardBarrierObservedFile)); err == nil {
		t.Error("pre-forward barrier observation file exists despite the barrier being disabled")
	}
	log := readFakeHopLog(t, fx.logPath)
	if !strings.Contains(log, "msg\tsend\t--kind\tanswer\t--reply-to\t"+fx.msgHoldQuestionID) {
		t.Errorf("fake hop invocation log missing the forward call; got:\n%s", log)
	}
}

func testFixtureManagerPreForwardBarrierEnabled(t *testing.T) {
	artifacts := newArtifactDir(t)
	fx := buildPreForwardBarrierFixture(t, artifacts)
	if err := os.WriteFile(filepath.Join(fx.scratchDir, preForwardBarrierControlFile), []byte("enable\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	stdout := runPreForwardBarrierManager(t, fx)
	if !strings.Contains(stdout, "FIXTURE-PRE-FORWARD-BARRIER answer=["+fx.msgHumanAnswerID+"]") {
		t.Fatalf("manager stdout missing the pre-forward barrier marker; got:\n%s", stdout)
	}
	if strings.Contains(stdout, "FIXTURE-FORWARDED") {
		t.Errorf("manager stdout shows a forward despite the barrier being enabled; got:\n%s", stdout)
	}
	log := readFakeHopLog(t, fx.logPath)
	if strings.Contains(log, "msg\tsend\t--kind\tanswer\t--reply-to\t"+fx.msgHoldQuestionID) {
		t.Errorf("fake hop invocation log shows the forward call despite the barrier being enabled; got:\n%s", log)
	}
	if strings.Contains(log, "msg\tack\t"+fx.msgHumanAnswerID) {
		t.Errorf("fake hop invocation log shows the answer acked despite the barrier blocking before it; got:\n%s", log)
	}

	obsContent, err := os.ReadFile(filepath.Join(fx.scratchDir, preForwardBarrierObservedFile))
	if err != nil {
		t.Fatalf("read pre-forward barrier observation file: %v", err)
	}
	wantSum := sha256.Sum256([]byte(preForwardBarrierAnswerBody))
	wantHex := hex.EncodeToString(wantSum[:])
	if !strings.Contains(string(obsContent), "id="+fx.msgHumanAnswerID+"\n") {
		t.Errorf("barrier observation missing id=%s; got:\n%s", fx.msgHumanAnswerID, obsContent)
	}
	if !strings.Contains(string(obsContent), "sha256="+wantHex+"\n") {
		t.Errorf("barrier observation missing sha256=%s; got:\n%s", wantHex, obsContent)
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

// runVerdictCorrelationCase drives the compiled fixture principal as a
// manager-feature manager against noticeBodyPaths' info notices, in
// order, under the fake hop stub's scripted hop-status output
// (statusEnv), and reports how many "fix from reject" task-create
// invocations it made, plus the manager's own stdout and the fake hop
// invocation log for further assertions (e.g. the evidence-inconsistent
// human-escalation question). The manager never exits on its own; a
// context deadline (matching TestFixtureManagerScriptDispatch's own
// pattern) bounds it once the scripted notices are exhausted.
func runVerdictCorrelationCase(t *testing.T, artifacts *artifactDir, noticeBodyPaths, statusEnv []string) (fixTaskCreates int, stdout, log string) {
	t.Helper()
	fx := buildManagerScriptDispatchFixture(t, artifacts)

	scriptDir := filepath.Dir(fx.counterPath)
	var blocks []string
	for i, body := range noticeBodyPaths {
		blocks = append(blocks, fakeMessageBlock(fmt.Sprintf("cccccccc-0000-4000-8000-%012d", i), "info", "controller", "", "", body))
	}
	scriptPath, indexPath := writeFakeHopMsgScript(t, scriptDir, blocks)

	const rolePath, cribPath = "/state/runs/a1/artifacts/roles/manager.md", "/state/runs/a1/artifacts/worker-protocol.md"
	prompt := testManagerInitialPrompt(fx.assignmentPath, rolePath, cribPath, fx.fakeHop)

	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, fx.principal, "--session-id", "22222222-2222-2222-2222-222222222222", prompt) //nolint:gosec // G204: fixed test-owned binary and arguments.
	cmd.Dir = fx.cwd
	cmd.Env = append([]string{
		"PATH=" + os.Getenv("PATH"),
		"HOP_STATE_DIR=" + fx.stateDir,
		"HOP_RUN_ID=" + fx.runID,
		"HOP_SESSION_ID=33333333-3333-3333-3333-333333333333",
		"HOP_INCARNATION_ID=44444444-4444-4444-4444-444444444444",
		"HOP_ROLE=manager",
		"FAKE_HOP_MSG_SCRIPT=" + scriptPath,
		"FAKE_HOP_MSG_INDEX=" + indexPath,
		"FAKE_HOP_TASK_COUNTER_FILE=" + fx.counterPath,
		"FAKE_HOP_LOG=" + fx.logPath,
	}, statusEnv...)
	var out strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &out
	_ = cmd.Run() //nolint:errcheck // the context deadline killing this intentionally endless manager is the expected outcome once its scripted notices are exhausted, asserted on its captured stdout/log below, never on this error.

	log = readFakeHopLog(t, fx.logPath)
	return countLogLinesWithPrefix(log, "task\tcreate\t--title\tfix from reject\t"), out.String(), log
}

// TestFixtureManagerVerdictRejectedCorrelation proves the manager's own
// STATUS-1 verdict-channel correlation rule in isolation
// (fixtureworker_test.go's verdictRejectionMatchingNotice/
// parseVerdictRejectedLines, per internal/app/templates.go's
// renderManagerAssignment): a verdict-rejected shortfall plans a fix task
// only when its reasons path equals the fetched notice's own body path; a
// different reasons path plans none; a repeated notice for a review
// already acted on never plans a SECOND fix; and an evidence-inconsistent
// shortfall is never treated as a rejection.
func TestFixtureManagerVerdictRejectedCorrelation(t *testing.T) {
	t.Run("matching path gives one fix", func(t *testing.T) {
		artifacts := newArtifactDir(t)
		noticePath := filepath.Join(artifacts.dir(t, "notice-bodies"), "reject-notice.txt")
		if err := os.WriteFile(noticePath, []byte("fixture reviewer reasons: reject\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		fixes, stdout, _ := runVerdictCorrelationCase(t, artifacts, []string{noticePath}, []string{
			"FAKE_HOP_STATUS_SHORTFALL=verdict-rejected",
			"FAKE_HOP_STATUS_REVIEW=aaaaaaaa-1111-4aaa-8aaa-aaaaaaaaaaaa",
			"FAKE_HOP_STATUS_SUBJECT=cccccccccccccccccccccccccccccccccccccccc",
			"FAKE_HOP_STATUS_REASONS=" + noticePath,
		})
		if fixes != 1 {
			t.Errorf("fix task creates = %d, want exactly 1 for a matching reasons path; stdout:\n%s", fixes, stdout)
		}
		if !strings.Contains(stdout, "FIXTURE-STATUS-CHECKED matched=[true]") {
			t.Errorf("manager stdout does not report a matched shortfall; got:\n%s", stdout)
		}
	})

	t.Run("different path gives none", func(t *testing.T) {
		artifacts := newArtifactDir(t)
		bodyDir := artifacts.dir(t, "notice-bodies")
		noticePath := filepath.Join(bodyDir, "reject-notice.txt")
		if err := os.WriteFile(noticePath, []byte("fixture reviewer reasons: reject\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		otherReasonsPath := filepath.Join(bodyDir, "a-different-reviews-reasons.txt")
		fixes, stdout, _ := runVerdictCorrelationCase(t, artifacts, []string{noticePath}, []string{
			"FAKE_HOP_STATUS_SHORTFALL=verdict-rejected",
			"FAKE_HOP_STATUS_REVIEW=bbbbbbbb-2222-4bbb-8bbb-bbbbbbbbbbbb",
			"FAKE_HOP_STATUS_SUBJECT=cccccccccccccccccccccccccccccccccccccccc",
			"FAKE_HOP_STATUS_REASONS=" + otherReasonsPath,
		})
		if fixes != 0 {
			t.Errorf("fix task creates = %d, want 0 when the shortfall's reasons path differs from this notice's own body path; stdout:\n%s", fixes, stdout)
		}
		if !strings.Contains(stdout, "FIXTURE-STATUS-CHECKED matched=[false]") {
			t.Errorf("manager stdout does not report an unmatched shortfall; got:\n%s", stdout)
		}
	})

	t.Run("repeated notice gives no second fix", func(t *testing.T) {
		artifacts := newArtifactDir(t)
		noticePath := filepath.Join(artifacts.dir(t, "notice-bodies"), "reject-notice.txt")
		if err := os.WriteFile(noticePath, []byte("fixture reviewer reasons: reject\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		// The SAME review's rejection notice, delivered twice (a re-served,
		// unacknowledged notice re-fetched after a fresh hop msg wait) —
		// both messages carry the identical body path, and hop status keeps
		// reporting the same verdict-rejected shortfall for as long as no
		// fix has integrated. Only the FIRST delivery may plan a fix.
		fixes, stdout, _ := runVerdictCorrelationCase(t, artifacts, []string{noticePath, noticePath}, []string{
			"FAKE_HOP_STATUS_SHORTFALL=verdict-rejected",
			"FAKE_HOP_STATUS_REVIEW=aaaaaaaa-1111-4aaa-8aaa-aaaaaaaaaaaa",
			"FAKE_HOP_STATUS_SUBJECT=cccccccccccccccccccccccccccccccccccccccc",
			"FAKE_HOP_STATUS_REASONS=" + noticePath,
		})
		if fixes != 1 {
			t.Errorf("fix task creates = %d, want exactly 1 across two deliveries of the same review's notice (never a second fix from the same review); stdout:\n%s", fixes, stdout)
		}
		if got := strings.Count(stdout, "FIXTURE-STATUS-CHECKED matched=[true]"); got != 2 {
			t.Errorf("matched status checks = %d, want 2 (both deliveries correlate to the same shortfall; only fix-planning is deduplicated); stdout:\n%s", got, stdout)
		}
	})

	t.Run("evidence-inconsistent gives none", func(t *testing.T) {
		artifacts := newArtifactDir(t)
		noticePath := filepath.Join(artifacts.dir(t, "notice-bodies"), "reject-notice.txt")
		if err := os.WriteFile(noticePath, []byte("fixture reviewer reasons: reject\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		fixes, stdout, log := runVerdictCorrelationCase(t, artifacts, []string{noticePath}, []string{
			"FAKE_HOP_STATUS_SHORTFALL=evidence-inconsistent",
		})
		if fixes != 0 {
			t.Errorf("fix task creates = %d, want 0 for an evidence-inconsistent shortfall (never a rejection); stdout:\n%s", fixes, stdout)
		}
		if !strings.Contains(stdout, "FIXTURE-STATUS-CHECKED matched=[false] evidence-inconsistent=[true]") {
			t.Errorf("manager stdout does not report the evidence-inconsistent shortfall; got:\n%s", stdout)
		}
		// The verdict-channel instruction's remaining rule: ask the human to
		// inspect the run.
		if !strings.Contains(stdout, "FIXTURE-EVIDENCE-INCONSISTENT-ESCALATED result=[sent") {
			t.Errorf("manager stdout does not report escalating to the human; got:\n%s", stdout)
		}
		if !strings.Contains(log, "msg\tsend\t--to\thuman\t--kind\tquestion\t--body\t"+fixtureEvidenceInconsistentQuestionBody) {
			t.Errorf("fake hop invocation log missing the human-escalation question send; got:\n%s", log)
		}
	})
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
	const msgAnswerID = "aaaaaaaa-2222-4aaa-8222-222222222222"
	blocks := []string{fakeMessageBlock(msgAnswerID, "answer", "manager-session", "99999999-9999-4999-8999-999999999999", "", answerBody)}
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
		"HOP_SESSION_ID=99999999-8888-4777-8666-555555555555",
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
		"FIXTURE-HOLD-RELEASED answer=[" + msgAnswerID + "]",
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
	if !strings.Contains(log, "msg\tack\t"+msgAnswerID) {
		t.Errorf("fake hop invocation log missing the release answer's ack; got:\n%s", log)
	}
	if !strings.Contains(log, "result\tsubmit\t") {
		t.Errorf("fake hop invocation log missing the post-release result submit; got:\n%s", log)
	}

	head := repo.git(t, "rev-parse", "HEAD^{commit}")
	if head == repo.Base {
		t.Error("worker-hold did not commit a change before submitting")
	}

	// Byte-exact-delivery evidence (design section 11's injection
	// scenario): the worker's OWN observation dump carries a digest of
	// its OWN read of the task instructions file, at the path it
	// resolved itself -- never a copy this test handed it.
	instructionsContent := "FIXTURE-BEHAVIOR: worker-hold " + scratchDir + "\n"
	workerObs := readWorkerObservation(t, filepath.Join(scratchDir, "worker-observed-"+attemptID+".txt"))
	if workerObs.Fields["instructions_path"] != instructionsPath {
		t.Errorf("worker observation instructions_path = %q, want %q", workerObs.Fields["instructions_path"], instructionsPath)
	}
	if want := sha256HexOf(instructionsContent); workerObs.Fields["instructions_sha256"] != want {
		t.Errorf("worker observation instructions_sha256 = %q, want %q (sha256 of the exact instructions content)", workerObs.Fields["instructions_sha256"], want)
	}
	if want := fmt.Sprintf("%d", len(instructionsContent)); workerObs.Fields["instructions_bytes"] != want {
		t.Errorf("worker observation instructions_bytes = %q, want %q", workerObs.Fields["instructions_bytes"], want)
	}

	// Same evidence for the answer channel: the worker's own read of the
	// body file hop msg wait itself named for the barrier-release
	// answer, dumped as a digest (no other dump captures a fetched
	// message's content).
	answerObs := readWorkerObservation(t, filepath.Join(scratchDir, "answer-observed-"+attemptID+".txt"))
	if answerObs.Fields["id"] != msgAnswerID {
		t.Errorf("answer observation id = %q, want %q", answerObs.Fields["id"], msgAnswerID)
	}
	answerContent := "released\n"
	if want := sha256HexOf(answerContent); answerObs.Fields["sha256"] != want {
		t.Errorf("answer observation sha256 = %q, want %q (sha256 of the exact answer content)", answerObs.Fields["sha256"], want)
	}
	if want := fmt.Sprintf("%d", len(answerContent)); answerObs.Fields["bytes"] != want {
		t.Errorf("answer observation bytes = %q, want %q", answerObs.Fields["bytes"], want)
	}
}

// TestFixtureWorkerFetchCrash proves DuplicateAndAmbiguousDelivery's
// worker-fetch-crash behavior (design section 11 scenario 3) in
// isolation, for both incarnations: the first fetches one message, dumps
// an observation naming it, then blocks until self-killed, never acking;
// a resumed incarnation re-fetches (the same message, re-served) and
// waits for a SEPARATE, test-controlled release gate before acking,
// draining and submitting — the deterministic ordering mechanism a
// real-process scenario needs to issue a stale-incarnation late-ack
// assertion with a guaranteed happens-before relationship against this
// session's own ack.
func TestFixtureWorkerFetchCrash(t *testing.T) {
	t.Run("BlocksBeforeAck", testFixtureWorkerFetchCrashBlocksBeforeAck)
	t.Run("ResumedWaitsForReleaseThenAcks", testFixtureWorkerFetchCrashResumedWaitsForReleaseThenAcks)
}

func testFixtureWorkerFetchCrashBlocksBeforeAck(t *testing.T) {
	artifacts := newArtifactDir(t)
	worker := buildFixtureWorker(t, artifacts)
	fakeHop := buildFakeHopStub(t, artifacts)
	repo := newFixtureRepo(t, artifacts, nil, "fetch-crash-repo")

	stateDir := artifacts.dir(t, "state")
	scratchDir := artifacts.dir(t, "fetch-crash-scratch")
	const (
		runID     = "c4c4c4c4-c4c4-4c4c-8c4c-c4c4c4c4c4c4"
		taskID    = "d5d5d5d5-d5d5-4d5d-8d5d-d5d5d5d5d5d5"
		attemptID = "e6e6e6e6-e6e6-4e6e-8e6e-e6e6e6e6e6e6"
		sessionID = "f7f7f7f7-f7f7-4f7f-8f7f-f7f7f7f7f7f7"
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
	if err := os.WriteFile(instructionsPath, []byte("FIXTURE-BEHAVIOR: worker-fetch-crash "+scratchDir+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	scriptDir := artifacts.dir(t, "fetch-crash-script")
	msgBodyPath := filepath.Join(scriptDir, "m1-body.txt")
	const msgBody = "manager info for t1\n"
	if err := os.WriteFile(msgBodyPath, []byte(msgBody), 0o600); err != nil {
		t.Fatal(err)
	}
	const msgID = "a8a8a8a8-a8a8-4a8a-8a8a-a8a8a8a8a8a8"
	blocks := []string{fakeMessageBlock(msgID, "info", "manager-session", "", "", msgBodyPath)}
	scriptPath, indexPath := writeFakeHopMsgScript(t, scriptDir, blocks)
	logPath := filepath.Join(scriptDir, "log.txt")

	prompt := testAssignmentPrompt(assignmentPath, fakeHop)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, worker, "--session-id", sessionID, prompt) //nolint:gosec // G204: fixed test-owned binary and arguments.
	cmd.Dir = repo.Root
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOP_STATE_DIR=" + stateDir,
		"HOP_RUN_ID=" + runID,
		"HOP_TASK_ID=" + taskID,
		"HOP_ATTEMPT_ID=" + attemptID,
		"HOP_SESSION_ID=" + sessionID,
		"HOP_INCARNATION_ID=b9b9b9b9-b9b9-4b9b-8b9b-b9b9b9b9b9b9",
		"HOP_ROLE=implementer",
		"FAKE_HOP_MSG_SCRIPT=" + scriptPath,
		"FAKE_HOP_MSG_INDEX=" + indexPath,
		"FAKE_HOP_LOG=" + logPath,
	}
	var out syncOutput
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatalf("start fixture worker: %v", err)
	}
	var (
		waitOnce sync.Once
		waitErr  error
	)
	wait := func() error {
		waitOnce.Do(func() { waitErr = cmd.Wait() })
		return waitErr
	}
	t.Cleanup(func() {
		if err := cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
			t.Logf("cleanup: kill fixture worker: %v", err)
		}
		if err := wait(); err != nil {
			t.Logf("cleanup: reap fixture worker: %v", err)
		}
	})

	wantObserved := "FIXTURE-FETCH-CRASH-OBSERVED id=[" + msgID + "] resumed=[false]"
	if !waitUntil(func() bool { return strings.Contains(out.snapshot(), wantObserved) }) {
		t.Fatalf("worker never reported observing the fetched message before blocking; output so far:\n%s", out.snapshot())
	}
	if strings.Contains(out.snapshot(), "FIXTURE-FETCH-CRASH-ACKED") {
		t.Fatalf("worker acked before being self-killed; output:\n%s", out.snapshot())
	}

	controlPath := filepath.Join(scratchDir, "self-kill-"+attemptID)
	tmp := controlPath + ".tmp"
	if err := os.WriteFile(tmp, []byte("FIXTURE-SELF-KILL\n"), 0o600); err != nil {
		t.Fatalf("write self-kill control file: %v", err)
	}
	if err := os.Rename(tmp, controlPath); err != nil {
		t.Fatalf("rename self-kill control file into place: %v", err)
	}
	if err := wait(); err == nil {
		t.Fatalf("worker exited 0 after the self-kill control file was written; want it SIGKILLed; output:\n%s", out.snapshot())
	}

	log := readFakeHopLog(t, logPath)
	if strings.Contains(log, "msg\tack\t"+msgID) {
		t.Errorf("fake hop invocation log shows the message acked despite the process being killed before it ever could; got:\n%s", log)
	}

	obsContent, err := os.ReadFile(filepath.Join(scratchDir, "fetch-crash-observed-"+attemptID+".txt")) //nolint:gosec // G304: a path this test constructed itself.
	if err != nil {
		t.Fatalf("read fetch-crash observation file: %v", err)
	}
	wantSum := sha256.Sum256([]byte(msgBody))
	wantHex := hex.EncodeToString(wantSum[:])
	if !strings.Contains(string(obsContent), "id="+msgID+"\n") {
		t.Errorf("fetch-crash observation missing id=%s; got:\n%s", msgID, obsContent)
	}
	if !strings.Contains(string(obsContent), "sha256="+wantHex+"\n") {
		t.Errorf("fetch-crash observation missing sha256=%s; got:\n%s", wantHex, obsContent)
	}
}

func testFixtureWorkerFetchCrashResumedWaitsForReleaseThenAcks(t *testing.T) {
	artifacts := newArtifactDir(t)
	worker := buildFixtureWorker(t, artifacts)
	fakeHop := buildFakeHopStub(t, artifacts)
	repo := newFixtureRepo(t, artifacts, nil, "fetch-crash-resumed-repo")

	stateDir := artifacts.dir(t, "state")
	scratchDir := artifacts.dir(t, "fetch-crash-resumed-scratch")
	const (
		runID     = "1a1a1a1a-1a1a-4a1a-8a1a-1a1a1a1a1a1a"
		taskID    = "2b2b2b2b-2b2b-4b2b-8b2b-2b2b2b2b2b2b"
		attemptID = "3c3c3c3c-3c3c-4c3c-8c3c-3c3c3c3c3c3c"
		sessionID = "4d4d4d4d-4d4d-4d4d-8d4d-4d4d4d4d4d4d"
		nativeRef = "5e5e5e5e-5e5e-4e5e-8e5e-5e5e5e5e5e5e"
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
	if err := os.WriteFile(instructionsPath, []byte("FIXTURE-BEHAVIOR: worker-fetch-crash "+scratchDir+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	scriptDir := artifacts.dir(t, "fetch-crash-resumed-script")
	msgBodyPath := filepath.Join(scriptDir, "m1-body.txt")
	const msgBody = "manager info for t1, re-served\n"
	if err := os.WriteFile(msgBodyPath, []byte(msgBody), 0o600); err != nil {
		t.Fatal(err)
	}
	const msgID = "6f6f6f6f-6f6f-4f6f-8f6f-6f6f6f6f6f6f"
	blocks := []string{fakeMessageBlock(msgID, "info", "manager-session", "", "", msgBodyPath)}
	scriptPath, indexPath := writeFakeHopMsgScript(t, scriptDir, blocks)
	logPath := filepath.Join(scriptDir, "log.txt")

	continuationPrompt := testContinuationPrompt(assignmentPath, fakeHop)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, worker, "--resume", nativeRef, continuationPrompt) //nolint:gosec // G204: fixed test-owned binary and arguments.
	cmd.Dir = repo.Root
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOP_STATE_DIR=" + stateDir,
		"HOP_RUN_ID=" + runID,
		"HOP_TASK_ID=" + taskID,
		"HOP_ATTEMPT_ID=" + attemptID,
		"HOP_SESSION_ID=" + sessionID,
		"HOP_INCARNATION_ID=7a7a7a7a-7a7a-4a7a-8a7a-7a7a7a7a7a7a",
		"HOP_ROLE=implementer",
		"FAKE_HOP_MSG_SCRIPT=" + scriptPath,
		"FAKE_HOP_MSG_INDEX=" + indexPath,
		"FAKE_HOP_LOG=" + logPath,
	}
	var out syncOutput
	cmd.Stdout, cmd.Stderr = &out, &out
	// idle() (reached after this incarnation submits) reads a clean-exit
	// line from stdin; ready from the start since it is only ever read
	// once the process reaches that loop.
	cmd.Stdin = strings.NewReader("FIXTURE-QUIT\n")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start fixture worker: %v", err)
	}
	var (
		waitOnce sync.Once
		waitErr  error
	)
	wait := func() error {
		waitOnce.Do(func() { waitErr = cmd.Wait() })
		return waitErr
	}
	t.Cleanup(func() {
		if err := cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
			t.Logf("cleanup: kill fixture worker: %v", err)
		}
		if err := wait(); err != nil {
			t.Logf("cleanup: reap fixture worker: %v", err)
		}
	})

	wantObserved := "FIXTURE-FETCH-CRASH-OBSERVED id=[" + msgID + "] resumed=[true]"
	if !waitUntil(func() bool { return strings.Contains(out.snapshot(), wantObserved) }) {
		t.Fatalf("resumed worker never reported observing the re-served message; output so far:\n%s", out.snapshot())
	}
	if strings.Contains(out.snapshot(), "FIXTURE-FETCH-CRASH-ACKED") {
		t.Fatalf("resumed worker acked before the release gate was created; output:\n%s", out.snapshot())
	}
	if log := readFakeHopLog(t, logPath); strings.Contains(log, "msg\tack\t"+msgID) {
		t.Fatalf("fake hop invocation log shows the message acked before the release gate was created; got:\n%s", log)
	}

	releasePath := filepath.Join(scratchDir, "fetch-crash-release-"+attemptID)
	tmp := releasePath + ".tmp"
	if err := os.WriteFile(tmp, []byte("FIXTURE-RELEASE\n"), 0o600); err != nil {
		t.Fatalf("write release control file: %v", err)
	}
	if err := os.Rename(tmp, releasePath); err != nil {
		t.Fatalf("rename release control file into place: %v", err)
	}

	wantAcked := "FIXTURE-FETCH-CRASH-ACKED id=[" + msgID + "]"
	if !waitUntil(func() bool { return strings.Contains(out.snapshot(), wantAcked) }) {
		t.Fatalf("resumed worker never acked after the release gate was created; output so far:\n%s", out.snapshot())
	}

	// P2-6's second, separate gate: after the drain (this fixture has only
	// the one scripted message, so the drain is a no-op msg-next-until-
	// none call) the worker pauses again, before its own submit, until
	// this SEPARATE presubmit-release control file appears.
	wantPresubmitHold := "FIXTURE-FETCH-CRASH-PRESUBMIT-HOLD attempt=[" + attemptID + "]"
	if !waitUntil(func() bool { return strings.Contains(out.snapshot(), wantPresubmitHold) }) {
		t.Fatalf("resumed worker never reached its presubmit hold after acking; output so far:\n%s", out.snapshot())
	}
	if strings.Contains(out.snapshot(), "FIXTURE-WORKER-IDLE") {
		t.Fatalf("resumed worker reached its post-submit idle loop before the presubmit release gate was created; output:\n%s", out.snapshot())
	}

	presubmitReleasePath := filepath.Join(scratchDir, "fetch-crash-presubmit-release-"+attemptID)
	presubmitTmp := presubmitReleasePath + ".tmp"
	if err := os.WriteFile(presubmitTmp, []byte("FIXTURE-RELEASE\n"), 0o600); err != nil {
		t.Fatalf("write presubmit release control file: %v", err)
	}
	if err := os.Rename(presubmitTmp, presubmitReleasePath); err != nil {
		t.Fatalf("rename presubmit release control file into place: %v", err)
	}

	if !waitUntil(func() bool { return strings.Contains(out.snapshot(), "FIXTURE-WORKER-IDLE") }) {
		t.Fatalf("resumed worker never reached its post-submit idle loop; output so far:\n%s", out.snapshot())
	}
	if err := wait(); err != nil {
		t.Fatalf("resumed worker did not exit cleanly after FIXTURE-QUIT: %v; output:\n%s", err, out.snapshot())
	}

	log := readFakeHopLog(t, logPath)
	if !strings.Contains(log, "msg\tack\t"+msgID) {
		t.Errorf("fake hop invocation log missing the ack after the release gate; got:\n%s", log)
	}
	if !strings.Contains(log, "result\tsubmit\t") {
		t.Errorf("fake hop invocation log missing the post-ack result submit; got:\n%s", log)
	}
}

// TestFixtureWorkerHoldMissingBodyFailsLoudly proves the negative side of
// the read-before-ack fix (Astra review pass 1, P2 "unread bodies"): a
// delivered barrier-release answer whose body file does not exist must
// never be acked — the worker must fail loudly (readFileOrFatal's own
// fatalf) instead of silently releasing on an envelope it never actually
// read.
func TestFixtureWorkerHoldMissingBodyFailsLoudly(t *testing.T) {
	artifacts := newArtifactDir(t)
	worker := buildFixtureWorker(t, artifacts)
	fakeHop := buildFakeHopStub(t, artifacts)
	repo := newFixtureRepo(t, artifacts, nil, "repo")

	stateDir := artifacts.dir(t, "state")
	scratchDir := artifacts.dir(t, "worker-hold-missing-body-scratch")
	const (
		runID     = "12121212-1212-4121-8121-121212121212"
		taskID    = "13131313-1313-4131-8131-131313131313"
		attemptID = "14141414-1414-4141-8141-141414141414"
		sessionID = "15151515-1515-4151-8151-151515151515"
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

	scriptDir := artifacts.dir(t, "worker-hold-missing-body-script")
	// Deliberately never created: the answer envelope names a body path
	// this fixture must attempt to read and fail on, never silently skip.
	missingBody := filepath.Join(scriptDir, "does-not-exist.txt")
	const msgAnswerID = "bbbbbbbb-3333-4bbb-8333-333333333333"
	blocks := []string{fakeMessageBlock(msgAnswerID, "answer", "manager-session", "99999999-9999-4999-8999-999999999999", "", missingBody)}
	scriptPath, indexPath := writeFakeHopMsgScript(t, scriptDir, blocks)
	logPath := filepath.Join(scriptDir, "log.txt")

	prompt := testAssignmentPrompt(assignmentPath, fakeHop)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, worker, "--session-id", sessionID, prompt) //nolint:gosec // G204: fixed test-owned binary and arguments.
	cmd.Dir = repo.Root
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOP_STATE_DIR=" + stateDir,
		"HOP_RUN_ID=" + runID,
		"HOP_TASK_ID=" + taskID,
		"HOP_ATTEMPT_ID=" + attemptID,
		"HOP_SESSION_ID=" + sessionID,
		"HOP_INCARNATION_ID=16161616-1616-4161-8161-161616161616",
		"HOP_ROLE=implementer",
		"FAKE_HOP_MSG_SCRIPT=" + scriptPath,
		"FAKE_HOP_MSG_INDEX=" + indexPath,
		"FAKE_HOP_LOG=" + logPath,
	}
	var out strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &out
	cmd.Stdin = strings.NewReader("FIXTURE-QUIT\n")
	runErr := cmd.Run()
	if runErr == nil {
		t.Fatalf("worker exited 0 despite an unreadable delivered body; want a fatal exit; output:\n%s", out.String())
	}

	output := out.String()
	if !strings.Contains(output, "fixture principal: read "+missingBody) {
		t.Errorf("worker output does not name the read failure; got:\n%s", output)
	}
	if strings.Contains(output, "FIXTURE-HOLD-RELEASED") {
		t.Errorf("worker reported the barrier released despite never reading the answer's body; got:\n%s", output)
	}

	log := readFakeHopLog(t, logPath)
	if strings.Contains(log, "msg\tack\t"+msgAnswerID) {
		t.Errorf("fixture acked a message whose body it never successfully read; log:\n%s", log)
	}
}

// TestFixtureWorkerDrainsOnUndeliveredResultTransient proves
// submitOnce's own mailbox-drain retry (Astra review pass 1, P2):
// previously it only slept and resubmitted on ANY transient line, so a
// message the section 5 mailbox rule requires draining first would sit
// undrained on every identical retry until the two-minute budget
// expired. Drives the SOLO submit-valid behavior deliberately — it never
// drains before its own first submit call (unlike worker-implement/
// worker-hold), so the scripted pending message survives untouched until
// submitOnce's OWN post-transient drain is what has to find it, proving
// the drain fires from the retry path itself, not some earlier one.
func TestFixtureWorkerDrainsOnUndeliveredResultTransient(t *testing.T) {
	artifacts := newArtifactDir(t)
	worker := buildFixtureWorker(t, artifacts)
	fakeHop := buildFakeHopStub(t, artifacts)
	repo := newFixtureRepo(t, artifacts, nil, "repo")

	stateDir := artifacts.dir(t, "state")
	const runID = "17171717-1717-4171-8171-171717171717"
	runDir := filepath.Join(stateDir, "runs", runID, "artifacts")
	if err := os.MkdirAll(runDir, 0o700); err != nil {
		t.Fatal(err)
	}
	assignmentPath := filepath.Join(runDir, "assignment.md")
	brief := fixtureWorkerBrief("submit-valid")
	if err := os.WriteFile(assignmentPath, []byte("# HOP Assignment\n\n## Brief\n\n"+brief+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	scriptDir := artifacts.dir(t, "drain-script")
	pendingBody := filepath.Join(scriptDir, "pending-info.txt")
	if err := os.WriteFile(pendingBody, []byte("pending notice\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	const msgPendingID = "cccccccc-4444-4ccc-8444-444444444444"
	blocks := []string{fakeMessageBlock(msgPendingID, "info", "manager-session", "", "", pendingBody)}
	scriptPath, indexPath := writeFakeHopMsgScript(t, scriptDir, blocks)
	logPath := filepath.Join(scriptDir, "log.txt")
	undeliveredCounterPath := filepath.Join(scriptDir, "undelivered-counter.txt")

	prompt := testAssignmentPrompt(assignmentPath, fakeHop)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	const sessionID = "22222222-2222-4222-8222-222222222222"
	cmd := exec.CommandContext(ctx, worker, "--session-id", sessionID, prompt) //nolint:gosec // G204: fixed test-owned binary and arguments.
	cmd.Dir = repo.Root
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOP_STATE_DIR=" + stateDir,
		"HOP_RUN_ID=" + runID,
		"HOP_TASK_ID=23232323-2323-4232-8232-232323232323",
		"HOP_ATTEMPT_ID=24242424-2424-4242-8242-242424242424",
		"HOP_SESSION_ID=" + sessionID,
		"HOP_INCARNATION_ID=25252525-2525-4252-8252-252525252525",
		"FAKE_HOP_MSG_SCRIPT=" + scriptPath,
		"FAKE_HOP_MSG_INDEX=" + indexPath,
		"FAKE_HOP_LOG=" + logPath,
		"FAKE_HOP_RESULT_SUBMIT_UNDELIVERED_COUNT=1",
		"FAKE_HOP_RESULT_SUBMIT_UNDELIVERED_COUNTER_FILE=" + undeliveredCounterPath,
	}
	var out strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &out
	cmd.Stdin = strings.NewReader("FIXTURE-QUIT\n")
	if err := cmd.Run(); err != nil {
		t.Fatalf("run fixture worker: %v\nstdout/stderr:\n%s", err, out.String())
	}

	stdout := out.String()
	if !strings.Contains(stdout, "FIXTURE-SUBMIT-RESULT exit=[1] first-line=[transient: undelivered messages") {
		t.Errorf("worker stdout missing the first (transient) submit attempt; got:\n%s", stdout)
	}
	if !strings.Contains(stdout, "FIXTURE-SUBMIT-RESULT exit=[0] first-line=[accepted") {
		t.Errorf("worker stdout missing the second (accepted) submit attempt; got:\n%s", stdout)
	}

	log := readFakeHopLog(t, logPath)
	if !strings.Contains(log, "msg\tack\t"+msgPendingID) {
		t.Errorf("fixture never drained (acked) the pending message between submit attempts; log:\n%s", log)
	}
	firstSubmit := strings.Index(log, "result\tsubmit\t")
	ackIdx := strings.Index(log, "msg\tack\t"+msgPendingID)
	secondSubmit := strings.LastIndex(log, "result\tsubmit\t")
	if firstSubmit < 0 || ackIdx < 0 || firstSubmit >= ackIdx || ackIdx >= secondSubmit {
		t.Errorf("the drain did not happen strictly between the two submit attempts; log:\n%s", log)
	}
}

// fakeHopValidEnv returns a legitimate message-verb environment (a
// UUID-shaped caller context, absolute HOP_STATE_DIR), with overrides
// applied on top — a table case sets exactly the one field it means to
// break, so a failure can only be attributed to that field.
func fakeHopValidEnv(t *testing.T, artifacts *artifactDir, overrides map[string]string) []string {
	t.Helper()
	fields := map[string]string{
		"PATH":               os.Getenv("PATH"),
		"HOP_STATE_DIR":      artifacts.dir(t, "fake-hop-contract-state"),
		"HOP_RUN_ID":         "10101010-1010-4101-8101-101010101010",
		"HOP_SESSION_ID":     "20202020-2020-4202-8202-202020202020",
		"HOP_INCARNATION_ID": "30303030-3030-4303-8303-303030303030",
	}
	for k, v := range overrides {
		fields[k] = v
	}
	env := make([]string, 0, len(fields))
	for k, v := range fields {
		env = append(env, k+"="+v)
	}
	return env
}

// TestFakeHopRejectsInvalidInvocations proves the fake hop stub's
// argv/context contract (Astra pass 2 finding F5): every invocation below
// is exactly the shape the real binary refuses before any store logic
// ever runs (identity.ParseMessageID's canonical-lowercase-UUID contract
// on ack/reply-to/relay-of, parseMessageKind's question/info/answer set,
// parseAddress's manager/human/task:<uuid> set, SendMessage's non-empty-
// body rule, flag.Duration's own parse failure on a nonsense --timeout,
// and requireWorkerStateRoot's absolute-path/present rule) — so this fake
// must fail on all of them too, exit 2 (a usage error, never a scripted
// "refused:" business outcome), rather than silently accepting a shape no
// real hop invocation ever could.
func TestFakeHopRejectsInvalidInvocations(t *testing.T) {
	artifacts := newArtifactDir(t)
	fakeHop := buildFakeHopStub(t, artifacts)

	const validMessageID = "40404040-4040-4404-8404-404040404040"
	tests := []struct {
		name      string
		args      []string
		overrides map[string]string
	}{
		{"msg ack: message id is not UUID-shaped", []string{"msg", "ack", "not-a-uuid"}, nil},
		{"msg send: reply-to is not UUID-shaped", []string{"msg", "send", "--kind", "answer", "--reply-to", "not-a-uuid", "--body", "x"}, nil},
		{"msg send: relay-of is not UUID-shaped", []string{"msg", "send", "--kind", "question", "--to", "human", "--relay-of", "not-a-uuid", "--body", "x"}, nil},
		{"msg send: unsupported kind", []string{"msg", "send", "--kind", "bogus", "--to", "manager", "--body", "x"}, nil},
		{"msg send: unrecognized address", []string{"msg", "send", "--kind", "question", "--to", "somewhere", "--body", "x"}, nil},
		{"msg send: task address is not UUID-shaped", []string{"msg", "send", "--kind", "info", "--to", "task:not-a-uuid", "--body", "x"}, nil},
		{"msg send: empty inline body", []string{"msg", "send", "--kind", "question", "--to", "manager", "--body", ""}, nil},
		{"msg wait: nonsense --timeout", []string{"msg", "wait", "--timeout", "not-a-duration"}, nil},
		{"message verb: relative HOP_STATE_DIR", []string{"msg", "ack", validMessageID}, map[string]string{"HOP_STATE_DIR": "relative/path"}},
		{"message verb: missing HOP_STATE_DIR", []string{"msg", "ack", validMessageID}, map[string]string{"HOP_STATE_DIR": ""}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, fakeHop, tc.args...) //nolint:gosec // G204: fixed test-owned binary; args are this table's own fixed literals.
			cmd.Env = fakeHopValidEnv(t, artifacts, tc.overrides)
			out, err := cmd.CombinedOutput()
			if err == nil {
				t.Fatalf("invocation succeeded, want a usage failure; output:\n%s", out)
			}
			exitErr, ok := err.(*exec.ExitError) //nolint:errorlint // a direct type assertion suffices for this test's own exec of a single known binary.
			if !ok {
				t.Fatalf("run error = %v (%T), want *exec.ExitError", err, err)
			}
			if exitErr.ExitCode() != 2 {
				t.Errorf("exit code = %d, want 2 (a usage error, not a scripted refusal); output:\n%s", exitErr.ExitCode(), out)
			}
		})
	}
}
