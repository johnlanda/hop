package process

import (
	"slices"
	"strings"
	"testing"
)

func TestParseCmdlineTable(t *testing.T) {
	cases := []struct {
		name    string
		raw     []byte
		want    []string
		wantErr string
	}{
		{
			name: "plain vector",
			raw:  []byte("/bin/sleep\x0030\x00"),
			want: []string{"/bin/sleep", "30"},
		},
		{
			name: "empty leading argument preserved",
			raw:  []byte("\x0030\x00"),
			want: []string{"", "30"},
		},
		{
			name: "empty middle and trailing arguments preserved",
			raw:  []byte("a\x00\x00b\x00\x00"),
			want: []string{"a", "", "b", ""},
		},
		{
			name: "lone empty argv0",
			raw:  []byte{0},
			want: []string{""},
		},
		{
			name: "missing final terminator still splits exactly",
			raw:  []byte("a\x00b"),
			want: []string{"a", "b"},
		},
		{
			name:    "empty buffer is unavailable, never an invented vector",
			raw:     nil,
			wantErr: "no readable argument vector",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			argv, err := parseCmdline(tc.raw)

			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error = %v, want one containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseCmdline: %v", err)
			}
			if !slices.Equal(argv, tc.want) {
				t.Errorf("argv = %q, want %q", argv, tc.want)
			}
		})
	}
}
