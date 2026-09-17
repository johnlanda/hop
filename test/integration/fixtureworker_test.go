package integration

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// The fixture worker generalizes into the fixture PRINCIPAL (design section
// 11): one deterministic Go program installed under the recognized name
// "claude" so hop launch's PATH resolution (section 6) finds it, dispatching
// on its session's role (HOP_ROLE, absent for a solo run) into one of three
// entry points — runWorker (solo and feature-mode implementer, which share
// the Phase 2 prompt/result-submission shapes byte for byte), runManager
// (manager-feature: creates a scripted plan through the real hop task/plan
// verbs, then spends its remaining lifetime in the section 7 idle-poll
// loop), and runReviewer (reviewer-approve / reviewer-reject-once). Every
// role reads the HOP_* environment and locates its own assignment artifact
// exactly the way section 6 delivers it, validates its delivered content,
// and (worker/reviewer) submits through the real `hop result submit` /
// `hop review submit` at the hop path its own prompt argv names. A worker's
// or the manager's behavior is scriptable through a
// "FIXTURE-BEHAVIOR: <name> [args...]" directive line delivered through the
// existing brief -> assignment.md channel (solo and the manager) or the
// manager-authored task-instructions artifact (a feature-mode implementer,
// an independently computable path — see runWorker); a reviewer's behavior
// comes from the run's frozen reviewer role artifact instead, since a review
// task's assignment carries no manager-authored free text at all.
// fixtureWorkerBrief renders the simple one-line directive every worker
// behavior and an unscripted manager use; fixtureManagerBrief renders the
// manager-feature script DSL (TASK/ANSWER/FIX lines) parseManagerScript
// parses.
//
// Locating a solo/manager assignment file: usecase_run.go and
// templates.go's renderManagerAssignment agree on
// filepath.Join(StateRoot, "runs", <run-uuid>, "artifacts", "assignment.md")
// — a stable convention independent of the prompt's exact wording. A
// feature-mode implementer's or reviewer's assignment is instead
// per-attempt: filepath.Join(StateRoot, "runs", <run-uuid>, "attempts",
// <attempt-uuid>, "assignment.md") (usecase_sessionlaunch.go's
// attemptAssignmentPath) — every role computes its own path independently
// from HOP_* env rather than parsing it out of the prompt, then
// cross-validates against the prompt's own marker (below), so a mismatch or
// an absent marker fails the run loudly rather than silently preferring one
// source.
//
// The hop path has no such independent source for any role: it reaches
// this principal only through the prompt argv, by design. Every role's
// initial/continuation prompt is rendered by internal/app (worker/
// implementer: usecase_execboundary.go's renderInitialPrompt/
// renderContinuationPrompt; reviewer and manager:
// usecase_sessionlaunch.go's renderReviewerInitialPrompt/
// renderReviewerContinuationPrompt and renderManagerInitialPrompt/
// renderManagerContinuationPrompt, byte-for-byte mirrored by
// roleprompts_test.go and pinned there against internal/app's own golden
// literals) with a fixed pair of markers surrounding the assignment path
// and the hop path; extractFromPrompt (embedded source, promptMarkers)
// applies the role/shape-appropriate pair — workerPromptMarkers,
// reviewerPromptMarkers or managerPromptMarkers — retyped here from those
// same pinned renderers, so a drift in either place fails a test before
// the two can desynchronize silently. A resume-shaped invocation
// (`--resume <native-ref> <continuation prompt>`) is identical across
// every role (internal/app's composeSessionArgvTail composes the same
// four-element tail regardless of role) and is detected and strictly
// validated (requireResumeShape) before any element is ever treated as a
// first-launch prompt, exactly as the original solo worker already did.
const fixtureWorkerSource = `package main

import (
	"bufio"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// reexecMarkerEnv, once present in the environment, means this process is
// the re-exec target of the exec-keep-pid behavior: skip straight to idling
// so the second incarnation cannot re-exec forever.
const reexecMarkerEnv = "HOP_FIXTURE_REEXEC_DONE"

// mcpStandInArg selects this binary's MCP-stand-in child mode (argv[1]).
const mcpStandInArg = "fixture-mcp-stand-in"

// mcpStandInPID is the spawned stand-in child's pid, recorded in the
// observation dump so a scenario can locate it in the pane's foreground
// group listing; mcpStandInStdin holds the pipe write end open for this
// process's whole lifetime, so the child's stdin reaches EOF — and the
// child exits — exactly when this process exits or execs (Go pipe fds are
// close-on-exec).
var (
	mcpStandInPID   int
	mcpStandInStdin io.WriteCloser
)

// fixtureRetryInterval is this principal's own back-off between hop result
// submit / hop review submit retries, mandated by the section 7 transient
// protocol the launch prompt itself states ("wait briefly and run the exact
// same command again") — behavior this fixture reproduces, never a
// test-side wait.
const fixtureRetryInterval = 200 * time.Millisecond

// fixtureHoldMarker is worker-hold's barrier question body: a fixed,
// recognizable token the manager script's ANSWER table matches on to decide
// whether to relay it to a human (a test-controlled release) or answer it
// directly.
const fixtureHoldMarker = "FIXTURE-HOLD-BARRIER"

// fixtureAckGenericBody is the manager's default direct-answer body for a
// question that matches no scripted ANSWER rule.
const fixtureAckGenericBody = "acknowledged"

func main() {
	if len(os.Args) > 1 && os.Args[1] == mcpStandInArg {
		mcpStandIn()
		return
	}
	// Reproduce the pinned real-harness process-group shape BEFORE anything
	// about this principal is observable as ready: Claude Code 2.1.270
	// spawns its configured MCP servers as children in its own process
	// group immediately after the trust check, so every settlement, stop
	// and adoption decision in production runs against a multi-member
	// foreground group whose raw listing order gives this principal no
	// particular index — for every role, not only a worker.
	spawnMCPStandIn()

	if os.Getenv(reexecMarkerEnv) != "" {
		fmt.Printf("FIXTURE-REEXECED pid=[%d]\n", os.Getpid())
		idle()
		return
	}

	// HOP_ROLE is present for every feature-mode session (manager,
	// implementer, reviewer) and absent for a solo run's pane. An
	// implementer shares the solo worker's prompt shapes and
	// behavior-directive channel byte for byte, so both dispatch through
	// runWorker; only the per-role assignment-path formula and behavior
	// source differ inside it.
	switch os.Getenv("HOP_ROLE") {
	case "manager":
		runManager()
	case "reviewer":
		runReviewer()
	default:
		runWorker()
	}
}

// spawnMCPStandIn starts one long-lived child in this process's OWN
// process group (plain fork/exec inheritance; no setpgid), standing in for
// the MCP servers a real Claude Code 2.1.270 launch spawns before the
// worker does anything observable. The child is this same binary in
// stand-in mode: its argv carries no HOP marker, its argv[0] mirrors this
// process's own argv[0] (in a wrapper topology that is the spoofed
// invocation name, keeping the pane's non-claimed members uniformly
// claude-named), its stdio is detached except a stdin pipe whose write
// end this process holds forever — so the child exits on EOF exactly when
// this process exits or execs, and it is never reaped here (it cannot
// outlive this process long enough to matter, and must never block a
// direct test run's cmd.Wait through inherited pipes).
func spawnMCPStandIn() {
	self, err := os.Executable()
	if err != nil {
		fatalf("resolve own executable for the mcp stand-in: %v", err)
	}
	cmd := exec.Command(self, mcpStandInArg)
	if len(os.Args) > 0 && os.Args[0] != "" {
		cmd.Args = []string{os.Args[0], mcpStandInArg}
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		fatalf("open the mcp stand-in's stdin pipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		fatalf("start the mcp stand-in: %v", err)
	}
	mcpStandInPID = cmd.Process.Pid
	mcpStandInStdin = stdin
}

// mcpStandIn is the stand-in child's own loop: block until stdin (held
// open by the parent) reaches EOF, then exit.
func mcpStandIn() {
	_, _ = io.Copy(io.Discard, os.Stdin)
}

// selfKillPollInterval paces watchForSelfKill's poll for the test's
// self-kill control file.
const selfKillPollInterval = 100 * time.Millisecond

// watchForSelfKill polls for controlPath — a test-owned file under the
// run's scratch directory, never HOP_STATE_DIR, never pane input — and
// SIGKILLs this process's OWN pid (os.Getpid()) the instant it appears.
// This is the only safe way a test ends a launched principal's process
// from outside: a test may OBSERVE this process's pid (via
// pane.process_info), but signaling an externally observed pid directly
// races the OS's own pid-reuse window between observation and signal —
// Herdr could reap this process and the kernel could recycle its pid
// before the test's own signal call executes, killing an unrelated
// process instead (Astra review finding P1). Asking the verified process
// to kill itself closes that window: no other process is ever named by
// the pid the test's signal ultimately targets. Runs forever in its own
// goroutine; the process exits from this or on its own, whichever is
// first.
func watchForSelfKill(controlPath string) {
	for {
		if _, err := os.Stat(controlPath); err == nil {
			_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
			return
		}
		time.Sleep(selfKillPollInterval)
	}
}

// waitForControlFile blocks until controlPath exists, polling at the same
// low frequency watchForSelfKill uses, for a control file whose own
// purpose is not a kill -- worker-fetch-crash's resumed-incarnation
// release gate (design section 11 scenario 3), letting a test finish its
// own stale-ack assertion deterministically before this session's own ack
// call ever lands.
func waitForControlFile(controlPath string) {
	for {
		if _, err := os.Stat(controlPath); err == nil {
			return
		}
		time.Sleep(selfKillPollInterval)
	}
}

// isResumeInvocation reports whether argv is a resume-SHAPED invocation:
// any element equals the exact "--resume". Detection only — it routes the
// invocation to requireResumeShape's strict validation, so a malformed
// resume argv can never fall through to first-launch prompt parsing, and
// the trailing continuation prompt every HOP relaunch carries never
// disturbs it. Shared by every role: composeSessionArgvTail composes the
// identical [--resume <ref> <continuation prompt>] tail regardless of role.
func isResumeInvocation(argv []string) bool {
	for _, a := range argv {
		if a == "--resume" {
			return true
		}
	}
	return false
}

// requireResumeShape validates HOP's one supported cold-relaunch argv,
// exactly [<exe> --resume <native-ref> <continuation prompt>]
// (internal/app's composeHarnessArgvTail / composeSessionArgvTail, design
// section 6 item 3): four elements, "--resume" at index 1 immediately
// followed by a UUID-shaped native reference, the continuation prompt as
// the final element. The reference check matters because the real CLI's
// "--resume [value]" takes an OPTIONAL value: without it, an argv like
// [worker --resume <prompt>] — no reference at all — would be consumed by
// the real CLI as a resume value, so this fixture must reject it rather
// than parse the prompt and proceed. A missing reference or prompt, an
// intervening extra argument and a wrong ordering all fail the run
// loudly. It returns the continuation prompt.
func requireResumeShape(argv []string) string {
	if len(argv) != 4 || argv[1] != "--resume" {
		fatalf("resume-shaped argv %q is not the supported [<exe> --resume <native-ref> <continuation prompt>] shape", argv)
	}
	if !isUUID(argv[2]) {
		fatalf("resume argv %q does not carry a UUID-shaped native reference immediately after --resume; the real CLI would consume the next argument as --resume's optional value", argv)
	}
	return argv[3]
}

// isUUID reports whether s has the 8-4-4-4-12 lowercase-hex UUID shape
// HOP mints for native session references (design section 6: lowercase
// hex and hyphens).
func isUUID(s string) bool {
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

func requireEnv(keys ...string) map[string]string {
	values := make(map[string]string, len(keys))
	for _, key := range keys {
		value := os.Getenv(key)
		if value == "" {
			fatalf("required %s is not set", key)
		}
		values[key] = value
	}
	return values
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "fixture principal: "+format+"\n", args...)
	os.Exit(1)
}

// extractMarked returns the text between the first occurrence of start and
// the following occurrence of end, or "" if either is absent.
func extractMarked(s, start, end string) string {
	i := strings.Index(s, start)
	if i < 0 {
		return ""
	}
	i += len(start)
	j := strings.Index(s[i:], end)
	if j < 0 {
		return ""
	}
	return s[i : i+j]
}

// parseBehavior finds the "FIXTURE-BEHAVIOR: <name> [args...]" directive
// line in content and returns the behavior name and its extra arguments.
// Content carrying no directive returns an empty behavior. Shared by every
// role: a worker/implementer's assignment (solo) or task instructions
// (feature), a reviewer's frozen role artifact, and a manager's brief all
// carry this same directive line as their first line.
func parseBehavior(content string) (behavior string, args []string) {
	const prefix = "FIXTURE-BEHAVIOR: "
	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, prefix) {
			continue
		}
		fields := strings.Fields(strings.TrimPrefix(trimmed, prefix))
		if len(fields) == 0 {
			return "", nil
		}
		return fields[0], fields[1:]
	}
	return "", nil
}

// writeObservation dumps everything this principal observed to path,
// atomically (write-temp-then-rename), so the test can read it back after
// the fact rather than trusting this process's own judgment of correctness.
// The dump carries the principal's COMPLETE inherited environment
// (os.Environ()), not just the required HOP_* subset, so a scenario can
// assert the full sanitized-exec contract directly: every strip-matrix and
// policy-strip variable absent even when seeded into the server's own
// environment, HERDR_*/HOP_* present, and passthrough/profile entries
// exactly as configured (docs/plan/phase-2-design.md section 6).
func writeObservation(path, role, assignmentPath, promptAssignmentPath, hopPath, assignmentContent, behavior string, behaviorArgs []string, instructionsPath, instructionsContent string) {
	var b strings.Builder
	fmt.Fprintf(&b, "pid=%d\n", os.Getpid())
	// os.Executable() is this process's own resolved image path — the
	// absolute path hop launch's PATH-based harness resolution actually
	// exec'd. A scenario asserts this equals the fixture-installed stub
	// path exactly, never a real claude binary elsewhere on the machine.
	if executable, execErr := os.Executable(); execErr == nil {
		fmt.Fprintf(&b, "executable=%s\n", executable)
	} else {
		fmt.Fprintf(&b, "executable_error=%v\n", execErr)
	}
	fmt.Fprintf(&b, "mcp_stand_in_pid=%d\n", mcpStandInPID)
	fmt.Fprintf(&b, "role=%s\n", role)
	fmt.Fprintf(&b, "assignment_path=%s\n", assignmentPath)
	fmt.Fprintf(&b, "prompt_assignment_path=%s\n", promptAssignmentPath)
	fmt.Fprintf(&b, "hop_path=%s\n", hopPath)
	fmt.Fprintf(&b, "behavior=%s\n", behavior)
	fmt.Fprintf(&b, "behavior_args=%s\n", strings.Join(behaviorArgs, " "))
	environ := os.Environ()
	sort.Strings(environ)
	for _, entry := range environ {
		fmt.Fprintf(&b, "env:%s\n", entry)
	}
	fmt.Fprintf(&b, "assignment_content_begin\n%s\nassignment_content_end\n", assignmentContent)
	// A feature-mode implementer's own read of its manager-authored task
	// instructions, at the path it resolved independently: the digest
	// and length prove THIS PROCESS'S OWN os.ReadFile call observed
	// exactly these bytes at that exact path — never a copy the test
	// handed it — the byte-exact-delivery evidence design section 11's
	// injection scenario needs for this channel specifically, since a
	// feature-mode implementer's OWN per-attempt assignment.md (dumped
	// above) only names the instructions file by path and never embeds
	// its content.
	if instructionsPath != "" {
		sum := sha256.Sum256([]byte(instructionsContent))
		fmt.Fprintf(&b, "instructions_path=%s\n", instructionsPath)
		fmt.Fprintf(&b, "instructions_sha256=%s\n", hex.EncodeToString(sum[:]))
		fmt.Fprintf(&b, "instructions_bytes=%d\n", len(instructionsContent))
	}
	atomicWriteFile(path, b.String())
}

// atomicWriteFile writes content to path, temp-file-then-rename, so a
// reader (the test, or this same principal recovering after a relaunch)
// never observes a partial write.
func atomicWriteFile(path, content string) {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(content), 0o600); err != nil {
		fatalf("write %s: %v", path, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		fatalf("rename %s into place: %v", path, err)
	}
}

// writeContentDigest dumps this process's own digest and length of
// content — already read from a path this process resolved or was
// handed by hop msg next/wait, never re-derived — as byte-exact-delivery
// evidence for a channel writeObservation's own fixed shape does not
// cover (a fetched message body: every behavior that reads one
// otherwise discards it once acked). id names the entity the digest
// belongs to (a message id), so a scenario can cross-check it against
// the store's own row without ambiguity.
func writeContentDigest(path, id, content string) {
	var b strings.Builder
	sum := sha256.Sum256([]byte(content))
	fmt.Fprintf(&b, "id=%s\n", id)
	fmt.Fprintf(&b, "sha256=%s\n", hex.EncodeToString(sum[:]))
	fmt.Fprintf(&b, "bytes=%d\n", len(content))
	atomicWriteFile(path, b.String())
}

// readFileOrFatal reads path, failing the run loudly on any error — every
// path this fixture reads is either independently computed from HOP_* env
// (a HOP-owned artifact convention) or extracted from a pinned prompt/role
// marker, so a read failure is always a defect worth stopping for.
func readFileOrFatal(path string) string {
	content, err := os.ReadFile(path) //nolint:gosec // G304: a path this fixture computed itself from its own environment or a pinned marker.
	if err != nil {
		fatalf("read %s: %v", path, err)
	}
	return string(content)
}

// commitChange writes a small, unique change to the working tree (its own
// cwd, which hop launch's exec leaves at the launched worktree, per
// WorkerPaneRequest's Cwd) and commits it, using the fixture repository's
// own local git identity (initFixtureRepo, in the Go test process) — nothing
// here depends on HOME or any environment variable surviving the sanitized
// exec boundary. It returns the new commit's object id.
func commitChange(message string) string {
	path := fmt.Sprintf("fixture-worker-change-%d.txt", time.Now().UnixNano())
	if err := os.WriteFile(path, []byte(message+"\n"), 0o644); err != nil {
		fatalf("write change: %v", err)
	}
	runGit("add", "-A")
	runGit("commit", "-m", message)
	return strings.TrimSpace(runGit("rev-parse", "HEAD^{commit}"))
}

func runGit(args ...string) string {
	cmd := exec.Command("git", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

// hopResult captures one bounded hop CLI invocation's outcome.
type hopResult struct {
	Stdout   string
	ExitCode int
}

// FirstLine returns Stdout's first line without its terminator.
func (r hopResult) FirstLine() string {
	line, _, _ := strings.Cut(r.Stdout, "\n")
	return line
}

// runHopCLI runs "<hopPath> <args...>", inheriting this process's own
// environment (the sanitized launch environment hop launch composed —
// exactly as the existing solo worker's submitOnce already relied on Go's
// nil-Env-means-inherit default) and blocking until it exits, exactly as
// every real worker-plumbing verb invocation is a short, bounded store
// round trip. It never fails the run itself on a non-zero exit — refusals
// are data the caller decides how to act on, exactly like a real harness
// would read them; only a failure to even start or run the process is
// fatal.
func runHopCLI(hopPath string, args ...string) hopResult {
	cmd := exec.Command(hopPath, args...)
	var out strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &out
	runErr := cmd.Run()
	code := 0
	if runErr != nil {
		if exitErr, ok := runErr.(*exec.ExitError); ok { //nolint:errorlint // a direct type assertion suffices for this fixture's own exec of a single known binary.
			code = exitErr.ExitCode()
		} else {
			fatalf("run %s %s: %v", hopPath, strings.Join(args, " "), runErr)
		}
	}
	return hopResult{Stdout: out.String(), ExitCode: code}
}

// newRequestID mints a fresh lowercase-hex UUIDv4-shaped request id for one
// logical mutating-verb call (design section 7: "the templates and
// fixtures ALWAYS pass it"), reused UNCHANGED across every retry of that
// SAME call — never regenerated per attempt — so a transient retry is
// idempotent rather than minting a fresh, unrelated request each time.
func newRequestID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		fatalf("generate request id: %v", err)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// deterministicUUIDFrom derives a UUID-shaped, DETERMINISTIC string from
// seed -- never random, unlike newRequestID above -- so recomputing it from
// the SAME seed always yields the identical value. The manager's forward of
// a relayed human answer (handleManagerMessage's "answer" case) uses this,
// seeded with the answer's own message id (CLI-returned data the manager
// never persists), so a cold-relaunched successor recomputes the IDENTICAL
// request id with no hidden state of its own -- design trace 2: "the
// forward's request ID makes any redo idempotent". Not a cryptographic
// UUIDv5 (no real namespace semantics needed here), just a stable,
// collision-resistant hash rendered in UUID form for readability.
func deterministicUUIDFrom(seed string) string {
	sum := sha256.Sum256([]byte("hop-fixture-manager-forward-request-id:" + seed))
	var b [16]byte
	copy(b[:], sum[:16])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// runHopCLIRetryable runs hopPath with args, retrying while the response's
// first line begins with "transient:" — Phase 2's own hop result submit
// retry convention (submitOnce), generalized here to every OTHER mutating
// verb a validated manager/worker principal calls (design section 7/8): a
// run still launching or resuming legitimately refuses a manager/message
// verb with a retryable transient line rather than a hard refusal
// (defect LAUNCH-1). It NEVER retries a "refused:" line — only a literal
// "transient:" prefix is retryable — and reuses the identical args
// (therefore the same --request-id, when the caller included one)
// unchanged on every attempt, so a retry is idempotent. Bounded exactly
// like submitOnce/reviewSubmitOnce so a persistently broken run does not
// hang forever.
func runHopCLIRetryable(hopPath string, args ...string) hopResult {
	deadline := time.Now().Add(2 * time.Minute)
	for {
		res := runHopCLI(hopPath, args...)
		if strings.HasPrefix(res.FirstLine(), "transient:") && time.Now().Before(deadline) {
			time.Sleep(fixtureRetryInterval)
			continue
		}
		return res
	}
}

// submitOnce runs "<hopPath> result submit --summary <summary> --commit
// <oid>", retrying while the first stdout line begins with "transient" (the
// assignment template's own retry instruction), bounded so a persistent
// rejection does not hang the worker forever.
func submitOnce(hopPath, oid, summary string) {
	// The retry budget must comfortably outlast the controller's own
	// corroboration polling: launch claim settlement depends on real pane
	// creation, InspectPane polling and (in the early-submission case) this
	// very transient response as a wakeup, none of which are instant under
	// test. The assignment template's own instruction has no fixed
	// deadline ("wait briefly and run the exact same command again"); this
	// bounds it only so a persistently broken run does not hang forever.
	deadline := time.Now().Add(2 * time.Minute)
	for {
		res := runHopCLI(hopPath, "result", "submit", "--summary", summary, "--commit", oid)
		fmt.Printf("FIXTURE-SUBMIT-RESULT exit=[%d] first-line=[%s]\n", res.ExitCode, res.FirstLine())
		if !strings.HasPrefix(res.FirstLine(), "transient") || !time.Now().Before(deadline) {
			return
		}
		// Feature mode's own mailbox-drain transient (design section 5:
		// "transient: undelivered messages; drain with hop msg next, ack,
		// then resubmit") names a SPECIFIC required action, distinct from
		// solo's "attempt not yet running" retry: the identical resubmit
		// would see the SAME pending message forever without this drain.
		// Solo never renders this line, so this never fires for it.
		if strings.HasPrefix(res.FirstLine(), "transient: undelivered messages") {
			drainMailbox(hopPath)
		}
		time.Sleep(fixtureRetryInterval)
	}
}

// drainMailbox drains this session's task-address mailbox with hop msg
// next until "none: no queued message", acknowledging every delivered
// message regardless of kind — the section 5 mailbox rule's worker-side
// half ("before hop result submit it must drain its queue"), applied
// uniformly before every submit/verdict attempt so a message left over
// from an earlier attempt of the same task can never block acceptance.
// Every ack is the recipient's statement of receipt-AND-READ (design
// section 7): this reads each message's own body file successfully
// before acking it, even when the content is otherwise ignored, and
// fails loudly rather than acking a message whose body it never
// actually read.
func drainMailbox(hopPath string) {
	for {
		res := runHopCLI(hopPath, "msg", "next")
		msg, ok := parseDeliveredMessage(res.Stdout)
		if !ok {
			return
		}
		readFileOrFatal(msg.BodyPath)
		ackAndRequireSuccess(hopPath, msg.ID)
	}
}

// ackAndRequireSuccess runs hop msg ack and fails the run loudly unless
// the response is accepted/duplicate — an ack that read back the body
// successfully but was itself refused must never be silently ignored
// (design section 7: an ack is the recipient's own statement of
// receipt-and-read).
func ackAndRequireSuccess(hopPath, messageID string) {
	res := runHopCLI(hopPath, "msg", "ack", messageID)
	first := res.FirstLine()
	if !strings.HasPrefix(first, "acknowledged ") && !strings.HasPrefix(first, "duplicate ") {
		fatalf("hop msg ack %s failed: %s", messageID, first)
	}
}

// deliveredMessage is one hop msg next/wait response, parsed from its
// fixed three-line shape (design section 7's grammar,
// internal/app/grammar.go's GrammarMessageLine/GrammarBodyLine).
type deliveredMessage struct {
	ID, Kind, From, ReplyTo, RelayOf, Origin string
	BodyPath                                 string
}

// parseDeliveredMessage parses one hop msg next/wait stdout, returning
// ok=false for the empty-queue/timeout line ("none: ..."). It never
// assumes field order or presence beyond kind/from, which the grammar
// renders unconditionally.
func parseDeliveredMessage(stdout string) (deliveredMessage, bool) {
	lines := strings.Split(stdout, "\n")
	if len(lines) == 0 || !strings.HasPrefix(lines[0], "message ") {
		return deliveredMessage{}, false
	}
	fields := strings.Fields(lines[0])
	if len(fields) < 2 {
		fatalf("malformed delivered-message first line %q", lines[0])
	}
	msg := deliveredMessage{ID: fields[1]}
	for _, field := range fields[2:] {
		key, value, ok := strings.Cut(field, "=")
		if !ok {
			continue
		}
		switch key {
		case "kind":
			msg.Kind = value
		case "from":
			msg.From = value
		case "reply-to":
			msg.ReplyTo = value
		case "relay-of":
			msg.RelayOf = value
		case "origin":
			msg.Origin = value
		}
	}
	for _, line := range lines[1:] {
		if path, ok := strings.CutPrefix(line, "body: "); ok {
			msg.BodyPath = path
			break
		}
	}
	if msg.BodyPath == "" {
		fatalf("delivered message %s carries no body: line", msg.ID)
	}
	return msg, true
}

// waitForGo blocks on stdin for a line reading exactly "FIXTURE-GO",
// letting a direct (non-pane) test drive this process's timing precisely
// (e.g. drive a relaunch that retires this incarnation before releasing a
// submit-stale worker). Stdin reaching EOF before that line is treated as
// an immediate go-ahead, so a test that does not care about precise timing
// can just close stdin. Real pane scenarios never write to a launched
// principal's stdin (that would be typed pane input, forbidden by design);
// they control timing through the messaging barrier (fixtureHoldMarker)
// instead, so this remains a direct-invocation-only mechanism, exactly as
// it always was.
func waitForGo() {
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		if strings.TrimSpace(scanner.Text()) == "FIXTURE-GO" {
			return
		}
	}
}

// reexecSelf replaces this process image via execve while keeping its pid,
// proving the exec-keep-pid capability the design's claim-settlement
// predicate depends on (section 6: "execve preserves it"). The child
// carries reexecMarkerEnv so it idles immediately instead of looping.
func reexecSelf() {
	self, err := os.Executable()
	if err != nil {
		fatalf("resolve own executable for re-exec: %v", err)
	}
	env := append(os.Environ(), reexecMarkerEnv+"=1")
	if err := syscall.Exec(self, []string{self}, env); err != nil {
		fatalf("exec self: %v", err)
	}
}

// idle keeps this process alive as the pane's foreground process (success
// and failure are both observed only through the result/verdict protocol
// and check, never through pane exit — section 5) until told to stop:
// FIXTURE-QUIT exits 0, FIXTURE-QUIT-FAIL exits 3, and stdin EOF exits 0
// like a process that was simply closed. Every role idles the same way
// after acting: the composer-like survivor per-attempt retirement (section
// 6) must actually terminate, never a self-exit standing in for it.
func idle() {
	fmt.Println("FIXTURE-WORKER-IDLE")
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		switch strings.TrimSpace(scanner.Text()) {
		case "FIXTURE-QUIT":
			return
		case "FIXTURE-QUIT-FAIL":
			os.Exit(3)
		}
	}
}

// promptMarkers names one role/shape's fixed extraction markers: the
// substrings immediately surrounding the assignment path and the hop path
// in that role's pinned prompt (internal/app/usecase_execboundary.go's
// renderInitialPrompt/renderContinuationPrompt for a solo/implementer
// worker, usecase_sessionlaunch.go's renderReviewerInitialPrompt/
// renderReviewerContinuationPrompt and renderManagerInitialPrompt/
// renderManagerContinuationPrompt — test/integration/roleprompts_test.go
// and fixtureworker_test.go's testAssignmentPrompt/testContinuationPrompt
// mirror and pin the exact same literals on the test side).
type promptMarkers struct {
	assignmentStart, assignmentEnd string
	hopStart, hopEnd               string
}

// workerPromptMarkers are the solo/implementer worker's markers: identical
// for both roles (design section 6 — "worker and implementer share the
// Phase 2 prompt shapes byte for byte").
var workerPromptMarkers = map[bool]promptMarkers{
	false: {"Read your assignment at ", " and complete it.", "submit it by running: ", " result submit --summary"},
	true:  {"Re-read your assignment at ", " and continue it.", "submit it by running: ", " result submit --summary"},
}

// reviewerPromptMarkers are the reviewer's markers.
var reviewerPromptMarkers = map[bool]promptMarkers{
	false: {"Read your review assignment at ", " and evaluate the frozen subject it names.", "submit your verdict by running: ", " review submit --verdict"},
	true:  {"Re-read your review assignment at ", " and continue it.", "submit your verdict by running: ", " review submit --verdict"},
}

// managerPromptMarkers are the manager's markers: the hop-path marker
// text is identical on both shapes ("poll for messages by running: " is
// lowercased differently only by sentence position — see the capital-P
// continuation variant below); the assignment marker's end text is shared
// too.
var managerPromptMarkers = map[bool]promptMarkers{
	false: {"Read your assignment at ", ", your role instructions at", "poll for messages by running: ", " msg wait."},
	true:  {"Re-read your assignment at ", ", your role instructions at", "Poll for messages by running: ", " msg wait."},
}

// extractFromPrompt applies markers to prompt, failing the run loudly if
// either path is absent — a malformed launch must never silently proceed
// on a guessed value.
func extractFromPrompt(markers promptMarkers, prompt string) (assignmentPath, hopPath string) {
	assignmentPath = extractMarked(prompt, markers.assignmentStart, markers.assignmentEnd)
	hopPath = extractMarked(prompt, markers.hopStart, markers.hopEnd)
	if assignmentPath == "" {
		fatalf("prompt does not carry the %q marker naming the assignment path", markers.assignmentStart)
	}
	if hopPath == "" {
		fatalf("prompt does not carry the %q marker naming the hop path", markers.hopStart)
	}
	return assignmentPath, hopPath
}

// resolvePrompt extracts the trailing prompt from argv per HOP's two
// invocation shapes, detecting and strictly validating a resume-shaped
// argv before ever treating any element as a first-launch prompt.
func resolvePrompt(argv []string) (prompt string, resumed bool) {
	if isResumeInvocation(argv) {
		return requireResumeShape(argv), true
	}
	if len(argv) > 0 {
		return argv[len(argv)-1], false
	}
	return "", false
}

// runWorker is the solo-worker and feature-implementer entry point: they
// share the Phase 2 prompt shapes and the result-submission protocol byte
// for byte, differing only in the assignment-path formula (solo: the
// run's own artifacts/assignment.md; implementer: this attempt's own
// attempts/<id>/assignment.md, computed exactly as
// internal/app/usecase_sessionlaunch.go's attemptAssignmentPath does) and
// in where the FIXTURE-BEHAVIOR directive lives (solo: inlined in the
// brief, so it is already part of assignment.md's own content; feature:
// the manager-authored task instructions file, an independently
// computable path this worker never needs to extract from anything —
// filepath.Join(<state>, "runs", <run>, "tasks", <task>+".md"), exactly
// internal/app/usecase_plan.go's taskInstructionsPath).
func runWorker() {
	env := requireEnv("HOP_STATE_DIR", "HOP_RUN_ID", "HOP_TASK_ID", "HOP_ATTEMPT_ID", "HOP_INCARNATION_ID")
	role := os.Getenv("HOP_ROLE") // "" for solo, "implementer" for feature mode.
	feature := role == "implementer"

	var assignmentPath string
	if feature {
		assignmentPath = filepath.Join(env["HOP_STATE_DIR"], "runs", env["HOP_RUN_ID"], "attempts", env["HOP_ATTEMPT_ID"], "assignment.md")
	} else {
		assignmentPath = filepath.Join(env["HOP_STATE_DIR"], "runs", env["HOP_RUN_ID"], "artifacts", "assignment.md")
	}

	prompt, resumed := resolvePrompt(os.Args)
	promptAssignmentPath, hopPath := extractFromPrompt(workerPromptMarkers[resumed], prompt)
	if promptAssignmentPath != assignmentPath {
		fatalf("prompt's assignment path (%s) does not match the independently computed path (%s)", promptAssignmentPath, assignmentPath)
	}

	assignmentContent := readFileOrFatal(assignmentPath)
	behavior, behaviorArgs := parseBehavior(assignmentContent)
	// instructionsPath/instructionsContent stay "" for a solo run: only a
	// feature-mode implementer has a separate manager-authored
	// instructions artifact distinct from its own per-attempt
	// assignment.md, and only its OWN read of that exact path (never a
	// copy the test handed it) is byte-exact-delivery evidence.
	var instructionsPath, instructionsContent string
	if feature {
		// The manager-authored task instructions carry the directive for a
		// feature-mode implementer; assignment.md itself is fully computed
		// (no manager-authored free text), so parseBehavior would find
		// nothing there.
		instructionsPath = filepath.Join(env["HOP_STATE_DIR"], "runs", env["HOP_RUN_ID"], "tasks", env["HOP_TASK_ID"]+".md")
		instructionsContent = readFileOrFatal(instructionsPath)
		behavior, behaviorArgs = parseBehavior(instructionsContent)
	}

	// Every Phase 3 behavior's directive carries a test-owned scratch
	// directory as its first argument: HOP_STATE_DIR's tree is HOP's own,
	// never a place this fixture writes into, so its observation dump and
	// any message body it authors (worker-hold's barrier question) land
	// there instead. Phase 2 behaviors are unchanged: they keep writing
	// beside the assignment file, exactly as they always have.
	scratchDir := requireScratchDir(behavior, behaviorArgs)
	observationPath := filepath.Join(filepath.Dir(assignmentPath), "worker-observed.txt")
	if scratchDir != "" {
		observationPath = filepath.Join(scratchDir, "worker-observed-"+env["HOP_ATTEMPT_ID"]+".txt")
		// A real-process scenario that ends this attempt mid-flight (design
		// section 11 scenario 4) must never signal a pid it only OBSERVED
		// via pane.process_info: Herdr could reap and the OS could recycle
		// that pid before the signal lands (Astra review finding P1). This
		// watcher is the ONLY safe channel — the test asks this verified
		// process to kill ITSELF (os.Getpid()) by writing a control file
		// under the run's own scratch directory, never HOP_STATE_DIR and
		// never a pane/typed-input path.
		go watchForSelfKill(filepath.Join(scratchDir, "self-kill-"+env["HOP_ATTEMPT_ID"]))
	}
	writeObservation(observationPath, cmp(role, "solo"), assignmentPath, promptAssignmentPath, hopPath, assignmentContent, behavior, behaviorArgs, instructionsPath, instructionsContent)
	fmt.Println("FIXTURE-WORKER-READY")

	switch behavior {
	case "submit-valid":
		oid := commitChange("fixture worker change")
		submitOnce(hopPath, oid, "fixture worker result")
	case "submit-stale":
		waitForGo()
		oid := commitChange("fixture worker stale change")
		submitOnce(hopPath, oid, "fixture worker stale result")
	case "submit-twice":
		oid := commitChange("fixture worker change")
		submitOnce(hopPath, oid, "fixture worker result")
		submitOnce(hopPath, oid, "fixture worker result")
	case "exit-without-submitting":
		waitForGo()
		return
	case "exec-keep-pid":
		waitForGo()
		reexecSelf()
	case "worker-implement":
		oid := commitChange("fixture implementer change")
		drainMailbox(hopPath)
		submitOnce(hopPath, oid, "fixture implementer result")
	case "worker-hold":
		oid := commitChange("fixture implementer change (held)")
		questionPath := filepath.Join(scratchDir, "hold-question-"+env["HOP_ATTEMPT_ID"]+".txt")
		atomicWriteFile(questionPath, fixtureHoldMarker+"\n")
		res := runHopCLIRetryable(hopPath, "msg", "send", "--to", "manager", "--kind", "question", "--file", questionPath, "--request-id", newRequestID())
		question, ok := parseSentMessageID(res.Stdout)
		if !ok {
			fatalf("worker-hold: hop msg send did not accept the barrier question: %s", res.FirstLine())
		}
		fmt.Printf("FIXTURE-HOLD-SENT question=[%s]\n", question)
		for {
			answer, delivered := parseDeliveredMessage(runHopCLI(hopPath, "msg", "wait").Stdout)
			if !delivered {
				continue
			}
			answerBody := readFileOrFatal(answer.BodyPath)
			ackAndRequireSuccess(hopPath, answer.ID)
			if answer.Kind == "answer" && answer.ReplyTo == question {
				// This process's OWN read of the body file hop msg wait
				// itself named — never a copy the test handed it — dumped
				// as a digest, the byte-exact-delivery evidence design
				// section 11's injection scenario needs for the answer
				// channel: no existing dump captures a fetched message's
				// content, since every other behavior discards it once
				// read (design section 7: an ack is the statement of
				// receipt-and-read, not a promise to retain the bytes).
				writeContentDigest(filepath.Join(scratchDir, "answer-observed-"+env["HOP_ATTEMPT_ID"]+".txt"), answer.ID, answerBody)
				fmt.Printf("FIXTURE-HOLD-RELEASED answer=[%s]\n", answer.ID)
				break
			}
		}
		drainMailbox(hopPath)
		submitOnce(hopPath, oid, "fixture implementer result (released)")
	case "worker-fetch-crash":
		var msg deliveredMessage
		for {
			var delivered bool
			msg, delivered = parseDeliveredMessage(runHopCLI(hopPath, "msg", "wait").Stdout)
			if delivered {
				break
			}
		}
		msgBody := readFileOrFatal(msg.BodyPath)
		// This process's OWN read of the fetched message, dumped as a
		// digest naming its id -- the deterministic-kill-point evidence a
		// test waits for before triggering the self-kill (design section
		// 11 scenario 3, mirroring RelayedQuestion's manager barrier).
		writeContentDigest(filepath.Join(scratchDir, "fetch-crash-observed-"+env["HOP_ATTEMPT_ID"]+".txt"), msg.ID, msgBody)
		fmt.Printf("FIXTURE-FETCH-CRASH-OBSERVED id=[%s] resumed=[%t]\n", msg.ID, resumed)
		if !resumed {
			// Blocks here until this process's own self-kill control file
			// appears (already watched by the background watchForSelfKill
			// goroutine started above): the message is deliberately never
			// acked on this incarnation.
			watchForSelfKill(filepath.Join(scratchDir, "self-kill-"+env["HOP_ATTEMPT_ID"]))
		}
		// Resumed: the SAME message is re-served to this new incarnation
		// (design trace 3's "second delivery row"). Before acking, wait
		// for a SEPARATE, test-controlled release gate -- distinct from
		// the self-kill control file above -- so a scenario can issue a
		// stale-incarnation late-ack assertion with a guaranteed
		// happens-before relationship against this session's own ack,
		// rather than racing how quickly this relaunched process happens
		// to reach its own ack call (AcceptAck checks "already
		// acknowledged" before incarnation currency, so ordering decides
		// whether a late ack observes "stale" or "duplicate").
		waitForControlFile(filepath.Join(scratchDir, "fetch-crash-release-"+env["HOP_ATTEMPT_ID"]))
		ackAndRequireSuccess(hopPath, msg.ID)
		fmt.Printf("FIXTURE-FETCH-CRASH-ACKED id=[%s]\n", msg.ID)
		oid := commitChange("fixture implementer change (fetch-crash resumed)")
		drainMailbox(hopPath)
		// A THIRD, separate gate -- distinct from both the self-kill
		// control file and the ack-release gate above -- so a scenario can
		// assert the section 7 attention condition has fully cleared WHILE
		// this session is still active, never merely because retirement
		// made the address no-longer-live (design section 11 scenario 3's
		// own status-surface assertion needs the drain to have already
		// happened -- both m1 and m2 acked -- but this process not yet
		// exited). Dumps an observation the instant the post-drain,
		// pre-submit checkpoint is reached, then blocks until released.
		presubmitObservedPath := filepath.Join(scratchDir, "fetch-crash-presubmit-observed-"+env["HOP_ATTEMPT_ID"]+".txt")
		writeContentDigest(presubmitObservedPath, env["HOP_ATTEMPT_ID"], "fetch-crash-presubmit-hold")
		fmt.Printf("FIXTURE-FETCH-CRASH-PRESUBMIT-HOLD attempt=[%s]\n", env["HOP_ATTEMPT_ID"])
		waitForControlFile(filepath.Join(scratchDir, "fetch-crash-presubmit-release-"+env["HOP_ATTEMPT_ID"]))
		submitOnce(hopPath, oid, "fixture implementer result (fetch-crash resumed)")
	default:
		// Unknown or empty directive: submit nothing, just stay alive, so a
		// scenario that only needs a settled, idle worker still gets one.
	}
	idle()
}

// cmp returns a for a non-empty role, else fallback — used only to render
// a readable "solo" label in the observation dump when HOP_ROLE is unset.
func cmp(role, fallback string) string {
	if role == "" {
		return fallback
	}
	return role
}

// scratchDirRequiringBehaviors names every Phase 3 behavior whose
// FIXTURE-BEHAVIOR directive's first argument is a test-owned scratch
// directory: HOP_STATE_DIR's tree is HOP's own, never a place this
// fixture writes into, so observation dumps, message bodies it authors
// and one-shot markers all land in a directory the test itself supplied
// instead. Phase 2 behaviors carry no such argument and are unaffected.
var scratchDirRequiringBehaviors = map[string]bool{
	"worker-implement":     true,
	"worker-hold":          true,
	"worker-fetch-crash":   true,
	"manager-feature":      true,
	"reviewer-approve":     true,
	"reviewer-reject-once": true,
}

// requireScratchDir returns behaviorArgs[0] for a behavior that requires
// a test-owned scratch directory, failing the run loudly if it is absent
// or empty; "" for every other (Phase 2) behavior.
func requireScratchDir(behavior string, behaviorArgs []string) string {
	if !scratchDirRequiringBehaviors[behavior] {
		return ""
	}
	if len(behaviorArgs) == 0 || behaviorArgs[0] == "" {
		fatalf("FIXTURE-BEHAVIOR %s requires a test-owned scratch directory as its first argument", behavior)
	}
	return behaviorArgs[0]
}

// parseSentMessageID parses hop msg send's accepted/duplicate first line
// ("sent <uuid>" / "duplicate <uuid>") and returns the message id.
func parseSentMessageID(stdout string) (string, bool) {
	first, _, _ := strings.Cut(stdout, "\n")
	for _, prefix := range []string{"sent ", "duplicate "} {
		if id, ok := strings.CutPrefix(first, prefix); ok {
			return strings.TrimSpace(id), true
		}
	}
	return "", false
}

// parseCreatedTaskID parses hop task create's accepted/duplicate first
// line ("task <uuid> t<seq> created" / "duplicate <uuid> t<seq>") and
// returns the task id.
func parseCreatedTaskID(stdout string) (string, bool) {
	first, _, _ := strings.Cut(stdout, "\n")
	fields := strings.Fields(first)
	if len(fields) >= 2 && (fields[0] == "task" || fields[0] == "duplicate") {
		return fields[1], true
	}
	return "", false
}

// managerTask is one scripted task the manager creates.
type managerTask struct {
	Label     string
	Title     string
	Behavior  string
	DependsOn []string
}

// managerAnswerRule is one scripted ANSWER table entry: when a question's
// body contains Match, either relay it to the human (Action=="relay") or
// answer it directly with Body.
type managerAnswerRule struct {
	Match  string
	Action string // "answer" | "relay"
	Body   string
}

// managerScript is the manager-feature behavior's complete scripted plan,
// parsed from the run's brief (the same brief -> assignment.md channel
// every solo FIXTURE-BEHAVIOR directive already travels through — no side
// channel through the controller's environment). Line grammar, one
// directive per line after the leading "FIXTURE-BEHAVIOR: manager-feature"
// line:
//
//	TASK <label> title=<title, underscores for spaces> behavior=<name> [depends=<label>[,<label>...]]
//	ANSWER match=<substring, underscores for spaces> action=answer body=<body, underscores for spaces>
//	ANSWER match=<substring, underscores for spaces> action=relay
//	FIX behavior=<name>
//
// FIX names the behavior a reject-triggered fix task uses; absent, it
// defaults to "worker-implement".
type managerScript struct {
	Tasks       []managerTask
	Answers     []managerAnswerRule
	FixBehavior string
}

// parseManagerScript parses every script line in content (every line
// after the FIXTURE-BEHAVIOR directive; parseBehavior already located
// that line and is not re-run here).
func parseManagerScript(content string) *managerScript {
	script := &managerScript{FixBehavior: "worker-implement"}
	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		fields := strings.Fields(trimmed)
		if len(fields) == 0 {
			continue
		}
		switch fields[0] {
		case "TASK":
			if len(fields) < 2 {
				fatalf("manager script: TASK line %q names no label", trimmed)
			}
			task := managerTask{Label: fields[1]}
			for _, kv := range fields[2:] {
				key, value, ok := strings.Cut(kv, "=")
				if !ok {
					continue
				}
				switch key {
				case "title":
					task.Title = strings.ReplaceAll(value, "_", " ")
				case "behavior":
					task.Behavior = value
				case "depends":
					task.DependsOn = strings.Split(value, ",")
				}
			}
			script.Tasks = append(script.Tasks, task)
		case "ANSWER":
			rule := managerAnswerRule{}
			for _, kv := range fields[1:] {
				key, value, ok := strings.Cut(kv, "=")
				if !ok {
					continue
				}
				switch key {
				case "match":
					rule.Match = strings.ReplaceAll(value, "_", " ")
				case "action":
					rule.Action = value
				case "body":
					rule.Body = strings.ReplaceAll(value, "_", " ")
				}
			}
			script.Answers = append(script.Answers, rule)
		case "FIX":
			for _, kv := range fields[1:] {
				key, value, ok := strings.Cut(kv, "=")
				if ok && key == "behavior" {
					script.FixBehavior = value
				}
			}
		}
	}
	return script
}

// matchAnswer returns the first scripted rule whose Match is a substring
// of body, or nil when none matches.
func (s *managerScript) matchAnswer(body string) *managerAnswerRule {
	for i := range s.Answers {
		if strings.Contains(body, s.Answers[i].Match) {
			return &s.Answers[i]
		}
	}
	return nil
}

// writeTempInstructions writes a temporary instructions file whose sole
// content is the FIXTURE-BEHAVIOR directive for one task, in the
// manager's own cwd (the run's repository root — a real, writable git
// checkout the manager never commits from) — the artifact channel hop
// task create durably copies from before this file is ever read again.
// scratchDir is threaded through as the child worker's own directive
// argument (requireScratchDir), so a worker-implement/worker-hold
// attempt this task spawns writes its own observation dump and any
// message body it authors into the SAME test-owned directory the
// manager itself was given, never under HOP_STATE_DIR.
func writeTempInstructions(dir, behavior, scratchDir string) string {
	path := filepath.Join(dir, fmt.Sprintf("fixture-instructions-%d.md", time.Now().UnixNano()))
	atomicWriteFile(path, "FIXTURE-BEHAVIOR: "+behavior+" "+scratchDir+"\n\nImplement the assigned change and commit it.\n")
	return path
}

// parseNeedsReworkLabel extracts the task label from a renderTaskNotice
// body's first line ("task t<seq> needs-rework\n...",
// internal/app/usecase_featurecheck.go's renderTaskNotice) when its
// consequence is needs-rework; ok is false for any other notice shape
// (an integrated/dependents-released notice, a failed notice, etc.),
// which the manager acks without acting on.
func parseNeedsReworkLabel(body string) (label string, ok bool) {
	first, _, _ := strings.Cut(body, "\n")
	fields := strings.Fields(first)
	if len(fields) == 3 && fields[0] == "task" && fields[2] == "needs-rework" {
		return fields[1], true
	}
	return "", false
}

// runManager is the manager-feature behavior's entry point: a scripted
// manager that creates its plan through the real hop task/plan verbs,
// closes the plan, then spends its entire remaining lifetime in the
// section 7 idle-poll loop (hop msg wait), answering questions from its
// scripted table, relaying a barrier question to the human when scripted,
// forwarding a human's answer back through the relay chain using only the
// envelope's own origin field, retrying a task on its needs-rework notice,
// and — the ONLY channel section 8/STATUS-1 actually name for a reject
// verdict, since the acceptance notice's own body is just the reviewer's
// raw reasons text with no distinguishing marker — checking hop status
// for the rendered guard shortfall on any other info notice
// (statusReportsVerdictRejected) and planning a fix task when it names
// one. It never exits on its own: per-attempt retirement (section 6) is
// what a scenario proves actually terminates it.
func runManager() {
	env := requireEnv("HOP_STATE_DIR", "HOP_RUN_ID", "HOP_SESSION_ID", "HOP_INCARNATION_ID")
	assignmentPath := filepath.Join(env["HOP_STATE_DIR"], "runs", env["HOP_RUN_ID"], "artifacts", "assignment.md")

	prompt, resumed := resolvePrompt(os.Args)
	promptAssignmentPath, hopPath := extractFromPrompt(managerPromptMarkers[resumed], prompt)
	if promptAssignmentPath != assignmentPath {
		fatalf("prompt's assignment path (%s) does not match the independently computed path (%s)", promptAssignmentPath, assignmentPath)
	}

	assignmentContent := readFileOrFatal(assignmentPath)
	behavior, behaviorArgs := parseBehavior(assignmentContent)
	scratchDir := requireScratchDir(behavior, behaviorArgs)
	observationPath := filepath.Join(filepath.Dir(assignmentPath), "manager-observed.txt")
	var selfKillControlPath string
	if scratchDir != "" {
		observationPath = filepath.Join(scratchDir, "manager-observed.txt")
		// The same safe self-kill channel runWorker's own attempts use
		// (Astra review finding P1): a test asks this VERIFIED process to
		// kill ITSELF by writing a control file, never signals a pid it
		// only observed. Named by this session's own id (the manager has
		// no attempt id) so a cold-relaunched successor's watcher never
		// collides with its predecessor's already-consumed control file.
		selfKillControlPath = filepath.Join(scratchDir, "self-kill-"+env["HOP_SESSION_ID"])
		go watchForSelfKill(selfKillControlPath)
	}
	writeObservation(observationPath, "manager", assignmentPath, promptAssignmentPath, hopPath, assignmentContent, behavior, nil, "", "")
	fmt.Println("FIXTURE-MANAGER-READY")

	if behavior != "manager-feature" {
		// An unscripted manager: nothing to plan. Still idle at the
		// composer-like loop so retirement has something to terminate.
		idle()
		return
	}
	script := parseManagerScript(assignmentContent)

	cwd, err := os.Getwd()
	if err != nil {
		fatalf("resolve manager working directory: %v", err)
	}
	labelToID := map[string]string{}
	if !resumed {
		// A cold-relaunched successor re-reads the SAME assignment (and
		// therefore the same script) but must never re-plan: every task
		// this script names was already created by a PRIOR incarnation
		// (each hop task create call mints a FRESH --request-id per
		// invocation, so request-id idempotency alone would not catch a
		// resumed re-run, and the design's own reopen-on-create rule
		// means a redundant create would not even be refused). Skipping
		// this block on resume leaves labelToID/fixCounter's later use
		// (a needs-rework retry, a reject-verdict fix task) unavailable
		// to a cold-relaunched manager -- out of scope for the scenarios
		// that exercise this resume path today, which never combine it
		// with a retry or a fix task.
		for _, task := range script.Tasks {
			instructionsPath := writeTempInstructions(cwd, task.Behavior, scratchDir)
			args := []string{"task", "create", "--title", task.Title, "--file", instructionsPath, "--request-id", newRequestID()}
			for _, dep := range task.DependsOn {
				depID, known := labelToID[dep]
				if !known {
					fatalf("manager script: TASK %s depends on unknown label %s (declare it earlier)", task.Label, dep)
				}
				args = append(args, "--depends-on", depID)
			}
			res := runHopCLIRetryable(hopPath, args...)
			taskID, ok := parseCreatedTaskID(res.Stdout)
			if !ok {
				fatalf("manager script: hop task create for %s was refused: %s", task.Label, res.FirstLine())
			}
			labelToID[task.Label] = taskID
			fmt.Printf("FIXTURE-TASK-CREATED label=[%s] id=[%s]\n", task.Label, taskID)
		}
		if res := runHopCLIRetryable(hopPath, "plan", "close", "--request-id", newRequestID()); res.ExitCode != 0 && !strings.HasPrefix(res.FirstLine(), "duplicate") {
			fatalf("manager script: hop plan close was refused: %s", res.FirstLine())
		}
		fmt.Println("FIXTURE-PLAN-CLOSED")
	}

	fixCounter := len(script.Tasks)
	plannedFixReviews := map[string]bool{}
	for {
		msg, delivered := parseDeliveredMessage(runHopCLI(hopPath, "msg", "wait").Stdout)
		if !delivered {
			continue
		}
		handleManagerMessage(hopPath, cwd, env["HOP_RUN_ID"], scratchDir, script, labelToID, &fixCounter, plannedFixReviews, &msg, resumed, selfKillControlPath)
	}
}

// verdictRejectedShortfallToken mirrors app.GrammarShortfallVerdictRejected
// (internal/app/grammar.go, itself mirroring run.ShortfallVerdictRejected):
// EvaluateReadiness's reject-verdict guard-shortfall kind token,
// "verdict-rejected". The manager's standing instruction (design section
// 7/8, STATUS-1's manager verdict channel, internal/app/templates.go's
// renderManagerAssignment) is to run hop status after any controller info
// notice it does not otherwise recognize and read its shortfall lines —
// the ONLY channel that names a reject verdict at all, since the
// acceptance notice's own body is just the reviewer's raw reasons text
// (sqlite/review.go's persistVerdictAcceptance), never a marker.
const verdictRejectedShortfallToken = "verdict-rejected"

// evidenceInconsistentShortfallToken mirrors
// app.GrammarShortfallEvidenceInconsistent: the status read model's own
// value-free shortfall kind, reported in place of the check and verdict
// guards when the recorded evidence about the current head contradicts
// itself. parseVerdictRejectedLines already skips it (and every other
// shortfall shape) by construction, so the manager never mistakes it for
// a rejection; statusHasEvidenceInconsistentShortfall below is the
// distinct positive detection the verdict-channel instruction's own
// human-escalation rule needs.
const evidenceInconsistentShortfallToken = "evidence-inconsistent"

// evidenceInconsistentShortfallLine mirrors the fixed line
// app.GrammarShortfallLine renders for evidence-inconsistent: value-free,
// carrying no task label (grammar.go: taskLabel is "" for every shortfall
// but task-not-integrated), so it is exactly this text with no
// variation.
const evidenceInconsistentShortfallLine = "shortfall: " + evidenceInconsistentShortfallToken

// statusHasEvidenceInconsistentShortfall reports whether one hop status
// rendering carries the evidence-inconsistent shortfall line. The
// manager's verdict-channel instruction treats it as never a rejection,
// but — distinctly from an ordinary non-match — still escalates it: HOP's
// own recorded evidence about the current head disagrees, so a human
// needs to look, not just wait.
func statusHasEvidenceInconsistentShortfall(statusOutput string) bool {
	for _, line := range strings.Split(statusOutput, "\n") {
		if strings.TrimSpace(line) == evidenceInconsistentShortfallLine {
			return true
		}
	}
	return false
}

// fixtureEvidenceInconsistentQuestionBody is the fixed body of the
// question the manager sends to human when hop status names the
// evidence-inconsistent shortfall on a notice it does not otherwise
// recognize (design section 7/8's verdict-channel instruction: "ask the
// human to inspect the run").
const fixtureEvidenceInconsistentQuestionBody = "HOP's recorded evidence about this run's current head is inconsistent; please inspect the run."

// verdictRejectedLinePrefix mirrors the fixed text
// app.GrammarVerdictRejectedLine renders before its review= field
// (grammar.go's GrammarVerdictRejectedLine: "shortfall: " plus the
// verdict-rejected token plus " review="), so parsing fails loudly on any
// drift rather than matching a bare substring anywhere in the output.
const verdictRejectedLinePrefix = "shortfall: " + verdictRejectedShortfallToken + " review="

// verdictRejection is one parsed "shortfall: verdict-rejected
// review=<id> subject=<oid> reasons=<path>" line
// (app.GrammarVerdictRejectedLine, STATUS-1): the specific review's
// identity, its subject commit and its reasons artifact path — the three
// fields the manager's own verdict-channel instruction says a
// verdict-rejected shortfall carries so a manager can tell WHICH review
// is being reported, never just "the latest one".
type verdictRejection struct {
	reviewID, subjectCommitOID, reasonsPath string
}

// parseVerdictRejectedLines extracts every verdict-rejected shortfall
// line from one "hop status -run" rendering. A reasons path
// cmd/hop's safeRenderExternal rendered through strconv.Quote (rather
// than raw) is unquoted here: safeRenderExternal's own contract is that a
// raw field never begins with a double quote — that shape is reserved for
// the quoted form, which always starts with one — so a leading '"'
// unambiguously means the field must be unquoted, never a literal
// character of the real path. Any other line shape, including a
// value-free "shortfall: evidence-inconsistent" line (which names no
// review at all), is not this shape and is silently skipped: the
// manager's standing instruction treats evidence-inconsistent, and every
// notice or shortfall it does not otherwise recognize, as never a
// rejection.
func parseVerdictRejectedLines(statusOutput string) []verdictRejection {
	var out []verdictRejection
	for _, line := range strings.Split(statusOutput, "\n") {
		rest, ok := strings.CutPrefix(strings.TrimSpace(line), verdictRejectedLinePrefix)
		if !ok {
			continue
		}
		reviewID, rest, ok := strings.Cut(rest, " subject=")
		if !ok || reviewID == "" {
			fatalf("manager: malformed verdict-rejected shortfall line: %q", line)
		}
		subjectCommitOID, rawReasons, ok := strings.Cut(rest, " reasons=")
		if !ok || subjectCommitOID == "" {
			fatalf("manager: malformed verdict-rejected shortfall line: %q", line)
		}
		reasonsPath := rawReasons
		if strings.HasPrefix(reasonsPath, "\"") {
			unquoted, err := strconv.Unquote(reasonsPath)
			if err != nil {
				fatalf("manager: could not unquote status reasons path %q: %v", reasonsPath, err)
			}
			reasonsPath = unquoted
		}
		out = append(out, verdictRejection{reviewID: reviewID, subjectCommitOID: subjectCommitOID, reasonsPath: reasonsPath})
	}
	return out
}

// statusOutcomeForNotice runs "hop status -C <repoDir> -run <runID>"
// ONCE and reports everything the manager's verdict-channel instruction
// needs to act on a non-task-consequence info notice: the
// verdict-rejected shortfall, if any, whose reasons path equals
// noticeBodyPath — the correlation rule (internal/app/templates.go's
// renderManagerAssignment): EQUAL means this notice IS that review's
// rejection; DIFFERENT (or no verdict-rejected line at all) means it is
// not — and, only when no rejection matched, whether the SAME rendering
// also carries the evidence-inconsistent shortfall (escalated to the
// human, but still never treated as a rejection).
func statusOutcomeForNotice(hopPath, repoDir, runID, noticeBodyPath string) (rejection verdictRejection, matched, evidenceInconsistent bool) {
	res := runHopCLI(hopPath, "status", "-C", repoDir, "-run", runID)
	for _, r := range parseVerdictRejectedLines(res.Stdout) {
		if r.reasonsPath == noticeBodyPath {
			return r, true, false
		}
	}
	return verdictRejection{}, false, statusHasEvidenceInconsistentShortfall(res.Stdout)
}

// preForwardBarrierControlFile is the fixed name of the opt-in control
// file a test creates under the manager's own scratch directory BEFORE
// starting the run, to enable RelayedQuestion's deterministic pre-forward
// barrier (design section 11 scenario 2): absent (every existing
// scenario), the manager forwards a fetched human answer immediately,
// exactly as it always has; present, the manager's FIRST incarnation
// (never a resumed one -- the barrier is one-shot, keyed to the
// incarnation via runManager's own resumed flag, since a cold-relaunched
// manager's argv is always --resume-shaped) blocks after fetching the
// answer and before forwarding it, until self-killed.
const preForwardBarrierControlFile = "manager-pre-forward-barrier"

// preForwardBarrierObservedFile is where the barrier dumps its own digest
// of the fetched answer id/body (writeContentDigest's shape) before
// blocking, so a test can wait for this file rather than guessing when
// the barrier took effect.
const preForwardBarrierObservedFile = "manager-pre-forward-observed.txt"

// preForwardBarrierEnabled reports whether a test has created
// preForwardBarrierControlFile under scratchDir.
func preForwardBarrierEnabled(scratchDir string) bool {
	if scratchDir == "" {
		return false
	}
	_, err := os.Stat(filepath.Join(scratchDir, preForwardBarrierControlFile))
	return err == nil
}

// postForwardBarrierControlFile is the fixed name of the opt-in control
// file a test creates under the manager's own scratch directory BEFORE
// starting the run, to enable RelayedQuestion's idempotency-proving
// post-forward, pre-ack kill variant (design trace 2: "a manager crash
// between the two [forward and ack] re-serves a2, and the forward's
// request ID makes any redo idempotent"): absent (every existing scenario,
// including the pre-forward-kill variant above), the manager acks a2
// immediately after its forward is accepted or found duplicate, exactly as
// it always has; present, the manager's FIRST incarnation (never a
// resumed one -- also one-shot, keyed the same way as the pre-forward
// barrier) self-kills immediately after the forward call returns and
// before its own ack of a2. Mutually exclusive with the pre-forward
// barrier in practice (no scenario enables both), but nothing enforces
// that here -- each is checked independently, at its own point in the
// case.
const postForwardBarrierControlFile = "manager-post-forward-barrier"

// postForwardBarrierObservedFile is where the post-forward barrier dumps
// its own digest of the fetched answer id/body, before blocking.
const postForwardBarrierObservedFile = "manager-post-forward-observed.txt"

// postForwardBarrierEnabled reports whether a test has created
// postForwardBarrierControlFile under scratchDir.
func postForwardBarrierEnabled(scratchDir string) bool {
	if scratchDir == "" {
		return false
	}
	_, err := os.Stat(filepath.Join(scratchDir, postForwardBarrierControlFile))
	return err == nil
}

// handleManagerMessage dispatches one delivered message per the section 7
// manager operating contract, acknowledging it before the next wait in
// every case. plannedFixReviews tracks every review id this manager has
// already planned a fix task for, keyed by the review's own id, so a
// re-served or redelivered rejection notice for a review already acted on
// never plans a second fix (the verdict-channel instruction's own rule:
// "never plan a second fix from the same shortfall once its path has
// already matched a notice you acted on"). resumed and selfKillControlPath
// support the opt-in pre-forward barrier above: resumed is runManager's
// own resolvePrompt flag (false on this incarnation's first launch, true
// on every cold relaunch), and selfKillControlPath is the same self-kill
// control file runManager's own background watchForSelfKill goroutine
// already watches ("" when the manager has no scratch directory, in which
// case the barrier can never be enabled either).
func handleManagerMessage(hopPath, cwd, runID, scratchDir string, script *managerScript, labelToID map[string]string, fixCounter *int, plannedFixReviews map[string]bool, msg *deliveredMessage, resumed bool, selfKillControlPath string) {
	body := readFileOrFatal(msg.BodyPath)
	switch msg.Kind {
	case "question":
		rule := script.matchAnswer(body)
		if rule != nil && rule.Action == "relay" {
			res := runHopCLIRetryable(hopPath, "msg", "send", "--to", "human", "--kind", "question", "--relay-of", msg.ID, "--file", msg.BodyPath, "--request-id", newRequestID())
			fmt.Printf("FIXTURE-RELAYED question=[%s] relay=[%s]\n", msg.ID, res.FirstLine())
		} else {
			answerBody := fixtureAckGenericBody
			if rule != nil {
				answerBody = rule.Body
			}
			runHopCLIRetryable(hopPath, "msg", "send", "--kind", "answer", "--reply-to", msg.ID, "--body", answerBody, "--request-id", newRequestID())
		}
	case "answer":
		if msg.From != "human" || msg.Origin == "" {
			fatalf("manager received an answer it did not expect (from=%s origin=%s); the fixture manager only ever asks the human via a relay", msg.From, msg.Origin)
		}
		if !resumed && selfKillControlPath != "" && preForwardBarrierEnabled(scratchDir) {
			writeContentDigest(filepath.Join(scratchDir, preForwardBarrierObservedFile), msg.ID, body)
			fmt.Printf("FIXTURE-PRE-FORWARD-BARRIER answer=[%s]\n", msg.ID)
			// Blocks here: watchForSelfKill polls the same control file its
			// own already-running background goroutine watches and
			// SIGKILLs this process the instant a test writes it -- this
			// call's only purpose is to keep this goroutine from ever
			// reaching the forward call below in the meantime.
			watchForSelfKill(selfKillControlPath)
		}
		// The forward's own request id is derived DETERMINISTICALLY from
		// a2's own id (CLI-returned data, never generated fresh and never
		// persisted anywhere by this fixture) -- a relaunched manager
		// recomputes the IDENTICAL value with no hidden state, making its
		// own redo of this exact call idempotent under request-ID reuse
		// (design trace 2).
		forwardRequestID := deterministicUUIDFrom(msg.ID)
		forwardRes := runHopCLIRetryable(hopPath, "msg", "send", "--kind", "answer", "--reply-to", msg.Origin, "--file", msg.BodyPath, "--request-id", forwardRequestID)
		forwardFirst := forwardRes.FirstLine()
		if !strings.HasPrefix(forwardFirst, "sent ") && !strings.HasPrefix(forwardFirst, "duplicate ") {
			fatalf("manager's forward of origin=%s was neither accepted nor duplicate: %s", msg.Origin, forwardFirst)
		}
		fmt.Printf("FIXTURE-FORWARDED origin=[%s] request-id=[%s] result=[%s]\n", msg.Origin, forwardRequestID, forwardFirst)
		if !resumed && selfKillControlPath != "" && postForwardBarrierEnabled(scratchDir) {
			writeContentDigest(filepath.Join(scratchDir, postForwardBarrierObservedFile), msg.ID, body)
			fmt.Printf("FIXTURE-POST-FORWARD-BARRIER answer=[%s]\n", msg.ID)
			// Blocks here, exactly like the pre-forward barrier above, but
			// AFTER the forward has already landed and BEFORE this
			// process ever reaches the shared ack call at the end of this
			// function -- design trace 2's "crash between forward and
			// ack" idempotency case.
			watchForSelfKill(selfKillControlPath)
		}
	case "info":
		if label, ok := parseNeedsReworkLabel(body); ok {
			taskID, known := labelToID[label]
			if !known {
				fatalf("manager received a needs-rework notice for unknown label %s", label)
			}
			res := runHopCLIRetryable(hopPath, "task", "retry", "--reason", "fixture retry after interruption", "--request-id", newRequestID(), taskID)
			fmt.Printf("FIXTURE-RETRIED label=[%s] result=[%s]\n", label, res.FirstLine())
			break
		}
		// Not a task-consequence notice: per the manager's own
		// verdict-channel instruction (design section 7/8, STATUS-1), run
		// hop status and correlate its verdict-rejected shortfall lines
		// against THIS notice's own body path — the ONLY way to know WHICH
		// review a reject verdict's notice reports, since the notice's body
		// is only the reviewer's raw reasons text with no distinguishing
		// marker.
		rejection, matched, evidenceInconsistent := statusOutcomeForNotice(hopPath, cwd, runID, msg.BodyPath)
		fmt.Printf("FIXTURE-STATUS-CHECKED matched=[%t] evidence-inconsistent=[%t]\n", matched, evidenceInconsistent)
		switch {
		case matched && !plannedFixReviews[rejection.reviewID]:
			plannedFixReviews[rejection.reviewID] = true
			*fixCounter++
			label := "fix" + strconv.Itoa(*fixCounter)
			instructionsPath := writeTempInstructions(cwd, script.FixBehavior, scratchDir)
			res := runHopCLIRetryable(hopPath, "task", "create", "--title", "fix from reject", "--file", instructionsPath, "--request-id", newRequestID())
			taskID, ok := parseCreatedTaskID(res.Stdout)
			if !ok {
				fatalf("manager script: fix task creation was refused: %s", res.FirstLine())
			}
			labelToID[label] = taskID
			fmt.Printf("FIXTURE-FIX-TASK-CREATED label=[%s] id=[%s] review=[%s]\n", label, taskID, rejection.reviewID)
			if closeRes := runHopCLIRetryable(hopPath, "plan", "close", "--request-id", newRequestID()); closeRes.ExitCode != 0 && !strings.HasPrefix(closeRes.FirstLine(), "duplicate") {
				fatalf("manager script: hop plan close after the fix task was refused: %s", closeRes.FirstLine())
			}
		case evidenceInconsistent:
			// Never a rejection: plan no fix. The verdict-channel
			// instruction's remaining two-thirds — act on the notice itself
			// (nothing further to do for a generic info notice) and ask the
			// human to inspect the run — apply here.
			res := runHopCLIRetryable(hopPath, "msg", "send", "--to", "human", "--kind", "question", "--body", fixtureEvidenceInconsistentQuestionBody, "--request-id", newRequestID())
			fmt.Printf("FIXTURE-EVIDENCE-INCONSISTENT-ESCALATED result=[%s]\n", res.FirstLine())
		}
	}
	ackAndRequireSuccess(hopPath, msg.ID)
}

// reviewSubmitOnce runs "<hopPath> review submit ...", retrying on the
// section 5 mailbox-drain transient line exactly as submitOnce retries
// hop result submit's own transient line, draining before each retry.
func reviewSubmitOnce(hopPath, verdict, subjectCommit, reasonsPath string) hopResult {
	deadline := time.Now().Add(2 * time.Minute)
	for {
		res := runHopCLI(hopPath, "review", "submit", "--verdict", verdict, "--subject", subjectCommit, "--reasons-file", reasonsPath)
		fmt.Printf("FIXTURE-REVIEW-RESULT exit=[%d] first-line=[%s]\n", res.ExitCode, res.FirstLine())
		if strings.Contains(res.FirstLine(), "transient") && time.Now().Before(deadline) {
			drainMailbox(hopPath)
			time.Sleep(fixtureRetryInterval)
			continue
		}
		return res
	}
}

// runReviewer is the reviewer-approve / reviewer-reject-once behaviors'
// entry point. The reviewer's behavior directive lives in the run's
// frozen reviewer role artifact (a review task's assignment carries no
// manager-authored free text at all — internal/app/templates.go's
// renderReviewAssignment is fully computed from frozen run facts), an
// independently computable path exactly like the run-level artifacts
// this fixture already locates without parsing anything out of a prompt:
// internal/app/templates.go's roleArtifactPath(stateRoot, runID,
// "reviewer"), carrying its own test-owned scratch-directory argument
// exactly like every other Phase 3 directive (requireScratchDir).
// reviewer-reject-once tracks its own one-shot state with a durable
// marker file under THAT scratch directory — never under HOP_STATE_DIR,
// which is HOP's own tree — and it persists there across every review
// task's own separate reviewer process/pane in this run, since the
// frozen role artifact (and the scratch directory it names) is identical
// for every one of them.
func runReviewer() {
	env := requireEnv("HOP_STATE_DIR", "HOP_RUN_ID", "HOP_SESSION_ID", "HOP_INCARNATION_ID", "HOP_TASK_ID", "HOP_ATTEMPT_ID")
	assignmentPath := filepath.Join(env["HOP_STATE_DIR"], "runs", env["HOP_RUN_ID"], "attempts", env["HOP_ATTEMPT_ID"], "assignment.md")

	prompt, resumed := resolvePrompt(os.Args)
	promptAssignmentPath, hopPath := extractFromPrompt(reviewerPromptMarkers[resumed], prompt)
	if promptAssignmentPath != assignmentPath {
		fatalf("prompt's assignment path (%s) does not match the independently computed path (%s)", promptAssignmentPath, assignmentPath)
	}

	assignmentContent := readFileOrFatal(assignmentPath)
	subjectCommit := extractMarked(assignmentContent, "Commit: ", "\n")
	if subjectCommit == "" {
		fatalf("review assignment %s does not carry a %q line", assignmentPath, "Commit: ")
	}

	rolePath := filepath.Join(env["HOP_STATE_DIR"], "runs", env["HOP_RUN_ID"], "artifacts", "roles", "reviewer.md")
	roleContent := readFileOrFatal(rolePath)
	behavior, behaviorArgs := parseBehavior(roleContent)
	scratchDir := requireScratchDir(behavior, behaviorArgs)

	writeObservation(filepath.Join(scratchDir, "reviewer-observed-"+env["HOP_ATTEMPT_ID"]+".txt"), "reviewer", assignmentPath, promptAssignmentPath, hopPath, assignmentContent, behavior, nil, "", "")
	fmt.Println("FIXTURE-REVIEWER-READY")

	verdict, reasons := "approve", "fixture reviewer reasons: approve\n"
	if behavior == "reviewer-reject-once" {
		markerPath := filepath.Join(scratchDir, "reviewer-rejected-once")
		if _, statErr := os.Stat(markerPath); statErr != nil {
			verdict, reasons = "reject", "fixture reviewer reasons: reject\n"
			atomicWriteFile(markerPath, "rejected once\n")
		}
	}

	drainMailbox(hopPath)
	reasonsPath := filepath.Join(scratchDir, "reviewer-reasons-"+env["HOP_ATTEMPT_ID"]+".txt")
	atomicWriteFile(reasonsPath, reasons)
	reviewSubmitOnce(hopPath, verdict, subjectCommit, reasonsPath)
	idle()
}
`

// fixtureWorkerBrief renders a hop run brief whose text embeds the fixture
// worker's scripted behavior directive, so the brief -> assignment.md
// delivery path (section 6) is what carries the script to the worker — no
// separate plumbing through the controller's fixed HOP_* env keys. Known
// solo/implementer behavior names: "submit-valid", "submit-stale",
// "submit-twice", "exit-without-submitting", "exec-keep-pid",
// "worker-implement", "worker-hold"; an empty or unrecognized behavior
// makes the worker idle without ever submitting. A feature-mode
// implementer's behavior travels through its task's own manager-authored
// instructions file instead (fixtureManagerScript's TASK lines), never the
// run's top-level brief.
func fixtureWorkerBrief(behavior string) string {
	return "FIXTURE-BEHAVIOR: " + behavior + "\n"
}

// fixtureManagerTask/fixtureManagerAnswer describe one scripted manager
// plan, rendered by fixtureManagerBrief into the run's brief text the
// manager-feature behavior's embedded parseManagerScript parses (see the
// grammar documented on the embedded managerScript type above). Label is
// the manager's own local bookkeeping name for a task, referenced by
// later DependsOn entries — it is never sent to the store directly, but
// tasks are created in declaration order with no gaps, so it equals the
// real t<seq> label the store reports for a scenario that never mutates
// the plan out of order.
type fixtureManagerTask struct {
	Label, Title, Behavior string
	DependsOn              []string
}

// fixtureManagerAnswer is one scripted ANSWER rule: Action is "answer"
// (direct reply with Body) or "relay" (forwarded to the human, a
// test-controlled release — see fixtureHoldMarker).
type fixtureManagerAnswer struct {
	Match, Action, Body string
}

// fixtureManagerBrief renders the manager-feature behavior's complete
// scripted brief: the FIXTURE-BEHAVIOR directive (carrying scratchDir, the
// test-owned directory the manager's own observation dump and every task
// it plans use instead of anything under HOP_STATE_DIR — HOP's own tree),
// one TASK line per task (with its dependencies), one ANSWER line per
// scripted rule, and a FIX line naming the behavior a reject-triggered fix
// task uses. Spaces in Title/Match/Body are rendered as underscores (the
// embedded parser's simple whitespace-delimited field grammar); fixBehavior
// may be "" to accept parseManagerScript's own default ("worker-implement").
func fixtureManagerBrief(scratchDir string, tasks []fixtureManagerTask, answers []fixtureManagerAnswer, fixBehavior string) string {
	underscored := func(s string) string { return strings.ReplaceAll(s, " ", "_") }
	var b strings.Builder
	b.WriteString("FIXTURE-BEHAVIOR: manager-feature " + scratchDir + "\n")
	for _, task := range tasks {
		fmt.Fprintf(&b, "TASK %s title=%s behavior=%s", task.Label, underscored(task.Title), task.Behavior)
		if len(task.DependsOn) > 0 {
			fmt.Fprintf(&b, " depends=%s", strings.Join(task.DependsOn, ","))
		}
		b.WriteString("\n")
	}
	for _, answer := range answers {
		if answer.Action == "relay" {
			fmt.Fprintf(&b, "ANSWER match=%s action=relay\n", underscored(answer.Match))
			continue
		}
		fmt.Fprintf(&b, "ANSWER match=%s action=answer body=%s\n", underscored(answer.Match), underscored(answer.Body))
	}
	if fixBehavior != "" {
		fmt.Fprintf(&b, "FIX behavior=%s\n", fixBehavior)
	}
	return b.String()
}

// buildFixtureWorker compiles fixtureWorkerSource once for the calling test
// into a temporary module under the test's artifact directory and installs
// it under the name "claude" — never anywhere on the real system — so hop
// launch's PATH-based harness resolution (section 6) finds it exactly as it
// would the real Claude Code binary.
func buildFixtureWorker(t *testing.T, artifacts *artifactDir) string {
	t.Helper()
	src := artifacts.dir(t, "fixture-worker-src")
	if err := os.WriteFile(filepath.Join(src, "go.mod"), []byte("module fixtureworker\n\ngo 1.21\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "main.go"), []byte(fixtureWorkerSource), 0o600); err != nil {
		t.Fatal(err)
	}
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Fatalf("the go tool is required to build the fixture worker: %v", err)
	}
	binDir := artifacts.dir(t, "fixture-worker-bin")
	out := filepath.Join(binDir, "claude")
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	build := exec.CommandContext(ctx, goBin, "build", "-o", out, ".") //nolint:gosec // G204: the go tool builds this test's own generated fixture module.
	build.Dir = src
	if combined, buildErr := build.CombinedOutput(); buildErr != nil {
		t.Fatalf("go build fixture worker: %v\n%s", buildErr, combined)
	}
	return out
}

// testAssignmentPrompt reproduces internal/app/usecase_execboundary.go's
// renderInitialPrompt byte for byte (that function is unexported, so this
// package cannot call it directly) — the actual landed prompt hop launch
// composes into the harness argv. Used only to exercise the fixture worker's
// parsing directly, in isolation from cmd/hop and herdr; the full
// run/launch/corroborate scenarios exercise the real rendering end to end.
func testAssignmentPrompt(assignmentPath, hopPath string) string {
	return fmt.Sprintf("Read your assignment at %s and complete it. "+
		"When your work is committed, submit it by running: %s result submit --summary \"<one-line summary>\" --commit <commit-oid>. "+
		"If the first output line begins with \"transient\", wait briefly and run the exact same command again.",
		assignmentPath, hopPath)
}

// testContinuationPrompt reproduces internal/app/usecase_execboundary.go's
// renderContinuationPrompt byte for byte, exactly as testAssignmentPrompt
// mirrors renderInitialPrompt — the fixed positional prompt hop launch
// appends after `--resume <native-ref>` on a cold relaunch. Used to
// exercise the fixture worker's resume parsing directly, and by the
// real-process cold-relaunch scenario to recompute the relaunched claim's
// argv digest (resume_test.go).
func testContinuationPrompt(assignmentPath, hopPath string) string {
	return fmt.Sprintf("You were relaunched after an interruption; your restored session may show earlier, unfinished work. "+
		"Re-read your assignment at %s and continue it. "+
		"When your work is committed, submit it by running: %s result submit --summary \"<one-line summary>\" --commit <commit-oid>. "+
		"If the first output line begins with \"transient\", wait briefly and run the exact same command again.",
		assignmentPath, hopPath)
}

// TestFixtureWorkerSubmitValid drives the compiled fixture worker directly
// (no herdr, no cmd/hop) against a fixture repository and a fake hop stub
// that answers "transient" once before succeeding, proving: HOP_* env
// validation, prompt parsing for the hop path, assignment-path computation
// from HOP_STATE_DIR/HOP_RUN_ID, FIXTURE-BEHAVIOR directive extraction from
// the brief, a real git commit against the fixture repository's own
// identity, transient-retry, and graceful idle-then-quit.
func TestFixtureWorkerSubmitValid(t *testing.T) {
	artifacts := newArtifactDir(t)
	worker := buildFixtureWorker(t, artifacts)
	repo := newFixtureRepo(t, artifacts, nil, "repo")

	stateDir := artifacts.dir(t, "state")
	runID := "11111111-1111-1111-1111-111111111111"
	runDir := filepath.Join(stateDir, "runs", runID, "artifacts")
	if err := os.MkdirAll(runDir, 0o700); err != nil {
		t.Fatal(err)
	}
	assignmentPath := filepath.Join(runDir, "assignment.md")
	brief := fixtureWorkerBrief("submit-valid")
	assignmentContent := "# HOP Assignment\n\n## Brief\n\n" + brief + "\n## Instructions\n"
	if err := os.WriteFile(assignmentPath, []byte(assignmentContent), 0o600); err != nil {
		t.Fatal(err)
	}

	hopStub, transientCountFile := writeTransientOnceHopStub(t, artifacts)
	prompt := testAssignmentPrompt(assignmentPath, hopStub)

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, worker, "--session-id", "22222222-2222-2222-2222-222222222222", prompt) //nolint:gosec // G204: fixed test-owned binary and arguments.
	cmd.Dir = repo.Root
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOP_STATE_DIR=" + stateDir,
		"HOP_RUN_ID=" + runID,
		"HOP_TASK_ID=33333333-3333-3333-3333-333333333333",
		"HOP_ATTEMPT_ID=44444444-4444-4444-4444-444444444444",
		"HOP_INCARNATION_ID=55555555-5555-5555-5555-555555555555",
	}
	var stdout strings.Builder
	cmd.Stdout = &stdout
	cmd.Stdin = strings.NewReader("FIXTURE-QUIT\n")
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("run fixture worker: %v\nstdout:\n%s\nstderr:\n%s", err, stdout.String(), stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{"FIXTURE-WORKER-READY", "FIXTURE-SUBMIT-RESULT", "FIXTURE-WORKER-IDLE"} {
		if !strings.Contains(out, want) {
			t.Errorf("worker stdout missing %q; got:\n%s", want, out)
		}
	}

	calls, err := os.ReadFile(transientCountFile) //nolint:gosec // G304: a path this test constructed itself, under its own artifact directory.
	if err != nil {
		t.Fatalf("read hop stub call count: %v", err)
	}
	if strings.TrimSpace(string(calls)) != "2" {
		t.Errorf("hop stub called %s times, want 2 (one transient, one accepted)", strings.TrimSpace(string(calls)))
	}

	observed, err := os.ReadFile(filepath.Join(runDir, "worker-observed.txt")) //nolint:gosec // G304: a path this test constructed itself, under its own artifact directory.
	if err != nil {
		t.Fatalf("read worker observation dump: %v", err)
	}
	dump := string(observed)
	for _, want := range []string{
		"hop_path=" + hopStub,
		"behavior=submit-valid",
		"env:HOP_RUN_ID=" + runID,
		"assignment_content_begin",
		brief,
	} {
		if !strings.Contains(dump, want) {
			t.Errorf("worker observation dump missing %q; got:\n%s", want, dump)
		}
	}

	// The worker spawned its MCP stand-in child before reporting ready and
	// recorded its pid; the child holds the worker's pipe, so it must be
	// gone shortly after the worker exits — the fixture never leaks a
	// process past its own run.
	standInPID := parsedStandInPID(t, dump)
	if !waitUntil(func() bool { return !processExists(standInPID) }) {
		t.Errorf("mcp stand-in child pid %d still exists after the worker exited; the stand-in must die with its parent", standInPID)
	}

	head := repo.git(t, "rev-parse", "HEAD^{commit}")
	if head == repo.Base {
		t.Error("fixture worker did not commit a change before submitting")
	}
}

// parsedStandInPID extracts the positive mcp_stand_in_pid the worker's
// observation dump records.
func parsedStandInPID(t *testing.T, dump string) int {
	t.Helper()
	for _, line := range strings.Split(dump, "\n") {
		if value, ok := strings.CutPrefix(line, "mcp_stand_in_pid="); ok {
			pid, err := strconv.Atoi(strings.TrimSpace(value))
			if err != nil || pid <= 0 {
				t.Fatalf("worker observation dump mcp_stand_in_pid = %q, want a positive pid", value)
			}
			return pid
		}
	}
	t.Fatalf("worker observation dump carries no mcp_stand_in_pid line; dump:\n%s", dump)
	return 0
}

// writeTransientOnceHopStub writes a fake `hop` at a fresh path that prints
// "transient: retry" and exits 1 on its first invocation, then prints a
// normal acceptance line and exits 0 on every later call, counting
// invocations in a sibling file — a minimal stand-in for the real `hop
// result submit`, used only to prove the fixture worker's own retry
// behavior in isolation from cmd/hop.
func writeTransientOnceHopStub(t *testing.T, artifacts *artifactDir) (hopPath, countFile string) {
	t.Helper()
	dir := artifacts.dir(t, "hop-stub")
	countFile = filepath.Join(dir, "calls")
	if err := os.WriteFile(countFile, []byte("0"), 0o600); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\n" +
		"n=$(cat '" + countFile + "')\n" +
		"n=$((n + 1))\n" +
		"echo \"$n\" > '" + countFile + "'\n" +
		"if [ \"$n\" -eq 1 ]; then\n" +
		"  echo 'transient: attempt not yet running; retry'\n" +
		"  exit 1\n" +
		"fi\n" +
		"echo 'accepted result deadbeef'\n" +
		"exit 0\n"
	hopPath = filepath.Join(dir, "hop")
	if err := os.WriteFile(hopPath, []byte(script), 0o755); err != nil { //nolint:gosec // G306: this stub must be executable; it lives in this test's own artifact directory.
		t.Fatal(err)
	}
	return hopPath, countFile
}

// TestFixtureWorkerExitWithoutSubmitting proves the exit-without-submitting
// behavior never invokes hop at all, and that closing stdin (no FIXTURE-GO)
// is treated as an immediate go-ahead.
func TestFixtureWorkerExitWithoutSubmitting(t *testing.T) {
	artifacts := newArtifactDir(t)
	worker := buildFixtureWorker(t, artifacts)
	repo := newFixtureRepo(t, artifacts, nil, "repo")

	stateDir := artifacts.dir(t, "state")
	runID := "66666666-6666-6666-6666-666666666666"
	runDir := filepath.Join(stateDir, "runs", runID, "artifacts")
	if err := os.MkdirAll(runDir, 0o700); err != nil {
		t.Fatal(err)
	}
	assignmentPath := filepath.Join(runDir, "assignment.md")
	brief := fixtureWorkerBrief("exit-without-submitting")
	if err := os.WriteFile(assignmentPath, []byte("# HOP Assignment\n\n## Brief\n\n"+brief+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	hopStub, countFile := writeTransientOnceHopStub(t, artifacts)
	prompt := testAssignmentPrompt(assignmentPath, hopStub)

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, worker, "--session-id", "77777777-7777-7777-7777-777777777777", prompt) //nolint:gosec // G204: fixed test-owned binary and arguments.
	cmd.Dir = repo.Root
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOP_STATE_DIR=" + stateDir,
		"HOP_RUN_ID=" + runID,
		"HOP_TASK_ID=88888888-8888-8888-8888-888888888888",
		"HOP_ATTEMPT_ID=99999999-9999-9999-9999-999999999999",
		"HOP_INCARNATION_ID=00000000-0000-0000-0000-000000000000",
	}
	// No FIXTURE-GO is sent; stdin closes at once, which waitForGo treats as
	// an immediate go-ahead, and exit-without-submitting returns without
	// ever idling.
	if err := cmd.Run(); err != nil {
		t.Fatalf("run fixture worker: %v", err)
	}

	calls, err := os.ReadFile(countFile) //nolint:gosec // G304: a path this test constructed itself, under its own artifact directory.
	if err != nil {
		t.Fatalf("read hop stub call count: %v", err)
	}
	if strings.TrimSpace(string(calls)) != "0" {
		t.Errorf("hop stub called %s times, want 0 (exit-without-submitting must never invoke hop)", strings.TrimSpace(string(calls)))
	}
	if head := repo.git(t, "rev-parse", "HEAD^{commit}"); head != repo.Base {
		t.Error("exit-without-submitting committed a change; it must leave the repository untouched")
	}
}

// TestFixtureWorkerResumeContinuation drives the compiled fixture worker
// directly with the cold-relaunch argv shape
// (`--resume <native-ref> <continuation prompt>`): resume detection must
// accept the trailing prompt, and the worker must recover BOTH paths from
// the continuation prompt itself (its only source — nothing is persisted
// by a first launch), cross-validate the assignment path, and submit
// through the prompt-named hop path exactly as a first launch would.
func TestFixtureWorkerResumeContinuation(t *testing.T) {
	artifacts := newArtifactDir(t)
	worker := buildFixtureWorker(t, artifacts)
	repo := newFixtureRepo(t, artifacts, nil, "repo")

	stateDir := artifacts.dir(t, "state")
	runID := "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	runDir := filepath.Join(stateDir, "runs", runID, "artifacts")
	if err := os.MkdirAll(runDir, 0o700); err != nil {
		t.Fatal(err)
	}
	assignmentPath := filepath.Join(runDir, "assignment.md")
	brief := fixtureWorkerBrief("submit-valid")
	if err := os.WriteFile(assignmentPath, []byte("# HOP Assignment\n\n## Brief\n\n"+brief+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	hopStub, transientCountFile := writeTransientOnceHopStub(t, artifacts)
	prompt := testContinuationPrompt(assignmentPath, hopStub)

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, worker, "--resume", "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", prompt) //nolint:gosec // G204: fixed test-owned binary and arguments.
	cmd.Dir = repo.Root
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOP_STATE_DIR=" + stateDir,
		"HOP_RUN_ID=" + runID,
		"HOP_TASK_ID=cccccccc-cccc-4ccc-8ccc-cccccccccccc",
		"HOP_ATTEMPT_ID=dddddddd-dddd-4ddd-8ddd-dddddddddddd",
		"HOP_INCARNATION_ID=eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee",
	}
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	cmd.Stdin = strings.NewReader("FIXTURE-QUIT\n")
	if err := cmd.Run(); err != nil {
		t.Fatalf("run fixture worker (resume): %v\nstdout:\n%s\nstderr:\n%s", err, stdout.String(), stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{"FIXTURE-WORKER-READY", "FIXTURE-SUBMIT-RESULT", "FIXTURE-WORKER-IDLE"} {
		if !strings.Contains(out, want) {
			t.Errorf("worker stdout missing %q; got:\n%s", want, out)
		}
	}

	calls, err := os.ReadFile(transientCountFile) //nolint:gosec // G304: a path this test constructed itself, under its own artifact directory.
	if err != nil {
		t.Fatalf("read hop stub call count: %v", err)
	}
	if strings.TrimSpace(string(calls)) != "2" {
		t.Errorf("hop stub called %s times, want 2 (one transient, one accepted) — the resumed worker must submit through the continuation prompt's hop path", strings.TrimSpace(string(calls)))
	}

	obs := readWorkerObservation(t, filepath.Join(runDir, "worker-observed.txt"))
	if obs.Fields["hop_path"] != hopStub {
		t.Errorf("resumed worker hop_path = %q, want the continuation prompt's %q", obs.Fields["hop_path"], hopStub)
	}
	if obs.Fields["prompt_assignment_path"] != assignmentPath {
		t.Errorf("resumed worker prompt_assignment_path = %q, want %q", obs.Fields["prompt_assignment_path"], assignmentPath)
	}
}

// TestFixtureWorkerResumeShapeRejections proves the fixture worker refuses
// every resume-shaped argv that is not exactly
// `<exe> --resume <native-ref> <continuation prompt>`: the real CLI's
// `--resume [value]` takes an optional value, so a lax fixture reading the
// final element unconditionally would accept a reference-less
// `--resume <prompt>` (consuming the prompt as the resume value) or an
// invocation with extra arguments, masking a malformed launch instead of
// failing the scenario. Each rejected worker must exit non-zero, name the
// resume shape on stderr, and never invoke hop.
func TestFixtureWorkerResumeShapeRejections(t *testing.T) {
	artifacts := newArtifactDir(t)
	worker := buildFixtureWorker(t, artifacts)
	repo := newFixtureRepo(t, artifacts, nil, "repo")

	stateDir := artifacts.dir(t, "state")
	runID := "12121212-1212-4121-8121-121212121212"
	runDir := filepath.Join(stateDir, "runs", runID, "artifacts")
	if err := os.MkdirAll(runDir, 0o700); err != nil {
		t.Fatal(err)
	}
	assignmentPath := filepath.Join(runDir, "assignment.md")
	if err := os.WriteFile(assignmentPath, []byte("# HOP Assignment\n\n## Brief\n\n"+fixtureWorkerBrief("submit-valid")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	hopStub, countFile := writeTransientOnceHopStub(t, artifacts)
	prompt := testContinuationPrompt(assignmentPath, hopStub)
	const ref = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"

	cases := []struct {
		name string
		args []string
	}{
		{"missing reference: the prompt would be consumed as --resume's value", []string{"--resume", prompt}},
		{"missing prompt", []string{"--resume", ref}},
		{"intervening extra argument", []string{"--resume", ref, "--model", prompt}},
		{"wrong ordering", []string{ref, "--resume", prompt}},
		{"reference is not UUID-shaped", []string{"--resume", "not-a-native-ref", prompt}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, worker, tc.args...) //nolint:gosec // G204: fixed test-owned binary and arguments.
			cmd.Dir = repo.Root
			cmd.Env = []string{
				"PATH=" + os.Getenv("PATH"),
				"HOP_STATE_DIR=" + stateDir,
				"HOP_RUN_ID=" + runID,
				"HOP_TASK_ID=34343434-3434-4343-8343-343434343434",
				"HOP_ATTEMPT_ID=56565656-5656-4565-8565-565656565656",
				"HOP_INCARNATION_ID=78787878-7878-4787-8787-787878787878",
			}
			var stdout, stderr strings.Builder
			cmd.Stdout = &stdout
			cmd.Stderr = &stderr
			cmd.Stdin = strings.NewReader("")
			if err := cmd.Run(); err == nil {
				t.Fatalf("worker accepted the malformed resume argv %q\nstdout:\n%s\nstderr:\n%s", tc.args, stdout.String(), stderr.String())
			}
			if !strings.Contains(stderr.String(), "resume") {
				t.Errorf("worker stderr does not name the resume shape; got:\n%s", stderr.String())
			}
			if strings.Contains(stdout.String(), "FIXTURE-WORKER-READY") {
				t.Errorf("worker reported ready despite the malformed resume argv %q", tc.args)
			}
		})
	}

	calls, err := os.ReadFile(countFile) //nolint:gosec // G304: a path this test constructed itself, under its own artifact directory.
	if err != nil {
		t.Fatalf("read hop stub call count: %v", err)
	}
	if strings.TrimSpace(string(calls)) != "0" {
		t.Errorf("hop stub called %s times, want 0 (a rejected resume shape must never submit)", strings.TrimSpace(string(calls)))
	}
}

// workerObservation is the parsed contents of one worker-observed.txt dump
// (writeObservation, inside fixtureWorkerSource).
type workerObservation struct {
	Fields            map[string]string // pid, assignment_path, prompt_assignment_path, hop_path, behavior, behavior_args
	Environ           []string          // every "NAME=VALUE" entry from the worker's own os.Environ()
	AssignmentContent string
}

// readWorkerObservation reads and parses one worker-observed.txt dump.
func readWorkerObservation(t *testing.T, path string) workerObservation {
	t.Helper()
	raw, err := os.ReadFile(path) //nolint:gosec // G304: a path this test constructed itself, under its own artifact/state directory.
	if err != nil {
		t.Fatalf("read worker observation %s: %v", path, err)
	}
	obs := workerObservation{Fields: map[string]string{}}
	lines := strings.Split(string(raw), "\n")
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		switch {
		case line == "assignment_content_begin":
			var content []string
			for i++; i < len(lines) && lines[i] != "assignment_content_end"; i++ {
				content = append(content, lines[i])
			}
			obs.AssignmentContent = strings.Join(content, "\n")
		case strings.HasPrefix(line, "env:"):
			obs.Environ = append(obs.Environ, strings.TrimPrefix(line, "env:"))
		default:
			if key, value, ok := strings.Cut(line, "="); ok {
				obs.Fields[key] = value
			}
		}
	}
	return obs
}

// HasEnv reports whether the observed environment carries name=value
// exactly.
func (o workerObservation) HasEnv(name, value string) bool {
	return slices.Contains(o.Environ, name+"="+value)
}

// HasEnvName reports whether the observed environment carries any entry for
// name, regardless of value.
func (o workerObservation) HasEnvName(name string) bool {
	for _, entry := range o.Environ {
		if n, _, ok := strings.Cut(entry, "="); ok && n == name {
			return true
		}
	}
	return false
}

// syncOutput is a mutex-protected byte buffer safe as an exec.Cmd's
// Stdout/Stderr target while a live poll reads its accumulated content
// concurrently — os/exec copies a child's output into that writer from
// its own goroutine the moment Start returns, and a bare
// strings.Builder/bytes.Buffer provides no synchronization against a
// concurrent read of its internal slice header (a real data race, caught
// by `go test -race`, distinct from the writes themselves being
// serialized by os/exec since Stdout and Stderr share this same
// target). Mirrors ptyClient's own mu+buffer+snapshot shape (pty_test.go).
type syncOutput struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (o *syncOutput) Write(p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.buf.Write(p)
}

// snapshot returns the bytes captured so far. Safe to call at any time,
// including while the child is still running and writing concurrently;
// contrast with reading the raw buffer only after Wait, which this test
// also still does for its final failure-message content.
func (o *syncOutput) snapshot() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.buf.String()
}

// TestFixtureWorkerSelfKillOnControlFile proves the fixture principal's
// self-kill watcher (Astra review finding P1: a real-process scenario
// must never signal a pid it only OBSERVED via pane.process_info — the
// OS could recycle it between observation and signal). Drives a
// worker-hold implementer directly (no herdr, no pane) until it has sent
// its own barrier question — genuinely blocked in its own hop msg wait
// loop, exactly the state a real scenario kills it in — then writes the
// self-kill control file the SAME way featureharness_test.go's
// killSession does, and asserts the process terminates by SIGKILL of its
// OWN doing, never a signal this test aimed at an externally observed pid.
func TestFixtureWorkerSelfKillOnControlFile(t *testing.T) {
	artifacts := newArtifactDir(t)
	worker := buildFixtureWorker(t, artifacts)
	fakeHop := buildFakeHopStub(t, artifacts)
	repo := newFixtureRepo(t, artifacts, nil, "repo")

	stateDir := artifacts.dir(t, "state")
	scratchDir := artifacts.dir(t, "scratch")
	const (
		runID     = "d3d3d3d3-d3d3-4d3d-8d3d-d3d3d3d3d3d3"
		taskID    = "e4e4e4e4-e4e4-4e4e-8e4e-e4e4e4e4e4e4"
		attemptID = "f5f5f5f5-f5f5-4f5f-8f5f-f5f5f5f5f5f5"
		sessionID = "a6a6a6a6-a6a6-4a6a-8a6a-a6a6a6a6a6a6"
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
	logPath := filepath.Join(artifacts.dir(t, "log"), "log.txt")

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
		"HOP_INCARNATION_ID=b7b7b7b7-b7b7-4b7b-8b7b-b7b7b7b7b7b7",
		"HOP_ROLE=implementer",
		"FAKE_HOP_LOG=" + logPath,
	}
	var out syncOutput
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Start(); err != nil {
		t.Fatalf("start fixture worker: %v", err)
	}
	// Exactly one goroutine ever calls cmd.Wait, on every path: the happy
	// path below calls it directly, and cleanup calls it too (a no-op via
	// sync.Once if the happy path already did) so an early t.Fatalf before
	// that point — the readiness poll failing, say — still reaps this
	// directly-started child and lets its output-copy goroutine finish,
	// rather than leaving both dangling. Never signals or waits on any pid
	// other than this cmd's own Process handle.
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

	if !waitUntil(func() bool { return strings.Contains(out.snapshot(), "FIXTURE-HOLD-SENT") }) {
		t.Fatalf("worker never reported sending its barrier question; output so far:\n%s", out.snapshot())
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
	exitErr, ok := waitErr.(*exec.ExitError) //nolint:errorlint // a direct type assertion suffices for this test's own exec of a single known binary.
	if !ok {
		t.Fatalf("worker wait error = %v (%T), want *exec.ExitError", waitErr, waitErr)
	}
	ws, ok := exitErr.Sys().(syscall.WaitStatus)
	if !ok {
		t.Fatalf("worker exit status %#v is not a syscall.WaitStatus", exitErr.Sys())
	}
	if !ws.Signaled() || ws.Signal() != syscall.SIGKILL {
		t.Fatalf("worker exit status = %+v, want signaled by SIGKILL", ws)
	}
}
