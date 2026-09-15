package system

import (
	"crypto/rand"
	"fmt"

	"github.com/johnlanda/hop/internal/app"
)

// IDGenerator implements app.IDGenerator with version-4 UUIDs from
// crypto/rand, rendered in the canonical lowercase form the
// internal/domain/identity parsers accept.
type IDGenerator struct{}

var _ app.IDGenerator = IDGenerator{}

// NewID returns a fresh canonical lowercase UUIDv4 string
// (xxxxxxxx-xxxx-4xxx-yxxx-xxxxxxxxxxxx with the RFC 4122 variant).
func (IDGenerator) NewID() string {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		// crypto/rand.Read is documented to always fill the slice and never
		// return an error on supported platforms; an error here means the
		// platform's randomness source is unusable and no identity may be
		// minted.
		panic(fmt.Sprintf("crypto/rand failed: %v", err))
	}
	raw[6] = raw[6]&0x0f | 0x40 // version 4
	raw[8] = raw[8]&0x3f | 0x80 // RFC 4122 variant
	return fmt.Sprintf("%x-%x-%x-%x-%x", raw[0:4], raw[4:6], raw[6:8], raw[8:10], raw[10:16])
}
