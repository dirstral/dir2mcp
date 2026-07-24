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
	"strings"
	"syscall"
	"time"

	"github.com/dirstral/dir2mcp/internal/model"
)

// recognizeSource is the meta_json source value recorded on a recognition
// representation (design 0004 §4). Like an STT transcript — and unlike an
// authored sidecar — it is model-derived and carries the backend's
// derivation identity.
const recognizeSource = "recognize"

const (
	// recognizeServeTimeout bounds one /recognize call. Recognition samples
	// frames across a whole video, so this mirrors the docling-serve ceiling
	// rather than the API-provider default.
	recognizeServeTimeout = 10 * time.Minute
	// recognizeMaxResponseBytes bounds the decoded response body.
	recognizeMaxResponseBytes = 64 << 20
)

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

func NewRecognizeServeClient(baseURL string) *RecognizeServeClient {
	return &RecognizeServeClient{
		baseURL:    strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		httpClient: &http.Client{Timeout: recognizeServeTimeout},
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
	deadline := time.Now().Add(wait)
	for {
		if err := probeRecognizeHealth(ctx, baseURL); err == nil {
			s.getLogger().Printf("recognize backend healthy at %s (pid %d)", baseURL, cmd.Process.Pid)
			return nil
		}
		if time.Now().After(deadline) {
			terminate()
			return fmt.Errorf("recognize backend did not become healthy at %s within %s", baseURL, wait)
		}
		select {
		case <-exited:
			return fmt.Errorf("recognize backend exited before becoming healthy: %v", exitErr)
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

// GenerateRecognitionRepresentation runs the configured recognition backend
// over a video document and persists the result as a `recognition`
// representation: one chunk per annotation statement, each with a single
// `time` span (design 0004 §4). No-op when no recognizer is bound or the
// document is not a video. A backend that returns zero annotations persists
// nothing — an empty representation would only inflate coverage.
func (s *Service) GenerateRecognitionRepresentation(ctx context.Context, doc model.Document) error {
	if s.repGen == nil || s.recognizer == nil || doc.DocType != "video" {
		return nil
	}
	absPath := filepath.Join(s.cfg.RootDir, filepath.FromSlash(doc.RelPath))
	result, err := s.recognizer.Recognize(ctx, absPath)
	if err != nil {
		return fmt.Errorf("recognize %s: %w", doc.RelPath, err)
	}

	segments := make([]chunkSegment, 0, len(result.Annotations))
	var hashInput strings.Builder
	for _, ann := range result.Annotations {
		text := strings.TrimSpace(ann.Text)
		if text == "" || ann.EndMS < ann.StartMS {
			continue
		}
		segments = append(segments, chunkSegment{
			Text: text,
			Span: model.Span{Kind: "time", StartMS: ann.StartMS, EndMS: ann.EndMS},
		})
		fmt.Fprintf(&hashInput, "%d|%d|%s\n", ann.StartMS, ann.EndMS, text)
	}
	if len(segments) == 0 {
		return nil
	}

	meta, err := json.Marshal(recognitionMeta{
		Source:       recognizeSource,
		Provider:     s.cfg.RecognizeProvider,
		Model:        result.Name,
		ModelVersion: result.Version,
	})
	if err != nil {
		return fmt.Errorf("marshal recognition meta: %w", err)
	}

	rep := model.Representation{
		DocID:       doc.DocID,
		RepType:     RepTypeRecognition,
		RepHash:     computeRepHash([]byte(hashInput.String())),
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
		return err
	}
	s.addRepresentations(1)
	return nil
}
