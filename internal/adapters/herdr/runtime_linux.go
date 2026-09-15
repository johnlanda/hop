//go:build linux

package herdr

import (
	"net"
	"syscall"
)

// peerPID reads conn's connected peer pid via getsockopt(SOL_SOCKET,
// SO_PEERCRED). It reports false, never a fabricated pid, when conn is not
// backed by a real socket descriptor or the option could not be read.
func peerPID(conn net.Conn) (int, bool) {
	syscallConn, ok := conn.(syscall.Conn)
	if !ok {
		return 0, false
	}
	rawConn, err := syscallConn.SyscallConn()
	if err != nil {
		return 0, false
	}
	var ucred *syscall.Ucred
	var sockErr error
	if ctrlErr := rawConn.Control(func(fd uintptr) {
		ucred, sockErr = syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
	}); ctrlErr != nil || sockErr != nil || ucred == nil {
		return 0, false
	}
	return int(ucred.Pid), true
}
