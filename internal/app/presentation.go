package app

import (
	"context"
	"fmt"
	"sort"
)

// Presentation source and token names. HOP owns a single reporter source so
// its view selection and metadata patches are attributable and clearable
// without disturbing another owner's projection.
const (
	// PresentationSource identifies HOP as a metadata reporter and view
	// owner. It assumes an enabled plugin with manifest id "hop".
	PresentationSource = "plugin:hop"

	// Metadata field (token) names published on a pane. Herdr caps token
	// names at 32 characters and values at 80; these stay well inside that.
	FieldRun      = "hop_run"
	FieldRunOrder = "hop_run_order"
	FieldOrder    = "hop_order"
	FieldRole     = "hop_role"
	FieldTask     = "hop_task"
	FieldParent   = "hop_parent"
	FieldAccount  = "hop_account"
	FieldState    = "hop_state"
)

// orderKeyWidth is the fixed width of a zero-padded ordering key. Token sorts
// are string comparisons in Herdr, so numeric keys must be padded to sort
// numerically. Six digits orders up to a million runs or workers.
const orderKeyWidth = 6

// Role is a worker's role within a run. Manager sorts before every other
// role so a run reads manager-first.
type Role string

const (
	// RoleManager is the run's manager; it has no parent label and sorts first.
	RoleManager Role = "manager"
	// RoleImplementer implements a task.
	RoleImplementer Role = "implementer"
	// RoleReviewer reviews a candidate.
	RoleReviewer Role = "reviewer"
)

// AgentDisplay is the HOP-owned presentation of one agent pane: the durable
// facts HOP wants shown in the native Agents view. It carries no Herdr wire
// shape; the adapter translates it into a metadata report.
type AgentDisplay struct {
	// PaneID is the pane whose metadata this describes.
	PaneID string
	// Run is the run's short label (the hop_run filter value).
	Run string
	// RunSequence orders runs relative to each other.
	RunSequence int
	// Role is the worker's role; a manager sorts first within its run.
	Role Role
	// WorkerSequence orders workers within a run after the manager.
	WorkerSequence int
	// Task is a short task summary; empty for a manager.
	Task string
	// ParentLabel is a readable parent label; empty for a manager.
	ParentLabel string
	// Account is the non-secret account label; empty when unassigned.
	Account string
	// State is the HOP workflow state, independent of native agent status.
	State string
}

// orderKey returns the deterministic manager-first ordering key for the
// display: a role rank (manager 0, others 1) followed by the worker
// sequence, both zero-padded so string comparison orders them numerically.
func (d *AgentDisplay) orderKey() string {
	rank := 1
	if d.Role == RoleManager {
		rank = 0
	}
	return fmt.Sprintf("%d%0*d", rank, orderKeyWidth, d.WorkerSequence)
}

// optionalFields pairs each optional token with the display value it renders,
// in a fixed order.
func (d *AgentDisplay) optionalFields() []struct {
	name  string
	value string
} {
	return []struct {
		name  string
		value string
	}{
		{FieldTask, d.Task},
		{FieldParent, d.ParentLabel},
		{FieldAccount, d.Account},
		{FieldState, d.State},
	}
}

// Tokens renders the tokens HOP writes for the display: the always-present
// required tokens plus every optional token that has a value. Empty optional
// fields are not written here; they are deleted through Cleared, because
// pane.report_metadata is a patch and an omitted key would otherwise keep its
// stale value.
func (d *AgentDisplay) Tokens() map[string]string {
	tokens := map[string]string{
		FieldRun:      d.Run,
		FieldRunOrder: fmt.Sprintf("%0*d", orderKeyWidth, d.RunSequence),
		FieldOrder:    d.orderKey(),
		FieldRole:     string(d.Role),
	}
	for _, field := range d.optionalFields() {
		if field.value != "" {
			tokens[field.name] = field.value
		}
	}
	return tokens
}

// Cleared returns the optional token keys that are unset for this display and
// so must be deleted from the pane's metadata, sorted for determinism.
// Publishing a full display is thus a complete replacement of HOP's optional
// tokens: a worker that loses its account or task has that token removed
// rather than left behind.
func (d *AgentDisplay) Cleared() []string {
	var cleared []string
	for _, field := range d.optionalFields() {
		if field.value == "" {
			cleared = append(cleared, field.name)
		}
	}
	sort.Strings(cleared)
	return cleared
}

// PaneMetadata is one metadata report the AgentPresentation port applies. It
// is the boundary value: the adapter turns it into a pane.report_metadata
// request under HOP's source. Tokens are written; Clear names token keys to
// delete. The adapter translates a delete into a JSON null, which Herdr
// treats as a removal, since its token map is a patch that leaves omitted
// keys untouched.
type PaneMetadata struct {
	PaneID string
	Tokens map[string]string
	Clear  []string
	// TTLMillis, when non-zero, expires the reported tokens after that many
	// milliseconds; use it for transient progress, not durable labels.
	TTLMillis int
}

// ViewSelection is one native Agents view projection HOP installs: a filter
// naming the run to show and the manager-first sort. It is deliberately a
// user action, not a per-update side effect, because one server-wide
// projection exists.
type ViewSelection struct {
	// Label is the human-readable view name shown in the sidebar.
	Label string
	// Run, when set, restricts the view to that run's agents.
	Run string
}

// AgentPresentation publishes HOP display metadata and selects or clears the
// native Agents view without changing domain state. The Herdr adapter
// implements it. View selection is server-wide and singleton per owner
// source, so callers serialize it rather than competing per worker update.
type AgentPresentation interface {
	// ReportMetadata applies one pane metadata patch under HOP's source. A
	// pane the server does not have, or that has no terminal to carry
	// metadata, is reported as an error wrapping ErrPaneNotFound; every
	// other error is an ordinary failure. Callers never read that error as
	// absence evidence for any lifecycle decision.
	ReportMetadata(ctx context.Context, metadata PaneMetadata) error
	// SelectView installs HOP's projection, replacing the previous view.
	SelectView(ctx context.Context, selection ViewSelection) error
	// ClearView clears the view only when HOP still owns it, leaving another
	// owner's projection intact.
	ClearView(ctx context.Context) error
}

// Presenter coordinates the presentation use case over the port. It owns the
// token and ordering conventions so adapters stay mechanical.
type Presenter struct {
	Presentation AgentPresentation
}

// Publish reports one agent's display metadata: it writes the current tokens
// and deletes the optional tokens that are now unset, so a full display is a
// complete replacement of HOP's tokens for the pane.
func (p *Presenter) Publish(ctx context.Context, display *AgentDisplay) error {
	return p.Presentation.ReportMetadata(ctx, PaneMetadata{
		PaneID: display.PaneID,
		Tokens: display.Tokens(),
		Clear:  display.Cleared(),
	})
}

// Focus installs the manager-first view for one run.
func (p *Presenter) Focus(ctx context.Context, run, label string) error {
	return p.Presentation.SelectView(ctx, ViewSelection{Label: label, Run: run})
}

// Clear removes HOP's view if HOP still owns it.
func (p *Presenter) Clear(ctx context.Context) error {
	return p.Presentation.ClearView(ctx)
}

// SortDisplays returns the displays in the order the native view renders them
// under HOP's projection: by run sequence, then manager-first within a run,
// then pane id for stability. It mirrors the token sort so tests and callers
// can predict the rendered order without a live server. The pane-id tie-break
// is a HOP-side determinism choice for equal ordering keys; Herdr's own view
// keeps incoming order for equal keys, so the two agree only up to that
// tie-break. Ordering assumes run and worker sequences in [0, 999999]; the
// six-digit padding does not order larger or negative values.
func SortDisplays(displays []AgentDisplay) []AgentDisplay {
	sorted := make([]AgentDisplay, len(displays))
	copy(sorted, displays)
	sort.SliceStable(sorted, func(i, j int) bool {
		a, b := sorted[i], sorted[j]
		if a.RunSequence != b.RunSequence {
			return a.RunSequence < b.RunSequence
		}
		if a.orderKey() != b.orderKey() {
			return a.orderKey() < b.orderKey()
		}
		return a.PaneID < b.PaneID
	})
	return sorted
}
