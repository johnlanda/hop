package main

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/johnlanda/hop/internal/app"
)

const (
	// heartbeatInterval paces Controller.Heartbeat safely below the store's
	// 30s lease TTL (docs/plan/phase-2-design.md section 4: TTL 30s,
	// interval 10s). The heartbeat runs concurrently with the round loop so
	// a long check execution never outlives the TTL.
	heartbeatInterval = 10 * time.Second
	// loopPollInterval paces the foreground rounds: the controller consumes
	// durable pending check requests by polling the store on the design's
	// 2s interval (docs/plan/phase-2-design.md section 7).
	loopPollInterval = 2 * time.Second
)

// Terminal run states, as the status view renders them.
const (
	runStateCompleted = "completed"
	runStateFailed    = "failed"
	runStateStopped   = "stopped"
	runStateStopping  = "stopping"
)

// isTerminalRunState reports whether state ends the foreground loop.
func isTerminalRunState(state string) bool {
	return state == runStateCompleted || state == runStateFailed || state == runStateStopped
}

// exitForRunState maps a final run state to the design's exit code: 0 only
// on completed, 1 on failed, stopped or anything else.
func exitForRunState(state string) int {
	if state == runStateCompleted {
		return exitOK
	}
	return exitFailure
}

// runHeartbeats extends handle's lease every heartbeatInterval until ctx
// ends, delivering the first heartbeat failure on the returned channel. A
// failed heartbeat has already canceled the handle's dispatch scope inside
// the application, so in-flight external calls die with the lost lease.
func runHeartbeats(ctx context.Context, d *deps, ctrl controllerAPI, handle app.RunHandle) <-chan error { //nolint:gocritic // hugeParam: RunHandle is the app-defined opaque token, passed by value as every Controller method takes it.
	failed := make(chan error, 1)
	go func() {
		for {
			if err := d.wait(ctx, heartbeatInterval); err != nil {
				return
			}
			if err := ctrl.Heartbeat(ctx, handle); err != nil {
				failed <- err
				return
			}
		}
	}()
	return failed
}

// loopResult is how one foreground controller loop ended: Detached when the
// context was canceled (SIGINT/SIGTERM — never a stop), FinalState when the
// run reached a terminal state.
type loopResult struct {
	FinalState string
	Detached   bool
}

// runControllerLoop drives one held run in the foreground until the run
// reaches a terminal state or ctx is canceled: it prints one line per run
// transition, heartbeats concurrently on the design's interval, drives
// launch-claim corroboration while the attempt is launching or
// relaunching, routes to DriveStop as soon as a stop request is visible,
// and consumes pending check requests through ClaimAndRunCheck on the poll
// interval. It never sleeps outside d.wait, so tests pace it with a fake.
func runControllerLoop(ctx context.Context, d *deps, ctrl controllerAPI, handle app.RunHandle, runID, label, hopPath string, stdout io.Writer) (loopResult, error) { //nolint:gocritic // hugeParam: RunHandle is the app-defined opaque token, passed by value as every Controller method takes it.
	heartbeatFailed := runHeartbeats(ctx, d, ctrl, handle)
	var (
		lastState    string
		lastProgress app.LaunchProgress
		spawnEnv     []string
	)
	for {
		select {
		case err := <-heartbeatFailed:
			return loopResult{}, fmt.Errorf("heartbeat failed, the lease is lost and in-flight work is canceled: %w", err)
		case <-ctx.Done():
			return loopResult{Detached: true}, nil
		default:
		}

		status, err := ctrl.Status(ctx, app.StatusRequest{RunID: runID})
		if err != nil {
			return loopResult{}, fmt.Errorf("load run status: %w", err)
		}
		detail := status.Detail
		if detail == nil {
			return loopResult{}, fmt.Errorf("run %s has no status detail", runID)
		}
		if detail.State != lastState {
			if _, werr := fmt.Fprintf(stdout, "run %s %s\n", label, detail.State); werr != nil {
				return loopResult{}, werr
			}
			lastState = detail.State
		}
		if isTerminalRunState(detail.State) {
			return loopResult{FinalState: detail.State}, nil
		}

		switch {
		case detail.StopRequested || detail.State == runStateStopping:
			report, stopErr := ctrl.DriveStop(ctx, handle)
			if stopErr != nil {
				return loopResult{}, fmt.Errorf("drive stop: %w", stopErr)
			}
			if report.Terminated {
				if _, werr := fmt.Fprintf(stdout, "run %s %s\n", label, report.RunState); werr != nil {
					return loopResult{}, werr
				}
				return loopResult{FinalState: report.RunState}, nil
			}
		default:
			if detail.AttemptState == "launching" || detail.AttemptState == "relaunching" {
				progress, corrErr := ctrl.CorroborateLaunch(ctx, handle)
				if corrErr != nil {
					return loopResult{}, fmt.Errorf("corroborate launch: %w", corrErr)
				}
				if progress != lastProgress {
					if _, werr := fmt.Fprintf(stdout, "launch %s\n", describeLaunchProgress(progress)); werr != nil {
						return loopResult{}, werr
					}
					lastProgress = progress
				}
			}
			if spawnEnv == nil {
				spawnEnv, err = ctrl.CheckSpawnEnvironment(ctx, handle, d.environ())
				if err != nil {
					return loopResult{}, fmt.Errorf("compose check spawn environment: %w", err)
				}
			}
			report, checkErr := ctrl.ClaimAndRunCheck(ctx, handle, hopPath, spawnEnv)
			if checkErr != nil {
				return loopResult{}, fmt.Errorf("run check: %w", checkErr)
			}
			if report.Ran {
				if _, werr := fmt.Fprintf(stdout, "check %s %s\n", report.OperationID, describeCheckReport(&report)); werr != nil {
					return loopResult{}, werr
				}
			}
		}

		if err := d.wait(ctx, loopPollInterval); err != nil {
			return loopResult{Detached: true}, nil
		}
	}
}

// describeLaunchProgress renders one CorroborateLaunch progress value as
// the loop's transition line.
func describeLaunchProgress(progress app.LaunchProgress) string {
	switch progress {
	case app.LaunchNeedsInteraction:
		return "blocked, needs interaction: the observed process does not match the claimed exec (see hop status)"
	case app.LaunchOverdue:
		return "overdue: no launch claim within the deadline; the operation is reconciling and is never re-sent"
	default:
		return string(progress)
	}
}

// describeCheckReport renders one executed check's outcome line.
func describeCheckReport(report *app.CheckReport) string {
	switch {
	case report.Interrupted:
		return "interrupted"
	case report.Unknown:
		return "unknown outcome; the run stays actionable (see hop status)"
	case report.Passed:
		return "passed"
	default:
		return "failed"
	}
}

// detachAndReport releases the run without stopping it after a
// SIGINT/SIGTERM detach and prints the resume instruction. The detach uses
// a fresh bounded context because the loop's own context is already
// canceled.
func detachAndReport(ctx context.Context, ctrl controllerAPI, handle app.RunHandle, runID string, stdout io.Writer) error { //nolint:gocritic // hugeParam: RunHandle is the app-defined opaque token, passed by value as every Controller method takes it.
	// The loop's context is already canceled by the detach signal; the
	// release itself runs bounded, detached from that cancellation.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := ctrl.Detach(ctx, handle); err != nil {
		if _, werr := fmt.Fprintf(stdout, "detach: %v\n", err); werr != nil {
			return werr
		}
	}
	_, werr := fmt.Fprintf(stdout, "detached; the run keeps its state and the worker keeps running — resume with: hop resume %s\n", runID)
	return werr
}

// releaseQuietly releases the lease on a non-detach exit path (terminal
// state or error), best-effort: the run's state already tells the story.
func releaseQuietly(ctx context.Context, ctrl controllerAPI, handle app.RunHandle) { //nolint:gocritic // hugeParam: RunHandle is the app-defined opaque token, passed by value as every Controller method takes it.
	// The caller's context may already be canceled or expired on this exit
	// path; the best-effort release runs bounded, detached from that.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	_ = ctrl.Detach(ctx, handle) //nolint:errcheck // best-effort release; a lost lease releases itself by expiry.
}

// resolveRunArg accepts a run argument as a full UUID or the
// repository-scoped r<seq> label hop status prints, resolving a label
// through the repository's run listing.
func resolveRunArg(ctx context.Context, ctrl controllerAPI, repositoryRoot, arg string) (string, error) {
	if !isSeqLabel(arg) {
		return arg, nil
	}
	seq, err := strconv.Atoi(arg[1:])
	if err != nil {
		return "", fmt.Errorf("run label %q: %w", arg, err)
	}
	listing, err := ctrl.Status(ctx, app.StatusRequest{RepositoryRoot: repositoryRoot})
	if err != nil {
		return "", fmt.Errorf("list runs to resolve %q: %w", arg, err)
	}
	for _, r := range listing.Runs {
		if r.Sequence == seq {
			return r.RunID, nil
		}
	}
	return "", fmt.Errorf("no run %s in repository %s", arg, repositoryRoot)
}

// isSeqLabel reports whether arg has the r<seq> shape: a leading 'r'
// followed by one or more digits.
func isSeqLabel(arg string) bool {
	if len(arg) < 2 || arg[0] != 'r' {
		return false
	}
	return strings.Trim(arg[1:], "0123456789") == ""
}

// seqLabel renders a run's repository-scoped label.
func seqLabel(sequence int) string {
	return "r" + strconv.Itoa(sequence)
}
