package app

import (
	"context"
	"time"
)

// Command is one process invocation: the complete argv, working directory
// and complete environment (not additions — unlike the Runtime pane env).
// MaxOutputBytes bounds each captured stream of this invocation; 0 keeps
// the runner's default bound (1 MiB), and a negative value is refused
// before anything starts. A larger bound never removes the truncation
// report.
type Command struct {
	Argv           []string
	Dir            string
	Env            []string
	MaxOutputBytes int
}

// CommandResult is one completed invocation's outcome. Each captured
// stream is bounded by the runner; StdoutTruncated and StderrTruncated
// report that the runner discarded bytes of that stream past its bound, so
// the stream holds only a prefix of what the command wrote, whatever its
// exit status. A caller whose decision depends on a stream's whole content
// treats a truncated stream as unobserved.
type CommandResult struct {
	ExitCode        int
	Stdout          []byte
	Stderr          []byte
	StdoutTruncated bool
	StderrTruncated bool
	Duration        time.Duration
}

// CommandRunner executes argv as the leader of a new process group and
// kills the whole group on ctx cancellation. Durable execution identity is
// not this port's job: the exec-boundary commands record their own pid
// before exec (sections 6-7); CommandRunner is used for the check's git
// operations and for spawning hop check-exec.
type CommandRunner interface {
	Run(ctx context.Context, cmd Command) (CommandResult, error)
}

// GroupProcess is one member of a process group, as the platform process
// table reports it: pid and argv.
type GroupProcess struct {
	PID  int
	Argv []string
}

// ProcessGroupInspector lists and signals a recorded process group. Group
// retirement after takeover has no surviving CommandRunner context to
// cancel, so it is a separate, consumer-owned port. Ownership
// classification stays in application code (ClassifyGroupRetirement); the
// port itself only observes and signals.
type ProcessGroupInspector interface {
	// GroupProcesses lists the group's members via the platform process
	// table. A listing that cannot be made is an error, never an empty
	// result — the caller distinguishes "empty" from "inspection failed".
	GroupProcesses(ctx context.Context, pgid int) ([]GroupProcess, error)
	// SignalGroup kills the whole group.
	SignalGroup(ctx context.Context, pgid int) error
}
