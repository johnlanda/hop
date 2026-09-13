// Package main is the hop composition root. It parses the command line,
// constructs concrete dependencies and dispatches to the selected command.
// No business rule lives here; the only command today reports the build version.
package main

import (
	"fmt"
	"io"
	"os"
)

// Process exit codes returned by run.
const (
	exitOK      = 0
	exitFailure = 1
	exitUsage   = 2
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run executes the command named by args and returns the process exit code.
// Command output goes to stdout; usage text and diagnostics go to stderr.
// A failed write to either stream leaves nothing to report to, so it is
// reported only through exitFailure.
func run(args []string, stdout, stderr io.Writer) int {
	code, err := dispatch(args, stdout, stderr)
	if err != nil {
		return exitFailure
	}
	return code
}

// dispatch selects the command for args and returns its exit code. The error
// is non-nil only when writing to stdout or stderr failed.
func dispatch(args []string, stdout, stderr io.Writer) (int, error) {
	if len(args) == 0 {
		return exitUsage, printUsage(stderr)
	}
	switch args[0] {
	case "version":
		return runVersion(args[1:], stdout, stderr)
	case "help", "-h", "-help", "--help":
		return exitOK, printUsage(stdout)
	default:
		if _, err := fmt.Fprintf(stderr, "hop: unknown command %q\n\n", args[0]); err != nil {
			return exitUsage, err
		}
		return exitUsage, printUsage(stderr)
	}
}

// printUsage writes the top-level command summary.
func printUsage(w io.Writer) error {
	_, err := fmt.Fprint(w, "Usage: hop <command> [arguments]\n\n"+
		"Commands:\n"+
		"  version    Print the hop build version\n"+
		"  help       Print this usage text\n")
	return err
}
