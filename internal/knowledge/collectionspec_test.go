package knowledge_test

import (
	"testing"

	"github.com/ryanthedev/engram/internal/knowledge"
)

// TestDW_1_2_FragmentSizingFallback pins the global-fallback rule: a
// collection with absent (zero — the proto default) or nonsensical
// (negative) sizing resolves to 240/3, never to a zero-size or zero-count
// fragment; explicitly set values pass through untouched, each knob falling
// back independently.
func TestDW_1_2_FragmentSizingFallback(t *testing.T) {
	cases := []struct {
		name                string
		size, count         int
		wantSize, wantCount int
	}{
		{name: "both absent (pre-knob collection)", wantSize: 240, wantCount: 3},
		{name: "both set", size: 512, count: 5, wantSize: 512, wantCount: 5},
		{name: "size set, count absent", size: 100, wantSize: 100, wantCount: 3},
		{name: "count set, size absent", count: 7, wantSize: 240, wantCount: 7},
		{name: "negative values fall back, not zero-size", size: -1, count: -3, wantSize: 240, wantCount: 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec := knowledge.CollectionSpec{Name: "docs", FragmentSize: tc.size, NumberOfFragments: tc.count}
			size, count := spec.FragmentSizing()
			if size != tc.wantSize || count != tc.wantCount {
				t.Errorf("FragmentSizing() = (%d, %d), want (%d, %d)", size, count, tc.wantSize, tc.wantCount)
			}
		})
	}
	if knowledge.DefaultFragmentSize != 240 || knowledge.DefaultNumberOfFragments != 3 {
		t.Errorf("global defaults = (%d, %d), want (240, 3)",
			knowledge.DefaultFragmentSize, knowledge.DefaultNumberOfFragments)
	}
}
