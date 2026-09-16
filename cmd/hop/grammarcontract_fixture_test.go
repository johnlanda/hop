package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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
// suite if anything ever connects to it (herdrCanary below). No exception
// exists: the Go settings the build needs are resolved in-process
// (goBuildVars), never by running anything. Nothing in this file starts a
// Herdr server or lives under test/integration; the real SQLite adapter
// is the only production dependency exercised.

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
// settings goBuildVars resolves in-process.
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

// buildTimeout bounds the one go build.
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
	goVars, err := goBuildVars(os.LookupEnv)
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

// goBuildVars resolves, without executing anything, the Go settings the
// isolated build needs so its caches stay warm and no module is fetched:
// GOCACHE, GOPATH and GOMODCACHE from the test process's environment when
// set there, otherwise the go command's documented defaults computed
// in-process (os.UserCacheDir()/go-build; the home directory's "go";
// the first GOPATH entry's pkg/mod), plus GOTOOLCHAIN and GOFLAGS exactly
// when the environment sets them (make exports GOTOOLCHAIN from go.mod).
// Settings persisted with `go env -w` are not consulted: the build's
// temporary HOME hides that file, and reading it would mean running go
// with the operator's HOME. lookup is os.LookupEnv in production.
func goBuildVars(lookup func(string) (string, bool)) (map[string]string, error) {
	set := func(key string) (string, bool) {
		value, ok := lookup(key)
		return value, ok && value != ""
	}
	vars := map[string]string{}
	gocache, ok := set("GOCACHE")
	if !ok {
		cacheDir, err := os.UserCacheDir()
		if err != nil {
			return nil, fmt.Errorf("default GOCACHE: %w", err)
		}
		gocache = filepath.Join(cacheDir, "go-build")
	}
	gopath, ok := set("GOPATH")
	if !ok {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("default GOPATH: %w", err)
		}
		gopath = filepath.Join(home, "go")
	}
	gomodcache, ok := set("GOMODCACHE")
	if !ok {
		gomodcache = filepath.Join(filepath.SplitList(gopath)[0], "pkg", "mod")
	}
	vars["GOCACHE"], vars["GOPATH"], vars["GOMODCACHE"] = gocache, gopath, gomodcache
	for _, key := range []string{"GOCACHE", "GOMODCACHE"} {
		if !filepath.IsAbs(vars[key]) {
			return nil, fmt.Errorf("%s is not an absolute path", key)
		}
	}
	for _, entry := range filepath.SplitList(gopath) {
		if !filepath.IsAbs(entry) {
			return nil, errors.New("a GOPATH entry is not an absolute path")
		}
	}
	for _, key := range []string{"GOTOOLCHAIN", "GOFLAGS"} {
		if value, ok := set(key); ok {
			vars[key] = value
		}
	}
	return vars, nil
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

// Canary placement: a unix socket path must fit the platform's
// sockaddr_un (sun_path holds the path and its terminating NUL — 104
// bytes on darwin, 108 on linux), and os.MkdirTemp appends up to ten
// random digits to the directory pattern.
const (
	canaryDirPattern   = "hop-canary"
	canarySocketName   = "herdr.sock"
	canaryLongestTag   = "4294967295"
	shortCanaryBaseDir = "/tmp"
)

// maxUnixSocketPathLen is the longest socket path this platform binds.
func maxUnixSocketPathLen() int {
	if runtime.GOOS == "darwin" {
		return 103
	}
	return 107
}

// canaryBase returns the directory a canary's own directory is created
// under: tmp (the process temporary directory) when even MkdirTemp's
// longest name keeps the socket path within the platform limit, else the
// short fixed base, so a long TMPDIR never makes the bind fail.
func canaryBase(tmp string) string {
	if len(filepath.Join(tmp, canaryDirPattern+canaryLongestTag, canarySocketName)) <= maxUnixSocketPathLen() {
		return tmp
	}
	return shortCanaryBaseDir
}

// startHerdrCanary starts listening at <dir>/herdr.sock, where dir is a
// short-lived directory of its own under canaryBase (never t.TempDir(),
// whose nested subtest path can exceed the socket path limit); stop
// removes it.
func startHerdrCanary() (*herdrCanary, error) {
	dir, err := os.MkdirTemp(canaryBase(os.TempDir()), canaryDirPattern)
	if err != nil {
		return nil, fmt.Errorf("create herdr canary dir: %w", err)
	}
	path := filepath.Join(dir, canarySocketName)
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
		vars, err := goBuildVars(os.LookupEnv)
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
		env, err := isolatedEnvironment(home, socket, "", vars)
		if err != nil {
			t.Fatal(err)
		}
		check(t, env, allowlistedKeys(names...))
		if envValue(env, envHome) == os.Getenv(envHome) {
			t.Error("the build environment carries the operator's HOME")
		}
	})

	t.Run("go build settings are resolved in-process", func(t *testing.T) {
		cacheDir, err := os.UserCacheDir()
		if err != nil {
			t.Skipf("no user cache dir: %v", err)
		}
		userHome, err := os.UserHomeDir()
		if err != nil {
			t.Skipf("no user home dir: %v", err)
		}
		lookupFrom := func(values map[string]string) func(string) (string, bool) {
			return func(key string) (string, bool) {
				value, ok := values[key]
				return value, ok
			}
		}
		defaults, err := goBuildVars(lookupFrom(map[string]string{"GOTOOLCHAIN": "", "GOFLAGS": ""}))
		if err != nil {
			t.Fatal(err)
		}
		want := map[string]string{
			"GOCACHE":    filepath.Join(cacheDir, "go-build"),
			"GOPATH":     filepath.Join(userHome, "go"),
			"GOMODCACHE": filepath.Join(userHome, "go", "pkg", "mod"),
		}
		if !maps.Equal(defaults, want) {
			t.Errorf("defaults = %v, want %v", defaults, want)
		}
		explicit, err := goBuildVars(lookupFrom(map[string]string{
			"GOCACHE": "/cache", "GOPATH": "/first" + string(os.PathListSeparator) + "/second",
			"GOTOOLCHAIN": "go1.27.1", "GOFLAGS": "-mod=mod",
		}))
		if err != nil {
			t.Fatal(err)
		}
		want = map[string]string{
			"GOCACHE": "/cache", "GOPATH": "/first" + string(os.PathListSeparator) + "/second",
			"GOMODCACHE": "/first/pkg/mod", "GOTOOLCHAIN": "go1.27.1", "GOFLAGS": "-mod=mod",
		}
		if !maps.Equal(explicit, want) {
			t.Errorf("explicit = %v, want %v", explicit, want)
		}
		if _, err := goBuildVars(lookupFrom(map[string]string{"GOCACHE": "relative"})); err == nil {
			t.Error("a relative GOCACHE was accepted")
		}
	})
}

// TestCanaryBaseFitsTheSocketLimit proves canary placement never produces
// a socket path the platform refuses to bind: a temporary directory whose
// longest canary socket path fits is used as is, and a longer one falls
// back to the short fixed base — whose own longest path fits.
func TestCanaryBaseFitsTheSocketLimit(t *testing.T) {
	longest := func(base string) int {
		return len(filepath.Join(base, canaryDirPattern+canaryLongestTag, canarySocketName))
	}
	if got := canaryBase(shortCanaryBaseDir); got != shortCanaryBaseDir {
		t.Errorf("canaryBase(%q) = %q, want it kept", shortCanaryBaseDir, got)
	}
	if longest(shortCanaryBaseDir) > maxUnixSocketPathLen() {
		t.Fatalf("the short base's longest socket path (%d bytes) exceeds the limit %d", longest(shortCanaryBaseDir), maxUnixSocketPathLen())
	}
	long := filepath.Join(t.TempDir(), strings.Repeat("deliberately-long-tmpdir-", 6))
	if longest(long) <= maxUnixSocketPathLen() {
		t.Fatalf("fixture base is not long enough: %d bytes", longest(long))
	}
	if got := canaryBase(long); got != shortCanaryBaseDir {
		t.Errorf("canaryBase(long) = %q, want the short base %q", got, shortCanaryBaseDir)
	}
	if got := canaryBase(os.TempDir()); longest(got) > maxUnixSocketPathLen() {
		t.Errorf("canaryBase(os.TempDir()) = %q, whose longest socket path exceeds the limit", got)
	}
}
