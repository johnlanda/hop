package sqlite_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/johnlanda/hop/internal/adapters/sqlite"
)

// TestOpenEscapesStateRootInDSN proves filesystem characters in the state
// root are never read as SQLite URI syntax: for every root the database
// lands at exactly <root>/hop.db, the per-connection PRAGMAs hold on both
// pools, and initialized data survives a close and reopen — in particular
// a mode=memory lookalike directory name cannot make the store ephemeral.
func TestOpenEscapesStateRootInDSN(t *testing.T) {
	cases := []struct {
		name string
		dir  string
	}{
		{name: "hash", dir: "hash#name"},
		{name: "question mark", dir: "question?x=y"},
		{name: "percent escape", dir: "percent%41"},
		{name: "space", dir: "space in root"},
		{name: "unicode", dir: "räume-Übung"},
		{name: "memory URI lookalike", dir: "memory?mode=memory&cache=shared&x="},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			base := t.TempDir()
			root := filepath.Join(base, tc.dir)
			clock := newFakeClock()
			store, err := sqlite.Open(t.Context(), root, sqlite.Options{Clock: clock})
			if err != nil {
				t.Fatalf("open on root %q: %v", root, err)
			}

			wantPath := filepath.Join(root, "hop.db")
			if _, statErr := os.Stat(wantPath); statErr != nil {
				t.Fatalf("database file is not at %q: %v", wantPath, statErr)
			}
			entries, err := os.ReadDir(base)
			if err != nil {
				t.Fatalf("read base directory: %v", err)
			}
			if len(entries) != 1 || entries[0].Name() != tc.dir {
				t.Fatalf("base holds %v, want only the requested root %q — a sibling means URI syntax leaked", entries, tc.dir)
			}
			assertPool := func(db string) {
				t.Helper()
				pool := sqlite.WriteDB(store)
				if db == "reads" {
					pool = sqlite.ReadDB(store)
				}
				var file string
				if listErr := pool.QueryRowContext(t.Context(),
					`SELECT file FROM pragma_database_list WHERE name = 'main'`,
				).Scan(&file); listErr != nil {
					t.Fatalf("%s pool database_list: %v", db, listErr)
				}
				resolvedGot, resolveErr := filepath.EvalSymlinks(file)
				if resolveErr != nil {
					t.Fatalf("resolve reported path %q: %v", file, resolveErr)
				}
				resolvedWant, resolveErr := filepath.EvalSymlinks(wantPath)
				if resolveErr != nil {
					t.Fatalf("resolve expected path %q: %v", wantPath, resolveErr)
				}
				if resolvedGot != resolvedWant {
					t.Fatalf("%s pool opened %q, want %q", db, resolvedGot, resolvedWant)
				}
				var foreignKeys, busyTimeout string
				if scanErr := pool.QueryRowContext(t.Context(), `PRAGMA foreign_keys`).Scan(&foreignKeys); scanErr != nil {
					t.Fatalf("%s pool foreign_keys: %v", db, scanErr)
				}
				if scanErr := pool.QueryRowContext(t.Context(), `PRAGMA busy_timeout`).Scan(&busyTimeout); scanErr != nil {
					t.Fatalf("%s pool busy_timeout: %v", db, scanErr)
				}
				if foreignKeys != "1" || busyTimeout != "5000" {
					t.Fatalf("%s pool settings = (foreign_keys %s, busy_timeout %s), want (1, 5000)", db, foreignKeys, busyTimeout)
				}
			}
			assertPool("writes")
			assertPool("reads")

			spec := newSpec("/repos/alpha", specStride, clock.Now())
			if _, _, initErr := store.InitializeRun(t.Context(), spec); initErr != nil {
				t.Fatalf("InitializeRun on root %q: %v", root, initErr)
			}
			if closeErr := store.Close(); closeErr != nil {
				t.Fatalf("close: %v", closeErr)
			}

			reopened, err := sqlite.Open(t.Context(), root, sqlite.Options{Clock: clock})
			if err != nil {
				t.Fatalf("reopen on root %q: %v", root, err)
			}
			defer func() {
				if closeErr := reopened.Close(); closeErr != nil {
					t.Errorf("close reopened store: %v", closeErr)
				}
			}()
			statuses, err := reopened.ListRuns(t.Context(), "/repos/alpha")
			if err != nil {
				t.Fatalf("ListRuns after reopen: %v", err)
			}
			if len(statuses) != 1 || statuses[0].RunID != spec.RunID {
				t.Fatalf("runs after reopen = %+v, want the one initialized run — data must survive reopen", statuses)
			}
		})
	}
}
