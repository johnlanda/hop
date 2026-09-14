package run_test

import (
	"time"

	"github.com/johnlanda/hop/internal/domain/identity"
)

// epoch is the fixed instant every test threads through as "now". Domain
// functions never read a clock, so any fixed time.Time proves determinism.
// It is a function, not a package-level time.Time, only to keep the shared
// fixture out of the global-variable set the project's lint configuration
// restricts; every call returns the identical instant.
func epoch() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) }

// later is a second fixed instant, strictly after epoch, for tests that
// assert a second transition's timestamp advances.
func later() time.Time { return epoch().Add(time.Hour) }

// Shared test identities. Every ID type's underlying representation is
// string, so these are typed constants rather than package-level
// variables.
const (
	testRepositoryID = identity.RepositoryID("11111111-1111-4111-8111-111111111111")
	testRunID        = identity.RunID("22222222-2222-4222-8222-222222222222")
	testTaskID       = identity.TaskID("33333333-3333-4333-8333-333333333333")
	testAttemptID    = identity.AttemptID("44444444-4444-4444-8444-444444444444")
	testSessionID    = identity.SessionID("55555555-5555-4555-8555-555555555555")
	// testSecondSessionID is a cold relaunch's new session: a fresh
	// identity for a fresh incarnation of the same attempt.
	testSecondSessionID = identity.SessionID("55555555-5555-4555-8555-555555555556")
	testWorktreeID      = identity.WorktreeID("66666666-6666-4666-8666-666666666666")
	testResultID        = identity.ResultID("77777777-7777-4777-8777-777777777777")
	testArtifactID      = identity.ArtifactID("88888888-8888-4888-8888-888888888888")
	testIncarnation     = identity.IncarnationID("99999999-9999-4999-8999-999999999999")

	// identityOtherResultID is a second, distinct result identity used by
	// resubmission tests: a duplicate or conflicting retry is pre-generated
	// with its own identity by the application before the domain resolves
	// whether it is actually accepted.
	identityOtherResultID = identity.ResultID("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa")
)
