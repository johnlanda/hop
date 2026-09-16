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
	// maxCapturedBytes is the default bound of each captured output stream,
	// used when a command sets no MaxOutputBytes of its own. A child that
	// writes more keeps running with its pipe drained, but only the first
	// bound's bytes per stream are returned, and the result reports that
	// stream truncated.
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
type Runner struct {
	// anchorPath overrides the group-anchor executable for tests; empty
	// resolves "sleep" from PATH at call time.
	anchorPath string
}

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
// -1 for a termination by a signal Run did not send. Each stream keeps its
// first cmd.MaxOutputBytes (maxCapturedBytes when 0; a negative bound is
// refused before anything starts), growing only as the child writes, and
// StdoutTruncated/StderrTruncated report any stream whose later bytes were
// discarded. On ctx cancellation the whole group is SIGKILLed, the leader
// is reaped within a bounded wait, and the returned error is a
// *CancellationError alongside the output captured so far.
//
// Group discipline: the leader is a fresh group leader (Setpgid, pgid ==
// leader pid), a sleep anchor is joined into that same group and kept
// unreaped until after the one group SIGKILL, each child has exactly one
// Wait owner, and every teardown wait is bounded — so the group signal can
// never reach a recycled group id, and no pid is ever waited twice. An
// anchor that cannot be started proves nothing about the leader; that path
// retires the group while the leader is provably unreaped and still
// returns a completed command's own result (see finishUnanchored). If the
// controller process dies mid-run, the orphaned anchor keeps the group id
// pinned (for at most anchorSleepSeconds) so a successor's retirement of
// the recorded group cannot hit a recycled id either.
func (r Runner) Run(ctx context.Context, cmd app.Command) (app.CommandResult, error) {
	if len(cmd.Argv) == 0 {
		return app.CommandResult{}, errors.New("run: argv is empty")
	}
	if !filepath.IsAbs(cmd.Argv[0]) {
		return app.CommandResult{}, fmt.Errorf("run: executable %q is not an absolute path; bare and relative names do not pin which binary runs", cmd.Argv[0])
	}
	if cmd.MaxOutputBytes < 0 {
		return app.CommandResult{}, errors.New("run: the output bound is negative")
	}
	if err := ctx.Err(); err != nil {
		return app.CommandResult{}, fmt.Errorf("run: context already canceled, nothing started: %w", err)
	}
	limit := maxCapturedBytes
	if cmd.MaxOutputBytes > 0 {
		limit = cmd.MaxOutputBytes
	}
	stdout := &boundedBuffer{limit: limit}
	stderr := &boundedBuffer{limit: limit}
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
	anchor, anchorErr := startAnchor(pgid, r.anchorPath) //nolint:contextcheck // the anchor must outlive ctx: it pins the group id through the cancellation teardown and is retired explicitly, never by a context.
	if anchorErr != nil {
		return finishUnanchored(leader, pgid, anchorErr, stdout, stderr, start)
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

// finishUnanchored settles a run whose group anchor could not be started.
// The join failure's errno proves nothing about the leader — EPERM can come
// from the anchor's own exec as well as from an empty group — so no
// completion is ever inferred from it. The leader has no Wait owner yet, so
// it is provably unreaped here: a live process or a zombie, either of which
// pins the group id, making the one group SIGKILL safe — a no-op for a
// command that already finished (a zombie's recorded exit status is
// unaffected), a retirement for one that cannot be supervised without an
// anchor. The wait that follows is bounded; its goroutine is the leader's
// single Wait owner and keeps the eventual reap past the bound.
func finishUnanchored(leader *exec.Cmd, pgid int, anchorErr error, stdout, stderr *boundedBuffer, start time.Time) (app.CommandResult, error) {
	killErr := killGroup(pgid)
	waitDone := make(chan error, 1)
	go func() { waitDone <- leader.Wait() }()
	select {
	case waitErr := <-waitDone:
		result, runErr := resultFromWait(waitErr, stdout, stderr, time.Since(start))
		if runErr == nil && result.ExitCode >= 0 && killErr == nil {
			// The leader had already exited on its own — its recorded exit
			// status survived the group kill — so the completed command's
			// result stands.
			return result, nil
		}
		// The leader died by a signal (ours, having been alive when the
		// group was retired, or an indistinguishable external one) or the
		// teardown itself failed: the run was not supervised to completion.
		return result, errors.Join(fmt.Errorf("run not supervised: anchor process group %d: %w", pgid, anchorErr), killErr, runErr)
	case <-time.After(reapTimeout):
		// The output buffers may still be written by exec's copiers, so
		// they are not read; the wait goroutine keeps the eventual reap.
		return app.CommandResult{}, errors.Join(fmt.Errorf("anchor process group %d: %w", pgid, anchorErr), killErr, fmt.Errorf("killed leader (group %d) was not reaped within %s", pgid, reapTimeout))
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
		// read. The anchor is retired within its own bound so it keeps a
		// reap owner; the unreaped leader itself still pins the group id
		// for a later retirement of the recorded group.
		anchorRetireErr := retireAnchor(anchor)
		return app.CommandResult{}, errors.Join(fmt.Errorf("canceled command's leader (group %d) was not reaped within %s", pgid, reapTimeout), killErr, anchorRetireErr, cancellation)
	}
	anchorRetireErr := retireAnchor(anchor) // the anchor took the group SIGKILL; this is its single reap
	cancellation.GroupEmptied = awaitGroupGone(pgid)
	result := capturedResult(stdout, stderr, time.Since(start))
	result.ExitCode = leader.ProcessState.ExitCode()
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
	result := capturedResult(stdout, stderr, duration)
	if waitErr == nil {
		return result, nil
	}
	if exitErr, ok := errors.AsType[*exec.ExitError](waitErr); ok {
		result.ExitCode = exitErr.ExitCode()
		return result, nil
	}
	return result, fmt.Errorf("wait: %w", waitErr)
}

// capturedResult is a result carrying the reaped command's captured
// streams, each with whether its bound discarded bytes.
func capturedResult(stdout, stderr *boundedBuffer, duration time.Duration) app.CommandResult {
	return app.CommandResult{
		Stdout: stdout.bytes(), StdoutTruncated: stdout.truncated,
		Stderr: stderr.bytes(), StderrTruncated: stderr.truncated,
		Duration: duration,
	}
}

// startAnchor launches a sleep into the existing process group pgid, so an
// unreaped member of ours pins the group id until retirement. A start
// failure carries no usable meaning about the leader (an empty target
// group and the anchor's own exec failure are indistinguishable at this
// level); the caller settles that path with finishUnanchored. The anchor
// gets no stdio, so it never holds the leader's output pipes open.
func startAnchor(pgid int, anchorPath string) (*exec.Cmd, error) {
	sleepBin := anchorPath
	if sleepBin == "" {
		resolved, err := exec.LookPath("sleep")
		if err != nil {
			return nil, fmt.Errorf("resolve sleep for the group anchor: %w", err)
		}
		sleepBin = resolved
	}
	anchor := exec.CommandContext(context.Background(), sleepBin, anchorSleepSeconds) //nolint:gosec // G204: a fixed sleep binary with a fixed argument; the anchor only pins the process group id.
	anchor.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pgid: pgid}
	if err := anchor.Start(); err != nil {
		return nil, err
	}
	return anchor, nil
}

// retireAnchor kills and reaps the group anchor within a bounded wait. The
// goroutine spawned here is the anchor's single Wait owner and keeps the
// eventual reap even past the bound, so no path leaves the anchor without
// an owner. Killing by pid is safe (os.Process tracks its own done state),
// and a SIGKILLed anchor's wait error is the expected signal exit.
func retireAnchor(anchor *exec.Cmd) error {
	waitDone := make(chan error, 1)
	go func() { waitDone <- anchor.Wait() }()
	killErr := anchor.Process.Kill()
	if killErr != nil && errors.Is(killErr, os.ErrProcessDone) {
		killErr = nil
	}
	select {
	case err := <-waitDone:
		var exitErr *exec.ExitError
		if err != nil && !errors.As(err, &exitErr) {
			return errors.Join(fmt.Errorf("reap group anchor: %w", err), killErr)
		}
		return killErr
	case <-time.After(reapTimeout):
		return errors.Join(fmt.Errorf("group anchor (pid %d) was not reaped within %s; its wait owner keeps the eventual reap", anchor.Process.Pid, reapTimeout), killErr)
	}
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
// so a torrential child cannot grow the captured output without bound;
// truncated records that some byte was discarded. Its storage grows only
// as bytes arrive, doubling at most, and never beyond limit, so a large
// bound costs nothing until a child writes that much. Write never fails,
// which keeps the child's pipe drained to the end.
type boundedBuffer struct {
	limit     int
	data      []byte
	truncated bool
}

// Write records up to the remaining capacity and reports the full length as
// written.
func (b *boundedBuffer) Write(p []byte) (int, error) {
	keep := min(max(b.limit-len(b.data), 0), len(p))
	if keep < len(p) {
		b.truncated = true
	}
	if need := len(b.data) + keep; need > cap(b.data) {
		grown := make([]byte, len(b.data), min(b.limit, max(need, 2*cap(b.data))))
		copy(grown, b.data)
		b.data = grown
	}
	b.data = append(b.data, p[:keep]...)
	return len(p), nil
}

// bytes returns the captured prefix.
func (b *boundedBuffer) bytes() []byte { return b.data }
