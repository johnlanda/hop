package integration

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// TestSpikeRestartBarrier proves the S3 restart barrier waits for the server
// leader to ACTUALLY exit before treating a graceful stop as complete — the
// guarantee the restore evidence depends on — and that a leader which never
// exits is force-killed rather than silently accepted. It exercises the
// barrier's building blocks (the leaderExited channel and retireGroup) against
// fixture leaders, with no real herdr binary.
func TestSpikeRestartBarrier(t *testing.T) {
	t.Run("leaderExited closes only after a release-gated exit, and the save survives", func(t *testing.T) {
		dir := t.TempDir()
		releaseFile := filepath.Join(dir, "release")
		saveFile := filepath.Join(dir, "delayed-save")
		// The leader blocks until the test releases it, then writes its save file
		// and exits — modeling Herdr saving only as its run loop exits. The
		// barrier (leaderExited) must close only after that exit, so the save
		// file must exist when it closes; this is a release barrier, not a timing
		// assertion.
		leader := startFixtureLeader(t, "while [ ! -f '"+releaseFile+"' ]; do sleep 0.02; done; : > '"+saveFile+"'; exit 0")

		select {
		case <-leader.leaderExited:
			t.Fatal("the barrier closed before the leader was released")
		default:
		}

		if err := os.WriteFile(releaseFile, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		<-leader.leaderExited
		if _, err := os.Stat(saveFile); err != nil {
			t.Errorf("the leader's release-gated save is missing after the barrier: %v", err)
		}
		if ps := leader.leaderCmd.ProcessState; ps == nil || !ps.Exited() || ps.ExitCode() != 0 {
			t.Errorf("leader exit status = %v, want a clean exit(0)", ps)
		}
		if leader.takeTeardown() {
			retireInTest(t, leader)
		}
		if !leader.groupGone(t) {
			t.Error("group not gone after retiring a cleanly exited leader")
		}
	})

	t.Run("a leader that never exits is force-killed by retirement", func(t *testing.T) {
		leader := startFixtureLeader(t, "sleep 300")
		select {
		case <-leader.leaderExited:
			t.Fatal("leaderExited closed for a leader that never exits")
		case <-time.After(300 * time.Millisecond):
		}
		// retireGroup is what restart runs on the inconclusive branch: it kills
		// the group while the anchor pins it, then reaps.
		if leader.takeTeardown() {
			retireInTest(t, leader)
		}
		if !leader.groupGone(t) {
			t.Error("the force-killed leader's group is not gone")
		}
	})
}

// TestSpikeGroupAnchor proves the anchor invariant: an unreaped anchor keeps
// the group alive (and signal-safe) after the leader itself has been reaped,
// retirement empties the group, and a leader that has already exited cannot be
// anchored — the case start() and runInOwnedGroup report as inconclusive.
func TestSpikeGroupAnchor(t *testing.T) {
	t.Run("the anchor pins the group after the leader exits, then retirement empties it", func(t *testing.T) {
		leader := startFixtureLeader(t, "exit 0")
		<-leader.leaderExited // the leader is reaped by its owning goroutine
		// The anchor, still unreaped, keeps the group non-empty even though the
		// leader is gone — this is what makes a later group SIGKILL safe.
		if errors.Is(syscall.Kill(-leader.pgid, 0), syscall.ESRCH) {
			t.Fatal("group is already empty after the leader exited; the anchor did not pin it")
		}
		if leader.takeTeardown() {
			retireInTest(t, leader)
		}
		if !leader.groupGone(t) {
			t.Error("retirement did not empty the anchored group")
		}
	})

	t.Run("a leader that already exited cannot be anchored (inconclusive)", func(t *testing.T) {
		// setpgid into pgid 1 (init, in another session) fails with EPERM — the
		// same failure start()/runInOwnedGroup get when the leader's group is
		// already gone, and which they report as an inconclusive launch rather
		// than proceeding without a pinned group.
		if _, err := startAnchor(1); err == nil {
			t.Fatal("expected startAnchor to fail when it cannot join the target group")
		} else {
			t.Logf("startAnchor into an unjoinable group failed as expected: %v", err)
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
			name:       "leader exits normally, no descendants",
			script:     "exit 0",
			timeout:    30 * time.Second,
			wantTimedO: false,
		},
		{
			// Never-exiting leader with a child holding the output pipe: the
			// deadline cancellation kills the group. A short timeout hits it fast.
			name:       "never-exiting leader, pipe-holding child",
			script:     "sleep 300 & wait",
			timeout:    time.Second,
			wantTimedO: true,
		},
		{
			// Leader exits normally while a backgrounded child keeps the output
			// pipe open: the reader drains after the child is retired.
			name:       "leader exits normally, child holds stdout open",
			script:     "sleep 300 & exit 0",
			timeout:    30 * time.Second,
			wantTimedO: false,
		},
		{
			// Leader exits normally while a backgrounded child has closed the
			// output pipe but stays alive: output ends at once and the post-exit
			// retirement kills the surviving child.
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

// TestSpikeOwnedGroupDrainDeadline proves the output-drain deadline: a write
// descriptor held OUTSIDE any killed group keeps the reader from reaching EOF,
// so joinReader must fire its deadline, close the read end to unblock the
// reader, and join it rather than hang. The write end is owned and cleaned up
// by this test, so nothing leaks.
func TestSpikeOwnedGroupDrainDeadline(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeOrLog(t, "test-owned write end", w) }) // this test owns and retires w

	var cerr error
	readDone := make(chan struct{})
	go func() {
		_, cerr = io.Copy(io.Discard, r)
		close(readDone)
	}()

	// w is still open, so the reader cannot reach EOF on its own; the deadline
	// must fire.
	if joinReader(t, readDone, r, 200*time.Millisecond) {
		t.Error("joinReader reported the drain finished, but the write end was still held open")
	}
	// joinReader closed r and joined the reader; the goroutine has returned.
	select {
	case <-readDone:
	default:
		t.Error("the reader was not joined after the drain deadline")
	}
	if cerr != nil && !errors.Is(cerr, os.ErrClosed) {
		t.Logf("drain copy after deadline: %v", cerr)
	}
}

// startFixtureLeader starts a /bin/sh leader in its own process group, anchors
// that group, and wraps both as a serverProcess, so the ownership helpers can
// be exercised without a herdr binary. It has no log files. Its cleanup retires
// the group at most once.
func startFixtureLeader(t *testing.T, script string) *serverProcess {
	t.Helper()
	// context.Background, not t.Context: the leader is owned by its single Wait
	// goroutine and retired explicitly, with no CommandContext watcher.
	cmd := exec.CommandContext(context.Background(), "/bin/sh", "-c", script) //nolint:gosec // G204: a fixed shell fixture chosen by this test.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start fixture leader: %v", err)
	}
	anchor, err := startAnchor(cmd.Process.Pid)
	if err != nil {
		retireUnanchoredLeader(t, cmd)
		t.Fatalf("anchor could not join fixture leader group: %v", err)
	}
	sp := newServerProcess(cmd, anchor, nil, nil)
	t.Cleanup(func() {
		if sp.takeTeardown() {
			retireInTest(t, sp)
		}
	})
	return sp
}

// retireInTest retires a fixture group and reports a failure.
func retireInTest(t *testing.T, sp *serverProcess) {
	t.Helper()
	if err := sp.retireGroup(); err != nil {
		t.Errorf("retire group: %v", err)
	}
}

// groupGone reports whether the leader's process group has no members left,
// bounded by the suite's poll deadline.
func (sp *serverProcess) groupGone(t *testing.T) bool {
	t.Helper()
	return groupIsGone(sp.pgid)
}
