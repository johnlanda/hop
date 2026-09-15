package process

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"

	"github.com/johnlanda/hop/internal/app"
)

// ArgvUnavailable is the single Argv entry of a group member whose exact
// argument vector could not be read (the process exited between the
// membership listing and the argv read, is a zombie, or denied the read).
// It can never equal a real frozen argv, so an application-side match
// against it fails and the classification falls to the ambiguous,
// fail-closed outcome instead of adopting a guessed argv.
const ArgvUnavailable = "<hop:argv-unavailable>"

// GroupInspector implements app.ProcessGroupInspector. Membership comes
// from one `ps -A -o pid=,pgid=` listing of the platform process table;
// each member's argv comes from the exact per-pid source (/proc/<pid>/
// cmdline on linux, the kern.procargs2 sysctl on darwin), never from ps's
// whitespace-joined command column, so an argument containing whitespace
// survives byte-for-byte.
//
// Recycled-id limitation, stated honestly: no observable surface carries a
// process start time, so neither a pid nor a group id proves identity by
// itself. The application lists and argv-matches a group immediately before
// signaling it (the group-retirement rule); the window between that listing
// and the signal, in which a fully-exited group's id could be recycled, is
// a documented residual limitation of the cooperative model, not a solved
// race.
type GroupInspector struct {
	// psPath overrides the ps executable for tests; empty resolves "ps"
	// from PATH at call time.
	psPath string
}

var _ app.ProcessGroupInspector = GroupInspector{}

// GroupProcesses lists the live members of pgid with pid and exact argv. A
// listing that cannot be made — an uninspectable group id (pgid <= 1), an
// unresolvable or failing ps, unparseable ps output — is an error, never an
// empty result; an empty slice from a successful listing means the group
// has no member left. A member whose argv cannot be read is reported with
// the single entry ArgvUnavailable rather than dropped or guessed.
func (g GroupInspector) GroupProcesses(ctx context.Context, pgid int) ([]app.GroupProcess, error) {
	if pgid <= 1 {
		return nil, fmt.Errorf("process group id %d is not inspectable; a real group id is greater than 1", pgid)
	}
	out, err := g.listProcessTable(ctx)
	if err != nil {
		return nil, err
	}
	pids, err := parseGroupMembers(out, pgid)
	if err != nil {
		return nil, err
	}
	members := make([]app.GroupProcess, 0, len(pids))
	for _, pid := range pids {
		argv, argvErr := processArgv(pid)
		if argvErr != nil {
			argv = []string{ArgvUnavailable}
		}
		members = append(members, app.GroupProcess{PID: pid, Argv: argv})
	}
	return members, nil
}

// SignalGroup sends the one SIGKILL to the whole group, guarded so a bogus
// id can never broadcast (pgid <= 1) or hit this process's own group. An
// ESRCH answer means the group is already empty — the goal state — and is
// success. The caller applies the group-retirement rule first: it never
// signals a group it has not just listed and argv-matched.
func (GroupInspector) SignalGroup(ctx context.Context, pgid int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return killGroup(pgid)
}

// listProcessTable runs one full-table ps with fixed pid and pgid columns
// and returns its stdout. Any failure carries ps's stderr.
func (g GroupInspector) listProcessTable(ctx context.Context) ([]byte, error) {
	psPath := g.psPath
	if psPath == "" {
		resolved, err := exec.LookPath("ps")
		if err != nil {
			return nil, fmt.Errorf("resolve ps for the process-group listing: %w", err)
		}
		psPath = resolved
	}
	cmd := exec.CommandContext(ctx, psPath, "-A", "-o", "pid=,pgid=") //nolint:gosec // G204: the executable is the platform ps resolved from PATH (or a test-injected stub) with fixed arguments.
	out, err := cmd.Output()
	if err != nil {
		detail := ""
		if exitErr, ok := errors.AsType[*exec.ExitError](err); ok {
			detail = ": " + strings.TrimSpace(string(exitErr.Stderr))
		}
		return nil, fmt.Errorf("list the process table with ps: %w%s", err, detail)
	}
	return out, nil
}

// parseGroupMembers extracts the pids belonging to pgid from ps output,
// where every line is "<pid> <pgid>". A line that does not parse is a
// listing error, never skipped: a row that cannot be attributed could be a
// member this inspection would otherwise silently miss.
func parseGroupMembers(out []byte, pgid int) ([]int, error) {
	var pids []int
	for line := range strings.Lines(string(out)) {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		fields := strings.Fields(trimmed)
		if len(fields) != 2 {
			return nil, fmt.Errorf("unparseable ps row %q: want exactly a pid and a pgid", trimmed)
		}
		pid, err := strconv.Atoi(fields[0])
		if err != nil {
			return nil, fmt.Errorf("unparseable pid in ps row %q: %w", trimmed, err)
		}
		rowPgid, err := strconv.Atoi(fields[1])
		if err != nil {
			return nil, fmt.Errorf("unparseable pgid in ps row %q: %w", trimmed, err)
		}
		if rowPgid == pgid {
			pids = append(pids, pid)
		}
	}
	return pids, nil
}
