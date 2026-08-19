package ingest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/dirstral/dir2mcp/internal/avutil"
	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/model"
)

// recognizeSource is the meta_json source value recorded on a recognition
// representation (design 0004 §4). Like an STT transcript — and unlike an
// authored sidecar — it is model-derived and carries the backend's
// derivation identity.
const recognizeSource = "recognize"

// recognizeMaxResponseBytes bounds the decoded response body.
const recognizeMaxResponseBytes = 64 << 20

// ErrRecognizeTimeout marks the one recognition failure that is NOT evidence of a
// broken backend: the call was accepted and answered nothing before the budget
// this daemon granted it ran out (#894).
//
// It wraps ErrRecognitionProviderFailure, so every existing classification keeps
// working unchanged — manifestErrorCode still records RECOGNIZE_FAILED — while the
// media pipeline can tell the two cases apart. The distinction is deliberate and
// narrow:
//
//   - A misconfigured or unreachable backend (bad URL, refused connection, non-200,
//     undecodable body, unreadable media) is a HARD per-document error, exactly as
//     before. Hard propagation is what stops a typo'd base_url from silently
//     indexing nothing, and every one of those failures is instant and explicit.
//   - A deadline expiry is the opposite evidence: something answered the startup
//     health probe, accepted the POST, and was still working on the media when the
//     clock ran out. Failing the whole document on that discards representations
//     the pipeline already produced (SPEC §7.7 status="error" hides every chunk of
//     the document from search, see store.liveParentDocument), which is how a
//     3h24m broadcast that had 892 indexed annotations ended up serving nothing.
var ErrRecognizeTimeout = fmt.Errorf("%w: recognition deadline exceeded", ErrRecognitionProviderFailure)

// RecognizeCallTimeout is the exported counterpart of recognizeCallTimeout so the
// tests/ tree can pin the arithmetic of the bound without reading the code.
func RecognizeCallTimeout(base time.Duration, perMediaSecond float64, media time.Duration) time.Duration {
	return recognizeCallTimeout(base, perMediaSecond, media)
}

// recognizeCallTimeout resolves the wall-clock bound for ONE /recognize call
// (#894). The bound is the LARGER of two parts:
//
//   - base (`recognize.timeout`): a flat floor. It alone governs short media and
//     media whose duration cannot be probed.
//   - media * perMediaSecond (`recognize.timeout_per_media_second`): wall-clock
//     seconds allowed per second of media. Recognition analyses frames across the
//     whole file, so its cost scales with duration; a flat ceiling that suits a
//     30-second clip cannot also suit a four-hour broadcast, which is the ceiling
//     that made the capability unusable on long media.
//
// Taking the maximum (rather than only the ratio) means the ratio can never make
// the bound TIGHTER than the flat floor, so lowering media duration or losing the
// probe never shortens a deadline. A non-positive base falls back to
// config.DefaultRecognizeTimeout, so the returned bound is always > 0: a
// /recognize call is never unbounded, whatever the config says.
func recognizeCallTimeout(base time.Duration, perMediaSecond float64, media time.Duration) time.Duration {
	if base <= 0 {
		base = config.DefaultRecognizeTimeout
	}
	if perMediaSecond <= 0 || media <= 0 {
		return base
	}
	scaled := float64(media) * perMediaSecond
	// A huge duration or ratio would overflow int64 and wrap to a negative
	// duration, i.e. an already-expired deadline. Clamp instead.
	if scaled >= math.MaxInt64 {
		return time.Duration(math.MaxInt64)
	}
	if d := time.Duration(scaled); d > base {
		return d
	}
	return base
}

// RecognizeServeClient talks to a locally served recognition backend
// (`recognize.provider: serve`, design 0004 §5) such as the reference
// `dirstral-annotator serve`. It follows the docling-serve conventions:
// operator-started process, base URL from config, bounded reads, and no
// response-body echo into errors.
type RecognizeServeClient struct {
	baseURL    string
	httpClient *http.Client
}

var _ model.Recognizer = (*RecognizeServeClient)(nil)

// NewRecognizeServeClient builds a serve-provider client. The client carries NO
// fixed http.Client.Timeout: the bound on one call is per-DOCUMENT (it scales with
// media duration, #894) and therefore arrives on the request context. A
// client-level ceiling would silently cap that context and reinstate the very
// ten-minute wall this closed, because whichever bound fires first wins.
func NewRecognizeServeClient(baseURL string) *RecognizeServeClient {
	return &RecognizeServeClient{
		baseURL:    strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		httpClient: &http.Client{},
	}
}

// recognizeWireResponse is the serve wire contract (design 0004 §5;
// draft schema 0004-recognize-response.schema.json in dirstral-spec).
type recognizeWireResponse struct {
	Recognizer struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	} `json:"recognizer"`
	Annotations []struct {
		StartS     float64  `json:"start_s"`
		EndS       float64  `json:"end_s"`
		Event      string   `json:"event"`
		Entities   []string `json:"entities"`
		Text       string   `json:"text"`
		Confidence float64  `json:"confidence"`
		Sources    []string `json:"sources"`
	} `json:"annotations"`
}

func (c *RecognizeServeClient) Recognize(ctx context.Context, absPath string) (model.RecognizeResult, error) {
	// The ingest service always supplies the per-document deadline. A caller that
	// does not (a bare client in a test or a tool) still gets a bound, so no call
	// can hang forever now that the client itself carries no Timeout.
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, config.DefaultRecognizeTimeout)
		defer cancel()
	}
	payload, err := json.Marshal(map[string]string{"path": absPath})
	if err != nil {
		return model.RecognizeResult{}, fmt.Errorf("marshal recognize request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/recognize", bytes.NewReader(payload))
	if err != nil {
		return model.RecognizeResult{}, fmt.Errorf("build recognize request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return model.RecognizeResult{}, fmt.Errorf("recognize backend request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		// Deliberately no body echo: backend errors may include local paths;
		// the status code is the diagnostic (docling-serve convention).
		return model.RecognizeResult{}, fmt.Errorf("recognize backend returned status %d", resp.StatusCode)
	}

	var wire recognizeWireResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, recognizeMaxResponseBytes)).Decode(&wire); err != nil {
		return model.RecognizeResult{}, fmt.Errorf("decode recognize response: %w", err)
	}
	out := model.RecognizeResult{Name: wire.Recognizer.Name, Version: wire.Recognizer.Version}
	for _, a := range wire.Annotations {
		out.Annotations = append(out.Annotations, model.RecognizedAnnotation{
			StartMS:    int(math.Round(a.StartS * 1000)),
			EndMS:      int(math.Round(a.EndS * 1000)),
			Event:      a.Event,
			Entities:   a.Entities,
			Text:       a.Text,
			Confidence: a.Confidence,
			Sources:    a.Sources,
		})
	}
	return out, nil
}

// SetRecognizer overrides the recognition binding (primarily for tests; the
// real binding is resolved from config in NewService).
func (s *Service) SetRecognizer(recognizer model.Recognizer) {
	s.recognizer = recognizer
}

const (
	// recognizeHealthWaitDefault bounds how long a freshly launched managed
	// backend may take to answer /health before startup fails.
	recognizeHealthWaitDefault = 30 * time.Second
	recognizeHealthPoll        = 250 * time.Millisecond
	// recognizeProbeTimeout bounds one /health round-trip.
	recognizeProbeTimeout = 3 * time.Second
	// recognizeStopGrace is how long a SIGTERM-ed managed backend gets to
	// exit before SIGKILL (mirrors the daemon's own shutdown grace idea).
	recognizeStopGrace = 5 * time.Second
)

// probeRecognizeHealth performs one GET {base_url}/health round-trip.
func probeRecognizeHealth(ctx context.Context, baseURL string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(strings.TrimSpace(baseURL), "/")+"/health", nil)
	if err != nil {
		return fmt.Errorf("build health request: %w", err)
	}
	client := &http.Client{Timeout: recognizeProbeTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("health returned status %d", resp.StatusCode)
	}
	return nil
}

// SetRecognizeHealthWait overrides the managed-backend health deadline
// (tests only).
func (s *Service) SetRecognizeHealthWait(d time.Duration) { s.recognizeHealthWait = d }

// RecognizeBackendPID reports the managed backend's process id (0 when none
// was launched). Exposed for tests and diagnostics.
func (s *Service) RecognizeBackendPID() int { return s.recognizeBackendPID }

// StartRecognizeBackend brings the recognition backend up for the daemon's
// lifetime (design 0004 §3). With recognize.serve_command configured,
// dir2mcp launches the command itself, waits for /health, and terminates
// the child when ctx is cancelled — `dir2mcp up` is the only process the
// operator runs. Without a command (connect-only), it probes the configured
// base URL once and logs a warning when unreachable; per-document ingest
// errors remain the hard signal. No-op when recognition is off.
func (s *Service) StartRecognizeBackend(ctx context.Context) error {
	if s.recognizer == nil {
		return nil
	}
	baseURL := s.cfg.RecognizeServeURL
	command := strings.TrimSpace(s.cfg.RecognizeServeCommand)
	if command == "" {
		if err := probeRecognizeHealth(ctx, baseURL); err != nil {
			s.getLogger().Printf("warning: recognize backend %s is not reachable yet: %v", baseURL, err)
		}
		return nil
	}

	cmd := exec.Command("sh", "-c", command)
	setRecognizeBackendProcAttr(cmd)
	logW := s.getLogger().Writer()
	cmd.Stdout, cmd.Stderr = logW, logW
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start recognize backend: %w", err)
	}
	s.recognizeBackendPID = cmd.Process.Pid

	var exitErr error
	exited := make(chan struct{})
	go func() {
		exitErr = cmd.Wait()
		close(exited)
	}()
	terminate := func() {
		_ = signalRecognizeBackend(cmd.Process.Pid, syscall.SIGTERM)
		select {
		case <-exited:
		case <-time.After(recognizeStopGrace):
			_ = signalRecognizeBackend(cmd.Process.Pid, syscall.SIGKILL)
		}
	}
	go func() {
		select {
		case <-ctx.Done():
			terminate()
		case <-exited:
		}
	}()

	wait := s.recognizeHealthWait
	if wait <= 0 {
		wait = recognizeHealthWaitDefault
	}
	return s.awaitRecognizeHealthy(ctx, baseURL, wait, cmd.Process.Pid, exited, &exitErr, terminate)
}

// awaitRecognizeHealthy polls GET {base_url}/health until the managed backend is
// up (returns nil), the deadline passes (terminates the child, returns an
// error), the child exits early (returns the exit error), or ctx is cancelled.
// exitErr is read only after the exited channel closes, so the read is safe.
func (s *Service) awaitRecognizeHealthy(ctx context.Context, baseURL string, wait time.Duration, pid int, exited <-chan struct{}, exitErr *error, terminate func()) error {
	deadline := time.Now().Add(wait)
	for {
		if err := probeRecognizeHealth(ctx, baseURL); err == nil {
			s.getLogger().Printf("recognize backend healthy at %s (pid %d)", baseURL, pid)
			return nil
		}
		if time.Now().After(deadline) {
			terminate()
			return fmt.Errorf("recognize backend did not become healthy at %s within %s", baseURL, wait)
		}
		select {
		case <-exited:
			return fmt.Errorf("recognize backend exited before becoming healthy: %v", *exitErr)
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(recognizeHealthPoll):
		}
	}
}

// recognitionMeta is the meta_json persisted on a recognition representation.
// Provider/model/model_version are the design 0004 §4 derivation identity
// fields, mirroring the STT transcript meta shape.
type recognitionMeta struct {
	Source       string `json:"source"`
	Provider     string `json:"provider,omitempty"`
	Model        string `json:"model,omitempty"`
	ModelVersion string `json:"model_version,omitempty"`
}

// RecognitionSegments is the exported counterpart of recognitionSegments,
// converting the unexported segment type to the public ChunkSegment. It exists
// so tests in the tests/ tree can assert what an annotation actually carries
// into persistence, which is where the entity attribution used to be silently
// dropped (design 0004 §7). The derivation hash input is returned alongside so
// a test can pin that a change in attribution re-derives the representation.
func RecognitionSegments(anns []model.RecognizedAnnotation) ([]ChunkSegment, string) {
	raw, hashInput := recognitionSegments(anns)
	out := make([]ChunkSegment, 0, len(raw))
	for _, seg := range raw {
		out = append(out, ChunkSegment(seg))
	}
	return out, hashInput
}

// recognitionSegments filters a backend's annotations to the well-formed ones,
// sorts them deterministically, and returns the time-spanned chunk segments plus
// the canonical string the derivation rep_hash is computed over.
//
// Malformed annotations are dropped (a served backend is untrusted input, so the
// core does not rely on the draft schema alone): empty or whitespace-only text
// (a non-searchable chunk), a negative start (the wire contract is 0-based, §5),
// or a reversed span (end before start). The survivors are sorted by
// (start, end, text, canonical line) so the rep_hash and the persisted chunk
// order are STABLE regardless of the order the backend emitted them in — the
// serve contract makes no ordering guarantee, and an order-only change MUST NOT
// flap the rep_hash and force a spurious re-derivation. The canonical line is
// the last key because bounds and text alone do not identify an annotation: two
// annotations can share all three and differ in their attribution.
func recognitionSegments(anns []model.RecognizedAnnotation) ([]chunkSegment, string) {
	type validAnnotation struct {
		startMS  int
		endMS    int
		text     string
		entities []string
		event    string
		sources  []string
		// hashLine is this annotation's whole contribution to the derivation
		// hash input, and also the final sort tie-break below.
		hashLine string
	}
	// canonicalLine renders one annotation as the line the rep_hash covers.
	//
	// The attribution joins the derivation hash: a backend that changes which
	// entities an annotation names, or which recognizer produced it, has
	// produced different content, and the representation must be re-derived
	// rather than kept because the prose happens to match (§8.6.7).
	//
	// Every variable-length field is LENGTH-PREFIXED rather than delimited.
	// Entity ids and recognizer tags are opaque backend-declared tokens, so any
	// delimiter can legitimately appear inside one: joining with commas would
	// encode ["a,b", "c"] and ["a", "b,c"] identically, and two genuinely
	// different attributions that hash the same would silently NOT re-derive.
	// That is the exact failure this input exists to prevent.
	//
	// The source group is APPENDED ONLY WHEN PRESENT, unlike the entity count.
	// The entity list ends at a known count, so a line either stops there or
	// continues with the source count: both readings are unambiguous. Emitting
	// "|0" instead would change the hash input of every annotation that carries
	// no sources, and that re-runs the recognition backend over a whole corpus
	// of video to store a field those annotations do not have.
	canonicalLine := func(v validAnnotation) string {
		var line strings.Builder
		fmt.Fprintf(&line, "%d|%d|%d:%s|%d:%s|%d",
			v.startMS, v.endMS, len(v.text), v.text, len(v.event), v.event, len(v.entities))
		for _, id := range v.entities {
			fmt.Fprintf(&line, "|%d:%s", len(id), id)
		}
		if len(v.sources) > 0 {
			fmt.Fprintf(&line, "|%d", len(v.sources))
			for _, source := range v.sources {
				fmt.Fprintf(&line, "|%d:%s", len(source), source)
			}
		}
		return line.String()
	}
	valid := make([]validAnnotation, 0, len(anns))
	for _, ann := range anns {
		text := strings.TrimSpace(ann.Text)
		if text == "" || ann.StartMS < 0 || ann.EndMS < ann.StartMS {
			continue
		}
		v := validAnnotation{
			startMS: ann.StartMS, endMS: ann.EndMS, text: text,
			// Carried, not dropped: these are what an entity filter selects on
			// (design 0004 §7). The backend is required to compute them, and
			// persisting only the text made the filter unimplementable.
			entities: model.NormalizeEntityIDs(ann.Entities),
			event:    strings.TrimSpace(ann.Event),
			// Carried for the same reason, but for a different question
			// (df-005 0.3.0 `sources`): entities and event say WHAT the
			// annotation is about, sources says WHICH recognizer said it. The
			// backend has always sent it and this function used to drop it, so
			// no client could tell a scorebug reading from a face match (#861).
			// Provenance only: nothing downstream ranks or filters on it.
			sources: model.NormalizeSources(ann.Sources),
		}
		v.hashLine = canonicalLine(v)
		valid = append(valid, v)
	}
	sort.Slice(valid, func(i, j int) bool {
		if valid[i].startMS != valid[j].startMS {
			return valid[i].startMS < valid[j].startMS
		}
		if valid[i].endMS != valid[j].endMS {
			return valid[i].endMS < valid[j].endMS
		}
		if valid[i].text != valid[j].text {
			return valid[i].text < valid[j].text
		}
		// Bounds and text do not identify an annotation: two of them can share
		// all three and still name different entities, a different event or a
		// different recognizer. sort.Slice is not stable, so without this final
		// key the emitted order (and with it the hash input) would depend on the
		// order the backend happened to send them in, and a re-ordered response
		// would re-derive the representation for no change. The canonical line
		// covers every field, so equal keys mean identical annotations and the
		// order between them cannot matter.
		return valid[i].hashLine < valid[j].hashLine
	})
	segments := make([]chunkSegment, 0, len(valid))
	var hashInput strings.Builder
	for _, v := range valid {
		segments = append(segments, chunkSegment{
			Text: v.text,
			Span: model.Span{
				Kind: "time", StartMS: v.startMS, EndMS: v.endMS,
				Entities: v.entities, Event: v.event, Sources: v.sources,
			},
		})
		hashInput.WriteString(v.hashLine)
		hashInput.WriteByte('\n')
	}
	return segments, hashInput.String()
}

// GenerateRecognitionRepresentation runs the configured recognition backend
// over a video document and persists the result as a `recognition`
// representation: one chunk per annotation statement, each with a single
// `time` span (design 0004 §4). No-op when no recognizer is bound or the
// document is not a video. A backend that returns zero annotations persists
// nothing — an empty representation would only inflate coverage.
func (s *Service) GenerateRecognitionRepresentation(ctx context.Context, doc model.Document) error {
	_, err := s.generateRecognitionRepresentation(ctx, doc)
	return err
}

// generateRecognitionRepresentation is GenerateRecognitionRepresentation plus
// whether a recognition representation was actually persisted. The media
// pipeline needs that signal because recognition is an independent
// representation source (§5.2 `recognize`): a video whose only available source
// is recognition must still count as searchable rather than being reported as
// representation-less (#622).
func (s *Service) generateRecognitionRepresentation(ctx context.Context, doc model.Document) (bool, error) {
	if s.repGen == nil || s.recognizer == nil || doc.DocType != "video" {
		return false, nil
	}
	// Recognition backends read the media from a real filesystem path, so the
	// media must be resolved through the CorpusFS rather than reconstructed from
	// RootDir+rel_path (#734): an object-store backend reports no local path
	// (DiscoveredFile.AbsPath == "") and its Walk ignores RootDir, so a joined
	// path names a file that does not exist and recognition fails on a corpus
	// that ingested fine. Localize returns the resolved in-root path with a
	// no-op cleanup for a local corpus (no copy, unchanged behavior) and a temp
	// download for an object store. The Recognizer contract is synchronous — the
	// backend has finished reading the file by the time Recognize returns — so
	// releasing the copy on return is safe.
	localPath, cleanup, err := s.corpusFS().Localize(ctx, doc.RelPath)
	if err != nil {
		// The backend was never reached, but the outcome for this document is the
		// same as a backend failure — no recognition, retriable next scan — so it
		// carries the same sentinel and classifies as RECOGNIZE_FAILED rather than
		// the generic EXTRACT_FAILED (parallel to the STT path, which tags a failed
		// audio-track extraction as a transcript provider failure).
		return false, fmt.Errorf("%w: localize %s: %w", ErrRecognitionProviderFailure, doc.RelPath, err)
	}
	defer cleanup()

	// The serve wire contract is an ABSOLUTE media path (design 0004 §5). LocalFS
	// already returns one; an object store's temp copy lands under StateDir, which
	// can be configured relative (e.g. "./.dir2mcp"), so absolutize before handing
	// the path over — a connect-only backend does not share the daemon's CWD.
	absPath, err := filepath.Abs(localPath)
	if err != nil {
		return false, fmt.Errorf("%w: resolve media path for %s: %w", ErrRecognitionProviderFailure, doc.RelPath, err)
	}

	// The bound on this call scales with the media's own duration (#894), so it is
	// resolved per document and carried on the context. The probe is best-effort:
	// an absent ffprobe or an undecodable input yields 0 and the flat
	// `recognize.timeout` floor governs alone.
	budget := recognizeCallTimeout(s.cfg.RecognizeTimeout, s.cfg.RecognizeTimeoutPerMediaSecond,
		s.recognizeMediaDuration(ctx, absPath))
	callCtx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()

	result, err := s.recognizer.Recognize(callCtx, absPath)
	if err != nil {
		// Our own budget expired while the PARENT context is still live: the backend
		// was reachable and working, it just did not finish in the wall clock it was
		// given. Tag it so the media pipeline degrades instead of failing the whole
		// document (#894). The parent-context check matters: a cancelled or expired
		// parent means the DAEMON is going away (shutdown, an outer deadline), which
		// is not evidence about the backend, so that keeps the hard path below.
		if callCtx.Err() == context.DeadlineExceeded && ctx.Err() == nil {
			return false, fmt.Errorf("%w: recognize %s after %s: %w", ErrRecognizeTimeout, doc.RelPath, budget, err)
		}
		// Tag with the provider-failure sentinel so a transient recognize-backend
		// failure classifies as RECOGNIZE_FAILED (manifestErrorCode), parallel to
		// the STT/OCR provider sentinels — not the generic EXTRACT_FAILED.
		return false, fmt.Errorf("%w: recognize %s: %w", ErrRecognitionProviderFailure, doc.RelPath, err)
	}

	segments, hashInput := recognitionSegments(result.Annotations)
	if len(segments) == 0 {
		return false, nil
	}

	// #681: recognition statements are text this daemon derives from the media and
	// then indexes, so they are screened before persistence. The scan runs over the
	// segment texts exactly as they will be chunked, not over hashInput, which
	// interleaves length prefixes for a different purpose. On a match the document
	// is already withheld, so this reports "nothing recognized".
	if s.screenDerivedSecrets(ctx, doc, derivedKindRecognition, joinSegmentTexts(segments)) {
		return false, nil
	}

	meta, err := json.Marshal(recognitionMeta{
		Source:       recognizeSource,
		Provider:     s.cfg.RecognizeProvider,
		Model:        result.Name,
		ModelVersion: result.Version,
	})
	if err != nil {
		return false, fmt.Errorf("marshal recognition meta: %w", err)
	}

	rep := model.Representation{
		DocID:       doc.DocID,
		RepType:     RepTypeRecognition,
		RepHash:     computeRepHash([]byte(hashInput)),
		MetaJSON:    string(meta),
		CreatedUnix: time.Now().Unix(),
		Deleted:     false,
	}
	err = s.repGen.store.WithTx(ctx, func(tx model.RepresentationStore) error {
		repID, upsertErr := tx.UpsertRepresentation(ctx, rep)
		if upsertErr != nil {
			return fmt.Errorf("upsert recognition representation: %w", upsertErr)
		}
		return s.repGen.upsertChunksForRepresentationWithStore(ctx, tx, repID, "text", segments, quarantineDecision{})
	})
	if err != nil {
		return false, err
	}
	s.addRepresentations(1)
	return true, nil
}

// recognizeMediaDuration resolves the media's duration for the duration-scaled
// /recognize bound (#894). It reuses the service's injectable probe
// (ProbeDurationFunc, default avutil.Duration) and takes the ALREADY-localized
// absolute path, so it never localizes the media a second time.
//
// Best-effort by design: an absent ffprobe, an undecodable container, or a
// non-positive result yields 0, which makes recognizeCallTimeout fall back to the
// flat floor. A failed probe must never fail recognition.
func (s *Service) recognizeMediaDuration(ctx context.Context, absPath string) time.Duration {
	probe := s.ProbeDurationFunc
	if probe == nil {
		probe = avutil.Duration
	}
	d, err := probe(ctx, absPath)
	if err != nil || d <= 0 {
		return 0
	}
	return d
}
