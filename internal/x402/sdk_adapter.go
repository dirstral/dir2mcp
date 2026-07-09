package x402

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	sdkhttp "github.com/x402-foundation/x402/go/http"
)

const defaultHTTPTimeout = 10 * time.Second

// NewFacilitatorClient constructs the x402 SDK-backed facilitator client
// (github.com/x402-foundation/x402/go). The httpClient argument is forwarded
// to the SDK client; pass nil to use the package default timeout.
func NewFacilitatorClient(baseURL, bearerToken string, httpClient *http.Client) FacilitatorClient {
	return newSDKFacilitatorClient(baseURL, bearerToken, httpClient)
}

type sdkAdapterClient struct {
	baseURL     string
	facilitator *sdkhttp.HTTPFacilitatorClient
}

type sdkBearerAuthProvider struct {
	token string
}

func (p sdkBearerAuthProvider) GetAuthHeaders(context.Context) (sdkhttp.AuthHeaders, error) {
	headers := map[string]string{"Authorization": "Bearer " + p.token}
	return sdkhttp.AuthHeaders{
		Verify:    headers,
		Settle:    headers,
		Supported: headers,
		Bazaar:    headers,
	}, nil
}

func newSDKFacilitatorClient(baseURL, bearerToken string, httpClient *http.Client) FacilitatorClient {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if httpClient == nil {
		httpClient = &http.Client{Timeout: defaultHTTPTimeout}
	}
	if baseURL == "" {
		return &sdkAdapterClient{
			baseURL: baseURL,
		}
	}

	hasCredential := strings.TrimSpace(bearerToken) != ""
	sdkURL, ok := normalizeSDKFacilitatorURL(baseURL, hasCredential)
	if !ok {
		return &sdkAdapterClient{baseURL: ""}
	}
	cfg := &sdkhttp.FacilitatorConfig{
		URL:        sdkURL,
		HTTPClient: httpClient,
		Timeout:    defaultHTTPTimeout,
	}
	if token := strings.TrimSpace(bearerToken); token != "" {
		cfg.AuthProvider = sdkBearerAuthProvider{token: token}
	}

	return &sdkAdapterClient{
		baseURL:     baseURL,
		facilitator: sdkhttp.NewFacilitatorClient(cfg),
	}
}

func normalizeSDKFacilitatorURL(baseURL string, hasCredential bool) (string, bool) {
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", false
	}
	// Transport security (bs-010 / x402 adapter spec): refuse to build a
	// facilitator client that would send a bearer token or payment payload over
	// plaintext http to a credentialed or non-loopback endpoint. https is
	// required whenever the connection is credentialed OR the host is
	// non-loopback; plaintext http is accepted only for a credential-less
	// loopback host. Returning ok=false yields a client whose calls fail closed
	// with PAYMENT_CONFIG_INVALID rather than leaking the credential.
	if !isTransportSecure(parsed, hasCredential) {
		return "", false
	}
	trimmedPath := strings.TrimSuffix(parsed.Path, "/")
	if !strings.HasSuffix(trimmedPath, "/v2/x402") {
		joined, joinErr := url.JoinPath(trimmedPath, "/v2/x402")
		if joinErr != nil {
			return "", false
		}
		parsed.Path = joined
	}
	return strings.TrimRight(parsed.String(), "/"), true
}

// isTransportSecure reports whether the parsed facilitator URL is acceptable for
// the adapter->facilitator transport given whether a credential is attached.
// https is always acceptable; plaintext http is acceptable only for a
// credential-less loopback host.
func isTransportSecure(parsed *url.URL, hasCredential bool) bool {
	switch strings.ToLower(parsed.Scheme) {
	case "https":
		return true
	case "http":
		if hasCredential {
			return false
		}
		return isLoopbackHost(parsed.Hostname())
	default:
		return false
	}
}

// isLoopbackHost reports whether host refers to the local machine: an IP in the
// IPv4 127.0.0.0/8 block, the IPv6 ::1 address, or the "localhost" name.
func isLoopbackHost(host string) bool {
	host = strings.TrimSpace(host)
	if host == "" {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

func (c *sdkAdapterClient) Verify(ctx context.Context, paymentSignature string, req Requirement) (json.RawMessage, error) {
	return c.do(ctx, "verify", paymentSignature, req)
}

func (c *sdkAdapterClient) Settle(ctx context.Context, paymentSignature string, req Requirement) (json.RawMessage, error) {
	return c.do(ctx, "settle", paymentSignature, req)
}

func (c *sdkAdapterClient) do(ctx context.Context, operation, paymentSignature string, req Requirement) (json.RawMessage, error) {
	if c == nil || strings.TrimSpace(c.baseURL) == "" || c.facilitator == nil {
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

	payloadBytes, err := buildSDKPaymentPayloadBytes(paymentSignature, req)
	if err != nil {
		return nil, &FacilitatorError{
			Operation: operation,
			Code:      CodePaymentConfigInvalid,
			Message:   "failed to serialize x402 payment payload",
			Retryable: false,
			Cause:     err,
		}
	}
	requirementsBytes, err := buildSDKPaymentRequirementsBytes(req)
	if err != nil {
		return nil, &FacilitatorError{
			Operation: operation,
			Code:      CodePaymentConfigInvalid,
			Message:   "failed to serialize x402 payment requirements",
			Retryable: false,
			Cause:     err,
		}
	}

	var rawResponse any
	switch operation {
	case "verify":
		rawResponse, err = c.facilitator.Verify(ctx, payloadBytes, requirementsBytes)
	case "settle":
		rawResponse, err = c.facilitator.Settle(ctx, payloadBytes, requirementsBytes)
	default:
		err = fmt.Errorf("unsupported facilitator operation: %s", operation)
	}
	if err != nil {
		return nil, mapSDKFacilitatorError(operation, err)
	}

	raw, err := json.Marshal(rawResponse)
	if err != nil {
		return nil, &FacilitatorError{
			Operation: operation,
			Code:      facilitatorUnavailableCode(operation),
			Message:   "failed to serialize facilitator response",
			Retryable: true,
			Cause:     err,
		}
	}
	return json.RawMessage(raw), nil
}

type sdkPaymentPayload struct {
	X402Version int                    `json:"x402Version"`
	Scheme      string                 `json:"scheme"`
	Network     string                 `json:"network"`
	Payload     map[string]interface{} `json:"payload"`
}

type sdkPaymentRequirements struct {
	Scheme            string                 `json:"scheme"`
	Network           string                 `json:"network"`
	Amount            string                 `json:"amount"`
	Asset             string                 `json:"asset"`
	PayTo             string                 `json:"payTo"`
	Resource          string                 `json:"resource,omitempty"`
	MaxTimeoutSeconds int                    `json:"maxTimeoutSeconds,omitempty"`
	Extra             map[string]interface{} `json:"extra,omitempty"`
}

func buildSDKPaymentPayloadBytes(paymentSignature string, req Requirement) ([]byte, error) {
	trimmed := strings.TrimSpace(paymentSignature)
	if trimmed == "" {
		return nil, errors.New("empty payment signature")
	}
	if json.Valid([]byte(trimmed)) {
		return []byte(trimmed), nil
	}
	if decoded, err := base64.StdEncoding.DecodeString(trimmed); err == nil && json.Valid(decoded) {
		return decoded, nil
	}

	payload := sdkPaymentPayload{
		X402Version: X402Version,
		Scheme:      strings.ToLower(strings.TrimSpace(req.Scheme)),
		Network:     strings.TrimSpace(req.Network),
		Payload: map[string]interface{}{
			"paymentSignature": trimmed,
		},
	}
	return json.Marshal(payload)
}

func buildSDKPaymentRequirementsBytes(req Requirement) ([]byte, error) {
	req = req.Normalize()
	extra := make(map[string]interface{})
	if trimmed := strings.TrimSpace(req.MaxAmountRequired); trimmed != "" {
		extra["maxAmountRequired"] = trimmed
	}
	if len(extra) == 0 {
		extra = nil
	}

	// Bind the proof to the full PaymentRequirements: emit resource and
	// maxTimeoutSeconds as first-class fields (never only inside extra) so the
	// facilitator's Parameter-Matching verification covers the entire object and
	// a proof for one resource/price cannot verify against another (bs-010).
	payload := sdkPaymentRequirements{
		Scheme:            req.Scheme,
		Network:           req.Network,
		Amount:            req.Amount,
		Asset:             req.Asset,
		PayTo:             req.PayTo,
		Resource:          req.Resource,
		MaxTimeoutSeconds: req.MaxTimeoutSeconds,
		Extra:             extra,
	}
	return json.Marshal(payload)
}

func mapSDKFacilitatorError(operation string, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return &FacilitatorError{
			Operation: operation,
			Code:      facilitatorUnavailableCode(operation),
			Message:   "facilitator request failed",
			Retryable: false,
			Cause:     err,
		}
	}
	var netErr net.Error
	if errors.As(err, &netErr) || isURLTransportError(err) {
		return &FacilitatorError{
			Operation: operation,
			Code:      facilitatorUnavailableCode(operation),
			Message:   "facilitator request failed",
			Retryable: true,
			Cause:     err,
		}
	}

	lower := strings.ToLower(err.Error())
	if isSDKInvalidFailure(lower) {
		code := CodePaymentInvalid
		if operation == "settle" {
			code = CodePaymentSettlementFailed
		}
		return &FacilitatorError{
			Operation: operation,
			Code:      code,
			Message:   err.Error(),
			Retryable: false,
			Cause:     err,
		}
	}

	return &FacilitatorError{
		Operation: operation,
		Code:      facilitatorUnavailableCode(operation),
		Message:   "facilitator request failed",
		Retryable: true,
		Cause:     err,
	}
}

func facilitatorUnavailableCode(operation string) string {
	if operation == "settle" {
		return CodePaymentSettlementUnavailable
	}
	return CodePaymentFacilitatorUnavailable
}

func isURLTransportError(err error) bool {
	var urlErr *url.Error
	return errors.As(err, &urlErr)
}

func isSDKInvalidFailure(lowerErr string) bool {
	invalidSignals := []string{
		"invalid",
		"rejected",
		"reject",
		"insufficient",
		"expired",
		"unauthorized",
		"forbidden",
		"unprocessable",
		"bad request",
		"status 4",
		" status 4",
		"400",
		"401",
		"403",
		"404",
		"422",
	}
	for _, sig := range invalidSignals {
		if strings.Contains(lowerErr, sig) {
			return true
		}
	}
	return false
}
