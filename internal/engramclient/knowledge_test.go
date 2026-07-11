package engramclient_test

import (
	"context"
	"strings"
	"testing"

	"github.com/ryanthedev/engram/internal/engramclient"
	"github.com/ryanthedev/engram/internal/mcp"
)

// TestDW_1_2_KnowledgeStubsReturnNotImplemented pins the Phase-1 stub
// posture: every knowledge method on the gRPC adapter fails loudly with a
// not-implemented error — and does so without dialing (a zero Client must
// not panic), so nothing can mistake the stubs for a working path before
// Phase 6 replaces them.
func TestDW_1_2_KnowledgeStubsReturnNotImplemented(t *testing.T) {
	ctx := context.Background()
	c := &engramclient.Client{}

	tests := []struct {
		name string
		call func() error
	}{
		{"KnowledgeIngest", func() error {
			_, err := c.KnowledgeIngest(ctx, "col", "src", "h1", []mcp.KnowledgeDoc{{ID: "d1", Text: "t"}})
			return err
		}},
		{"KnowledgeSearch", func() error {
			_, err := c.KnowledgeSearch(ctx, "col", "q", nil, nil, 5)
			return err
		}},
		{"KnowledgeCollections", func() error {
			_, err := c.KnowledgeCollections(ctx)
			return err
		}},
		{"KnowledgeDelete", func() error {
			_, err := c.KnowledgeDelete(ctx, "col", "src", "h1")
			return err
		}},
		{"CreateCollection", func() error {
			return c.CreateCollection(ctx, mcp.CollectionSpec{Name: "col"})
		}},
		{"UpdateCollection", func() error {
			return c.UpdateCollection(ctx, mcp.CollectionSpec{Name: "col"})
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.call()
			if err == nil {
				t.Fatalf("%s = nil error, want a loud not-implemented stub error", tt.name)
			}
			if !strings.Contains(err.Error(), "not implemented") || !strings.Contains(err.Error(), tt.name) {
				t.Errorf("%s error = %q, want it to name the op and say not implemented", tt.name, err)
			}
		})
	}
}
