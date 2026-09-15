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

// procargs2PathAlign is the alignment of the executable-path region at the
// head of the saved argument area: the kernel writes the path and pads it
// with NULs so the argv strings start at the next multiple of 8. The fixed
// rule is what disambiguates that padding from a legitimately empty leading
// argument — the argv area begins exactly at the rounded boundary, never at
// "the first non-NUL byte". The platform regression tests in
// argv_darwin_test.go pin this contract with real children whose argv
// carries empty leading, middle and trailing arguments.
const procargs2PathAlign = 8

// processArgv reads pid's exact argv through the kern.procargs2 sysctl.
// The sysctl fails for a process that is gone, a zombie, or another
// user's; the caller marks the argv unavailable.
func processArgv(pid int) ([]string, error) {
	raw, err := procargs2(pid)
	if err != nil {
		return nil, fmt.Errorf("sysctl kern.procargs2 for pid %d: %w", pid, err)
	}
	argv, err := parseProcargs2(raw)
	if err != nil {
		return nil, fmt.Errorf("kern.procargs2 for pid %d: %w", pid, err)
	}
	return argv, nil
}

// parseProcargs2 decodes one kern.procargs2 buffer exactly. Layout: a
// native int32 argc (both darwin targets are little-endian); the
// executable path in a NUL-padded region of len(path)+1 rounded up to
// procargs2PathAlign; then exactly argc NUL-terminated argv entries with
// empty entries preserved. The environment strings that follow are never
// read: parsing stops at the argc-th terminator, a buffer that ends before
// it is an error, and a non-NUL byte inside the computed padding region is
// an error rather than a guessed argument boundary — so an environment
// value can never be returned as an argument and no argv is ever invented.
func parseProcargs2(raw []byte) ([]string, error) {
	if len(raw) < 4 {
		return nil, fmt.Errorf("buffer is truncated (%d bytes)", len(raw))
	}
	argc := int(int32(binary.LittleEndian.Uint32(raw[:4]))) //nolint:gosec // G115: the buffer's leading word is the kernel's native int32 argc; the uint32 is reinterpreted, not range-converted, and a negative result is rejected on the next line.
	if argc < 0 {
		return nil, fmt.Errorf("argc %d is negative", argc)
	}
	rest := raw[4:]
	pathEnd := bytes.IndexByte(rest, 0)
	if pathEnd < 0 {
		return nil, errors.New("the executable path is not NUL-terminated")
	}
	argvStart := (pathEnd + 1 + procargs2PathAlign - 1) / procargs2PathAlign * procargs2PathAlign
	if argvStart > len(rest) {
		return nil, errors.New("buffer ends inside the executable-path region")
	}
	for _, b := range rest[pathEnd+1 : argvStart] {
		if b != 0 {
			return nil, errors.New("the executable-path padding holds a non-NUL byte; the layout is not the understood one")
		}
	}
	argv := make([]string, 0, argc)
	p := argvStart
	for range argc {
		end := bytes.IndexByte(rest[p:], 0)
		if end < 0 {
			return nil, fmt.Errorf("buffer ends inside argument %d of %d", len(argv), argc)
		}
		argv = append(argv, string(rest[p:p+end]))
		p += end + 1
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
