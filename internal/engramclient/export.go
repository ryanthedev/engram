package engramclient

// Plain-struct view of the Export RPC page, mirroring how Audit adapts proto
// types into AuditResult/FactVersion: the generated engrampb types stay
// inside this allowlisted transport edge (internal/importlint boundary), and
// CLI code consumes transport-free records.

import (
	"context"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"
)

// ExportEntity is one live, deduplicated graph entity from an export page —
// the record the CLI renders into an Obsidian note.
type ExportEntity struct {
	ID           string
	Name         string
	Aliases      []string
	MentionCount int64
	SourceIDs    []string
	Scope        string
	TeamID       string
	OwnerAgentID string
	ValidAt      *time.Time
	CreatedAt    *time.Time
}

// ExportEdge is one live, predicate-typed relation from an export page; the
// endpoints are ExportEntity.ID values.
type ExportEdge struct {
	ID           string
	FromEntityID string
	ToEntityID   string
	Predicate    string
	Statement    string
	SourceIDs    []string
	Scope        string
	TeamID       string
	OwnerAgentID string
	ValidAt      *time.Time
	CreatedAt    *time.Time
}

// ExportEpisodic is one processed, non-dead-lettered episodic record from an
// export page — the raw event prose the CLI renders into an event note and
// joins to entities/edges via their SourceIDs → EventID.
type ExportEpisodic struct {
	EventID    string
	Kind       string
	Text       string
	OccurredAt *time.Time
	SourceIDs  []string
}

// ExportPage is one bounded page of the caller's tenant-scoped memory —
// episodic records plus the live graph. An empty NextCursor means the export
// is complete.
type ExportPage struct {
	Episodics  []ExportEpisodic
	Entities   []ExportEntity
	Edges      []ExportEdge
	NextCursor string
}

// ExportPage fetches one Export page and adapts it into plain records (the
// transport-free view of Export; same paging contract).
func (c *Client) ExportPage(ctx context.Context, cursor string) (ExportPage, error) {
	resp, err := c.Export(ctx, cursor)
	if err != nil {
		return ExportPage{}, err
	}
	page := ExportPage{NextCursor: resp.GetNextCursor()}
	for _, ep := range resp.GetEpisodics() {
		page.Episodics = append(page.Episodics, ExportEpisodic{
			EventID: ep.GetEventId(), Kind: ep.GetKind(), Text: ep.GetText(),
			OccurredAt: tsPtr(ep.GetOccurredAt()), SourceIDs: ep.GetSourceIds(),
		})
	}
	for _, e := range resp.GetEntities() {
		page.Entities = append(page.Entities, ExportEntity{
			ID: e.GetId(), Name: e.GetName(), Aliases: e.GetAliases(),
			MentionCount: e.GetMentionCount(), SourceIDs: e.GetSourceIds(),
			Scope: e.GetScope(), TeamID: e.GetTeamId(), OwnerAgentID: e.GetOwnerAgentId(),
			ValidAt: tsPtr(e.GetValidAt()), CreatedAt: tsPtr(e.GetCreatedAt()),
		})
	}
	for _, ed := range resp.GetEdges() {
		page.Edges = append(page.Edges, ExportEdge{
			ID: ed.GetId(), FromEntityID: ed.GetFromEntityId(), ToEntityID: ed.GetToEntityId(),
			Predicate: ed.GetPredicate(), Statement: ed.GetStatement(), SourceIDs: ed.GetSourceIds(),
			Scope: ed.GetScope(), TeamID: ed.GetTeamId(), OwnerAgentID: ed.GetOwnerAgentId(),
			ValidAt: tsPtr(ed.GetValidAt()), CreatedAt: tsPtr(ed.GetCreatedAt()),
		})
	}
	return page, nil
}

// tsPtr converts an optional proto timestamp to *time.Time (nil when unset).
func tsPtr(ts *timestamppb.Timestamp) *time.Time {
	if ts == nil {
		return nil
	}
	t := ts.AsTime()
	return &t
}
