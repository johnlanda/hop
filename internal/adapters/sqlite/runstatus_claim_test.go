package sqlite_test

import (
	"errors"
	"testing"

	"github.com/johnlanda/hop/internal/app"
	"github.com/johnlanda/hop/internal/domain/identity"
	"github.com/johnlanda/hop/internal/domain/run"
)

// requireDetailClaim asserts LoadRunStatus names session, a binding when
// wantBinding, and the claim of incarnation (none when incarnation is "").
func requireDetailClaim(t *testing.T, f *fixture, session identity.SessionID, wantBinding bool, incarnation identity.IncarnationID) {
	t.Helper()
	detail, err := f.store.LoadRunStatus(t.Context(), f.spec.RunID)
	if err != nil {
		t.Fatalf("LoadRunStatus: %v", err)
	}
	if detail.SessionID != session {
		t.Errorf("detail session = %s, want %s", detail.SessionID, session)
	}
	if (detail.Binding != nil) != wantBinding {
		t.Errorf("detail binding = %+v, want present %t", detail.Binding, wantBinding)
	}
	switch {
	case incarnation == "" && detail.Claim != nil:
		t.Errorf("detail claim = %+v, want none", detail.Claim)
	case incarnation != "" && (detail.Claim == nil || detail.Claim.IncarnationID != incarnation):
		t.Errorf("detail claim = %+v, want incarnation %s's", detail.Claim, incarnation)
	}
}

// TestLoadRunStatusResolvesTheLaunchContextClaim pins the status read
// model's claim to the session launch context's resolution
// (docs/plan/phase-2-design.md section 4: claim state decides): the
// current binding's incarnation, else the session's newest pending launch
// intent's, a disagreement surfacing no claim — for the solo worker and
// the feature manager alike.
func TestLoadRunStatusResolvesTheLaunchContextClaim(t *testing.T) {
	t.Run("solo: a claim written before the binding", func(t *testing.T) {
		f := newFixture(t)
		f.launchAttempt(t)
		f.createLaunchIntent(t, f.spec.SessionID, f.spec.IncarnationID)
		requireDetailClaim(t, f, f.spec.SessionID, false, "")
		f.claimLaunch(t)
		requireDetailClaim(t, f, f.spec.SessionID, false, f.spec.IncarnationID)
		f.createBinding(t)
		requireDetailClaim(t, f, f.spec.SessionID, true, f.spec.IncarnationID)
	})

	t.Run("solo: a binding and a differing pending intent surface no claim", func(t *testing.T) {
		f := newFixture(t)
		f.launchAttempt(t)
		f.createBinding(t)
		f.claimLaunch(t)
		requireDetailClaim(t, f, f.spec.SessionID, true, f.spec.IncarnationID)
		f.createLaunchIntent(t, f.spec.SessionID, identity.IncarnationID(uid(8401)))
		requireDetailClaim(t, f, f.spec.SessionID, true, "")
	})

	t.Run("feature manager: a claim written before the binding", func(t *testing.T) {
		clock := newFakeClock()
		store := openStoreAt(t, t.TempDir(), clock)
		spec := newSpec("/repos/manager-status", specStride, clock.Now())
		lease := initLegacyFeatureRun(t, store, &spec)
		f := &featureFixture{fixture: &fixture{store: store, clock: clock, spec: spec, lease: lease}}
		managerID := identity.SessionID(uid(8411))
		incarnation := identity.IncarnationID(uid(8412))
		now := clock.Now()
		f.inUOW(t, func(uow app.UnitOfWork) {
			manager, err := run.NewManagerSession(managerID, spec.RunID, run.HarnessClaude, now).Launch(now)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := uow.Sessions().Create(t.Context(), manager); err != nil {
				t.Fatalf("create manager session: %v", err)
			}
		})
		createLaunchIntentFor(t, f, 8413, managerID, incarnation)
		if err := store.ClaimLaunch(t.Context(), claimFor(f, incarnation, managerID, "", 8414)); err != nil {
			t.Fatalf("ClaimLaunch(manager): %v", err)
		}
		requireDetailClaim(t, f.fixture, managerID, false, incarnation)
	})

	t.Run("feature manager: a binding and a differing pending intent surface no claim", func(t *testing.T) {
		f := newFeatureFixture(t)
		if err := f.store.ClaimLaunch(t.Context(), claimFor(f, f.ManagerIncarnation, f.ManagerID, "", 8421)); err != nil {
			t.Fatalf("ClaimLaunch(bound manager): %v", err)
		}
		requireDetailClaim(t, f.fixture, f.ManagerID, true, f.ManagerIncarnation)
		createLaunchIntentFor(t, f, 8422, f.ManagerID, identity.IncarnationID(uid(8424)))
		requireDetailClaim(t, f.fixture, f.ManagerID, true, "")
	})

	t.Run("feature manager: an older differing pending intent surfaces no claim though the newest agrees", func(t *testing.T) {
		f := newFeatureFixture(t)
		if err := f.store.ClaimLaunch(t.Context(), claimFor(f, f.ManagerIncarnation, f.ManagerID, "", 8431)); err != nil {
			t.Fatalf("ClaimLaunch(bound manager): %v", err)
		}
		createLaunchIntentFor(t, f, 8432, f.ManagerID, identity.IncarnationID(uid(8434)))
		createLaunchIntentFor(t, f, 8435, f.ManagerID, f.ManagerIncarnation)
		requireDetailClaim(t, f.fixture, f.ManagerID, true, "")
		if _, err := f.store.LoadSessionLaunchContext(t.Context(), f.spec.RunID, f.ManagerID); !errors.Is(err, app.ErrNotFound) {
			t.Errorf("LoadSessionLaunchContext() error = %v, want ErrNotFound: the launch context resolves nothing either", err)
		}
	})
}
