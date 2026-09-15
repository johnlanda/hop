// Package process implements the application's local-process ports: the
// exec boundary (Exec), the process-group command runner (Runner
// implementing app.CommandRunner) and the process-group inspector
// (GroupInspector implementing app.ProcessGroupInspector).
//
// Process-ownership discipline, shared by every symbol here: each spawned
// process has exactly one owner of its reap (one Wait call site), a process
// group is signaled only while a known-unreaped member of ours provably pins
// its id, and group existence is probed with signal 0, which reads existence
// and can never deliver to a recycled group.
package process

import (
	"fmt"
	"path/filepath"
	"syscall"
)

// Exec replaces the current process with argv via execve, preserving the
// pid. It is the exec boundary of the sanitizing launcher commands
// (`hop launch`, `hop check-exec`): env is the complete environment the new
// image receives — nothing is inherited, and a nil env execs with an empty
// environment. On success Exec never returns; every return is a failure with
// nothing replaced.
//
// argv[0] must be an absolute executable path. A bare name or a relative
// path is rejected before any system call: bare names are PATH-dependent
// (unreliable under macOS path_helper reordering) and a relative path would
// resolve against the working directory, so neither pins which binary runs.
func Exec(argv, env []string) error {
	if len(argv) == 0 {
		return fmt.Errorf("exec: argv is empty; the exec boundary needs an absolute executable path")
	}
	if !filepath.IsAbs(argv[0]) {
		return fmt.Errorf("exec: executable %q is not an absolute path; bare and relative names do not pin which binary runs", argv[0])
	}
	if env == nil {
		env = []string{}
	}
	err := syscall.Exec(argv[0], argv, env) //nolint:gosec // G204: the argv is chosen by the caller from the run's frozen policy and snapshot, and argv[0] is enforced absolute above; executing it is this boundary's purpose.
	return fmt.Errorf("exec %s: %w", argv[0], err)
}
