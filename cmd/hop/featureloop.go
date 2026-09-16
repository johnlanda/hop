package main

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/johnlanda/hop/internal/app"
)

// featureCheckOutcome carries one asynchronous DriveFeatureChecks
// completion, mirroring checkOutcome for the feature-mode report shape.
type featureCheckOutcome struct {
	report app.FeatureCheckReport
	err    error
}

// featureCheckDriver runs at most one DriveFeatureChecks round at a time
// in its own goroutine under a cancelable context derived from the loop's,
// mirroring checkDriver exactly (solo's own async check driver, untouched)
// so a long-running feature-mode check does not block the rest of the
// scheduling pass — the loop keeps observing a stop request and
// heartbeating while a check executes, and a stop interrupts the check
// through the app's own stop precedence instead of waiting it out.
type featureCheckDriver struct {
	cancel context.CancelFunc
	done   chan featureCheckOutcome
}

// start begins one asynchronous feature-check round; the driver must be
// idle. posted, when non-nil, is called after the outcome is in the
// channel, exactly like checkDriver.start's barrier for deterministic test
// pacing; production wiring leaves it nil.
func (c *featureCheckDriver) start(ctx context.Context, ctrl controllerAPI, handle app.RunHandle, hopPath string, spawnEnv []string, posted func()) { //nolint:gocritic // hugeParam: RunHandle is the app-defined opaque token, passed by value as every Controller method takes it.
	checkCtx, cancel := context.WithCancel(ctx)
	done := make(chan featureCheckOutcome, 1)
	c.cancel = cancel
	c.done = done
	go func() {
		defer cancel()
		report, err := ctrl.DriveFeatureChecks(checkCtx, handle, hopPath, spawnEnv)
		done <- featureCheckOutcome{report: report, err: err}
		if posted != nil {
			posted()
		}
	}()
}

// running reports whether a check round is in flight.
func (c *featureCheckDriver) running() bool { return c.done != nil }

// poll consumes a finished round without blocking; ok is false while the
// round is still running.
func (c *featureCheckDriver) poll() (featureCheckOutcome, bool) {
	if c.done == nil {
		return featureCheckOutcome{}, false
	}
	select {
	case outcome := <-c.done:
		c.done = nil
		return outcome, true
	default:
		return featureCheckOutcome{}, false
	}
}

// interrupt cancels the in-flight round, if any, and waits for it to
// return.
func (c *featureCheckDriver) interrupt() (featureCheckOutcome, bool) {
	if c.done == nil {
		return featureCheckOutcome{}, false
	}
	c.cancel()
	outcome := <-c.done
	c.done = nil
	return outcome, true
}

// runFeatureControllerLoop drives one held feature-mode run in the
// foreground until it reaches a terminal state or ctx is canceled. It
// prints one line per run transition, heartbeats concurrently on the
// design's interval exactly like the solo loop, and executes ONE
// deterministic scheduling pass per poll tick
// (docs/plan/phase-3-design.md section 6, L796-798):
//
// consume manager requests (no discrete call: hop task create/retry and
// hop plan close already commit their durable rows synchronously via
// PlanStore, outside this loop — RecomputeReleases and AssignReadyTasks
// below consume them naturally) -> retire settled sessions -> recompute
// releases -> claim/advance at most one integration -> create the review
// task when due -> drive completion (the run leaves running only inside
// this step's own re-validated guard transaction; placed here, after
// review-task creation and before assignment, since completion can only
// become reachable once every task is integrated and the review task
// exists and is settled — a placement choice, not a design-fixed
// position: DriveCompletion re-validates readiness inside its own
// transaction, so a run that becomes ready between this step and the
// next tick simply completes one tick later, never incorrectly) -> assign
// released tasks in seq order -> corroborate launches -> drive checks.
//
// Every dispatch inside these calls revalidates (heartbeat CAS + stop
// re-read) exactly as Phase 2 requires; this loop adds no revalidation of
// its own. Stop handling takes precedence over the scheduling pass,
// mirroring the solo loop's own stop-first branch. While the run is still
// launching (design L560: launching -> running is the manager's settled
// launch claim, applied by CorroborateSessionLaunches' own settlement
// path, not a discrete step here), every other step above depends on a
// live manager session that does not exist yet — RecomputeReleases has no
// released work, DriveIntegration and DriveCompletion have no integrated
// task, AssignReadyTasks has no manager to delegate from — so the pass
// runs only session-launch corroboration until the run is observed
// running.
//
// A manager whose launch exec failed fails the run through the app's
// terminal-failure path; the pass prints the solo loop's `launch failed`
// line for it, and the loop then observes the failed run and exits 1.
//
// The pass honors the reports it receives: once retirement reports a
// terminal failure settled or still due, or completion reports the run
// has left running, the rest of the pass is skipped and no new check
// round is dispatched; while the run is completing, each tick drives only
// completion retirement. The loop then observes the terminal state on a
// later tick and returns it, so the caller maps it to the normal exit
// code.
func runFeatureControllerLoop(ctx context.Context, d *deps, ctrl controllerAPI, handle app.RunHandle, runID, label, hopPath string, stdout io.Writer) (loopResult, error) { //nolint:gocritic // hugeParam: RunHandle is the app-defined opaque token, passed by value as every Controller method takes it.
	heartbeatFailed := runHeartbeats(ctx, d, ctrl, handle)
	var (
		lastState string
		spawnEnv  []string
		checks    featureCheckDriver
	)
	reportOutcome := func(outcome featureCheckOutcome) error {
		if outcome.err != nil {
			if errors.Is(outcome.err, app.ErrStopRequested) {
				return nil
			}
			if reportable := nonCancellationCauses(outcome.err); reportable != nil {
				return fmt.Errorf("run feature checks: %w", reportable)
			}
			return nil
		}
		if outcome.report.Ran {
			if _, werr := fmt.Fprintf(stdout, "check %s %s\n", outcome.report.TaskID, describeFeatureCheckReport(&outcome.report)); werr != nil {
				return werr
			}
		}
		return nil
	}
	drainChecks := func() error {
		if outcome, ok := checks.interrupt(); ok {
			return reportOutcome(outcome)
		}
		return nil
	}

	for {
		select {
		case err := <-heartbeatFailed:
			return loopResult{}, errors.Join(fmt.Errorf("heartbeat failed, the lease is lost and in-flight work is canceled: %w", err), drainChecks())
		case <-ctx.Done():
			return loopResult{Detached: true}, drainChecks()
		default:
		}

		status, err := ctrl.Status(ctx, app.StatusRequest{RunID: runID})
		if err != nil {
			return loopResult{}, errors.Join(fmt.Errorf("load run status: %w", err), drainChecks())
		}
		detail := status.Detail
		if detail == nil {
			return loopResult{}, errors.Join(fmt.Errorf("run %s has no status detail", runID), drainChecks())
		}
		if detail.State != lastState {
			if _, werr := fmt.Fprintf(stdout, "run %s %s\n", label, detail.State); werr != nil {
				return loopResult{}, errors.Join(werr, drainChecks())
			}
			lastState = detail.State
		}
		if isTerminalRunState(detail.State) {
			if drainErr := drainChecks(); drainErr != nil {
				return loopResult{}, drainErr
			}
			return loopResult{FinalState: detail.State}, nil
		}

		if detail.StopRequested || detail.State == runStateStopping {
			if outcome, ok := checks.interrupt(); ok {
				if repErr := reportOutcome(outcome); repErr != nil {
					return loopResult{}, repErr
				}
			}
			report, stopErr := ctrl.DriveFeatureStop(ctx, handle)
			if stopErr != nil {
				return loopResult{}, fmt.Errorf("drive stop: %w", stopErr)
			}
			if report.Terminated {
				if _, werr := fmt.Fprintf(stdout, "run %s %s\n", label, report.RunState); werr != nil {
					return loopResult{}, werr
				}
				return loopResult{FinalState: report.RunState}, nil
			}
		} else {
			if spawnEnv == nil {
				spawnEnv, err = ctrl.CheckSpawnEnvironment(ctx, handle, d.environ())
				if err != nil {
					return loopResult{}, errors.Join(fmt.Errorf("compose check spawn environment: %w", err), drainChecks())
				}
			}
			pass, err := runFeatureSchedulingPass(ctx, ctrl, handle, detail.State, hopPath, spawnEnv)
			for _, line := range pass.lines {
				if _, werr := fmt.Fprintln(stdout, line); werr != nil {
					return loopResult{}, errors.Join(werr, err, drainChecks())
				}
			}
			if err != nil {
				return loopResult{}, errors.Join(err, drainChecks())
			}
			if outcome, ok := checks.poll(); ok {
				if repErr := reportOutcome(outcome); repErr != nil {
					return loopResult{}, repErr
				}
			}
			if !pass.halted && !checks.running() {
				checks.start(ctx, ctrl, handle, hopPath, spawnEnv, d.checkOutcomePosted)
			}
		}

		if err := d.wait(ctx, loopPollInterval); err != nil {
			return loopResult{Detached: true}, drainChecks()
		}
	}
}

// featurePassResult reports how far one scheduling pass got.
type featurePassResult struct {
	// halted is true when the pass ended early because the run left
	// ordinary scheduling: a terminal failure settled or still due, or
	// completion under way or recorded. The loop dispatches no new check
	// round after a halted pass.
	halted bool
	// lines are the transition lines the pass observed, for the loop to
	// print: the solo loop's launch line for a failed manager launch.
	lines []string
}

// roleManager is the manager session's role as SessionLaunchProgress
// renders it.
const roleManager = "manager"

// managerLaunchLines renders, for every manager session whose launch
// corroboration reported failed, the solo loop's own launch transition
// line (`launch failed`, describeLaunchProgress): the run fails through
// the terminal-failure path, and this line is the reason the loop shows
// before it observes the failed run. Only the fixed category text is
// printed.
func managerLaunchLines(reports []app.SessionLaunchProgress) []string {
	var lines []string
	for _, report := range reports {
		if report.Role == roleManager && report.Progress == app.LaunchFailed {
			lines = append(lines, "launch "+describeLaunchProgress(report.Progress))
		}
	}
	return lines
}

// attemptLaunchLines renders the pass's attempt-launch conditions — every
// recovered launch, and every assignment whose launch did not reach an
// opened pane — plus the unattributable worktree operations, one line
// each. A launch that does not complete never ends the loop: a later pass
// recovers or settles it. Only the app's fixed detail text is printed.
func attemptLaunchLines(report *app.AssignmentReport) []string {
	lines := make([]string, 0, len(report.Launches)+len(report.Blocked))
	for i := range report.Launches {
		launch := &report.Launches[i]
		lines = append(lines, fmt.Sprintf("attempt t%da%d %s: %s", launch.TaskSeq, launch.AttemptNumber, launch.Disposition, launch.Detail))
	}
	for _, blocked := range report.Blocked {
		lines = append(lines, "worktree blocked: "+blocked)
	}
	return lines
}

// runFeatureSchedulingPass runs one deterministic scheduling-pass round
// (design section 6) up to, but not including, check-driving — which the
// caller runs asynchronously through featureCheckDriver, mirroring the
// solo loop's own separation of the synchronous pass from the async check
// round. spawnEnv is the same sanitized environment the check driver
// spawns hop check-exec with (computed once by the caller and cached
// across ticks), reused here for the integration merge's own spawn.
// Every run-fixed AssignReadyTasks field — the frozen repository and
// state roots included — comes from AssignmentDefaults: a known run
// operates on its frozen repository, never on the directory hop was
// invoked from, and AssignReadyTasks refuses any other root (fakes law
// 06FD1A61 — cmd/hop's fakeController enforces the identical contract).
// hopPath is the running binary, the one field the loop supplies itself.
//
// runState is the state the loop observed this tick. A launching run gets
// session-launch corroboration alone (every other step requires a settled
// manager session). A completing run gets completion retirement alone
// (DriveCompletion continues a completion whose retirement is still
// outstanding). A running run gets the full pass, cut short — halted —
// as soon as a report says the run left ordinary scheduling:
// RetireSettledSessions' RunFailed or RunFailing, or DriveCompletion's
// RunState other than running. AssignReadyTasks accepts only a running
// run, so it must never be reached past either report.
func runFeatureSchedulingPass(ctx context.Context, ctrl controllerAPI, handle app.RunHandle, runState, hopPath string, spawnEnv []string) (featurePassResult, error) { //nolint:gocritic // hugeParam: RunHandle is the app-defined opaque token, passed by value as every Controller method takes it.
	halted := featurePassResult{halted: true}
	switch runState {
	case runStateLaunching:
		launches, err := ctrl.CorroborateSessionLaunches(ctx, handle)
		if err != nil {
			return featurePassResult{}, fmt.Errorf("corroborate session launches: %w", err)
		}
		return featurePassResult{lines: managerLaunchLines(launches)}, nil
	case runStateCompleting:
		if _, err := ctrl.DriveCompletion(ctx, handle); err != nil {
			return halted, fmt.Errorf("drive completion: %w", err)
		}
		return halted, nil
	}

	retirement, err := ctrl.RetireSettledSessions(ctx, handle)
	if err != nil {
		return featurePassResult{}, fmt.Errorf("retire settled sessions: %w", err)
	}
	if retirement.RunFailed || retirement.RunFailing {
		return halted, nil
	}
	if _, err = ctrl.RecomputeReleases(ctx, handle); err != nil {
		return featurePassResult{}, fmt.Errorf("recompute releases: %w", err)
	}
	if _, err = ctrl.DriveIntegration(ctx, handle, hopPath, spawnEnv); err != nil {
		return featurePassResult{}, fmt.Errorf("drive integration: %w", err)
	}
	if _, err = ctrl.EnsureReviewTask(ctx, handle); err != nil {
		return featurePassResult{}, fmt.Errorf("ensure review task: %w", err)
	}
	completion, err := ctrl.DriveCompletion(ctx, handle)
	if err != nil {
		return featurePassResult{}, fmt.Errorf("drive completion: %w", err)
	}
	if completion.RunState != runStateRunning {
		return halted, nil
	}
	opts, err := ctrl.AssignmentDefaults(ctx, handle)
	if err != nil {
		return featurePassResult{}, fmt.Errorf("load assignment defaults: %w", err)
	}
	opts.HOPPath = hopPath
	opts.IntegrationHeadCommitOID, err = ctrl.ResolveIntegrationHead(ctx, handle)
	if err != nil {
		return featurePassResult{}, fmt.Errorf("resolve integration head: %w", err)
	}
	assignment, err := ctrl.AssignReadyTasks(ctx, handle, opts)
	if err != nil {
		return featurePassResult{lines: attemptLaunchLines(&assignment)}, fmt.Errorf("assign ready tasks: %w", err)
	}
	launches, err := ctrl.CorroborateSessionLaunches(ctx, handle)
	if err != nil {
		return featurePassResult{lines: attemptLaunchLines(&assignment)}, fmt.Errorf("corroborate session launches: %w", err)
	}
	result := featurePassResult{lines: append(attemptLaunchLines(&assignment), managerLaunchLines(launches)...)}
	if _, err = ctrl.PublishRunPresentation(ctx, handle); err != nil {
		return result, fmt.Errorf("publish run presentation: %w", err)
	}
	return result, nil
}

// describeFeatureCheckReport renders one executed feature-mode check's
// outcome line, mirroring describeCheckReport's solo shape.
func describeFeatureCheckReport(report *app.FeatureCheckReport) string {
	switch {
	case report.Interrupted:
		return "interrupted"
	case report.Blocked != "":
		return "blocked: " + report.Blocked
	case report.Passed:
		return "passed"
	default:
		return "failed"
	}
}

// finishFeatureControllerLoop runs the feature-mode foreground loop and
// maps its ending to the design's exit codes, mirroring
// finishControllerLoop's solo shape exactly (detach classification before
// ordinary errors, the resume instruction on detach, releaseQuietly on
// every other exit).
func finishFeatureControllerLoop(ctx context.Context, d *deps, ctrl controllerAPI, handle app.RunHandle, runID, label, hopPath string, stdout, stderr io.Writer, command string) (int, error) { //nolint:gocritic // hugeParam: RunHandle is the app-defined opaque token, passed by value as every Controller method takes it.
	result, err := runFeatureControllerLoop(ctx, d, ctrl, handle, runID, label, hopPath, stdout)
	if err != nil {
		if ctx.Err() != nil {
			if reportable := nonCancellationCauses(err); reportable != nil {
				if _, werr := fmt.Fprintf(stderr, "%s: %v\n", command, reportable); werr != nil {
					return exitFailure, werr
				}
			}
			return exitFailure, detachAndReport(ctx, ctrl, handle, runID, stdout)
		}
		releaseQuietly(ctx, ctrl, handle)
		_, werr := fmt.Fprintf(stderr, "%s: %v\n", command, err)
		return exitFailure, werr
	}
	if result.Detached {
		return exitFailure, detachAndReport(ctx, ctrl, handle, runID, stdout)
	}
	releaseQuietly(ctx, ctrl, handle)
	return exitForRunState(result.FinalState), nil
}
