package herdr

import (
	"context"
	"encoding/binary"
	"errors"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

// psStartTime reads pid's start time from the process table through ps's
// lstart column, an independent source to the kinfo_proc read under test,
// at its one-second resolution in the local time zone.
func psStartTime(t *testing.T, pid int) time.Time {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ps", "-o", "lstart=", "-p", strconv.Itoa(pid)).Output() //nolint:gosec // G204: the fixed ps binary; the one variable argument is a decimal pid.
	if err != nil {
		t.Fatalf("ps -o lstart= -p %d: %v", pid, err)
	}
	started, err := time.ParseInLocation("Mon Jan _2 15:04:05 2006", strings.TrimSpace(string(out)), time.Local)
	if err != nil {
		t.Fatalf("parse ps lstart %q: %v", out, err)
	}
	return started
}

// TestProcessStartTimeMatchesTheProcessTable pins the kinfo_proc layout
// processStartTime reads: for this process and for a live child, the
// second it reports is the one ps reports, and a pid whose process has
// exited and been reaped yields no start time.
func TestProcessStartTimeMatchesTheProcessTable(t *testing.T) {
	child := exec.CommandContext(t.Context(), "/bin/sleep", "30")
	if err := child.Start(); err != nil {
		t.Fatalf("start child: %v", err)
	}
	childPID := child.Process.Pid
	reaped := false
	reap := func() {
		reaped = true
		if err := child.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
			t.Errorf("kill child: %v", err)
		}
		var exitErr *exec.ExitError
		if err := child.Wait(); err != nil && !errors.As(err, &exitErr) {
			t.Errorf("reap child: %v", err)
		}
	}
	t.Cleanup(func() {
		if !reaped {
			reap()
		}
	})

	for _, pid := range []int{os.Getpid(), childPID} {
		sec, usec, ok := processStartTime(pid)
		if !ok {
			t.Fatalf("processStartTime(%d) reported no start time for a live process", pid)
		}
		if want := psStartTime(t, pid).Unix(); sec != want {
			t.Errorf("processStartTime(%d) second = %d (usec %d), ps reports %d", pid, sec, usec, want)
		}
	}

	reap()
	if sec, usec, ok := processStartTime(childPID); ok {
		t.Errorf("processStartTime(%d) = %d.%06d after the child was reaped, want no start time", childPID, sec, usec)
	}
}

// TestParseKinfoProcStartRefusesUnknownLayouts proves a record is read only
// when its size and the pid it carries both match and its time is sane.
func TestParseKinfoProcStartRefusesUnknownLayouts(t *testing.T) {
	record := func(size, pid int, sec int64, usec int32) []byte {
		raw := make([]byte, size)
		if size >= kinfoProcPIDOffset+4 {
			binary.LittleEndian.PutUint64(raw[kinfoProcStartSecOffset:], uint64(sec))   //nolint:gosec // G115: test fixture writing a signed value's bit pattern.
			binary.LittleEndian.PutUint32(raw[kinfoProcStartUsecOffset:], uint32(usec)) //nolint:gosec // G115: test fixture writing a signed value's bit pattern.
			binary.LittleEndian.PutUint32(raw[kinfoProcPIDOffset:], uint32(pid))        //nolint:gosec // G115: test fixture writing a pid's bit pattern.
		}
		return raw
	}
	if sec, usec, ok := parseKinfoProcStart(record(kinfoProcSize, 4242, 1789614609, 80021), 4242); !ok || sec != 1789614609 || usec != 80021 {
		t.Fatalf("well-formed record = (%d, %d, %t), want (1789614609, 80021, true)", sec, usec, ok)
	}
	for _, tt := range []struct {
		name string
		raw  []byte
	}{
		{"an empty record (no such process)", nil},
		{"a shorter record", record(kinfoProcSize-8, 4242, 1789614609, 80021)},
		{"a longer record", append(record(kinfoProcSize, 4242, 1789614609, 80021), 0, 0, 0, 0, 0, 0, 0, 0)},
		{"another pid's record", record(kinfoProcSize, 4243, 1789614609, 80021)},
		{"a zero start second", record(kinfoProcSize, 4242, 0, 80021)},
		{"a negative start second", record(kinfoProcSize, 4242, -1, 80021)},
		{"a negative microsecond", record(kinfoProcSize, 4242, 1789614609, -1)},
		{"an out-of-range microsecond", record(kinfoProcSize, 4242, 1789614609, 1_000_000)},
	} {
		if sec, usec, ok := parseKinfoProcStart(tt.raw, 4242); ok {
			t.Errorf("%s: parsed (%d, %d), want refused", tt.name, sec, usec)
		}
	}
}
