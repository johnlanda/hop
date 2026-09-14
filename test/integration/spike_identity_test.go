package integration

import (
	"strings"
	"testing"
)

// TestSpikePaneProcessIdentity is spike item S2: it establishes exactly what
// pane.process_info exposes for occupant identity, and that a same-kind
// occupant replacement in the same pane is distinguishable — by pid and by
// the launch argv, which the API reports — while noting the API exposes no
// process start time, so pid alone is not reuse-proof (see FINDINGS.md and
// the PaneProcessInfo schema in the Herdr source).
//
// The scenario runs the recognized-name harness stand-in as a foreground
// child of the pane shell (no exec), ends it through its own quit line, and
// starts a second one with a different attempt identity in its argv: the
// same pane, the same process name, a different occupant.
func TestSpikePaneProcessIdentity(t *testing.T) {
	fixtures := buildSpikeFixtures(t)
	artifacts := newArtifactDir(t)
	server := prepareServer(t, artifacts)
	server.start(t)

	pane := server.createSpikeWorkerPane(t, nil)
	firstAttempt, secondAttempt := newSpikeUUID(t), newSpikeUUID(t)

	// First occupant: a foreground child of the pane shell.
	server.call(t, "pane.send_text", map[string]any{
		"pane_id": pane,
		"text":    "'" + fixtures.claude + "' --attempt " + firstAttempt + "\n",
	}, nil)
	server.waitForPaneText(t, pane, "SPIKE-ARGS=[--attempt "+firstAttempt+"]")
	first := server.waitForForegroundProcess(t, pane, "claude")
	artifacts.save(t, "process-info-first-occupant.txt", renderProcessInfo(first))

	firstProcess := foregroundProcessNamed(first, "claude")
	if firstProcess == nil {
		t.Fatalf("no foreground claude process for the first occupant:\n%s", renderProcessInfo(first))
	}
	// The identity fields the API exposes, verified populated on this
	// platform: pid, name, argv0, argv, cmdline, cwd, plus the pane-level
	// shell pid, foreground process group and tty.
	if firstProcess.PID == 0 || firstProcess.Name == "" || firstProcess.Argv0 == "" ||
		len(firstProcess.Argv) == 0 || firstProcess.Cmdline == "" || firstProcess.Cwd == "" {
		t.Errorf("process identity fields incomplete: %+v", firstProcess)
	}
	if first.ShellPID == 0 || first.ForegroundProcessGroup == 0 {
		t.Errorf("pane identity fields incomplete: shell_pid=%d pgid=%d",
			first.ShellPID, first.ForegroundProcessGroup)
	}
	// The tty field exists in the schema but is not populated on this
	// platform; recorded as evidence, not asserted (see FINDINGS.md).
	t.Logf("pane.process_info tty=%q", first.TTY)
	// A non-exec'd child keeps the shell alive: the occupant pid is not the
	// shell pid.
	if firstProcess.PID == first.ShellPID {
		t.Errorf("foreground child pid %d equals shell pid; expected a distinct child", firstProcess.PID)
	}
	if !strings.Contains(strings.Join(firstProcess.Argv, " "), firstAttempt) {
		t.Errorf("first occupant argv %q does not carry its attempt identity", firstProcess.Argv)
	}

	// End the first occupant through its own protocol and wait until the
	// shell is the foreground again.
	server.call(t, "pane.send_text", map[string]any{"pane_id": pane, "text": "SPIKE-QUIT\n"}, nil)
	if !waitUntil(func() bool {
		return foregroundProcessNamed(server.processInfo(t, pane), "claude") == nil
	}) {
		t.Fatal("first occupant never exited after SPIKE-QUIT")
	}

	// Second occupant: same pane, same recognized name, different identity.
	server.call(t, "pane.send_text", map[string]any{
		"pane_id": pane,
		"text":    "'" + fixtures.claude + "' --attempt " + secondAttempt + "\n",
	}, nil)
	server.waitForPaneText(t, pane, "SPIKE-ARGS=[--attempt "+secondAttempt+"]")
	second := server.waitForForegroundProcess(t, pane, "claude")
	artifacts.save(t, "process-info-second-occupant.txt", renderProcessInfo(second))

	secondProcess := foregroundProcessNamed(second, "claude")
	if secondProcess == nil {
		t.Fatalf("no foreground claude process for the second occupant:\n%s", renderProcessInfo(second))
	}
	// The replacement is distinguishable: a new pid and a new argv identity,
	// even though pane id and process name are identical.
	if secondProcess.PID == firstProcess.PID {
		t.Errorf("second occupant reuses pid %d; occupants would be indistinguishable by pid", firstProcess.PID)
	}
	if !strings.Contains(strings.Join(secondProcess.Argv, " "), secondAttempt) {
		t.Errorf("second occupant argv %q does not carry its attempt identity", secondProcess.Argv)
	}
	if strings.Contains(strings.Join(secondProcess.Argv, " "), firstAttempt) {
		t.Errorf("second occupant argv %q still carries the first attempt identity", secondProcess.Argv)
	}
	// The shell survived both occupants: the pane-level shell pid is stable
	// across the replacement.
	if second.ShellPID != first.ShellPID {
		t.Errorf("shell pid changed across occupant replacement: %d then %d", first.ShellPID, second.ShellPID)
	}
}

// waitForForegroundProcess polls pane.process_info until a foreground
// process with the given name is reported with a populated argv, then
// returns the full info; it fails on timeout with the last state. Waiting on
// the marker text alone is not enough: the process table sampling is
// asynchronous to the pane output.
func (s *testServer) waitForForegroundProcess(t *testing.T, paneID, name string) spikeProcessInfo {
	t.Helper()
	var last spikeProcessInfo
	found := waitUntil(func() bool {
		last = s.processInfo(t, paneID)
		process := foregroundProcessNamed(last, name)
		return process != nil && len(process.Argv) > 0
	})
	if !found {
		t.Fatalf("pane %s never reported a foreground process named %q with argv; last:\n%s",
			paneID, name, renderProcessInfo(last))
	}
	return last
}
