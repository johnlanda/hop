//go:build !darwin

package herdr

import "net"

// peerPID reports false unconditionally: no server-lifetime identity is
// implemented for this platform, because none has been pinned by an
// executed probe here.
func peerPID(net.Conn) (int, bool) {
	return 0, false
}

// processLifetime reports "" (unknown) unconditionally, for the same
// reason as peerPID. Every continuity decision reads an unknown identity as
// not established, so on these platforms each such decision fails closed.
func processLifetime(int) string {
	return ""
}
