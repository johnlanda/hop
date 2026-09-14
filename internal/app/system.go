package app

import "time"

// Clock reports the current time. Application code reads it explicitly and
// passes the result into domain transitions and persisted values; it never
// reads a clock ambiently.
type Clock interface {
	Now() time.Time
}

// IDGenerator mints a fresh canonical lowercase UUID string, parsed into a
// typed identity.<Kind>ID by the caller.
type IDGenerator interface {
	NewID() string
}
