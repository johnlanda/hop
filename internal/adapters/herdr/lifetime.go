package herdr

import "net"

// serverLifetime renders the lifetime identity of the server process that
// accepted conn (serverLifetimeTag's format where one is implemented): its socket peer pid (fixed when the connection is
// established, before any request is sent) and that process's OS start
// time (processLifetime). It is "" — unknown — when either cannot be read.
// Herdr never re-execs a running server in place (a restart and a live
// handoff each start a new process), so one token never spans two server
// lifetimes.
func serverLifetime(conn net.Conn) string {
	pid, ok := peerPID(conn)
	if !ok {
		return ""
	}
	return processLifetime(pid)
}

// stableLifetime is the identity an observation bracketed by two lifetime
// reads of the same peer is stamped with: the identity itself when both
// reads are known and equal, else "" (unknown). A pid cannot be reused
// while its process lives, so equal reads before and after an exchange
// mean one process held the peer pid throughout it.
func stableLifetime(before, after string) string {
	if before == "" || before != after {
		return ""
	}
	return before
}
