package elevenlabs

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"dir2mcp/internal/model"
)

const (
	defaultBaseURL  = "https://api.elevenlabs.io"
	defaultTimeout  = 30 * time.Second
	defaultSTTModel = "scribe_v1"
)

type Client struct {
	APIKey                 string
	BaseURL                string
	HTTPClient             *http.Client
	VoiceID                string
	TranscribeModel        string
	TranscribeLanguageCode string
}

type synthesizeRequest struct {
	Text string `json:"text"`
}

func NewClient(apiKey, voiceID string) *Client {
	baseURL := strings.TrimRight(strings.TrimSpace(defaultBaseURL), "/")
	return &Client{
		APIKey:          strings.TrimSpace(apiKey),
		BaseURL:         baseURL,
		HTTPClient:      &http.Client{Timeout: defaultTimeout},
		VoiceID:         strings.TrimSpace(voiceID),
		TranscribeModel: defaultSTTModel,
	}
}

type transcribeResponse struct {
	Text       string `json:"text"`
	Transcript string `json:"transcript"`
	Segments   []struct {
		Text    string  `json:"text"`
		Start   float64 `json:"start"`
		StartMS float64 `json:"start_ms"`
	} `json:"segments"`
}

func (c *Client) Synthesize(ctx context.Context, text string) ([]byte, error) {
	return c.SynthesizeWithVoice(ctx, text, c.VoiceID)
}

func (c *Client) Transcribe(ctx context.Context, relPath string, data []byte) (string, error) {
	apiKey := strings.TrimSpace(c.APIKey)
	if apiKey == "" {
		return "", &model.ProviderError{
			Code:      "ELEVENLABS_AUTH",
			Message:   "missing ElevenLabs API key",
			Retryable: false,
		}
	}
	if len(data) == 0 {
		return "", &model.ProviderError{
			Code:      "ELEVENLABS_FAILED",
			Message:   "transcription input is empty",
			Retryable: false,
		}
	}

	fileName := strings.TrimSpace(filepath.Base(relPath))
	if fileName == "" || fileName == "." || fileName == string(filepath.Separator) {
		fileName = "audio.wav"
	}

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("file", fileName)
	if err != nil {
		return "", &model.ProviderError{Code: "ELEVENLABS_FAILED", Message: "failed to build STT request body", Retryable: false, Cause: err}
	}
	if _, err := part.Write(data); err != nil {
		return "", &model.ProviderError{Code: "ELEVENLABS_FAILED", Message: "failed to write STT input", Retryable: false, Cause: err}
	}

	modelName := strings.TrimSpace(c.TranscribeModel)
	if modelName == "" {
		modelName = defaultSTTModel
	}
	if err := writer.WriteField("model_id", modelName); err != nil {
		return "", &model.ProviderError{Code: "ELEVENLABS_FAILED", Message: "failed to set STT model", Retryable: false, Cause: err}
	}
	if languageCode := strings.TrimSpace(c.TranscribeLanguageCode); languageCode != "" {
		if err := writer.WriteField("language_code", languageCode); err != nil {
			return "", &model.ProviderError{Code: "ELEVENLABS_FAILED", Message: "failed to set STT language", Retryable: false, Cause: err}
		}
	}
	if err := writer.Close(); err != nil {
		return "", &model.ProviderError{Code: "ELEVENLABS_FAILED", Message: "failed to finalize STT request body", Retryable: false, Cause: err}
	}

	baseURL := strings.TrimSpace(c.BaseURL)
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	baseURL = strings.TrimRight(baseURL, "/")

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/v1/speech-to-text", bytes.NewReader(body.Bytes()))
	if err != nil {
		return "", &model.ProviderError{Code: "ELEVENLABS_FAILED", Message: "failed to build STT request", Retryable: false, Cause: err}
	}
	req.Header.Set("xi-api-key", apiKey)
	req.Header.Set("Content-Type", writer.FormDataContentType())

	httpClient := c.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: defaultTimeout}
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", &model.ProviderError{Code: "ELEVENLABS_FAILED", Message: "stt request failed", Retryable: true, Cause: err}
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", &model.ProviderError{Code: "ELEVENLABS_FAILED", Message: "failed to read STT response", Retryable: true, StatusCode: resp.StatusCode, Cause: err}
	}

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		message := strings.TrimSpace(string(bodyBytes))
		if message == "" {
			message = fmt.Sprintf("elevenlabs stt returned status %d", resp.StatusCode)
		}
		return "", mapProviderError(resp.StatusCode, message)
	}

	var parsed transcribeResponse
	if err := json.Unmarshal(bodyBytes, &parsed); err != nil {
		return "", &model.ProviderError{Code: "ELEVENLABS_FAILED", Message: "failed to decode STT response", Retryable: false, Cause: err}
	}

	if len(parsed.Segments) > 0 {
		lines := make([]string, 0, len(parsed.Segments))
		for _, segment := range parsed.Segments {
			text := strings.TrimSpace(segment.Text)
			if text == "" {
				continue
			}
			startMS := int(segment.StartMS)
			if startMS <= 0 {
				startMS = int(segment.Start * 1000)
			}
			mm := (startMS / 1000) / 60
			ss := (startMS / 1000) % 60
			lines = append(lines, "["+pad2(mm)+":"+pad2(ss)+"] "+text)
		}
		if len(lines) > 0 {
			return strings.Join(lines, "\n"), nil
		}
	}

	text := strings.TrimSpace(parsed.Text)
	if text == "" {
		text = strings.TrimSpace(parsed.Transcript)
	}
	if text == "" {
		return "", &model.ProviderError{Code: "ELEVENLABS_FAILED", Message: "stt response had no text content", Retryable: false}
	}
	return text, nil
}

func (c *Client) SynthesizeWithVoice(ctx context.Context, text, voiceID string) ([]byte, error) {
	apiKey := strings.TrimSpace(c.APIKey)
	if apiKey == "" {
		return nil, &model.ProviderError{
			Code:      "ELEVENLABS_AUTH",
			Message:   "missing ElevenLabs API key",
			Retryable: false,
		}
	}

	voiceID = strings.TrimSpace(voiceID)
	if voiceID == "" {
		return nil, &model.ProviderError{
			Code:      "ELEVENLABS_FAILED",
			Message:   "voice_id is required",
			Retryable: false,
		}
	}

	text = strings.TrimSpace(text)
	if text == "" {
		return nil, &model.ProviderError{
			Code:      "ELEVENLABS_FAILED",
			Message:   "text is required",
			Retryable: false,
		}
	}

	payload, err := json.Marshal(synthesizeRequest{Text: text})
	if err != nil {
		return nil, &model.ProviderError{
			Code:      "ELEVENLABS_FAILED",
			Message:   "failed to marshal TTS request",
			Retryable: false,
			Cause:     err,
		}
	}

	baseURL := strings.TrimSpace(c.BaseURL)
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	baseURL = strings.TrimRight(baseURL, "/")

	reqURL := baseURL + "/v1/text-to-speech/" + url.PathEscape(voiceID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, reqURL, bytes.NewReader(payload))
	if err != nil {
		return nil, &model.ProviderError{
			Code:      "ELEVENLABS_FAILED",
			Message:   "failed to build TTS request",
			Retryable: false,
			Cause:     err,
		}
	}
	req.Header.Set("xi-api-key", apiKey)
	req.Header.Set("Accept", "audio/mpeg")
	req.Header.Set("Content-Type", "application/json")

	httpClient := c.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: defaultTimeout}
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, &model.ProviderError{
			Code:      "ELEVENLABS_FAILED",
			Message:   "tts request failed",
			Retryable: true,
			Cause:     err,
		}
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, &model.ProviderError{
			Code:       "ELEVENLABS_FAILED",
			Message:    "failed to read TTS response",
			Retryable:  true,
			StatusCode: resp.StatusCode,
			Cause:      err,
		}
	}

	if resp.StatusCode >= http.StatusOK && resp.StatusCode < http.StatusMultipleChoices {
		return body, nil
	}

	message := strings.TrimSpace(string(body))
	if message == "" {
		message = fmt.Sprintf("elevenlabs tts returned status %d", resp.StatusCode)
	}
	return nil, mapProviderError(resp.StatusCode, message)
}

func mapProviderError(statusCode int, message string) error {
	pe := &model.ProviderError{
		Code:       "ELEVENLABS_FAILED",
		Message:    message,
		Retryable:  false,
		StatusCode: statusCode,
	}

	switch {
	case statusCode == http.StatusUnauthorized || statusCode == http.StatusForbidden:
		pe.Code = "ELEVENLABS_AUTH"
		pe.Retryable = false
	case statusCode == http.StatusTooManyRequests:
		pe.Code = "ELEVENLABS_RATE_LIMIT"
		pe.Retryable = true
	case statusCode >= http.StatusInternalServerError:
		pe.Code = "ELEVENLABS_FAILED"
		pe.Retryable = true
	case statusCode >= http.StatusBadRequest && statusCode < http.StatusInternalServerError:
		pe.Code = "ELEVENLABS_FAILED"
		pe.Retryable = false
	default:
		pe.Code = "ELEVENLABS_FAILED"
		pe.Retryable = true
	}

	return pe
}

func pad2(n int) string {
	if n < 10 {
		return "0" + strconv.Itoa(n)
	}
	return strconv.Itoa(n)
}
