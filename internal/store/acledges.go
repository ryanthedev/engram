package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/ryanthedev/engram/internal/acl"
	"github.com/ryanthedev/engram/internal/auth"
)

// ACLEdgeStore is the OpenSearch-backed acl.EdgeSource: the acl_edges index
// holding user↔agent, membership, and org-grant edges. It is the reachability
// source the query-time ACL (internal/acl) reads on every request — no cache,
// so an edge deletion revokes on the next call (D6, ≤5 s). Each edge has a
// deterministic id (sha256 of its identifying fields) so grant is an idempotent
// upsert and revoke is a point delete.
type ACLEdgeStore struct {
	client  *http.Client
	baseURL string
	index   string
}

var _ acl.EdgeSource = (*ACLEdgeStore)(nil)

// ACLEdgeOption configures an ACLEdgeStore.
type ACLEdgeOption func(*ACLEdgeStore)

// WithACLEdgesIndex overrides the edges index (tests use a scratch index).
func WithACLEdgesIndex(name string) ACLEdgeOption {
	return func(s *ACLEdgeStore) { s.index = name }
}

// NewACLEdgeStore returns an ACLEdgeStore over the cluster at baseURL. client
// must not be nil.
func NewACLEdgeStore(client *http.Client, baseURL string, opts ...ACLEdgeOption) *ACLEdgeStore {
	s := &ACLEdgeStore{client: client, baseURL: strings.TrimRight(baseURL, "/"), index: ACLEdgesIndex}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// edgeDoc is the persisted shape of an edge (adds created_at for audit).
type edgeDoc struct {
	Type      acl.EdgeType `json:"type"`
	TenantID  string       `json:"tenant_id"`
	UserID    string       `json:"user_id"`
	AgentID   string       `json:"agent_id,omitempty"`
	TeamID    string       `json:"team_id,omitempty"`
	CreatedAt time.Time    `json:"created_at"`
}

// edgeID is the deterministic doc _id for an edge: identical grants collapse to
// one doc (idempotent grant) and revoke can delete by id without a search.
func edgeID(e acl.Edge) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{
		string(e.Type), e.TenantID, e.UserID, e.AgentID, e.TeamID,
	}, "\x1f")))
	return hex.EncodeToString(sum[:])
}

// PutEdge validates and upserts an edge. A malformed edge is rejected with
// acl.ErrMalformedEdge BEFORE any write (fail-closed — the acl_edges index is
// an authorization input and must never hold garbage that could widen access).
func (s *ACLEdgeStore) PutEdge(ctx context.Context, e acl.Edge) error {
	if err := e.Validate(); err != nil {
		return err
	}
	body, err := json.Marshal(edgeDoc{
		Type: e.Type, TenantID: e.TenantID, UserID: e.UserID,
		AgentID: e.AgentID, TeamID: e.TeamID, CreatedAt: time.Now().UTC(),
	})
	if err != nil {
		return fmt.Errorf("store: encoding acl edge: %w", err)
	}
	url := fmt.Sprintf("%s/%s/_doc/%s?refresh=true", s.baseURL, s.index, edgeID(e))
	status, decoded, err := doJSON(ctx, s.client, http.MethodPut, url, body)
	if err != nil {
		return fmt.Errorf("store: storing acl edge: %w", err)
	}
	if status != http.StatusCreated && status != http.StatusOK {
		return fmt.Errorf("store: storing acl edge: unexpected status %d: %v", status, decoded)
	}
	return nil
}

// DeleteEdge removes an edge (revocation). found reports whether it existed;
// deleting a missing edge is not an error (idempotent revoke).
func (s *ACLEdgeStore) DeleteEdge(ctx context.Context, e acl.Edge) (found bool, err error) {
	url := fmt.Sprintf("%s/%s/_doc/%s?refresh=true", s.baseURL, s.index, edgeID(e))
	status, decoded, err := doJSON(ctx, s.client, http.MethodDelete, url, nil)
	if err != nil {
		return false, fmt.Errorf("store: deleting acl edge: %w", err)
	}
	switch status {
	case http.StatusOK:
		return true, nil
	case http.StatusNotFound:
		return false, nil
	default:
		return false, fmt.Errorf("store: deleting acl edge: unexpected status %d: %v", status, decoded)
	}
}

// ListEdges returns every edge for (tenant, user) — the `engram acl list`
// surface and the input Reachability aggregates.
func (s *ACLEdgeStore) ListEdges(ctx context.Context, id auth.Identity) ([]acl.Edge, error) {
	docs, err := s.query(ctx, id.TenantID, id.UserID)
	if err != nil {
		return nil, err
	}
	out := make([]acl.Edge, 0, len(docs))
	for _, d := range docs {
		out = append(out, acl.Edge{Type: d.Type, TenantID: d.TenantID, UserID: d.UserID, AgentID: d.AgentID, TeamID: d.TeamID})
	}
	return out, nil
}

// Reachability implements acl.EdgeSource: it reads the user's edges fresh and
// folds them into a Reach. The identity's own agent is always reachable (self);
// user_agent edges add other agents; member edges add teams; an org_grant edge
// sets OrgGrant. A blank agent with no edges yields an empty Reach (deny-all).
func (s *ACLEdgeStore) Reachability(ctx context.Context, id auth.Identity) (acl.Reach, error) {
	docs, err := s.query(ctx, id.TenantID, id.UserID)
	if err != nil {
		return acl.Reach{}, err
	}
	var reach acl.Reach
	if id.AgentID != "" {
		reach.Agents = append(reach.Agents, id.AgentID)
	}
	for _, d := range docs {
		switch d.Type {
		case acl.EdgeUserAgent:
			if d.AgentID != "" && !containsStr(reach.Agents, d.AgentID) {
				reach.Agents = append(reach.Agents, d.AgentID)
			}
		case acl.EdgeMember:
			if d.TeamID != "" && !containsStr(reach.Teams, d.TeamID) {
				reach.Teams = append(reach.Teams, d.TeamID)
			}
		case acl.EdgeOrgGrant:
			reach.OrgGrant = true
		}
	}
	return reach, nil
}

// query returns every edge doc for (tenant, user).
func (s *ACLEdgeStore) query(ctx context.Context, tenantID, userID string) ([]edgeDoc, error) {
	if tenantID == "" || userID == "" {
		return nil, nil
	}
	q := map[string]any{
		"size": 1000,
		"query": map[string]any{"bool": map[string]any{"filter": []any{
			map[string]any{"term": map[string]any{"tenant_id": tenantID}},
			map[string]any{"term": map[string]any{"user_id": userID}},
		}}},
	}
	body, err := json.Marshal(q)
	if err != nil {
		return nil, fmt.Errorf("store: encoding acl edge query: %w", err)
	}
	status, decoded, err := doJSON(ctx, s.client, http.MethodPost, s.baseURL+"/"+s.index+"/_search", body)
	if err != nil {
		return nil, fmt.Errorf("store: reading acl edges: %w", err)
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("store: reading acl edges: unexpected status %d: %v", status, decoded)
	}
	var out []edgeDoc
	for _, hit := range searchHits(decoded) {
		var d edgeDoc
		if err := decodeSource(hit, &d); err != nil {
			return nil, fmt.Errorf("store: decoding acl edge: %w", err)
		}
		out = append(out, d)
	}
	return out, nil
}

func containsStr(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}
