package integration

import "syscall"

// sessionIDUnavailable is empty on darwin: processSessionID always reads
// the session id here, so the session-leader check never skips.
const sessionIDUnavailable = ""

// processSessionID reads pid's session id with getsid(2). ok is always true
// on darwin, where the syscall package provides it.
func processSessionID(pid int) (sid int, ok bool, err error) {
	sid, err = syscall.Getsid(pid)
	return sid, true, err
}
