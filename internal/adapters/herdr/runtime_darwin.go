//go:build darwin

package herdr

import (
	"net"
	"syscall"
)

// solLocal and localPeerPID are macOS's <sys/un.h> getsockopt level and
// option for a connected Unix-domain socket's peer pid; Go's syscall
// package does not name them.
const (
	solLocal     = 0
	localPeerPID = 2
)

// peerPID reads conn's connected peer pid via getsockopt(SOL_LOCAL,
// LOCAL_PEERPID). It reports false, never a fabricated pid, when conn is
// not backed by a real socket descriptor or the option could not be read.
func peerPID(conn net.Conn) (int, bool) {
	syscallConn, ok := conn.(syscall.Conn)
	if !ok {
		return 0, false
	}
	rawConn, err := syscallConn.SyscallConn()
	if err != nil {
		return 0, false
	}
	var pid int
	var sockErr error
	if ctrlErr := rawConn.Control(func(fd uintptr) {
		pid, sockErr = syscall.GetsockoptInt(int(fd), solLocal, localPeerPID)
	}); ctrlErr != nil || sockErr != nil {
		return 0, false
	}
	return pid, true
}
