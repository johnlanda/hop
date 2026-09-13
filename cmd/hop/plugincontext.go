package main

import (
	"flag"
	"fmt"
	"io"

	"github.com/johnlanda/hop/internal/app"
)

// runPluginContext implements `hop plugin-context`: it validates that the
// process was started by a Herdr plugin command and prints the injected
// invocation context as deterministic lines. The Herdr plugin log captures
// this output, so a completed invocation leaves inspectable evidence. It is
// the manifest's startup hook and its context action.
func runPluginContext(args []string, stdout, stderr io.Writer, environ []string) (int, error) {
	diagnostics := &recordingWriter{w: stderr}
	flags := flag.NewFlagSet("hop plugin-context", flag.ContinueOnError)
	flags.SetOutput(diagnostics)
	if err := flags.Parse(args); err != nil {
		return exitUsage, diagnostics.err
	}
	if flags.NArg() > 0 {
		_, err := fmt.Fprintf(stderr, "hop plugin-context: unexpected argument %q\n", flags.Arg(0))
		return exitUsage, err
	}
	invocation := app.InvocationFromEnviron(environ)
	if err := invocation.Validate(); err != nil {
		_, writeErr := fmt.Fprintf(stderr, "hop plugin-context: %v\n", err)
		return exitFailure, writeErr
	}
	_, err := io.WriteString(stdout, invocation.Describe())
	return exitOK, err
}
