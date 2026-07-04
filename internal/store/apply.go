package store

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// ApplyResult reports what one Apply run did per resource: "created" for new
// resources, "applied" for idempotent re-PUTs of definitions (templates and
// pipeline PUTs are upserts), "unchanged" for indices that already exist.
type ApplyResult struct {
	// Actions maps resource name → action taken.
	Actions map[string]string
	// Version is the cluster's reported version.number.
	Version string
}

// Changed reports whether the run created anything (false means the run was
// a pure no-op re-application — the DW-0.4 idempotency contract).
func (r ApplyResult) Changed() bool {
	for _, a := range r.Actions {
		if a == "created" {
			return true
		}
	}
	return false
}

// Apply idempotently applies Engram's cluster contract to the OpenSearch at
// baseURL: the episodic + semantic + extraction-ledger index templates, their
// concrete indices, and the RRF search pipeline. It refuses to touch any cluster whose version
// is not the pinned 3.1 line (D14) — one code path, no fallback. Re-running
// against an up-to-date cluster is a no-op (Changed() == false).
func Apply(ctx context.Context, client *http.Client, baseURL string) (ApplyResult, error) {
	if client == nil {
		client = http.DefaultClient
	}
	base := strings.TrimRight(baseURL, "/")
	res := ApplyResult{Actions: map[string]string{}}

	version, err := clusterVersion(ctx, client, base)
	if err != nil {
		return res, err
	}
	res.Version = version
	if !strings.HasPrefix(version, PinnedVersionPrefix) {
		return res, fmt.Errorf("store: cluster at %s runs OpenSearch %s but Engram is pinned to %sx exactly (D14); refusing to apply — point ENGRAM_OPENSEARCH_URL at a %sx cluster", base, version, PinnedVersionPrefix, PinnedVersionPrefix)
	}

	steps := []struct {
		name   string
		method string
		path   string
		body   []byte
		upsert bool // PUT is an idempotent upsert; report "applied"
	}{
		{EpisodicTemplateName, http.MethodPut, "/_index_template/" + EpisodicTemplateName, EpisodicTemplateJSON, true},
		{SemanticTemplateName, http.MethodPut, "/_index_template/" + SemanticTemplateName, SemanticTemplateJSON, true},
		{LedgerTemplateName, http.MethodPut, "/_index_template/" + LedgerTemplateName, LedgerTemplateJSON, true},
		{AuthTokenTemplateName, http.MethodPut, "/_index_template/" + AuthTokenTemplateName, AuthTokenTemplateJSON, true},
		{RRFPipelineName, http.MethodPut, "/_search/pipeline/" + RRFPipelineName, RRFPipelineJSON, true},
	}
	for _, s := range steps {
		if err := do(ctx, client, s.method, base+s.path, s.body); err != nil {
			return res, fmt.Errorf("store: applying %s: %w", s.name, err)
		}
		res.Actions[s.name] = "applied"
	}

	for _, idx := range []string{EpisodicIndex, SemanticIndex, LedgerIndex, AuthTokenIndex} {
		exists, err := indexExists(ctx, client, base, idx)
		if err != nil {
			return res, fmt.Errorf("store: checking index %s: %w", idx, err)
		}
		if exists {
			res.Actions[idx] = "unchanged"
			continue
		}
		switch err := do(ctx, client, http.MethodPut, base+"/"+idx, nil); {
		case err == nil:
			res.Actions[idx] = "created"
		case strings.Contains(err.Error(), "resource_already_exists_exception"):
			// A concurrent Apply won the exists-check/create race; the index
			// is there, which is all idempotency promises.
			res.Actions[idx] = "unchanged"
		default:
			return res, fmt.Errorf("store: creating index %s: %w", idx, err)
		}
	}
	return res, nil
}

// clusterVersion fetches version.number from the cluster root endpoint.
func clusterVersion(ctx context.Context, client *http.Client, base string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/", nil)
	if err != nil {
		return "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("store: cluster unreachable at %s: %w", base, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("store: cluster root returned %s", resp.Status)
	}
	var info struct {
		Version struct {
			Number string `json:"number"`
		} `json:"version"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return "", fmt.Errorf("store: decoding cluster info: %w", err)
	}
	if info.Version.Number == "" {
		return "", fmt.Errorf("store: cluster info has no version.number")
	}
	return info.Version.Number, nil
}

func indexExists(ctx context.Context, client *http.Client, base, index string) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, base+"/"+index, nil)
	if err != nil {
		return false, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		return true, nil
	case http.StatusNotFound:
		return false, nil
	default:
		return false, fmt.Errorf("HEAD %s returned %s", index, resp.Status)
	}
}

func do(ctx context.Context, client *http.Client, method, url string, body []byte) error {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rdr)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("%s %s returned %s: %s", method, url, resp.Status, string(b))
	}
	return nil
}

// DefaultTimeout is a sane default for apply-tool HTTP calls.
const DefaultTimeout = 30 * time.Second
