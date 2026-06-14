// Package qdrantindex implements the pluggable model.Index (issue #247)
// contract on top of a Qdrant collection (issue #268, epic #250). It is the
// optional Tier-C vector backend: the in-memory HNSW remains dir2mcp's
// local-first default, and the pure-Go on-disk store (issue #246) is the
// Tier-B sibling; Qdrant is only active when index.backend=qdrant is selected.
//
// The package is split into three concerns so the network-free logic can be
// unit-tested directly:
//
//   - payload.go: the IndexPayload <-> Qdrant point-payload mapping.
//   - filter.go:  the model.Filter -> Qdrant filter mapping and the
//     CanFilter push-down decision.
//   - qdrant.go:  the model.Index implementation, parameterised over a small
//     Client interface so tests can stub the wire calls.
package qdrantindex

import (
	"strings"

	"github.com/qdrant/go-client/qdrant"

	"github.com/dirstral/dir2mcp/internal/model"
)

// Payload field keys stored on each Qdrant point. Keeping them as exported
// constants lets the filter mapping and the round-trip tests reference the
// exact wire keys rather than duplicating string literals.
const (
	fieldChunkID = "chunk_id"
	fieldRelPath = "rel_path"
	fieldDocType = "doc_type"
	// fieldDocTypeLC holds the lower-cased doc_type used purely for filtering.
	// model.Filter.Match treats DocTypes case-insensitively; Qdrant keyword
	// match is case-sensitive, so we store and query a normalised copy to keep
	// the pushed-down DocTypes predicate faithful (CanFilter exactness).
	fieldDocTypeLC = "doc_type_lc"
	fieldRepType   = "rep_type"
	fieldModality  = "modality"
	fieldTitle     = "title"
	fieldStartMS   = "start_ms"
	fieldEndMS     = "end_ms"
	fieldLanguage  = "language"
	fieldSpeaker   = "speaker"
	fieldSnippet   = "snippet"
	fieldMediaRef  = "media_ref"

	// identityKey marks the reserved sentinel point that records the
	// corpus-lifetime embed identity (SPEC 8.1.4). A real chunk payload never
	// sets it because chunk IDs are always > 0 and the sentinel uses id 0.
	identityKey = "__embed_identity__"
)

// identityPointID is the reserved Qdrant point id used to persist the embed
// identity. model.Index.Upsert rejects a zero ChunkID, so 0 never collides
// with a real corpus chunk.
const identityPointID uint64 = 0

// payloadToQdrant maps a model.IndexPayload to the Qdrant payload map. Only the
// time-span fields are stored as integers; everything else is a keyword string
// so DocTypes match-any and the orphan check filter natively. Empty strings are
// still written so the schema is stable across points (the orphan check relies
// on rel_path being present-but-empty rather than absent).
func payloadToQdrant(p model.IndexPayload) map[string]*qdrant.Value {
	return map[string]*qdrant.Value{
		fieldChunkID:   qdrant.NewValueInt(int64(p.ChunkID)),
		fieldRelPath:   qdrant.NewValueString(p.RelPath),
		fieldDocType:   qdrant.NewValueString(p.DocType),
		fieldDocTypeLC: qdrant.NewValueString(strings.ToLower(strings.TrimSpace(p.DocType))),
		fieldRepType:   qdrant.NewValueString(p.RepType),
		fieldModality:  qdrant.NewValueString(p.Modality),
		fieldTitle:     qdrant.NewValueString(p.Title),
		fieldStartMS:   qdrant.NewValueInt(int64(p.StartMS)),
		fieldEndMS:     qdrant.NewValueInt(int64(p.EndMS)),
		fieldLanguage:  qdrant.NewValueString(p.Language),
		fieldSpeaker:   qdrant.NewValueString(p.Speaker),
		fieldSnippet:   qdrant.NewValueString(p.Snippet),
		fieldMediaRef:  qdrant.NewValueString(p.MediaRef),
	}
}

// qdrantToPayload reconstructs a model.IndexPayload from a Qdrant payload map.
// It is the inverse of payloadToQdrant for the fields that round-trip; the
// Span is not persisted in Qdrant (region/bbox metadata lives in SQLite and is
// re-attached by retrieval from its in-memory chunk metadata), so Span is left
// zero. chunkID falls back to the point id when the payload lacks chunk_id.
func qdrantToPayload(chunkID uint64, payload map[string]*qdrant.Value) model.IndexPayload {
	if id := payload[fieldChunkID]; id != nil && id.GetIntegerValue() > 0 {
		chunkID = uint64(id.GetIntegerValue())
	}
	return model.IndexPayload{
		ChunkID:  chunkID,
		RelPath:  payload[fieldRelPath].GetStringValue(),
		DocType:  payload[fieldDocType].GetStringValue(),
		RepType:  payload[fieldRepType].GetStringValue(),
		Modality: payload[fieldModality].GetStringValue(),
		Title:    payload[fieldTitle].GetStringValue(),
		StartMS:  int(payload[fieldStartMS].GetIntegerValue()),
		EndMS:    int(payload[fieldEndMS].GetIntegerValue()),
		Language: payload[fieldLanguage].GetStringValue(),
		Speaker:  payload[fieldSpeaker].GetStringValue(),
		Snippet:  payload[fieldSnippet].GetStringValue(),
		MediaRef: payload[fieldMediaRef].GetStringValue(),
	}
}
