package store

import "testing"

// TestIsIndexNotFound pins the 404-as-empty detector to EXACTLY the OpenSearch
// index_not_found_exception shape (HTTP 404 with that error.type) and nothing
// else — the belt behind DW-1.1/DW-1.4. A different 404 type, any non-404
// status, or a transport failure (nil decoded) must NOT be mistaken for a
// missing index, so a real fault still surfaces as an error at the call site.
func TestIsIndexNotFound(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		decoded map[string]any
		want    bool
	}{
		{"missing index 404", 404, map[string]any{"error": map[string]any{"type": "index_not_found_exception"}}, true},
		{"different 404 type", 404, map[string]any{"error": map[string]any{"type": "resource_already_exists_exception"}}, false},
		{"index_not_found type but 500 status", 500, map[string]any{"error": map[string]any{"type": "index_not_found_exception"}}, false},
		{"transport failure, nil decoded", 0, nil, false},
		{"200 ok", 200, map[string]any{}, false},
		{"404 with no error object", 404, map[string]any{}, false},
		{"404 error not a map", 404, map[string]any{"error": "boom"}, false},
		{"404 error map without type", 404, map[string]any{"error": map[string]any{"reason": "x"}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isIndexNotFound(tc.status, tc.decoded); got != tc.want {
				t.Errorf("isIndexNotFound(%d, %v) = %v, want %v", tc.status, tc.decoded, got, tc.want)
			}
		})
	}
}
