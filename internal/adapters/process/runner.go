package process

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"github.com/johnlanda/hop/internal/app"
)

const (
	// maxCapturedBytes bounds each captured output stream. A child that
	// writes more keeps running with its pipe drained, but only the first
	// maxCapturedBytes per stream are returned.
	maxCapturedBytes = 1 << 20
	// reapTimeout bounds the cancellation teardown: the wait for the
	// SIGKILLed leader to be reaped and the poll for its group to empty.
	reapTimeout = 10 * time.Second
	// pollInterval paces the bounded group-absence poll.
	pollInterval = 25 * time.Millisecond
	// anchorSleepSeconds is the group anchor's sleep argument. It bounds how
	// long an anchor orphaned by a crashed controller keeps the group id
	// pinned before exiting on its own.
	anchorSleepSeconds = "100000"
)

// Runner implements app.CommandRunner: it runs one argv as the leader of a
// new process group with the complete environment given, captures bounded
// output, and on context cancellation kills the whole group and reaps the
// leader within a bounded wait.
type Runner struct{}

var _ app.CommandRunner = Runner{}

// CancellationError is the typed outcome of a Run whose context was
// canceled: the process group was SIGKILLed and the leader reaped. It wraps
// the context's cause, so errors.Is against context.Canceled or
// context.DeadlineExceeded still holds.
type CancellationError struct {
	// Cause is the canceled context's cause.
	Cause error
	// GroupEmptied reports whether the killed group was observed empty
	// (signal-0 probe answered ESRCH) within the bounded wait. False means
	// a member may survive the kill window; the recorded group id remains
	// the retirement handle.
	GroupEmptied bool
}

// Error describes the cancellation and what the teardown observed.
func (e *CancellationError) Error() string {
	return fmt.Sprintf("command canceled and its process group killed (group emptied: %t): %v", e.GroupEmptied, e.Cause)
}

// Unwrap exposes the context cause.
func (e *CancellationError) Unwrap() error { return e.Cause }

// Run executes cmd.Argv as the leader of a new process group. The argv's
// executable must be an absolute path (bare and relative names do not pin
// which binary runs); cmd.Env is the complete environment — nothing is
// inherited, and a nil Env runs with an empty environment. A completed
// command is a result, not an error: ExitCode carries the exit status, or
// -1 for a termination by a signal Run did not send. On ctx cancellation
// the whole group is SIGKILLed, the leader is reaped within a bounded wait,
// and the returned error is a *CancellationError alongside the output
// captured so far.
//
// Group discipline: the leader is a fresh group leader (Setpgid, pgid ==
// leader pid), a sleep anchor is joined into that same group and kept
// unreaped until after the one group SIGKILL, and a single goroutine owns
// the leader's Wait — so the group signal can never reach a recycled group
// id, and no pid is ever waited twice. If the controller process dies
// mid-run, the orphaned anchor keeps the group id pinned (for at most
// anchorSleepSeconds) so a successor's retirement of the recorded group
// cannot hit a recycled id either.
func (Runner) Run(ctx context.Context, cmd app.Command) (app.CommandResult, error) {
	if len(cmd.Argv) == 0 {
		return app.CommandResult{}, errors.New("run: argv is empty")
	}
	if !filepath.IsAbs(cmd.Argv[0]) {
		return app.CommandResult{}, fmt.Errorf("run: executable %q is not an absolute path; bare and relative names do not pin which binary runs", cmd.Argv[0])
	}
	if err := ctx.Err(); err != nil {
		return app.CommandResult{}, fmt.Errorf("run: context already canceled, nothing started: %w", err)
	}
	stdout := &boundedBuffer{limit: maxCapturedBytes}
	stderr := &boundedBuffer{limit: maxCapturedBytes}
	// The context here is deliberately not ctx: cancellation must kill the
	// whole group, in the reap order below, never exec's leader-only kill.
	leader := exec.CommandContext(context.Background(), cmd.Argv[0], cmd.Argv[1:]...) //nolint:gosec,contextcheck // G204: the argv is chosen by the caller from the run's frozen policy, and argv[0] is enforced absolute above. contextcheck: ctx cancellation is owned by the group-kill select below, never by exec's per-process kill.
	leader.Args = cmd.Argv
	leader.Dir = cmd.Dir
	leader.Env = append([]string{}, cmd.Env...)
	leader.Stdout = stdout
	leader.Stderr = stderr
	leader.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	start := time.Now()
	if err := leader.Start(); err != nil {
		return app.CommandResult{}, fmt.Errorf("start %s: %w", cmd.Argv[0], err)
	}
	pgid := leader.Process.Pid

	// Join the anchor before any Wait exists: until the leader is reaped it
	// pins its own group, so this window is race-free, and from here on the
	// unreaped anchor pins it.
	anchor, anchorErr := startAnchor(pgid) //nolint:contextcheck // the anchor must outlive ctx: it pins the group id through the cancellation teardown and is retired explicitly, never by a context.
	if anchorErr != nil {
		if isEmptyGroupJoinError(anchorErr) {
			// The group is already empty: the leader exited (leaving no
			// member) before the anchor could join. There is nothing left to
			// signal; collect the completed result.
			return resultFromWait(leader.Wait(), stdout, stderr, time.Since(start))
		}
		// The group cannot be pinned, so cancellation could not be enforced
		// safely. The leader is still unreaped (no Wait has run), so the
		// group kill is safe now; then reap and fail.
		killErr := killGroup(pgid)
		_, waitErr := resultFromWait(leader.Wait(), stdout, stderr, time.Since(start))
		return app.CommandResult{}, errors.Join(fmt.Errorf("anchor process group %d: %w", pgid, anchorErr), killErr, waitErr)
	}

	waitDone := make(chan error, 1)
	go func() { waitDone <- leader.Wait() }() // the single owner of the leader's reap

	select {
	case waitErr := <-waitDone:
		duration := time.Since(start)
		result, runErr := resultFromWait(waitErr, stdout, stderr, duration)
		if anchorRetireErr := retireAnchor(anchor); anchorRetireErr != nil {
			return result, errors.Join(runErr, anchorRetireErr)
		}
		return result, runErr
	case <-ctx.Done():
		return cancelRun(ctx, leader, anchor, pgid, waitDone, stdout, stderr, start)
	}
}

// cancelRun tears the canceled command's group down: one group SIGKILL while
// the anchor provably pins the group id, a bounded reap of the leader, the
// anchor's reap, and a bounded poll for group absence.
func cancelRun(ctx context.Context, leader, anchor *exec.Cmd, pgid int, waitDone <-chan error, stdout, stderr *boundedBuffer, start time.Time) (app.CommandResult, error) {
	killErr := killGroup(pgid)
	cancellation := &CancellationError{Cause: context.Cause(ctx)}
	select {
	case <-waitDone:
	case <-time.After(reapTimeout):
		// The SIGKILLed leader was not reaped within the bound. The output
		// buffers may still be written by exec's copiers, so they are not
		// read; the anchor stays unreaped and keeps the group id pinned for
		// a later retirement of the recorded group.
		return app.CommandResult{}, errors.Join(fmt.Errorf("canceled command's leader (group %d) was not reaped within %s", pgid, reapTimeout), killErr, cancellation)
	}
	anchorRetireErr := retireAnchor(anchor) // the anchor took the group SIGKILL; this is its single reap
	cancellation.GroupEmptied = awaitGroupGone(pgid)
	result := app.CommandResult{
		ExitCode: leader.ProcessState.ExitCode(),
		Stdout:   stdout.bytes(),
		Stderr:   stderr.bytes(),
		Duration: time.Since(start),
	}
	if killErr != nil || anchorRetireErr != nil {
		return result, errors.Join(cancellation, killErr, anchorRetireErr)
	}
	return result, cancellation
}

// resultFromWait maps one reaped completion. A clean or nonzero exit is a
// result with that exit code and no error; a termination by a signal is a
// result with ExitCode -1 (the ProcessState convention) and no error — the
// caller judges outcomes by exit code; any other wait failure is an error.
func resultFromWait(waitErr error, stdout, stderr *boundedBuffer, duration time.Duration) (app.CommandResult, error) {
	result := app.CommandResult{Stdout: stdout.bytes(), Stderr: stderr.bytes(), Duration: duration}
	if waitErr == nil {
		return result, nil
	}
	if exitErr, ok := errors.AsType[*exec.ExitError](waitErr); ok {
		result.ExitCode = exitErr.ExitCode()
		return result, nil
	}
	return result, fmt.Errorf("wait: %w", waitErr)
}

// startAnchor launches a sleep into the existing process group pgid, so an
// unreaped member of ours pins the group id until retirement. Joining an
// existing group fails (EPERM or ESRCH) once that group is empty, which the
// caller classifies with isEmptyGroupJoinError. The anchor gets no stdio,
// so it never holds the leader's output pipes open.
func startAnchor(pgid int) (*exec.Cmd, error) {
	sleepBin, err := exec.LookPath("sleep")
	if err != nil {
		return nil, fmt.Errorf("resolve sleep for the group anchor: %w", err)
	}
	anchor := exec.CommandContext(context.Background(), sleepBin, anchorSleepSeconds) //nolint:gosec // G204: a fixed sleep binary with a fixed argument; the anchor only pins the process group id.
	anchor.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pgid: pgid}
	if err := anchor.Start(); err != nil {
		return nil, err
	}
	return anchor, nil
}

// isEmptyGroupJoinError reports whether an anchor start failure means the
// target group has no member left: setpgid into an existing group answers
// EPERM or ESRCH once the group is empty.
func isEmptyGroupJoinError(err error) bool {
	return errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.ESRCH)
}

// retireAnchor kills and reaps the group anchor; it is the anchor's single
// reap owner. Killing by pid is safe (os.Process tracks its own done state),
// and a SIGKILLed anchor's wait error is the expected signal exit.
func retireAnchor(anchor *exec.Cmd) error {
	if err := anchor.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		// Cannot happen for our own live child; without the kill the Wait
		// below could block for the anchor's full sleep, so fail instead.
		return fmt.Errorf("kill group anchor: %w", err)
	}
	err := anchor.Wait()
	if err == nil {
		return nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) { // the SIGKILLed anchor's signal exit, expected
		return nil
	}
	return fmt.Errorf("reap group anchor: %w", err)
}

// killGroup sends the one SIGKILL to the whole group, guarded so a bogus id
// can never broadcast (pgid <= 1) or hit the runner's own group. An ESRCH
// answer means the group is already empty, which is the goal state.
func killGroup(pgid int) error {
	if !safeToSignalGroup(pgid, syscall.Getpgrp()) {
		return fmt.Errorf("refusing to signal process group %d: not a specific group this process may kill", pgid)
	}
	if err := syscall.Kill(-pgid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("kill process group %d: %w", pgid, err)
	}
	return nil
}

// safeToSignalGroup reports whether pgid is a real, specific process group
// this process may signal: it rejects non-positive values, 1 (init) and the
// caller's own group.
func safeToSignalGroup(pgid, ownGroup int) bool {
	return pgid > 1 && pgid != ownGroup
}

// awaitGroupGone polls until no process remains in the group, bounded by
// reapTimeout. Signal 0 only reads existence, so even a recycled group id
// cannot be signaled by the probe; ESRCH means every member has exited and
// been reaped.
func awaitGroupGone(pgid int) bool {
	deadline := time.Now().Add(reapTimeout)
	for {
		if errors.Is(syscall.Kill(-pgid, 0), syscall.ESRCH) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(pollInterval)
	}
}

// boundedBuffer keeps the first limit bytes written and discards the rest,
// so a torrential child cannot grow the captured output without bound.
// Write never fails, which keeps the child's pipe drained to the end.
type boundedBuffer struct {
	limit int
	data  []byte
}

// Write records up to the remaining capacity and reports the full length as
// written.
func (b *boundedBuffer) Write(p []byte) (int, error) {
	if room := b.limit - len(b.data); room > 0 {
		b.data = append(b.data, p[:min(room, len(p))]...)
	}
	return len(p), nil
}

// bytes returns the captured prefix.
func (b *boundedBuffer) bytes() []byte { return b.data }
