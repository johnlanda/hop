package process

import (
	"bytes"
	"fmt"
	"os"
	"strconv"
)

// processArgv reads pid's exact argv from /proc/<pid>/cmdline, where the
// kernel reports the NUL-separated argument vector byte-for-byte. An empty
// file (a zombie or a kernel thread has no argument space) is reported as
// an error so the caller marks the argv unavailable rather than inventing
// an empty one.
func processArgv(pid int) ([]string, error) {
	raw, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/cmdline")
	if err != nil {
		return nil, fmt.Errorf("read /proc/%d/cmdline: %w", pid, err)
	}
	raw = bytes.TrimSuffix(raw, []byte{0})
	if len(raw) == 0 {
		return nil, fmt.Errorf("/proc/%d/cmdline is empty; the process has no readable argument vector", pid)
	}
	parts := bytes.Split(raw, []byte{0})
	argv := make([]string, len(parts))
	for i, part := range parts {
		argv[i] = string(part)
	}
	return argv, nil
}
