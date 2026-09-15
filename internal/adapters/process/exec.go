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
	return ExecResolved(argv[0], argv, env)
}

// ExecResolved replaces the current process with the binary at path via
// execve, preserving the pid AND argv exactly as given: argv[0] may be the
// bare name the caller already resolved to path. It is the exec boundary
// of `hop check-exec`, whose executed argv must stay byte-identical to the
// run's frozen check argv — group retirement matches a running member's
// argv against that exact frozen value, so rewriting argv[0] to the
// resolved path would make an owned check unrecognizable. path must be
// absolute, which pins which binary runs regardless of what argv[0] says;
// env is the complete environment of the new image, nil execs with an
// empty environment. On success ExecResolved never returns; every return
// is a failure with nothing replaced.
func ExecResolved(path string, argv, env []string) error {
	if len(argv) == 0 {
		return fmt.Errorf("exec: argv is empty; the new process image needs at least argv[0]")
	}
	if !filepath.IsAbs(path) {
		return fmt.Errorf("exec: executable %q is not an absolute path; bare and relative names do not pin which binary runs", path)
	}
	if env == nil {
		env = []string{}
	}
	err := syscall.Exec(path, argv, env) //nolint:gosec // G204: path and argv are chosen by the caller from the run's frozen policy and snapshot, and path is enforced absolute above; executing it is this boundary's purpose.
	return fmt.Errorf("exec %s: %w", path, err)
}
