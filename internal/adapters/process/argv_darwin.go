package process

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"syscall"
	"unsafe"
)

// The darwin sysctl MIB addressing a process's saved argument area:
// sysctl(CTL_KERN, KERN_PROCARGS2, pid).
const (
	ctlKern       = 1  // CTL_KERN
	kernProcargs2 = 49 // KERN_PROCARGS2
)

// processArgv reads pid's exact argv through the kern.procargs2 sysctl,
// which reports the process's saved argument area byte-for-byte: a native
// int32 argc, the executable path NUL-terminated, NUL padding, then the
// argc NUL-separated argv strings (the environment follows and is never
// read past). Both darwin targets (arm64, amd64) are little-endian, which
// is the native layout the kernel wrote. The sysctl fails for a process
// that is gone, a zombie, or another user's; the caller marks the argv
// unavailable.
func processArgv(pid int) ([]string, error) {
	raw, err := procargs2(pid)
	if err != nil {
		return nil, fmt.Errorf("sysctl kern.procargs2 for pid %d: %w", pid, err)
	}
	if len(raw) < 4 {
		return nil, fmt.Errorf("kern.procargs2 for pid %d is truncated (%d bytes)", pid, len(raw))
	}
	argc := int(binary.LittleEndian.Uint32(raw[:4]))
	if argc <= 0 {
		return nil, fmt.Errorf("kern.procargs2 for pid %d reports argc %d; the process has no readable argument vector", pid, argc)
	}
	rest := raw[4:]
	execPathEnd := bytes.IndexByte(rest, 0)
	if execPathEnd < 0 {
		return nil, fmt.Errorf("kern.procargs2 for pid %d has no terminated executable path", pid)
	}
	rest = rest[execPathEnd:]
	for len(rest) > 0 && rest[0] == 0 {
		rest = rest[1:]
	}
	argv := make([]string, 0, argc)
	for len(argv) < argc {
		end := bytes.IndexByte(rest, 0)
		if end < 0 {
			return nil, fmt.Errorf("kern.procargs2 for pid %d ends inside argument %d of %d", pid, len(argv), argc)
		}
		argv = append(argv, string(rest[:end]))
		rest = rest[end+1:]
	}
	return argv, nil
}

// procargs2 reads the raw kern.procargs2 buffer for pid: one size query,
// one read, retried when the argument area outgrew the reported size in
// between. The standard library's exported Sysctl resolves dotted names
// only and cannot address a pid-parameterized MIB, so the three-integer MIB
// is issued through the raw sysctl(2).
func procargs2(pid int) ([]byte, error) {
	if pid <= 0 || pid > math.MaxInt32 {
		return nil, fmt.Errorf("pid %d is outside the platform pid range", pid)
	}
	mib := [3]int32{ctlKern, kernProcargs2, int32(pid)}
	for range 4 {
		var size uintptr
		if err := rawSysctl(&mib, nil, &size); err != nil {
			return nil, err
		}
		if size == 0 {
			return nil, errors.New("size 0 reported")
		}
		buf := make([]byte, size)
		err := rawSysctl(&mib, &buf[0], &size)
		if err == nil {
			return buf[:size], nil
		}
		if !errors.Is(err, syscall.ENOMEM) {
			return nil, err
		}
	}
	return nil, errors.New("argument area kept outgrowing its reported size")
}

// rawSysctl issues one sysctl(2) with a three-integer MIB. A nil out is the
// size query; the kernel updates size in place either way. Every pointer is
// converted inline in the call expression, per the unsafe.Pointer rule for
// syscall arguments.
func rawSysctl(mib *[3]int32, out *byte, size *uintptr) error {
	_, _, errno := syscall.Syscall6(syscall.SYS___SYSCTL, uintptr(unsafe.Pointer(&mib[0])), uintptr(len(mib)), uintptr(unsafe.Pointer(out)), uintptr(unsafe.Pointer(size)), 0, 0) //nolint:gosec // G103: every unsafe.Pointer is a live Go allocation converted inline in the syscall argument list, per the unsafe.Pointer syscall rule; the stdlib offers no pid-parameterized sysctl and the dependency set has no golang.org/x/sys.
	if errno != 0 {
		return errno
	}
	return nil
}
