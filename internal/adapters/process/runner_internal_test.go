package process

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/johnlanda/hop/internal/app"
)

// TestBoundedBufferTruncation pins the capture bound's accounting over
// exact write sequences: the kept prefix, the full length reported for
// every write, and the truncated flag set exactly when a byte is discarded
// — never for a write that ends on the bound, always for any later byte.
func TestBoundedBufferTruncation(t *testing.T) {
	cases := []struct {
		name      string
		limit     int
		writes    []int
		wantKept  int
		wantTrunc bool
	}{
		{name: "under the bound", limit: 10, writes: []int{3, 4}, wantKept: 7},
		{name: "a write ending on the bound", limit: 10, writes: []int{6, 4}, wantKept: 10},
		{name: "one write filling the bound", limit: 10, writes: []int{10}, wantKept: 10},
		{name: "an empty write after the bound", limit: 10, writes: []int{10, 0}, wantKept: 10},
		{name: "a byte after a write ending on the bound", limit: 10, writes: []int{10, 1}, wantKept: 10, wantTrunc: true},
		{name: "a write crossing the bound", limit: 10, writes: []int{8, 3}, wantKept: 10, wantTrunc: true},
		{name: "one write past the bound", limit: 10, writes: []int{11}, wantKept: 10, wantTrunc: true},
		{name: "nothing written", limit: 10},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := &boundedBuffer{limit: tc.limit}
			var want []byte
			for i, n := range tc.writes {
				p := bytes.Repeat([]byte{byte('a' + i)}, n)
				want = append(want, p...)
				if written, err := b.Write(p); written != n || err != nil {
					t.Fatalf("Write(%d bytes) = %d, %v; want the full length and no error", n, written, err)
				}
			}
			if got := b.bytes(); !bytes.Equal(got, want[:min(len(want), tc.limit)]) || len(got) != tc.wantKept {
				t.Errorf("kept %q, want the first %d bytes of %q", got, tc.wantKept, want)
			}
			if b.truncated != tc.wantTrunc {
				t.Errorf("truncated = %v, want %v", b.truncated, tc.wantTrunc)
			}
		})
	}
}

// TestRunnerAnchorFailureRetiresLiveLeader proves the unanchored path: when
// the group anchor cannot be started, no completion is inferred from the
// failure — the still-live leader's group is retired while the leader is
// provably unreaped, and Run returns an error naming the anchor failure
// rather than a fabricated result. The fixture leader is the never-exiting
// sleeper, so a clean exit can only mean the discipline was bypassed.
func TestRunnerAnchorFailureRetiresLiveLeader(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	runner := Runner{anchorPath: filepath.Join(dir, "missing-anchor-binary")}

	result, err := runner.Run(t.Context(), app.Command{
		Argv: []string{exe},
		Env: []string{
			"HOP_PROCESS_HELPER=sleeper",
			"HOP_HELPER_PIDFILE=" + filepath.Join(dir, "leader.pid"),
		},
	})

	if err == nil {
		t.Fatalf("Run with an unstartable anchor succeeded (%+v); a live leader must be retired with an error", result)
	}
	if !strings.Contains(err.Error(), "anchor") {
		t.Errorf("error %q does not name the anchor failure", err)
	}
	if result.ExitCode != -1 {
		t.Errorf("ExitCode = %d, want -1: the live leader must have been killed by the group retirement", result.ExitCode)
	}
}
