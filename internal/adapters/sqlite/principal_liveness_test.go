package sqlite_test

import (
	"slices"
	"testing"

	"github.com/johnlanda/hop/internal/app"
	"github.com/johnlanda/hop/internal/domain/identity"
	"github.com/johnlanda/hop/internal/domain/run"
)

// endSession drives one session to a terminal state through the domain's
// own transitions, leaving its binding row untouched. Nothing supersedes a
// binding on the way to lost or terminated, so a session that has ended
// keeps the current, unsuperseded binding it was placed under: only the
// session's own state tells a caller rule that the principal is gone.
func endSession(t *testing.T, f *featureFixture, sessionID identity.SessionID, state run.SessionState) {
	t.Helper()
	f.inUOW(t, func(uow app.UnitOfWork) {
		v, rev, err := uow.Sessions().Get(t.Context(), sessionID)
		if err != nil {
			t.Fatalf("get session %s: %v", sessionID, err)
		}
		now := f.clock.Now()
		switch state {
		case run.SessionLost:
			if v.State == run.SessionActive || v.State == run.SessionLaunching {
				if v, err = v.Reconcile(now); err != nil {
					t.Fatalf("reconcile session %s: %v", sessionID, err)
				}
			}
			if v, err = v.MarkLost(now); err != nil {
				t.Fatalf("mark session %s lost: %v", sessionID, err)
			}
		default:
			if v.State == run.SessionActive {
				if v, err = v.Stop(now); err != nil {
					t.Fatalf("stop session %s: %v", sessionID, err)
				}
			}
			if v, err = v.Terminate(now); err != nil {
				t.Fatalf("terminate session %s: %v", sessionID, err)
			}
		}
		if _, err := uow.Sessions().Save(t.Context(), v, rev); err != nil {
			t.Fatalf("save ended session %s: %v", sessionID, err)
		}
	})
}

// requirePlacementCurrent fails unless sessionID still holds a current,
// unsuperseded binding carrying incarnation. It guards the liveness test
// against passing for the wrong reason: a refusal that came from a retired
// placement would prove nothing about the session's own state.
func requirePlacementCurrent(t *testing.T, f *featureFixture, sessionID identity.SessionID, incarnation identity.IncarnationID) {
	t.Helper()
	f.inUOW(t, func(uow app.UnitOfWork) {
		binding, ok, err := uow.Bindings().Current(t.Context(), sessionID)
		switch {
		case err != nil:
			t.Fatalf("current binding of %s: %v", sessionID, err)
		case !ok:
			t.Fatalf("session %s has no current binding; the liveness test needs a live placement", sessionID)
		case binding.Superseded:
			t.Fatalf("session %s's binding is superseded; the liveness test needs a live placement", sessionID)
		case binding.IncarnationID != incarnation:
			t.Fatalf("session %s's binding carries incarnation %s, want %s", sessionID, binding.IncarnationID, incarnation)
		}
	})
}

// endedPrincipalVerbs names the principalVerbs whose judge reports a
// liveness refusal as a refusal. The others refuse an ended session too,
// but each recognizes only its own incarnation-refusal wording, and
// message ack cannot be staged against one at all: seeding its delivery
// needs a live parent session to hang a worker off.
var endedPrincipalVerbs = []string{ //nolint:gochecknoglobals // the verb subset this rule covers, immutable after init like principalVerbs itself.
	"task create", "task retry", "plan close", "message send", "review submit",
}

// TestPrincipalEndedSessionRefused pins the liveness half of the one
// caller rule: a session that has ended is never a current principal,
// however current its placement still looks. Each case leaves the binding
// exactly as the placement recorded it — current, unsuperseded, carrying
// the caller's own incarnation — so the incarnation rule alone would admit
// every one of these calls, and only the session's state can refuse them.
func TestPrincipalEndedSessionRefused(t *testing.T) {
	for _, verb := range principalVerbs() {
		if !slices.Contains(endedPrincipalVerbs, verb.name) {
			continue
		}
		for _, state := range []run.SessionState{run.SessionTerminated, run.SessionLost} {
			t.Run(verb.name+"/"+string(state), func(t *testing.T) {
				f, p := verb.setup(t)
				endSession(t, f, p.session, state)
				requirePlacementCurrent(t, f, p.session, p.bound)
				if verb.judge(t, f, p, p.bound, 7940) {
					t.Errorf("%s accepted a %s session; want a refusal", verb.name, state)
				}
			})
		}
	}
}
