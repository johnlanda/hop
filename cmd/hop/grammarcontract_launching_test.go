package main

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/johnlanda/hop/internal/app"
	"github.com/johnlanda/hop/internal/testsupport/hopfixtures"
)

// launchingManagerPID is the pid the launching manager's claim records;
// the settlement names the same pid as the corroborated occupant.
const launchingManagerPID = 5151

// launchingFeature is a feature run started the way `hop run --workflow
// feature` starts one — feature InitializeRun, then the manager's pane
// placed and its launcher's claim recorded — with the claim still
// exec_pending, so the run is still launching: the window the controller's
// next corroboration pass closes.
type launchingFeature struct {
	StateRoot string
	base      hopfixtures.FeatureBase
	store     hopfixtures.Store
	lease     app.Lease
}

func newLaunchingFeature(t *testing.T, seed int) *launchingFeature {
	t.Helper()
	stateRoot := freshStateDir(t)
	store := openFixtureStore(t, stateRoot)
	ctx := context.Background()
	now := time.Now().UTC()

	workflow := featureWorkflowSnapshot("", defaultMessageWait)
	workflow.IntegrationBranch = app.IntegrationBranchName(1)
	base, lease, err := hopfixtures.InitializeFeature(ctx, store, stateRoot, realDir(t), workflow, seed, now)
	if err != nil {
		t.Fatalf("initialize feature run: %v", err)
	}
	if err := hopfixtures.LaunchManager(ctx, store, lease, base, now); err != nil {
		t.Fatalf("launch manager: %v", err)
	}
	if err := hopfixtures.SeedLaunchClaim(ctx, store, base.RunID, base.ManagerID, "", base.ManagerIncarnation, launchingManagerPID, now); err != nil {
		t.Fatalf("claim manager launch: %v", err)
	}
	return &launchingFeature{StateRoot: stateRoot, base: base, store: store, lease: lease}
}

// settle applies the controller's corroboration of the manager's claim.
func (f *launchingFeature) settle(t *testing.T) {
	t.Helper()
	if err := hopfixtures.SettleManagerLaunch(context.Background(), f.store, f.lease, f.base, launchingManagerPID, time.Now().UTC()); err != nil {
		t.Fatalf("settle manager launch: %v", err)
	}
}

func (f *launchingFeature) env() map[string]string {
	return map[string]string{
		"HOP_STATE_DIR":      f.StateRoot,
		"HOP_RUN_ID":         f.base.RunID,
		"HOP_SESSION_ID":     f.base.ManagerID,
		"HOP_INCARNATION_ID": f.base.ManagerIncarnation,
	}
}

// summary reads, through a read-only raw query, the run and manager
// states, the manager's claim state, the run's task and message counts,
// and the outcomes its workflow and message receipts recorded, in order.
func (f *launchingFeature) summary(t *testing.T) string {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(f.StateRoot, "hop.db")+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("open raw db for a read: %v", err)
	}
	defer func() {
		if closeErr := db.Close(); closeErr != nil {
			t.Errorf("close raw db after a read: %v", closeErr)
		}
	}()
	var summary string
	err = db.QueryRowContext(context.Background(), `
		SELECT 'run=' || (SELECT state FROM runs WHERE id = ?1)
		    || ' manager=' || (SELECT state FROM sessions WHERE id = ?2)
		    || ' claim=' || (SELECT state FROM launch_claims WHERE incarnation_id = ?3)
		    || ' tasks=' || (SELECT COUNT(*) FROM tasks WHERE run_id = ?1)
		    || ' messages=' || (SELECT COUNT(*) FROM messages WHERE run_id = ?1)
		    || ' plan=' || COALESCE((SELECT group_concat(outcome, ',') FROM (SELECT outcome FROM workflow_receipts WHERE run_id = ?1 ORDER BY at, rowid)), '')
		    || ' msg=' || COALESCE((SELECT group_concat(outcome, ',') FROM (SELECT outcome FROM message_receipts WHERE run_id = ?1 ORDER BY at, rowid)), '')`,
		f.base.RunID, f.base.ManagerID, f.base.ManagerIncarnation).Scan(&summary)
	if err != nil {
		t.Fatalf("read launching fixture rows: %v", err)
	}
	return summary
}

// TestGrammarContractManagerVerbsWhileLaunching is LAUNCH-1's real-binary
// proof. A manager that plans the moment it starts runs `hop task create`
// against a feature run whose manager claim is still exec_pending: the
// built binary prints the retryable run-not-running line alone, the
// value-free detail on stderr, and exits 1, with nothing created and a
// transient receipt only. The same holds for `hop plan close` and
// `hop msg send`. Once the controller's corroboration moves the run to
// running, the SAME invocations (same --request-id) succeed with their
// grammar lines, and a further retry is a duplicate.
func TestGrammarContractManagerVerbsWhileLaunching(t *testing.T) {
	f := newLaunchingFeature(t, 9100)
	dir := freshStateDir(t)
	instructions := filepath.Join(dir, "instructions.md")
	if err := os.WriteFile(instructions, []byte("do the thing"), 0o600); err != nil {
		t.Fatalf("write instructions fixture: %v", err)
	}
	createArgs := []string{"task", "create", "--title", "first task", "--file", instructions, "--request-id", testUUID(9101)}
	closeArgs := []string{"plan", "close", "--request-id", testUUID(9102)}
	sendArgs := []string{"msg", "send", "--to", "human", "--kind", "question", "--body", "which way?", "--request-id", testUUID(9103)}

	if got, want := f.summary(t), "run=launching manager=launching claim=exec_pending tasks=0 messages=0 plan= msg="; got != want {
		t.Fatalf("fixture = %q, want %q", got, want)
	}

	for _, tc := range []struct {
		verb string
		args []string
	}{
		{"hop task create", createArgs},
		{"hop plan close", closeArgs},
		{"hop msg send", sendArgs},
	} {
		result := execHop(t, f.env(), dir, tc.args...)
		if result.ExitCode != exitFailure {
			t.Fatalf("%s while launching: exit = %d, want %d; stdout=%q stderr=%q", tc.verb, result.ExitCode, exitFailure, result.Stdout, result.Stderr)
		}
		if result.Stdout != app.GrammarTransientRunNotRunningLine+"\n" {
			t.Fatalf("%s while launching: stdout = %q, want only %q", tc.verb, result.Stdout, app.GrammarTransientRunNotRunningLine)
		}
		if want := tc.verb + ": run is launching, not yet running\n"; result.Stderr != want {
			t.Fatalf("%s while launching: stderr = %q, want %q", tc.verb, result.Stderr, want)
		}
	}
	if got, want := f.summary(t), "run=launching manager=launching claim=exec_pending tasks=0 messages=0 plan=transient,transient msg=transient"; got != want {
		t.Fatalf("after the transient round = %q, want %q", got, want)
	}

	f.settle(t)

	created := execHop(t, f.env(), dir, createArgs...)
	fields := strings.Fields(created.FirstStdoutLine())
	if created.ExitCode != exitOK || len(fields) != 4 || created.Stdout != app.GrammarTaskCreatedLine(fields[1], 1)+"\n" {
		t.Fatalf("hop task create once running: exit = %d stdout=%q stderr=%q; want %q", created.ExitCode, created.Stdout, created.Stderr, app.GrammarTaskCreatedLine("<uuid>", 1))
	}
	taskID := fields[1]
	closed := execHop(t, f.env(), dir, closeArgs...)
	if closed.ExitCode != exitOK || closed.Stdout != app.GrammarPlanClosedLine+"\n" {
		t.Fatalf("hop plan close once running: exit = %d stdout=%q stderr=%q", closed.ExitCode, closed.Stdout, closed.Stderr)
	}
	sent := execHop(t, f.env(), dir, sendArgs...)
	sentFields := strings.Fields(sent.FirstStdoutLine())
	if sent.ExitCode != exitOK || len(sentFields) != 2 || sent.Stdout != app.GrammarSentLine(sentFields[1])+"\n" {
		t.Fatalf("hop msg send once running: exit = %d stdout=%q stderr=%q", sent.ExitCode, sent.Stdout, sent.Stderr)
	}

	for _, tc := range []struct {
		args []string
		want string
	}{
		{createArgs, app.GrammarTaskCreateDuplicateLine(taskID, 1)},
		{closeArgs, app.GrammarPlanCloseDuplicateLine},
		{sendArgs, app.GrammarSendDuplicateLine(sentFields[1])},
	} {
		again := execHop(t, f.env(), dir, tc.args...)
		if again.ExitCode != exitOK || again.Stdout != tc.want+"\n" || again.Stderr != "" {
			t.Fatalf("hop %s retried: exit = %d stdout=%q stderr=%q; want %q", strings.Join(tc.args[:2], " "), again.ExitCode, again.Stdout, again.Stderr, tc.want)
		}
	}
	// The duplicates replay the receipts and record their own duplicate
	// rows; each key holds exactly one acceptance beside its transient row.
	if got, want := f.summary(t), "run=running manager=active claim=execed tasks=1 messages=1 plan=transient,transient,accepted,accepted,duplicate,duplicate msg=transient,accepted,duplicate"; got != want {
		t.Fatalf("after settlement = %q, want %q", got, want)
	}
}
