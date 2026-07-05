// Package reindex implements the Phase-7 rollback contract's "point of no
// return" mitigation: a breaking index-template/mapping change is never
// applied in place. Instead, a new versioned concrete index is created under
// the current (possibly changed) template, and a search alias is flipped
// from the old index to the new one atomically — there is never a moment
// with zero or two authoritative indices behind the alias, and the OLD
// index's mapping is never touched by a migration.
//
// This package is deploy-time tooling, not a runtime dependency of
// internal/store: it operates over any OpenSearch base URL (local podman or
// a real OpenSearch Service domain) via the same HTTP primitives
// internal/store already uses, and is exercised for real against the local
// 3.1 cluster (integration tag). Wiring the live retrieval path to resolve
// indices through an alias instead of a fixed concrete name is a follow-on
// migration this package's mechanism enables but does not itself perform —
// see docs/runbooks/index-template-migration.md for the operator procedure.
package reindex

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// EnsureAliasedIndex creates a versioned concrete index named
// "<alias>-<version>" (zero-padded to 6 digits, matching the skeleton's
// existing "-000001" convention) from templateJSON's mappings/settings, and
// — only if alias does not already point anywhere — points alias at it. It
// is idempotent: calling it again with the same alias/version when the index
// already exists is a no-op. Migrating an EXISTING alias to a new version is
// FlipAlias's job, not this constructor's.
func EnsureAliasedIndex(ctx context.Context, client *http.Client, baseURL, alias string, version int, templateJSON []byte) (indexName string, created bool, err error) {
	indexName = versionedName(alias, version)
	base := strings.TrimRight(baseURL, "/")

	exists, err := indexExists(ctx, client, base, indexName)
	if err != nil {
		return indexName, false, err
	}
	if !exists {
		if err := createIndex(ctx, client, base, indexName, templateJSON); err != nil {
			return indexName, false, err
		}
		created = true
	}

	target, err := CurrentAliasTarget(ctx, client, base, alias)
	if err != nil {
		return indexName, created, err
	}
	if target == "" {
		if err := putAliasAction(ctx, client, base, map[string]any{
			"actions": []any{map[string]any{"add": map[string]any{"index": indexName, "alias": alias}}},
		}); err != nil {
			return indexName, created, fmt.Errorf("reindex: pointing alias %s at %s: %w", alias, indexName, err)
		}
	}
	return indexName, created, nil
}

// FlipAlias atomically repoints alias from fromIndex to toIndex using
// OpenSearch's multi-action _aliases API: the remove and add are one
// request, so a concurrent reader never observes the alias resolving to
// zero or two indices. It never issues a mapping PUT to fromIndex — the
// "never in-place mapping mutation" constraint is structural here, not just
// a convention: this function has no code path that touches fromIndex's
// mapping at all.
func FlipAlias(ctx context.Context, client *http.Client, baseURL, alias, fromIndex, toIndex string) error {
	base := strings.TrimRight(baseURL, "/")
	body := map[string]any{
		"actions": []any{
			map[string]any{"remove": map[string]any{"index": fromIndex, "alias": alias}},
			map[string]any{"add": map[string]any{"index": toIndex, "alias": alias}},
		},
	}
	if err := putAliasAction(ctx, client, base, body); err != nil {
		return fmt.Errorf("reindex: flipping alias %s from %s to %s: %w", alias, fromIndex, toIndex, err)
	}
	return nil
}

// CurrentAliasTarget returns the single concrete index alias currently
// resolves to, or "" if the alias does not exist yet. It errors if the alias
// resolves to more than one index — Engram's alias contract is always
// exactly one authoritative index per alias, so ambiguity is a bug, not a
// case to silently pick from.
func CurrentAliasTarget(ctx context.Context, client *http.Client, baseURL, alias string) (string, error) {
	base := strings.TrimRight(baseURL, "/")
	status, decoded, err := doJSON(ctx, client, http.MethodGet, base+"/_alias/"+alias, nil)
	if err != nil {
		return "", fmt.Errorf("reindex: reading alias %s: %w", alias, err)
	}
	if status == http.StatusNotFound {
		return "", nil
	}
	if status != http.StatusOK {
		return "", fmt.Errorf("reindex: reading alias %s: unexpected status %d: %v", alias, status, decoded)
	}
	if len(decoded) == 0 {
		return "", nil
	}
	if len(decoded) > 1 {
		return "", fmt.Errorf("reindex: alias %s resolves to %d indices, want exactly 1", alias, len(decoded))
	}
	for name := range decoded {
		return name, nil
	}
	return "", nil
}

func versionedName(alias string, version int) string {
	return fmt.Sprintf("%s-%06d", alias, version)
}

func indexExists(ctx context.Context, client *http.Client, base, index string) (bool, error) {
	status, _, err := doJSON(ctx, client, http.MethodHead, base+"/"+index, nil)
	if err != nil {
		return false, fmt.Errorf("reindex: checking index %s: %w", index, err)
	}
	return status == http.StatusOK, nil
}

// createIndex PUTs templateJSON's mappings/settings block directly onto a
// brand-new index. Engram's checked-in templates (internal/store/templates)
// are shaped as {"index_patterns":[...], "template": {"settings":...,
// "mappings":...}} — the same "template" object OpenSearch would apply
// automatically via the index_patterns match; PUTting it explicitly here
// makes the new index's mapping independent of whatever the currently
// applied template happens to say, which is the point: EnsureAliasedIndex
// creates the version the caller asked for, not whatever the live template
// resolves to at PUT time.
func createIndex(ctx context.Context, client *http.Client, base, index string, templateJSON []byte) error {
	var wrapper struct {
		Template struct {
			Settings any `json:"settings"`
			Mappings any `json:"mappings"`
		} `json:"template"`
	}
	if err := json.Unmarshal(templateJSON, &wrapper); err != nil {
		return fmt.Errorf("reindex: parsing template for %s: %w", index, err)
	}
	body, err := json.Marshal(map[string]any{
		"settings": wrapper.Template.Settings,
		"mappings": wrapper.Template.Mappings,
	})
	if err != nil {
		return fmt.Errorf("reindex: encoding create-index body for %s: %w", index, err)
	}
	status, decoded, err := doJSON(ctx, client, http.MethodPut, base+"/"+index, body)
	if err != nil {
		return fmt.Errorf("reindex: creating index %s: %w", index, err)
	}
	if status != http.StatusOK {
		return fmt.Errorf("reindex: creating index %s: unexpected status %d: %v", index, status, decoded)
	}
	return nil
}

func putAliasAction(ctx context.Context, client *http.Client, base string, body map[string]any) error {
	encoded, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("reindex: encoding alias action: %w", err)
	}
	status, decoded, err := doJSON(ctx, client, http.MethodPost, base+"/_aliases", encoded)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("reindex: alias action: unexpected status %d: %v", status, decoded)
	}
	return nil
}

// doJSON issues one JSON request and returns the status code and decoded
// body — the same small HTTP primitive internal/store uses, kept local here
// so this package has no dependency on internal/store (it operates over any
// OpenSearch endpoint, local or AWS-managed).
func doJSON(ctx context.Context, client *http.Client, method, url string, body []byte) (int, map[string]any, error) {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rdr)
	if err != nil {
		return 0, nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, nil, err
	}
	var decoded map[string]any
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &decoded)
	}
	return resp.StatusCode, decoded, nil
}
