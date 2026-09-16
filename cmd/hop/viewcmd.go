package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"time"
)

// defaultViewTimeout bounds one hop view invocation.
const defaultViewTimeout = 10 * time.Second

// runView dispatches the `hop view` subcommands (design section 9).
func runView(args []string, stdout, stderr io.Writer, d *deps) (int, error) {
	if len(args) == 0 {
		_, err := fmt.Fprintln(stderr, "hop view: usage: hop view set --run <id|r<seq>> | hop view clear")
		return exitUsage, err
	}
	switch args[0] {
	case "set":
		return runViewSet(args[1:], stdout, stderr, d)
	case "clear":
		return runViewClear(args[1:], stdout, stderr, d)
	default:
		_, err := fmt.Fprintf(stderr, "hop view: unknown subcommand %q\n", args[0])
		return exitUsage, err
	}
}

// runViewSet implements `hop view set --run <id|r<seq>>`: installs HOP's
// run-scoped, manager-first view under source plugin:hop.
func runViewSet(args []string, stdout, stderr io.Writer, d *deps) (int, error) {
	diagnostics := &recordingWriter{w: stderr}
	flags := flag.NewFlagSet("hop view set", flag.ContinueOnError)
	flags.SetOutput(diagnostics)
	repoDir := flags.String("C", "", "repository directory (default: the working directory)")
	socketPath := flags.String("socket", "", "herdr server socket (default $HERDR_SOCKET_PATH)")
	runArg := flags.String("run", "", "run id or r<seq> label (required)")
	if err := flags.Parse(args); err != nil {
		return exitUsage, diagnostics.err
	}
	if flags.NArg() > 0 {
		_, err := fmt.Fprintf(stderr, "hop view set: unexpected argument %q\n", flags.Arg(0))
		return exitUsage, err
	}
	if *runArg == "" {
		_, err := fmt.Fprintln(stderr, "hop view set: --run is required")
		return exitUsage, err
	}
	if *socketPath == "" {
		*socketPath = d.getenv("HERDR_SOCKET_PATH")
	}

	repoRoot, err := resolveRepositoryRoot(d, *repoDir)
	if err != nil {
		_, werr := fmt.Fprintf(stderr, "hop view set: %v\n", err)
		return exitUsage, werr
	}
	stateRoot, _, err := resolveStateRoot(d.getenv)
	if err != nil {
		_, werr := fmt.Fprintf(stderr, "hop view set: %v\n", err)
		return exitUsage, werr
	}
	ctx, cancel := context.WithTimeout(context.Background(), defaultViewTimeout)
	defer cancel()
	ctrl, closeStore, err := d.openController(ctx, controllerConfig{stateRoot: stateRoot, socketPath: *socketPath, withRuntime: true})
	if err != nil {
		_, werr := fmt.Fprintf(stderr, "hop view set: %s\n", describeStoreOpenFailure(err, "the resolved state root"))
		return exitFailure, werr
	}
	defer closeStore() //nolint:errcheck // the store closes on process exit either way; commands report command errors, not pool teardown.

	runID, err := resolveRunArg(ctx, ctrl, repoRoot, *runArg)
	if err != nil {
		_, werr := fmt.Fprintf(stderr, "hop view set: %v\n", err)
		return exitUsage, werr
	}
	label, err := runLabel(ctx, ctrl, runID)
	if err != nil {
		_, werr := fmt.Fprintf(stderr, "hop view set: %v\n", err)
		return exitFailure, werr
	}
	if err := ctrl.SelectRunView(ctx, label); err != nil {
		_, werr := fmt.Fprintf(stderr, "hop view set: %v\n", err)
		return exitFailure, werr
	}
	_, werr := fmt.Fprintf(stdout, "view set: %s\n", label)
	return exitOK, werr
}

// runViewClear implements `hop view clear`: clears HOP's own view only.
func runViewClear(args []string, stdout, stderr io.Writer, d *deps) (int, error) {
	diagnostics := &recordingWriter{w: stderr}
	flags := flag.NewFlagSet("hop view clear", flag.ContinueOnError)
	flags.SetOutput(diagnostics)
	socketPath := flags.String("socket", "", "herdr server socket (default $HERDR_SOCKET_PATH)")
	if err := flags.Parse(args); err != nil {
		return exitUsage, diagnostics.err
	}
	if flags.NArg() > 0 {
		_, err := fmt.Fprintf(stderr, "hop view clear: unexpected argument %q\n", flags.Arg(0))
		return exitUsage, err
	}
	if *socketPath == "" {
		*socketPath = d.getenv("HERDR_SOCKET_PATH")
	}
	stateRoot, _, err := resolveStateRoot(d.getenv)
	if err != nil {
		_, werr := fmt.Fprintf(stderr, "hop view clear: %v\n", err)
		return exitUsage, werr
	}
	ctx, cancel := context.WithTimeout(context.Background(), defaultViewTimeout)
	defer cancel()
	ctrl, closeStore, err := d.openController(ctx, controllerConfig{stateRoot: stateRoot, socketPath: *socketPath, withRuntime: true})
	if err != nil {
		_, werr := fmt.Fprintf(stderr, "hop view clear: %s\n", describeStoreOpenFailure(err, "the resolved state root"))
		return exitFailure, werr
	}
	defer closeStore() //nolint:errcheck // the store closes on process exit either way; commands report command errors, not pool teardown.

	if err := ctrl.ClearRunView(ctx); err != nil {
		_, werr := fmt.Fprintf(stderr, "hop view clear: %v\n", err)
		return exitFailure, werr
	}
	_, werr := fmt.Fprintln(stdout, "view cleared")
	return exitOK, werr
}
