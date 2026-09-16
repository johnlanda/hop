package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// This file and grammarcontract_*_test.go are the real-binary grammar
// contract tests (docs/plan/phase-3-design.md section 11, L1569): they
// build the actual ./cmd/hop binary once, exec every verb this slice owns
// (plus the five Phase 2 verbs that never got one) against a fixture state
// root, and assert stdout/stderr's FIRST LINE byte-for-byte against
// internal/app/grammar.go's constants and the documented exit-code
// contract (main.go's exitOK/exitFailure/exitUsage). internal/app's own
// TestGoldenGrammar already pins that a constant's literal text is right;
// these tests answer the narrower question TestGoldenGrammar cannot: does
// cmd/hop actually render it, end to end, through the real compiled
// binary.
//
// No Herdr binary and no network: every exec here runs under
// isolatedHopEnv, which never inherits the calling process's environment
// and always points HERDR_SOCKET_PATH at a per-test canary listener that
// fails the test if anything ever connects to it (herdrCanary below).
// Nothing in this file starts a Herdr server or lives under
// test/integration; the real SQLite adapter is the only production
// dependency exercised.

// TestMain builds the hop binary once for every test in this package's
// `go test` process (buildHopBinary, cached behind hopBinary's sync.Once)
// and removes the build directory once every test has run. cmd/hop had no
// existing TestMain before this file; Go allows exactly one per package.
func TestMain(m *testing.M) {
	code := m.Run()
	if hopBinary.dir != "" {
		_ = os.RemoveAll(hopBinary.dir) //nolint:errcheck // best-effort process-exit cleanup; a leftover temp dir is not a test failure.
	}
	os.Exit(code)
}

// hopBinary caches the single ./cmd/hop build every grammar contract test
// in this process shares, mirroring test/integration/hopcmd_test.go's
// identical pattern (that package cannot be imported here or vice versa;
// cmd/hop's own architecture rule permits only internal/app and the
// adapter packages, never test/integration). It is a narrow,
// process-lifetime mutable global guarded by sync.Once and cleaned up by
// TestMain above.
var hopBinary struct { //nolint:gochecknoglobals // process-lifetime build cache guarded by sync.Once; see comment above.
	once sync.Once
	dir  string
	path string
	err  error
}

// buildHopBinary builds this package (cmd/hop) once into a temporary
// directory and returns the built executable's absolute path. go test
// runs with the package's own source directory as the working directory,
// so building "." here is exactly `go build ./cmd/hop` from the module
// root — no module-root discovery is needed.
func buildHopBinary(t *testing.T) string {
	t.Helper()
	hopBinary.once.Do(func() {
		dir, err := os.MkdirTemp("", "hop-grammar-contract")
		if err != nil {
			hopBinary.err = err
			return
		}
		hopBinary.dir = dir
		goBin, err := exec.LookPath("go")
		if err != nil {
			hopBinary.err = fmt.Errorf("the go tool is required to build cmd/hop: %w", err)
			return
		}
		wd, err := os.Getwd()
		if err != nil {
			hopBinary.err = fmt.Errorf("resolve cmd/hop source directory: %w", err)
			return
		}
		out := filepath.Join(dir, "hop")
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		build := exec.CommandContext(ctx, goBin, "build", "-o", out, ".") //nolint:gosec // G204: the go tool builds this repository's own command with a fixed argument list.
		build.Dir = wd
		if combined, buildErr := build.CombinedOutput(); buildErr != nil {
			hopBinary.err = fmt.Errorf("go build ./cmd/hop: %w\n%s", buildErr, combined)
			return
		}
		hopBinary.path = out
	})
	if hopBinary.err != nil {
		t.Fatalf("build hop binary: %v", hopBinary.err)
	}
	return hopBinary.path
}

// callTimeout bounds one hop subcommand invocation in this suite; every
// fixture keeps the exercised command well inside it (no live pane is
// ever awaited).
const callTimeout = 30 * time.Second

// hopResult captures one bounded hop subcommand invocation.
type hopResult struct {
	Stdout   string
	Stderr   string
	ExitCode int
}

// FirstStdoutLine returns Stdout's first line without its terminator: the
// value the section 7 grammar and the Phase 2 command contracts render
// their protocol line into.
func (r hopResult) FirstStdoutLine() string {
	line, _, _ := strings.Cut(r.Stdout, "\n")
	return line
}

// FirstStderrLine returns Stderr's first line without its terminator.
func (r hopResult) FirstStderrLine() string {
	line, _, _ := strings.Cut(r.Stderr, "\n")
	return line
}

// runHop runs the built hop binary with args and dir as its working
// directory, under env (always isolatedHopEnv's output — see below; never
// os.Environ()), bounded by callTimeout. It never fails the test on a
// non-zero exit — the caller decides what a given code means for the verb
// it ran — but failing to even start the process is a hard test failure.
func runHop(t *testing.T, env []string, dir string, args ...string) hopResult {
	t.Helper()
	hopPath := buildHopBinary(t)
	ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, hopPath, args...) //nolint:gosec // G204: the executable is this suite's own build of cmd/hop; args are chosen by the calling test.
	cmd.Env = env
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	result := hopResult{Stdout: stdout.String(), Stderr: stderr.String()}
	var exitErr *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &exitErr):
		result.ExitCode = exitErr.ExitCode()
	default:
		t.Fatalf("run hop %s: %v", strings.Join(args, " "), err)
	}
	return result
}

// closeQuietly closes c, discarding the result: used only where the close
// outcome carries no signal worth a test failure (the herdr canary
// listener and any connection it accepts).
func closeQuietly(c io.Closer) {
	_ = c.Close() //nolint:errcheck // the close result carries no signal here.
}

// herdrCanary is a unix-socket listener that fails the test if anything
// ever connects to it: every exec in this suite points HERDR_SOCKET_PATH
// at one, and no command under test (a store-only refusal or usage-error
// shape, by construction — see each test file's scope comment) should
// ever dial it. Accept runs on its own goroutine; the connection flag is
// checked and reported from t.Cleanup, which still runs while the test is
// considered active, so it is safe to call t.Errorf there (unlike from
// the accept goroutine itself, which could otherwise race a completed
// test).
type herdrCanary struct {
	path     string
	ln       net.Listener
	breached atomic.Bool
}

// newHerdrCanary starts listening at <dir>/herdr.sock, where dir is a
// short-lived directory of its own (never t.TempDir(), whose nested
// subtest path can exceed macOS's ~104-byte unix socket path limit), and
// registers a cleanup that closes the listener, removes dir and fails t
// if anything ever connected.
func newHerdrCanary(t *testing.T) *herdrCanary {
	t.Helper()
	dir, err := os.MkdirTemp("", "hop-canary")
	if err != nil {
		t.Fatalf("create herdr canary dir: %v", err)
	}
	path := filepath.Join(dir, "herdr.sock")
	var listenConfig net.ListenConfig
	ln, err := listenConfig.Listen(context.Background(), "unix", path)
	if err != nil {
		t.Fatalf("listen on herdr canary socket: %v", err)
	}
	c := &herdrCanary{path: path, ln: ln}
	go func() {
		for {
			conn, acceptErr := ln.Accept()
			if acceptErr != nil {
				return
			}
			c.breached.Store(true)
			closeQuietly(conn)
		}
	}()
	t.Cleanup(func() {
		closeQuietly(ln)
		if removeErr := os.RemoveAll(dir); removeErr != nil {
			t.Errorf("remove herdr canary dir: %v", removeErr)
		}
		if c.breached.Load() {
			t.Errorf("HERDR_SOCKET_PATH canary at %s: a command connected to it; this suite must never reach Herdr", path)
		}
	})
	return c
}

// isolatedHopEnv is the one helper every exec in this suite uses to build
// the child process's environment (ruling: an explicit allowlist enforced
// by a helper all tests use). It never inherits the calling process's
// environment: only PATH and TMPDIR (when the host has one) plus hopVars'
// HOP_* entries are carried; HOME is always a fresh t.TempDir(); and
// HERDR_SOCKET_PATH always names a fresh herdrCanary listener — never a
// flag, never a value the caller can steer elsewhere. No ANTHROPIC_*,
// OPENAI_*, CLAUDE_CONFIG_DIR, CODEX_HOME, HOP_LIVE_HARNESS or
// HOP_LIVE_HARNESS_HOME entry is ever present, because nothing beyond
// this allowlist is ever added.
func isolatedHopEnv(t *testing.T, hopVars map[string]string) []string {
	t.Helper()
	home := t.TempDir()
	canary := newHerdrCanary(t)
	env := []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + home,
		"HERDR_SOCKET_PATH=" + canary.path,
	}
	if tmp, ok := os.LookupEnv("TMPDIR"); ok {
		env = append(env, "TMPDIR="+tmp)
	}
	keys := make([]string, 0, len(hopVars))
	for k := range hopVars {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		env = append(env, k+"="+hopVars[k])
	}
	return env
}

// execHop is the convenience wrapper every test in this package calls:
// isolatedHopEnv builds the environment, runHop execs the built binary.
func execHop(t *testing.T, hopVars map[string]string, dir string, args ...string) hopResult {
	t.Helper()
	return runHop(t, isolatedHopEnv(t, hopVars), dir, args...)
}

// freshStateDir returns a fresh, empty absolute directory suitable for
// HOP_STATE_DIR: opening a controller against it auto-creates and
// migrates the SQLite store with zero rows. Every shape reachable against
// an entirely empty store (usage errors, malformed/missing HOP_*
// identities that fail to parse before any store lookup, and a
// well-formed but nonexistent run/session/message/task id) needs nothing
// more than this — no fixture seeding at all.
func freshStateDir(t *testing.T) string {
	t.Helper()
	return t.TempDir()
}

// realDir returns a fresh, real, symlink-resolved absolute directory
// suitable for a `-C` repository-root argument or a launcher working
// directory: cmd/hop's own resolveRepositoryRoot (runcmd.go) and
// launcherWorkerDir (launchcmd.go) both call filepath.EvalSymlinks, which
// fails on a path that does not exist, and on macOS resolves a plain
// t.TempDir() path (under /var) to its /private/var form — so every
// caller that later needs to reproduce this exact directory's resolved
// spelling must resolve it here, once, the identical way.
func realDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("resolve real dir %s: %v", dir, err)
	}
	return resolved
}

// testUUID renders a deterministic, canonical-shaped lowercase UUID from a
// test-chosen number: 8-4-4-4-12 hyphenated hex groups, matching
// internal/domain/identity's ParseXxxID contract exactly, without cmd/hop
// test code importing that package directly (cmd/hop, including its test
// files, is not on the architecture checker's allowlist for
// internal/domain/identity or internal/domain/run — composition code,
// production or test, only ever carries identities as strings). Distinct
// n values never collide within one test's fixture.
func testUUID(n int) string {
	return fmt.Sprintf("00000000-0000-4000-8000-%012d", n)
}

// malformedUUID is a fixed, deliberately invalid identity value: valid
// hex but the wrong group width, so every ParseXxxID in internal/domain/identity
// rejects it the same way an empty or garbled value would.
const malformedUUID = "not-a-valid-uuid"
