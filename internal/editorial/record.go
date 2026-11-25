package editorial

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Key kinds of `--key` (SPEC §7.11.1).
const (
	KeyStem        = "stem"
	KeyRelPath     = "rel_path"
	KeyContentHash = "content_hash"
)

// ParseKeyKind validates a `--key` value. Empty selects the default, stem.
func ParseKeyKind(value string) (string, error) {
	switch strings.TrimSpace(value) {
	case "", KeyStem:
		return KeyStem, nil
	case KeyRelPath:
		return KeyRelPath, nil
	case KeyContentHash:
		return KeyContentHash, nil
	default:
		return "", fmt.Errorf("--key must be one of stem, rel_path, content_hash: %q", value)
	}
}

// MaxSourceRunes is the longest source name `--source` accepts (SPEC §7.11.1).
const MaxSourceRunes = 128

// ValidateSource checks a `--source` value: 1 to 128 characters, valid UTF-8,
// no control characters.
func ValidateSource(source string) error {
	if !utf8.ValidString(source) {
		return errors.New("--source is not valid UTF-8")
	}
	n := utf8.RuneCountInString(source)
	if n == 0 {
		return errors.New("--source is required")
	}
	if n > MaxSourceRunes {
		return fmt.Errorf("--source is longer than %d characters", MaxSourceRunes)
	}
	for _, r := range source {
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) {
			return errors.New("--source contains a control character")
		}
	}
	// A span stores its sources trimmed, so a name with surrounding white
	// space could never be selected by the literal source filter (SPEC §9.11).
	if strings.TrimSpace(source) != source {
		return errors.New("--source must not start or end with white space")
	}
	return nil
}

// Person is one validated `people` item.
type Person struct {
	Name string
	Role string
}

// Segment is one validated `segments` item; StartMS is the start offset
// rounded to the nearest millisecond.
type Segment struct {
	StartMS int64
	Text    string
}

// Match is the validated `match` object.
type Match struct {
	HasScore     bool
	Score        float64
	ExternalID   string
	ExternalName string
	ExternalDate string
	// Extra holds each extra scalar member under its attribute name
	// (`match_<member>`).
	Extra map[string]string
}

// Record is one validated record (SPEC §7.11.2).
type Record struct {
	Key         string
	Language    string
	Header      string
	Description string
	Match       *Match
	People      []Person
	Topics      []string
	Segments    []Segment
	// Extra holds each extra top-level scalar field under its own name.
	Extra map[string]string
	// Ignored lists the path of each extra field that was ignored, once per
	// path, sorted.
	Ignored []string
	// Canonical is the record re-encoded with sorted keys. It is what a staged
	// import and a retained record store, and Hash is its SHA-256.
	Canonical json.RawMessage
	Hash      string
}

// definedTopLevel names the record fields the record shape defines. The key
// fields that `--key` does not select are defined fields too: they are type
// checked and not kept as attributes.
var definedTopLevel = map[string]struct{}{
	"stem": {}, "rel_path": {}, "content_hash": {}, "source": {}, "match": {},
	"language": {}, "header": {}, "people": {}, "topics": {}, "segments": {}, "description": {},
}

// reservedAttributeNames are the attribute names a mapped annotation defines
// (SPEC §7.11.2). An extra top-level field with one of these names would
// overwrite a defined value, so it makes the record invalid.
var reservedAttributeNames = map[string]struct{}{
	"role": {}, "display_name": {}, "external_id": {}, "external_name": {}, "external_date": {},
}

// ParseRecord decodes and validates one JSON object line. The error text is the
// reason the line is invalid; it never echoes a field value.
func ParseRecord(line []byte, keyKind, source string) (Record, error) {
	obj, err := decodeObject(line)
	if err != nil {
		return Record{}, err
	}
	rec := Record{}
	ignored := map[string]struct{}{}
	if err := rec.readDefined(obj, keyKind, source, ignored); err != nil {
		return Record{}, err
	}
	if err := rec.readExtraTopLevel(obj, ignored); err != nil {
		return Record{}, err
	}
	rec.Ignored = sortedSet(ignored)
	canonical, err := canonicalJSON(obj)
	if err != nil {
		return Record{}, fmt.Errorf("encode record: %w", err)
	}
	rec.Canonical = canonical
	sum := sha256.Sum256(canonical)
	rec.Hash = hex.EncodeToString(sum[:])
	return rec, nil
}

// decodeObject decodes exactly one JSON object, with numbers kept as written.
func decodeObject(line []byte) (map[string]any, error) {
	if !utf8.Valid(line) {
		return nil, errors.New("the line is not valid UTF-8")
	}
	dec := json.NewDecoder(bytes.NewReader(line))
	dec.UseNumber()
	var value any
	if err := dec.Decode(&value); err != nil {
		return nil, errors.New("the line is not a JSON object")
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("the line holds more than one JSON value")
	}
	obj, ok := value.(map[string]any)
	if !ok {
		return nil, errors.New("the line is not a JSON object")
	}
	return obj, nil
}

// readDefined reads and type checks every defined field.
func (rec *Record) readDefined(obj map[string]any, keyKind, source string, ignored map[string]struct{}) error {
	// The key is kept exactly as written: key resolution compares it exactly
	// (SPEC §7.11.1). It must still hold more than white space.
	if _, err := requiredString(obj, keyKind); err != nil {
		return err
	}
	rec.Key = obj[keyKind].(string)
	var err error
	for _, other := range []string{KeyStem, KeyRelPath, KeyContentHash} {
		if other == keyKind {
			continue
		}
		if _, err := optionalString(obj, other); err != nil {
			return err
		}
	}
	if err := checkSource(obj, source); err != nil {
		return err
	}
	if rec.Language, err = optionalString(obj, "language"); err != nil {
		return err
	}
	if rec.Header, err = optionalString(obj, "header"); err != nil {
		return err
	}
	if rec.Description, err = optionalString(obj, "description"); err != nil {
		return err
	}
	if rec.Match, err = readMatch(obj["match"], ignored); err != nil {
		return err
	}
	if rec.People, err = readPeople(obj["people"], ignored); err != nil {
		return err
	}
	if rec.Topics, err = readTopics(obj["topics"]); err != nil {
		return err
	}
	rec.Segments, err = readSegments(obj["segments"], ignored)
	return err
}

// checkSource enforces that an optional `source` equals `--source`.
func checkSource(obj map[string]any, source string) error {
	raw, present := obj["source"]
	if !present || raw == nil {
		return nil
	}
	value, ok := raw.(string)
	if !ok {
		return errors.New("source is not a string")
	}
	// Compared as written: a source name is matched literally (SPEC §9.11).
	if value != source {
		return errors.New("source differs from --source")
	}
	return nil
}

// readExtraTopLevel applies the extra-field rule to the top level: a scalar
// becomes an attribute, any other value is ignored and reported, and a
// reserved name makes the record invalid.
func (rec *Record) readExtraTopLevel(obj map[string]any, ignored map[string]struct{}) error {
	for name, value := range obj {
		if _, defined := definedTopLevel[name]; defined {
			continue
		}
		if reservedTopLevelName(name) {
			return fmt.Errorf("extra field %q has a reserved name", name)
		}
		scalar, ok := scalarString(value)
		if !ok {
			ignored[name] = struct{}{}
			continue
		}
		if rec.Extra == nil {
			rec.Extra = map[string]string{}
		}
		rec.Extra[name] = scalar
	}
	return nil
}

// reservedTopLevelName reports whether an extra top-level field name is
// reserved (SPEC §7.11.2).
func reservedTopLevelName(name string) bool {
	if _, reserved := reservedAttributeNames[name]; reserved {
		return true
	}
	return strings.HasPrefix(name, "match_") || strings.HasPrefix(name, "dir2mcp:")
}

// readMatch validates the optional `match` object.
func readMatch(value any, ignored map[string]struct{}) (*Match, error) {
	if value == nil {
		return nil, nil
	}
	obj, ok := value.(map[string]any)
	if !ok {
		return nil, errors.New("match is not an object")
	}
	m := &Match{}
	if err := m.readDefined(obj); err != nil {
		return nil, err
	}
	for name, member := range obj {
		switch name {
		case "score", "external_id", "external_name", "external_date":
			continue
		}
		if strings.HasPrefix(name, "dir2mcp:") {
			ignored["match."+name] = struct{}{}
			continue
		}
		scalar, ok := scalarString(member)
		if !ok {
			ignored["match."+name] = struct{}{}
			continue
		}
		if m.Extra == nil {
			m.Extra = map[string]string{}
		}
		m.Extra["match_"+name] = scalar
	}
	return m, nil
}

// integerLiteralRe is a JSON number with no fraction and no exponent.
var integerLiteralRe = regexp.MustCompile(`^-?\d+$`)

// externalDateRe is the `YYYY-MM-DD` shape of match.external_date.
var externalDateRe = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)

// readDefined reads the four defined members of `match`.
func (m *Match) readDefined(obj map[string]any) error {
	if raw, present := obj["score"]; present && raw != nil {
		num, ok := raw.(json.Number)
		if !ok {
			return errors.New("match.score is not a number")
		}
		score, err := num.Float64()
		if err != nil || math.IsInf(score, 0) || math.IsNaN(score) {
			return errors.New("match.score is not a finite number")
		}
		m.HasScore, m.Score = true, score
	}
	var err error
	if m.ExternalID, err = optionalString(obj, "external_id"); err != nil {
		return prefixMatch(err)
	}
	if m.ExternalName, err = optionalString(obj, "external_name"); err != nil {
		return prefixMatch(err)
	}
	if m.ExternalDate, err = optionalString(obj, "external_date"); err != nil {
		return prefixMatch(err)
	}
	if m.ExternalDate != "" {
		if _, perr := time.Parse("2006-01-02", m.ExternalDate); perr != nil || !externalDateRe.MatchString(m.ExternalDate) {
			return errors.New("match.external_date is not a YYYY-MM-DD date")
		}
	}
	return nil
}

func prefixMatch(err error) error { return fmt.Errorf("match.%w", err) }

// readPeople validates the optional `people` array.
func readPeople(value any, ignored map[string]struct{}) ([]Person, error) {
	items, err := optionalArray(value, "people")
	if err != nil {
		return nil, err
	}
	out := make([]Person, 0, len(items))
	for i, item := range items {
		obj, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("people[%d] is not an object", i)
		}
		name, err := requiredString(obj, "name")
		if err != nil {
			return nil, fmt.Errorf("people[%d].%w", i, err)
		}
		role, err := optionalString(obj, "role")
		if err != nil {
			return nil, fmt.Errorf("people[%d].%w", i, err)
		}
		for field := range obj {
			if field != "name" && field != "role" {
				ignored["people[]."+field] = struct{}{}
			}
		}
		out = append(out, Person{Name: name, Role: role})
	}
	return out, nil
}

// readTopics validates the optional `topics` array of strings.
func readTopics(value any) ([]string, error) {
	items, err := optionalArray(value, "topics")
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(items))
	for i, item := range items {
		s, ok := item.(string)
		if !ok {
			return nil, fmt.Errorf("topics[%d] is not a string", i)
		}
		out = append(out, s)
	}
	return out, nil
}

// readSegments validates the optional `segments` array.
func readSegments(value any, ignored map[string]struct{}) ([]Segment, error) {
	items, err := optionalArray(value, "segments")
	if err != nil {
		return nil, err
	}
	out := make([]Segment, 0, len(items))
	for i, item := range items {
		obj, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("segments[%d] is not an object", i)
		}
		startMS, err := parseOffset(obj["t"])
		if err != nil {
			return nil, fmt.Errorf("segments[%d].t %w", i, err)
		}
		text, err := requiredString(obj, "text")
		if err != nil {
			return nil, fmt.Errorf("segments[%d].%w", i, err)
		}
		for field := range obj {
			if field != "t" && field != "text" {
				ignored["segments[]."+field] = struct{}{}
			}
		}
		out = append(out, Segment{StartMS: startMS, Text: text})
	}
	return out, nil
}

// maxOffsetSeconds bounds a segment offset so the millisecond value cannot
// overflow. It is about 31,700 years of media.
const maxOffsetSeconds = 1e12

// offsetRe is the string form of a segment offset: SS, MM:SS or HH:MM:SS, each
// with an optional fraction of one to three digits.
var offsetRe = regexp.MustCompile(`^(\d+)(?::(\d+))?(?::(\d+))?(?:\.(\d{1,3}))?$`)

// parseOffset parses `t` to milliseconds (SPEC §7.11.2).
func parseOffset(value any) (int64, error) {
	switch v := value.(type) {
	case json.Number:
		sec, err := v.Float64()
		if err != nil || math.IsNaN(sec) || math.IsInf(sec, 0) || sec < 0 || sec > maxOffsetSeconds {
			return 0, errors.New("is not a non-negative number of seconds")
		}
		return int64(math.Round(sec * 1000)), nil
	case string:
		return parseOffsetString(v)
	case nil:
		return 0, errors.New("is missing")
	default:
		return 0, errors.New("is not a string or a number")
	}
}

// parseOffsetString parses HH:MM:SS, MM:SS or SS with an optional fraction.
func parseOffsetString(s string) (int64, error) {
	m := offsetRe.FindStringSubmatch(s)
	if m == nil {
		return 0, errors.New("does not parse as HH:MM:SS, MM:SS or SS")
	}
	parts := []string{m[1]}
	for _, p := range m[2:4] {
		if p != "" {
			parts = append(parts, p)
		}
	}
	var total int64
	for i, p := range parts {
		if len(p) > 12 {
			return 0, errors.New("is out of range")
		}
		n, err := strconv.ParseInt(p, 10, 64)
		if err != nil {
			return 0, errors.New("does not parse")
		}
		// A minutes or seconds field after a larger unit is at most 59.
		if i > 0 && (len(p) > 2 || n > 59) {
			return 0, errors.New("has a minutes or seconds field above 59")
		}
		total = total*60 + n
	}
	if float64(total) > maxOffsetSeconds {
		return 0, errors.New("is out of range")
	}
	ms := total * 1000
	if frac := m[4]; frac != "" {
		n, _ := strconv.ParseInt((frac + "00")[:3], 10, 64)
		ms += n
	}
	return ms, nil
}

// optionalArray returns a JSON array, nil for an absent or null value, or an
// error for any other type.
func optionalArray(value any, field string) ([]any, error) {
	if value == nil {
		return nil, nil
	}
	items, ok := value.([]any)
	if !ok {
		return nil, fmt.Errorf("%s is not an array", field)
	}
	return items, nil
}

// requiredString returns a string field that must be present and non-empty
// after trimming. The value is returned trimmed.
func requiredString(obj map[string]any, field string) (string, error) {
	raw, present := obj[field]
	if !present || raw == nil {
		return "", fmt.Errorf("%s is missing", field)
	}
	s, ok := raw.(string)
	if !ok {
		return "", fmt.Errorf("%s is not a string", field)
	}
	s = strings.TrimSpace(s)
	if s == "" {
		return "", fmt.Errorf("%s is empty", field)
	}
	return s, nil
}

// optionalString returns a trimmed string field, "" when absent or null, or an
// error when the value has another type.
func optionalString(obj map[string]any, field string) (string, error) {
	raw, present := obj[field]
	if !present || raw == nil {
		return "", nil
	}
	s, ok := raw.(string)
	if !ok {
		return "", fmt.Errorf("%s is not a string", field)
	}
	return strings.TrimSpace(s), nil
}

// scalarString renders a scalar extra value as an attribute string (SPEC
// §7.11.2): a number in the shortest decimal form that reads back as the same
// number, a boolean as true or false, a string as written. ok is false for
// null, object and array values.
func scalarString(value any) (string, bool) {
	switch v := value.(type) {
	case string:
		return v, true
	case bool:
		return strconv.FormatBool(v), true
	case json.Number:
		return formatNumber(v), true
	default:
		return "", false
	}
}

// formatNumber renders a JSON number in its shortest decimal form.
func formatNumber(n json.Number) string {
	// An integer literal is kept exactly as written: a float64 round trip
	// would change an identifier above 2^53. JSON has no leading zeros, so the
	// literal is already its shortest form.
	if s := n.String(); integerLiteralRe.MatchString(s) {
		if s == "-0" {
			return "0"
		}
		return s
	}
	f, err := n.Float64()
	if err != nil || math.IsInf(f, 0) || math.IsNaN(f) {
		return n.String()
	}
	return FormatScore(f)
}

// FormatScore renders a number as the shortest decimal string that reads back
// as the same float64, with no exponent (for example 1.25).
func FormatScore(f float64) string {
	if f == 0 {
		return "0"
	}
	return strconv.FormatFloat(f, 'f', -1, 64)
}

// canonicalJSON re-encodes a decoded object with sorted keys and without HTML
// escaping, so the same record always gives the same bytes.
func canonicalJSON(obj map[string]any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(obj); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

func sortedSet(set map[string]struct{}) []string {
	if len(set) == 0 {
		return nil
	}
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// LineError is one invalid line of an import file.
type LineError struct {
	Line   int    `json:"line"`
	Reason string `json:"reason"`
}

// Batch is a fully validated import file.
type Batch struct {
	Records []Record
	// Ignored counts, for each ignored extra-field path, the records that had
	// it.
	Ignored map[string]int
}

// ParseFile validates a whole import file (SPEC §7.11.1): every non-blank line
// is one record. It returns every invalid line with its 1-based line number.
// The batch is usable only when the error list is empty. A read failure is
// returned as err.
func ParseFile(r io.Reader, keyKind, source string) (Batch, []LineError, error) {
	reader := bufio.NewReader(r)
	batch := Batch{Ignored: map[string]int{}}
	var problems []LineError
	firstLine := map[string]int{}
	for lineNo := 1; ; lineNo++ {
		line, readErr := reader.ReadBytes('\n')
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return Batch{}, nil, readErr
		}
		if lineNo == 1 {
			line = bytes.TrimPrefix(line, []byte("\xef\xbb\xbf"))
		}
		if len(bytes.TrimSpace(line)) > 0 {
			problems = batch.addLine(line, lineNo, keyKind, source, firstLine, problems)
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
	}
	return batch, problems, nil
}

// addLine validates one non-blank line and adds it to the batch or to the
// problem list.
func (b *Batch) addLine(line []byte, lineNo int, keyKind, source string, firstLine map[string]int, problems []LineError) []LineError {
	rec, err := ParseRecord(bytes.TrimSpace(line), keyKind, source)
	if err != nil {
		return append(problems, LineError{Line: lineNo, Reason: err.Error()})
	}
	if first, dup := firstLine[rec.Key]; dup {
		return append(problems, LineError{Line: lineNo, Reason: fmt.Sprintf("the key value repeats the key of line %d", first)})
	}
	firstLine[rec.Key] = lineNo
	b.Records = append(b.Records, rec)
	for _, path := range rec.Ignored {
		b.Ignored[path]++
	}
	return problems
}
