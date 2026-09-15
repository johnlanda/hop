package run

import (
	"fmt"
	"time"

	"github.com/johnlanda/hop/internal/domain/identity"
)

// SessionState is one state of the Session state machine (section 5,
// "Session").
type SessionState string

// Session states.
const (
	SessionReserved    SessionState = "reserved"
	SessionLaunching   SessionState = "launching"
	SessionActive      SessionState = "active"
	SessionReconciling SessionState = "reconciling"
	SessionLost        SessionState = "lost"
	SessionStopping    SessionState = "stopping"
	SessionTerminated  SessionState = "terminated"
)

// sessionTransitions is the exhaustive Session state table of section 5.
var sessionTransitions = newTransitionTable(concatPairs( //nolint:gochecknoglobals // sessionTransitions is the exhaustive, immutable Session state table of section 5; it never mutates after init.
	fromAny(SessionLaunching, SessionReserved),
	fromAny(SessionActive, SessionLaunching, SessionReconciling),
	fromAny(SessionReconciling, SessionLaunching, SessionActive),
	fromAny(SessionLost, SessionReconciling),
	fromAny(SessionStopping, SessionLaunching, SessionActive, SessionReconciling),
	fromAny(SessionTerminated, SessionReserved, SessionLaunching, SessionStopping, SessionReconciling),
))

// Role is a session's part in a run. Phase 2 has exactly one worker session
// per incarnation and no manager session.
type Role string

// RoleWorker is the only role Phase 2 creates: a solo run's single
// attempt-bound session.
const RoleWorker Role = "worker"

// Phase 3 feature-mode roles: RoleManager plans and delegates and has no
// bound attempt; RoleImplementer and RoleReviewer are attempt-bound
// children the manager delegates to, one level deep.
const (
	RoleManager     Role = "manager"
	RoleImplementer Role = "implementer"
	RoleReviewer    Role = "reviewer"
)

// Harness is the native agent a session runs.
type Harness string

// Harness values.
const (
	HarnessClaude   Harness = "claude"
	HarnessCodex    Harness = "codex"
	HarnessOpenCode Harness = "opencode"
)

// NativeRefSource distinguishes how a session's native reference came to be
// known: pre-assigned by HOP before first launch, or captured from an
// observed process afterward.
type NativeRefSource string

// NativeRefSource values.
const (
	NativeRefAssigned NativeRefSource = "assigned"
	NativeRefCaptured NativeRefSource = "captured"
)

// Session is a HOP identity for one manager/worker harness lifecycle,
// including its launch reservation. One session executes at most one
// attempt; a cold relaunch is a new session bound to the same attempt,
// never a revived old one. AttemptID is optional: it is empty for a
// manager session, the only role with none. ParentSessionID is nil for
// the manager and for every Phase 2 (RoleWorker) session; a Phase 3 child
// (RoleImplementer or RoleReviewer) carries the run's manager session ID
// that was current at its own creation — historical provenance, never
// rewritten even once a successor manager takes over.
type Session struct {
	ID               identity.SessionID
	RunID            identity.RunID
	AttemptID        identity.AttemptID
	ParentSessionID  *identity.SessionID
	Role             Role
	Harness          Harness
	NativeSessionRef string
	NativeRefSource  NativeRefSource
	State            SessionState
	UpdatedAt        time.Time
}

// NewSession constructs a worker session in its initial reserved state,
// bound to attempt. Manager sessions, and therefore a session with no bound
// attempt, are out of Phase 2's scope (Phase 3's NewManagerSession).
func NewSession(id identity.SessionID, runID identity.RunID, attemptID identity.AttemptID, harness Harness, now time.Time) Session {
	return Session{
		ID:        id,
		RunID:     runID,
		AttemptID: attemptID,
		Role:      RoleWorker,
		Harness:   harness,
		State:     SessionReserved,
		UpdatedAt: now,
	}
}

// NewManagerSession constructs a run's manager session in its initial
// reserved state. A manager session has no bound attempt and no parent —
// the only role for which both are true.
func NewManagerSession(id identity.SessionID, runID identity.RunID, harness Harness, now time.Time) Session {
	return Session{
		ID:        id,
		RunID:     runID,
		Role:      RoleManager,
		Harness:   harness,
		State:     SessionReserved,
		UpdatedAt: now,
	}
}

// NewChildSession constructs an implementer or reviewer session in its
// initial reserved state, bound to attemptID and delegated by parent — the
// run's manager session at creation time. One-level delegation is enforced
// here for the part this package can check unaided: parent must itself
// carry no parent (ErrDelegationDepth otherwise — a session with a parent
// can never itself be a parent). That parent is actually the run's
// CURRENT manager session is a cross-session fact only the application,
// with visibility into every session, can validate before calling this
// constructor. role must be RoleImplementer or RoleReviewer; any other
// role is ErrInvalidTransition.
func NewChildSession(id identity.SessionID, runID identity.RunID, attemptID identity.AttemptID, role Role, parent Session, harness Harness, now time.Time) (Session, error) { //nolint:gocritic // hugeParam: Session is passed by value everywhere in this package; this constructor mirrors that convention.
	if role != RoleImplementer && role != RoleReviewer {
		return Session{}, fmt.Errorf("%w: session %s: %q is not a delegable role", ErrInvalidTransition, id, role)
	}
	if parent.ParentSessionID != nil {
		return Session{}, fmt.Errorf("%w: session %s: parent %s already has a parent", ErrDelegationDepth, id, parent.ID)
	}
	parentID := parent.ID
	return Session{
		ID:              id,
		RunID:           runID,
		AttemptID:       attemptID,
		ParentSessionID: &parentID,
		Role:            role,
		Harness:         harness,
		State:           SessionReserved,
		UpdatedAt:       now,
	}, nil
}

// AssignNativeRef records the session's native reference and its source.
// The reference is immutable once assigned: a session that already carries
// one refuses to record another, even an identical one. ref must be
// non-empty and source must be NativeRefAssigned or NativeRefCaptured.
func (s Session) AssignNativeRef(ref string, source NativeRefSource, now time.Time) (Session, error) { //nolint:gocritic // hugeParam: Session is an immutable domain value returned by every transition; a pointer receiver would let a caller's original be mutated through it, breaking the pure-transition contract.
	if s.NativeSessionRef != "" {
		return s, fmt.Errorf("%w: session %s: native reference is already assigned", ErrInvalidTransition, s.ID)
	}
	if ref == "" {
		return s, fmt.Errorf("%w: session %s: native reference must not be empty", ErrInvalidTransition, s.ID)
	}
	if source != NativeRefAssigned && source != NativeRefCaptured {
		return s, fmt.Errorf("%w: session %s: %q is not a valid native reference source", ErrInvalidTransition, s.ID, source)
	}
	s.NativeSessionRef = ref
	s.NativeRefSource = source
	s.UpdatedAt = now
	return s, nil
}

// transition returns s with its state moved to to, or ErrInvalidTransition
// when the section 5 table does not list (s.State, to).
func (s Session) transition(to SessionState, now time.Time) (Session, error) { //nolint:gocritic // hugeParam: Session is an immutable domain value returned by every transition; a pointer receiver would let a caller's original be mutated through it, breaking the pure-transition contract.
	if !sessionTransitions.valid(s.State, to) {
		return s, fmt.Errorf("%w: session %s: %s to %s", ErrInvalidTransition, s.ID, s.State, to)
	}
	s.State = to
	s.UpdatedAt = now
	return s, nil
}

// Launch moves the session into launching: a launch intent has been
// recorded.
func (s Session) Launch(now time.Time) (Session, error) { return s.transition(SessionLaunching, now) } //nolint:gocritic // hugeParam: Session is an immutable domain value returned by every transition; a pointer receiver would let a caller's original be mutated through it, breaking the pure-transition contract.

// ConfirmActive moves the session into active: its launch claim settled
// execed, or, from reconciling, its occupant was verified against the
// current binding's claim evidence (warm reattach).
func (s Session) ConfirmActive(now time.Time) (Session, error) { //nolint:gocritic // hugeParam: Session is an immutable domain value returned by every transition; a pointer receiver would let a caller's original be mutated through it, breaking the pure-transition contract.
	return s.transition(SessionActive, now)
}

// Reconcile moves the session into reconciling: ambiguity or takeover
// leaves its identity not yet established.
func (s Session) Reconcile(now time.Time) (Session, error) { //nolint:gocritic // hugeParam: Session is an immutable domain value returned by every transition; a pointer receiver would let a caller's original be mutated through it, breaking the pure-transition contract.
	return s.transition(SessionReconciling, now)
}

// MarkLost moves the session into lost: absence was conclusively
// established, by positive evidence or a recorded human attestation.
func (s Session) MarkLost(now time.Time) (Session, error) { return s.transition(SessionLost, now) } //nolint:gocritic // hugeParam: Session is an immutable domain value returned by every transition; a pointer receiver would let a caller's original be mutated through it, breaking the pure-transition contract.

// Stop moves the session into stopping: an interrupt was dispatched against
// a corroborated live process; termination is then observed, never
// declared.
func (s Session) Stop(now time.Time) (Session, error) { return s.transition(SessionStopping, now) } //nolint:gocritic // hugeParam: Session is an immutable domain value returned by every transition; a pointer receiver would let a caller's original be mutated through it, breaking the pure-transition contract.

// Terminate moves the session into terminated: termination was observed
// from stopping, or the session ends with no corroborated live process to
// interrupt — stopped or exec failure from reserved or launching, or
// retirement completed, or a stop with no live process established, from
// reconciling.
func (s Session) Terminate(now time.Time) (Session, error) { //nolint:gocritic // hugeParam: Session is an immutable domain value returned by every transition; a pointer receiver would let a caller's original be mutated through it, breaking the pure-transition contract.
	return s.transition(SessionTerminated, now)
}
