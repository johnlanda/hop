package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
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
// No Herdr binary and no network: every subprocess this suite starts —
// the hop binary under test, the one go build and the fixture git
// commands — runs under isolatedEnvironment, which never inherits the
// calling process's environment, always sets a fresh temporary HOME and
// always points HERDR_SOCKET_PATH at a canary listener that fails the
// suite if anything ever connects to it (herdrCanary below). The one
// exception is the read-only `go env` query that locates the operator's
// Go caches (goBuildVars). Nothing in this file starts a Herdr server or
// lives under test/integration; the real SQLite adapter is the only
// production dependency exercised.

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
// root — no module-root discovery is needed. The build runs under
// isolatedEnvironment (a HOME of its own inside the build directory, its
// own canary listener, checked once the build returns) with only the Go
// settings goBuildVars resolves.
func buildHopBinary(t *testing.T) string {
	t.Helper()
	hopBinary.once.Do(func() {
		dir, err := os.MkdirTemp("", "hop-grammar-contract")
		if err != nil {
			hopBinary.err = err
			return
		}
		hopBinary.dir = dir
		hopBinary.path, hopBinary.err = buildInto(dir)
	})
	if hopBinary.err != nil {
		t.Fatalf("build hop binary: %v", hopBinary.err)
	}
	return hopBinary.path
}

// buildTimeout bounds the one go build and its go env query.
const buildTimeout = 2 * time.Minute

// buildInto builds cmd/hop into dir/hop under the isolated build
// environment and returns the executable's path.
func buildInto(dir string) (string, error) {
	goBin, err := exec.LookPath("go")
	if err != nil {
		return "", fmt.Errorf("the go tool is required to build cmd/hop: %w", err)
	}
	wd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("resolve cmd/hop source directory: %w", err)
	}
	home := filepath.Join(dir, "home")
	if err = os.Mkdir(home, 0o700); err != nil {
		return "", fmt.Errorf("create the build HOME: %w", err)
	}
	canary, err := startHerdrCanary()
	if err != nil {
		return "", err
	}
	defer canary.stop() //nolint:errcheck // best-effort removal of the build canary; a breach is reported below.

	ctx, cancel := context.WithTimeout(context.Background(), buildTimeout)
	defer cancel()
	goVars, err := goBuildVars(ctx, goBin, canary.path)
	if err != nil {
		return "", err
	}
	env, err := isolatedEnvironment(home, canary.path, "", goVars)
	if err != nil {
		return "", err
	}
	out := filepath.Join(dir, "hop")
	build := exec.CommandContext(ctx, goBin, "build", "-o", out, ".") //nolint:gosec // G204: the go tool builds this repository's own command with a fixed argument list.
	build.Dir = wd
	build.Env = env
	combined, buildErr := build.CombinedOutput()
	if canary.breached.Load() {
		return "", errors.New("the go build connected to its HERDR_SOCKET_PATH canary")
	}
	if buildErr != nil {
		return "", fmt.Errorf("go build ./cmd/hop: %w\n%s", buildErr, combined)
	}
	return out, nil
}

// goBuildVars resolves the Go settings the isolated build needs, once:
// GOCACHE, GOMODCACHE and GOPATH from the operator's own `go env` (so the
// build cache stays warm and no module is fetched), plus GOTOOLCHAIN and
// GOFLAGS exactly when the operator's environment sets them (make exports
// GOTOOLCHAIN from go.mod). The `go env` query is the suite's one
// subprocess that sees the operator's real HOME — Go locates its
// configuration file and default caches from it — and it is still an
// explicit allowlist (goEnvQueryKeys) with the canary socket: a read-only
// configuration query that builds and runs nothing.
func goBuildVars(ctx context.Context, goBin, herdrSocket string) (map[string]string, error) {
	query := exec.CommandContext(ctx, goBin, "env", "-json", "GOCACHE", "GOMODCACHE", "GOPATH")
	query.Env = goEnvQueryEnvironment(herdrSocket)
	raw, err := query.Output()
	if err != nil {
		return nil, fmt.Errorf("go env: %w", err)
	}
	var resolved map[string]string
	if err := json.Unmarshal(raw, &resolved); err != nil {
		return nil, fmt.Errorf("decode go env: %w", err)
	}
	vars := map[string]string{}
	for _, key := range []string{"GOCACHE", "GOMODCACHE", "GOPATH"} {
		value := resolved[key]
		if !filepath.IsAbs(value) {
			return nil, fmt.Errorf("go env %s is not an absolute path", key)
		}
		vars[key] = value
	}
	for _, key := range []string{"GOTOOLCHAIN", "GOFLAGS"} {
		if value, ok := os.LookupEnv(key); ok && value != "" {
			vars[key] = value
		}
	}
	return vars, nil
}

// goEnvQueryKeys are the calling process's variables the `go env` query
// may see: where Go finds its configuration and caches, and nothing else.
func goEnvQueryKeys() []string {
	return []string{"PATH", "HOME", "TMPDIR", "XDG_CONFIG_HOME", "XDG_CACHE_HOME", "GOENV", "GOROOT", "GOPATH", "GOCACHE", "GOMODCACHE", "GOTOOLCHAIN", "GOFLAGS"}
}

// goEnvQueryEnvironment builds the `go env` query's environment from
// goEnvQueryKeys and the canary socket.
func goEnvQueryEnvironment(herdrSocket string) []string {
	env := []string{envHerdrSocket + "=" + herdrSocket}
	for _, key := range goEnvQueryKeys() {
		if value, ok := os.LookupEnv(key); ok {
			env = append(env, key+"="+value)
		}
	}
	return env
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

// herdrCanary is a unix-socket listener that records any connection:
// every subprocess in this suite points HERDR_SOCKET_PATH at one, and
// nothing it runs (a store-only refusal or usage-error shape, by
// construction — see each test file's scope comment — or the build and
// fixture git commands) should ever dial it. Accept runs on its own
// goroutine; a test's canary is checked and reported from t.Cleanup,
// which still runs while the test is considered active, so it is safe to
// call t.Errorf there (unlike from the accept goroutine itself, which
// could otherwise race a completed test).
type herdrCanary struct {
	dir      string
	path     string
	ln       net.Listener
	breached atomic.Bool
}

// startHerdrCanary starts listening at <dir>/herdr.sock, where dir is a
// short-lived directory of its own (never t.TempDir(), whose nested
// subtest path can exceed macOS's ~104-byte unix socket path limit).
func startHerdrCanary() (*herdrCanary, error) {
	dir, err := os.MkdirTemp("", "hop-canary")
	if err != nil {
		return nil, fmt.Errorf("create herdr canary dir: %w", err)
	}
	path := filepath.Join(dir, "herdr.sock")
	var listenConfig net.ListenConfig
	ln, err := listenConfig.Listen(context.Background(), "unix", path)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("listen on herdr canary socket: %w", err), os.RemoveAll(dir))
	}
	c := &herdrCanary{dir: dir, path: path, ln: ln}
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
	return c, nil
}

// stop closes the listener and removes its directory.
func (c *herdrCanary) stop() error {
	closeQuietly(c.ln)
	return os.RemoveAll(c.dir)
}

// newHerdrCanary starts a canary for one test and registers a cleanup
// that stops it and fails t if anything ever connected.
func newHerdrCanary(t *testing.T) *herdrCanary {
	t.Helper()
	c, err := startHerdrCanary()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if stopErr := c.stop(); stopErr != nil {
			t.Errorf("remove herdr canary dir: %v", stopErr)
		}
		if c.breached.Load() {
			t.Errorf("HERDR_SOCKET_PATH canary at %s: a command connected to it; this suite must never reach Herdr", c.path)
		}
	})
	return c
}

// The keys isolatedEnvironment fixes itself; a caller's vars may never
// name one.
const (
	envPath        = "PATH"
	envHome        = "HOME"
	envTmpDir      = "TMPDIR"
	envHerdrSocket = "HERDR_SOCKET_PATH"
)

// isolatedEnvironment is the ONE constructor for every subprocess
// environment in this suite (ruling: an explicit allowlist enforced by a
// helper every exec uses) — the hop binary under test, the go build and
// the fixture git commands. It never inherits the calling process's
// environment: it carries PATH (behind pathPrefix, when one is given) and
// TMPDIR (when the host has one) from the calling process, HOME as home,
// HERDR_SOCKET_PATH as herdrSocket, and vars — nothing else. vars may not
// name any of those four keys, so a caller can never steer HOME or the
// canary elsewhere (a PATH change goes through pathPrefix alone). No
// ANTHROPIC_*, OPENAI_*, CLAUDE_CONFIG_DIR, CODEX_HOME, HOP_LIVE_HARNESS
// or other operator entry is ever present unless a caller names it.
func isolatedEnvironment(home, herdrSocket, pathPrefix string, vars map[string]string) ([]string, error) {
	if !filepath.IsAbs(home) || !filepath.IsAbs(herdrSocket) {
		return nil, errors.New("isolated environment: HOME and the canary socket must be absolute paths")
	}
	if pathPrefix != "" && !filepath.IsAbs(pathPrefix) {
		return nil, errors.New("isolated environment: the PATH prefix must be an absolute directory")
	}
	keys := make([]string, 0, len(vars))
	for key := range vars {
		switch key {
		case envPath, envHome, envTmpDir, envHerdrSocket:
			return nil, fmt.Errorf("isolated environment: %s is fixed and cannot be overridden", key)
		}
		if key == "" || strings.ContainsAny(key, "=\x00") {
			return nil, errors.New("isolated environment: a variable name is empty or malformed")
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)
	path := os.Getenv(envPath)
	if pathPrefix != "" {
		path = pathPrefix + string(os.PathListSeparator) + path
	}
	env := []string{envPath + "=" + path, envHome + "=" + home, envHerdrSocket + "=" + herdrSocket}
	if tmp, ok := os.LookupEnv(envTmpDir); ok {
		env = append(env, envTmpDir+"="+tmp)
	}
	for _, key := range keys {
		env = append(env, key+"="+vars[key])
	}
	return env, nil
}

// isolatedTestEnv builds one test's subprocess environment: a fresh
// t.TempDir() HOME and a fresh herdrCanary.
func isolatedTestEnv(t *testing.T, pathPrefix string, vars map[string]string) []string {
	t.Helper()
	env, err := isolatedEnvironment(t.TempDir(), newHerdrCanary(t).path, pathPrefix, vars)
	if err != nil {
		t.Fatal(err)
	}
	return env
}

// isolatedHopEnv builds a hop invocation's environment: the host PATH and
// hopVars' HOP_* entries on top of the fixed allowlist.
func isolatedHopEnv(t *testing.T, hopVars map[string]string) []string {
	t.Helper()
	return isolatedTestEnv(t, "", hopVars)
}

// execHop is the convenience wrapper every test in this package calls:
// isolatedHopEnv builds the environment, runHop execs the built binary.
func execHop(t *testing.T, hopVars map[string]string, dir string, args ...string) hopResult {
	t.Helper()
	return runHop(t, isolatedHopEnv(t, hopVars), dir, args...)
}

// fixtureGitVars are the fixture git commands' variables beyond the
// allowlist: a fixed author/committer identity, and no system-wide git
// configuration (HOME is already a fresh directory, so no user
// configuration is read either).
func fixtureGitVars() map[string]string {
	return map[string]string{
		"GIT_AUTHOR_NAME": "hop-fixture", "GIT_AUTHOR_EMAIL": "hop-fixture@example.invalid",
		"GIT_COMMITTER_NAME": "hop-fixture", "GIT_COMMITTER_EMAIL": "hop-fixture@example.invalid",
		"GIT_CONFIG_NOSYSTEM": "1",
	}
}

// runFixtureGit runs one git command in dir under the isolated fixture
// environment, bounded by callTimeout, and returns its trimmed output.
func runFixtureGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...) //nolint:gosec // G204: a fixed git invocation against this test's own throwaway repository.
	cmd.Dir = dir
	cmd.Env = isolatedTestEnv(t, "", fixtureGitVars())
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
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

// envKeys returns env's variable names, sorted.
func envKeys(env []string) []string {
	keys := make([]string, 0, len(env))
	for _, entry := range env {
		key, _, _ := strings.Cut(entry, "=")
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// envValue returns key's value in env.
func envValue(env []string, key string) string {
	for _, entry := range env {
		if k, v, ok := strings.Cut(entry, "="); ok && k == key {
			return v
		}
	}
	return ""
}

// allowlistedKeys returns the fixed allowlist's keys plus extra, sorted
// the way envKeys sorts them.
func allowlistedKeys(extra ...string) []string {
	keys := append([]string{envPath, envHome, envHerdrSocket}, extra...)
	if _, ok := os.LookupEnv(envTmpDir); ok {
		keys = append(keys, envTmpDir)
	}
	sort.Strings(keys)
	return keys
}

// TestIsolatedEnvironment pins the one subprocess-environment constructor
// every exec in this suite uses: a caller can override none of the fixed
// keys, and the hop, fixture-git and build environments each carry
// exactly the allowlist plus their own named variables — never an
// operator variable, never the operator's HOME or Herdr socket.
func TestIsolatedEnvironment(t *testing.T) {
	const operatorCanary = "not-a-real-credential-canary"
	t.Setenv("ANTHROPIC_API_KEY", operatorCanary)
	t.Setenv("HOP_LIVE_HARNESS", operatorCanary)
	t.Setenv(envHerdrSocket, "/operator/"+operatorCanary+"/herdr.sock")
	home, socket := t.TempDir(), filepath.Join(t.TempDir(), "herdr.sock")

	t.Run("fixed keys cannot be overridden", func(t *testing.T) {
		for _, key := range []string{envHome, envHerdrSocket, envPath, envTmpDir} {
			if env, err := isolatedEnvironment(home, socket, "", map[string]string{key: "/elsewhere"}); err == nil {
				t.Errorf("an override of %s was accepted: %q", key, env)
			}
		}
		for _, bad := range []map[string]string{{"": "x"}, {"A=B": "x"}} {
			if _, err := isolatedEnvironment(home, socket, "", bad); err == nil {
				t.Errorf("a malformed variable name %q was accepted", bad)
			}
		}
		if _, err := isolatedEnvironment("relative", socket, "", nil); err == nil {
			t.Error("a relative HOME was accepted")
		}
		if _, err := isolatedEnvironment(home, socket, "relative", nil); err == nil {
			t.Error("a relative PATH prefix was accepted")
		}
	})

	check := func(t *testing.T, env, wantKeys []string) {
		t.Helper()
		if got := envKeys(env); strings.Join(got, ",") != strings.Join(wantKeys, ",") {
			t.Errorf("keys = %v, want %v", got, wantKeys)
		}
		if envValue(env, envHome) != home || envValue(env, envHerdrSocket) != socket {
			t.Errorf("HOME/HERDR_SOCKET_PATH = %q/%q, want %q/%q", envValue(env, envHome), envValue(env, envHerdrSocket), home, socket)
		}
		for _, entry := range env {
			if strings.Contains(entry, operatorCanary) {
				t.Errorf("an operator value leaked: %q", entry)
			}
		}
	}

	t.Run("hop invocation", func(t *testing.T) {
		env, err := isolatedEnvironment(home, socket, "", map[string]string{"HOP_STATE_DIR": "/state"})
		if err != nil {
			t.Fatal(err)
		}
		check(t, env, allowlistedKeys("HOP_STATE_DIR"))
		if envValue(env, envPath) != os.Getenv(envPath) {
			t.Errorf("PATH = %q, want the host PATH", envValue(env, envPath))
		}
	})

	t.Run("PATH prefix goes first", func(t *testing.T) {
		prefix := t.TempDir()
		env, err := isolatedEnvironment(home, socket, prefix, nil)
		if err != nil {
			t.Fatal(err)
		}
		check(t, env, allowlistedKeys())
		if want := prefix + string(os.PathListSeparator) + os.Getenv(envPath); envValue(env, envPath) != want {
			t.Errorf("PATH = %q, want %q", envValue(env, envPath), want)
		}
	})

	t.Run("fixture git", func(t *testing.T) {
		env, err := isolatedEnvironment(home, socket, "", fixtureGitVars())
		if err != nil {
			t.Fatal(err)
		}
		gitKeys := make([]string, 0, len(fixtureGitVars()))
		for key := range fixtureGitVars() {
			gitKeys = append(gitKeys, key)
		}
		check(t, env, allowlistedKeys(gitKeys...))
		if envValue(env, "GIT_CONFIG_NOSYSTEM") != "1" {
			t.Error("the fixture git environment reads the system git configuration")
		}
	})

	t.Run("go build", func(t *testing.T) {
		goBin, err := exec.LookPath("go")
		if err != nil {
			t.Skip("the go tool is not on PATH")
		}
		ctx, cancel := context.WithTimeout(context.Background(), buildTimeout)
		defer cancel()
		vars, err := goBuildVars(ctx, goBin, socket)
		if err != nil {
			t.Fatalf("goBuildVars: %v", err)
		}
		var names []string
		for key := range vars {
			switch key {
			case "GOCACHE", "GOMODCACHE", "GOPATH", "GOTOOLCHAIN", "GOFLAGS":
				names = append(names, key)
			default:
				t.Errorf("the build carries %s, outside the Go allowlist", key)
			}
		}
		for _, required := range []string{"GOCACHE", "GOMODCACHE", "GOPATH"} {
			if vars[required] == "" {
				t.Errorf("the build lacks %s; its cache would go cold", required)
			}
		}
		env, err := isolatedEnvironment(home, socket, "", vars)
		if err != nil {
			t.Fatal(err)
		}
		check(t, env, allowlistedKeys(names...))
		for _, entry := range goEnvQueryEnvironment(socket) {
			key, _, _ := strings.Cut(entry, "=")
			if key != envHerdrSocket && !slices.Contains(goEnvQueryKeys(), key) {
				t.Errorf("the go env query carries %s, outside its allowlist", key)
			}
			if strings.Contains(entry, operatorCanary) {
				t.Errorf("the go env query carries an operator value: %q", entry)
			}
		}
	})
}
