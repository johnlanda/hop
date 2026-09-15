package process

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"strconv"
)

// processArgv reads pid's exact argv from /proc/<pid>/cmdline, where the
// kernel reports the NUL-separated argument vector byte-for-byte.
func processArgv(pid int) ([]string, error) {
	raw, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/cmdline")
	if err != nil {
		return nil, fmt.Errorf("read /proc/%d/cmdline: %w", pid, err)
	}
	argv, err := parseCmdline(raw)
	if err != nil {
		return nil, fmt.Errorf("/proc/%d/cmdline: %w", pid, err)
	}
	return argv, nil
}

// parseCmdline decodes one /proc cmdline buffer exactly: entries are
// NUL-terminated, and one trailing terminator is removed before splitting
// so empty entries — leading, middle or trailing, including a lone empty
// argv[0] (the buffer "\x00") — are preserved byte-for-byte. An empty
// buffer is an error, not an empty vector: a zombie or a kernel thread has
// no readable argument space, and the caller marks the argv unavailable
// rather than inventing one.
func parseCmdline(raw []byte) ([]string, error) {
	if len(raw) == 0 {
		return nil, errors.New("empty; the process has no readable argument vector")
	}
	raw = bytes.TrimSuffix(raw, []byte{0})
	parts := bytes.Split(raw, []byte{0})
	argv := make([]string, len(parts))
	for i, part := range parts {
		argv[i] = string(part)
	}
	return argv, nil
}
