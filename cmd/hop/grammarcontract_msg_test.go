package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/johnlanda/hop/internal/app"
)

// TestGrammarContractMsgSendUsage covers every hop msg send usage-error
// branch: all validated before any store is opened, so none of these
// cases seeds a fixture.
func TestGrammarContractMsgSendUsage(t *testing.T) {
	dir := freshStateDir(t)
	cases := []struct {
		name string
		args []string
	}{
		{"missing kind", []string{"msg", "send", "--to", "human", "--body", "hi"}},
		{"to forbidden for answer", []string{"msg", "send", "--kind", "answer", "--to", "human", "--reply-to", testUUID(1), "--body", "hi"}},
		{"reply-to required for answer", []string{"msg", "send", "--kind", "answer", "--body", "hi"}},
		{"reply-to forbidden for non-answer", []string{"msg", "send", "--kind", "info", "--to", "human", "--reply-to", testUUID(1), "--body", "hi"}},
		{"to required for non-answer", []string{"msg", "send", "--kind", "info", "--body", "hi"}},
		{"neither file nor body", []string{"msg", "send", "--kind", "info", "--to", "human"}},
		{"both file and body", []string{"msg", "send", "--kind", "info", "--to", "human", "--file", "/tmp/x", "--body", "hi"}},
		{"unexpected argument", []string{"msg", "send", "--kind", "info", "--to", "human", "--body", "hi", "extra"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result := execHop(t, map[string]string{"HOP_STATE_DIR": dir}, dir, tc.args...)
			if result.ExitCode != exitUsage {
				t.Fatalf("exit = %d, want %d (usage); stdout=%q stderr=%q", result.ExitCode, exitUsage, result.Stdout, result.Stderr)
			}
			if result.Stdout != "" {
				t.Errorf("usage error wrote to stdout: %q", result.Stdout)
			}
		})
	}
}

// TestGrammarContractMsgNextWaitUsage covers hop msg next/wait's own
// usage-error branch (an unexpected argument); no fixture needed.
func TestGrammarContractMsgNextWaitUsage(t *testing.T) {
	dir := freshStateDir(t)
	for _, verb := range []string{"next", "wait"} {
		t.Run(verb, func(t *testing.T) {
			result := execHop(t, map[string]string{"HOP_STATE_DIR": dir}, dir, "msg", verb, "extra")
			if result.ExitCode != exitUsage {
				t.Fatalf("exit = %d, want %d (usage); stdout=%q stderr=%q", result.ExitCode, exitUsage, result.Stdout, result.Stderr)
			}
		})
	}
}

// TestGrammarContractMsgAckShowUsage covers hop msg ack/show's own
// argument-count usage errors; no fixture needed.
func TestGrammarContractMsgAckShowUsage(t *testing.T) {
	dir := freshStateDir(t)
	cases := []struct {
		name string
		args []string
	}{
		{"ack missing id", []string{"msg", "ack"}},
		{"ack too many args", []string{"msg", "ack", testUUID(1), testUUID(2)}},
		{"show missing id", []string{"msg", "show"}},
		{"show too many args", []string{"msg", "show", testUUID(1), testUUID(2)}},
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

// TestGrammarContractMsgStateDirRequired proves every hop msg subcommand
// fails (not usage — a command failure) when HOP_STATE_DIR is missing or
// relative, without ever opening a store.
func TestGrammarContractMsgStateDirRequired(t *testing.T) {
	dir := freshStateDir(t)
	verbs := [][]string{
		{"msg", "send", "--kind", "info", "--to", "human", "--body", "hi"},
		{"msg", "next"},
		{"msg", "wait", "--timeout", "10ms"},
		{"msg", "ack", testUUID(1)},
		{"msg", "show", testUUID(1)},
	}
	for _, args := range verbs {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			t.Run("missing", func(t *testing.T) {
				result := execHop(t, map[string]string{}, dir, args...)
				if result.ExitCode != exitFailure {
					t.Fatalf("exit = %d, want %d (failure); stdout=%q stderr=%q", result.ExitCode, exitFailure, result.Stdout, result.Stderr)
				}
				if result.Stdout != "" {
					t.Errorf("missing HOP_STATE_DIR wrote to stdout: %q", result.Stdout)
				}
			})
			t.Run("relative", func(t *testing.T) {
				result := execHop(t, map[string]string{"HOP_STATE_DIR": "relative/path"}, dir, args...)
				if result.ExitCode != exitFailure {
					t.Fatalf("exit = %d, want %d (failure); stdout=%q stderr=%q", result.ExitCode, exitFailure, result.Stdout, result.Stderr)
				}
			})
		})
	}
}

// TestGrammarContractMsgShowNotFound proves hop msg show's `refused:
// not-found` first line against a well-formed but nonexistent message id,
// with zero fixture seeding: the store auto-creates and migrates empty,
// and ShowMessage's only failure mode for an absent id is exactly this
// refusal.
func TestGrammarContractMsgShowNotFound(t *testing.T) {
	dir := freshStateDir(t)
	result := execHop(t, map[string]string{"HOP_STATE_DIR": dir}, dir, "msg", "show", "--run", testUUID(2), testUUID(1))
	wantFirst := app.GrammarRefusalLine(app.GrammarReasonNotFound)
	if got := result.FirstStdoutLine(); got != wantFirst {
		t.Errorf("first stdout line = %q, want %q; full stdout=%q stderr=%q", got, wantFirst, result.Stdout, result.Stderr)
	}
	if result.ExitCode != exitFailure {
		t.Errorf("exit = %d, want %d (failure)", result.ExitCode, exitFailure)
	}
}

// TestGrammarContractMsgNextWaitIdentityEquivalence is the manager's
// required proof: hop msg next and hop msg wait, given the SAME missing
// or malformed HOP_RUN_ID/HOP_SESSION_ID/HOP_INCARNATION_ID, produce
// IDENTICAL first lines and exit codes — the end-to-end confirmation
// that commit 10's ordering fix (the first fetchOneMessage attempt
// validates identity before hop msg wait ever reaches
// MessageWaitDefault) actually holds against the real binary. Every case
// here needs zero fixture seeding: a missing or malformed identity fails
// to parse before any store lookup, in both verbs, against a freshly
// created (empty) store.
func TestGrammarContractMsgNextWaitIdentityEquivalence(t *testing.T) {
	dir := freshStateDir(t)
	validEnv := map[string]string{
		"HOP_STATE_DIR":      dir,
		"HOP_RUN_ID":         testUUID(1),
		"HOP_SESSION_ID":     testUUID(2),
		"HOP_INCARNATION_ID": testUUID(3),
	}
	cases := []struct {
		name    string
		mutate  func(env map[string]string)
		wantErr bool // true when this case is expected to be a genuine command failure (all of them are, here)
	}{
		{"missing HOP_RUN_ID", func(env map[string]string) { delete(env, "HOP_RUN_ID") }, true},
		{"malformed HOP_RUN_ID", func(env map[string]string) { env["HOP_RUN_ID"] = malformedUUID }, true},
		{"missing HOP_SESSION_ID", func(env map[string]string) { delete(env, "HOP_SESSION_ID") }, true},
		{"malformed HOP_SESSION_ID", func(env map[string]string) { env["HOP_SESSION_ID"] = malformedUUID }, true},
		{"missing HOP_INCARNATION_ID", func(env map[string]string) { delete(env, "HOP_INCARNATION_ID") }, true},
		{"malformed HOP_INCARNATION_ID", func(env map[string]string) { env["HOP_INCARNATION_ID"] = malformedUUID }, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			nextEnv := map[string]string{}
			for k, v := range validEnv {
				nextEnv[k] = v
			}
			tc.mutate(nextEnv)
			waitEnv := map[string]string{}
			for k, v := range nextEnv {
				waitEnv[k] = v
			}

			next := execHop(t, nextEnv, dir, "msg", "next")
			wait := execHop(t, waitEnv, dir, "msg", "wait", "--timeout", "50ms")

			if next.ExitCode != exitFailure {
				t.Errorf("msg next exit = %d, want %d (failure)", next.ExitCode, exitFailure)
			}
			if next.FirstStdoutLine() != "" {
				t.Errorf("msg next wrote to stdout: %q", next.Stdout)
			}
			if next.ExitCode != wait.ExitCode {
				t.Errorf("exit codes differ: msg next=%d msg wait=%d", next.ExitCode, wait.ExitCode)
			}
			if next.FirstStdoutLine() != wait.FirstStdoutLine() {
				t.Errorf("first stdout lines differ: msg next=%q msg wait=%q", next.FirstStdoutLine(), wait.FirstStdoutLine())
			}
		})
	}
}

// TestGrammarContractAnswerUsage covers hop answer's usage-error branch
// (argument count, --run required, mutually exclusive body flags); no
// fixture needed since these are all validated before any store is
// opened.
func TestGrammarContractAnswerUsage(t *testing.T) {
	dir := realDir(t)
	cases := []struct {
		name string
		args []string
	}{
		{"missing question id", []string{"answer", "--run", testUUID(1), "--body", "hi"}},
		{"too many args", []string{"answer", testUUID(1), testUUID(2), "--run", testUUID(3), "--body", "hi"}},
		{"missing run", []string{"answer", testUUID(1), "--body", "hi"}},
		{"neither file nor body", []string{"answer", testUUID(1), "--run", testUUID(2)}},
		{"both file and body", []string{"answer", testUUID(1), "--run", testUUID(2), "--file", "/tmp/x", "--body", "hi"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result := execHop(t, map[string]string{}, dir, tc.args...)
			if result.ExitCode != exitUsage {
				t.Fatalf("exit = %d, want %d (usage); stdout=%q stderr=%q", result.ExitCode, exitUsage, result.Stdout, result.Stderr)
			}
		})
	}
}

// TestGrammarContractAnswerNeverReadsWorkerEnv proves hop answer resolves
// everything through -C/--run, never HOP_* worker identities: the
// process environment carries no HOP_* variable at all, and an unknown
// run id still produces the SAME "no run" usage refusal a populated
// worker environment would (zero fixture seeding either way).
func TestGrammarContractAnswerNeverReadsWorkerEnv(t *testing.T) {
	dir := realDir(t)
	result := execHop(t, map[string]string{}, dir, "answer", testUUID(1), "-C", dir, "--run", "r1", "--body", "hi")
	if result.ExitCode != exitUsage {
		t.Fatalf("exit = %d, want %d (usage, unknown r1 label); stdout=%q stderr=%q", result.ExitCode, exitUsage, result.Stdout, result.Stderr)
	}
}

// readArtifact reads a message/answer body artifact path back from disk,
// failing the test if it cannot be read — the direct-bytes check every
// hostile-body test in this file uses to confirm the stored content is
// byte-for-byte what was sent, independent of what stdout ever rendered.
func readArtifact(t *testing.T, path string) []byte {
	t.Helper()
	if !filepath.IsAbs(path) {
		t.Fatalf("artifact path %q is not absolute", path)
	}
	content, err := os.ReadFile(path) //nolint:gosec // G304: a path this test read back from the command's own stdout, under its own fixture state root.
	if err != nil {
		t.Fatalf("read artifact %s: %v", path, err)
	}
	return content
}

// TestGrammarContractMsgSendAndDeliver drives an accepted hop msg send
// (manager -> the implement task's mailbox), the implementer receiving it
// via hop msg next, then acknowledging it — the send/next/ack accepted
// path end to end, plus msg send's duplicate (request-id retry) outcome,
// msg next's none line, msg ack's duplicate and not-delivered (a
// different session's ack attempt) outcomes, and hop msg show's found
// rendering.
func TestGrammarContractMsgSendAndDeliver(t *testing.T) {
	f := newFeatureManager(t, 1000, defaultMessageWait)
	worker := f.addImplementTask(t, 1100, "implement the fixture change")

	send := execHop(t, f.env(nil), f.StateRoot, "msg", "send", "--to", "task:"+worker.TaskID, "--kind", "info", "--body", "please proceed", "--request-id", "req-1")
	if send.ExitCode != exitOK {
		t.Fatalf("msg send: exit=%d stdout=%q stderr=%q", send.ExitCode, send.Stdout, send.Stderr)
	}
	msgID := strings.TrimPrefix(send.FirstStdoutLine(), "sent ")
	if msgID == send.FirstStdoutLine() || msgID == "" {
		t.Fatalf("msg send first line = %q, want \"sent <id>\"", send.FirstStdoutLine())
	}

	dup := execHop(t, f.env(nil), f.StateRoot, "msg", "send", "--to", "task:"+worker.TaskID, "--kind", "info", "--body", "please proceed", "--request-id", "req-1")
	if want := app.GrammarSendDuplicateLine(msgID); dup.FirstStdoutLine() != want {
		t.Errorf("duplicate send first line = %q, want %q; stdout=%q stderr=%q", dup.FirstStdoutLine(), want, dup.Stdout, dup.Stderr)
	}

	next := execHop(t, worker.env(f, nil), f.StateRoot, "msg", "next")
	if next.ExitCode != exitOK {
		t.Fatalf("msg next: exit=%d stdout=%q stderr=%q", next.ExitCode, next.Stdout, next.Stderr)
	}
	wantFirst := app.GrammarMessageLine(msgID, "info", f.ManagerID, "", "", "")
	if got := next.FirstStdoutLine(); got != wantFirst {
		t.Errorf("msg next first line = %q, want %q", got, wantFirst)
	}
	lines := strings.Split(strings.TrimRight(next.Stdout, "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("msg next stdout lines = %v, want 3 (message/body/ack)", lines)
	}
	bodyPath := strings.TrimPrefix(lines[1], "body: ")
	if lines[1] == bodyPath {
		t.Fatalf("second line %q does not start with \"body: \"", lines[1])
	}
	if want := app.GrammarAckHintLine(msgID); lines[2] != want {
		t.Errorf("third line = %q, want %q", lines[2], want)
	}
	if got := string(readArtifact(t, bodyPath)); got != "please proceed" {
		t.Errorf("body artifact = %q, want %q", got, "please proceed")
	}

	// The reviewer (a different session, never served this message) acks
	// it before the implementer does: refused not-delivered. Undelivered
	// re-serves on every fetch until acked (design section 7's "no
	// pop-once queue" semantics — see the message/body/ack lines above,
	// which would repeat on a THIRD msg next here), so this must run
	// before the implementer's own ack below settles the message.
	reviewer := f.addReviewTask(t, 1200, strings.Repeat("a", 40), strings.Repeat("b", 40))
	foreignAck := execHop(t, reviewer.env(f, nil), f.StateRoot, "msg", "ack", msgID)
	if want := app.GrammarRefusalLine(app.GrammarReasonNotDelivered); foreignAck.FirstStdoutLine() != want {
		t.Errorf("foreign ack first line = %q, want %q; stdout=%q stderr=%q", foreignAck.FirstStdoutLine(), want, foreignAck.Stdout, foreignAck.Stderr)
	}

	ack := execHop(t, worker.env(f, nil), f.StateRoot, "msg", "ack", msgID)
	if want := app.GrammarAckAcceptedLine(msgID); ack.FirstStdoutLine() != want {
		t.Errorf("ack first line = %q, want %q; stdout=%q stderr=%q", ack.FirstStdoutLine(), want, ack.Stdout, ack.Stderr)
	}

	ackDup := execHop(t, worker.env(f, nil), f.StateRoot, "msg", "ack", msgID)
	if want := app.GrammarAckDuplicateLine(msgID); ackDup.FirstStdoutLine() != want {
		t.Errorf("duplicate ack first line = %q, want %q", ackDup.FirstStdoutLine(), want)
	}

	// Now that the only message is acked, a further fetch finds nothing
	// queued.
	none := execHop(t, worker.env(f, nil), f.StateRoot, "msg", "next")
	if want := app.GrammarMsgNoneLine; none.FirstStdoutLine() != want {
		t.Errorf("msg next after ack first line = %q, want %q", none.FirstStdoutLine(), want)
	}
	if none.ExitCode != exitOK {
		t.Errorf("msg next after ack exit = %d, want %d", none.ExitCode, exitOK)
	}

	show := execHop(t, map[string]string{"HOP_STATE_DIR": f.StateRoot}, f.StateRoot, "msg", "show", "--run", f.RunID, msgID)
	if show.ExitCode != exitOK {
		t.Fatalf("msg show: exit=%d stdout=%q stderr=%q", show.ExitCode, show.Stdout, show.Stderr)
	}
	showLines := strings.Split(strings.TrimRight(show.Stdout, "\n"), "\n")
	if len(showLines) < 4 {
		t.Fatalf("msg show stdout lines = %v, want at least 4 (message/body/delivered/acknowledged)", showLines)
	}
	if !strings.HasPrefix(showLines[2], "delivered: ") {
		t.Errorf("third show line = %q, want a delivered: line", showLines[2])
	}
	if !strings.HasPrefix(showLines[3], "acknowledged: ") {
		t.Errorf("fourth show line = %q, want an acknowledged: line", showLines[3])
	}
}

// TestGrammarContractMsgSendArtifactWriteFailureEchoesNoPath proves a
// file-first body write that fails (the run's message directory is
// blocked by a regular file) is a plain command failure whose one stderr
// line names the step and category only: neither stream ever carries the
// state root, which is the operator's path.
func TestGrammarContractMsgSendArtifactWriteFailureEchoesNoPath(t *testing.T) {
	f := newFeatureManager(t, 1500, defaultMessageWait)
	runDir := filepath.Join(f.StateRoot, "runs", f.RunID)
	if err := os.MkdirAll(runDir, 0o700); err != nil {
		t.Fatalf("create run dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(runDir, "messages"), []byte("blocks the message directory"), 0o600); err != nil {
		t.Fatalf("block the message directory: %v", err)
	}

	result := execHop(t, f.env(nil), f.StateRoot, "msg", "send", "--to", "human", "--kind", "question", "--body", "can I proceed?")
	if result.ExitCode != exitFailure || result.Stdout != "" {
		t.Fatalf("exit = %d stdout = %q, want %d and nothing; stderr=%q", result.ExitCode, result.Stdout, exitFailure, result.Stderr)
	}
	if !strings.Contains(result.Stderr, "a path element is not a directory") {
		t.Errorf("stderr = %q, want the fixed category", result.Stderr)
	}
	if lines := strings.Split(strings.TrimRight(result.Stderr, "\n"), "\n"); len(lines) != 1 {
		t.Errorf("stderr = %q, want exactly one line", result.Stderr)
	}
	for _, leak := range []string{f.StateRoot, filepath.Base(f.StateRoot), runDir} {
		if strings.Contains(result.Stderr, leak) || strings.Contains(result.Stdout, leak) {
			t.Errorf("a stream echoes %q: stdout=%q stderr=%q", leak, result.Stdout, result.Stderr)
		}
	}
}

// TestGrammarContractMsgWaitDefaultTimeout is the manager's required
// real-binary confirmation of ruling C: hop msg wait's rendered "none"
// line uses the run's frozen [messages] wait_timeout when --timeout is
// absent, and an explicit --timeout always overrides it. The fixture
// freezes a short wait_timeout so this test stays fast.
func TestGrammarContractMsgWaitDefaultTimeout(t *testing.T) {
	f := newFeatureManager(t, 2000, 2*time.Second)
	worker := f.addImplementTask(t, 2100, "wait for a message")

	byDefault := execHop(t, worker.env(f, nil), f.StateRoot, "msg", "wait")
	wantDefault := app.GrammarMsgWaitNoneLine(2 * time.Second)
	if got := byDefault.FirstStdoutLine(); got != wantDefault {
		t.Errorf("msg wait (default) first line = %q, want %q; stdout=%q stderr=%q", got, wantDefault, byDefault.Stdout, byDefault.Stderr)
	}
	if byDefault.ExitCode != exitOK {
		t.Errorf("msg wait (default) exit = %d, want %d", byDefault.ExitCode, exitOK)
	}

	explicit := execHop(t, worker.env(f, nil), f.StateRoot, "msg", "wait", "--timeout", "300ms")
	wantExplicit := app.GrammarMsgWaitNoneLine(300 * time.Millisecond)
	if got := explicit.FirstStdoutLine(); got != wantExplicit {
		t.Errorf("msg wait (explicit) first line = %q, want %q; stdout=%q stderr=%q", got, wantExplicit, explicit.Stdout, explicit.Stderr)
	}
}

// TestGrammarContractMsgWaitDelivered proves hop msg wait's delivered
// shape is byte-identical to hop msg next's (both render
// deliveredMessageLines), against a message already queued before the
// wait call — so the very first poll iteration serves it, keeping this
// test fast without racing a background sender.
func TestGrammarContractMsgWaitDelivered(t *testing.T) {
	f := newFeatureManager(t, 2200, defaultMessageWait)
	worker := f.addImplementTask(t, 2300, "wait for a queued message")

	send := execHop(t, f.env(nil), f.StateRoot, "msg", "send", "--to", "task:"+worker.TaskID, "--kind", "info", "--body", "already queued")
	if send.ExitCode != exitOK {
		t.Fatalf("msg send: exit=%d stdout=%q stderr=%q", send.ExitCode, send.Stdout, send.Stderr)
	}
	msgID := strings.TrimPrefix(send.FirstStdoutLine(), "sent ")

	wait := execHop(t, worker.env(f, nil), f.StateRoot, "msg", "wait", "--timeout", "5s")
	wantFirst := app.GrammarMessageLine(msgID, "info", f.ManagerID, "", "", "")
	if got := wait.FirstStdoutLine(); got != wantFirst {
		t.Errorf("msg wait (delivered) first line = %q, want %q; stdout=%q stderr=%q", got, wantFirst, wait.Stdout, wait.Stderr)
	}
	if wait.ExitCode != exitOK {
		t.Errorf("msg wait (delivered) exit = %d, want %d", wait.ExitCode, exitOK)
	}
}

// TestGrammarContractMsgSendUnauthorizedCrossRun proves hop msg send's
// `refused: unauthorized` first line when the caller session belongs to
// a DIFFERENT run than HOP_RUN_ID names: two runs seeded in the SAME
// database (newFeatureManagerIn), b's manager identity used against a's
// run id and state root.
func TestGrammarContractMsgSendUnauthorizedCrossRun(t *testing.T) {
	a := newFeatureManager(t, 3000, defaultMessageWait)
	b := a.newFeatureManagerIn(t, 3100, defaultMessageWait)

	env := map[string]string{
		"HOP_STATE_DIR":      a.StateRoot,
		"HOP_RUN_ID":         a.RunID,
		"HOP_SESSION_ID":     b.ManagerID,
		"HOP_INCARNATION_ID": b.ManagerIncarnation,
	}
	result := execHop(t, env, a.StateRoot, "msg", "send", "--to", "human", "--kind", "info", "--body", "hi")
	want := app.GrammarRefusalLine(app.GrammarReasonUnauthorized)
	if got := result.FirstStdoutLine(); got != want {
		t.Errorf("first line = %q, want %q; stdout=%q stderr=%q", got, want, result.Stdout, result.Stderr)
	}
	if result.ExitCode != exitFailure {
		t.Errorf("exit = %d, want %d", result.ExitCode, exitFailure)
	}
}

// hostileBodyFragment is a plain-text piece of hostileBody no legitimate
// output line contains: finding it anywhere means body content leaked.
const hostileBodyFragment = "hostile-fragment-q7x"

// hostileBody is a message/answer body deliberately carrying ANSI escape
// sequences, a bracketed-paste envelope and a literal "y\n" — the exact
// shapes that could hijack a naive terminal or auto-confirm an
// interactive prompt if they ever reached a stream raw. Every test using
// it proves the opposite: only the body's absolute artifact PATH is ever
// printed, and the artifact itself carries these bytes unmodified.
const hostileBody = "\x1b[31mred\x1b[0m\x1b[200~pasted " + hostileBodyFragment + "\x1b[201~y\n"

// hostileCanaries lists hostileBody's independently checked pieces: a
// partial leak (an escape stripped but its sequence kept, or the text
// without its escapes) fails as surely as the whole body would.
func hostileCanaries() map[string]string {
	return map[string]string{
		"ESC":                   "\x1b",
		"bracketed-paste start": "[200~",
		"bracketed-paste end":   "[201~",
		"SGR color sequence":    "[31m",
		"body fragment":         hostileBodyFragment,
	}
}

// requireCleanSuccess fails when either stream carries any hostile
// canary (each reported on its own), then stops the test unless result
// exited 0 with an empty stderr.
func requireCleanSuccess(t *testing.T, label string, result hopResult) {
	t.Helper()
	for name, canary := range hostileCanaries() {
		if strings.Contains(result.Stdout, canary) {
			t.Errorf("%s: stdout carries the hostile body's %s: %q", label, name, result.Stdout)
		}
		if strings.Contains(result.Stderr, canary) {
			t.Errorf("%s: stderr carries the hostile body's %s: %q", label, name, result.Stderr)
		}
	}
	if result.ExitCode != exitOK || result.Stderr != "" {
		t.Fatalf("%s: exit=%d stderr=%q, want 0 and an empty stderr; stdout=%q", label, result.ExitCode, result.Stderr, result.Stdout)
	}
}

// requireSentOnly proves a producing invocation (hop msg send, hop
// answer) printed exactly one `sent <message-id>` line and nothing else,
// returning the id.
func requireSentOnly(t *testing.T, label string, result hopResult) string {
	t.Helper()
	requireCleanSuccess(t, label, result)
	id := strings.TrimSuffix(strings.TrimPrefix(result.Stdout, "sent "), "\n")
	if !uuidShape.MatchString(id) || result.Stdout != app.GrammarSentLine(id)+"\n" {
		t.Fatalf("%s: stdout = %q, want exactly one %q line", label, result.Stdout, app.GrammarSentLine("<message-id>"))
	}
	return id
}

// requireDeliveredOnly proves a consuming fetch (hop msg next/wait)
// printed exactly the three permitted lines — the envelope, the body PATH
// under the run's message directory, the ack hint — and that the artifact
// behind that path holds the hostile bytes unmodified.
func requireDeliveredOnly(t *testing.T, label string, result hopResult, stateRoot, runID, messageID, firstLine string) {
	t.Helper()
	requireCleanSuccess(t, label, result)
	bodyPath := hostileBodyPath(stateRoot, runID, messageID)
	want := firstLine + "\n" + app.GrammarBodyLine(bodyPath) + "\n" + app.GrammarAckHintLine(messageID) + "\n"
	if result.Stdout != want {
		t.Fatalf("%s: stdout = %q, want exactly %q", label, result.Stdout, want)
	}
	if got := string(readArtifact(t, bodyPath)); got != hostileBody {
		t.Errorf("%s: body artifact = %q, want %q", label, got, hostileBody)
	}
}

// hostileBodyPath is the file-first body artifact's path for messageID
// (internal/app's messageBodyPath layout).
func hostileBodyPath(stateRoot, runID, messageID string) string {
	return filepath.Join(stateRoot, "runs", runID, "messages", messageID+".md")
}

// uuidShape matches a canonical lowercase UUID.
var uuidShape = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// TestGrammarContractMsgHostileBodyNeverReachesStdoutRaw is the
// manager's required proof: a hostile body sent through hop msg send
// never reaches either stream raw through the producing send or the
// consuming hop msg next, show or wait — every invocation prints exactly
// its permitted lines (only the body's absolute artifact path, never its
// content) with an empty stderr, checked against independent canaries for
// ESC, both bracketed-paste markers, the SGR sequence and a body
// fragment — and the artifact itself carries the hostile bytes unmodified.
func TestGrammarContractMsgHostileBodyNeverReachesStdoutRaw(t *testing.T) {
	f := newFeatureManager(t, 4000, defaultMessageWait)
	worker := f.addImplementTask(t, 4100, "handle hostile bodies")
	recipient := "task:" + worker.TaskID

	msgIDA := requireSentOnly(t, "msg send (for next)", execHop(t, f.env(nil), f.StateRoot, "msg", "send", "--to", recipient, "--kind", "info", "--body", hostileBody))

	next := execHop(t, worker.env(f, nil), f.StateRoot, "msg", "next")
	requireDeliveredOnly(t, "msg next", next, f.StateRoot, f.RunID, msgIDA, app.GrammarMessageLine(msgIDA, "info", f.ManagerID, "", "", ""))

	show := execHop(t, map[string]string{"HOP_STATE_DIR": f.StateRoot}, f.StateRoot, "msg", "show", "--run", f.RunID, msgIDA)
	requireCleanSuccess(t, "msg show", show)
	showLines := strings.Split(strings.TrimSuffix(show.Stdout, "\n"), "\n")
	wantShow := []string{
		app.GrammarMessageShowLine(msgIDA, "info", f.ManagerID, recipient, "", "", 1),
		app.GrammarBodyLine(hostileBodyPath(f.StateRoot, f.RunID, msgIDA)),
	}
	if len(showLines) != 3 || showLines[0] != wantShow[0] || showLines[1] != wantShow[1] {
		t.Fatalf("msg show stdout = %q, want %q, %q and one delivered line", show.Stdout, wantShow[0], wantShow[1])
	}
	deliveredAt, ok := strings.CutPrefix(showLines[2], "delivered: "+worker.SessionID+" ")
	if _, err := time.Parse(time.RFC3339, deliveredAt); !ok || err != nil {
		t.Errorf("msg show third line = %q, want %q", showLines[2], app.GrammarDeliveredLine(worker.SessionID, time.Time{}))
	}

	// The worker settles A first: an unacknowledged message re-serves
	// ahead of B on every fetch.
	ack := execHop(t, worker.env(f, nil), f.StateRoot, "msg", "ack", msgIDA)
	requireCleanSuccess(t, "msg ack", ack)
	if want := app.GrammarAckAcceptedLine(msgIDA) + "\n"; ack.Stdout != want {
		t.Errorf("msg ack stdout = %q, want %q", ack.Stdout, want)
	}

	msgIDB := requireSentOnly(t, "msg send (for wait)", execHop(t, f.env(nil), f.StateRoot, "msg", "send", "--to", recipient, "--kind", "info", "--body", hostileBody))
	wait := execHop(t, worker.env(f, nil), f.StateRoot, "msg", "wait", "--timeout", "3s")
	requireDeliveredOnly(t, "msg wait", wait, f.StateRoot, f.RunID, msgIDB, app.GrammarMessageLine(msgIDB, "info", f.ManagerID, "", "", ""))
}

// TestGrammarContractAnswerAcceptedAndHostileBody drives hop answer's
// accepted path end to end: a worker's question goes to its manager (a
// worker may not address "human" directly — confirmed empirically: doing
// so refuses unauthorized, "task cannot send question to human"), the
// manager relays it to human (--relay-of), hop answer (through -C/--run,
// never worker env) answers the relayed question, and the manager
// receives the answer via hop msg next with relay provenance (origin=
// the ORIGINAL question id). Also proves a hostile answer body reaches
// neither stream of the producing hop answer nor of the consuming hop msg
// next: exactly the permitted lines, an empty stderr, every canary absent.
func TestGrammarContractAnswerAcceptedAndHostileBody(t *testing.T) {
	f := newFeatureManager(t, 5000, defaultMessageWait)
	worker := f.addImplementTask(t, 5100, "ask a question")

	ask := execHop(t, worker.env(f, nil), f.StateRoot, "msg", "send", "--to", "manager", "--kind", "question", "--body", "need guidance")
	questionID := requireSentOnly(t, "msg send (worker question)", ask)

	// The manager fetches and acks the worker's question before relaying
	// it, exactly as a real manager pass would — otherwise it stays the
	// oldest queued message and hop msg next below would re-serve it
	// ahead of the answer (FIFO, no pop-once queue).
	managerNext := execHop(t, f.env(nil), f.StateRoot, "msg", "next")
	if managerNext.ExitCode != exitOK {
		t.Fatalf("msg next (manager fetches worker question): exit=%d stdout=%q stderr=%q", managerNext.ExitCode, managerNext.Stdout, managerNext.Stderr)
	}
	managerAck := execHop(t, f.env(nil), f.StateRoot, "msg", "ack", questionID)
	if managerAck.ExitCode != exitOK {
		t.Fatalf("msg ack (manager acks worker question): exit=%d stdout=%q stderr=%q", managerAck.ExitCode, managerAck.Stdout, managerAck.Stderr)
	}

	relay := execHop(t, f.env(nil), f.StateRoot, "msg", "send", "--to", "human", "--kind", "question", "--relay-of", questionID, "--body", "worker needs guidance")
	relayID := requireSentOnly(t, "msg send (relay to human)", relay)

	answerEnv := map[string]string{"HOP_STATE_DIR": f.StateRoot}
	answer := execHop(t, answerEnv, f.RepositoryRoot, "answer", "-C", f.RepositoryRoot, "--run", f.RunID, "--body", hostileBody, relayID)
	answerMsgID := requireSentOnly(t, "answer", answer)

	next := execHop(t, f.env(nil), f.StateRoot, "msg", "next")
	requireDeliveredOnly(t, "msg next (answer)", next, f.StateRoot, f.RunID, answerMsgID, app.GrammarMessageLine(answerMsgID, "answer", "human", relayID, "", questionID))
}

// TestGrammarContractAnswerRefusesNonQuestion proves hop answer's
// `refused: malformed` first line when the named message is not a
// question at all (AcceptAnswer's shared "not a question" default).
func TestGrammarContractAnswerRefusesNonQuestion(t *testing.T) {
	f := newFeatureManager(t, 5200, defaultMessageWait)
	worker := f.addImplementTask(t, 5300, "send an info message")

	info := execHop(t, worker.env(f, nil), f.StateRoot, "msg", "send", "--to", "manager", "--kind", "info", "--body", "fyi")
	if info.ExitCode != exitOK {
		t.Fatalf("msg send (info): exit=%d stdout=%q stderr=%q", info.ExitCode, info.Stdout, info.Stderr)
	}
	infoID := strings.TrimPrefix(info.FirstStdoutLine(), "sent ")

	answer := execHop(t, map[string]string{"HOP_STATE_DIR": f.StateRoot}, f.RepositoryRoot, "answer", "-C", f.RepositoryRoot, "--run", f.RunID, "--body", "misdirected", infoID)
	want := app.GrammarRefusalLine(app.GrammarReasonMalformed)
	if got := answer.FirstStdoutLine(); got != want {
		t.Errorf("first line = %q, want %q; stdout=%q stderr=%q", got, want, answer.Stdout, answer.Stderr)
	}
	if answer.ExitCode != exitFailure {
		t.Errorf("exit = %d, want %d", answer.ExitCode, exitFailure)
	}
}
