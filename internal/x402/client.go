package x402

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const defaultHTTPTimeout = 10 * time.Second

type HTTPClient struct {
	baseURL     string
	bearerToken string
	httpClient  *http.Client
}

func NewHTTPClient(baseURL, bearerToken string, httpClient *http.Client) *HTTPClient {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if httpClient == nil {
		httpClient = &http.Client{Timeout: defaultHTTPTimeout}
	}
	return &HTTPClient{
		baseURL:     baseURL,
		bearerToken: strings.TrimSpace(bearerToken),
		httpClient:  httpClient,
	}
}

func (c *HTTPClient) Verify(ctx context.Context, paymentSignature string, req Requirement) (json.RawMessage, error) {
	return c.do(ctx, "verify", paymentSignature, req)
}

func (c *HTTPClient) Settle(ctx context.Context, paymentSignature string, req Requirement) (json.RawMessage, error) {
	return c.do(ctx, "settle", paymentSignature, req)
}

func (c *HTTPClient) do(ctx context.Context, operation, paymentSignature string, req Requirement) (json.RawMessage, error) {
	// constructor already trims/normalizes baseURL, so a simple empty
	// comparison is sufficient here.
	if c.baseURL == "" {
		return nil, &FacilitatorError{
			Operation: operation,
			Code:      CodePaymentConfigInvalid,
			Message:   "x402 facilitator URL is required",
			Retryable: false,
		}
	}
	if err := req.Validate(); err != nil {
		return nil, &FacilitatorError{
			Operation: operation,
			Code:      CodePaymentConfigInvalid,
			Message:   err.Error(),
			Retryable: false,
			Cause:     err,
		}
	}
	paymentSignature = strings.TrimSpace(paymentSignature)
	if paymentSignature == "" {
		return nil, &FacilitatorError{
			Operation: operation,
			Code:      CodePaymentRequired,
			Message:   "missing payment signature",
			Retryable: false,
		}
	}

	endpoint, err := url.JoinPath(c.baseURL, "v2", "x402", operation)
	if err != nil {
		return nil, &FacilitatorError{
			Operation: operation,
			Code:      CodePaymentConfigInvalid,
			Message:   "invalid facilitator URL",
			Retryable: false,
			Cause:     err,
		}
	}

	body := map[string]interface{}{
		"paymentPayload": paymentSignature,
		"paymentRequirements": []map[string]interface{}{
			toRequirementPayload(req),
		},
	}
	rawBody, err := json.Marshal(body)
	if err != nil {
		return nil, &FacilitatorError{
			Operation: operation,
			Code:      CodePaymentConfigInvalid,
			Message:   "failed to serialize facilitator request",
			Retryable: false,
			Cause:     err,
		}
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(rawBody))
	if err != nil {
		// request construction failures are programming/validation issues; not
		// retryable since a retry will never succeed.
		return nil, &FacilitatorError{
			Operation: operation,
			Code:      CodePaymentFacilitatorUnavailable,
			Message:   "failed to create facilitator request",
			Retryable: false,
			Cause:     err,
		}
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")
	if c.bearerToken != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.bearerToken)
	}

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		code := CodePaymentFacilitatorUnavailable
		if operation == "settle" {
			code = CodePaymentSettlementUnavailable
		}
		retryable := true
		// context cancellation or deadline errors should not be retried
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) ||
			httpReq.Context().Err() != nil {
			retryable = false
		}
		return nil, &FacilitatorError{
			Operation: operation,
			Code:      code,
			Message:   "facilitator request failed",
			Retryable: retryable,
			Cause:     err,
		}
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	const maxRespSize = 1 << 20
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxRespSize+1))
	if err != nil {
		// reading the response failed; wrap in a FacilitatorError so callers
		// can handle it like other transport-level failures.  This situation
		// is unlikely but we treat it as retryable since it usually indicates
		// a transient network or server problem.
		return nil, &FacilitatorError{
			Operation: operation,
			Code:      CodePaymentFacilitatorUnavailable,
			Message:   "failed to read facilitator response",
			Retryable: true,
			Cause:     err,
		}
	}
	if len(respBody) > maxRespSize {
		// The response body was truncated by LimitReader above, so we only
		// examine the first maxRespSize+1 bytes. Treat over-limit responses as
		// deterministic validation failures rather than applying content-based
		// heuristics.
		return nil, &FacilitatorError{
			Operation: operation,
			Code:      CodePaymentFacilitatorUnavailable,
			Message:   fmt.Sprintf("facilitator response exceeds maximum size (%d bytes)", maxRespSize),
			Retryable: false,
		}
	}
	normalized := normalizeResponsePayload(respBody)

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return normalized, nil
	}

	retryable := isRetryableStatus(resp.StatusCode)
	code := CodePaymentInvalid
	if operation == "settle" {
		code = CodePaymentSettlementFailed
	}
	if retryable {
		if operation == "settle" {
			code = CodePaymentSettlementUnavailable
		} else {
			code = CodePaymentFacilitatorUnavailable
		}
	}

	message := strings.TrimSpace(extractFacilitatorMessage(respBody))
	if message == "" {
		message = fmt.Sprintf("facilitator %s request failed with status %d", operation, resp.StatusCode)
	}

	// redact the body before exposing it in the error object.  We still
	// preserve a human-readable summary via extractFacilitatorMessage above,
	// but the raw payload may contain secrets that should not leak.
	return nil, &FacilitatorError{
		Operation:  operation,
		StatusCode: resp.StatusCode,
		Retryable:  retryable,
		Code:       code,
		Message:    message,
		Body:       redactNormalizedPayload(normalized),
	}
}

func toRequirementPayload(req Requirement) map[string]interface{} {
	return map[string]interface{}{
		"scheme":            strings.ToLower(strings.TrimSpace(req.Scheme)),
		"network":           strings.TrimSpace(req.Network),
		"amount":            strings.TrimSpace(req.Amount),
		"maxAmountRequired": strings.TrimSpace(req.MaxAmountRequired),
		"asset":             strings.TrimSpace(req.Asset),
		"payTo":             strings.TrimSpace(req.PayTo),
		"resource":          strings.TrimSpace(req.Resource),
	}
}

func isRetryableStatus(status int) bool {
	if status >= 500 {
		return true
	}
	switch status {
	case http.StatusRequestTimeout, http.StatusTooManyRequests, http.StatusTooEarly:
		return true
	default:
		return false
	}
}

const maxFacilitatorBody = 1024

func normalizeResponsePayload(payload []byte) json.RawMessage {
	trimmed := bytes.TrimSpace(payload)
	if len(trimmed) == 0 {
		return json.RawMessage(`{}`)
	}

	var check json.RawMessage
	if err := json.Unmarshal(trimmed, &check); err == nil {
		return json.RawMessage(trimmed)
	}

	fallback, _ := json.Marshal(map[string]string{
		"raw": string(trimmed),
	})
	return json.RawMessage(fallback)
}

// redactNormalizedPayload returns a safe string representation of the
// normalized JSON returned by the facilitator.  The goal is to avoid
// including potentially sensitive information (payment payloads,
// tokens, etc.) and to prevent unbounded logging of very large error
// responses.  It attempts to parse the payload and redact a handful of
// common keys, then truncates the result to a reasonable maximum length.
// list of keys whose values should be masked when found in a JSON
// object.  Note that keys are normalized exactly; casing matters, but the
// set includes both snake_case and camelCase forms used in various APIs.
var sensitiveKeys = map[string]struct{}{
	"paymentPayload":      {},
	"token":               {},
	"secret":              {},
	"password":            {},
	"authorization":       {},
	"authorizationHeader": {},
	"api_key":             {},
	"apiKey":              {},
	"access_token":        {},
	"refresh_token":       {},
	"credential":          {},
	"auth":                {},
	"bearer":              {},
}

// redactRecursive walks v (which may be a map, slice, or other value) and
// replaces any values associated with sensitive keys with the string
// "[REDACTED]".  Non-map/slice values are left untouched.  The function is
// safe when called with nil or unknown types.
func redactRecursive(v interface{}) {
	switch t := v.(type) {
	case map[string]interface{}:
		for k, val := range t {
			if _, ok := sensitiveKeys[k]; ok {
				t[k] = "[REDACTED]"
			} else {
				redactRecursive(val)
			}
		}
	case []interface{}:
		for i := range t {
			redactRecursive(t[i])
		}
	default:
		// primitives and other types are ignored
	}
}

func redactNormalizedPayload(normalized json.RawMessage) string {
	s := string(normalized)
	if s == "" {
		return ""
	}

	var obj interface{}
	if err := json.Unmarshal(normalized, &obj); err == nil {
		// we expect a JSON object most of the time, but our helper can handle
		// arrays or other top-level types as well.
		redactRecursive(obj)
		if data, err := json.Marshal(obj); err == nil {
			s = string(data)
		}
	}
	return truncateString(s, maxFacilitatorBody)
}

func extractFacilitatorMessage(payload []byte) string {
	trimmed := bytes.TrimSpace(payload)
	if len(trimmed) == 0 {
		return ""
	}

	var asObj map[string]interface{}
	if err := json.Unmarshal(trimmed, &asObj); err != nil {
		// failure to parse means we can't rely on structured keys; return
		// a truncated, UTF-8-safe rendition of the raw payload so that very
		// large or sensitive blobs are not accidentally logged or surfaced.
		return truncateString(string(trimmed), 256)
	}
	for _, key := range []string{"message", "error", "reason"} {
		if raw, ok := asObj[key]; ok {
			switch value := raw.(type) {
			case string:
				// always truncate extracted strings to avoid unbounded length
				return truncateString(value, 256)
			case map[string]interface{}:
				if msg, ok := value["message"].(string); ok {
					return truncateString(msg, 256)
				}
			}
		}
	}
	return ""
}

// truncateString returns a UTF-8-safe slice of s limited to max runes. If
// the input exceeds the limit it appends an ellipsis indicator. This is used
// when dumping unparsed payloads so we don't expose huge or sensitive content.
func truncateString(s string, max int) string {
	if len(s) == 0 || max <= 0 {
		return ""
	}
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max]) + "… (truncated)"
}
