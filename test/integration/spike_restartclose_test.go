package integration

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/johnlanda/hop/internal/adapters/herdr"
	"github.com/johnlanda/hop/internal/app"
)

// standInAgentName is the native-agent name this probe reports a session
// for, and therefore the program Herdr's own deferred restore runs after a
// restart. It is deliberately NOT claude, codex or opencode: all three are
// really installed on a developer machine (an executable in a directory
// /etc/paths.d puts on PATH), and a restored pane's login shell runs
// path_helper, which places those directories AHEAD of the inherited
// hermetic PATH — so a restore plan naming one of them would execute the
// operator's real harness. Nothing on the machine is named standInAgentName
// (requireNoInstalledAgent asserts it and skips otherwise), so the only
// program that name can resolve to is this probe's own recorder. The
// mechanism under test is agent-independent: Herdr's deferred restore
// shell-quotes the plan's argv and types it into the restored shell
// (repos/herdr/src/app/agent_resume.rs, start_pending_agent_resume), and
// the plan for every agent is built by the same crate::agent_resume::plan.
const standInAgentName = "droid"

// standInAgentSource is the reserved native-state source paired with
// standInAgentName; the pair must satisfy Herdr's is_official_agent_source
// for a resume plan to be built at all.
const standInAgentSource = "herdr:droid"

// standInRecorderSource is the stand-in agent, rendered with this test's
// own absolute paths (marker, records directory, gate): it records one line
// per invocation into a single append-only marker file, dumps its complete
// environment to a pid-named file, and then holds until its gate file
// appears — so a fired restore is durable positive evidence AND stays the
// restored pane's live foreground occupant for inspection. The paths are
// baked in rather than read from the environment, because the environment a
// restored pane's shell carries is itself one of this probe's findings. The
// gate is the same release discipline openGatedPane uses for a pane command.
//
// It is COMPILED rather than written as a shell script, and that is the
// whole point of the extra build: a script's argv[0] is the interpreter, so
// a restored script reports argv ["/bin/sh" "<path>" "--resume" "<ref>"] and
// MatchRestoredHarness — which reads filepath.Base(argv[0]) — would not
// recognize it. Production's harness is an executable, not a script (the
// installed claude is a Mach-O binary), so a bare name typed by Herdr's
// restore yields argv ["<name>" "--resume" "<ref>"]. Compiling the stand-in
// is what makes this probe's observed argv carry production's shape, so the
// predicate the close rule depends on is exercised here rather than
// inferred from another test.
const standInRecorderSource = `package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	marker  = %q
	records = %q
	gate    = %q
)

func main() {
	var line strings.Builder
	fmt.Fprintf(&line, "invocation pid=%%d ppid=%%d argv:", os.Getpid(), os.Getppid())
	for _, arg := range os.Args[1:] {
		fmt.Fprintf(&line, " [%%s]", arg)
	}
	line.WriteString("\n")
	// One O_APPEND write, so concurrent invocations cannot interleave.
	if file, err := os.OpenFile(marker, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o600); err == nil {
		fmt.Fprint(file, line.String())
		file.Close()
	}
	environ := os.Environ()
	sort.Strings(environ)
	dump := []byte(strings.Join(environ, "\n") + "\n")
	path := filepath.Join(records, fmt.Sprintf("env-%%d.txt", os.Getpid()))
	if err := os.WriteFile(path+".tmp", dump, 0o600); err == nil {
		os.Rename(path+".tmp", path)
	}
	for {
		if _, err := os.Stat(gate); err == nil {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
}
`

// standInRecorder is the installed stand-in agent and the evidence it
// writes.
type standInRecorder struct {
	// Dir is the directory the executable lives in, first on the pane
	// shell's PATH.
	Dir string
	// Marker is the append-only file every invocation records one line in.
	Marker string
	// Records is the directory the per-pid environment dumps land in.
	Records string
	// Gate releases every recorded invocation.
	Gate string
}

// requireNoInstalledAgent skips the test unless the agent name resolves to
// nothing on the machine's own system PATH — the one condition that makes
// running a restore plan for it provably safe. The lookup uses the system
// PATH path_helper would build (every directory in /etc/paths and
// /etc/paths.d), not this process's PATH, because the pane's login shell
// resolves against the former.
func requireNoInstalledAgent(t *testing.T, name string) {
	t.Helper()
	var dirs []string
	read := func(path string) {
		content, err := os.ReadFile(path) //nolint:gosec // G304: fixed OS-owned path-configuration files.
		if err != nil {
			return
		}
		for _, line := range strings.Split(string(content), "\n") {
			if trimmed := strings.TrimSpace(line); trimmed != "" {
				dirs = append(dirs, trimmed)
			}
		}
	}
	const pathsDirectory = "/etc/paths.d"
	read("/etc/paths")
	entries, err := os.ReadDir(pathsDirectory)
	if err == nil {
		for _, entry := range entries {
			read(filepath.Join(pathsDirectory, entry.Name()))
		}
	}
	dirs = append(dirs, filepath.SplitList(os.Getenv("PATH"))...)
	for _, dir := range dirs {
		candidate := filepath.Join(dir, name)
		if info, statErr := os.Stat(candidate); statErr == nil && !info.IsDir() && info.Mode()&0o111 != 0 { //nolint:gosec // G703: the candidate path is built from the machine's own PATH configuration and is only stat'd, to report whether such an executable exists.
			t.Skipf("an executable named %s is installed at %s; this probe runs a restore plan for that name and must never execute a real agent binary", name, candidate)
		}
	}
	t.Logf("no executable named %s exists in any system PATH directory (%d checked); the restore plan can only reach this probe's recorder", name, len(dirs))
}

// installStandInAgent writes the recorder under the stand-in agent's name
// into the server's own hermetic bin directory (already first on the
// server environment's PATH) and prepends that directory in the temporary
// HOME's shell startup files, so the LOGIN shell Herdr's deferred restore
// spawns resolves the bare name to it even after path_helper has reordered
// PATH. It must run before the server starts.
func installStandInAgent(t *testing.T, server *testServer, artifacts *artifactDir) standInRecorder {
	t.Helper()
	recorder := standInRecorder{
		Dir:     filepath.Join(server.base, "bin"),
		Marker:  filepath.Join(artifacts.dir(t, "records"), "invocations.txt"),
		Records: artifacts.dir(t, "records"),
		Gate:    filepath.Join(artifacts.dir(t, "gates"), "stand-in-release"),
	}
	buildStandInAgent(t, filepath.Join(recorder.Dir, standInAgentName), &recorder)
	if err := os.WriteFile(recorder.Marker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	prepend := "PATH=\"" + recorder.Dir + ":$PATH\"\nexport PATH\n"
	for _, name := range []string{".profile", ".zprofile", ".zshrc"} {
		if err := os.WriteFile(filepath.Join(server.homeDir(), name), []byte(prepend), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		if err := os.WriteFile(recorder.Gate, nil, 0o600); err != nil {
			t.Errorf("open the stand-in release gate: %v", err)
		}
	})
	return recorder
}

// buildStandInAgent compiles standInRecorderSource, with this test's own
// paths baked in, to an executable at path — the stand-in agent's name on
// the pane shell's PATH.
func buildStandInAgent(t *testing.T, path string, recorder *standInRecorder) {
	t.Helper()
	src, err := os.MkdirTemp("", "hop-spike-standin")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if removeErr := os.RemoveAll(src); removeErr != nil {
			t.Logf("remove the stand-in source: %v", removeErr)
		}
	})
	source := fmt.Sprintf(standInRecorderSource, recorder.Marker, recorder.Records, recorder.Gate)
	if err = os.WriteFile(filepath.Join(src, "go.mod"), []byte("module standinagent\n\ngo 1.21\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(src, "main.go"), []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Fatalf("the go tool is required to build the stand-in agent: %v", err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	build := exec.CommandContext(ctx, goBin, "build", "-o", path, ".") //nolint:gosec // G204: the go tool builds this test's own generated module into this test's own roots.
	build.Dir = src
	if out, buildErr := build.CombinedOutput(); buildErr != nil {
		t.Fatalf("go build the stand-in agent: %v\n%s", buildErr, out)
	}
}

// invocations reads every line the recorder has written so far.
func (r *standInRecorder) invocations(t *testing.T) string {
	t.Helper()
	content, err := os.ReadFile(r.Marker)
	if err != nil {
		t.Fatalf("read the stand-in invocation marker: %v", err)
	}
	return string(content)
}

// fired reports whether the recorder has recorded an invocation carrying
// sessionRef as one of its arguments — the resume plan for exactly that
// pane's recorded native session.
func (r *standInRecorder) fired(t *testing.T, sessionRef string) bool {
	t.Helper()
	return r.firings(t, sessionRef) > 0
}

// firings counts the invocations carrying sessionRef. A pane that is never
// closed resumes once per restart it survives, so the count is what tells
// one restore from another for the same reference.
func (r *standInRecorder) firings(t *testing.T, sessionRef string) int {
	t.Helper()
	return strings.Count(r.invocations(t), "["+sessionRef+"]")
}

// lines returns every invocation the recorder has written, empty lines
// dropped.
func (r *standInRecorder) lines(t *testing.T) []string {
	t.Helper()
	var recorded []string
	for _, line := range strings.Split(r.invocations(t), "\n") {
		if strings.TrimSpace(line) != "" {
			recorded = append(recorded, line)
		}
	}
	return recorded
}

// requireOnlyResumesFor fails unless EVERY invocation the recorder has
// written names sessionRef. It is the whole-file form of the negative: a
// resume fired under any other reference — a closed pane's, or the pane
// that never had a session recorded at all — fails here even though no
// per-reference search was looking for it.
func (r *standInRecorder) requireOnlyResumesFor(t *testing.T, sessionRef, when string) {
	t.Helper()
	recorded := r.lines(t)
	for _, line := range recorded {
		if !strings.Contains(line, "["+sessionRef+"]") {
			t.Fatalf("the stand-in recorded an invocation for a reference other than the control's %s: %q (every invocation %s:\n%s)", when, line, when, strings.Join(recorded, "\n"))
		}
	}
}

// resumeEnvironment returns the environment dump of the invocation that
// resumed sessionRef, keyed by the pid its own marker line recorded.
func (r *standInRecorder) resumeEnvironment(t *testing.T, sessionRef string) (map[string]string, bool) {
	t.Helper()
	pid := ""
	for _, line := range strings.Split(r.invocations(t), "\n") {
		if !strings.Contains(line, "["+sessionRef+"]") {
			continue
		}
		for _, field := range strings.Fields(line) {
			if rest, ok := strings.CutPrefix(field, "pid="); ok {
				pid = rest
			}
		}
	}
	if pid == "" {
		return nil, false
	}
	content, err := os.ReadFile(filepath.Join(r.Records, "env-"+pid+".txt"))
	if err != nil {
		return nil, false
	}
	env := map[string]string{}
	for _, line := range strings.Split(string(content), "\n") {
		name, value, ok := strings.Cut(line, "=")
		if ok {
			env[name] = value
		}
	}
	return env, true
}

// restartPane is one command pane this probe owns: what HOP would have
// recorded for it (pane id, creation label, native session reference) and
// what a human may have done to it since.
type restartPane struct {
	Name string
	// PaneID is the pane id the creating request answered — the id HOP
	// records in its binding and the only address this probe closes by.
	PaneID string
	// Label is the creation label, the second half of HOP's absence rule.
	Label string
	// Manual is a human's pane.rename, empty when none was made.
	Manual string
	// SessionRef is the native agent session reported for the pane, and
	// therefore the argument its restore plan carries.
	SessionRef string
	// ShellPID is the pane's own process before the restart.
	ShellPID int
	// Gate releases the pane's original command.
	Gate string
}

// openRestartPane opens one command pane through the production
// Runtime.OpenWorkerPane with a production-shaped additive environment,
// and observes its own process. The full process relation a command pane
// holds is already pinned by TestSpikeVanishedPaneShapes; this opener
// asserts only what this probe's own conclusions rest on: the pane answers
// by its recorded id, its creation label resolves to exactly it, and its
// own process is live before the restart.
func openRestartPane(t *testing.T, runtime *herdr.Runtime, server *testServer, gates, workspaceID, name, runID string) restartPane {
	t.Helper()
	pane := restartPane{
		Name:  name,
		Label: "hop-spike-restartclose-" + name + "-" + newSpikeUUID(t),
		Gate:  filepath.Join(gates, name),
	}
	ctx, cancel := context.WithTimeout(t.Context(), callTimeout)
	handle, err := runtime.OpenWorkerPane(ctx, app.WorkerPaneRequest{
		WorkspaceID: workspaceID,
		Cwd:         server.workDir(),
		Command:     []string{"/bin/sh", "-c", gatedPaneScript, "sh", pane.Gate},
		Env: map[string]string{
			"HOP_RUN_ID":         runID,
			"HOP_SESSION_ID":     newSpikeUUID(t),
			"HOP_INCARNATION_ID": newSpikeUUID(t),
			"HOP_ROLE":           "implementer",
		},
		Label: pane.Label,
	})
	cancel()
	if err != nil {
		t.Fatalf("OpenWorkerPane(%s): %v", name, err)
	}
	pane.PaneID = handle.PaneID
	t.Cleanup(func() {
		if gateErr := os.WriteFile(pane.Gate, nil, 0o600); gateErr != nil {
			t.Errorf("open the gate for pane %s: %v", name, gateErr)
		}
	})

	var observed app.PaneProcess
	if !waitUntil(func() bool {
		inspectCtx, inspectCancel := context.WithTimeout(t.Context(), callTimeout)
		defer inspectCancel()
		info, inspectErr := runtime.InspectPane(inspectCtx, pane.PaneID)
		if inspectErr != nil {
			return false
		}
		observed = info
		return info.ShellPID > 1 && slices.ContainsFunc(info.Foreground, func(p app.ProcessInfo) bool {
			return p.PID == info.ShellPID && (slices.Contains(p.Argv, pane.Gate) || strings.Contains(p.Cmdline, pane.Gate))
		})
	}) {
		t.Fatalf("pane %s (%s) never showed its command as the shell-pid foreground member; last observation %+v", pane.PaneID, name, observed)
	}
	pane.ShellPID = observed.ShellPID

	lookupCtx, lookupCancel := context.WithTimeout(t.Context(), callTimeout)
	ref, found, err := runtime.FindPaneByLabel(lookupCtx, pane.Label)
	lookupCancel()
	if err != nil || !found || ref.PaneID != pane.PaneID {
		t.Fatalf("FindPaneByLabel(%s) = %+v, found=%t, err=%v; want pane %s", pane.Label, ref, found, err, pane.PaneID)
	}
	return pane
}

// reportAgentSession records a native agent session for the pane through
// Herdr's own pane.report_agent_session — the call Herdr's harness
// integration makes for a real conversation — so the saved session carries
// the reference its restore plan is built from.
func reportAgentSession(t *testing.T, server *testServer, pane *restartPane) {
	t.Helper()
	pane.SessionRef = newSpikeUUID(t)
	server.call(t, "pane.report_agent_session", map[string]any{
		"pane_id":          pane.PaneID,
		"source":           standInAgentSource,
		"agent":            standInAgentName,
		"agent_session_id": pane.SessionRef,
		"seq":              1,
	}, &struct{}{})
}

// paneAgentSessions reads pane.list and returns each pane's recorded agent
// session value, present only for panes Herdr will build a restore plan
// for.
func paneAgentSessions(t *testing.T, server *testServer) map[string]string {
	t.Helper()
	var result struct {
		Panes []struct {
			PaneID       string `json:"pane_id"`
			AgentSession *struct {
				Agent string `json:"agent"`
				Value string `json:"value"`
			} `json:"agent_session"`
		} `json:"panes"`
	}
	server.call(t, "pane.list", map[string]any{}, &result)
	sessions := map[string]string{}
	for _, pane := range result.Panes {
		if pane.AgentSession != nil {
			sessions[pane.PaneID] = pane.AgentSession.Agent + ":" + pane.AgentSession.Value
		}
	}
	return sessions
}

// paneObservation is one bounded look at a pane through the production
// adapter, recorded whether or not it answered.
type paneObservation struct {
	Answered   bool
	NotFound   bool
	Err        error
	Process    app.PaneProcess
	Foreground []string
}

// observePane inspects paneID once and classifies the answer, rendering
// every foreground member's argv for the evidence file.
func observePane(t *testing.T, runtime *herdr.Runtime, paneID string) paneObservation {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), callTimeout)
	defer cancel()
	info, err := runtime.InspectPane(ctx, paneID)
	observation := paneObservation{Err: err, Process: info}
	switch {
	case err == nil:
		observation.Answered = true
		for _, fg := range info.Foreground {
			observation.Foreground = append(observation.Foreground, fmt.Sprintf("pid=%d argv=%q", fg.PID, fg.Argv))
		}
	case errors.Is(err, app.ErrPaneNotFound):
		observation.NotFound = true
	}
	return observation
}

// requirePaneGoneByIDAndLabel asserts the one absence rule holds for the
// pane: no pane answers by its recorded id, and every label it has ever
// carried — the creation label and any manual rename — resolves nothing.
func requirePaneGoneByIDAndLabel(t *testing.T, runtime *herdr.Runtime, pane *restartPane, when string) {
	t.Helper()
	if !waitUntil(func() bool { return observePane(t, runtime, pane.PaneID).NotFound }) {
		t.Errorf("pane %s (%s) still answers by id %s", pane.PaneID, pane.Name, when)
	}
	labels := []string{pane.Label}
	if pane.Manual != "" {
		labels = append(labels, pane.Manual)
	}
	for _, label := range labels {
		ctx, cancel := context.WithTimeout(t.Context(), callTimeout)
		ref, found, err := runtime.FindPaneByLabel(ctx, label)
		cancel()
		if err != nil || found {
			t.Errorf("FindPaneByLabel(%s) for pane %s %s = %+v, found=%t, err=%v; want a successful lookup finding nothing", label, pane.Name, when, ref, found, err)
		}
	}
}

// closeAttempt is one close of a restored pane by its RECORDED id, with
// the window it landed in recorded rather than assumed.
type closeAttempt struct {
	Pane         string
	SinceRestart time.Duration
	Before       paneObservation
	CloseErr     error
}

// render formats one close attempt for the evidence file.
func (a *closeAttempt) render() string {
	window := "the deferred restore had NOT fired (the pane answered no inspection)"
	if a.Before.Answered {
		window = "the deferred restore HAD fired (the pane answered with a live occupant): " + strings.Join(a.Before.Foreground, "; ")
	} else if !a.Before.NotFound {
		window = fmt.Sprintf("inconclusive: the inspection failed with %v", a.Before.Err)
	}
	return fmt.Sprintf("pane %s: closed %s after the restart; window: %s; ClosePane answered: %v\n", a.Pane, a.SinceRestart, window, a.CloseErr)
}

// closeRestoredPane takes the one observation that classifies the window
// and then closes the pane by its RECORDED id — never by a label, never
// against a matched occupant, since after a restart the occupant can only
// be a fresh shell or a restored agent.
func closeRestoredPane(t *testing.T, runtime *herdr.Runtime, pane *restartPane, restartedAt time.Time) closeAttempt {
	t.Helper()
	attempt := closeAttempt{Pane: pane.Name, Before: observePane(t, runtime, pane.PaneID)}
	ctx, cancel := context.WithTimeout(t.Context(), callTimeout)
	attempt.CloseErr = runtime.ClosePane(ctx, pane.PaneID)
	cancel()
	attempt.SinceRestart = time.Since(restartedAt)
	return attempt
}

// negativeWindowFloor is the shortest a negative observation is ever made
// over, whatever the control's own latency turns out to be.
const negativeWindowFloor = 15 * time.Second

// negativeWindowMultiple scales the negative window against the control's
// measured latency, so the absence is always observed over an interval
// many times longer than the one a restore demonstrably needs.
const negativeWindowMultiple = 20

// requireNoResumeOver samples the recorder across a window and fails if a
// resume ever appears for any of the references that must stay discarded.
func requireNoResumeOver(t *testing.T, recorder *standInRecorder, window time.Duration, panes []*restartPane, condition string) {
	t.Helper()
	deadline := time.Now().Add(window)
	for {
		for _, pane := range panes {
			if recorder.fired(t, pane.SessionRef) {
				t.Fatalf("a resume fired for closed pane %s (%s) %s; the close did not discard its pending restore. recorded invocations:\n%s", pane.Name, pane.SessionRef, condition, recorder.invocations(t))
			}
		}
		if time.Now().After(deadline) {
			t.Logf("no resume appeared for %d closed pane(s) over %s %s", len(panes), window, condition)
			return
		}
		time.Sleep(pollInterval)
	}
}

// TestSpikeRestoredPaneCloseDiscardsResume is the executed probe behind the
// restart-lifecycle decision (docs/plan/phase-3-design.md sections 4 and 6):
// on a CHANGED server lifetime, closing a pane HOP owns by its RECORDED
// pane id is a positive act that both removes the pane and discards the
// deferred native agent restore Herdr would otherwise spawn without HOP's
// knowledge. Against a real, disposable server and through the production
// herdr.Runtime it pins:
//
//   - the lifetime tokens on both sides of a graceful restart, so CHANGED
//     is distinguishable from UNKNOWN within one run;
//   - that a pane restored after a restart — including one a human RENAMED
//     first, which the rename hides from label lookup — is closed by its
//     recorded id, in whichever restore window the close lands in, after
//     which no pane answers by that id or by any label it has carried;
//   - what a close of such a pane answers before its deferred restore has
//     fired versus after, the second being the ordinary case: the restore
//     fires about PENDING_AGENT_RESUME_THEME_WAIT after the restore with no
//     client ever attaching, so HOP normally meets an already-resumed pane
//     whose live occupant is not the process it recorded;
//   - that the closed panes never resume — measured against a CONTROL pane,
//     left open in the same run, whose resume demonstrably DOES fire, and
//     observed again under a client attach and across a second restart,
//     which is what "the plan is discarded at the root" means. The control
//     carries the second half too: never closed, it resumes AGAIN after the
//     second restart, so that restart is proven to re-arm a surviving
//     pane's plan and the closed panes' silence across it means something;
//   - the environment a fired resume actually carries, which decides
//     whether an orphaned agent is a stray process or a stale incarnation
//     that can still reach HOP's CLIs.
//
// The stand-in agent is not claude: see standInAgentName for why running a
// restore plan named for a really-installed harness is forbidden here, and
// why the mechanism pinned is the same one.
func TestSpikeRestoredPaneCloseDiscardsResume(t *testing.T) {
	requireNoInstalledAgent(t, standInAgentName)
	artifacts := newArtifactDir(t)
	server := prepareServer(t, artifacts)
	recorder := installStandInAgent(t, server, artifacts)
	server.start(t)
	runtime := herdr.NewRuntime(server.socketPath)
	gates := artifacts.dir(t, "gates")
	runID := newSpikeUUID(t)

	ctx, cancel := context.WithTimeout(t.Context(), callTimeout)
	workspace, err := runtime.CreateWorkspace(ctx, app.WorkspaceRequest{Cwd: server.workDir(), Label: "hop-spike-restartclose-" + runID})
	cancel()
	if err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}

	// Four panes: two subjects this probe closes after the restart (one of
	// them renamed by a "human" first), the control whose restore must
	// fire, and one pane with no recorded session at all — the plain
	// cold-restore shape, which tells "no runtime because a restore is
	// pending" apart from "no pane".
	subject := openRestartPane(t, runtime, server, gates, workspace.WorkspaceID, "subject", runID)
	renamed := openRestartPane(t, runtime, server, gates, workspace.WorkspaceID, "renamed", runID)
	control := openRestartPane(t, runtime, server, gates, workspace.WorkspaceID, "control", runID)
	plain := openRestartPane(t, runtime, server, gates, workspace.WorkspaceID, "plain", runID)

	for _, pane := range []*restartPane{&subject, &renamed, &control} {
		reportAgentSession(t, server, pane)
	}
	renamed.Manual = "human-renamed-" + newSpikeUUID(t)
	renamePane(t, server, renamed.PaneID, renamed.Manual)
	requireLabelAnswersNothing(t, runtime, renamed.Label, "after the rename, before the restart")

	// The precondition everything else rests on: Herdr has recorded a
	// resumable session for exactly the three panes, and none for the
	// fourth. Without it there is no restore plan to discard and this probe
	// proves nothing.
	sessions := paneAgentSessions(t, server)
	for _, pane := range []*restartPane{&subject, &renamed, &control} {
		want := standInAgentName + ":" + pane.SessionRef
		if got := sessions[pane.PaneID]; got != want {
			t.Fatalf("pane.list agent_session for %s = %q, want %q; without a recorded session no restore plan exists and this probe can conclude nothing", pane.Name, got, want)
		}
	}
	if got, present := sessions[plain.PaneID]; present {
		t.Errorf("pane.list agent_session for the plain pane = %q, want none recorded", got)
	}
	artifacts.save(t, "agent-sessions-before-restart.txt", fmt.Sprintf("%+v\n", sessions))

	before := requireServerLifetime(t, runtime, server, "before the restart")

	server.restart(t)
	restartedAt := time.Now()

	// The close, immediately and by the RECORDED id alone. Which restore
	// window each lands in is observed and reported, never assumed: the
	// close rule this probe backs cannot depend on winning that race.
	subjectClose := closeRestoredPane(t, runtime, &subject, restartedAt)
	renamedClose := closeRestoredPane(t, runtime, &renamed, restartedAt)

	// The CONTROL's own restore is waited for FIRST, from the restart's own
	// instant: it is the present signal the whole negative is measured
	// against, and how long it takes is the measurement that says how short
	// the window HOP has really is. The headless condition is tried alone,
	// because whether a restore fires with no client attached is itself the
	// finding. Every later assertion is unaffected by the wait.
	var client *ptyClient
	firedHeadless := waitUntilDeadline(30*time.Second, func() bool { return recorder.fired(t, control.SessionRef) })
	latency := time.Since(restartedAt)
	if firedHeadless {
		t.Logf("the control pane's deferred restore fired %s after the restart, with NO client ever attached", latency)
	} else {
		t.Logf("the control pane's deferred restore did NOT fire within 30s on the headless server; attaching a client")
		client = server.attachPTYClient(t)
		attachedAt := time.Now()
		if !waitUntilDeadline(30*time.Second, func() bool { return recorder.fired(t, control.SessionRef) }) {
			t.Fatalf("the control pane's restore never fired, headless or with a client attached; this probe's negative result would be measured against nothing. recorded invocations:\n%s", recorder.invocations(t))
		}
		latency = time.Since(attachedAt)
		t.Logf("the control pane's deferred restore fired %s after a client attached", latency)
	}

	after := requireServerLifetime(t, runtime, server, "after the restart")
	if goruntime.GOOS == "darwin" && after == before {
		t.Errorf("ServerInstance after the restart = %q, the same as before; want a different server lifetime", after)
	}
	t.Logf("server lifetime before the restart %q, after %q", before, after)

	for _, attempt := range []*closeAttempt{&subjectClose, &renamedClose} {
		t.Log(strings.TrimSpace(attempt.render()))
		if attempt.CloseErr != nil {
			t.Errorf("ClosePane by the recorded id for pane %s: %v", attempt.Pane, attempt.CloseErr)
		}
	}
	artifacts.save(t, "close-attempts.txt", subjectClose.render()+renamedClose.render())

	// Whatever window the close landed in, the pane is gone by every
	// address HOP holds: its recorded id, its creation label, and (for the
	// renamed pane) the manual name a rename left in its place.
	requirePaneGoneByIDAndLabel(t, runtime, &subject, "after the close")
	requirePaneGoneByIDAndLabel(t, runtime, &renamed, "after the close")

	// What the orphan actually is: the environment the resumed process
	// carries decides whether it is a stray process or a stale incarnation
	// that could still reach HOP's CLIs.
	resumeEnv, found := recorder.resumeEnvironment(t, control.SessionRef)
	if !found {
		t.Errorf("no environment dump was recorded for the control pane's resume")
	} else {
		var hop []string
		for name, value := range resumeEnv {
			if strings.HasPrefix(name, "HOP_") && !strings.HasPrefix(name, "HOP_SPIKE_") {
				hop = append(hop, name+"="+value)
			}
		}
		slices.Sort(hop)
		t.Logf("the resumed process carries HERDR_PANE_ID=%q and these HOP_* variables: %v", resumeEnv["HERDR_PANE_ID"], hop)
		artifacts.save(t, "resumed-environment.txt", fmt.Sprintf("hop_variables=%v\nherdr_pane_id=%s\n", hop, resumeEnv["HERDR_PANE_ID"]))
	}

	// The negative, measured against that present signal.
	window := max(latency*negativeWindowMultiple, negativeWindowFloor)
	closed := []*restartPane{&subject, &renamed}
	requireNoResumeOver(t, &recorder, window, closed, "after the close, while the control's own restore had already fired")

	// The second condition: a real attached client, which supplies the host
	// terminal theme and drives the resize path that starts pending
	// restores — the condition under which a restore most reliably fires.
	if client == nil {
		client = server.attachPTYClient(t)
	}
	requireNoResumeOver(t, &recorder, window, closed, "with a client attached")
	client.close(t)

	// Everything recorded so far is the control's ONE restore, and nothing
	// else: the whole-file form of the negative, which also catches a
	// resume fired for the pane that never had a session recorded.
	recorder.requireOnlyResumesFor(t, control.SessionRef, "before the second restart")
	if got := recorder.firings(t, control.SessionRef); got != 1 {
		t.Fatalf("the control pane resumed %d times before the second restart, want exactly 1:\n%s", got, recorder.invocations(t))
	}

	// The root: a plan discarded from the persisted session cannot be
	// resurrected by a later restore. The CONTROL is the positive control
	// for this half too — it was never closed, so its session is saved
	// again and its restore must fire a SECOND time, which is what makes
	// the closed panes' silence across the same restart mean something.
	server.restart(t)
	if !waitUntilDeadline(30*time.Second, func() bool { return recorder.firings(t, control.SessionRef) >= 2 }) {
		t.Fatalf("the control pane did not resume again after the second restart; the second restart re-armed nothing, so the closed panes' silence across it proves nothing:\n%s", recorder.invocations(t))
	}
	t.Logf("the control pane resumed a SECOND time after the second restart: the restart re-arms a surviving pane's restore plan")
	requireNoResumeOver(t, &recorder, window, closed, "across a second restart")
	recorder.requireOnlyResumesFor(t, control.SessionRef, "after the second restart")
	requirePaneGoneByIDAndLabel(t, runtime, &subject, "after the second restart")
	requirePaneGoneByIDAndLabel(t, runtime, &renamed, "after the second restart")

	// The ordinary case, pinned explicitly: a pane whose restore HAS fired
	// holds an occupant that is not the process HOP recorded and carries no
	// pid HOP has ever seen, and it still closes by its recorded id.
	fired := observePane(t, runtime, control.PaneID)
	if !fired.Answered {
		t.Fatalf("the control pane does not answer after its restore fired (err=%v); the already-fired close cannot be pinned", fired.Err)
	}
	if fired.Process.ShellPID == control.ShellPID {
		t.Errorf("the control pane's process is still the pre-restart pid %d; want the restored agent's own process", control.ShellPID)
	}
	// The three properties the close rule's restored-harness conjunct reads
	// (internal/app's MatchRestoredHarness, retyped here rather than
	// derived): argv[0]'s BASENAME is the agent's own name, an argv element
	// equal to the resume flag is IMMEDIATELY followed by one EXACTLY equal
	// to the native reference, and EXACTLY ONE foreground member satisfies
	// both — the last is what makes that predicate's ambiguous outcome a
	// real case rather than a theoretical one.
	var matched []app.ProcessInfo
	for _, fg := range fired.Process.Foreground {
		if len(fg.Argv) == 0 || filepath.Base(fg.Argv[0]) != standInAgentName {
			continue
		}
		for i := 1; i+1 < len(fg.Argv); i++ {
			if fg.Argv[i] == "--resume" && fg.Argv[i+1] == control.SessionRef {
				matched = append(matched, fg)
				break
			}
		}
	}
	if len(matched) != 1 {
		t.Errorf("the control pane's foreground holds %d members running the restore plan for %s, want exactly 1; foreground = %v", len(matched), control.SessionRef, fired.Foreground)
	}
	artifacts.save(t, "control-after-resume.txt", strings.Join(fired.Foreground, "\n")+"\n")
	t.Logf("the already-resumed control pane holds: %s", strings.Join(fired.Foreground, "; "))

	firedClose := closeRestoredPane(t, runtime, &control, restartedAt)
	t.Log(strings.TrimSpace(firedClose.render()))
	if firedClose.CloseErr != nil {
		t.Errorf("ClosePane of the already-resumed control pane by its recorded id: %v", firedClose.CloseErr)
	}
	requirePaneGoneByIDAndLabel(t, runtime, &control, "after closing the already-resumed pane")

	// The plain pane is the shape a restart leaves with no recorded
	// session: restored, answering by id under the new lifetime, with a
	// fresh process of its own.
	plainObservation := observePane(t, runtime, plain.PaneID)
	switch {
	case !plainObservation.Answered:
		t.Errorf("the plain restored pane does not answer by its recorded id (err=%v)", plainObservation.Err)
	case plainObservation.Process.ShellPID == plain.ShellPID:
		t.Errorf("the plain restored pane's process is still the pre-restart pid %d; a cold restore starts a fresh shell", plain.ShellPID)
	}

	artifacts.save(t, "invocations.txt", recorder.invocations(t))
	// The final whole-file claim: every invocation the stand-in ever
	// recorded, across both restarts, is the control's — two of them, one
	// per restart it survived — and no other reference, closed or
	// session-less, ever appears.
	recorder.requireOnlyResumesFor(t, control.SessionRef, "over the whole run")
	if got := len(recorder.lines(t)); got != 2 {
		t.Errorf("the stand-in recorded %d invocations over the run, want exactly the control's two (one per restart it survived):\n%s", got, recorder.invocations(t))
	}
}

// TestSpikeRecordedPaneIDCanAddressADifferentPane is the executed probe
// behind the restart-close rule's IDENTIFICATION requirement
// (docs/plan/phase-3-design.md sections 4 and 6): a recorded pane id is not
// a durable address across a restart, so closing one on a changed server
// lifetime must first identify the pane as the session's own.
//
// The mechanism is not pane renumbering — a workspace's pane counter never
// regresses, within a lifetime (repos/herdr/src/workspace.rs,
// register_new_pane_with_number advances the counter and unregister_pane
// does not lower it) or across a restore (repos/herdr/src/persist/restore.rs
// re-seeds it to the saved high-water mark). It is that a pane id is
// `<workspace id>:p<n>` and WORKSPACE ids are reissued: the id counter is
// re-seeded from the highest RESTORED workspace, so a workspace closed
// before the restart leaves its id free for the next new one
// (TestSpikeWorkspaceIDReissuedAfterRestart pins that half). The pane id
// composed from a reissued workspace id therefore answers for a pane that
// belongs to somebody else.
//
// What makes the observation decisive is the pair: the recorded pane id
// ANSWERS, and the recorded creation label — a UUID this run minted —
// answers nothing. The durable identity is the label; the pane id is a
// lookup key that is only trustworthy once something else has vouched for
// it.
func TestSpikeRecordedPaneIDCanAddressADifferentPane(t *testing.T) {
	artifacts := newArtifactDir(t)
	server := prepareServer(t, artifacts)
	server.start(t)
	runtime := herdr.NewRuntime(server.socketPath)

	create := func(name string) app.WorkspaceHandle {
		t.Helper()
		ctx, cancel := context.WithTimeout(t.Context(), callTimeout)
		defer cancel()
		handle, err := runtime.CreateWorkspace(ctx, app.WorkspaceRequest{
			Cwd:   server.workDir(),
			Label: "hop-spike-idreuse-" + name + "-" + newSpikeUUID(t),
		})
		if err != nil {
			t.Fatalf("CreateWorkspace(%s): %v", name, err)
		}
		return handle
	}

	// kept survives the restart and seeds the restored counter; closed is
	// the one whose id falls free.
	kept := create("kept")
	closed := create("closed")
	closedLabel := "hop-spike-idreuse-closed-label-" + newSpikeUUID(t)
	renamePane(t, server, closed.PaneID, closedLabel)
	requireLabelResolves(t, runtime, &vanishingPane{Label: closedLabel, PaneID: closed.PaneID}, "before the workspace is closed")
	t.Logf("before the restart: kept workspace %s (pane %s), closed workspace %s (pane %s)", kept.WorkspaceID, kept.PaneID, closed.WorkspaceID, closed.PaneID)

	server.call(t, "workspace.close", map[string]any{"workspace_id": closed.WorkspaceID}, nil)
	if !waitUntil(func() bool { return observePane(t, runtime, closed.PaneID).NotFound }) {
		t.Fatalf("pane %s still answers after its workspace was closed", closed.PaneID)
	}

	server.restart(t)

	// A fresh workspace after the restart takes the closed workspace's id
	// back, and with it the whole pane-id namespace underneath it.
	fresh := create("fresh")
	t.Logf("after the restart: fresh workspace %s (pane %s); the closed workspace was %s (pane %s)", fresh.WorkspaceID, fresh.PaneID, closed.WorkspaceID, closed.PaneID)
	if fresh.WorkspaceID != closed.WorkspaceID {
		t.Skipf("the restarted server issued %s to the new workspace rather than reusing the closed %s; this run cannot pin the reuse", fresh.WorkspaceID, closed.WorkspaceID)
	}
	if fresh.PaneID != closed.PaneID {
		t.Fatalf("the new workspace's root pane is %s, not the closed workspace's recorded %s; the collision this probe is about did not occur", fresh.PaneID, closed.PaneID)
	}

	// The decisive pair: the recorded id answers, and it is NOT ours.
	answering := observePane(t, runtime, closed.PaneID)
	if !answering.Answered {
		t.Fatalf("the recorded pane id %s does not answer after the restart (err=%v); the collision this probe is about did not occur", closed.PaneID, answering.Err)
	}
	requireLabelAnswersNothing(t, runtime, closedLabel, "after the restart reissued the pane id to another workspace")
	t.Logf("the recorded pane id %s now answers for the new workspace's own pane (foreground %v) while the recorded label %s answers nothing: a close by recorded id alone would have destroyed a pane this run never owned", closed.PaneID, answering.Foreground, closedLabel)
	artifacts.save(t, "reissued-pane-id.txt", fmt.Sprintf("recorded pane id %s, recorded label %s\nafter the restart it answers: %v\n", closed.PaneID, closedLabel, answering.Foreground))
}
