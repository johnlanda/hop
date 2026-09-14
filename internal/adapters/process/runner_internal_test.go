package process

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/johnlanda/hop/internal/app"
)

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
