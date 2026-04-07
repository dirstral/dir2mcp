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

// unavailableCode returns the appropriate "unavailable" error code for the operation.
func unavailableCode(operation string) string {
	if operation == "settle" {
		return CodePaymentSettlementUnavailable
	}
	return CodePaymentFacilitatorUnavailable
}

// failedCode returns the appropriate "failed" error code for the operation.
func failedCode(operation string) string {
	if operation == "settle" {
		return CodePaymentSettlementFailed
	}
	return CodePaymentInvalid
}

// isContextuallyRetryable returns false when err represents a context
// cancellation or deadline, which should not be retried.
func isContextuallyRetryable(err error, req *http.Request) bool {
	return !errors.Is(err, context.Canceled) &&
		!errors.Is(err, context.DeadlineExceeded) &&
		req.Context().Err() == nil
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
			Code:      unavailableCode(operation),
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
		return nil, &FacilitatorError{
			Operation: operation,
			Code:      unavailableCode(operation),
			Message:   "facilitator request failed",
			Retryable: isContextuallyRetryable(err, httpReq),
			Cause:     err,
		}
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	const maxRespSize = 1 << 20
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxRespSize+1))
	if err != nil {
		// reading the response failed; treat as retryable transient failure.
		return nil, &FacilitatorError{
			Operation: operation,
			Code:      unavailableCode(operation),
			Message:   "failed to read facilitator response",
			Retryable: true,
			Cause:     err,
		}
	}
	if len(respBody) > maxRespSize {
		return nil, &FacilitatorError{
			Operation: operation,
			Code:      unavailableCode(operation),
			Message:   fmt.Sprintf("facilitator response exceeds maximum size (%d bytes)", maxRespSize),
			Retryable: false,
		}
	}
	normalized, isFallback := normalizeResponsePayload(respBody)

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		if isFallback {
			return nil, &FacilitatorError{
				Operation:  operation,
				StatusCode: resp.StatusCode,
				Code:       unavailableCode(operation),
				Message:    "malformed facilitator response",
				Retryable:  false,
			}
		}
		return normalized, nil
	}

	retryable := isRetryableStatus(resp.StatusCode)
	code := failedCode(operation)
	if retryable {
		code = unavailableCode(operation)
	}

	return nil, &FacilitatorError{
		Operation:  operation,
		StatusCode: resp.StatusCode,
		Retryable:  retryable,
		Code:       code,
		Message:    fmt.Sprintf("facilitator %s request failed with status %d", operation, resp.StatusCode),
	}
}

func toRequirementPayload(req Requirement) map[string]interface{} {
	m := map[string]interface{}{
		"scheme":   strings.ToLower(strings.TrimSpace(req.Scheme)),
		"network":  strings.TrimSpace(req.Network),
		"amount":   strings.TrimSpace(req.Amount),
		"asset":    strings.TrimSpace(req.Asset),
		"payTo":    strings.TrimSpace(req.PayTo),
		"resource": strings.TrimSpace(req.Resource),
	}
	if max := strings.TrimSpace(req.MaxAmountRequired); max != "" {
		m["maxAmountRequired"] = max
	}
	return m
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

// normalizeResponsePayload parses the raw facilitator response body into a
// json.RawMessage. The bool return is true when the body was non-empty but
// not valid JSON (a fallback {"raw":...} wrapper is NOT returned in that case;
// callers should treat isFallback=true as an error on the success path).
func normalizeResponsePayload(payload []byte) (json.RawMessage, bool) {
	trimmed := bytes.TrimSpace(payload)
	if len(trimmed) == 0 {
		return json.RawMessage(`{}`), false
	}

	var check json.RawMessage
	if err := json.Unmarshal(trimmed, &check); err == nil {
		return json.RawMessage(trimmed), false
	}

	return nil, true
}
