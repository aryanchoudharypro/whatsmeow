// Copyright (c) 2026 Tulir Asokan
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package richresponse

import (
	"encoding/json"
	"reflect"
)

type TextEntity struct {
	Key      string             `json:"key"`
	Metadata TextEntityMetadata `json:"metadata"`
}

type textEntityMetaType struct {
	Key      string          `json:"key"`
	Metadata json.RawMessage `json:"metadata"`
}

func (te *TextEntity) UnmarshalJSON(data []byte) error {
	var metaType textEntityMetaType
	if err := json.Unmarshal(data, &metaType); err != nil {
		return err
	}
	te.Key = metaType.Key

	// Unmarshaling the concrete metadata type from just its own sub-object,
	// via the same helper primitive.go/viewmodel.go use, rather than handing
	// the full outer `data` back to json.Unmarshal(data, te)/(data, &te):
	// either of those would find that *TextEntity (or **TextEntity, which
	// json.Unmarshal dereferences down to it too) still implements
	// Unmarshaler and call straight back into this method with the same
	// bytes - unbounded recursion crashing the process with a stack
	// overflow, which is what every metadata type in textEntityMetadataTypes
	// used to do (an unrecognized type is the one case that never recursed,
	// since unmarshalWithTypeName returns UnknownTextEntityMetadata for it
	// without going through this method again).
	val, err := unmarshalWithTypeName[UnknownTextEntityMetadata](metaType.Metadata, textEntityMetadataTypes)
	if err != nil {
		return err
	}
	te.Metadata = val.(TextEntityMetadata)
	return nil
}

type TextEntityMetadata interface {
	isTextEntityMetadata()
}

var textEntityMetadataTypes = map[string]reflect.Type{
	"GenAISearchCitationItem": reflect.TypeFor[GenAISearchCitationItem](),
	"GenAIInlineLinkItem":     reflect.TypeFor[GenAIInlineLinkItem](),
	"GenAIDeepLinkItem":       reflect.TypeFor[GenAIDeepLinkItem](),
	"GenAILatexItem":          reflect.TypeFor[GenAILatexItem](),
}

func (*GenAISearchCitationItem) isTextEntityMetadata()  {}
func (*GenAIInlineLinkItem) isTextEntityMetadata()      {}
func (*GenAIDeepLinkItem) isTextEntityMetadata()        {}
func (*GenAILatexItem) isTextEntityMetadata()           {}
func (UnknownTextEntityMetadata) isTextEntityMetadata() {}

type GenAISearchCitationItem struct {
}

type GenAIInlineLinkItem struct {
	URL         string `json:"url"`
	DisplayName string `json:"display_name"`
}

type GenAIDeepLinkItem struct {
	DeepLinkURL string `json:"deeplink_url"`
	Text        string `json:"text"`
}

type GenAILatexItem struct {
	LatexExpression string `json:"latex_expression"`
}

type UnknownTextEntityMetadata json.RawMessage
