package editorial

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Layout of the `imports/` directory (SPEC §4.1; the layout is
// implementation-defined):
//
//	imports/staged/<order>.json            staged imports, applied in name order
//	imports/records/<doc>/<source>.json    applied records kept for rebuilds
//	imports/reports/<order>.json           one report per applied import
//
// <order> sorts in staging order. <doc> and <source> are short hashes of the
// document's rel_path and of the source name, so no file name carries a path
// or a source name.
const (
	importsDirName = "imports"
	stagedDirName  = "staged"
	recordsDirName = "records"
	reportsDirName = "reports"
	stateFormat    = 1
	dirMode        = 0o700
	fileMode       = 0o600
)

// ImportsDir is the imports directory of a state directory.
func ImportsDir(stateDir string) string { return filepath.Join(stateDir, importsDirName) }

func stagedDir(stateDir string) string  { return filepath.Join(ImportsDir(stateDir), stagedDirName) }
func recordsDir(stateDir string) string { return filepath.Join(ImportsDir(stateDir), recordsDirName) }

// ReportsDir is where the apply step records its import reports.
func ReportsDir(stateDir string) string { return filepath.Join(ImportsDir(stateDir), reportsDirName) }

// Staged is one staged import file.
type Staged struct {
	Format        int               `json:"format"`
	Source        string            `json:"source"`
	Key           string            `json:"key"`
	StagedUnix    int64             `json:"staged_unix"`
	Records       []json.RawMessage `json:"records"`
	IgnoredFields map[string]int    `json:"ignored_fields"`
}

// NewStaged builds the staged form of a validated batch.
func NewStaged(batch Batch, source, keyKind string, now time.Time) Staged {
	records := make([]json.RawMessage, 0, len(batch.Records))
	for _, rec := range batch.Records {
		records = append(records, rec.Canonical)
	}
	ignored := map[string]int{}
	for k, v := range batch.Ignored {
		ignored[k] = v
	}
	return Staged{
		Format: stateFormat, Source: source, Key: keyKind, StagedUnix: now.Unix(),
		Records: records, IgnoredFields: ignored,
	}
}

// Stage writes a staged import atomically and returns its path. The name sorts
// after every import staged before it.
func Stage(stateDir string, staged Staged, now time.Time) (string, error) {
	dir := stagedDir(stateDir)
	if err := os.MkdirAll(dir, dirMode); err != nil {
		return "", err
	}
	data, err := json.Marshal(staged)
	if err != nil {
		return "", err
	}
	return publishInOrder(dir, data, now.UTC().UnixNano())
}

// publishInOrder writes data under the next free order key: never below the
// key of an import that is still staged, so a clock that goes back or two
// imports in the same nanosecond keep their staging order. The final name is
// claimed with a hard link, which fails when the name exists, so two
// concurrent commands never take the same key.
func publishInOrder(dir string, data []byte, key int64) (string, error) {
	if last, err := lastStagedKey(dir); err == nil && last >= key {
		key = last + 1
	}
	tmp, err := os.CreateTemp(dir, ".stage.tmp-*")
	if err != nil {
		return "", err
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()
	if err := writeAndClose(tmp, data); err != nil {
		return "", err
	}
	for attempt := 0; attempt < 1000; attempt++ {
		path := filepath.Join(dir, fmt.Sprintf("%020d.json", key))
		err := os.Link(tmpPath, path)
		if err == nil {
			return path, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return "", err
		}
		key++
	}
	return "", errors.New("no free staging name")
}

// lastStagedKey returns the largest order key among the staged files.
func lastStagedKey(dir string) (int64, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, err
	}
	var last int64 = -1
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, ".") || !strings.HasSuffix(name, ".json") {
			continue
		}
		if n, perr := strconv.ParseInt(strings.TrimSuffix(name, ".json"), 10, 64); perr == nil && n > last {
			last = n
		}
	}
	return last, nil
}

func writeAndClose(f *os.File, data []byte) error {
	if err := f.Chmod(fileMode); err != nil {
		_ = f.Close()
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// rebuildMarkerName is the file that records a pending forced rebuild.
const rebuildMarkerName = "rebuild-pending"

// MarkRebuildPending records that every editorial representation must be
// written again by the next apply pass.
func MarkRebuildPending(stateDir string) error {
	if err := os.MkdirAll(ImportsDir(stateDir), dirMode); err != nil {
		return err
	}
	return writeAtomic(filepath.Join(ImportsDir(stateDir), rebuildMarkerName), []byte("1\n"))
}

// RebuildPending reports whether a forced rebuild is pending.
func RebuildPending(stateDir string) bool {
	_, err := os.Stat(filepath.Join(ImportsDir(stateDir), rebuildMarkerName))
	return err == nil
}

// ClearRebuildPending removes the marker after a forced rebuild succeeded.
func ClearRebuildPending(stateDir string) error {
	err := os.Remove(filepath.Join(ImportsDir(stateDir), rebuildMarkerName))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// ListStaged returns the staged import files in staging order. A temporary
// file of an unfinished write is never listed.
func ListStaged(stateDir string) ([]string, error) {
	entries, err := os.ReadDir(stagedDir(stateDir))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || strings.HasPrefix(name, ".") || !strings.HasSuffix(name, ".json") {
			continue
		}
		out = append(out, filepath.Join(stagedDir(stateDir), name))
	}
	sort.Strings(out)
	return out, nil
}

// ReadStaged reads one staged import file.
func ReadStaged(path string) (Staged, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Staged{}, err
	}
	var staged Staged
	if err := json.Unmarshal(data, &staged); err != nil {
		return Staged{}, fmt.Errorf("decode staged import %s: %w", filepath.Base(path), err)
	}
	if staged.Format != stateFormat {
		return Staged{}, fmt.Errorf("staged import %s has unknown format %d", filepath.Base(path), staged.Format)
	}
	if _, err := ParseKeyKind(staged.Key); err != nil {
		return Staged{}, fmt.Errorf("staged import %s: %w", filepath.Base(path), err)
	}
	return staged, nil
}

// Retained is one applied record kept for rebuilds (SPEC §7.11.3).
type Retained struct {
	Format       int             `json:"format"`
	Source       string          `json:"source"`
	KeyKind      string          `json:"key_kind"`
	RelPath      string          `json:"rel_path"`
	Record       json.RawMessage `json:"record"`
	RecordHash   string          `json:"record_hash"`
	ImportedUnix int64           `json:"imported_unix"`
}

// Parse re-validates the retained record. It cannot fail for a record that was
// validated when it was staged, unless the file was edited.
func (r Retained) Parse() (Record, error) {
	return ParseRecord(r.Record, r.KeyKind, r.Source)
}

func retainedPath(stateDir, relPath, source string) string {
	return filepath.Join(recordsDir(stateDir), shortHash(relPath), shortHash(source)+".json")
}

// SaveRetained writes a retained record atomically, replacing an earlier one
// for the same (source, document).
func SaveRetained(stateDir string, r Retained) error {
	r.Format = stateFormat
	path := retainedPath(stateDir, r.RelPath, r.Source)
	if err := os.MkdirAll(filepath.Dir(path), dirMode); err != nil {
		return err
	}
	data, err := json.Marshal(r)
	if err != nil {
		return err
	}
	return writeAtomic(path, data)
}

// LoadRetained reads the retained record for (source, document). ok is false
// when there is none.
func LoadRetained(stateDir, relPath, source string) (Retained, bool, error) {
	r, err := readRetained(retainedPath(stateDir, relPath, source))
	if errors.Is(err, fs.ErrNotExist) {
		return Retained{}, false, nil
	}
	if err != nil {
		return Retained{}, false, err
	}
	return r, true, nil
}

// RemoveRetained deletes the retained record for (source, document) and its
// directory when that is left empty. A missing record is not an error.
func RemoveRetained(stateDir, relPath, source string) error {
	path := retainedPath(stateDir, relPath, source)
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	_ = os.Remove(filepath.Dir(path)) // fails while other sources remain
	return nil
}

// LoadAllRetained reads every retained record. A file that does not decode is
// reported in bad and skipped, so one damaged file cannot stop the rebuild of
// the others.
func LoadAllRetained(stateDir string) (records []Retained, bad []string, err error) {
	docDirs, err := os.ReadDir(recordsDir(stateDir))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	for _, d := range docDirs {
		if !d.IsDir() {
			continue
		}
		dir := filepath.Join(recordsDir(stateDir), d.Name())
		files, rerr := os.ReadDir(dir)
		if rerr != nil {
			return nil, nil, rerr
		}
		for _, f := range files {
			if f.IsDir() || strings.HasPrefix(f.Name(), ".") || !strings.HasSuffix(f.Name(), ".json") {
				continue
			}
			r, rerr := readRetained(filepath.Join(dir, f.Name()))
			if rerr != nil {
				bad = append(bad, filepath.Join(d.Name(), f.Name()))
				continue
			}
			records = append(records, r)
		}
	}
	sort.Slice(records, func(i, j int) bool {
		if records[i].RelPath != records[j].RelPath {
			return records[i].RelPath < records[j].RelPath
		}
		return records[i].Source < records[j].Source
	})
	return records, bad, nil
}

func readRetained(path string) (Retained, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Retained{}, err
	}
	var r Retained
	if err := json.Unmarshal(data, &r); err != nil {
		return Retained{}, err
	}
	if r.Format != stateFormat || r.RelPath == "" || r.Source == "" {
		return Retained{}, errors.New("retained record is incomplete")
	}
	return r, nil
}

// Report is the import report of one applied import (SPEC §7.11.5).
type Report struct {
	Source           string         `json:"source"`
	Key              string         `json:"key"`
	Records          int            `json:"records"`
	AppliedUnix      int64          `json:"applied_unix"`
	Applied          int            `json:"applied"`
	Annotations      int            `json:"annotations"`
	Unmatched        int            `json:"unmatched"`
	UnmatchedKeys    []string       `json:"unmatched_keys"`
	NonMediaOnly     int            `json:"non_media_only"`
	NonMediaOnlyKeys []string       `json:"non_media_only_keys"`
	Conflicting      int            `json:"conflicting"`
	ConflictingKeys  []string       `json:"conflicting_keys"`
	Withheld         int            `json:"withheld"`
	WithheldKeys     []string       `json:"withheld_keys"`
	MultiMatched     int            `json:"multi_matched"`
	IgnoredFields    map[string]int `json:"ignored_fields"`
	SegmentsDropped  int            `json:"segments_dropped"`
}

// NewReport starts a report with every list present and empty.
func NewReport(source, keyKind string, records int, ignored map[string]int) Report {
	fields := map[string]int{}
	for k, v := range ignored {
		fields[k] = v
	}
	return Report{
		Source: source, Key: keyKind, Records: records,
		UnmatchedKeys: []string{}, NonMediaOnlyKeys: []string{},
		ConflictingKeys: []string{}, WithheldKeys: []string{},
		IgnoredFields: fields,
	}
}

// Finish sorts the key lists and sets each count from its list.
func (r *Report) Finish(now time.Time) {
	r.AppliedUnix = now.Unix()
	for _, list := range []*[]string{&r.UnmatchedKeys, &r.NonMediaOnlyKeys, &r.ConflictingKeys, &r.WithheldKeys} {
		sort.Strings(*list)
	}
	r.Unmatched = len(r.UnmatchedKeys)
	r.NonMediaOnly = len(r.NonMediaOnlyKeys)
	r.Conflicting = len(r.ConflictingKeys)
	r.Withheld = len(r.WithheldKeys)
}

// Level is the event level of the report: warn when any key was unmatched,
// matched only non-media documents, conflicted or was withheld.
func (r Report) Level() string {
	if r.Unmatched+r.NonMediaOnly+r.Conflicting+r.Withheld > 0 {
		return "warn"
	}
	return "info"
}

// SaveReport records a report under imports/reports with the staged file's
// name, so the report of each import is found by the name it was staged with.
func SaveReport(stateDir, stagedName string, r Report) (string, error) {
	dir := ReportsDir(stateDir)
	if err := os.MkdirAll(dir, dirMode); err != nil {
		return "", err
	}
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, filepath.Base(stagedName))
	return path, writeAtomic(path, data)
}

// writeAtomic writes data to a private temporary file in the same directory
// and renames it over path, so a reader never sees a partial file.
func writeAtomic(path string, data []byte) (err error) {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer func() {
		if err != nil {
			_ = tmp.Close()
			_ = os.Remove(tmpPath)
		}
	}()
	if err = tmp.Chmod(fileMode); err != nil {
		return err
	}
	if _, err = tmp.Write(data); err != nil {
		return err
	}
	if err = tmp.Sync(); err != nil {
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}
