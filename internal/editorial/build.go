package editorial

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
)

// Event values of an editorial annotation (SPEC §7.11.4).
const (
	EventAppears     = "appears"
	EventSegment     = "segment"
	EventDescription = "description"
)

// Span kinds an editorial annotation uses.
const (
	SpanTime     = "time"
	SpanDocument = "document"
)

// RepType is the rep_type every editorial hit carries (SPEC §5.2).
const RepType = "editorial"

// RepTypeForSource is the stored rep_type of the editorial representation of
// one source. The store keeps one representation per (document, rep_type), and
// SPEC §7.11.3 asks for one per (document, source), so the stored rep_type is
// qualified by a hash of the source name. Chunks and hits carry the bare
// RepType.
func RepTypeForSource(source string) string {
	return RepType + "@" + shortHash(source)
}

// IsRepType reports whether a stored rep_type is an editorial one.
func IsRepType(repType string) bool {
	return repType == RepType || strings.HasPrefix(repType, RepType+"@")
}

func shortHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:8])
}

// Annotation is one mapped annotation: one chunk with one span.
type Annotation struct {
	Text       string
	SpanKind   string
	StartMS    int64
	EndMS      int64
	Entities   []string
	Event      string
	Attributes map[string]string
}

// Built is the mapping of one record for one document.
type Built struct {
	// Annotations holds the person and segment annotations, in order.
	Annotations []Annotation
	// Description is the document-level text (header, topics, description),
	// "" when all three are empty. The caller chunks it and gives each chunk
	// a document span with DescriptionAttributes.
	Description           string
	DescriptionAttributes map[string]string
	SegmentsDropped       int
}

// Duration is the media duration in milliseconds, or unknown.
type Duration struct {
	MS    int64
	Known bool
}

// Build maps a record to its annotations (SPEC §7.11.4). The result is a
// deterministic function of the record, the duration and the folds; the
// caller adds the source name to every span.
func Build(rec Record, duration Duration, folds Folds) Built {
	provenance := rec.ProvenanceAttributes()
	out := Built{}
	out.Annotations = append(out.Annotations, buildPeople(rec.People, duration, folds, provenance)...)
	segs, dropped := buildSegments(rec.Segments, duration, provenance)
	out.Annotations = append(out.Annotations, segs...)
	out.SegmentsDropped = dropped
	out.Description = rec.DescriptionText()
	if out.Description != "" {
		out.DescriptionAttributes = copyAttrs(provenance)
	}
	return out
}

// HasContent reports whether the record has any header, people, topics,
// segments or description content. A record with none removes the source's
// annotations from the document (SPEC §7.11.2).
func (rec Record) HasContent() bool {
	return len(rec.People) > 0 || len(rec.Segments) > 0 || rec.DescriptionText() != ""
}

// DescriptionText joins header, topics (one per line) and description,
// separated by blank lines.
func (rec Record) DescriptionText() string {
	var parts []string
	if h := strings.TrimSpace(rec.Header); h != "" {
		parts = append(parts, h)
	}
	var topics []string
	for _, t := range rec.Topics {
		if t = strings.TrimSpace(t); t != "" {
			topics = append(topics, t)
		}
	}
	if len(topics) > 0 {
		parts = append(parts, strings.Join(topics, "\n"))
	}
	if d := strings.TrimSpace(rec.Description); d != "" {
		parts = append(parts, d)
	}
	return strings.Join(parts, "\n\n")
}

// ProvenanceAttributes are the attributes every annotation of the record
// carries: the match provenance, each extra scalar match member as
// match_<member>, and each extra top-level scalar field.
func (rec Record) ProvenanceAttributes() map[string]string {
	attrs := map[string]string{}
	for k, v := range rec.Extra {
		attrs[k] = v
	}
	if m := rec.Match; m != nil {
		for k, v := range m.Extra {
			attrs[k] = v
		}
		if m.HasScore {
			attrs["match_score"] = FormatScore(m.Score)
		}
		setIfNotEmpty(attrs, "external_id", m.ExternalID)
		setIfNotEmpty(attrs, "external_name", m.ExternalName)
		setIfNotEmpty(attrs, "external_date", m.ExternalDate)
	}
	return attrs
}

// Texts returns every text of the record that becomes a chunk or a span
// attribute: the secret check (SPEC §7.11.3) screens all of them. The key is
// not among them, because it only names the document and is never indexed.
func (rec Record) Texts() []string {
	out := []string{rec.Header, rec.Description}
	out = append(out, rec.Topics...)
	for _, p := range rec.People {
		out = append(out, p.Name, p.Role)
	}
	for _, s := range rec.Segments {
		out = append(out, s.Text)
	}
	attrs := rec.ProvenanceAttributes()
	keys := make([]string, 0, len(attrs))
	for k := range attrs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		out = append(out, attrs[k])
	}
	return out
}

// wholeMedia is the scope of a person annotation: the time span
// [0, duration] when the duration is known, otherwise the document span.
func wholeMedia(duration Duration) (kind string, start, end int64) {
	if duration.Known {
		return SpanTime, 0, duration.MS
	}
	return SpanDocument, 0, 0
}

// buildPeople maps people to one annotation per person id. A repeated id keeps
// the first display name and joins the non-empty roles in record order.
func buildPeople(people []Person, duration Duration, folds Folds, provenance map[string]string) []Annotation {
	type person struct {
		display string
		roles   []string
	}
	var order []string
	byID := map[string]*person{}
	for _, p := range people {
		id := PersonID(p.Name, folds)
		if id == "" {
			continue
		}
		entry, seen := byID[id]
		if !seen {
			entry = &person{display: strings.TrimSpace(p.Name)}
			byID[id] = entry
			order = append(order, id)
		}
		if role := strings.TrimSpace(p.Role); role != "" {
			entry.roles = append(entry.roles, role)
		}
	}
	kind, start, end := wholeMedia(duration)
	out := make([]Annotation, 0, len(order))
	for _, id := range order {
		entry := byID[id]
		attrs := copyAttrs(provenance)
		attrs["display_name"] = entry.display
		text := entry.display
		if len(entry.roles) > 0 {
			role := strings.Join(entry.roles, "; ")
			attrs["role"] = role
			text = entry.display + ", " + role
		}
		out = append(out, Annotation{
			Text: text, SpanKind: kind, StartMS: start, EndMS: end,
			Entities: []string{id}, Event: EventAppears, Attributes: attrs,
		})
	}
	return out
}

// buildSegments maps segments to annotations over [t, next). Segments are
// sorted by start (stable); one that starts at or after a known duration is
// dropped and counted. With no known duration the last segment ends at its own
// start until the duration is known.
func buildSegments(segments []Segment, duration Duration, provenance map[string]string) ([]Annotation, int) {
	sorted := make([]Segment, 0, len(segments))
	dropped := 0
	for _, s := range segments {
		if duration.Known && s.StartMS >= duration.MS {
			dropped++
			continue
		}
		sorted = append(sorted, s)
	}
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].StartMS < sorted[j].StartMS })
	ends := segmentEnds(sorted, duration)
	out := make([]Annotation, 0, len(sorted))
	for i, s := range sorted {
		out = append(out, Annotation{
			Text: strings.TrimSpace(s.Text), SpanKind: SpanTime, StartMS: s.StartMS, EndMS: ends[i],
			Event: EventSegment, Attributes: copyAttrs(provenance),
		})
	}
	return out, dropped
}

// segmentEnds computes the end of each sorted segment in one backward pass: the
// start of the next segment with a greater offset. Segments that share a start
// share an end, so many equal starts cost linear time. The last group ends at
// the known duration, or at its own start when the duration is unknown.
func segmentEnds(sorted []Segment, duration Duration) []int64 {
	ends := make([]int64, len(sorted))
	for i := len(sorted) - 1; i >= 0; i-- {
		switch {
		case i == len(sorted)-1 && duration.Known:
			ends[i] = duration.MS
		case i == len(sorted)-1:
			ends[i] = sorted[i].StartMS
		case sorted[i+1].StartMS > sorted[i].StartMS:
			ends[i] = sorted[i+1].StartMS
		default:
			ends[i] = ends[i+1]
		}
	}
	return ends
}

func setIfNotEmpty(attrs map[string]string, key, value string) {
	if strings.TrimSpace(value) != "" {
		attrs[key] = value
	}
}

func copyAttrs(in map[string]string) map[string]string {
	out := make(map[string]string, len(in)+2)
	for k, v := range in {
		out[k] = v
	}
	return out
}

// CitationLabel renders the SPEC §9.3 label of an editorial hit, "editor
// annotation (<source>, match <score>)". The match part is left out when the
// score is empty.
func CitationLabel(source, matchScore string) string {
	label := "editor annotation (" + strings.TrimSpace(source)
	if score := strings.TrimSpace(matchScore); score != "" {
		label += ", match " + score
	}
	return label + ")"
}

// RepHash is the stable hash of a built editorial representation: a function
// of the source name, the record, the duration, the folds and the text chunk
// budget, so an unchanged input never rewrites or embeds again.
func RepHash(source, recordHash string, duration Duration, folds Folds, chunkBudget string) string {
	durationPart := "unknown"
	if duration.Known {
		durationPart = fmt.Sprintf("%d", duration.MS)
	}
	input := strings.Join([]string{
		"editorial/v1",
		fmt.Sprintf("%d:%s", len(source), source),
		recordHash,
		durationPart,
		strings.Join(folds.Entries(), "\x1f"),
		chunkBudget,
	}, "\n")
	sum := sha256.Sum256([]byte(input))
	return hex.EncodeToString(sum[:])
}
