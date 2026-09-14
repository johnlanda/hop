package run

import "github.com/johnlanda/hop/internal/domain/identity"

// ArtifactKind names what an artifact records.
type ArtifactKind string

// ArtifactKind values.
const (
	ArtifactAssignment   ArtifactKind = "assignment"
	ArtifactPaneSnapshot ArtifactKind = "pane-snapshot"
	ArtifactStopEvidence ArtifactKind = "stop-evidence"
	ArtifactCheckStdout  ArtifactKind = "check-stdout"
	ArtifactCheckStderr  ArtifactKind = "check-stderr"
)

// Artifact references a file under the run's artifact directory. Artifacts
// hold references, never blobs, in domain state; ResultID is nil for an
// artifact not owned by a particular result (an assignment or a pane
// snapshot, for example).
type Artifact struct {
	ID       identity.ArtifactID
	RunID    identity.RunID
	ResultID *identity.ResultID
	Kind     ArtifactKind
	Path     string
	Digest   string
}

// NewArtifact constructs an artifact with no owning result.
func NewArtifact(id identity.ArtifactID, runID identity.RunID, kind ArtifactKind, path, digest string) Artifact {
	return Artifact{ID: id, RunID: runID, Kind: kind, Path: path, Digest: digest}
}

// NewResultArtifact constructs an artifact owned by resultID.
func NewResultArtifact(id identity.ArtifactID, runID identity.RunID, resultID identity.ResultID, kind ArtifactKind, path, digest string) Artifact {
	return Artifact{ID: id, RunID: runID, ResultID: &resultID, Kind: kind, Path: path, Digest: digest}
}
