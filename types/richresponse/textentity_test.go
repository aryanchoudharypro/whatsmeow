// Copyright (c) 2026 Tulir Asokan
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package richresponse

import (
	"encoding/json"
	"testing"
)

// A metadata type name present in textEntityMetadataTypes used to make
// TextEntity.UnmarshalJSON recurse into itself on the same bytes forever,
// crashing the whole process with a stack overflow instead of returning an
// error. This must return quickly with the entity decoded.
func TestTextEntityUnmarshalJSON_KnownType(t *testing.T) {
	data := []byte(`{"key":"abc","metadata":{"__typename":"GenAIInlineLinkItem","url":"https://example.com","display_name":"Example"}}`)

	var te TextEntity
	if err := json.Unmarshal(data, &te); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}
	if te.Key != "abc" {
		t.Errorf("Key = %q, want %q", te.Key, "abc")
	}
	link, ok := te.Metadata.(*GenAIInlineLinkItem)
	if !ok {
		t.Fatalf("Metadata type = %T, want *GenAIInlineLinkItem", te.Metadata)
	}
	if link.URL != "https://example.com" || link.DisplayName != "Example" {
		t.Errorf("unexpected link data: %+v", link)
	}
}

func TestTextEntityUnmarshalJSON_UnknownType(t *testing.T) {
	data := []byte(`{"key":"abc","metadata":{"__typename":"SomethingNew","foo":"bar"}}`)

	var te TextEntity
	if err := json.Unmarshal(data, &te); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}
	if _, ok := te.Metadata.(UnknownTextEntityMetadata); !ok {
		t.Fatalf("Metadata type = %T, want UnknownTextEntityMetadata", te.Metadata)
	}
}
