package integration

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// TestSpikeRestartWaitsForGracefulExit proves the S3 restart barrier waits for
// the server's leader to ACTUALLY exit before treating a graceful stop as
// complete — the guarantee the restore evidence depends on — and that a leader
// which never exits is force-killed and reported as inconclusive rather than
// silently accepted. It uses fixture leaders, not a real herdr binary, so it
// exercises the barrier mechanism directly.
func TestSpikeRestartWaitsForGracefulExit(t *testing.T) {
	t.Run("waits for a delayed exit, and the delayed save completes", func(t *testing.T) {
		saveFile := filepath.Join(t.TempDir(), "delayed-save")
		// The leader does its shutdown work (sleep) BEFORE writing its save file
		// and exiting, modeling Herdr saving only as its run loop exits. A
		// barrier that killed on a failed ping instead of waiting for exit would
		// lose this file.
		leader := startFixtureLeader(t, "sleep 0.4; : > '"+saveFile+"'; exit 0")

		start := time.Now()
		exited := awaitProcessExit(leader, 5*time.Second)
		if !exited {
			t.Fatal("barrier reported no exit for a leader that exits on its own")
		}
		if time.Since(start) < 300*time.Millisecond {
			t.Errorf("barrier returned in %v, before the leader's delayed exit; it did not wait", time.Since(start))
		}
		if _, err := os.Stat(saveFile); err != nil {
			t.Errorf("the leader's delayed save is missing after the barrier: %v", err)
		}
	})

	t.Run("a leader that never exits is force-killed and reported inconclusive", func(t *testing.T) {
		leader := startFixtureLeader(t, "sleep 300")
		if awaitProcessExit(leader, 300*time.Millisecond) {
			t.Fatal("barrier reported a graceful exit for a leader that never exits")
		}
		// The forced path (what restart runs on the inconclusive branch) must
		// tear the group down.
		srv := &testServer{}
		srv.forceReap(t, leader)
		if !leader.reapedGroupGone(t) {
			t.Error("the force-killed leader's group is not gone")
		}
	})
}

// TestSpikeOwnedGroupTeardownKillsPipeHoldingChild validates the owned-group
// lifecycle used by the live (S4) probe, without any real harness: a leader
// that backgrounds a child inheriting the output pipe and never exits must be
// torn down within the deadline, its group left empty, so a real Claude child
// could not outlive the probe or block its output draining.
func TestSpikeOwnedGroupTeardownKillsPipeHoldingChild(t *testing.T) {
	// The leader backgrounds a long sleep that inherits stdout (the
	// CombinedOutput pipe) and then waits; nothing exits on its own.
	out, timedOut, err := runInOwnedGroup(t, 500*time.Millisecond, "/bin/sh", nil, t.TempDir(),
		"-c", "sleep 300 & wait")
	if !timedOut {
		t.Errorf("expected the never-exiting leader to hit the deadline; err=%v out=%q", err, out)
	}
	// runInOwnedGroup asserts the owned group is gone before returning; reaching
	// here without that t.Error is the teardown proof. The bounded return (the
	// test's own deadline) is the anti-hang evidence.
}

// startFixtureLeader starts a /bin/sh leader in its own process group and wraps
// it as a serverProcess with the wait goroutine, so the barrier helpers can be
// exercised without a herdr binary. It has no log files.
func startFixtureLeader(t *testing.T, script string) *serverProcess {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "/bin/sh", "-c", script) //nolint:gosec // G204: a fixed shell fixture chosen by this test.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start fixture leader: %v", err)
	}
	sp := newServerProcess(cmd, nil, nil)
	t.Cleanup(func() {
		// Belt-and-suspenders: if a subtest returned before reaping, kill the
		// group. Idempotent with an earlier forceReap via the exited channel.
		select {
		case <-sp.exited:
		default:
			if err := syscall.Kill(-sp.pgid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
				t.Logf("cleanup kill fixture group %d: %v", sp.pgid, err)
			}
			<-sp.exited
		}
	})
	return sp
}

// reapedGroupGone reports whether the leader's process group has no members
// left, bounded by the suite's poll deadline.
func (sp *serverProcess) reapedGroupGone(t *testing.T) bool {
	t.Helper()
	return waitUntil(func() bool { return errors.Is(syscall.Kill(-sp.pgid, 0), syscall.ESRCH) })
}
