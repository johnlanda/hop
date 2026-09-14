// Package system implements the application's local-machine ports: the UTC
// wall clock (app.Clock), the crypto/rand UUID generator (app.IDGenerator)
// and the durable temp-then-rename artifact store (app.ArtifactStore).
package system

import (
	"time"

	"github.com/johnlanda/hop/internal/app"
)

// Clock implements app.Clock with the real wall clock normalized to UTC, so
// every value handed into domain transitions and persisted state carries
// one location.
type Clock struct{}

var _ app.Clock = Clock{}

// Now returns the current wall-clock time in UTC.
func (Clock) Now() time.Time { return time.Now().UTC() }
