package app_test

import (
	"testing"

	"github.com/johnlanda/hop/internal/app"
	"github.com/johnlanda/hop/internal/domain/identity"
	"github.com/johnlanda/hop/internal/domain/run"
)

// TestFakeSubmitResultEligibilityBeforeMailbox is the fake half of the
// sqlite adapter's TestSubmitResultEligibilityBeforeMailbox, vector for
// vector: with a message queued to the task, a result from a caller that
// can never be accepted is stale, one from an attempt still launching
// with an unsettled claim is attempt-not-running, and only an otherwise
// eligible caller is told to drain — with no result recorded.
func TestFakeSubmitResultEligibilityBeforeMailbox(t *testing.T) {
	supersede := func(f *mailboxFixture) {
		history := f.tc.Store.Bindings[f.w.SessionID]
		history[len(history)-1].Superseded = true
	}
	cases := []struct {
		name       string
		arrange    func(t *testing.T, f *mailboxFixture)
		wantKind   app.SubmissionOutcomeKind
		wantReason app.TransientReason
	}{
		{
			name:     "a superseded binding with no successor",
			arrange:  func(_ *testing.T, f *mailboxFixture) { supersede(f) },
			wantKind: app.SubmissionStale,
		},
		{
			name: "an old incarnation whose session a relaunch rebound",
			arrange: func(t *testing.T, f *mailboxFixture) {
				supersede(f)
				successor, err := identity.ParseIncarnationID(f.tc.IDs.NewID())
				if err != nil {
					t.Fatalf("parse incarnation id: %v", err)
				}
				f.tc.Store.Bindings[f.w.SessionID] = append(f.tc.Store.Bindings[f.w.SessionID],
					run.NewRuntimeBinding(f.w.SessionID, successor, "", fakeServerToken(1), "ws", "tab", "pane-successor", "label-successor", run.LaunchResume, f.tc.Clock.Now()))
			},
			wantKind: app.SubmissionStale,
		},
		{
			name: "a run with a stop request",
			arrange: func(_ *testing.T, f *mailboxFixture) {
				row := f.tc.Store.Runs[f.fr.RunID]
				row.value = row.value.RequestStop(f.tc.Clock.Now())
				row.revision++
			},
			wantKind: app.SubmissionStale,
		},
		{
			name: "an interrupted attempt whose session is still bound",
			arrange: func(_ *testing.T, f *mailboxFixture) {
				f.tc.Store.Attempts[f.w.AttemptID].value.State = run.AttemptInterrupted
			},
			wantKind: app.SubmissionStale,
		},
		{
			name: "a launching attempt with an unsettled claim",
			arrange: func(_ *testing.T, f *mailboxFixture) {
				f.tc.Store.Attempts[f.w.AttemptID].value.State = run.AttemptLaunching
				claim := f.tc.Store.LaunchClaims[f.w.IncarnationID]
				claim.State = app.LaunchClaimExecPending
				f.tc.Store.LaunchClaims[f.w.IncarnationID] = claim
			},
			wantKind: app.SubmissionTransient, wantReason: app.TransientAttemptNotRunning,
		},
		{
			name:     "a running attempt, the only caller told to drain",
			arrange:  func(*testing.T, *mailboxFixture) {},
			wantKind: app.SubmissionTransient, wantReason: app.TransientUndeliveredMessages,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newMailboxFixture(t)
			if outcome := f.send(t, ""); outcome.Kind != app.MessageAccepted {
				t.Fatalf("queue info = %+v, want accepted", outcome)
			}
			tc.arrange(t, f)
			for _, round := range []string{"first", "retried"} {
				outcome := f.submit(t)
				if outcome.Kind != tc.wantKind || outcome.Transient != tc.wantReason {
					t.Fatalf("%s SubmitResult() = %+v, want %s/%q", round, outcome, tc.wantKind, tc.wantReason)
				}
				if tc.wantReason == app.TransientUndeliveredMessages && outcome.Detail != "transient: undelivered messages; drain with hop msg next, ack, then resubmit" {
					t.Fatalf("%s Detail = %q, want the drain detail unchanged", round, outcome.Detail)
				}
			}
			if _, recorded := f.tc.Store.Results[f.w.AttemptID]; recorded {
				t.Fatal("a refused submission recorded a result")
			}
		})
	}
}
