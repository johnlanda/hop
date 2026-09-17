package herdr

import (
	"net"
	"testing"
)

// TestServerLifetimeUnknownForNonSocketConn proves serverLifetime reports
// "" (unknown), never a fabricated identity, for a connection not backed by
// a real socket descriptor. net.Pipe's connections implement no
// syscall.Conn on any platform, so this is portable and exercises every
// serverLifetime implementation identically.
func TestServerLifetimeUnknownForNonSocketConn(t *testing.T) {
	client, server := net.Pipe()
	t.Cleanup(func() {
		closeConn(client)
		closeConn(server)
	})

	if got := serverLifetime(client); got != "" {
		t.Errorf("serverLifetime = %q for a net.Pipe connection, want \"\"", got)
	}
}

// TestStableLifetime proves an observation is stamped only when the
// identities read before and after it agree and are known.
func TestStableLifetime(t *testing.T) {
	for _, tt := range []struct {
		before, after, want string
	}{
		{"a", "a", "a"},
		{"a", "b", ""},
		{"a", "", ""},
		{"", "a", ""},
		{"", "", ""},
	} {
		if got := stableLifetime(tt.before, tt.after); got != tt.want {
			t.Errorf("stableLifetime(%q, %q) = %q, want %q", tt.before, tt.after, got, tt.want)
		}
	}
}
