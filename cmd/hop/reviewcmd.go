package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/johnlanda/hop/internal/app"
)

// defaultReviewSubmitTimeout bounds one hop review submit invocation.
const defaultReviewSubmitTimeout = 30 * time.Second

// runReview dispatches the `hop review` subcommands.
func runReview(args []string, stdout, stderr io.Writer, d *deps) (int, error) {
	if len(args) == 0 || args[0] != "submit" {
		_, err := fmt.Fprintln(stderr, "hop review: usage: hop review submit --verdict approve|reject --subject <commit-oid> --reasons-file <path>")
		return exitUsage, err
	}
	return runReviewSubmit(args[1:], stdout, stderr, d)
}

// runReviewSubmit implements `hop review submit` (design section 8):
// reviewer-only, identities from HOP_* env, the reasons body read from
// --reasons-file before any store call. Retryable:
// GrammarTransientUndeliveredLine (the reviewer's mailbox must drain
// first, exactly like hop result submit's own transient protocol).
func runReviewSubmit(args []string, stdout, stderr io.Writer, d *deps) (int, error) {
	diagnostics := &recordingWriter{w: stderr}
	flags := flag.NewFlagSet("hop review submit", flag.ContinueOnError)
	flags.SetOutput(diagnostics)
	verdict := flags.String("verdict", "", "approve | reject (required)")
	subject := flags.String("subject", "", "reviewed candidate's commit object id (required)")
	reasonsFile := flags.String("reasons-file", "", "path to the verdict's reasons (required)")
	if err := flags.Parse(args); err != nil {
		return exitUsage, diagnostics.err
	}
	if flags.NArg() > 0 {
		_, err := fmt.Fprintf(stderr, "hop review submit: unexpected argument %q\n", flags.Arg(0))
		return exitUsage, err
	}
	if *verdict == "" || *subject == "" || *reasonsFile == "" {
		_, err := fmt.Fprintln(stderr, "hop review submit: --verdict, --subject and --reasons-file are required")
		return exitUsage, err
	}
	reasons, err := os.ReadFile(*reasonsFile)
	if err != nil {
		_, werr := fmt.Fprintf(stderr, "hop review submit: cannot read reasons file: %v\n", err)
		return exitFailure, werr
	}

	stateRoot, err := requireWorkerStateRoot(d.getenv)
	if err != nil {
		_, werr := fmt.Fprintf(stderr, "hop review submit: %v\n", err)
		return exitFailure, werr
	}
	ctx, cancel := context.WithTimeout(context.Background(), defaultReviewSubmitTimeout)
	defer cancel()
	ctrl, closeStore, err := d.openController(ctx, controllerConfig{stateRoot: stateRoot})
	if err != nil {
		_, werr := fmt.Fprintf(stderr, "hop review submit: %s\n", describeStoreOpenFailure(err, "HOP_STATE_DIR"))
		return exitFailure, werr
	}
	defer closeStore() //nolint:errcheck // the store closes on process exit either way; commands report command errors, not pool teardown.

	result, err := ctrl.SubmitReviewVerdict(ctx, app.SubmitReviewRequest{
		RunID:            d.getenv("HOP_RUN_ID"),
		TaskID:           d.getenv("HOP_TASK_ID"),
		AttemptID:        d.getenv("HOP_ATTEMPT_ID"),
		SessionID:        d.getenv("HOP_SESSION_ID"),
		IncarnationID:    d.getenv("HOP_INCARNATION_ID"),
		Verdict:          *verdict,
		SubjectCommitOID: *subject,
		ReasonsBody:      reasons,
	})
	if err != nil {
		_, werr := fmt.Fprintf(stderr, "hop review submit: %v\n", err)
		return exitFailure, werr
	}
	return writeReviewResultAndExit(stdout, stderr, &result)
}

// writeReviewResultAndExit renders SubmitReviewResult per the grammar: a
// transient outcome is always the fixed retry line on its own (the
// store's own Detail, if any, goes to stderr, mirroring hop result
// submit's identical convention), accepted/duplicate name the review,
// and everything else is a refusal.
func writeReviewResultAndExit(stdout, stderr io.Writer, result *app.SubmitReviewResult) (int, error) {
	if result.Outcome == "transient" {
		if _, err := fmt.Fprintln(stdout, app.GrammarTransientUndeliveredLine); err != nil {
			return exitFailure, err
		}
		if result.Detail != "" {
			if _, err := fmt.Fprintf(stderr, "hop review submit: %s\n", result.Detail); err != nil {
				return exitFailure, err
			}
		}
		return exitFailure, nil
	}
	switch result.Outcome {
	case "accepted":
		return writeLinesAndExit(stdout, []string{app.GrammarVerdictAcceptedLine(result.ReviewID)})
	case "duplicate":
		return writeLinesAndExit(stdout, []string{app.GrammarVerdictDuplicateLine(result.ReviewID)})
	default:
		return writeLinesAndExit(stdout, renderRefusal(reviewRefusalToken(result.Outcome), result.Detail))
	}
}
