//go:build !darwin

package integration

// sessionIDUnavailable is why the session-leader check is skipped here.
const sessionIDUnavailable = "the syscall package offers no getsid(2) on this platform; the pane process's session-leader relation is pinned on darwin only"

// processSessionID reports ok false: this suite reads no session id on
// this platform, and the caller skips the check with sessionIDUnavailable.
func processSessionID(int) (sid int, ok bool, err error) {
	return 0, false, nil
}
