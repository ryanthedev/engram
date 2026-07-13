package graph

import (
	"context"
	"fmt"
	"net/http"
	"strings"
)

// OpenSearchIndexDropper is the production IndexDropper: it hard-deletes
// the two T4 indices and recreates them via Apply (which also reapplies
// the templates, matching engram-apply-templates' idempotent-upsert
// idiom). DELETE tolerates the index already being absent — a fresh
// store, or a rebuild interrupted after the drop but before the recreate
// — both are legitimate starting states, not errors (mirrors Apply's own
// tolerance of resource_already_exists_exception on the way back up).
//
// This never touches the episodic or semantic indices: it only ever
// addresses EntityIndex/EdgeIndex by name, and Apply only ever PUTs the
// graph templates/indices — there is no code path here that can reach
// another tier (DW-3.4).
type OpenSearchIndexDropper struct {
	client  *http.Client
	baseURL string
}

var _ IndexDropper = (*OpenSearchIndexDropper)(nil)

// NewOpenSearchIndexDropper returns an IndexDropper over the cluster at
// baseURL. client must not be nil.
func NewOpenSearchIndexDropper(client *http.Client, baseURL string) *OpenSearchIndexDropper {
	return &OpenSearchIndexDropper{client: client, baseURL: strings.TrimRight(baseURL, "/")}
}

// DropAndRecreate implements IndexDropper.
func (d *OpenSearchIndexDropper) DropAndRecreate(ctx context.Context) error {
	for _, idx := range []string{EntityIndex, EdgeIndex} {
		status, decoded, err := osJSON(ctx, d.client, http.MethodDelete, d.baseURL+"/"+idx, nil)
		if err != nil {
			return fmt.Errorf("graph: dropping index %s: %w", idx, err)
		}
		if status != http.StatusOK && !isIndexNotFound(status, decoded) {
			return fmt.Errorf("graph: dropping index %s: unexpected status %d: %v", idx, status, decoded)
		}
	}
	if err := Apply(ctx, d.client, d.baseURL); err != nil {
		return fmt.Errorf("graph: recreating graph indices: %w", err)
	}
	return nil
}
