package sqlite

import (
	"strings"
	"testing"
	"time"
)

// TestTimeFormatRoundTrip proves the canonical timestamp form is
// fixed-width, UTC with a literal trailing Z, round-trips exactly, and
// orders lexically the way the times order temporally.
func TestTimeFormatRoundTrip(t *testing.T) {
	cases := []struct {
		name string
		in   time.Time
		want string
	}{
		{
			name: "nanosecond precision",
			in:   time.Date(2026, 9, 14, 1, 2, 3, 123456789, time.UTC),
			want: "2026-09-14T01:02:03.123456789Z",
		},
		{
			name: "whole second pads nine zeros",
			in:   time.Date(2026, 9, 14, 1, 2, 3, 0, time.UTC),
			want: "2026-09-14T01:02:03.000000000Z",
		},
		{
			name: "non-UTC input is rendered in UTC",
			in:   time.Date(2026, 9, 14, 1, 2, 3, 5, time.FixedZone("plus2", 2*3600)),
			want: "2026-09-13T23:02:03.000000005Z",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := formatTime(tc.in)

			if got != tc.want {
				t.Fatalf("formatTime(%v) = %q, want %q", tc.in, got, tc.want)
			}
			if len(got) != len("2006-01-02T15:04:05.000000000Z") {
				t.Fatalf("formatTime(%v) = %q is not fixed-width", tc.in, got)
			}
			parsed, err := parseTime(got)
			if err != nil {
				t.Fatalf("parseTime(%q): %v", got, err)
			}
			if !parsed.Equal(tc.in) {
				t.Fatalf("round trip of %v = %v", tc.in, parsed)
			}
		})
	}
}

// TestTimeFormatLexicalOrderAgreesWithTimeOrder proves equal-length lexical
// comparison of formatted values agrees with the order of the times.
func TestTimeFormatLexicalOrderAgreesWithTimeOrder(t *testing.T) {
	base := time.Date(2026, 9, 14, 1, 2, 3, 999999999, time.UTC)
	times := []time.Time{
		base,
		base.Add(time.Nanosecond),
		base.Add(time.Second),
		base.Add(24 * time.Hour),
		time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC),
	}
	for i := 1; i < len(times); i++ {
		earlier, later := formatTime(times[i-1]), formatTime(times[i])

		if strings.Compare(earlier, later) >= 0 {
			t.Fatalf("lexical order disagrees with time order: %q is not below %q", earlier, later)
		}
	}
}

// TestParseTimeRejectsNonCanonicalForms proves stored timestamps must be in
// the exact canonical form.
func TestParseTimeRejectsNonCanonicalForms(t *testing.T) {
	cases := []struct {
		name string
		in   string
	}{
		{name: "missing fractional digits", in: "2026-09-14T01:02:03Z"},
		{name: "offset instead of Z", in: "2026-09-14T01:02:03.000000000+00:00"},
		{name: "short fraction", in: "2026-09-14T01:02:03.123Z"},
		{name: "comma fractional separator", in: "2026-09-14T10:00:00,000000000Z"},
		{name: "unpadded hour", in: "2026-09-14T1:00:00.000000000Z"},
		{name: "empty", in: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := parseTime(tc.in); err == nil {
				t.Fatalf("parseTime(%q) accepted a non-canonical form", tc.in)
			}
		})
	}
}
