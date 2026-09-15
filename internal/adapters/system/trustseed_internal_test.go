package system

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// externalAtomicWrite replaces config the way an external writer (a
// running Claude of the same profile) does: a same-directory temp file
// published by rename.
func externalAtomicWrite(t *testing.T, config, content string) {
	t.Helper()
	tmp := config + ".external-tmp"
	if err := os.WriteFile(tmp, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, config); err != nil {
		t.Fatal(err)
	}
}

// TestTrustSeederRetriesOverInterleavedExternalWrite proves the detected
// half of the best-effort contract deterministically: an external atomic
// write landing between the seed's edit computation and its pre-rename
// freshness check is observed by that check — the seeder discards its
// stale edit, redoes it on the fresh content, and the final document
// carries both the external update and the seed.
func TestTrustSeederRetriesOverInterleavedExternalWrite(t *testing.T) {
	dir := t.TempDir()
	config := filepath.Join(dir, ".claude.json")
	if err := os.WriteFile(config, []byte(`{"externalCounter":1,"projects":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	seeder := TrustSeeder{beforePublishCheck: func(attempt int) {
		if attempt == 0 {
			externalAtomicWrite(t, config, `{"externalCounter":2,"newExternalField":"kept","projects":{}}`)
		}
	}}

	outcome, err := seeder.SeedWorkspaceTrust(context.Background(), config, "/private/var/worktrees/hop-run-1")
	if err != nil || !outcome.Seeded {
		t.Fatalf("SeedWorkspaceTrust = %+v, %v", outcome, err)
	}

	content, err := os.ReadFile(config) //nolint:gosec // G304: a path this test constructed itself under t.TempDir.
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		ExternalCounter  int    `json:"externalCounter"`
		NewExternalField string `json:"newExternalField"`
		Projects         map[string]struct {
			HasTrustDialogAccepted bool `json:"hasTrustDialogAccepted"`
		} `json:"projects"`
	}
	if err := json.Unmarshal(content, &doc); err != nil {
		t.Fatalf("final config is not valid JSON: %v\n%s", err, content)
	}
	if doc.ExternalCounter != 2 || doc.NewExternalField != "kept" {
		t.Errorf("the interleaved external update was lost: %s", content)
	}
	if !doc.Projects["/private/var/worktrees/hop-run-1"].HasTrustDialogAccepted {
		t.Errorf("the seed did not land on the fresh content: %s", content)
	}
}

// TestTrustSeederGivesUpUnderConstantExternalWrites proves the bounded
// budget: a config rewritten by an external writer inside every attempt's
// window yields a not-seeded outcome — the launch proceeds on the
// interactive fallback — and the external writer's last content survives
// untouched.
func TestTrustSeederGivesUpUnderConstantExternalWrites(t *testing.T) {
	dir := t.TempDir()
	config := filepath.Join(dir, ".claude.json")
	if err := os.WriteFile(config, []byte(`{"externalCounter":0,"projects":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	attempts := 0
	seeder := TrustSeeder{beforePublishCheck: func(attempt int) {
		attempts++
		externalAtomicWrite(t, config, fmt.Sprintf(`{"externalCounter":%d,"projects":{}}`, attempt+1))
	}}

	outcome, err := seeder.SeedWorkspaceTrust(context.Background(), config, "/private/var/worktrees/hop-run-1")
	if err != nil {
		t.Fatalf("SeedWorkspaceTrust: %v", err)
	}
	if outcome.Seeded || !strings.Contains(outcome.Reason, "external writer") {
		t.Fatalf("outcome = %+v, want the not-seeded external-writer reason", outcome)
	}
	if attempts != trustSeedAttempts {
		t.Errorf("attempts = %d, want the full bounded budget %d", attempts, trustSeedAttempts)
	}

	content, err := os.ReadFile(config) //nolint:gosec // G304: a path this test constructed itself under t.TempDir.
	if err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf(`{"externalCounter":%d,"projects":{}}`, trustSeedAttempts)
	if string(content) != want {
		t.Errorf("final config = %s, want the external writer's last content %s untouched", content, want)
	}
}
