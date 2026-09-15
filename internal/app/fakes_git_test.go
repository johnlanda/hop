package app_test

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/johnlanda/hop/internal/app"
)

// fakeGitCommit is one commit in the fake object store.
type fakeGitCommit struct {
	tree    string
	parents []string
}

// fakeGitRepo is a stateful fake git: a ref store with the G1/G3-pinned
// compare-and-swap semantics (`update-ref <ref> <new> <old>` moves the
// ref only when <old> matches its actual current value; "" as <old> is
// create-only), a commit graph, and detached worktrees keyed by path. It
// is installed as fakeCommands.RunHook and simulates both direct git
// invocations and the `hop check-exec --op <id> -- …` spawn (running the
// inner argv; OnCheckExec is the test's stand-in for the child's
// pre-exec claim write). Merge behavior is scripted per test through
// MergeOutcome, reproducing the G2-pinned outcome matrix shapes.
type fakeGitRepo struct {
	mu  sync.Mutex
	git string
	hop string

	refs      map[string]string
	commits   map[string]fakeGitCommit
	worktrees map[string]string
	counter   int

	// MergeOutcome scripts the next merges: "merged" (default), "conflict"
	// or "no-op".
	MergeOutcome string
	// CheckExitCode scripts the inner non-git check command's exit code.
	CheckExitCode int
	// OnCheckExec, when set, runs for every simulated check-exec spawn
	// with the operation id parsed from the argv and the inner argv.
	OnCheckExec func(opID string, inner []string)
	// UpdateRefCalls records every update-ref invocation as
	// "ref new old" strings, oldest first.
	UpdateRefCalls []string
	// CommitTreeCalls counts commit-tree invocations.
	CommitTreeCalls int
	// CheckExecCalls counts simulated check-exec spawns.
	CheckExecCalls int
}

func newFakeGitRepo(git, hop string) *fakeGitRepo {
	return &fakeGitRepo{
		git: git, hop: hop,
		refs:      map[string]string{},
		commits:   map[string]fakeGitCommit{},
		worktrees: map[string]string{},
	}
}

// newCommit mints a commit with the given tree and parents.
func (g *fakeGitRepo) newCommit(tree string, parents ...string) string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.newCommitLocked(tree, parents...)
}

func (g *fakeGitRepo) newCommitLocked(tree string, parents ...string) string {
	g.counter++
	oid := fmt.Sprintf("commit-%04d", g.counter)
	g.commits[oid] = fakeGitCommit{tree: tree, parents: parents}
	return oid
}

// ref reads a ref's current value ("" when unset).
func (g *fakeGitRepo) ref(name string) string { //nolint:unparam // a general ref-store helper; every current scenario exercises the one integration ref.
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.refs[name]
}

// setRef sets a ref unconditionally (test seeding only).
func (g *fakeGitRepo) setRef(name, oid string) { //nolint:unparam // a general ref-store helper; every current scenario exercises the one integration ref.
	g.mu.Lock()
	defer g.mu.Unlock()
	g.refs[name] = oid
}

// treeOf returns a commit's tree.
func (g *fakeGitRepo) treeOf(oid string) string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.commits[oid].tree
}

// parentsOf returns a commit's parents.
func (g *fakeGitRepo) parentsOf(oid string) []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.commits[oid].parents...)
}

// Hook adapts the fake repo to fakeCommands.RunHook.
func (g *fakeGitRepo) Hook(_ context.Context, cmd app.Command) (app.CommandResult, bool, error) {
	argv := cmd.Argv
	if len(argv) >= 2 && argv[0] == g.hop && argv[1] == "check-exec" {
		return g.runCheckExec(argv), true, nil
	}
	if len(argv) >= 1 && argv[0] == g.git {
		return g.runGitArgv(argv[1:]), true, nil
	}
	return app.CommandResult{}, false, nil
}

// runCheckExec simulates the exec boundary: parse the operation id, hand
// it to the test's claim hook, then run the inner argv.
func (g *fakeGitRepo) runCheckExec(argv []string) app.CommandResult {
	g.mu.Lock()
	g.CheckExecCalls++
	g.mu.Unlock()
	opID := ""
	inner := []string(nil)
	for i := 0; i < len(argv); i++ {
		if argv[i] == "--op" && i+1 < len(argv) {
			opID = argv[i+1]
		}
		if argv[i] == "--" {
			inner = argv[i+1:]
			break
		}
	}
	if g.OnCheckExec != nil {
		g.OnCheckExec(opID, inner)
	}
	if len(inner) == 0 {
		return app.CommandResult{ExitCode: 2, Stderr: []byte("check-exec: no inner argv")}
	}
	if inner[0] == g.git {
		return g.runGitArgv(inner[1:])
	}
	return app.CommandResult{ExitCode: g.CheckExitCode}
}

// runGitArgv interprets one git invocation.
func (g *fakeGitRepo) runGitArgv(args []string) app.CommandResult {
	g.mu.Lock()
	defer g.mu.Unlock()

	dir := ""
	i := 0
	for i < len(args) {
		switch {
		case args[i] == "-C" && i+1 < len(args):
			dir = args[i+1]
			i += 2
		case args[i] == "-c" && i+1 < len(args):
			i += 2
		default:
			goto subcommand
		}
	}
subcommand:
	if i >= len(args) {
		return app.CommandResult{ExitCode: 2, Stderr: []byte("git: no subcommand")}
	}
	sub := args[i]
	rest := args[i+1:]
	switch sub {
	case "rev-parse":
		// The single-repository world: every checkout shares one common
		// directory, matching fakeCommands' default stub.
		if slices.Contains(rest, "--git-common-dir") {
			return app.CommandResult{ExitCode: 0, Stdout: []byte("/repo/.git\n")}
		}
		var revs []string
		for _, a := range rest {
			if strings.HasPrefix(a, "--") {
				continue
			}
			revs = append(revs, a)
		}
		var out []string
		for _, rev := range revs {
			var (
				resolved string
				err      error
			)
			if strings.HasSuffix(rev, "^{tree}") {
				resolved, err = g.resolveTreeLocked(rev, dir)
			} else {
				resolved, err = g.resolveLocked(rev, dir)
			}
			if err != nil {
				return app.CommandResult{ExitCode: 128, Stderr: []byte(err.Error())}
			}
			out = append(out, resolved)
		}
		return app.CommandResult{ExitCode: 0, Stdout: []byte(strings.Join(out, "\n") + "\n")}
	case "update-ref":
		if len(rest) < 2 {
			return app.CommandResult{ExitCode: 2, Stderr: []byte("update-ref: usage")}
		}
		ref, newOID := rest[0], rest[1]
		old := ""
		hasOld := false
		if len(rest) >= 3 {
			old = rest[2]
			hasOld = true
		}
		g.UpdateRefCalls = append(g.UpdateRefCalls, strings.TrimSpace(ref+" "+newOID+" "+old))
		if _, ok := g.commits[newOID]; !ok {
			return app.CommandResult{ExitCode: 128, Stderr: []byte("update-ref: new value is not a commit")}
		}
		current := g.refs[ref]
		switch {
		case hasOld && old == "":
			if current != "" {
				return app.CommandResult{ExitCode: 128, Stderr: []byte(fmt.Sprintf("cannot lock ref '%s': ref exists", ref))}
			}
		case hasOld:
			if current != old {
				return app.CommandResult{ExitCode: 128, Stderr: []byte(fmt.Sprintf("cannot lock ref '%s': is at %s but expected %s", ref, current, old))}
			}
		}
		g.refs[ref] = newOID
		return app.CommandResult{ExitCode: 0}
	case "worktree":
		if len(rest) >= 3 && rest[0] == "add" && rest[1] == "--detach" {
			path, rev := rest[2], rest[3]
			oid, err := g.resolveLocked(rev, dir)
			if err != nil {
				return app.CommandResult{ExitCode: 128, Stderr: []byte(err.Error())}
			}
			g.worktrees[path] = oid
			return app.CommandResult{ExitCode: 0}
		}
		if len(rest) >= 3 && rest[0] == "remove" {
			delete(g.worktrees, rest[len(rest)-1])
			return app.CommandResult{ExitCode: 0}
		}
		return app.CommandResult{ExitCode: 2, Stderr: []byte("worktree: usage")}
	case "merge":
		source := rest[len(rest)-1]
		sourceOID, err := g.resolveLocked(source, dir)
		if err != nil {
			return app.CommandResult{ExitCode: 128, Stderr: []byte(err.Error())}
		}
		head, ok := g.worktrees[dir]
		if !ok {
			return app.CommandResult{ExitCode: 128, Stderr: []byte("merge: not a worktree: " + dir)}
		}
		switch g.MergeOutcome {
		case "conflict":
			return app.CommandResult{ExitCode: 1, Stdout: []byte("Auto-merging x\nCONFLICT (content): Merge conflict in x\n"), Stderr: []byte("Automatic merge failed; fix conflicts and then commit the result.\n")}
		case "no-op":
			return app.CommandResult{ExitCode: 0, Stdout: []byte("Already up to date.\n")}
		default:
			merged := g.newCommitLocked(fmt.Sprintf("tree-merge-%d", g.counter+1), head, sourceOID)
			g.worktrees[dir] = merged
			return app.CommandResult{ExitCode: 0, Stdout: []byte("Merge made by the 'ort' strategy.\n")}
		}
	case "rev-list":
		head, ok := g.worktrees[dir]
		if !ok {
			return app.CommandResult{ExitCode: 128, Stderr: []byte("rev-list: not a worktree: " + dir)}
		}
		c := g.commits[head]
		return app.CommandResult{ExitCode: 0, Stdout: []byte(strings.Join(append([]string{head}, c.parents...), " ") + "\n")}
	case "merge-base":
		if len(rest) == 3 && rest[0] == "--is-ancestor" {
			ancestor, err := g.resolveLocked(rest[1], dir)
			if err != nil {
				return app.CommandResult{ExitCode: 128, Stderr: []byte(err.Error())}
			}
			descendant, err := g.resolveLocked(rest[2], dir)
			if err != nil {
				return app.CommandResult{ExitCode: 128, Stderr: []byte(err.Error())}
			}
			if g.isAncestorLocked(ancestor, descendant) {
				return app.CommandResult{ExitCode: 0}
			}
			return app.CommandResult{ExitCode: 1}
		}
		return app.CommandResult{ExitCode: 2, Stderr: []byte("merge-base: usage")}
	case "commit-tree":
		g.CommitTreeCalls++
		tree, err := g.resolveTreeLocked(rest[0], dir)
		if err != nil {
			return app.CommandResult{ExitCode: 128, Stderr: []byte(err.Error())}
		}
		var parents []string
		for j := 1; j < len(rest); j++ {
			if rest[j] == "-p" && j+1 < len(rest) {
				parent, err := g.resolveLocked(rest[j+1], dir)
				if err != nil {
					return app.CommandResult{ExitCode: 128, Stderr: []byte(err.Error())}
				}
				parents = append(parents, parent)
				j++
			}
		}
		oid := g.newCommitLocked(tree, parents...)
		return app.CommandResult{ExitCode: 0, Stdout: []byte(oid + "\n")}
	case "cat-file":
		return app.CommandResult{ExitCode: 0, Stdout: []byte("commit\n")}
	case "ls-tree":
		return app.CommandResult{ExitCode: 0, Stdout: []byte("")}
	default:
		return app.CommandResult{ExitCode: 2, Stderr: []byte("git: unhandled subcommand " + sub)}
	}
}

// resolveLocked resolves a rev to a commit oid. Callers hold g.mu.
func (g *fakeGitRepo) resolveLocked(rev, dir string) (string, error) {
	if base, ok := strings.CutSuffix(rev, "^{commit}"); ok {
		return g.resolveLocked(base, dir)
	}
	if base, ok := strings.CutSuffix(rev, "^{tree}"); ok {
		// A tree resolution requested through the commit resolver is a
		// caller bug; resolveTreeLocked owns it.
		return "", fmt.Errorf("rev %s^{tree} resolved as a commit", base)
	}
	if rev == "HEAD" {
		if oid, ok := g.worktrees[dir]; ok {
			return oid, nil
		}
		return "", fmt.Errorf("HEAD: not a worktree: %s", dir)
	}
	if oid, ok := g.refs[rev]; ok && oid != "" {
		return oid, nil
	}
	if _, ok := g.commits[rev]; ok {
		return rev, nil
	}
	return "", fmt.Errorf("unknown revision %s", rev)
}

// resolveTreeLocked resolves "<rev>^{tree}" (or a bare rev) to the
// commit's tree. Callers hold g.mu.
func (g *fakeGitRepo) resolveTreeLocked(rev, dir string) (string, error) {
	base := strings.TrimSuffix(rev, "^{tree}")
	oid, err := g.resolveLocked(base, dir)
	if err != nil {
		return "", err
	}
	return g.commits[oid].tree, nil
}

// isAncestorLocked walks descendant's parent graph looking for ancestor
// (a commit is its own ancestor, matching git). Callers hold g.mu.
func (g *fakeGitRepo) isAncestorLocked(ancestor, descendant string) bool {
	seen := map[string]bool{}
	queue := []string{descendant}
	for len(queue) > 0 {
		oid := queue[0]
		queue = queue[1:]
		if oid == ancestor {
			return true
		}
		if seen[oid] {
			continue
		}
		seen[oid] = true
		queue = append(queue, g.commits[oid].parents...)
	}
	return false
}

// registerWorktree records a checkout at path for a resolvable rev — the
// stand-in for Runtime.CreateWorktree's real materialization.
func (g *fakeGitRepo) registerWorktree(path, rev string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	oid, err := g.resolveLocked(rev, "")
	if err != nil {
		return err
	}
	g.worktrees[path] = oid
	return nil
}
