package x402_test

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
	"testing"

	"dir2mcp/internal/x402"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

type errorReader struct{ err error }

func (e *errorReader) Read(p []byte) (int, error) { return 0, e.err }
func (e *errorReader) Close() error               { return nil }

// Verify currently wraps the internal do method, so this test exercises
// behavior triggered by the lower-level call.  The original name referred to
// the unexported "do" helper, but the exported API used throughout the code
// is Verify.
func TestVerify_ReadError(t *testing.T) {
	errRead := errors.New("read failure")
	r := &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       &errorReader{err: errRead},

		Request: &http.Request{
			Method: http.MethodPost,
			URL:    &url.URL{Scheme: "https", Host: "api.example.com", Path: "/"},
		},
	}
	r.Header.Set("Content-Type", "application/json")

	rt := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return r, nil
	})

	client := x402.NewHTTPClient("https://facilitator.test", "token", &http.Client{Transport: rt})
	// valid requirement so we get past preflight validation.  network must
	// satisfy CAIP-2 (<namespace>:<reference>), so use a simple placeholder.
	req := x402.Requirement{
		Scheme:            "exact",
		Network:           "foo:bar",
		Amount:            "1",
		MaxAmountRequired: "1",
		Asset:             "asset",
		PayTo:             "pay",
		Resource:          "res",
	}
	_, err := client.Verify(context.Background(), "sig", req)
	if err == nil {
		t.Fatalf("expected error when reading response")
	}
	var fe *x402.FacilitatorError
	if !errors.As(err, &fe) {
		t.Fatalf("expected FacilitatorError, got %v", err)
	}
	if fe.Cause != errRead {
		t.Fatalf("expected cause to be read error; got %v", fe.Cause)
	}
}

func TestVerify_ResponseTooLarge(t *testing.T) {
	large := strings.Repeat("a", (1<<20)+1)
	r := &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(large)),
		Request: &http.Request{
			Method: http.MethodPost,
			URL:    &url.URL{Scheme: "https", Host: "api.example.com", Path: "/"},
		},
	}
	r.Header.Set("Content-Type", "application/json")

	rt := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return r, nil
	})

	client := x402.NewHTTPClient("https://facilitator.test", "token", &http.Client{Transport: rt})
	req := x402.Requirement{
		Scheme:            "exact",
		Network:           "foo:bar",
		Amount:            "1",
		MaxAmountRequired: "1",
		Asset:             "asset",
		PayTo:             "pay",
		Resource:          "res",
	}
	_, err := client.Verify(context.Background(), "sig", req)
	if err == nil {
		t.Fatalf("expected error when facilitator response exceeds max size")
	}
	var fe *x402.FacilitatorError
	if !errors.As(err, &fe) {
		t.Fatalf("expected FacilitatorError, got %v", err)
	}
	if fe.Code != x402.CodePaymentFacilitatorUnavailable {
		t.Fatalf("code=%q want=%q", fe.Code, x402.CodePaymentFacilitatorUnavailable)
	}
	if fe.Retryable {
		t.Fatalf("expected non-retryable for oversized response; got retryable")
	}
	if !strings.Contains(fe.Message, "exceeds maximum size") {
		t.Fatalf("message=%q missing overflow detail", fe.Message)
	}
}

// overflow remains non-retryable even for HTML-like payloads.
func TestVerify_ResponseTooLarge_HtmlProxy(t *testing.T) {
	header := "<html>"
	count := (1<<20)/len(header) + 10
	large := strings.Repeat(header, count)
	r := &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(large)),
		Request: &http.Request{
			Method: http.MethodPost,
			URL:    &url.URL{Scheme: "https", Host: "api.example.com", Path: "/"},
		},
	}
	r.Header.Set("Content-Type", "application/json")

	rt := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return r, nil
	})

	client := x402.NewHTTPClient("https://facilitator.test", "token", &http.Client{Transport: rt})
	req := x402.Requirement{
		Scheme:            "exact",
		Network:           "foo:bar",
		Amount:            "1",
		MaxAmountRequired: "1",
		Asset:             "asset",
		PayTo:             "pay",
		Resource:          "res",
	}
	_, err := client.Verify(context.Background(), "sig", req)
	if err == nil {
		t.Fatalf("expected error when facilitator response exceeds max size")
	}
	var fe *x402.FacilitatorError
	if !errors.As(err, &fe) {
		t.Fatalf("expected FacilitatorError, got %v", err)
	}
	if fe.Code != x402.CodePaymentFacilitatorUnavailable {
		t.Fatalf("code=%q want=%q", fe.Code, x402.CodePaymentFacilitatorUnavailable)
	}
	if fe.Retryable {
		t.Fatalf("expected non-retryable for HTML-like overflow")
	}
}

// When the facilitator returns a non-2xx status we include a copy of the
// normalized response in FacilitatorError.Body.  That payload should be
// redacted/truncated so that large or sensitive fields are not exposed.
func TestVerify_BodyRedactedOnError(t *testing.T) {
	secret := strings.Repeat("x", 2000)
	orig := fmt.Sprintf(`{"ok":false,"secret":%q}`, secret)
	resp := &http.Response{
		StatusCode: 400,
		Header:     make(http.Header),
		Body:       io.NopCloser(bytes.NewBufferString(orig)),
		Request: &http.Request{
			Method: http.MethodPost,
			URL:    &url.URL{Scheme: "https", Host: "api.example.com", Path: "/"},
		},
	}
	resp.Header.Set("Content-Type", "application/json")

	rt := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return resp, nil
	})

	client := x402.NewHTTPClient("https://facilitator.test", "token", &http.Client{Transport: rt})
	req := x402.Requirement{
		Scheme:            "exact",
		Network:           "foo:bar",
		Amount:            "1",
		MaxAmountRequired: "1",
		Asset:             "asset",
		PayTo:             "pay",
		Resource:          "res",
	}
	_, err := client.Verify(context.Background(), "sig", req)
	if err == nil {
		t.Fatalf("expected error from bad status code")
	}
	var fe *x402.FacilitatorError
	if !errors.As(err, &fe) {
		t.Fatalf("expected FacilitatorError, got %v", err)
	}
	if strings.Contains(fe.Body, secret) {
		t.Errorf("body contains full secret, should be redacted or truncated")
	}
	if !strings.Contains(fe.Body, "[REDACTED]") {
		t.Errorf("expected redacted value in body, got %q", fe.Body)
	}
}

// Nested sensitive fields (in maps and arrays) should also be scrubbed.
func TestVerify_BodyRedactedOnError_Nested(t *testing.T) {
	secret := "topsecret"
	orig := fmt.Sprintf(`{
		"ok":false,
		"nested": {"password": %q, "inner": {"apiKey":"abc"}},
		"arr": [{"bearer":"token"},"plain"],
		"secret": %q
	}`, "pw", secret)
	resp := &http.Response{
		StatusCode: 400,
		Header:     make(http.Header),
		Body:       io.NopCloser(bytes.NewBufferString(orig)),
		Request: &http.Request{
			Method: http.MethodPost,
			URL:    &url.URL{Scheme: "https", Host: "api.example.com", Path: "/"},
		},
	}
	resp.Header.Set("Content-Type", "application/json")

	rt := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return resp, nil
	})

	client := x402.NewHTTPClient("https://facilitator.test", "token", &http.Client{Transport: rt})
	req := x402.Requirement{
		Scheme:            "exact",
		Network:           "foo:bar",
		Amount:            "1",
		MaxAmountRequired: "1",
		Asset:             "asset",
		PayTo:             "pay",
		Resource:          "res",
	}
	_, err := client.Verify(context.Background(), "sig", req)
	if err == nil {
		t.Fatalf("expected error from bad status code")
	}
	var fe *x402.FacilitatorError
	if !errors.As(err, &fe) {
		t.Fatalf("expected FacilitatorError, got %v", err)
	}
	if strings.Contains(fe.Body, "pw") || strings.Contains(fe.Body, "abc") || strings.Contains(fe.Body, "token") || strings.Contains(fe.Body, secret) {
		t.Errorf("nested sensitive values leaked: %s", fe.Body)
	}
	count := strings.Count(fe.Body, "[REDACTED]")
	if count < 4 {
		t.Errorf("expected at least 4 redactions; got %d body=%s", count, fe.Body)
	}
}

// When a structured payload includes a long message key we should still
// truncate the returned error message.  This covers the top‑level "message"
// case plus nested maps such as {"error":{"message":"…"}} and simple
// "reason" keys.  All branches pass through truncateString(…,256).
func TestVerify_MessageTruncatedOnStructuredPayload(t *testing.T) {
	long := strings.Repeat("a", 300)
	testCases := []struct {
		name    string
		payload string
	}{
		{"top", fmt.Sprintf(`{"message":%q}`, long)},
		{"nested", fmt.Sprintf(`{"error":{"message":%q}}`, long)},
		{"reason", fmt.Sprintf(`{"reason":%q}`, long)},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			resp := &http.Response{
				StatusCode: 400,
				Header:     make(http.Header),
				Body:       io.NopCloser(bytes.NewBufferString(tc.payload)),
				Request: &http.Request{
					Method: http.MethodPost,
					URL:    &url.URL{Scheme: "https", Host: "api.example.com", Path: "/"},
				},
			}
			resp.Header.Set("Content-Type", "application/json")

			rt := roundTripFunc(func(req *http.Request) (*http.Response, error) {
				return resp, nil
			})

			client := x402.NewHTTPClient("https://facilitator.test", "token", &http.Client{Transport: rt})
			req := x402.Requirement{
				Scheme:            "exact",
				Network:           "foo:bar",
				Amount:            "1",
				MaxAmountRequired: "1",
				Asset:             "asset",
				PayTo:             "pay",
				Resource:          "res",
			}
			_, err := client.Verify(context.Background(), "sig", req)
			if err == nil {
				t.Fatalf("expected error from bad status code")
			}
			var fe *x402.FacilitatorError
			if !errors.As(err, &fe) {
				t.Fatalf("expected FacilitatorError, got %v", err)
			}
			// maxContentRunes is the maximum number of runes allowed for the
			// *content portion* of the error message. The truncation indicator
			// string (`sel`) may be appended afterward, so the total length of
			// `fe.Message` is allowed to exceed maxContentRunes by len(sel).
			maxContentRunes := 256
			sel := "… (truncated)"
			if !strings.Contains(fe.Message, sel) {
				t.Errorf("expected truncation indicator %q, got %q", sel, fe.Message)
			}
			if len([]rune(fe.Message)) > maxContentRunes+len([]rune(sel)) {
				t.Errorf("message too long after truncation (%d runes); %q", len([]rune(fe.Message)), fe.Message)
			}
		})
	}
}

func TestRequirementValidate_SchemeWhitelist(t *testing.T) {
	cases := []struct {
		scheme       string
		expectsError bool
	}{
		{"", true},
		{"invalid", true},
		{"exact", false},
		{"EXACT", false},
		{" upto ", false},
		{"upto", false},
	}
	for i, tc := range cases {
		name := fmt.Sprintf("%d-%s", i, tc.scheme)
		t.Run(name, func(t *testing.T) {
			r := x402.Requirement{
				Scheme:            tc.scheme,
				Network:           "foo:bar",
				Amount:            "1",
				MaxAmountRequired: "1",
				Asset:             "a",
				PayTo:             "p",
				Resource:          "r",
			}
			err := r.Validate()
			if tc.expectsError {
				if err == nil {
					t.Errorf("scheme %q should have failed validation", tc.scheme)
				}
			} else {
				if err != nil {
					t.Errorf("scheme %q should be accepted: %v", tc.scheme, err)
				}
			}
		})
	}
}

func TestRequirementValidate_UptoMax(t *testing.T) {
	r := x402.Requirement{
		Scheme:            "upto",
		Network:           "foo:bar",
		Amount:            "5",
		MaxAmountRequired: "10",
		Asset:             "a",
		PayTo:             "p",
		Resource:          "r",
	}
	if err := r.Validate(); err != nil {
		t.Fatalf("valid upto requirement should pass: %v", err)
	}

	r.MaxAmountRequired = ""
	if err := r.Validate(); err == nil {
		t.Error("expected error when maxAmountRequired missing for upto scheme")
	}

	r.MaxAmountRequired = "1"
	if err := r.Validate(); err == nil {
		t.Error("expected error when maxAmountRequired < amount")
	}
}

func TestBuildPaymentRequiredHeaderValue_MaxIncluded(t *testing.T) {
	req := x402.Requirement{
		Scheme:            "exact",
		Network:           "foo:bar",
		Amount:            "3",
		MaxAmountRequired: " 4 ",
		Asset:             "a",
		PayTo:             "p",
		Resource:          "r",
	}
	payload, err := x402.BuildPaymentRequiredHeaderValue(req)
	if err != nil {
		t.Fatalf("build header failed: %v", err)
	}
	var p x402.X402Payload
	if err := json.Unmarshal([]byte(payload), &p); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if len(p.Accept) != 1 {
		t.Fatalf("accept count=%d want=1", len(p.Accept))
	}
	if p.Accept[0].MaxAmountRequired != "4" {
		t.Errorf("maxAmountRequired=%q want %q", p.Accept[0].MaxAmountRequired, "4")
	}
}

func TestBuildPaymentRequiredHeaderValue_MaxOmittedWhenEmpty(t *testing.T) {
	req := x402.Requirement{
		Scheme:            "exact",
		Network:           "foo:bar",
		Amount:            "3",
		MaxAmountRequired: "", // explicitly empty
		Asset:             "a",
		PayTo:             "p",
		Resource:          "r",
	}
	payload, err := x402.BuildPaymentRequiredHeaderValue(req)
	if err != nil {
		t.Fatalf("build header failed: %v", err)
	}
	// inspect raw JSON for absence of maxAmountRequired key
	if strings.Contains(payload, "maxAmountRequired") {
		t.Errorf("payload should not include maxAmountRequired when empty: %s", payload)
	}
}

func TestVerify_NormalizesSchemeInOutgoingPayload(t *testing.T) {
	var gotScheme string
	var gotMax string
	// the transport should return errors for failures so that the upstream
	// Verify call can observe them and fail at the assertion site.
	rt := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		var body struct {
			PaymentRequirements []struct {
				Scheme            string `json:"scheme"`
				MaxAmountRequired string `json:"maxAmountRequired"`
			} `json:"paymentRequirements"`
		}
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			return nil, fmt.Errorf("decode request body: %w", err)
		}
		if len(body.PaymentRequirements) != 1 {
			return nil, fmt.Errorf("payment requirements len=%d want=1", len(body.PaymentRequirements))
		}
		gotScheme = body.PaymentRequirements[0].Scheme
		gotMax = body.PaymentRequirements[0].MaxAmountRequired
		r := &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(bytes.NewBufferString(`{"ok":true}`)),
			Request: &http.Request{
				Method: http.MethodPost,
				URL:    &url.URL{Scheme: "https", Host: "api.example.com", Path: "/"},
			},
		}
		r.Header.Set("Content-Type", "application/json")
		return r, nil
	})

	client := x402.NewHTTPClient("https://facilitator.test", "token", &http.Client{Transport: rt})
	req := x402.Requirement{
		Scheme:            " UpTo ",
		Network:           "foo:bar",
		Amount:            "1",
		MaxAmountRequired: " 2 ", // intentionally include spaces to exercise trimming
		Asset:             "asset",
		PayTo:             "pay",
		Resource:          "res",
	}
	if _, err := client.Verify(context.Background(), "sig", req); err != nil {
		t.Fatalf("verify error: %v", err)
	}
	if gotScheme != "upto" {
		t.Fatalf("scheme sent=%q want=%q", gotScheme, "upto")
	}
	// ensure the payload actually contained the trimmed max amount -- don't rely on
	// the original request struct since trimming happens during serialization.
	if strings.TrimSpace(gotMax) == "" {
		t.Fatalf("max amount was not sent")
	}
	if gotMax != "2" {
		t.Fatalf("max amount sent=%q want=%q", gotMax, "2")
	}
}
