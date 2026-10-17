package gateway

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// Table-driven coverage for the upstream failure classifier: every provider
// shape the story names must land in its class with a stable status and the
// right retryability.
func TestClassifyUpstreamTable(t *testing.T) {
	cases := []struct {
		name       string
		status     int
		body       string
		retry      string // Retry-After header
		class      UpstreamClass
		httpStatus int
		retryable  bool
	}{
		{
			name:       "openai invalid key",
			status:     http.StatusUnauthorized,
			body:       `{"error":{"message":"Incorrect API key provided: sk-xxxx","type":"invalid_request_error","code":"invalid_api_key"}}`,
			class:      UpstreamAuth,
			httpStatus: http.StatusUnauthorized,
			retryable:  false,
		},
		{
			name:       "anthropic bad x-api-key",
			status:     http.StatusUnauthorized,
			body:       `{"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"}}`,
			class:      UpstreamAuth,
			httpStatus: http.StatusUnauthorized,
			retryable:  false,
		},
		{
			name:       "forbidden without billing text is auth",
			status:     http.StatusForbidden,
			body:       `{"error":{"message":"You don't have access to this model","type":"forbidden"}}`,
			class:      UpstreamAuth,
			httpStatus: http.StatusUnauthorized,
			retryable:  false,
		},
		{
			name:       "openai insufficient_quota",
			status:     http.StatusTooManyRequests,
			body:       `{"error":{"message":"You exceeded your current quota, please check your plan and billing details.","type":"insufficient_quota","code":"insufficient_quota"}}`,
			class:      UpstreamBilling,
			httpStatus: 402,
			retryable:  false,
		},
		{
			name:       "anthropic credit balance exhausted",
			status:     http.StatusBadRequest,
			body:       `{"type":"error","error":{"type":"billing_error","message":"Your credit balance is too low to access the Anthropic API. Please add credits."}}`,
			class:      UpstreamBilling,
			httpStatus: 402,
			retryable:  false,
		},
		{
			name:       "zhipu glm balance code",
			status:     400,
			body:       `{"code":1113,"msg":"Your account balance is insufficient, please recharge and try again","success":false}`,
			class:      UpstreamBilling,
			httpStatus: 402,
			retryable:  false,
		},
		{
			name:       "deepseek insufficient balance",
			status:     402,
			body:       `{"error":{"message":"Insufficient Balance","type":"insufficient_balance","code":"insufficient_balance"}}`,
			class:      UpstreamBilling,
			httpStatus: 402,
			retryable:  false,
		},
		{
			name:       "kimi insufficient balance",
			status:     402,
			body:       `{"error":{"message":"insufficient balance, please recharge at platform.moonshot.ai","type":"insufficient_balance"}}`,
			class:      UpstreamBilling,
			httpStatus: 402,
			retryable:  false,
		},
		{
			name:       "bare 402 is billing",
			status:     402,
			body:       `payment required`,
			class:      UpstreamBilling,
			httpStatus: 402,
			retryable:  false,
		},
		{
			name:       "rate limited with retry-after",
			status:     http.StatusTooManyRequests,
			body:       `{"error":{"message":"Rate limit reached for gpt-4, slow down","type":"rate_limit_exceeded"}}`,
			retry:      "20",
			class:      UpstreamRateLimited,
			httpStatus: http.StatusTooManyRequests,
			retryable:  true,
		},
		{
			name:       "worker stderr invalid key (no http status)",
			status:     0,
			body:       `ERROR: Incorrect API key provided for this model (cli stderr)`,
			class:      UpstreamAuth,
			httpStatus: http.StatusUnauthorized,
			retryable:  false,
		},
		{
			name:       "worker stderr exhausted balance (no http status)",
			status:     0,
			body:       `Error: Insufficient Balance, please recharge and retry`,
			class:      UpstreamBilling,
			httpStatus: 402,
			retryable:  false,
		},
		{
			name:       "worker stderr unknown failure stays retryable",
			status:     0,
			body:       `Error: worker exited with status 1`,
			class:      UpstreamTransient,
			httpStatus: http.StatusBadGateway,
			retryable:  true,
		},
		{
			name:       "plain 500 is transient",
			status:     http.StatusInternalServerError,
			body:       `internal error`,
			class:      UpstreamTransient,
			httpStatus: http.StatusBadGateway,
			retryable:  true,
		},
		{
			name:       "bad gateway upstream",
			status:     http.StatusBadGateway,
			body:       ``,
			class:      UpstreamTransient,
			httpStatus: http.StatusBadGateway,
			retryable:  true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hdr := http.Header{}
			if tc.retry != "" {
				hdr.Set("Retry-After", tc.retry)
			}
			got := ClassifyUpstream(tc.status, []byte(tc.body), hdr)
			if got.Class != tc.class {
				t.Errorf("class=%q, want %q", got.Class, tc.class)
			}
			if got.Status != tc.httpStatus {
				t.Errorf("status=%d, want %d", got.Status, tc.httpStatus)
			}
			if got.Retryable != tc.retryable {
				t.Errorf("retryable=%v, want %v", got.Retryable, tc.retryable)
			}
			if got.Hint == "" {
				t.Errorf("hint is empty")
			}
			if tc.retry != "" && got.RetryAfter != tc.retry {
				t.Errorf("retry-after=%q, want %q", got.RetryAfter, tc.retry)
			}
		})
	}
}

type timeoutNetError struct{ msg string }

func (e timeoutNetError) Error() string   { return e.msg }
func (e timeoutNetError) Timeout() bool   { return true }
func (e timeoutNetError) Temporary() bool { return true }

func TestClassifyTransportErrorTable(t *testing.T) {
	cases := []struct {
		name       string
		err        error
		class      UpstreamClass
		httpStatus int
		retryable  bool
	}{
		{name: "connection refused", err: errors.New("dial tcp 127.0.0.1:1: connect: connection refused"), class: UpstreamUnreachable, httpStatus: http.StatusBadGateway, retryable: true},
		{name: "dns failure", err: errors.New("dial tcp: lookup api.example.com: no such host"), class: UpstreamUnreachable, httpStatus: http.StatusBadGateway, retryable: true},
		{name: "tls verify", err: errors.New(`tls: failed to verify certificate: x509: certificate is not valid`), class: UpstreamUnreachable, httpStatus: http.StatusBadGateway, retryable: true},
		{name: "context deadline", err: errors.New("context deadline exceeded"), class: UpstreamTimeout, httpStatus: http.StatusGatewayTimeout, retryable: true},
		{name: "header timeout", err: timeoutNetError{msg: `net/http: timeout awaiting response headers`}, class: UpstreamTimeout, httpStatus: http.StatusGatewayTimeout, retryable: true},
		{name: "client timeout", err: errors.New(`Client.Timeout exceeded while awaiting headers`), class: UpstreamTimeout, httpStatus: http.StatusGatewayTimeout, retryable: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ClassifyTransportError(tc.err)
			if got.Class != tc.class {
				t.Errorf("class=%q, want %q", got.Class, tc.class)
			}
			if got.Status != tc.httpStatus {
				t.Errorf("status=%d, want %d", got.Status, tc.httpStatus)
			}
			if got.Retryable != tc.retryable {
				t.Errorf("retryable=%v, want %v", got.Retryable, tc.retryable)
			}
		})
	}
}

// Both wire shapes preserve the classified fields: the Anthropic codec's
// generic writer would flatten them away, so classified errors bypass it.
func TestWriteClassifiedErrorShapes(t *testing.T) {
	failure := ClassifyUpstream(http.StatusTooManyRequests,
		[]byte(`{"error":{"code":"insufficient_quota"}}`), nil).
		WithEndpoint("alpha", "glm-4")

	openaiRec := httptest.NewRecorder()
	writeClassifiedError(openaiRec, openAICodec(ChatCompletionsPath, chatCompletionsBody), failure)
	if openaiRec.Code != 402 {
		t.Fatalf("openai status=%d, want 402", openaiRec.Code)
	}
	var openaiBody struct {
		Error map[string]any `json:"error"`
	}
	if err := json.Unmarshal(openaiRec.Body.Bytes(), &openaiBody); err != nil {
		t.Fatalf("decode openai error: %v", err)
	}
	for _, key := range []string{"type", "provider", "model", "retryable", "hint"} {
		if _, ok := openaiBody.Error[key]; !ok {
			t.Errorf("openai error missing %q: %v", key, openaiBody.Error)
		}
	}

	anthropicRec := httptest.NewRecorder()
	writeClassifiedError(anthropicRec, anthropicCodec(), failure)
	if anthropicRec.Code != 402 {
		t.Fatalf("anthropic status=%d, want 402", anthropicRec.Code)
	}
	var anthropicBody struct {
		Type  string         `json:"type"`
		Error map[string]any `json:"error"`
	}
	if err := json.Unmarshal(anthropicRec.Body.Bytes(), &anthropicBody); err != nil {
		t.Fatalf("decode anthropic error: %v", err)
	}
	if anthropicBody.Type != "error" {
		t.Errorf("anthropic envelope type=%q, want error", anthropicBody.Type)
	}
	for _, key := range []string{"type", "provider", "model", "retryable", "hint"} {
		if _, ok := anthropicBody.Error[key]; !ok {
			t.Errorf("anthropic error missing %q: %v", key, anthropicBody.Error)
		}
	}
	if anthropicBody.Error["type"] != "billing" {
		t.Errorf("anthropic error type=%v, want billing", anthropicBody.Error["type"])
	}
}

// The classified error body carries everything a caller needs to act without
// another round trip.
func TestUpstreamFailureErrorBody(t *testing.T) {
	f := ClassifyUpstream(http.StatusUnauthorized, []byte(`{"error":{"code":"invalid_api_key"}}`), nil).
		WithEndpoint("alpha", "glm-4")
	body := f.ErrorBody()
	for _, key := range []string{"type", "provider", "model", "retryable", "hint"} {
		if _, ok := body[key]; !ok {
			t.Errorf("error body missing %q: %v", key, body)
		}
	}
	if body["type"] != "auth" || body["provider"] != "alpha" || body["model"] != "glm-4" {
		t.Errorf("error body=%v", body)
	}
	if body["retryable"] != false {
		t.Errorf("auth must not be retryable: %v", body)
	}
}
