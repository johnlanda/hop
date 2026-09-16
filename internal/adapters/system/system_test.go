package system_test

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/johnlanda/hop/internal/adapters/system"
	"github.com/johnlanda/hop/internal/domain/identity"
)

func TestClockReportsUTC(t *testing.T) {
	before := time.Now().UTC()

	now := system.Clock{}.Now()

	after := time.Now().UTC()
	if now.Location() != time.UTC {
		t.Errorf("Now().Location() = %v, want UTC", now.Location())
	}
	if now.Before(before) || now.After(after) {
		t.Errorf("Now() = %v, want within [%v, %v]", now, before, after)
	}
}

func TestIDGeneratorMintsCanonicalIdentityParsableUUIDs(t *testing.T) {
	canonical := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	generator := system.IDGenerator{}
	seen := make(map[string]bool)

	for range 1000 {
		id := generator.NewID()

		if !canonical.MatchString(id) {
			t.Fatalf("NewID() = %q, not canonical lowercase UUIDv4", id)
		}
		if _, err := identity.ParseRunID(id); err != nil {
			t.Fatalf("identity.ParseRunID(%q): %v", id, err)
		}
		if seen[id] {
			t.Fatalf("NewID() repeated %q", id)
		}
		seen[id] = true
	}
}

func TestArtifactStoreWriteReadRoundTrip(t *testing.T) {
	cases := []struct {
		name    string
		content []byte
	}{
		{name: "text", content: []byte("assignment body\nwith a second line\n")},
		{name: "empty", content: []byte{}},
		{name: "binary", content: []byte{0x00, 0xff, 0x10, 0x00, 0x7f}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := system.ArtifactStore{}
			path := filepath.Join(t.TempDir(), "runs", "r1", "artifacts", "assignment.md")

			if err := store.WriteArtifact(t.Context(), path, tc.content); err != nil {
				t.Fatalf("WriteArtifact: %v", err)
			}
			got, err := store.ReadArtifact(t.Context(), path)
			if err != nil {
				t.Fatalf("ReadArtifact: %v", err)
			}

			if !bytes.Equal(got, tc.content) {
				t.Errorf("round trip = %q, want %q", got, tc.content)
			}
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if info.Mode().Perm() != 0o600 {
				t.Errorf("artifact mode = %o, want 0600", info.Mode().Perm())
			}
			assertNoTempFiles(t, filepath.Dir(path))
		})
	}
}

func TestArtifactStoreWriteReplacesExistingContentCompletely(t *testing.T) {
	store := system.ArtifactStore{}
	path := filepath.Join(t.TempDir(), "artifact")
	if err := store.WriteArtifact(t.Context(), path, []byte("a much longer first version of the content")); err != nil {
		t.Fatal(err)
	}

	if err := store.WriteArtifact(t.Context(), path, []byte("short")); err != nil {
		t.Fatalf("WriteArtifact over an existing artifact: %v", err)
	}

	got, err := store.ReadArtifact(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "short" {
		t.Errorf("replaced content = %q, want %q", got, "short")
	}
	assertNoTempFiles(t, filepath.Dir(path))
}

func TestArtifactStoreFailedPublishLeavesNoTempFile(t *testing.T) {
	store := system.ArtifactStore{}
	dir := t.TempDir()
	// The destination path is an existing directory, so the publishing
	// rename must fail after the temp file was created and filled.
	target := filepath.Join(dir, "occupied")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}

	err := store.WriteArtifact(t.Context(), target, []byte("content"))

	if err == nil {
		t.Fatal("WriteArtifact over a directory succeeded, want a publish failure")
	}
	assertNoTempFiles(t, dir)
}

func TestArtifactStoreCreatesDeepHierarchyWithPrivateDirectories(t *testing.T) {
	store := system.ArtifactStore{}
	root := t.TempDir()
	path := filepath.Join(root, "runs", "r2", "checks", "op-9", "stdout")

	if err := store.WriteArtifact(t.Context(), path, []byte("out")); err != nil {
		t.Fatalf("WriteArtifact into a fresh hierarchy: %v", err)
	}

	for dir := filepath.Dir(path); dir != root; dir = filepath.Dir(dir) {
		info, err := os.Stat(dir)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o700 {
			t.Errorf("directory %s mode = %o, want 0700", dir, info.Mode().Perm())
		}
	}
	got, err := store.ReadArtifact(t.Context(), path)
	if err != nil || string(got) != "out" {
		t.Errorf("round trip = %q, %v", got, err)
	}
}

func TestArtifactStoreRejectsFileOnTheDirectoryChain(t *testing.T) {
	store := system.ArtifactStore{}
	root := t.TempDir()
	occupied := filepath.Join(root, "runs")
	if err := os.WriteFile(occupied, []byte("file"), 0o600); err != nil {
		t.Fatal(err)
	}

	err := store.WriteArtifact(t.Context(), filepath.Join(occupied, "r1", "artifact"), []byte("x"))

	if err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Errorf("WriteArtifact under a file = %v, want a not-a-directory error", err)
	}
}

func TestArtifactStoreReadMissingArtifactFails(t *testing.T) {
	_, err := system.ArtifactStore{}.ReadArtifact(t.Context(), filepath.Join(t.TempDir(), "absent"))

	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("ReadArtifact error = %v, want it to wrap fs.ErrNotExist", err)
	}
}

// assertNoTempFiles fails when the directory still holds an unpublished
// temp file.
func assertNoTempFiles(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".hop-artifact-") {
			t.Errorf("temp file %s left behind in %s", entry.Name(), dir)
		}
	}
}

// TestArtifactStoreErrorsNeverEchoThePath proves every artifact failure
// renders a fixed category and no path — an artifact lives under the
// operator's state root, and callers print these errors — while the chain
// still classifies through errors.Is.
func TestArtifactStoreErrorsNeverEchoThePath(t *testing.T) {
	const canary = "credential-like-artifact-canary"
	store := system.ArtifactStore{}
	root := filepath.Join(t.TempDir(), canary)
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	occupied := filepath.Join(root, "runs")
	if err := os.WriteFile(occupied, []byte("file"), 0o600); err != nil {
		t.Fatal(err)
	}

	writeErr := store.WriteArtifact(t.Context(), filepath.Join(occupied, "r1", "artifact"), []byte("x"))
	if writeErr == nil || !strings.Contains(writeErr.Error(), "a path element is not a directory") {
		t.Errorf("WriteArtifact under a file = %v, want the not-a-directory category", writeErr)
	}

	readErr := func() error {
		_, err := store.ReadArtifact(t.Context(), filepath.Join(root, "absent"))
		return err
	}()
	if !errors.Is(readErr, fs.ErrNotExist) || !strings.Contains(fmt.Sprint(readErr), "a path element does not exist") {
		t.Errorf("ReadArtifact(absent) = %v, want the not-exist category wrapping fs.ErrNotExist", readErr)
	}

	publishErr := store.WriteArtifact(t.Context(), root, []byte("x")) // a directory is the destination
	if publishErr == nil {
		t.Error("WriteArtifact onto a directory succeeded")
	}

	for _, err := range []error{writeErr, readErr, publishErr} {
		if err != nil && strings.Contains(err.Error(), canary) {
			t.Errorf("error %q echoes the artifact path", err)
		}
	}
}
