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
// which never exits is force-killed rather than silently accepted. It uses
// fixture leaders, not a real herdr binary, so it exercises the barrier
// mechanism directly.
func TestSpikeRestartWaitsForGracefulExit(t *testing.T) {
	t.Run("waits for the release-gated exit, and the delayed save survives", func(t *testing.T) {
		dir := t.TempDir()
		releaseFile := filepath.Join(dir, "release")
		saveFile := filepath.Join(dir, "delayed-save")
		// The leader blocks until the test releases it, then writes its save file
		// and exits — modeling Herdr saving only as its run loop exits. The
		// barrier must return only after that exit, so the save file must exist
		// when it returns; this is a release barrier, not a timing assertion.
		leader := startFixtureLeader(t, "while [ ! -f '"+releaseFile+"' ]; do sleep 0.02; done; : > '"+saveFile+"'; exit 0")

		done := make(chan bool, 1)
		go func() { done <- awaitGracefulExit(leader, conditionTimeout) }()

		// The barrier must not have returned while the leader is still gated.
		select {
		case <-done:
			t.Fatal("the barrier returned before the leader was released")
		default:
		}

		if err := os.WriteFile(releaseFile, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if !<-done {
			t.Fatal("the barrier reported no exit for a leader that exits on its own")
		}
		// The save the leader wrote just before exiting is present, proving the
		// barrier waited for that exit rather than returning early.
		if _, err := os.Stat(saveFile); err != nil {
			t.Errorf("the leader's release-gated save is missing after the barrier: %v", err)
		}
	})

	t.Run("a leader that never exits is force-killed", func(t *testing.T) {
		leader := startFixtureLeader(t, "sleep 300")
		if awaitGracefulExit(leader, 300*time.Millisecond) {
			t.Fatal("the barrier reported a graceful exit for a leader that never exits")
		}
		// The forced path (what restart runs on the inconclusive branch) tears
		// the group down; the leader was never reaped, so the signal is safe.
		srv := &testServer{}
		srv.forceReap(t, leader)
		if !leader.groupGone(t) {
			t.Error("the force-killed leader's group is not gone")
		}
	})

	t.Run("a leader that exits as cleanup starts is reaped without an unsafe signal", func(t *testing.T) {
		// The leader exits promptly on its own. By the time forceReap runs it may
		// already be an unreaped zombie or already reaped by a prior tryReap;
		// either way forceReap must retire it and its group without signaling a
		// possibly recycled pgid.
		leader := startFixtureLeader(t, "exit 0")
		// Race the reap: let it exit, then force-reap.
		if !awaitGracefulExit(leader, conditionTimeout) {
			t.Fatal("leader did not exit on its own")
		}
		srv := &testServer{}
		srv.forceReap(t, leader) // leader already reaped: must be a no-op signal + group drain
		if !leader.groupGone(t) {
			t.Error("group not gone after a leader that exited on its own")
		}
	})
}

// TestSpikeOwnedGroupTeardown validates the owned-group lifecycle used by the
// live (S4) probe, without any real harness: whether the leader is killed on
// the deadline or exits normally, any surviving child is retired and the group
// is left empty, so a real Claude child could not outlive the probe or block
// its output draining. runInOwnedGroup asserts group-empty internally, so a
// clean return with the expected timedOut value is the teardown proof.
func TestSpikeOwnedGroupTeardown(t *testing.T) {
	cases := []struct {
		name       string
		script     string
		timeout    time.Duration
		wantTimedO bool
	}{
		{
			// Never-exiting leader with a child holding the output pipe: the
			// deadline cancellation kills the group. A short timeout hits the
			// deadline quickly.
			name:       "never-exiting leader, pipe-holding child",
			script:     "sleep 300 & wait",
			timeout:    time.Second,
			wantTimedO: true,
		},
		{
			// Leader exits normally while a backgrounded child keeps the output
			// pipe open: WaitDelay (5s) bounds draining, then the post-exit
			// retirement kills the surviving child. The timeout must exceed
			// WaitDelay so the deadline does not fire first.
			name:       "leader exits normally, child holds stdout open",
			script:     "sleep 300 & exit 0",
			timeout:    30 * time.Second,
			wantTimedO: false,
		},
		{
			// Leader exits normally while a backgrounded child has closed the
			// output pipe but stays alive: CombinedOutput returns at once and the
			// post-exit retirement kills the surviving child.
			name:       "leader exits normally, child closed stdout but alive",
			script:     "sleep 300 >/dev/null 2>&1 & exit 0",
			timeout:    30 * time.Second,
			wantTimedO: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, timedOut, err := runInOwnedGroup(t, tc.timeout, "/bin/sh", nil, t.TempDir(), "-c", tc.script)
			if timedOut != tc.wantTimedO {
				t.Errorf("timedOut = %t, want %t (err=%v, out=%q)", timedOut, tc.wantTimedO, err, out)
			}
			// runInOwnedGroup asserts the owned group is empty before returning;
			// reaching here without its t.Error is the teardown proof.
		})
	}
}

// startFixtureLeader starts a /bin/sh leader in its own process group and wraps
// it as a serverProcess, so the ownership helpers can be exercised without a
// herdr binary. It has no log files. Its cleanup retires the group at most once
// via the same lock-serialized path.
func startFixtureLeader(t *testing.T, script string) *serverProcess {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "/bin/sh", "-c", script) //nolint:gosec // G204: a fixed shell fixture chosen by this test.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start fixture leader: %v", err)
	}
	sp := newServerProcess(cmd, nil, nil)
	t.Cleanup(func() {
		if !sp.takeTeardown() {
			return
		}
		sp.killGroupIfUnreaped(t)
		if !sp.reapLeader() {
			t.Errorf("fixture leader %d was not reaped", sp.pgid)
		}
	})
	return sp
}

// groupGone reports whether the leader's process group has no members left,
// bounded by the suite's poll deadline.
func (sp *serverProcess) groupGone(t *testing.T) bool {
	t.Helper()
	return waitUntil(func() bool { return errors.Is(syscall.Kill(-sp.pgid, 0), syscall.ESRCH) })
}
