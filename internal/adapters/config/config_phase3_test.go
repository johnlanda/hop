package config_test

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/johnlanda/hop/internal/adapters/config"
	"github.com/johnlanda/hop/internal/app"
)

// checkHeader is the minimal valid Phase 2 prefix every Phase 3 case
// builds on.
const checkHeader = "[check]\ncommand = [\"sh\", \"check.sh\"]\n"

// featureRoles is a complete [roles] block with relative instruction
// paths.
const featureRoles = "[roles.manager]\n" +
	"instructions = \"roles/orchestrator.md\"\n" +
	"[roles.implementer]\n" +
	"instructions = \"roles/implementer.md\"\n" +
	"[roles.reviewer]\n" +
	"instructions = \"roles/reviewer.md\"\n"

// assertWorkflowPolicyEqual compares the Phase 3 policy fields, the
// counterpart of config_test.go's Phase 2 assertPolicyEqual.
func assertWorkflowPolicyEqual(t *testing.T, got, want *app.RunPolicy) {
	t.Helper()
	if got.WorkflowMode != want.WorkflowMode {
		t.Errorf("WorkflowMode = %q, want %q", got.WorkflowMode, want.WorkflowMode)
	}
	if got.MaxWorkers != want.MaxWorkers {
		t.Errorf("MaxWorkers = %d, want %d", got.MaxWorkers, want.MaxWorkers)
	}
	if got.RetryLimit != want.RetryLimit {
		t.Errorf("RetryLimit = %d, want %d", got.RetryLimit, want.RetryLimit)
	}
	if got.ManagerRole != want.ManagerRole {
		t.Errorf("ManagerRole = %q, want %q", got.ManagerRole, want.ManagerRole)
	}
	if got.ImplementerRole != want.ImplementerRole {
		t.Errorf("ImplementerRole = %q, want %q", got.ImplementerRole, want.ImplementerRole)
	}
	if got.ReviewerRole != want.ReviewerRole {
		t.Errorf("ReviewerRole = %q, want %q", got.ReviewerRole, want.ReviewerRole)
	}
	if got.ReviewerHarness != want.ReviewerHarness {
		t.Errorf("ReviewerHarness = %q, want %q", got.ReviewerHarness, want.ReviewerHarness)
	}
	if got.MessageAttention != want.MessageAttention {
		t.Errorf("MessageAttention = %v, want %v", got.MessageAttention, want.MessageAttention)
	}
	if got.MessageWait != want.MessageWait {
		t.Errorf("MessageWait = %v, want %v", got.MessageWait, want.MessageWait)
	}
}

func TestLoadParsesWorkflowPolicies(t *testing.T) {
	rolesDir := func(root string) string { return filepath.Join(root, ".herdr-orchestrator", "roles") }
	cases := []struct {
		name    string
		content string
		want    func(root string) app.RunPolicy
	}{
		{
			name:    "phase 2 file leaves every workflow field zero",
			content: checkHeader,
			want:    func(string) app.RunPolicy { return app.RunPolicy{} },
		},
		{
			name:    "explicit solo mode carries the spelling and nothing else",
			content: checkHeader + "[workflow]\nmode = \"solo\"\n",
			want:    func(string) app.RunPolicy { return app.RunPolicy{WorkflowMode: "solo"} },
		},
		{
			name: "minimal feature policy applies every workflow default",
			content: checkHeader +
				"[worker]\nharness = \"codex\"\n" +
				"[workflow]\nmode = \"feature\"\n" +
				featureRoles,
			want: func(root string) app.RunPolicy {
				return app.RunPolicy{
					WorkflowMode:     "feature",
					MaxWorkers:       2,
					RetryLimit:       3,
					ManagerRole:      filepath.Join(rolesDir(root), "orchestrator.md"),
					ImplementerRole:  filepath.Join(rolesDir(root), "implementer.md"),
					ReviewerRole:     filepath.Join(rolesDir(root), "reviewer.md"),
					ReviewerHarness:  app.HarnessCodex,
					MessageAttention: 120 * time.Second,
					MessageWait:      50 * time.Second,
				}
			},
		},
		{
			name: "complete feature policy validates every explicit value",
			content: checkHeader +
				"[workflow]\nmode = \"feature\"\n" +
				"[workers]\nmax = 4\n" +
				"[retry]\nmax_attempts = 1\n" +
				"[messages]\nattention_after = \"90s\"\nwait_timeout = \"2m\"\n" +
				featureRoles +
				"harness = \"claude\"\n", // continues [roles.reviewer]
			want: func(root string) app.RunPolicy {
				return app.RunPolicy{
					WorkflowMode:     "feature",
					MaxWorkers:       4,
					RetryLimit:       1,
					ManagerRole:      filepath.Join(rolesDir(root), "orchestrator.md"),
					ImplementerRole:  filepath.Join(rolesDir(root), "implementer.md"),
					ReviewerRole:     filepath.Join(rolesDir(root), "reviewer.md"),
					ReviewerHarness:  app.HarnessClaude,
					MessageAttention: 90 * time.Second,
					MessageWait:      2 * time.Minute,
				}
			},
		},
		{
			name: "absolute role path passes through cleaned",
			content: checkHeader +
				"[roles.manager]\ninstructions = \"/opt/roles//orchestrator.md\"\n",
			want: func(string) app.RunPolicy {
				return app.RunPolicy{ManagerRole: "/opt/roles/orchestrator.md"}
			},
		},
		{
			name: "feature sections without feature mode are validated and carried, never defaulted",
			content: checkHeader +
				"[workers]\nmax = 4\n" +
				"[messages]\nwait_timeout = \"30s\"\n" +
				featureRoles,
			want: func(root string) app.RunPolicy {
				return app.RunPolicy{
					MaxWorkers:      4,
					ManagerRole:     filepath.Join(rolesDir(root), "orchestrator.md"),
					ImplementerRole: filepath.Join(rolesDir(root), "implementer.md"),
					ReviewerRole:    filepath.Join(rolesDir(root), "reviewer.md"),
					MessageWait:     30 * time.Second,
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := writePolicy(t, tc.content)

			policy, err := config.Source{}.Load(t.Context(), root)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}

			want := tc.want(root)
			assertWorkflowPolicyEqual(t, &policy, &want)
			for _, role := range []string{policy.ManagerRole, policy.ImplementerRole, policy.ReviewerRole} {
				if role != "" && !filepath.IsAbs(role) {
					t.Errorf("role path %q is not absolute", role)
				}
			}
		})
	}
}

func TestLoadRejectsInvalidWorkflowPolicies(t *testing.T) {
	// secretMarker stands in for a secret mistakenly pasted into the policy
	// file; no rejection may ever echo it back through a rendered error.
	const secretMarker = "DUMMY-SECRET-MARKER"
	cases := []struct {
		name        string
		content     string
		wantMessage string
	}{
		{
			name:        "unsupported workflow mode",
			content:     checkHeader + "[workflow]\nmode = \"" + secretMarker + "\"\n",
			wantMessage: "workflow.mode is not a supported mode",
		},
		{
			name:        "explicitly empty workflow mode",
			content:     checkHeader + "[workflow]\nmode = \"\"\n",
			wantMessage: "workflow.mode is not a supported mode",
		},
		{
			name:        "zero workers bound",
			content:     checkHeader + "[workers]\nmax = 0\n",
			wantMessage: "workers.max must be at least 1",
		},
		{
			name:        "negative workers bound",
			content:     checkHeader + "[workers]\nmax = -2\n",
			wantMessage: "workers.max must be at least 1",
		},
		{
			name:        "zero retry limit",
			content:     checkHeader + "[retry]\nmax_attempts = 0\n",
			wantMessage: "retry.max_attempts must be at least 1",
		},
		{
			name:        "unparsable attention duration",
			content:     checkHeader + "[messages]\nattention_after = \"" + secretMarker + "\"\n",
			wantMessage: "messages.attention_after is not a Go duration string",
		},
		{
			name:        "explicitly empty attention duration",
			content:     checkHeader + "[messages]\nattention_after = \"\"\n",
			wantMessage: "messages.attention_after is not a Go duration string",
		},
		{
			name:        "non-positive wait duration",
			content:     checkHeader + "[messages]\nwait_timeout = \"0s\"\n",
			wantMessage: "messages.wait_timeout must be a positive duration",
		},
		{
			name:        "role table without instructions",
			content:     checkHeader + "[roles.manager]\n",
			wantMessage: "roles.manager.instructions is required",
		},
		{
			name:        "role table with empty instructions",
			content:     checkHeader + "[roles.implementer]\ninstructions = \"\"\n",
			wantMessage: "roles.implementer.instructions is required",
		},
		{
			name:        "role instructions with an escaped control byte",
			content:     checkHeader + "[roles.reviewer]\ninstructions = \"roles/\\u0007" + secretMarker + ".md\"\n",
			wantMessage: "roles.reviewer.instructions contains a control byte",
		},
		{
			name:        "unsupported reviewer harness",
			content:     checkHeader + "[roles.reviewer]\ninstructions = \"roles/reviewer.md\"\nharness = \"" + secretMarker + "\"\n",
			wantMessage: "roles.reviewer.harness is not a supported harness",
		},
		{
			name:        "explicitly empty reviewer harness",
			content:     checkHeader + "[roles.reviewer]\ninstructions = \"roles/reviewer.md\"\nharness = \"\"\n",
			wantMessage: "roles.reviewer.harness is not a supported harness",
		},
		{
			name: "feature mode requires the manager role",
			content: checkHeader + "[workflow]\nmode = \"feature\"\n" +
				"[roles.implementer]\ninstructions = \"roles/implementer.md\"\n" +
				"[roles.reviewer]\ninstructions = \"roles/reviewer.md\"\n",
			wantMessage: "roles.manager.instructions is required in feature mode",
		},
		{
			name: "feature mode requires the reviewer role",
			content: checkHeader + "[workflow]\nmode = \"feature\"\n" +
				"[roles.manager]\ninstructions = \"roles/orchestrator.md\"\n" +
				"[roles.implementer]\ninstructions = \"roles/implementer.md\"\n",
			wantMessage: "roles.reviewer.instructions is required in feature mode",
		},
		{
			name:        "feature mode with no roles at all",
			content:     checkHeader + "[workflow]\nmode = \"feature\"\n",
			wantMessage: "roles.manager.instructions is required in feature mode",
		},
		{
			name:        "unknown key inside a workflow table is named",
			content:     checkHeader + "[workflow]\nmade = \"feature\"\n",
			wantMessage: "unknown key workflow.made",
		},
		{
			name:        "unknown key inside a role table is named",
			content:     checkHeader + "[roles.manager]\ninstrutions = \"roles/orchestrator.md\"\n",
			wantMessage: "unknown key roles.manager.instrutions",
		},
		{
			name:        "wrongly typed workers bound names the expected shape",
			content:     checkHeader + "[workers]\nmax = \"" + secretMarker + "\"\n",
			wantMessage: "key workers.max must be an integer",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := writePolicy(t, tc.content)

			_, err := config.Source{}.Load(t.Context(), root)

			if err == nil {
				t.Fatalf("Load accepted an invalid policy")
			}
			if !strings.Contains(err.Error(), tc.wantMessage) {
				t.Errorf("error %q does not contain %q", err, tc.wantMessage)
			}
			if strings.Contains(err.Error(), secretMarker) {
				t.Errorf("error %q echoes the supplied value", err)
			}
		})
	}
}

// TestWorkflowDefaultsAndRequiredsAreSharedHelpers pins that the adapter's
// feature-mode behavior IS app.ApplyWorkflowDefaults +
// app.ValidateFeaturePolicy — the same helpers hop run's --workflow
// feature override path calls (slice 6) — by driving the helpers directly
// over the carried-not-defaulted solo policy shape the adapter produces.
func TestWorkflowDefaultsAndRequiredsAreSharedHelpers(t *testing.T) {
	root := writePolicy(t, checkHeader+"[workers]\nmax = 4\n"+featureRoles)
	policy, err := config.Source{}.Load(t.Context(), root)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	policy.Harness = app.HarnessClaude

	// The override path: mode flipped to feature after load.
	policy.WorkflowMode = app.WorkflowModeFeature
	app.ApplyWorkflowDefaults(&policy)
	if err := app.ValidateFeaturePolicy(&policy); err != nil {
		t.Fatalf("ValidateFeaturePolicy: %v", err)
	}
	if policy.MaxWorkers != 4 {
		t.Errorf("MaxWorkers = %d, want the explicit 4 kept over the default", policy.MaxWorkers)
	}
	if policy.RetryLimit != 3 || policy.MessageAttention != 120*time.Second || policy.MessageWait != 50*time.Second {
		t.Errorf("defaults not applied: retry %d attention %v wait %v", policy.RetryLimit, policy.MessageAttention, policy.MessageWait)
	}
	if policy.ReviewerHarness != app.HarnessClaude {
		t.Errorf("ReviewerHarness = %q, want the worker harness inherited", policy.ReviewerHarness)
	}

	// The same shape without roles fails the shared requireds.
	bare := app.RunPolicy{WorkflowMode: app.WorkflowModeFeature, Harness: app.HarnessClaude}
	app.ApplyWorkflowDefaults(&bare)
	if err := app.ValidateFeaturePolicy(&bare); err == nil {
		t.Fatalf("ValidateFeaturePolicy accepted a roleless feature policy")
	}
}
