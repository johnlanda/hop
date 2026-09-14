package run

import (
	"fmt"
	"time"

	"github.com/johnlanda/hop/internal/domain/identity"
)

// LaunchKind distinguishes why a runtime binding exists: the run's initial
// launch, a cold relaunch's resume, or an occupant Herdr restored on its
// own that HOP only observed afterward.
type LaunchKind string

// LaunchKind values.
const (
	LaunchInitial          LaunchKind = "initial"
	LaunchResume           LaunchKind = "resume"
	LaunchRestoredObserved LaunchKind = "restored-observed"
)

// OccupantEvidence is the identity the runtime observed occupying a pane:
// the creation label, the argv marker (the run/attempt/incarnation or
// native session identifiers visible in the process's full argv) and the
// pid. Herdr's pane surface carries no process start time, so a pid is
// never evidence on its own — occupant identity is always this whole
// triple.
type OccupantEvidence struct {
	Label      string
	ArgvMarker string
	PID        int
}

// RuntimeBinding is one append-only record of a session's observed runtime
// placement: one row per launch incarnation, plus observed-restoration
// rows. Observations, closes and retirements are valid only against a
// current (non-superseded) binding; supersession requires recorded
// evidence, never assumption.
type RuntimeBinding struct {
	SessionID          identity.SessionID
	IncarnationID      identity.IncarnationID
	ServerSocketPath   string
	WorkspaceID        string
	TabID              string
	PaneID             string
	CreationLabel      string
	LaunchKind         LaunchKind
	Occupant           *OccupantEvidence
	ObservedAt         time.Time
	Superseded         bool
	SupersededAt       time.Time
	SupersededEvidence string
}

// NewRuntimeBinding constructs a binding for a newly created pane, with no
// occupant evidence yet: it is recorded once the runtime is inspected.
func NewRuntimeBinding(sessionID identity.SessionID, incarnationID identity.IncarnationID, serverSocketPath, workspaceID, tabID, paneID, creationLabel string, kind LaunchKind, now time.Time) RuntimeBinding {
	return RuntimeBinding{
		SessionID:        sessionID,
		IncarnationID:    incarnationID,
		ServerSocketPath: serverSocketPath,
		WorkspaceID:      workspaceID,
		TabID:            tabID,
		PaneID:           paneID,
		CreationLabel:    creationLabel,
		LaunchKind:       kind,
		ObservedAt:       now,
	}
}

// Observe records freshly inspected occupant evidence against a current
// binding. A superseded binding never accepts a new observation: once
// another binding has taken over as current, only that binding's own
// observations are meaningful.
func (b RuntimeBinding) Observe(evidence OccupantEvidence, now time.Time) (RuntimeBinding, error) { //nolint:gocritic // hugeParam: RuntimeBinding is an immutable domain value returned by every transition; a pointer receiver would let a caller's original be mutated through it, breaking the pure-transition contract.
	if b.Superseded {
		return b, fmt.Errorf("%w: binding %s/%s: cannot observe a superseded binding", ErrInvalidTransition, b.SessionID, b.IncarnationID)
	}
	if evidence.Label == "" || evidence.ArgvMarker == "" || evidence.PID <= 0 {
		return b, fmt.Errorf("%w: binding %s/%s: occupant evidence requires a label, an argv marker and a positive pid — a pid is never evidence alone", ErrInvalidTransition, b.SessionID, b.IncarnationID)
	}
	b.Occupant = &evidence
	b.ObservedAt = now
	return b, nil
}

// Supersede marks the binding no longer current, recording the evidence
// that established supersession. A binding supersedes at most once;
// evidence must be non-empty, since supersession is never assumed.
func (b RuntimeBinding) Supersede(evidence string, now time.Time) (RuntimeBinding, error) { //nolint:gocritic // hugeParam: RuntimeBinding is an immutable domain value returned by every transition; a pointer receiver would let a caller's original be mutated through it, breaking the pure-transition contract.
	if b.Superseded {
		return b, fmt.Errorf("%w: binding %s/%s: already superseded", ErrInvalidTransition, b.SessionID, b.IncarnationID)
	}
	if evidence == "" {
		return b, fmt.Errorf("%w: binding %s/%s: supersession requires recorded evidence", ErrInvalidTransition, b.SessionID, b.IncarnationID)
	}
	b.Superseded = true
	b.SupersededAt = now
	b.SupersededEvidence = evidence
	return b, nil
}
