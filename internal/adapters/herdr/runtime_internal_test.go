package herdr

import (
	"net"
	"testing"
)

// TestPeerPIDUnavailableForNonSocketConn proves peerPID reports false,
// never a fabricated pid, for a connection not backed by a real socket
// descriptor. net.Pipe's connections implement no syscall.Conn on any
// platform, so this is portable and exercises every peerPID
// implementation's defensive type check identically, on every platform
// this package builds for.
func TestPeerPIDUnavailableForNonSocketConn(t *testing.T) {
	client, server := net.Pipe()
	t.Cleanup(func() {
		closeConn(client)
		closeConn(server)
	})

	pid, ok := peerPID(client)

	if ok {
		t.Errorf("peerPID = (%d, true) for a net.Pipe connection, want (0, false)", pid)
	}
	if pid != 0 {
		t.Errorf("pid = %d, want 0 when unavailable", pid)
	}
}
