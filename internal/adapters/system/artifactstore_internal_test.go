package system

import (
	"errors"
	"path/filepath"
	"slices"
	"testing"
)

// TestWriteArtifactRetryResyncsParentsAfterSyncFailure proves that a retry
// re-establishes ancestor durability a failed first attempt left behind:
// after the first write fails on a parent's fsync (its directories already
// created), the second write must fsync that same parent again — an
// existing directory is never taken as an already-durable one.
func TestWriteArtifactRetryResyncsParentsAfterSyncFailure(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "runs", "r1", "artifacts", "assignment.md")
	var synced []string
	failRootSync := true
	store := ArtifactStore{dirSyncer: func(dir string) error {
		synced = append(synced, dir)
		if failRootSync && dir == root {
			return errors.New("injected parent-sync failure")
		}
		return nil
	}}

	if err := store.WriteArtifact(t.Context(), target, []byte("body")); err == nil {
		t.Fatal("first WriteArtifact succeeded, want the injected parent-sync failure")
	}

	failRootSync = false
	synced = nil
	if err := store.WriteArtifact(t.Context(), target, []byte("body")); err != nil {
		t.Fatalf("retry WriteArtifact: %v", err)
	}

	for _, want := range []string{
		root,
		filepath.Join(root, "runs"),
		filepath.Join(root, "runs", "r1"),
		filepath.Join(root, "runs", "r1", "artifacts"),
	} {
		if !slices.Contains(synced, want) {
			t.Errorf("the retry did not re-fsync %s; synced: %q", want, synced)
		}
	}
}
