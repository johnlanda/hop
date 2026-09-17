//go:build darwin

package herdr

import (
	"encoding/binary"
	"fmt"
	"math"
	"net"
	"syscall"
	"unsafe"
)

// solLocal and localPeerPID are macOS's <sys/un.h> getsockopt level and
// option for a connected Unix-domain socket's peer pid; Go's syscall
// package does not name them.
const (
	solLocal     = 0
	localPeerPID = 2
)

// The darwin sysctl MIB addressing one process's kinfo_proc record:
// sysctl(CTL_KERN, KERN_PROC, KERN_PROC_PID, pid).
const (
	ctlKern     = 1  // CTL_KERN
	kernProc    = 14 // KERN_PROC
	kernProcPID = 1  // KERN_PROC_PID
)

// The kinfo_proc layout (<sys/sysctl.h>, <sys/proc.h>) on both darwin
// targets, which are 64-bit little-endian: the record is exactly
// kinfoProcSize bytes; kp_proc.p_un.__p_starttime is a struct timeval at
// offset 0 (tv_sec an int64, tv_usec an int32); kp_proc.p_pid is an int32
// at offset 40. parseKinfoProcStart accepts a record only when its size and
// the pid it carries both match, so a layout this code does not understand
// yields no identity rather than a misread one. The layout is pinned by
// TestProcessStartTimeMatchesTheProcessTable (lifetime_darwin_test.go)
// against ps's independent start-time column.
const (
	kinfoProcSize            = 648
	kinfoProcStartSecOffset  = 0
	kinfoProcStartUsecOffset = 8
	kinfoProcPIDOffset       = 40
)

// processLifetime renders the lifetime identity of the process pid names
// right now: "herdr-server-lifetime/v1 pid=<pid> start=<sec>.<usec>", the
// start time read from pid's kinfo_proc record. It is "" — unknown, never
// fabricated — for a pid with no live process or a failed or refused read.
//
// A token names exactly one process lifetime. A pid names one live process
// at a time, and a process that reuses a pid is created only after the
// earlier holder exited, so its start time — the fork time the kernel
// records at microsecond resolution and never changes afterwards, exec
// included — is later than the earlier holder's; the only way a reused pid
// could repeat a token is a backwards wall-clock step landing on the
// earlier start's exact microsecond.
func processLifetime(pid int) string {
	sec, usec, ok := processStartTime(pid)
	if !ok {
		return ""
	}
	return fmt.Sprintf("%s pid=%d start=%d.%06d", serverLifetimeTag, pid, sec, usec)
}

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

// processStartTime reads pid's start time from its kinfo_proc record. ok is
// false for a process that does not exist (the kernel answers an empty
// record), a failed sysctl, and a record parseKinfoProcStart refuses.
func processStartTime(pid int) (sec int64, usec int32, ok bool) {
	if pid <= 0 || pid > math.MaxInt32 {
		return 0, 0, false
	}
	raw, err := kinfoProc(int32(pid))
	if err != nil {
		return 0, 0, false
	}
	return parseKinfoProcStart(raw, pid)
}

// parseKinfoProcStart decodes a kinfo_proc record's start time, accepting
// the record only when it is exactly kinfoProcSize bytes and carries pid,
// and the time itself is a positive second with an in-range microsecond.
func parseKinfoProcStart(raw []byte, pid int) (sec int64, usec int32, ok bool) {
	if len(raw) != kinfoProcSize {
		return 0, 0, false
	}
	recordPID := int32(binary.LittleEndian.Uint32(raw[kinfoProcPIDOffset : kinfoProcPIDOffset+4])) //nolint:gosec // G115: the four bytes are the kernel's native int32 p_pid, reinterpreted, not range-converted.
	if int(recordPID) != pid {
		return 0, 0, false
	}
	sec = int64(binary.LittleEndian.Uint64(raw[kinfoProcStartSecOffset : kinfoProcStartSecOffset+8]))    //nolint:gosec // G115: the eight bytes are the kernel's native int64 tv_sec, reinterpreted, not range-converted.
	usec = int32(binary.LittleEndian.Uint32(raw[kinfoProcStartUsecOffset : kinfoProcStartUsecOffset+4])) //nolint:gosec // G115: the four bytes are the kernel's native int32 tv_usec, reinterpreted, not range-converted.
	if sec <= 0 || usec < 0 || usec >= 1_000_000 {
		return 0, 0, false
	}
	return sec, usec, true
}

// kinfoProc reads pid's kinfo_proc record through a raw sysctl(2): the
// stdlib Sysctl cannot address a pid-parameterized MIB, and the dependency
// set has no golang.org/x/sys. The buffer is exactly one record; a kernel
// whose record is larger answers ENOMEM, which is an error here, and a pid
// with no process answers a zero-length record.
func kinfoProc(pid int32) ([]byte, error) {
	mib := [4]int32{ctlKern, kernProc, kernProcPID, pid}
	buf := make([]byte, kinfoProcSize)
	size := uintptr(len(buf))
	_, _, errno := syscall.Syscall6(syscall.SYS___SYSCTL, uintptr(unsafe.Pointer(&mib[0])), uintptr(len(mib)), uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&size)), 0, 0) //nolint:gosec // G103: every unsafe.Pointer is a live Go allocation converted inline in the syscall argument list, per the unsafe.Pointer syscall rule.
	if errno != 0 {
		return nil, errno
	}
	if size > uintptr(len(buf)) {
		return nil, syscall.EIO
	}
	return buf[:size], nil
}
