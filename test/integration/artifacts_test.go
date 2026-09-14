package integration

import (
	"os"
	"testing"

	"github.com/johnlanda/hop/internal/adapters/herdr"
)

// TestRealProcessArtifactDirRemovedOnPassingRun exercises the real teardown:
// a passing run that starts a disposable server, creates a fixture of panes
// with shells, and opens a live subscription (the shape that regressed
// retention) must still leave nothing under the artifact base. The inner
// subtest's cleanups — stop (kill the group, wait for it to empty, close the
// logs), remove the roots, remove the artifact directory — all run before
// t.Run returns, so the parent can assert the directory is gone.
func TestRealProcessArtifactDirRemovedOnPassingRun(t *testing.T) {
	var artifactPath string
	var ran bool
	t.Run("passing server run", func(t *testing.T) {
		artifacts := newArtifactDir(t)
		artifactPath = artifacts.path
		server := prepareServer(t, artifacts) // skips here when no herdr binary
		server.start(t)
		ran = true

		fixture := server.createRunFixture(t)
		stream, err := herdr.NewObserver(server.socketPath, fixture.implementer).Subscribe(testContext(t))
		if err != nil {
			t.Fatalf("subscribe: %v", err)
		}
		if err := stream.Close(); err != nil {
			t.Fatalf("close subscription: %v", err)
		}
	})

	if !ran {
		t.Skip("real-process suite skipped, not run: no herdr binary")
	}
	if _, err := os.Stat(artifactPath); !os.IsNotExist(err) {
		t.Errorf("artifact dir %s survived a passing real-server run (stat err: %v); the teardown left evidence behind", artifactPath, err)
	}
}

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
