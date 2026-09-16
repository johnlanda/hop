// Package main is the hop composition root. It parses the command line,
// constructs concrete dependencies and dispatches to the selected command.
// No business rule lives here: version reports the build, doctor wires the
// herdr installation probe into the application's doctor use case, and
// plugin-context reports the plugin invocation environment.
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
	case "doctor":
		return runDoctor(args[1:], stdout, stderr, os.Getenv)
	case "plugin-context":
		return runPluginContext(args[1:], stdout, stderr, os.Environ())
	case "run":
		return runRun(args[1:], stdout, stderr, defaultDeps())
	case "status":
		return runStatus(args[1:], stdout, stderr, defaultDeps())
	case "stop":
		return runStop(args[1:], stdout, stderr, defaultDeps())
	case "resume":
		return runResume(args[1:], stdout, stderr, defaultDeps())
	case "result":
		return runResult(args[1:], stdout, stderr, defaultDeps())
	case "launch":
		return runLaunch(args[1:], stdout, stderr, defaultDeps())
	case "check-exec":
		return runCheckExec(args[1:], stdout, stderr, defaultDeps())
	case "task":
		return runTask(args[1:], stdout, stderr, defaultDeps())
	case "plan":
		return runPlan(args[1:], stdout, stderr, defaultDeps())
	case "msg":
		return runMsg(args[1:], stdout, stderr, defaultDeps())
	case "answer":
		return runAnswer(args[1:], stdout, stderr, defaultDeps())
	case "review":
		return runReview(args[1:], stdout, stderr, defaultDeps())
	case "view":
		return runView(args[1:], stdout, stderr, defaultDeps())
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
		"  run \"<brief>\"   Start a run and stay as its foreground controller\n"+
		"  status          List the repository's active runs (-all includes finished; -run for one run's detail)\n"+
		"  stop <run-id>   Request and drive a stop until termination is observed\n"+
		"  resume <run-id> Reacquire a run's lease and reconcile, then continue as its controller\n"+
		"  result submit   Submit a worker result (worker-facing; a transient first line signals retry)\n"+
		"  version         Print the hop build version\n"+
		"  doctor          Check the Herdr installation, harness support and the state root\n"+
		"  plugin-context  Print the Herdr plugin invocation environment\n"+
		"  view            Select or clear HOP's native Agents view (set --run ID | clear)\n"+
		"  answer          Answer a question relayed to the human (controller-machine command)\n"+
		"  help            Print this usage text\n\n"+
		"Worker plumbing (spawned by HOP, not for direct use):\n"+
		"  launch          Worker exec boundary: claim, sanitize and exec the harness\n"+
		"  check-exec      Check exec boundary: claim, sanitize and exec the frozen check command\n"+
		"  task            Manager plan verbs: create | retry <task-id>\n"+
		"  plan            Manager plan verb: close\n"+
		"  msg             Messaging verbs: send | next | wait | ack <id> | show <id>\n"+
		"  review          Reviewer verb: submit --verdict --subject --reasons-file\n")
	return err
}
