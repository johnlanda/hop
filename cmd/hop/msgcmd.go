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

// defaultMsgVerbTimeout bounds one hop msg/answer invocation, except hop
// msg wait, which is bounded by its own --timeout instead.
const defaultMsgVerbTimeout = 30 * time.Second

// msgPollInterval paces hop msg wait's 1s poll loop (design section 7):
// the loop and its timeout live here, never in internal/app, whose
// FetchMessage is one non-blocking attempt.
const msgPollInterval = 1 * time.Second

// readBodyFlag resolves a message/answer/reasons body from --file or
// --body (mutually exclusive; exactly one required), reporting whether it
// came from --body (Inline) for the caller's size-bound choice. A read
// failure renders pathErrorCategory's fixed category, never the operator's
// path.
func readBodyFlag(file, body string, fileSet, bodySet bool) (data []byte, inline bool, err error) {
	if fileSet == bodySet {
		return nil, false, fmt.Errorf("exactly one of --file and --body is required")
	}
	if bodySet {
		return []byte(body), true, nil
	}
	content, err := os.ReadFile(file) //nolint:gosec // G304: an operator-supplied --file path; reading it is this flag's purpose.
	if err != nil {
		return nil, false, fmt.Errorf("cannot read body file: %s", pathErrorCategory(err))
	}
	return content, false, nil
}

// runMsg dispatches the `hop msg` subcommands.
func runMsg(args []string, stdout, stderr io.Writer, d *deps) (int, error) {
	if len(args) == 0 {
		_, err := fmt.Fprintln(stderr, "hop msg: usage: hop msg send|next|wait|ack|show ...")
		return exitUsage, err
	}
	switch args[0] {
	case "send":
		return runMsgSend(args[1:], stdout, stderr, d)
	case "next":
		return runMsgNext(args[1:], stdout, stderr, d)
	case "wait":
		return runMsgWait(args[1:], stdout, stderr, d)
	case "ack":
		return runMsgAck(args[1:], stdout, stderr, d)
	case "show":
		return runMsgShow(args[1:], stdout, stderr, d)
	default:
		_, err := fmt.Fprintf(stderr, "hop msg: unknown subcommand %q\n", args[0])
		return exitUsage, err
	}
}

// runMsgSend implements `hop msg send` (design section 7): --to and
// --kind select the destination and message kind; --reply-to is required
// for kind=answer and forbidden otherwise (an answer's destination is
// derived server-side from the question, never caller-chosen); --relay-of
// is optional, question-only.
func runMsgSend(args []string, stdout, stderr io.Writer, d *deps) (int, error) {
	diagnostics := &recordingWriter{w: stderr}
	flags := flag.NewFlagSet("hop msg send", flag.ContinueOnError)
	flags.SetOutput(diagnostics)
	to := flags.String("to", "", "recipient: manager | human | task:<uuid> (forbidden for --kind answer)")
	kind := flags.String("kind", "", "question | info | answer (required)")
	replyTo := flags.String("reply-to", "", "question id being answered (required for --kind answer, forbidden otherwise)")
	relayOf := flags.String("relay-of", "", "original question id being relayed (question kind only)")
	file := flags.String("file", "", "path to the message body (mutually exclusive with --body)")
	body := flags.String("body", "", "inline message body, <= 4 KiB (mutually exclusive with --file)")
	requestID := flags.String("request-id", "", "idempotency key: an identical retry returns the original outcome")
	if err := flags.Parse(args); err != nil {
		return exitUsage, diagnostics.err
	}
	if flags.NArg() > 0 {
		_, err := fmt.Fprintf(stderr, "hop msg send: unexpected argument %q\n", flags.Arg(0))
		return exitUsage, err
	}
	supplied := map[string]bool{}
	flags.Visit(func(f *flag.Flag) { supplied[f.Name] = true })
	if *kind == "" {
		_, err := fmt.Fprintln(stderr, "hop msg send: --kind is required")
		return exitUsage, err
	}
	if *kind == "answer" {
		if supplied["to"] {
			_, err := fmt.Fprintln(stderr, "hop msg send: --to is forbidden for --kind answer; the destination is derived from the question")
			return exitUsage, err
		}
		if *replyTo == "" {
			_, err := fmt.Fprintln(stderr, "hop msg send: --reply-to is required for --kind answer")
			return exitUsage, err
		}
	} else {
		if *to == "" {
			_, err := fmt.Fprintln(stderr, "hop msg send: --to is required")
			return exitUsage, err
		}
		if supplied["reply-to"] {
			_, err := fmt.Fprintln(stderr, "hop msg send: --reply-to is only valid for --kind answer")
			return exitUsage, err
		}
	}
	msgBody, inline, err := readBodyFlag(*file, *body, supplied["file"], supplied["body"])
	if err != nil {
		_, werr := fmt.Fprintf(stderr, "hop msg send: %v\n", err)
		return exitUsage, werr
	}

	stateRoot, err := requireWorkerStateRoot(d.getenv)
	if err != nil {
		_, werr := fmt.Fprintf(stderr, "hop msg send: %v\n", err)
		return exitFailure, werr
	}
	ctx, cancel := context.WithTimeout(context.Background(), defaultMsgVerbTimeout)
	defer cancel()
	ctrl, closeStore, err := d.openController(ctx, controllerConfig{stateRoot: stateRoot})
	if err != nil {
		_, werr := fmt.Fprintf(stderr, "hop msg send: %s\n", describeStoreOpenFailure(err, "HOP_STATE_DIR"))
		return exitFailure, werr
	}
	defer closeStore() //nolint:errcheck // the store closes on process exit either way; commands report command errors, not pool teardown.

	result, err := ctrl.SendMessage(ctx, app.SendMessageRequest{
		RunID: d.getenv("HOP_RUN_ID"), SessionID: d.getenv("HOP_SESSION_ID"), IncarnationID: d.getenv("HOP_INCARNATION_ID"),
		StateRoot: stateRoot, To: *to, Kind: *kind, ReplyTo: *replyTo, RelayOf: *relayOf,
		Body: msgBody, Inline: inline, RequestID: *requestID,
	})
	if err != nil {
		_, werr := fmt.Fprintf(stderr, "hop msg send: %v\n", err)
		return exitFailure, werr
	}
	return writeLinesAndExit(stdout, sendMessageLines(&result))
}

// sendMessageLines renders SendMessageResult as the grammar's fixed lines.
func sendMessageLines(result *app.SendMessageResult) []string {
	switch result.Outcome {
	case "accepted":
		return []string{app.GrammarSentLine(result.MessageID)}
	case "duplicate":
		return []string{app.GrammarSendDuplicateLine(result.MessageID)}
	default:
		return renderRefusal(refusalToken(result.Reason), result.Detail)
	}
}

// runMsgNext implements `hop msg next`: one non-blocking fetch attempt.
func runMsgNext(args []string, stdout, stderr io.Writer, d *deps) (int, error) {
	diagnostics := &recordingWriter{w: stderr}
	flags := flag.NewFlagSet("hop msg next", flag.ContinueOnError)
	flags.SetOutput(diagnostics)
	if err := flags.Parse(args); err != nil {
		return exitUsage, diagnostics.err
	}
	if flags.NArg() > 0 {
		_, err := fmt.Fprintf(stderr, "hop msg next: unexpected argument %q\n", flags.Arg(0))
		return exitUsage, err
	}
	stateRoot, err := requireWorkerStateRoot(d.getenv)
	if err != nil {
		_, werr := fmt.Fprintf(stderr, "hop msg next: %v\n", err)
		return exitFailure, werr
	}
	ctx, cancel := context.WithTimeout(context.Background(), defaultMsgVerbTimeout)
	defer cancel()
	ctrl, closeStore, err := d.openController(ctx, controllerConfig{stateRoot: stateRoot})
	if err != nil {
		_, werr := fmt.Fprintf(stderr, "hop msg next: %s\n", describeStoreOpenFailure(err, "HOP_STATE_DIR"))
		return exitFailure, werr
	}
	defer closeStore() //nolint:errcheck // the store closes on process exit either way; commands report command errors, not pool teardown.

	result, err := fetchOneMessage(ctx, ctrl, d)
	if err != nil {
		_, werr := fmt.Fprintf(stderr, "hop msg next: %v\n", err)
		return exitFailure, werr
	}
	if !result.Delivered {
		return writeLinesAndExit(stdout, []string{app.GrammarMsgNoneLine})
	}
	return writeLinesAndExit(stdout, deliveredMessageLines(&result))
}

// fetchOneMessage issues one FetchMessage call using the caller's HOP_*
// identity, shared by hop msg next and each round of hop msg wait's poll
// loop.
func fetchOneMessage(ctx context.Context, ctrl controllerAPI, d *deps) (app.FetchMessageResult, error) {
	return ctrl.FetchMessage(ctx, app.FetchMessageRequest{
		RunID: d.getenv("HOP_RUN_ID"), SessionID: d.getenv("HOP_SESSION_ID"), IncarnationID: d.getenv("HOP_INCARNATION_ID"),
	})
}

// deliveredMessageLines renders a served FetchMessageResult as the
// grammar's fixed three lines.
func deliveredMessageLines(result *app.FetchMessageResult) []string {
	from := app.GrammarPrincipal(result.SenderKind, result.SenderSession)
	return []string{
		app.GrammarMessageLine(result.MessageID, result.Kind, from, result.ReplyTo, result.RelayOf, result.Origin),
		app.GrammarBodyLine(result.BodyPath),
		app.GrammarAckHintLine(result.MessageID),
	}
}

// runMsgWait implements `hop msg wait [--timeout <dur>]`: the CLI-side 1s
// poll loop and timeout (design section 7 — internal/app's FetchMessage is
// one non-blocking attempt). When --timeout is absent (flag.Visit detects
// this — the flag.Duration default below is only the fallback for
// resolving the run's own frozen [messages] wait_timeout, never the
// rendered value itself), the default is resolved from the run's frozen
// WorkflowSnapshot (Controller.MessageWaitDefault, lease-free); an
// explicit --timeout always wins. The "none" line renders whichever
// timeout was actually used. The FIRST fetch attempt validates the
// caller's HOP_* identity through the exact same FetchMessage call hop
// msg next uses, before MessageWaitDefault is ever reached: a missing or
// malformed HOP_RUN_ID/HOP_SESSION_ID/HOP_INCARNATION_ID therefore fails
// identically for both verbs (same first line, same exit code), and a
// broken environment never pays for a wasted frozen-run read.
func runMsgWait(args []string, stdout, stderr io.Writer, d *deps) (int, error) {
	diagnostics := &recordingWriter{w: stderr}
	flags := flag.NewFlagSet("hop msg wait", flag.ContinueOnError)
	flags.SetOutput(diagnostics)
	timeout := flags.Duration("timeout", app.DefaultMessageWait, "how long to wait for a message before giving up (default: the run's frozen [messages] wait_timeout)")
	if err := flags.Parse(args); err != nil {
		return exitUsage, diagnostics.err
	}
	if flags.NArg() > 0 {
		_, err := fmt.Fprintf(stderr, "hop msg wait: unexpected argument %q\n", flags.Arg(0))
		return exitUsage, err
	}
	timeoutSupplied := false
	flags.Visit(func(f *flag.Flag) {
		if f.Name == "timeout" {
			timeoutSupplied = true
		}
	})
	stateRoot, err := requireWorkerStateRoot(d.getenv)
	if err != nil {
		_, werr := fmt.Fprintf(stderr, "hop msg wait: %v\n", err)
		return exitFailure, werr
	}

	// Opening the controller, the first (environment-validating) fetch
	// attempt and resolving the default timeout (when needed) all run
	// under their own short setup bound, before the wait's own deadline —
	// which depends on the resolved timeout — is ever created.
	setupCtx, setupCancel := context.WithTimeout(context.Background(), defaultMsgVerbTimeout)
	ctrl, closeStore, err := d.openController(setupCtx, controllerConfig{stateRoot: stateRoot})
	if err != nil {
		setupCancel()
		_, werr := fmt.Fprintf(stderr, "hop msg wait: %s\n", describeStoreOpenFailure(err, "HOP_STATE_DIR"))
		return exitFailure, werr
	}
	defer closeStore() //nolint:errcheck // the store closes on process exit either way; commands report command errors, not pool teardown.

	result, fetchErr := fetchOneMessage(setupCtx, ctrl, d)
	if fetchErr != nil {
		setupCancel()
		_, werr := fmt.Fprintf(stderr, "hop msg wait: %v\n", fetchErr)
		return exitFailure, werr
	}
	if result.Delivered {
		setupCancel()
		return writeLinesAndExit(stdout, deliveredMessageLines(&result))
	}
	if !timeoutSupplied {
		resolved, defaultErr := ctrl.MessageWaitDefault(setupCtx, d.getenv("HOP_RUN_ID"))
		if defaultErr != nil {
			setupCancel()
			_, werr := fmt.Fprintf(stderr, "hop msg wait: %v\n", defaultErr)
			return exitFailure, werr
		}
		*timeout = resolved
	}
	setupCancel()

	deadline, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	for {
		if waitErr := d.wait(deadline, msgPollInterval); waitErr != nil {
			return writeLinesAndExit(stdout, []string{app.GrammarMsgWaitNoneLine(*timeout)})
		}
		callCtx, callCancel := context.WithTimeout(context.Background(), defaultMsgVerbTimeout)
		result, fetchErr := fetchOneMessage(callCtx, ctrl, d)
		callCancel()
		if fetchErr != nil {
			_, werr := fmt.Fprintf(stderr, "hop msg wait: %v\n", fetchErr)
			return exitFailure, werr
		}
		if result.Delivered {
			return writeLinesAndExit(stdout, deliveredMessageLines(&result))
		}
	}
}

// runMsgAck implements `hop msg ack <message-id>`.
func runMsgAck(args []string, stdout, stderr io.Writer, d *deps) (int, error) {
	diagnostics := &recordingWriter{w: stderr}
	flags := flag.NewFlagSet("hop msg ack", flag.ContinueOnError)
	flags.SetOutput(diagnostics)
	if err := flags.Parse(args); err != nil {
		return exitUsage, diagnostics.err
	}
	if flags.NArg() != 1 {
		_, err := fmt.Fprintln(stderr, "hop msg ack: exactly one message-id argument is required")
		return exitUsage, err
	}
	stateRoot, err := requireWorkerStateRoot(d.getenv)
	if err != nil {
		_, werr := fmt.Fprintf(stderr, "hop msg ack: %v\n", err)
		return exitFailure, werr
	}
	ctx, cancel := context.WithTimeout(context.Background(), defaultMsgVerbTimeout)
	defer cancel()
	ctrl, closeStore, err := d.openController(ctx, controllerConfig{stateRoot: stateRoot})
	if err != nil {
		_, werr := fmt.Fprintf(stderr, "hop msg ack: %s\n", describeStoreOpenFailure(err, "HOP_STATE_DIR"))
		return exitFailure, werr
	}
	defer closeStore() //nolint:errcheck // the store closes on process exit either way; commands report command errors, not pool teardown.

	messageID := flags.Arg(0)
	result, err := ctrl.AckMessage(ctx, app.AckMessageRequest{
		RunID: d.getenv("HOP_RUN_ID"), MessageID: messageID,
		SessionID: d.getenv("HOP_SESSION_ID"), IncarnationID: d.getenv("HOP_INCARNATION_ID"),
	})
	if err != nil {
		_, werr := fmt.Fprintf(stderr, "hop msg ack: %v\n", err)
		return exitFailure, werr
	}
	return writeLinesAndExit(stdout, ackMessageLines(messageID, &result))
}

// ackMessageLines renders AckMessageResult as the grammar's fixed lines.
func ackMessageLines(messageID string, result *app.AckMessageResult) []string {
	switch result.Outcome {
	case "accepted":
		return []string{app.GrammarAckAcceptedLine(messageID)}
	case "duplicate":
		return []string{app.GrammarAckDuplicateLine(messageID)}
	default:
		return renderRefusal(refusalToken(result.Reason), result.Detail)
	}
}

// runMsgShow implements `hop msg show <message-id>`: a read-only,
// same-run envelope lookup with no caller-identity validation (design
// section 7's grammar table).
func runMsgShow(args []string, stdout, stderr io.Writer, d *deps) (int, error) {
	diagnostics := &recordingWriter{w: stderr}
	flags := flag.NewFlagSet("hop msg show", flag.ContinueOnError)
	flags.SetOutput(diagnostics)
	runID := flags.String("run", "", "run id (default $HOP_RUN_ID)")
	if err := flags.Parse(args); err != nil {
		return exitUsage, diagnostics.err
	}
	if flags.NArg() != 1 {
		_, err := fmt.Fprintln(stderr, "hop msg show: exactly one message-id argument is required")
		return exitUsage, err
	}
	if *runID == "" {
		*runID = d.getenv("HOP_RUN_ID")
	}
	stateRoot, err := requireWorkerStateRoot(d.getenv)
	if err != nil {
		_, werr := fmt.Fprintf(stderr, "hop msg show: %v\n", err)
		return exitFailure, werr
	}
	ctx, cancel := context.WithTimeout(context.Background(), defaultMsgVerbTimeout)
	defer cancel()
	ctrl, closeStore, err := d.openController(ctx, controllerConfig{stateRoot: stateRoot})
	if err != nil {
		_, werr := fmt.Fprintf(stderr, "hop msg show: %s\n", describeStoreOpenFailure(err, "HOP_STATE_DIR"))
		return exitFailure, werr
	}
	defer closeStore() //nolint:errcheck // the store closes on process exit either way; commands report command errors, not pool teardown.

	result, err := ctrl.ShowMessage(ctx, app.ShowMessageRequest{RunID: *runID, MessageID: flags.Arg(0)})
	if err != nil {
		_, werr := fmt.Fprintf(stderr, "hop msg show: %v\n", err)
		return exitFailure, werr
	}
	if !result.Found {
		return writeLinesAndExit(stdout, renderRefusal(app.GrammarReasonNotFound, ""))
	}
	return writeLinesAndExit(stdout, showMessageLines(&result))
}

// showMessageLines renders a found ShowMessageResult as the grammar's
// fixed lines: the envelope, the body path, one delivered: line per
// delivery (oldest first, re-serves included), then acknowledged: when
// acked.
func showMessageLines(result *app.ShowMessageResult) []string {
	from := app.GrammarPrincipal(result.SenderKind, result.SenderSession)
	lines := []string{
		app.GrammarMessageShowLine(result.MessageID, result.Kind, from, result.Recipient, result.ReplyTo, result.RelayOf, result.Seq),
		app.GrammarBodyLine(result.BodyPath),
	}
	for _, delivery := range result.Deliveries {
		lines = append(lines, app.GrammarDeliveredLine(delivery.SessionID, delivery.At))
	}
	if result.Acknowledged {
		lines = append(lines, app.GrammarAcknowledgedLine(result.AcknowledgedAt))
	}
	return lines
}

// runAnswer implements `hop answer <question-id>` (design section 7): a
// human/controller-machine command — no HOP_* worker env, no lease — so
// it resolves the repository and controller state root exactly like hop
// stop/resume, never the worker state-root rule.
func runAnswer(args []string, stdout, stderr io.Writer, d *deps) (int, error) {
	diagnostics := &recordingWriter{w: stderr}
	flags := flag.NewFlagSet("hop answer", flag.ContinueOnError)
	flags.SetOutput(diagnostics)
	repoDir := flags.String("C", "", "repository directory (default: the working directory)")
	runArg := flags.String("run", "", "run id or r<seq> label (required)")
	file := flags.String("file", "", "path to the answer body (mutually exclusive with --body)")
	body := flags.String("body", "", "inline answer body, <= 4 KiB (mutually exclusive with --file)")
	requestID := flags.String("request-id", "", "idempotency key: an identical retry returns the original outcome")
	if err := flags.Parse(args); err != nil {
		return exitUsage, diagnostics.err
	}
	if flags.NArg() != 1 {
		_, err := fmt.Fprintln(stderr, "hop answer: exactly one question-id argument is required")
		return exitUsage, err
	}
	if *runArg == "" {
		_, err := fmt.Fprintln(stderr, "hop answer: --run is required")
		return exitUsage, err
	}
	supplied := map[string]bool{}
	flags.Visit(func(f *flag.Flag) { supplied[f.Name] = true })
	answerBody, inline, err := readBodyFlag(*file, *body, supplied["file"], supplied["body"])
	if err != nil {
		_, werr := fmt.Fprintf(stderr, "hop answer: %v\n", err)
		return exitUsage, werr
	}

	repoRoot, err := resolveRepositoryRoot(d, *repoDir)
	if err != nil {
		_, werr := fmt.Fprintf(stderr, "hop answer: %v\n", err)
		return exitUsage, werr
	}
	stateRoot, _, err := resolveStateRoot(d.getenv)
	if err != nil {
		_, werr := fmt.Fprintf(stderr, "hop answer: %v\n", err)
		return exitUsage, werr
	}
	ctx, cancel := context.WithTimeout(context.Background(), defaultMsgVerbTimeout)
	defer cancel()
	ctrl, closeStore, err := d.openController(ctx, controllerConfig{stateRoot: stateRoot})
	if err != nil {
		_, werr := fmt.Fprintf(stderr, "hop answer: %s\n", describeStoreOpenFailure(err, "the resolved state root"))
		return exitFailure, werr
	}
	defer closeStore() //nolint:errcheck // the store closes on process exit either way; commands report command errors, not pool teardown.

	runID, err := resolveRunArg(ctx, ctrl, repoRoot, *runArg)
	if err != nil {
		_, werr := fmt.Fprintf(stderr, "hop answer: %v\n", err)
		return exitUsage, werr
	}
	result, err := ctrl.Answer(ctx, app.AnswerRequest{
		RunID: runID, QuestionID: flags.Arg(0), StateRoot: stateRoot,
		Body: answerBody, Inline: inline, RequestID: *requestID,
	})
	if err != nil {
		_, werr := fmt.Fprintf(stderr, "hop answer: %v\n", err)
		return exitFailure, werr
	}
	return writeLinesAndExit(stdout, answerLines(&result))
}

// answerLines renders AnswerResult. hop answer has no dedicated grammar
// constants of its own (it produces an ordinary message, delivered
// through the same "message accepted" shape hop msg send uses).
func answerLines(result *app.AnswerResult) []string {
	switch result.Outcome {
	case "accepted":
		return []string{app.GrammarSentLine(result.MessageID)}
	case "duplicate":
		return []string{app.GrammarSendDuplicateLine(result.MessageID)}
	default:
		return renderRefusal(refusalToken(result.Reason), result.Detail)
	}
}
