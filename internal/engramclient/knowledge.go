// Knowledge Backend stubs (knowledge platform Phase 1). The proto contract
// for the six knowledge operations is frozen, but the client wiring lands in
// Phase 6 — until then every method fails loudly instead of dialing gRPC, so
// the repo compiles and misuse is unmistakable. Phase 6 replaces these bodies
// with real engrampb.Knowledge* calls.

package engramclient

import (
	"context"
	"fmt"

	"github.com/ryanthedev/engram/internal/mcp"
)

// errKnowledgeUnimplemented is the shared Phase-1 stub error.
func errKnowledgeUnimplemented(op string) error {
	return fmt.Errorf("engramclient: %s: not implemented (knowledge platform Phase 6)", op)
}

// KnowledgeIngest is a Phase-1 stub; the real bulk-upsert call lands in Phase 6.
func (c *Client) KnowledgeIngest(_ context.Context, _, _, _ string, _ []mcp.KnowledgeDoc) (int, error) {
	return 0, errKnowledgeUnimplemented("KnowledgeIngest")
}

// KnowledgeSearch is a Phase-1 stub; the real BM25 search call lands in Phase 6.
func (c *Client) KnowledgeSearch(_ context.Context, _, _ string, _ []mcp.Predicate, _ []mcp.SortKey, _ int) ([]mcp.Hit, error) {
	return nil, errKnowledgeUnimplemented("KnowledgeSearch")
}

// KnowledgeCollections is a Phase-1 stub; the real listing call lands in Phase 6.
func (c *Client) KnowledgeCollections(_ context.Context) ([]mcp.CollectionInfo, error) {
	return nil, errKnowledgeUnimplemented("KnowledgeCollections")
}

// KnowledgeDelete is a Phase-1 stub; the real mark-and-sweep call lands in Phase 6.
func (c *Client) KnowledgeDelete(_ context.Context, _, _, _ string) (int, error) {
	return 0, errKnowledgeUnimplemented("KnowledgeDelete")
}

// CreateCollection is a Phase-1 stub; the real registry call lands in Phase 6.
func (c *Client) CreateCollection(_ context.Context, _ mcp.CollectionSpec) error {
	return errKnowledgeUnimplemented("CreateCollection")
}

// UpdateCollection is a Phase-1 stub; the real registry call lands in Phase 6.
func (c *Client) UpdateCollection(_ context.Context, _ mcp.CollectionSpec) error {
	return errKnowledgeUnimplemented("UpdateCollection")
}
