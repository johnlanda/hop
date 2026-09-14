package integration

import (
	"bytes"
	"context"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/creack/pty"

	"github.com/johnlanda/hop/internal/adapters/herdr"
	"github.com/johnlanda/hop/internal/app"
)

// ptyRows and ptyCols are the fixed client dimensions. They match the test
// config's headless geometry so the smoke is repeatable.
const (
	ptyRows = 30
	ptyCols = 100
)

// TestRealProcessPTYRendering attaches a real terminal client to the test
// server through a pseudo-terminal at fixed dimensions and captures how the
// native Agents sidebar renders HOP's projection. It is the rendering
// evidence the API-level presentation test cannot give: setting a view proves
// acceptance, not what a client draws. It asserts the three fixture agents
// render, that HOP's manager-first projection orders the manager before the
// workers on screen, and that a HOP metadata token is drawn in the rows.
func TestRealProcessPTYRendering(t *testing.T) {
	artifacts := newArtifactDir(t)
	server := prepareServer(t, artifacts)
	stage := stagePlugin(t)
	server.start(t)
	ctx := testContext(t)

	var linked struct {
		Plugin struct {
			PluginID string `json:"plugin_id"`
		} `json:"plugin"`
	}
	server.call(t, "plugin.link", map[string]any{"path": stage, "enabled": true}, &linked)

	fixture := server.createRunFixture(t)
	presenter := &app.Presenter{Presentation: herdr.NewPresentation(server.socketPath)}
	for _, display := range []app.AgentDisplay{
		{PaneID: fixture.manager, Run: "r1", RunSequence: 1, Role: app.RoleManager, State: "planning"},
		{PaneID: fixture.implementer, Run: "r1", RunSequence: 1, Role: app.RoleImplementer, WorkerSequence: 1, Task: "retry policy", ParentLabel: "manager-r1"},
		{PaneID: fixture.reviewer, Run: "r1", RunSequence: 1, Role: app.RoleReviewer, WorkerSequence: 2, Task: "review", ParentLabel: "manager-r1"},
	} {
		if err := presenter.Publish(ctx, &display); err != nil {
			t.Fatalf("publish metadata: %v", err)
		}
	}
	if err := presenter.Focus(ctx, "r1", "HOP r1"); err != nil {
		t.Fatalf("focus view: %v", err)
	}

	client := server.attachPTYClient(t)
	defer client.close(t)

	// The narrow sidebar truncates long entries (e.g. "implementer" renders
	// as "implement…"), so assertions use markers that survive truncation:
	// the manager and reviewer agent names, the implementer's unique task
	// token, and the HOP view label.
	const (
		managerMark     = "manager"
		implementerMark = "retry policy"
		reviewerMark    = "reviewer"
		viewLabel       = "HOP r1"
	)
	rendered := client.waitForScreen(t, func(screen string) bool {
		return strings.Contains(screen, viewLabel) &&
			strings.Contains(screen, managerMark) &&
			strings.Contains(screen, implementerMark) &&
			strings.Contains(screen, reviewerMark)
	})
	artifacts.save(t, "pty-screen.txt", rendered)
	artifacts.save(t, "pty-screen-raw.txt", client.rawSnapshot())

	// Manager-first: under HOP's projection the manager row is drawn above
	// the workers, and the workers follow in sequence (implementer then
	// reviewer).
	managerAt := strings.Index(rendered, managerMark)
	implementerAt := strings.Index(rendered, implementerMark)
	reviewerAt := strings.Index(rendered, reviewerMark)
	if managerAt >= implementerAt || implementerAt >= reviewerAt {
		t.Errorf("rendered order places manager at %d, implementer at %d, reviewer at %d; want manager-first then worker sequence\n%s",
			managerAt, implementerAt, reviewerAt, rendered)
	}

	// The HOP view label is drawn, evidence the projection reached the client.
	if !strings.Contains(rendered, viewLabel) {
		t.Errorf("rendered sidebar does not draw the HOP view label %q:\n%s", viewLabel, rendered)
	}

	// Owned view/clear: after HOP clears its own view the client keeps
	// rendering the agents (the projection is a filter/sort over the same
	// agents, not their existence). This is captured as evidence.
	if err := presenter.Clear(ctx); err != nil {
		t.Fatalf("clear view: %v", err)
	}
	afterClear := client.waitForScreen(t, func(screen string) bool {
		return strings.Contains(screen, "manager")
	})
	artifacts.save(t, "pty-screen-after-clear.txt", afterClear)
}

// ptyClient is an attached terminal client driven through a pseudo-terminal.
type ptyClient struct {
	cmd *exec.Cmd
	tty interface {
		Read([]byte) (int, error)
		Close() error
	}

	mu     sync.Mutex
	output bytes.Buffer
}

// attachPTYClient starts `herdr --session <name>` on a pseudo-terminal at the
// fixed dimensions and streams its output into a buffer. The client uses the
// same hermetic environment as every other subprocess, so it can only address
// the disposable test session.
func (s *testServer) attachPTYClient(t *testing.T) *ptyClient {
	t.Helper()
	cmd := exec.CommandContext(context.Background(), s.herdrBin, "--session", sessionName) //nolint:gosec // G204: the binary is the pinned or PATH-resolved herdr under test. The context is deliberately unbounded: close owns the client teardown.
	cmd.Env = s.environ()
	file, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: ptyRows, Cols: ptyCols})
	if err != nil {
		t.Fatalf("attach pty client: %v", err)
	}
	client := &ptyClient{cmd: cmd, tty: file}
	go client.stream()
	return client
}

// stream copies the client's terminal output into the buffer until the
// pseudo-terminal closes.
func (c *ptyClient) stream() {
	buffer := make([]byte, 4096)
	for {
		n, err := c.tty.Read(buffer)
		if n > 0 {
			c.mu.Lock()
			c.output.Write(buffer[:n])
			c.mu.Unlock()
		}
		if err != nil {
			return
		}
	}
}

// rawSnapshot returns the raw bytes captured so far.
func (c *ptyClient) rawSnapshot() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.output.String()
}

// screen returns the captured output with terminal escape sequences removed,
// so substring and ordering checks read the rendered text.
func (c *ptyClient) screen() string {
	return stripTerminalEscapes(c.rawSnapshot())
}

// waitForScreen polls the rendered screen until it satisfies want, then
// returns it; it fails on timeout with the last screen.
func (c *ptyClient) waitForScreen(t *testing.T, want func(string) bool) string {
	t.Helper()
	var screen string
	found := waitUntil(func() bool {
		screen = c.screen()
		return want(screen)
	})
	if !found {
		t.Fatalf("client never rendered the expected screen; last render:\n%s", screen)
	}
	return screen
}

// close ends the client process and its pseudo-terminal. Only this test's own
// client is signaled.
func (c *ptyClient) close(t *testing.T) {
	t.Helper()
	if err := c.cmd.Process.Kill(); err != nil {
		t.Logf("kill pty client: %v", err)
	}
	if err := c.cmd.Wait(); err != nil {
		// A killed process reports a signal error; that is expected here.
		t.Logf("pty client exited: %v", err)
	}
	if err := c.tty.Close(); err != nil {
		t.Logf("close pty: %v", err)
	}
}

// terminalEscape matches the escape sequences a full-screen TUI emits: OSC
// strings (ESC ] … BEL or ST), single-byte C1 escapes (ESC + 0x40–0x5f), and
// CSI sequences (ESC [ params intermediates final). Hex ranges are used so
// the intent is explicit. Reducing a frame to its visible text is enough for
// substring and ordering assertions.
var terminalEscape = regexp.MustCompile(`\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)|\x1b[\x40-\x5f]|\x1b\[[0-9;?]*[\x20-\x2f]*[\x40-\x7e]`)

// stripTerminalEscapes removes terminal escape sequences and normalizes the
// remaining control bytes to spaces so text drawn on different rows stays
// separated.
func stripTerminalEscapes(raw string) string {
	text := terminalEscape.ReplaceAllString(raw, "")
	var b strings.Builder
	for _, r := range text {
		switch {
		case r == '\n' || r == '\t':
			b.WriteRune(r)
		case r < 0x20 || r == 0x7f:
			b.WriteByte(' ')
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}
