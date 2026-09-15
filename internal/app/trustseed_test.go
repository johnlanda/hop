package app

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestPlanTrustSeed(t *testing.T) {
	const worktree = "/private/var/worktrees/hop-run-1"
	cases := []struct {
		name       string
		harness    string
		env        []string
		worktree   string
		wantConfig string
		wantKey    string
		wantReason string
	}{
		{
			name:       "claude resolves CLAUDE_CONFIG_DIR first",
			harness:    HarnessClaude,
			env:        []string{"HOME=/Users/dev", "CLAUDE_CONFIG_DIR=/profiles/claude"},
			worktree:   worktree,
			wantConfig: "/profiles/claude/.claude.json",
			wantKey:    worktree,
		},
		{
			name:       "claude falls back to HOME",
			harness:    HarnessClaude,
			env:        []string{"PATH=/bin", "HOME=/Users/dev"},
			worktree:   worktree,
			wantConfig: "/Users/dev/.claude.json",
			wantKey:    worktree,
		},
		{
			name:       "a trailing slash on the profile directory is not doubled",
			harness:    HarnessClaude,
			env:        []string{"CLAUDE_CONFIG_DIR=/profiles/claude/"},
			worktree:   worktree,
			wantConfig: "/profiles/claude/.claude.json",
			wantKey:    worktree,
		},
		{
			name:       "no profile variable resolves to a not-seeded reason",
			harness:    HarnessClaude,
			env:        []string{"PATH=/bin"},
			worktree:   worktree,
			wantReason: "neither CLAUDE_CONFIG_DIR nor HOME",
		},
		{
			name:       "a relative profile directory never seeds",
			harness:    HarnessClaude,
			env:        []string{"HOME=relative/home"},
			worktree:   worktree,
			wantReason: "not absolute",
		},
		{
			name:       "a relative worktree path never composes a key",
			harness:    HarnessClaude,
			env:        []string{"HOME=/Users/dev"},
			worktree:   "worktrees/hop-run-1",
			wantReason: "worktree path is not absolute",
		},
		{
			name:       "codex is not seeded",
			harness:    HarnessCodex,
			env:        []string{"CODEX_HOME=/profiles/codex"},
			worktree:   worktree,
			wantReason: "codex workspace trust is not seeded",
		},
		{
			name:       "opencode is not seeded",
			harness:    HarnessOpencode,
			env:        []string{"HOME=/profiles/opencode"},
			worktree:   worktree,
			wantReason: "opencode workspace trust is not seeded",
		},
		{
			name:       "an unknown harness is not seeded",
			harness:    "mystery",
			env:        []string{"HOME=/Users/dev"},
			worktree:   worktree,
			wantReason: `harness "mystery" has no workspace-trust seed`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			step := PlanTrustSeed(tc.harness, tc.env, tc.worktree)
			if tc.wantConfig == "" {
				if step.Seeds() {
					t.Fatalf("step = %+v, want not seeded", step)
				}
				if !strings.Contains(step.Reason, tc.wantReason) {
					t.Errorf("reason = %q, want it to contain %q", step.Reason, tc.wantReason)
				}
				return
			}
			if !step.Seeds() {
				t.Fatalf("step = %+v, want a seed", step)
			}
			if step.ConfigPath != tc.wantConfig || step.ProjectKey != tc.wantKey {
				t.Errorf("step = %+v, want config %q key %q", step, tc.wantConfig, tc.wantKey)
			}
		})
	}
}

// TestPlanTrustSeedLastDuplicateWins pins the duplicate-entry rule to
// SanitizeEnvironment's: the last entry of a name is the one read.
func TestPlanTrustSeedLastDuplicateWins(t *testing.T) {
	step := PlanTrustSeed(HarnessClaude,
		[]string{"CLAUDE_CONFIG_DIR=/stale", "CLAUDE_CONFIG_DIR=/current"},
		"/wt")
	if step.ConfigPath != "/current/.claude.json" {
		t.Errorf("config = %q, want the last duplicate's value resolved", step.ConfigPath)
	}
}

// fullDefaultEntry is the complete default project entry Claude Code
// 2.1.270 persists when a trust dialog is abandoned (the spike's P3
// observation): hasTrustDialogAccepted false among every other key, which
// the seed must flip without disturbing anything else.
const fullDefaultEntry = `{"allowedTools":[],"disabledMcpjsonServers":[],"enabledMcpjsonServers":[],"exampleFiles":[],"hasClaudeMdExternalIncludesApproved":false,"hasClaudeMdExternalIncludesWarningShown":false,"hasTrustDialogAccepted":false,"hasUnseenTeamArtifacts":false,"lastGracefulShutdown":false,"lastVersionBase":"2.1.270","mcpContextUris":[],"mcpServers":{}}`

func TestSeedTrustEdit(t *testing.T) {
	const key = "/private/var/worktrees/hop-run-1"
	cases := []struct {
		name        string
		config      string
		wantEdited  string
		wantChanged bool
	}{
		{
			name:        "creates the projects map when absent, preserving unknown fields",
			config:      `{"firstStartTime":"2026-01-01T00:00:00Z","unknownFutureKey":{"a":[1,2]}}`,
			wantEdited:  `{"projects":{"` + key + `":{"hasTrustDialogAccepted":true}},"firstStartTime":"2026-01-01T00:00:00Z","unknownFutureKey":{"a":[1,2]}}`,
			wantChanged: true,
		},
		{
			name:        "creates the projects map in an empty document",
			config:      `{}`,
			wantEdited:  `{"projects":{"` + key + `":{"hasTrustDialogAccepted":true}}}`,
			wantChanged: true,
		},
		{
			name:        "creates the entry when absent, other projects untouched",
			config:      `{"projects":{"/other/project":` + fullDefaultEntry + `}}`,
			wantEdited:  `{"projects":{"` + key + `":{"hasTrustDialogAccepted":true},"/other/project":` + fullDefaultEntry + `}}`,
			wantChanged: true,
		},
		{
			name:        "creates the entry in an empty projects map",
			config:      `{"projects":{}}`,
			wantEdited:  `{"projects":{"` + key + `":{"hasTrustDialogAccepted":true}}}`,
			wantChanged: true,
		},
		{
			name:        "adds the key to an existing entry, other entry keys untouched",
			config:      `{"projects":{"` + key + `":{"allowedTools":["Bash"],"mcpServers":{}}}}`,
			wantEdited:  `{"projects":{"` + key + `":{"hasTrustDialogAccepted":true,"allowedTools":["Bash"],"mcpServers":{}}}}`,
			wantChanged: true,
		},
		{
			name:        "overwrites false with true inside the full abandoned-dialog entry",
			config:      `{"projects":{"` + key + `":` + fullDefaultEntry + `}}`,
			wantEdited:  `{"projects":{"` + key + `":` + strings.Replace(fullDefaultEntry, `"hasTrustDialogAccepted":false`, `"hasTrustDialogAccepted":true`, 1) + `}}`,
			wantChanged: true,
		},
		{
			name:        "overwrites false preserving surrounding formatting",
			config:      "{\n  \"projects\": {\n    \"" + key + "\": {\n      \"hasTrustDialogAccepted\" :   false ,\n      \"allowedTools\": []\n    }\n  }\n}\n",
			wantEdited:  "{\n  \"projects\": {\n    \"" + key + "\": {\n      \"hasTrustDialogAccepted\" :   true ,\n      \"allowedTools\": []\n    }\n  }\n}\n",
			wantChanged: true,
		},
		{
			name:        "replaces a non-boolean value with true",
			config:      `{"projects":{"` + key + `":{"hasTrustDialogAccepted":{"weird":1}}}}`,
			wantEdited:  `{"projects":{"` + key + `":{"hasTrustDialogAccepted":true}}}`,
			wantChanged: true,
		},
		{
			name:        "already true changes nothing",
			config:      `{"projects":{"` + key + `":{"hasTrustDialogAccepted":true}}}`,
			wantChanged: false,
		},
		{
			name:        "a duplicate trust key edits the last occurrence",
			config:      `{"projects":{"` + key + `":{"hasTrustDialogAccepted":true,"hasTrustDialogAccepted":false}}}`,
			wantEdited:  `{"projects":{"` + key + `":{"hasTrustDialogAccepted":true,"hasTrustDialogAccepted":true}}}`,
			wantChanged: true,
		},
		{
			name:        "a duplicate projects map edits the last occurrence",
			config:      `{"projects":{"` + key + `":{"hasTrustDialogAccepted":false}},"projects":{}}`,
			wantEdited:  `{"projects":{"` + key + `":{"hasTrustDialogAccepted":false}},"projects":{"` + key + `":{"hasTrustDialogAccepted":true}}}`,
			wantChanged: true,
		},
		{
			name:        "a nested projects key inside another member is not the projects map",
			config:      `{"cache":{"projects":{"` + key + `":{"hasTrustDialogAccepted":false}}}}`,
			wantEdited:  `{"projects":{"` + key + `":{"hasTrustDialogAccepted":true}},"cache":{"projects":{"` + key + `":{"hasTrustDialogAccepted":false}}}}`,
			wantChanged: true,
		},
		{
			name:        "a prefix of the worktree path is not the entry",
			config:      `{"projects":{"/private/var/worktrees":{"hasTrustDialogAccepted":true}}}`,
			wantEdited:  `{"projects":{"` + key + `":{"hasTrustDialogAccepted":true},"/private/var/worktrees":{"hasTrustDialogAccepted":true}}}`,
			wantChanged: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			original := []byte(tc.config)
			edited, changed, err := SeedTrustEdit(original, key)
			if err != nil {
				t.Fatalf("SeedTrustEdit: %v", err)
			}
			if changed != tc.wantChanged {
				t.Fatalf("changed = %v, want %v", changed, tc.wantChanged)
			}
			if string(original) != tc.config {
				t.Errorf("the input slice was mutated")
			}
			if !tc.wantChanged {
				if edited != nil {
					t.Errorf("edited = %q, want nil for an unchanged document", edited)
				}
				return
			}
			if string(edited) != tc.wantEdited {
				t.Errorf("edited:\n%s\nwant:\n%s", edited, tc.wantEdited)
			}
			var check map[string]any
			if err := json.Unmarshal(edited, &check); err != nil {
				t.Errorf("edited document is not valid JSON: %v", err)
			}
		})
	}
}

// TestSeedTrustEditResultReadsBackTrue proves the property the seed
// exists for: whatever the input shape, Claude Code's own parse of the
// edited document reads projects[key].hasTrustDialogAccepted == true.
func TestSeedTrustEditResultReadsBackTrue(t *testing.T) {
	const key = "/private/var/worktrees/hop-run-1"
	configs := []string{
		`{}`,
		`{"projects":{}}`,
		`{"projects":{"` + key + `":{}}}`,
		`{"projects":{"` + key + `":` + fullDefaultEntry + `}}`,
		`{"projects":{"/other":` + fullDefaultEntry + `},"numStartups":7}`,
		`{"projects":{"` + key + `":{"hasTrustDialogAccepted":true,"hasTrustDialogAccepted":false}}}`,
	}
	for _, config := range configs {
		edited, changed, err := SeedTrustEdit([]byte(config), key)
		if err != nil || !changed {
			t.Fatalf("SeedTrustEdit(%q) = changed %v, %v", config, changed, err)
		}
		var doc struct {
			Projects map[string]struct {
				HasTrustDialogAccepted bool `json:"hasTrustDialogAccepted"`
			} `json:"projects"`
		}
		if err := json.Unmarshal(edited, &doc); err != nil {
			t.Fatalf("edited %q is not valid JSON: %v", edited, err)
		}
		if !doc.Projects[key].HasTrustDialogAccepted {
			t.Errorf("edited %q does not read back trust accepted", edited)
		}
	}
}

func TestSeedTrustEditUnparsable(t *testing.T) {
	const key = "/private/var/worktrees/hop-run-1"
	cases := []struct {
		name   string
		config string
	}{
		{"empty file", ""},
		{"truncated document", `{"projects":{`},
		{"not json at all", "hasTrustDialogAccepted\n"},
		{"top-level array", `[{"projects":{}}]`},
		{"top-level string", `"projects"`},
		{"projects is not an object", `{"projects":[]}`},
		{"projects is a scalar", `{"projects":true}`},
		{"the entry is not an object", `{"projects":{"` + key + `":"trusted"}}`},
		{"trailing content after the document", `{"projects":{}} {"more":1}`},
		{"a later duplicate projects value of the wrong shape", `{"projects":{},"projects":3}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			edited, changed, err := SeedTrustEdit([]byte(tc.config), key)
			if err == nil {
				t.Fatalf("SeedTrustEdit accepted an unparsable document: changed %v edited %q", changed, edited)
			}
			if edited != nil || changed {
				t.Errorf("an error must not return an edit: changed %v edited %q", changed, edited)
			}
		})
	}
}

// TestSeedTrustEditEscapedKey proves a project path needing JSON string
// escaping round-trips through the inserted member.
func TestSeedTrustEditEscapedKey(t *testing.T) {
	key := `/private/var/work"trees/hop run\1`
	edited, changed, err := SeedTrustEdit([]byte(`{}`), key)
	if err != nil || !changed {
		t.Fatalf("SeedTrustEdit = changed %v, %v", changed, err)
	}
	var doc map[string]map[string]map[string]bool
	if err := json.Unmarshal(edited, &doc); err != nil {
		t.Fatalf("edited %q is not valid JSON: %v", edited, err)
	}
	if !doc["projects"][key]["hasTrustDialogAccepted"] {
		t.Errorf("edited %q lost the escaped key", edited)
	}
}
