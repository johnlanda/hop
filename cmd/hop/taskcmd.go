package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/johnlanda/hop/internal/app"
)

// defaultPlanVerbTimeout bounds one manager plan-verb invocation.
const defaultPlanVerbTimeout = 30 * time.Second

// managerEnvIdentities reads the manager-session identities every plan
// verb (hop task create/retry, hop plan close) takes from its HOP_*
// environment, exactly as hop result submit reads its own worker
// identities: no flags name them, since a plan verb runs only inside a
// HOP-launched manager pane.
type managerEnvIdentities struct {
	RunID         string
	SessionID     string
	IncarnationID string
}

func readManagerEnvIdentities(d *deps) managerEnvIdentities {
	return managerEnvIdentities{
		RunID:         d.getenv("HOP_RUN_ID"),
		SessionID:     d.getenv("HOP_SESSION_ID"),
		IncarnationID: d.getenv("HOP_INCARNATION_ID"),
	}
}

// runTask dispatches the `hop task` subcommands.
func runTask(args []string, stdout, stderr io.Writer, d *deps) (int, error) {
	if len(args) == 0 {
		_, err := fmt.Fprintln(stderr, "hop task: usage: hop task create --title \"<text>\" --file <instructions> [--depends-on <task-uuid>]... | hop task retry --reason \"<text>\" <task-id>")
		return exitUsage, err
	}
	switch args[0] {
	case "create":
		return runTaskCreate(args[1:], stdout, stderr, d)
	case "retry":
		return runTaskRetry(args[1:], stdout, stderr, d)
	default:
		_, err := fmt.Fprintf(stderr, "hop task: unknown subcommand %q\n", args[0])
		return exitUsage, err
	}
}

// runTaskCreate implements `hop task create` (design section 8): manager-
// only, identities from HOP_* env, the instructions body read from --file
// before any store call (file-first protocol). First line:
// GrammarTaskCreatedLine / GrammarTaskCreateDuplicateLine on success,
// refused: <token> on refusal.
func runTaskCreate(args []string, stdout, stderr io.Writer, d *deps) (int, error) {
	diagnostics := &recordingWriter{w: stderr}
	flags := flag.NewFlagSet("hop task create", flag.ContinueOnError)
	flags.SetOutput(diagnostics)
	title := flags.String("title", "", "task title (required)")
	file := flags.String("file", "", "path to the instructions file (required)")
	requestID := flags.String("request-id", "", "idempotency key: an identical retry returns the original outcome")
	var dependsOn stringList
	flags.Var(&dependsOn, "depends-on", "prerequisite task id (repeatable)")
	if err := flags.Parse(args); err != nil {
		return exitUsage, diagnostics.err
	}
	if flags.NArg() > 0 {
		_, err := fmt.Fprintf(stderr, "hop task create: unexpected argument %q\n", flags.Arg(0))
		return exitUsage, err
	}
	if *title == "" || *file == "" {
		_, err := fmt.Fprintln(stderr, "hop task create: --title and --file are required")
		return exitUsage, err
	}
	body, err := os.ReadFile(*file)
	if err != nil {
		_, werr := fmt.Fprintf(stderr, "hop task create: cannot read instructions file: %v\n", err)
		return exitFailure, werr
	}
	env := readManagerEnvIdentities(d)
	stateRoot, err := requireWorkerStateRoot(d.getenv)
	if err != nil {
		_, werr := fmt.Fprintf(stderr, "hop task create: %v\n", err)
		return exitFailure, werr
	}

	ctx, cancel := context.WithTimeout(context.Background(), defaultPlanVerbTimeout)
	defer cancel()
	ctrl, closeStore, err := d.openController(ctx, controllerConfig{stateRoot: stateRoot})
	if err != nil {
		_, werr := fmt.Fprintf(stderr, "hop task create: %s\n", describeStoreOpenFailure(err, "HOP_STATE_DIR"))
		return exitFailure, werr
	}
	defer closeStore() //nolint:errcheck // the store closes on process exit either way; commands report command errors, not pool teardown.

	result, err := ctrl.CreateTask(ctx, app.CreateTaskRequest{
		RunID: env.RunID, SessionID: env.SessionID, IncarnationID: env.IncarnationID, StateRoot: stateRoot,
		Title: *title, InstructionsBody: body, DependsOn: dependsOn, RequestID: *requestID,
	})
	if err != nil {
		_, werr := fmt.Fprintf(stderr, "hop task create: %v\n", err)
		return exitFailure, werr
	}
	return writeLinesAndExit(stdout, taskCreateLines(&result))
}

// taskCreateLines renders CreateTaskResult as the grammar's fixed lines.
func taskCreateLines(result *app.CreateTaskResult) []string {
	switch result.Outcome {
	case "accepted":
		return []string{app.GrammarTaskCreatedLine(result.TaskID, result.Seq)}
	case "duplicate":
		return []string{app.GrammarTaskCreateDuplicateLine(result.TaskID, result.Seq)}
	default:
		return renderRefusal(planRefusalToken(result.Reason), result.Detail)
	}
}

// runTaskRetry implements `hop task retry <task-id>` (design section 8).
func runTaskRetry(args []string, stdout, stderr io.Writer, d *deps) (int, error) {
	diagnostics := &recordingWriter{w: stderr}
	flags := flag.NewFlagSet("hop task retry", flag.ContinueOnError)
	flags.SetOutput(diagnostics)
	reason := flags.String("reason", "", "reason for the retry (required)")
	requestID := flags.String("request-id", "", "idempotency key: an identical retry returns the original outcome")
	if err := flags.Parse(args); err != nil {
		return exitUsage, diagnostics.err
	}
	if flags.NArg() != 1 {
		_, err := fmt.Fprintln(stderr, "hop task retry: exactly one task-id argument is required")
		return exitUsage, err
	}
	if *reason == "" {
		_, err := fmt.Fprintln(stderr, "hop task retry: --reason is required")
		return exitUsage, err
	}
	env := readManagerEnvIdentities(d)
	stateRoot, err := requireWorkerStateRoot(d.getenv)
	if err != nil {
		_, werr := fmt.Fprintf(stderr, "hop task retry: %v\n", err)
		return exitFailure, werr
	}

	ctx, cancel := context.WithTimeout(context.Background(), defaultPlanVerbTimeout)
	defer cancel()
	ctrl, closeStore, err := d.openController(ctx, controllerConfig{stateRoot: stateRoot})
	if err != nil {
		_, werr := fmt.Fprintf(stderr, "hop task retry: %s\n", describeStoreOpenFailure(err, "HOP_STATE_DIR"))
		return exitFailure, werr
	}
	defer closeStore() //nolint:errcheck // the store closes on process exit either way; commands report command errors, not pool teardown.

	result, err := ctrl.RequestRetry(ctx, app.RequestRetryRequest{
		RunID: env.RunID, SessionID: env.SessionID, IncarnationID: env.IncarnationID,
		TaskID: flags.Arg(0), Reason: *reason, RequestID: *requestID,
	})
	if err != nil {
		_, werr := fmt.Fprintf(stderr, "hop task retry: %v\n", err)
		return exitFailure, werr
	}
	return writeLinesAndExit(stdout, taskRetryLines(flags.Arg(0), &result))
}

// taskRetryLines renders RequestRetryResult. The grammar's accepted/
// duplicate lines name the task by its t<seq> label, which this DTO does
// not carry (only the raw task id argument the caller passed); rendering
// the raw id here is a known simplification pending a Seq field on
// RequestRetryResult (see HANDOFF.md).
func taskRetryLines(taskArg string, result *app.RequestRetryResult) []string {
	switch result.Outcome {
	case "accepted":
		return []string{fmt.Sprintf("retry accepted %s attempt %d", taskArg, result.AttemptNumber)}
	case "duplicate":
		return []string{fmt.Sprintf("duplicate %s attempt %d", taskArg, result.AttemptNumber)}
	default:
		return renderRefusal(planRefusalToken(result.Reason), result.Detail)
	}
}

// runPlan dispatches the `hop plan` subcommands.
func runPlan(args []string, stdout, stderr io.Writer, d *deps) (int, error) {
	if len(args) == 0 || args[0] != "close" {
		_, err := fmt.Fprintln(stderr, "hop plan: usage: hop plan close")
		return exitUsage, err
	}
	return runPlanClose(args[1:], stdout, stderr, d)
}

// runPlanClose implements `hop plan close` (design section 8).
func runPlanClose(args []string, stdout, stderr io.Writer, d *deps) (int, error) {
	diagnostics := &recordingWriter{w: stderr}
	flags := flag.NewFlagSet("hop plan close", flag.ContinueOnError)
	flags.SetOutput(diagnostics)
	requestID := flags.String("request-id", "", "idempotency key: an identical retry returns the original outcome")
	if err := flags.Parse(args); err != nil {
		return exitUsage, diagnostics.err
	}
	if flags.NArg() > 0 {
		_, err := fmt.Fprintf(stderr, "hop plan close: unexpected argument %q\n", flags.Arg(0))
		return exitUsage, err
	}
	env := readManagerEnvIdentities(d)
	stateRoot, err := requireWorkerStateRoot(d.getenv)
	if err != nil {
		_, werr := fmt.Fprintf(stderr, "hop plan close: %v\n", err)
		return exitFailure, werr
	}

	ctx, cancel := context.WithTimeout(context.Background(), defaultPlanVerbTimeout)
	defer cancel()
	ctrl, closeStore, err := d.openController(ctx, controllerConfig{stateRoot: stateRoot})
	if err != nil {
		_, werr := fmt.Fprintf(stderr, "hop plan close: %s\n", describeStoreOpenFailure(err, "HOP_STATE_DIR"))
		return exitFailure, werr
	}
	defer closeStore() //nolint:errcheck // the store closes on process exit either way; commands report command errors, not pool teardown.

	result, err := ctrl.ClosePlan(ctx, app.ClosePlanRequest{
		RunID: env.RunID, SessionID: env.SessionID, IncarnationID: env.IncarnationID, RequestID: *requestID,
	})
	if err != nil {
		_, werr := fmt.Fprintf(stderr, "hop plan close: %v\n", err)
		return exitFailure, werr
	}
	return writeLinesAndExit(stdout, planCloseLines(&result))
}

// planCloseLines renders ClosePlanResult as the grammar's fixed lines.
func planCloseLines(result *app.ClosePlanResult) []string {
	switch result.Outcome {
	case "accepted":
		return []string{app.GrammarPlanClosedLine}
	case "duplicate":
		return []string{app.GrammarPlanCloseDuplicateLine}
	default:
		return renderRefusal(planRefusalToken(result.Reason), result.Detail)
	}
}

// writeLinesAndExit writes lines to stdout and maps the first line to the
// design's worker-protocol exit convention: exit 0 unless the first line
// is a refusal, which always exits 1 (refusals are never a usage error —
// the CLI syntax was fine; the request itself was refused).
func writeLinesAndExit(w io.Writer, lines []string) (int, error) {
	for _, line := range lines {
		if _, err := fmt.Fprintln(w, line); err != nil {
			return exitFailure, err
		}
	}
	if len(lines) > 0 && strings.HasPrefix(lines[0], app.GrammarRefusalPrefix) {
		return exitFailure, nil
	}
	return exitOK, nil
}
