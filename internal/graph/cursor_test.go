package graph

import (
	"encoding/json"
	"testing"
)

// TestCursorTextRoundTrip: MarshalText/UnmarshalText round-trip a cursor
// exactly — including through encoding/json, which is how the export RPC's
// wire cursor embeds it — and the zero cursor stays zero.
func TestCursorTextRoundTrip(t *testing.T) {
	for _, c := range []Cursor{{}, {after: "deadbeef"}} {
		b, err := c.MarshalText()
		if err != nil {
			t.Fatalf("MarshalText(%+v): %v", c, err)
		}
		var got Cursor
		if err := got.UnmarshalText(b); err != nil {
			t.Fatalf("UnmarshalText(%q): %v", b, err)
		}
		if got != c {
			t.Errorf("round-trip = %+v, want %+v", got, c)
		}
	}

	// Through JSON (the wire-cursor envelope shape).
	type envelope struct {
		After Cursor `json:"a"`
	}
	in := envelope{After: Cursor{after: "abc123"}}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	var out envelope
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	if out.After != in.After {
		t.Errorf("json round-trip = %+v, want %+v", out.After, in.After)
	}
}

// TestCursorUnmarshalGarbageIsHarmless: any byte string is accepted (the
// documented contract) and merely repositions a scan within whatever tenant
// the caller passes — verified here by scanning: a garbage cursor exhausts
// or pages within the tenant, never errors.
func TestCursorUnmarshalGarbageIsHarmless(t *testing.T) {
	var c Cursor
	if err := c.UnmarshalText([]byte("\x00garbage\xff")); err != nil {
		t.Fatalf("UnmarshalText(garbage): %v", err)
	}
	b := NewMemBackend()
	if err := b.PutEntity(t.Context(), mkEntity("aaa", "t1")); err != nil {
		t.Fatal(err)
	}
	items, next, err := b.ScanEntities(t.Context(), "t1", c)
	if err != nil {
		t.Fatalf("ScanEntities(garbage cursor): %v", err)
	}
	// "\x00garbage\xff" sorts after "" and before "aaa" only per byte order;
	// whatever the position, the invariant is: no error, and only t1 records.
	for _, e := range items {
		if e.TenantID != "t1" {
			t.Fatalf("garbage cursor surfaced foreign-tenant entity %+v", e)
		}
	}
	_ = next
}
