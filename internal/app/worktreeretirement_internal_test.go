package app

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

// TestWorktreeRetirementArgvShapes pins both frozen retirement argvs byte for
// byte: the detection compares two object ids, and the removal carries
// the untracked-files override before the subcommand and no force option
// anywhere.
func TestWorktreeRetirementArgvShapes(t *testing.T) {
	check := worktreeRetirementCheckArgv("/usr/bin/git", "/repo", "1111111111111111111111111111111111111111", "2222222222222222222222222222222222222222")
	wantCheck := []string{"/usr/bin/git", "-C", "/repo", "merge-base", "--is-ancestor", "1111111111111111111111111111111111111111", "2222222222222222222222222222222222222222"}
	if !slices.Equal(check, wantCheck) {
		t.Errorf("retirement check argv = %q, want %q", check, wantCheck)
	}

	remove := worktreeRetireArgv("/usr/bin/git", "/repo", "/worktrees/repo/hop-r3-t1a1")
	wantRemove := []string{"/usr/bin/git", "-C", "/repo", "-c", "status.showUntrackedFiles=all", "worktree", "remove", "/worktrees/repo/hop-r3-t1a1"}
	if !slices.Equal(remove, wantRemove) {
		t.Errorf("worktree retire argv = %q, want %q", remove, wantRemove)
	}
	for _, arg := range remove {
		if arg == "-f" || strings.HasPrefix(arg, "--force") {
			t.Errorf("worktree retire argv carries a force option %q", arg)
		}
	}
	// A path that looks like an option stays the positional path: git
	// receives it after the subcommand, and the argv is never re-parsed.
	if got := worktreeRetireArgv("/usr/bin/git", "/repo", "--force"); got[len(got)-1] != "--force" || len(got) != len(wantRemove) {
		t.Errorf("an option-shaped path changed the argv shape: %q", got)
	}
}

// TestWorktreeRetirementExecIntentKeys pins the JSON members the store reads for
// both retirement kinds, and that a persisted payload decodes back
// through decodeOperationPayload's generic path.
func TestWorktreeRetirementExecIntentKeys(t *testing.T) {
	intent := worktreeRetirementExecIntent{Argv: worktreeRetireArgv("/usr/bin/git", "/repo", "/wt"), Cwd: "/repo"}
	raw, err := json.Marshal(intent)
	if err != nil {
		t.Fatal(err)
	}
	var generic map[string]any
	if err := json.Unmarshal(raw, &generic); err != nil {
		t.Fatal(err)
	}
	keys := make([]string, 0, len(generic))
	for k := range generic {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	if !slices.Equal(keys, []string{"argv", "cwd"}) {
		t.Errorf("intent keys = %v, want [argv cwd]", keys)
	}
	decoded, ok := decodeOperationPayload[worktreeRetirementExecIntent](generic)
	if !ok || !slices.Equal(decoded.Argv, intent.Argv) || decoded.Cwd != intent.Cwd {
		t.Errorf("decoded intent = %+v (%v), want %+v", decoded, ok, intent)
	}
}
