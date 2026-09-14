package integration

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

func TestSafeToSignalGroup(t *testing.T) {
	own := syscall.Getpgrp()
	cases := []struct {
		name string
		pgid int
		want bool
	}{
		{name: "zero is the caller's own group in kill semantics", pgid: 0, want: false},
		{name: "one is init", pgid: 1, want: false},
		{name: "negative is not a specific group", pgid: -1, want: false},
		{name: "the test runner's own group", pgid: own, want: false},
		{name: "a distinct real group", pgid: own + 100000, want: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := safeToSignalGroup(tc.pgid, own); got != tc.want {
				t.Errorf("safeToSignalGroup(%d, %d) = %t, want %t", tc.pgid, own, got, tc.want)
			}
		})
	}
}

// TestKillProcessGroupThenReapKillsDescendants proves the teardown signals the
// whole process group and reaps the leader: a leader that spawns a grandchild
// in the same group is torn down, killing the grandchild too, and the leader
// is reaped. The group is signaled while the leader is still unreaped, so the
// pgid cannot have been recycled. No sleeps: it polls bounded conditions.
func TestKillProcessGroupThenReapKillsDescendants(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "grandchild.pid")
	// The leader backgrounds a long-lived grandchild in the same group,
	// records its pid, and waits. Nothing kills them but the teardown.
	cmd := exec.CommandContext(t.Context(), "/bin/sh", "-c", "sleep 300 & printf '%s' \"$!\" > "+pidFile+"; wait") //nolint:gosec // G204: a fixed shell fixture with a test-owned path.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start fixture leader: %v", err)
	}
	pgid := cmd.Process.Pid // Setpgid makes the leader its own group leader

	var grandchild int
	if !waitUntil(func() bool {
		contents, err := os.ReadFile(pidFile) //nolint:gosec // G304: pidFile is under this test's own t.TempDir.
		if err != nil {
			return false
		}
		grandchild, err = strconv.Atoi(strings.TrimSpace(string(contents)))
		return err == nil && grandchild > 0
	}) {
		t.Fatal("fixture grandchild never recorded its pid")
	}
	if !processExists(grandchild) {
		t.Fatalf("fixture grandchild %d is not running before teardown", grandchild)
	}

	killProcessGroupThenReap(t, cmd, pgid, syscall.Getpgrp())

	// The leader was reaped by the teardown; the grandchild, reparented after
	// its SIGKILL, is reaped by init. Both are gone.
	if !waitUntil(func() bool { return !processExists(grandchild) }) {
		t.Errorf("grandchild %d survived the process-group teardown", grandchild)
	}
	if !waitUntil(func() bool { return !processExists(pgid) }) {
		t.Errorf("leader %d was not reaped by the teardown", pgid)
	}
}

// processExists reports whether a pid names a live or zombie process. A
// reaped or never-existent pid returns false (ESRCH); a live one owned by
// another user would return EPERM, which still counts as existing.
func processExists(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}
