package app

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/johnlanda/hop/internal/domain/identity"
)

// ResultDigestTag is the version tag of the canonical result-acceptance
// digest (docs/plan/phase-2-design.md section 7). A future incompatible
// change to the encoding requires a new tag, never a silent reinterpretation
// of "hop-result-v1".
const ResultDigestTag = "hop-result-v1"

// ComputeResultDigest computes the canonical digest of one result
// submission: the version tag, then each field length-prefixed as
// "<decimal byte length>:<raw bytes>" in fixed order — run UUID, task UUID,
// attempt UUID, commit object ID, summary bytes — hashed with SHA-256 and
// rendered as lowercase hex. This is the only place HOP computes the
// digest; the domain receives it as an opaque, already-validated string.
func ComputeResultDigest(runID identity.RunID, taskID identity.TaskID, attemptID identity.AttemptID, commitOID, summary string) string {
	var b strings.Builder
	b.WriteString(ResultDigestTag)
	writeDigestField(&b, runID.String())
	writeDigestField(&b, taskID.String())
	writeDigestField(&b, attemptID.String())
	writeDigestField(&b, commitOID)
	writeDigestField(&b, summary)
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}

// writeDigestField appends one length-prefixed field to b.
func writeDigestField(b *strings.Builder, field string) {
	fmt.Fprintf(b, "%d:%s", len(field), field)
}
