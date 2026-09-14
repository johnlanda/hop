package app_test

import (
	"context"
	"testing"

	"github.com/johnlanda/hop/internal/app"
)

// runningRun drives a run through StartRun, claims and settles its launch,
// returning the handle and the resulting detail (attempt now running).
func runningRun(t *testing.T, tc *testController) (app.RunHandle, app.RunDetail) {
	t.Helper()
	handle, detail := startedRun(t, tc)
	claimLaunch(t, tc, detail, 4242)
	tc.Runtime.InspectPaneFn = func(string) (app.PaneProcess, error) {
		return app.PaneProcess{Foreground: []app.ProcessInfo{{PID: 4242, Argv0: "/usr/bin/claude", Argv: []string{"claude", detail.AttemptID.String()}}}}, nil
	}
	if _, err := tc.Controller.CorroborateLaunch(context.Background(), handle, "/usr/bin/claude", detail.AttemptID.String()); err != nil {
		t.Fatalf("CorroborateLaunch() error = %v", err)
	}
	updated, err := tc.Store.LoadRunStatus(context.Background(), tc.onlyRunID(t))
	if err != nil {
		t.Fatalf("LoadRunStatus() error = %v", err)
	}
	return handle, updated
}

func defaultSubmitRequest(detail app.RunDetail) app.SubmitResultRequest { //nolint:gocritic // hugeParam: RunDetail is a per-call test fixture value.
	return app.SubmitResultRequest{
		RunID: detail.RunID.String(), TaskID: detail.TaskID.String(), AttemptID: detail.AttemptID.String(),
		IncarnationID: detail.Binding.IncarnationID.String(),
		CommitOID:     "cccccccccccccccccccccccccccccccccccccccc", Summary: "did the thing",
	}
}

func TestSubmitResult(t *testing.T) {
	t.Run("accepted: first submission against a running attempt", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		_, detail := runningRun(t, tc)
		result, err := tc.Controller.SubmitResult(context.Background(), defaultSubmitRequest(detail))
		if err != nil {
			t.Fatalf("SubmitResult() error = %v", err)
		}
		if result.Kind != string(app.SubmissionAccepted) {
			t.Fatalf("Kind = %s, want %s", result.Kind, app.SubmissionAccepted)
		}
		if result.ResultID == "" {
			t.Fatalf("accepted submission carries no result id")
		}
	})

	t.Run("duplicate: identical resubmission is idempotent even after later states", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		_, detail := runningRun(t, tc)
		req := defaultSubmitRequest(detail)
		first, err := tc.Controller.SubmitResult(context.Background(), req)
		if err != nil {
			t.Fatalf("first SubmitResult() error = %v", err)
		}
		second, err := tc.Controller.SubmitResult(context.Background(), req)
		if err != nil {
			t.Fatalf("second SubmitResult() error = %v", err)
		}
		if second.Kind != string(app.SubmissionDuplicate) {
			t.Fatalf("Kind = %s, want %s", second.Kind, app.SubmissionDuplicate)
		}
		if second.ResultID != first.ResultID {
			t.Fatalf("duplicate result id = %s, want %s", second.ResultID, first.ResultID)
		}
	})

	t.Run("conflicting: different content for the same attempt is rejected without disturbing the accepted result", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		_, detail := runningRun(t, tc)
		req := defaultSubmitRequest(detail)
		if _, err := tc.Controller.SubmitResult(context.Background(), req); err != nil {
			t.Fatalf("first SubmitResult() error = %v", err)
		}
		req.Summary = "a different summary"
		result, err := tc.Controller.SubmitResult(context.Background(), req)
		if err != nil {
			t.Fatalf("SubmitResult() error = %v", err)
		}
		if result.Kind != string(app.SubmissionConflicting) {
			t.Fatalf("Kind = %s, want %s", result.Kind, app.SubmissionConflicting)
		}
		if len(tc.Store.Results) != 1 {
			t.Fatalf("a conflicting submission mutated the accepted result set: %d results", len(tc.Store.Results))
		}
	})

	t.Run("transient: launching with an unsettled claim, before running", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		handle, detail := startedRun(t, tc)
		claimLaunch(t, tc, detail, 4242)
		_ = handle

		req := app.SubmitResultRequest{
			RunID: detail.RunID.String(), TaskID: detail.TaskID.String(), AttemptID: detail.AttemptID.String(),
			IncarnationID: detail.Binding.IncarnationID.String(),
			CommitOID:     "cccccccccccccccccccccccccccccccccccccccc", Summary: "fast submission",
		}
		result, err := tc.Controller.SubmitResult(context.Background(), req)
		if err != nil {
			t.Fatalf("SubmitResult() error = %v", err)
		}
		if result.Kind != string(app.SubmissionTransient) {
			t.Fatalf("Kind = %s, want %s", result.Kind, app.SubmissionTransient)
		}
	})

	t.Run("early acceptance: settling the claim after a transient submission still lands the atomic handoff", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		handle, detail := startedRun(t, tc)
		claimLaunch(t, tc, detail, 4242)

		req := defaultSubmitRequest(detail)
		if result, err := tc.Controller.SubmitResult(context.Background(), req); err != nil || result.Kind != string(app.SubmissionTransient) {
			t.Fatalf("expected an initial transient submission, got %+v, err %v", result, err)
		}

		tc.Runtime.InspectPaneFn = func(string) (app.PaneProcess, error) {
			return app.PaneProcess{Foreground: []app.ProcessInfo{{PID: 4242, Argv0: "/usr/bin/claude", Argv: []string{"claude", detail.AttemptID.String()}}}}, nil
		}
		if _, err := tc.Controller.CorroborateLaunch(context.Background(), handle, "/usr/bin/claude", detail.AttemptID.String()); err != nil {
			t.Fatalf("CorroborateLaunch() error = %v", err)
		}

		result, err := tc.Controller.SubmitResult(context.Background(), req)
		if err != nil {
			t.Fatalf("SubmitResult() error = %v", err)
		}
		if result.Kind != string(app.SubmissionAccepted) {
			t.Fatalf("Kind = %s, want %s (retry after settlement should accept)", result.Kind, app.SubmissionAccepted)
		}
	})

	t.Run("malformed: a bad run id is recorded with the claimed value, never parsed", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		result, err := tc.Controller.SubmitResult(context.Background(), app.SubmitResultRequest{
			RunID: "not-a-uuid", TaskID: "22222222-2222-4222-8222-222222222222",
			AttemptID: "33333333-3333-4333-8333-333333333333", IncarnationID: "44444444-4444-4444-8444-444444444444",
			CommitOID: "cccccccccccccccccccccccccccccccccccccccc", Summary: "x",
		})
		if err != nil {
			t.Fatalf("SubmitResult() error = %v", err)
		}
		if result.Kind != string(app.SubmissionMalformed) {
			t.Fatalf("Kind = %s, want %s", result.Kind, app.SubmissionMalformed)
		}
	})

	t.Run("malformed: commit is not a full 40-hex object id", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		_, detail := runningRun(t, tc)
		req := defaultSubmitRequest(detail)
		req.CommitOID = "not-hex"
		result, err := tc.Controller.SubmitResult(context.Background(), req)
		if err != nil {
			t.Fatalf("SubmitResult() error = %v", err)
		}
		if result.Kind != string(app.SubmissionMalformed) {
			t.Fatalf("Kind = %s, want %s", result.Kind, app.SubmissionMalformed)
		}
	})
}
