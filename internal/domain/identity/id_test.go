package identity_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/johnlanda/hop/internal/domain/identity"
)

// parseCases are the shared valid/invalid canonical-UUID fixtures every
// Parse<Kind>ID function is exercised against. It is a function, not a
// package-level slice, only to keep this shared fixture out of the
// global-variable set the project's lint configuration restricts.
func parseCases() []struct {
	name    string
	raw     string
	wantErr bool
} {
	return []struct {
		name    string
		raw     string
		wantErr bool
	}{
		{name: "valid canonical", raw: "0b1f2b3a-4c5d-4e6f-8a9b-0c1d2e3f4a5b", wantErr: false},
		{name: "valid all zero", raw: "00000000-0000-0000-0000-000000000000", wantErr: false},
		{name: "valid all f", raw: "ffffffff-ffff-ffff-ffff-ffffffffffff", wantErr: false},
		{name: "empty string", raw: "", wantErr: true},
		{name: "uppercase", raw: "0B1F2B3A-4c5d-4e6f-8a9b-0c1d2e3f4a5b", wantErr: true},
		{name: "missing hyphens", raw: "0b1f2b3a4c5d4e6f8a9b0c1d2e3f4a5b", wantErr: true},
		{name: "first group too long", raw: "0b1f2b3a4-4c5d-4e6f-8a9b-0c1d2e3f4a5", wantErr: true},
		{name: "short", raw: "0b1f2b3a-4c5d-4e6f-8a9b-0c1d2e3f4a5", wantErr: true},
		{name: "long", raw: "0b1f2b3a-4c5d-4e6f-8a9b-0c1d2e3f4a5bc", wantErr: true},
		{name: "non-hex character", raw: "0b1f2b3g-4c5d-4e6f-8a9b-0c1d2e3f4a5b", wantErr: true},
		{name: "leading whitespace", raw: " 0b1f2b3a-4c5d-4e6f-8a9b-0c1d2e3f4a5b", wantErr: true},
		{name: "trailing whitespace", raw: "0b1f2b3a-4c5d-4e6f-8a9b-0c1d2e3f4a5b ", wantErr: true},
		{name: "extra group", raw: "0b1f2b3a-4c5d-4e6f-8a9b-0c1d-2e3f4a5b", wantErr: true},
		{name: "wrong separator", raw: "0b1f2b3a_4c5d_4e6f_8a9b_0c1d2e3f4a5b", wantErr: true},
		{name: "plus sign in group", raw: "+b1f2b3a-4c5d-4e6f-8a9b-0c1d2e3f4a5b", wantErr: true},
	}
}

// testParser runs parseCases against one Parse<Kind>ID function, asserting
// that a rejected string always wraps identity.ErrInvalidID and that an
// accepted string round-trips through String().
func testParser[T fmt.Stringer](t *testing.T, parse func(string) (T, error)) {
	t.Helper()
	for _, tc := range parseCases() {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parse(tc.raw)

			if tc.wantErr {
				if err == nil {
					t.Fatalf("parse(%q) = %v, want error", tc.raw, got)
				}
				if !errors.Is(err, identity.ErrInvalidID) {
					t.Fatalf("parse(%q) error = %v, want wrapping ErrInvalidID", tc.raw, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("parse(%q) unexpected error: %v", tc.raw, err)
			}
			if got.String() != tc.raw {
				t.Fatalf("parse(%q).String() = %q, want %q", tc.raw, got.String(), tc.raw)
			}
		})
	}
}

func TestParseRepositoryID(t *testing.T) { testParser(t, identity.ParseRepositoryID) }
func TestParseRunID(t *testing.T)        { testParser(t, identity.ParseRunID) }
func TestParseTaskID(t *testing.T)       { testParser(t, identity.ParseTaskID) }
func TestParseAttemptID(t *testing.T)    { testParser(t, identity.ParseAttemptID) }
func TestParseSessionID(t *testing.T)    { testParser(t, identity.ParseSessionID) }
func TestParseWorktreeID(t *testing.T)   { testParser(t, identity.ParseWorktreeID) }
func TestParseResultID(t *testing.T)     { testParser(t, identity.ParseResultID) }
func TestParseArtifactID(t *testing.T)   { testParser(t, identity.ParseArtifactID) }
func TestParseOperationID(t *testing.T)  { testParser(t, identity.ParseOperationID) }
func TestParseIncarnationID(t *testing.T) {
	testParser(t, identity.ParseIncarnationID)
}
func TestParseMessageID(t *testing.T)     { testParser(t, identity.ParseMessageID) }
func TestParseReviewID(t *testing.T)      { testParser(t, identity.ParseReviewID) }
func TestParseIntegrationID(t *testing.T) { testParser(t, identity.ParseIntegrationID) }

// TestParsedIDsAreDistinctTypes documents that every parsed identifier keeps
// its own Go type: a RunID and a TaskID built from the same raw UUID are not
// interchangeable at compile time. This test exercises the runtime value
// equality once the values are unwrapped to string, which is the only part
// of the distinctness invariant a test can observe; the type distinction
// itself is enforced by the compiler at every call site.
func TestParsedIDsAreDistinctTypes(t *testing.T) {
	const raw = "0b1f2b3a-4c5d-4e6f-8a9b-0c1d2e3f4a5b"

	run, err := identity.ParseRunID(raw)
	if err != nil {
		t.Fatalf("ParseRunID(%q): %v", raw, err)
	}
	task, err := identity.ParseTaskID(raw)
	if err != nil {
		t.Fatalf("ParseTaskID(%q): %v", raw, err)
	}

	if run.String() != task.String() {
		t.Fatalf("run.String() = %q, task.String() = %q, want equal underlying values", run.String(), task.String())
	}
}

// fuzzSeeds are shared seed corpus entries for every ID fuzz target. It is a
// function, not a package-level slice, for the same reason as parseCases.
func fuzzSeeds() []string {
	return []string{
		"0b1f2b3a-4c5d-4e6f-8a9b-0c1d2e3f4a5b",
		"00000000-0000-0000-0000-000000000000",
		"",
		"not-a-uuid",
		"0B1F2B3A-4c5d-4e6f-8a9b-0c1d2e3f4a5b",
		"0b1f2b3a4c5d4e6f8a9b0c1d2e3f4a5b",
		"0b1f2b3a-4c5d-4e6f-8a9b-0c1d2e3f4a5b\n",
	}
}

// checkParseInvariants asserts the properties every Parse<Kind>ID function
// must hold for arbitrary input: never accept a non-canonical string, and
// round-trip through String() whatever it does accept.
func checkParseInvariants(t *testing.T, raw, s string, err error) {
	t.Helper()
	if err != nil {
		if !errors.Is(err, identity.ErrInvalidID) {
			t.Fatalf("parse(%q) error = %v, want wrapping ErrInvalidID", raw, err)
		}
		return
	}
	if s != raw {
		t.Fatalf("parse(%q) accepted but String() = %q", raw, s)
	}
	if len(raw) != 36 {
		t.Fatalf("parse(%q) accepted a string of length %d, want 36", raw, len(raw))
	}
	if strings.ToLower(raw) != raw {
		t.Fatalf("parse(%q) accepted a string containing an uppercase letter", raw)
	}
}

func FuzzParseRunID(f *testing.F) {
	for _, s := range fuzzSeeds() {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		id, err := identity.ParseRunID(raw)
		checkParseInvariants(t, raw, id.String(), err)
	})
}

func FuzzParseSessionID(f *testing.F) {
	for _, s := range fuzzSeeds() {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		id, err := identity.ParseSessionID(raw)
		checkParseInvariants(t, raw, id.String(), err)
	})
}

func FuzzParseIncarnationID(f *testing.F) {
	for _, s := range fuzzSeeds() {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		id, err := identity.ParseIncarnationID(raw)
		checkParseInvariants(t, raw, id.String(), err)
	})
}

func FuzzParseMessageID(f *testing.F) {
	for _, s := range fuzzSeeds() {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		id, err := identity.ParseMessageID(raw)
		checkParseInvariants(t, raw, id.String(), err)
	})
}

func FuzzParseReviewID(f *testing.F) {
	for _, s := range fuzzSeeds() {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		id, err := identity.ParseReviewID(raw)
		checkParseInvariants(t, raw, id.String(), err)
	})
}

func FuzzParseIntegrationID(f *testing.F) {
	for _, s := range fuzzSeeds() {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		id, err := identity.ParseIntegrationID(raw)
		checkParseInvariants(t, raw, id.String(), err)
	})
}
