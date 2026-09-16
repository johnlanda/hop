package main

import (
	"context"
	"errors"
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

// Run states the loop dispatches on, as the status view renders them.
const (
	runStateCompleted = "completed"
	runStateFailed    = "failed"
	runStateStopped   = "stopped"
	runStateStopping  = "stopping"
	// runStateLaunching names the feature-mode run state before the
	// manager's own launch claim has settled (design L560): the
	// scheduling pass runs only session-launch corroboration during it,
	// since every other step (release, integration, completion,
	// assignment) depends on a live manager session that does not exist
	// yet.
	runStateLaunching = "launching"
	// runStateRunning and runStateCompleting name the feature-mode states
	// the scheduling pass dispatches on: only a running run takes the
	// ordinary pass, and a completing run takes completion retirement
	// alone.
	runStateRunning    = "running"
	runStateCompleting = "completing"
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

// checkOutcome carries one asynchronous ClaimAndRunCheck completion.
type checkOutcome struct {
	report app.CheckReport
	err    error
}

// checkDriver runs at most one ClaimAndRunCheck at a time in its own
// goroutine under a cancelable context derived from the loop's, so the
// loop keeps observing the run — a stop request, a transition — and
// heartbeating while a long check executes. Canceling propagates into the
// app's act context and CommandRunner kills the check's process group;
// the app's outcome transaction then applies stop precedence.
type checkDriver struct {
	cancel context.CancelFunc
	done   chan checkOutcome
}

// start begins one asynchronous check round; the driver must be idle.
// posted, when non-nil, is called after the outcome is in the channel —
// the deterministic synchronization point the loop tests' barrier waits
// consume (deps.checkOutcomePosted); production wiring leaves it nil.
func (c *checkDriver) start(ctx context.Context, ctrl controllerAPI, handle app.RunHandle, hopPath string, spawnEnv []string, posted func()) { //nolint:gocritic // hugeParam: RunHandle is the app-defined opaque token, passed by value as every Controller method takes it.
	checkCtx, cancel := context.WithCancel(ctx)
	done := make(chan checkOutcome, 1)
	c.cancel = cancel
	c.done = done
	go func() {
		defer cancel()
		report, err := ctrl.ClaimAndRunCheck(checkCtx, handle, hopPath, spawnEnv)
		done <- checkOutcome{report: report, err: err}
		if posted != nil {
			posted()
		}
	}()
}

// running reports whether a check round is in flight.
func (c *checkDriver) running() bool { return c.done != nil }

// poll consumes a finished round without blocking; ok is false while the
// round is still running.
func (c *checkDriver) poll() (checkOutcome, bool) {
	if c.done == nil {
		return checkOutcome{}, false
	}
	select {
	case outcome := <-c.done:
		c.done = nil
		return outcome, true
	default:
		return checkOutcome{}, false
	}
}

// interrupt cancels the in-flight round, if any, and waits for it to
// return: the group is killed through the runner's cancellation, and the
// app's outcome transaction records evidence under stop precedence.
func (c *checkDriver) interrupt() (checkOutcome, bool) {
	if c.done == nil {
		return checkOutcome{}, false
	}
	c.cancel()
	outcome := <-c.done
	c.done = nil
	return outcome, true
}

// runControllerLoop drives one held run in the foreground until the run
// reaches a terminal state or ctx is canceled: it prints one line per run
// transition, heartbeats concurrently on the design's interval, drives
// launch-claim corroboration while the attempt is launching or
// relaunching, routes to DriveStop as soon as a stop request is visible,
// and consumes pending check requests through ClaimAndRunCheck on the poll
// interval — asynchronously, under a cancelable context, so the loop
// still observes a stop request and keeps heartbeating while a check
// runs, and a stop interrupts the check through the app's stop precedence
// instead of waiting it out. It never sleeps outside d.wait, so tests
// pace it with a fake.
func runControllerLoop(ctx context.Context, d *deps, ctrl controllerAPI, handle app.RunHandle, runID, label, hopPath string, stdout io.Writer) (loopResult, error) { //nolint:gocritic // hugeParam: RunHandle is the app-defined opaque token, passed by value as every Controller method takes it.
	heartbeatFailed := runHeartbeats(ctx, d, ctrl, handle)
	var (
		lastState    string
		lastProgress app.LaunchProgress
		spawnEnv     []string
		checks       checkDriver
	)
	// Every exit path retires an in-flight check first: its context dies
	// with the loop's cancel or is canceled here, and the recorded round
	// is still reported. Only an error tree whose causes are ENTIRELY
	// cancellation — an interrupted round's expected shape — and a
	// stop-refused dispatch are silent; a retention or recording failure
	// joined with a cancellation is a real error on every path, deliberate
	// interruption included, and only its non-cancellation causes are
	// reported.
	reportOutcome := func(outcome checkOutcome) error {
		if outcome.err != nil {
			if errors.Is(outcome.err, app.ErrStopRequested) {
				return nil
			}
			if reportable := nonCancellationCauses(outcome.err); reportable != nil {
				return fmt.Errorf("run check: %w", reportable)
			}
			return nil
		}
		if outcome.report.Ran {
			if _, werr := fmt.Fprintf(stdout, "check %s %s\n", outcome.report.OperationID, describeCheckReport(&outcome.report)); werr != nil {
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
	// printRunState prints the run's transition line exactly when state
	// differs from the last one printed, updating lastState — the single
	// gate both the ordinary per-tick poll below and the post-settlement
	// re-read share, so a state observed either way is never printed
	// twice and never silently skipped.
	printRunState := func(state string) error {
		if state == lastState {
			return nil
		}
		if _, werr := fmt.Fprintf(stdout, "run %s %s\n", label, state); werr != nil {
			return werr
		}
		lastState = state
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
		if printErr := printRunState(detail.State); printErr != nil {
			return loopResult{}, errors.Join(printErr, drainChecks())
		}
		if isTerminalRunState(detail.State) {
			if drainErr := drainChecks(); drainErr != nil {
				return loopResult{}, drainErr
			}
			return loopResult{FinalState: detail.State}, nil
		}

		switch {
		case detail.StopRequested || detail.State == runStateStopping:
			// Interrupt the running check first: the group dies through
			// the runner's cancellation and the outcome transaction
			// records evidence under stop precedence; only then drive the
			// stop rounds.
			if outcome, ok := checks.interrupt(); ok {
				if repErr := reportOutcome(outcome); repErr != nil {
					return loopResult{}, repErr
				}
			}
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
					return loopResult{}, errors.Join(fmt.Errorf("corroborate launch: %w", corrErr), drainChecks())
				}
				if progress != lastProgress {
					if _, werr := fmt.Fprintf(stdout, "launch %s\n", describeLaunchProgress(progress)); werr != nil {
						return loopResult{}, errors.Join(werr, drainChecks())
					}
					lastProgress = progress
				}
				// A settled claim (fresh this round, or already settled by
				// a concurrent early-acceptance submission with a lifecycle
				// transition just applied) can move the run to running
				// without the ordinary top-of-tick poll ever observing it:
				// a trivial check claimed and finished later in this same
				// tick would otherwise advance the run straight to
				// completing/completed before the next poll, and "running"
				// would never print. Re-reading once here, before checks
				// are driven, guarantees a user watching the loop always
				// sees the run reach running.
				if progress == app.LaunchSettled || progress == app.LaunchAlreadySettled {
					settled, statusErr := ctrl.Status(ctx, app.StatusRequest{RunID: runID})
					if statusErr != nil {
						return loopResult{}, errors.Join(fmt.Errorf("load run status after launch settlement: %w", statusErr), drainChecks())
					}
					settledDetail := settled.Detail
					if settledDetail == nil {
						return loopResult{}, errors.Join(fmt.Errorf("run %s has no status detail", runID), drainChecks())
					}
					if printErr := printRunState(settledDetail.State); printErr != nil {
						return loopResult{}, errors.Join(printErr, drainChecks())
					}
				}
			}
			if spawnEnv == nil {
				spawnEnv, err = ctrl.CheckSpawnEnvironment(ctx, handle, d.environ())
				if err != nil {
					return loopResult{}, errors.Join(fmt.Errorf("compose check spawn environment: %w", err), drainChecks())
				}
			}
			if outcome, ok := checks.poll(); ok {
				if repErr := reportOutcome(outcome); repErr != nil {
					return loopResult{}, repErr
				}
			}
			if !checks.running() {
				checks.start(ctx, ctrl, handle, hopPath, spawnEnv, d.checkOutcomePosted)
			}
		}

		if err := d.wait(ctx, loopPollInterval); err != nil {
			return loopResult{Detached: true}, drainChecks()
		}
	}
}

// nonCancellationCauses strips every pure-cancellation cause out of err
// and returns what remains — nil when err was nothing but cancellation.
// It recurses through BOTH unwrapping shapes before deciding: a joined
// error (Unwrap() []error) keeps only its surviving members, and a
// single-error wrapper (Unwrap() error) is kept WHOLE — its wrapping text
// preserved — whenever anything under it survives, so a wrapped mixed
// join is never discarded as a plain cancellation just because errors.Is
// finds a canceled member somewhere inside. Retention and recording
// failures are constructed without the cancellation shape (the app
// carries a spawn error as text exactly so this holds), so they always
// survive the filter.
func nonCancellationCauses(err error) error {
	if err == nil {
		return nil
	}
	switch unwrapped := err.(type) { //nolint:errorlint // the filter walks the tree's unwrap SHAPE node by node — the structural traversal errors.As cannot express; cancellation itself is still decided semantically with errors.Is at each leaf.
	case interface{ Unwrap() []error }:
		var kept []error
		for _, cause := range unwrapped.Unwrap() {
			if survivor := nonCancellationCauses(cause); survivor != nil {
				kept = append(kept, survivor)
			}
		}
		return errors.Join(kept...)
	case interface{ Unwrap() error }:
		if inner := unwrapped.Unwrap(); inner != nil {
			if nonCancellationCauses(inner) == nil {
				return nil
			}
			return err
		}
	}
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
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
