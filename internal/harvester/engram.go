// Package harvester implements Phase 1 of the engram-harvester tool.
package harvester

import (
	"context"
	"fmt"

	"github.com/ryanthedev/engram/internal/engramclient"
	"github.com/ryanthedev/engram/internal/mcp"
)

// EngramClient represents the client capability needed by the harvester.
type EngramClient interface {
	Collections(ctx context.Context) ([]mcp.CollectionInfo, error)
	Ingest(ctx context.Context, collection, source, harvestID string, docs []mcp.KnowledgeDoc) (int, error)
	Delete(ctx context.Context, collection, source, currentHarvestID string) (int, error)
}

type clientAdapter struct {
	client *engramclient.Client
}

var _ EngramClient = (*clientAdapter)(nil)

// DialEngram connects to the engram server and returns an EngramClient interface.
func DialEngram(addr, token string) (EngramClient, error) {
	client, err := engramclient.Dial(addr, token)
	if err != nil {
		return nil, fmt.Errorf("harvester: dialing engram: %w", err)
	}
	return &clientAdapter{client: client}, nil
}

// Collections delegates to the underlying client's KnowledgeCollections method.
func (a *clientAdapter) Collections(ctx context.Context) ([]mcp.CollectionInfo, error) {
	infos, err := a.client.KnowledgeCollections(ctx)
	if err != nil {
		return nil, fmt.Errorf("harvester: collections: %w", err)
	}
	return infos, nil
}

// Ingest delegates to the underlying client's KnowledgeIngest method.
func (a *clientAdapter) Ingest(ctx context.Context, collection, source, harvestID string, docs []mcp.KnowledgeDoc) (int, error) {
	n, err := a.client.KnowledgeIngest(ctx, collection, source, harvestID, docs)
	if err != nil {
		return 0, fmt.Errorf("harvester: ingest: %w", err)
	}
	return n, nil
}

// Delete delegates to the underlying client's KnowledgeDelete method.
func (a *clientAdapter) Delete(ctx context.Context, collection, source, currentHarvestID string) (int, error) {
	n, err := a.client.KnowledgeDelete(ctx, collection, source, currentHarvestID)
	if err != nil {
		return 0, fmt.Errorf("harvester: delete: %w", err)
	}
	return n, nil
}

// Close releases the underlying gRPC connection.
func (a *clientAdapter) Close() error {
	return a.client.Close()
}
