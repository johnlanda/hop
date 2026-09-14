package integration

import (
	"os"
	"testing"
)

// TestArtifactDirRemovedOnPassingRun asserts the retention contract: a
// passing test leaves nothing under the shared artifact base. It runs an
// inner subtest that creates and writes an artifact directory and passes; the
// subtest's cleanups run before t.Run returns, so the parent can assert the
// directory is gone. This does not require the herdr binary.
func TestArtifactDirRemovedOnPassingRun(t *testing.T) {
	var path string
	t.Run("passing inner", func(t *testing.T) {
		artifacts := newArtifactDir(t)
		artifacts.save(t, "evidence.txt", "some evidence")
		path = artifacts.path
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("artifact directory was not created: %v", err)
		}
	})

	if path == "" {
		t.Fatal("inner subtest did not record the artifact path")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("artifact directory %s survived a passing subtest (stat err: %v); the success path must leave nothing", path, err)
	}
}

// TestArtifactRetentionDecision covers the other half of the contract at the
// decision level: a failing run retains, a passing run removes. The
// subprocess-teardown path cannot be exercised here without failing this
// test, since a failing subtest fails its parent, so the retain/remove choice
// is checked directly.
func TestArtifactRetentionDecision(t *testing.T) {
	cases := []struct {
		name       string
		failed     bool
		wantRetain bool
	}{
		{name: "passing run removes", failed: false, wantRetain: false},
		{name: "failing run retains", failed: true, wantRetain: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := retainArtifacts(tc.failed); got != tc.wantRetain {
				t.Errorf("retainArtifacts(%t) = %t, want %t", tc.failed, got, tc.wantRetain)
			}
		})
	}
}
