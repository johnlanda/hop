//go:build !darwin && !linux

package herdr

import "net"

// peerPID reports false unconditionally: no peer-pid lookup is implemented
// for this platform. ServerInstance's "" with a nil error contract covers
// this case without a build failure.
func peerPID(net.Conn) (int, bool) {
	return 0, false
}
