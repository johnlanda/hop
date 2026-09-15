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

// RequestDigestTag is the version tag of the canonical request digest
// (docs/plan/phase-3-design.md section 7).
const RequestDigestTag = "hop-request-v1"

// ComputeRequestDigest computes the canonical "hop-request-v1" digest of
// one mutating messaging/plan verb request: the version tag, then the verb
// name, the run UUID and the sender's LOGICAL address (AddressString —
// stable across incarnations and manager succession, unlike a session ID),
// then payload in the caller's given order, each length-prefixed as in
// ComputeResultDigest, hashed with SHA-256 and rendered as lowercase hex.
// Callers assemble payload per the section 7 normalization rules: for a
// send, recipient address string, kind, reply-to UUID or "", relay-of
// UUID or "", then the body file's bytes digest (never a path); for
// task-create, title, the instructions file's bytes digest, then the
// dependency UUIDs sorted, one field each; for retry, task UUID then
// reason; for plan-close, nothing further; for a human answer, the
// question UUID then the answer body's bytes digest.
func ComputeRequestDigest(verb, runID, senderAddress string, payload ...string) string {
	var b strings.Builder
	b.WriteString(RequestDigestTag)
	writeDigestField(&b, verb)
	writeDigestField(&b, runID)
	writeDigestField(&b, senderAddress)
	for _, field := range payload {
		writeDigestField(&b, field)
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}
