package herdr

import (
	"context"
	"fmt"

	"github.com/johnlanda/hop/internal/app"
)

// Presentation implements app.AgentPresentation over one server socket. It
// publishes metadata and selects or clears the native Agents view under
// HOP's fixed reporter source, so its patches and view are attributable and
// clearable without disturbing another owner.
type Presentation struct {
	client *Client
}

var _ app.AgentPresentation = (*Presentation)(nil)

// NewPresentation builds the presentation adapter for a server socket.
func NewPresentation(socketPath string) *Presentation {
	return &Presentation{client: NewClient(socketPath)}
}

// paneReportMetadataParams is the wire shape of pane.report_metadata. Only
// the token patch is used here; presentation guards and TTL are set when
// present. A nil-valued token clears that key, so the map carries strings.
type paneReportMetadataParams struct {
	PaneID string            `json:"pane_id"`
	Source string            `json:"source"`
	Tokens map[string]string `json:"tokens,omitempty"`
	TTLMs  int               `json:"ttl_ms,omitempty"`
}

// ReportMetadata applies one pane metadata token patch under HOP's source.
func (p *Presentation) ReportMetadata(ctx context.Context, metadata app.PaneMetadata) error {
	params := paneReportMetadataParams{
		PaneID: metadata.PaneID,
		Source: app.PresentationSource,
		Tokens: metadata.Tokens,
		TTLMs:  metadata.TTLMillis,
	}
	if err := p.client.Call(ctx, "pane.report_metadata", params, nil); err != nil {
		return fmt.Errorf("report metadata for pane %s: %w", metadata.PaneID, err)
	}
	return nil
}

// agentViewFilter is a token-equality filter node: it selects agents whose
// pane metadata token equals a value.
type agentViewFilter struct {
	Op    string         `json:"op"`
	Field agentViewField `json:"field"`
	Value string         `json:"value"`
}

// agentViewField names a plugin-reported pane metadata token as a filter or
// sort field.
type agentViewField struct {
	Token string `json:"token"`
}

// agentViewSort orders the projection by a token, ascending or descending.
type agentViewSort struct {
	Field agentViewField `json:"field"`
	Order string         `json:"order"`
}

// agentViewSetParams is the wire shape of agent.view.set.
type agentViewSetParams struct {
	Source string           `json:"source"`
	Label  string           `json:"label,omitempty"`
	Filter *agentViewFilter `json:"filter,omitempty"`
	Sort   []agentViewSort  `json:"sort,omitempty"`
}

// SelectView installs HOP's manager-first projection, filtered to one run
// when the selection names one. The sort is by the padded hop_order token so
// the manager sorts first and workers follow in sequence.
func (p *Presentation) SelectView(ctx context.Context, selection app.ViewSelection) error {
	params := agentViewSetParams{
		Source: app.PresentationSource,
		Label:  selection.Label,
		Sort: []agentViewSort{
			{Field: agentViewField{Token: app.FieldRunOrder}, Order: "asc"},
			{Field: agentViewField{Token: app.FieldOrder}, Order: "asc"},
		},
	}
	if selection.Run != "" {
		params.Filter = &agentViewFilter{
			Op:    "eq",
			Field: agentViewField{Token: app.FieldRun},
			Value: selection.Run,
		}
	}
	if err := p.client.Call(ctx, "agent.view.set", params, nil); err != nil {
		return fmt.Errorf("select agent view: %w", err)
	}
	return nil
}

// agentViewClearParams clears the view only when the named source still owns
// it, so another owner's projection stays intact.
type agentViewClearParams struct {
	Source string `json:"source"`
}

// ClearView clears the native view only when HOP still owns it.
func (p *Presentation) ClearView(ctx context.Context) error {
	if err := p.client.Call(ctx, "agent.view.clear", agentViewClearParams{Source: app.PresentationSource}, nil); err != nil {
		return fmt.Errorf("clear agent view: %w", err)
	}
	return nil
}
