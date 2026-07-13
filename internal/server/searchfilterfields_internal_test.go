package server

// This file is package server (not server_test) on purpose: searchFilterFields
// is unexported, and the whole point of this test is to pin the hand-written
// string to the proto's actual fields without a caller having to export it
// first.

import (
	"sort"
	"strings"
	"testing"

	"github.com/ryanthedev/engram/api/engrampb"
)

// searchRequestNonFilterFields are the SearchRequest fields that are NOT part
// of the caller-facing filter vocabulary: query text, result size, and the
// tenancy scope pinned from the verified identity (never a caller-supplied
// filter). Everything else on the message is a filter field and must be named
// in searchFilterFields.
var searchRequestNonFilterFields = map[string]bool{
	"query": true, "k": true, "tenant_id": true, "user_id": true,
}

// TestSearchFilterFieldsMatchesProtoFilterFields pins the hand-written
// searchFilterFields string to the proto's actual SearchRequest fields. A
// proto field added, renamed, or removed without updating this string would
// otherwise go unnoticed until a caller hit an error message that silently
// omitted or misnamed a valid filter — exactly the failure invalidFilter
// exists to prevent.
func TestSearchFilterFieldsMatchesProtoFilterFields(t *testing.T) {
	fields := (&engrampb.SearchRequest{}).ProtoReflect().Descriptor().Fields()
	var fromProto []string
	for i := 0; i < fields.Len(); i++ {
		name := string(fields.Get(i).Name())
		if !searchRequestNonFilterFields[name] {
			fromProto = append(fromProto, name)
		}
	}
	sort.Strings(fromProto)

	fromConst := strings.Split(searchFilterFields, ", ")
	sort.Strings(fromConst)

	if strings.Join(fromProto, ",") != strings.Join(fromConst, ",") {
		t.Errorf("searchFilterFields has drifted from the proto's SearchRequest filter fields:\n"+
			"  proto:            %v\n"+
			"  searchFilterFields: %v", fromProto, fromConst)
	}
}
