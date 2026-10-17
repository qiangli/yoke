package gateway

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// UpstreamClass is the fail-fast classification of an upstream failure.
// Auth and billing are NOT retryable: the key is dead, so spending the
// failover budget on another metered key only burns money and time.
type UpstreamClass string

const (
	// UpstreamAuth: the provider rejected the credential (401/403,
	// invalid/expired key). Stable status 401, never retried.
	UpstreamAuth UpstreamClass = "auth"
	// UpstreamBilling: the provider reports an exhausted balance, quota or
	// credit (402, insufficient_quota/balance/credit bodies). Stable
	// status 402, never retried.
	UpstreamBilling UpstreamClass = "billing"
	// UpstreamRateLimited: the provider is throttling (429 rate-limit
	// bodies). Retryable; the upstream Retry-After is honored.
	UpstreamRateLimited UpstreamClass = "rate_limited"
	// UpstreamUnreachable: the upstream could not be reached (DNS,
	// connection refused/reset, TLS). Retryable across candidates.
	UpstreamUnreachable UpstreamClass = "unreachable"
	// UpstreamTimeout: the upstream went silent before the first byte.
	// Retryable across candidates.
	UpstreamTimeout UpstreamClass = "timeout"
	// UpstreamTransient: any other 5xx. Retryable across candidates.
	UpstreamTransient UpstreamClass = "transient"
	// UpstreamPassthrough: an upstream error that is neither classified
	// nor retryable (other 4xx). Returned to the caller untouched.
	UpstreamPassthrough UpstreamClass = "passthrough"
)

// FallbackHeader marks a response served by the configured metered fallback:
// "X-Bashy-Fallback: <from> -> <to>".
const FallbackHeader = "X-Bashy-Fallback"

// DefaultFirstByteTimeout bounds how long an upstream may stay silent before
// its first response byte. Without it a hung upstream parks the caller (and
// its admission slot) until the end-to-end request timeout. Thirty seconds
// is generous for time-to-first-token and far below any request timeout.
const DefaultFirstByteTimeout = 30 * time.Second

// maxClassifyBody bounds how much of an upstream error body the classifier
// reads. Provider error payloads are small JSON objects; anything beyond
// this is not a signal.
const maxClassifyBody = 32 << 10

// UpstreamFailure is one classified upstream failure.
type UpstreamFailure struct {
	Class     UpstreamClass
	Retryable bool
	// Status is the stable HTTP status reported to the caller: auth 401,
	// billing 402, rate_limited 429, unreachable/transient 502, timeout 504.
	Status     int
	Hint       string
	RetryAfter string
	Provider   string
	Model      string
}

// WithEndpoint stamps the serving backend and model onto a classification.
func (f UpstreamFailure) WithEndpoint(provider, model string) UpstreamFailure {
	f.Provider, f.Model = provider, model
	return f
}

// ErrorBody is the caller-facing JSON error object: everything needed to
// act without another round trip.
func (f UpstreamFailure) ErrorBody() map[string]any {
	return map[string]any{
		"type":      string(f.Class),
		"provider":  f.Provider,
		"model":     f.Model,
		"retryable": f.Retryable,
		"hint":      f.Hint,
	}
}

const (
	hintAuth = "the provider rejected the API key (invalid, expired, or not authorized for this model): " +
		"check the key and its permissions; this failure is not retried"
	hintBilling = "the provider reports this key's balance, quota or credit is exhausted: " +
		"top up or rotate the key; this failure is not retried"
	hintRateLimited = "the provider is throttling this key: honor Retry-After and retry"
	hintUnreachable = "the upstream could not be reached (network, DNS or TLS): failing over"
	hintTimeout     = "the upstream went silent before the first response byte: failing over"
	hintTransient   = "the upstream errored: failing over"
)

var billingSignals = []string{
	"insufficient_quota", "insufficient quota",
	"insufficient_balance", "insufficient balance",
	"balance is insufficient", "balance insufficient",
	"insufficient funds",
	"credit balance", "out of credit", "credits exhausted", "credits depleted",
	"exceeded your current quota", "exceeds your quota", "exceed your quota",
	"quota exceeded", "quota_exceeded", "quota exhausted", "quota_exhausted",
	"billing_error", "billing",
	"payment required",
	"arrear", "overdue",
	"top up", "top-up", "topup", "recharge",
	"free quota", "trial quota",
	"balance too low", "balance is too low",
	"account balance",
	"usage limit exceeded", "spend limit",
	"plan and billing",
}

var authSignals = []string{
	"invalid_api_key", "invalid api key", "incorrect api key", "invalid-api-key",
	"authentication_error", "authenticate", "invalid x-api-key", "invalid x_api_key",
	"api key not valid", "wrong api key", "expired api key", "revoked api key",
	"missing api key", "no api key", "unauthorized", "unauthorised",
	"invalid bearer", "invalid token", "bad credentials",
	"permission denied", "access denied", "forbidden key",
}

var rateLimitSignals = []string{
	"rate_limit", "rate limit", "ratelimit",
	"too many requests", "throttl", "slow down",
	"requests per minute", "tokens per minute",
	"capacity", "overloaded", "server is busy",
}

// ClassifyUpstream maps one upstream HTTP response (status plus a bounded
// body prefix) to its failure class. Explicit provider billing/auth text is
// decisive over the status code; otherwise 401/403 is auth, 402 is billing,
// 429 is rate_limited and 5xx is transient. Any other 4xx passes through
// untouched so ordinary caller errors keep their upstream shape.
func ClassifyUpstream(status int, body []byte, header http.Header) UpstreamFailure {
	text := strings.ToLower(string(body))
	if containsAny(text, billingSignals) || zhBillingCode(text) {
		return UpstreamFailure{Class: UpstreamBilling, Retryable: false, Status: 402, Hint: hintBilling}
	}
	if containsAny(text, authSignals) {
		return UpstreamFailure{Class: UpstreamAuth, Retryable: false, Status: http.StatusUnauthorized, Hint: hintAuth}
	}
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden:
		return UpstreamFailure{Class: UpstreamAuth, Retryable: false, Status: http.StatusUnauthorized, Hint: hintAuth}
	case 402:
		return UpstreamFailure{Class: UpstreamBilling, Retryable: false, Status: 402, Hint: hintBilling}
	case http.StatusRequestTimeout:
		return UpstreamFailure{Class: UpstreamTimeout, Retryable: true, Status: http.StatusGatewayTimeout, Hint: hintTimeout}
	case http.StatusTooManyRequests:
		f := UpstreamFailure{Class: UpstreamRateLimited, Retryable: true, Status: http.StatusTooManyRequests, Hint: hintRateLimited}
		if header != nil {
			f.RetryAfter = strings.TrimSpace(header.Get("Retry-After"))
		}
		return f
	}
	if status >= 500 && status <= 599 {
		return UpstreamFailure{Class: UpstreamTransient, Retryable: true, Status: http.StatusBadGateway, Hint: hintTransient}
	}
	if status == 0 {
		// No HTTP status (a worker or transport report with no signal):
		// keep the historical behavior of trying the next candidate.
		return UpstreamFailure{Class: UpstreamTransient, Retryable: true, Status: http.StatusBadGateway, Hint: hintTransient}
	}
	return UpstreamFailure{Class: UpstreamPassthrough, Retryable: false, Status: status, Hint: ""}
}

// zhBillingCode matches z.ai/GLM-style balance-exhausted envelopes such as
// {"code":1113,"msg":"...balance..."} with or without JSON spacing/quoting.
func zhBillingCode(text string) bool {
	compact := strings.ReplaceAll(strings.ReplaceAll(text, " ", ""), `"`, "")
	return strings.Contains(compact, "code:1113") && containsAny(text, []string{"balance", "insufficient", "arrear", "recharge"})
}

func containsAny(text string, signals []string) bool {
	for _, s := range signals {
		if strings.Contains(text, s) {
			return true
		}
	}
	return false
}

// ClassifyTransportError maps a dial/TLS/timeout failure to unreachable or
// timeout. Both are retryable across candidates; the stable caller status is
// 502 for unreachable and 504 for timeout.
func ClassifyTransportError(err error) UpstreamFailure {
	if err == nil {
		return UpstreamFailure{Class: UpstreamTransient, Retryable: true, Status: http.StatusBadGateway, Hint: hintTransient}
	}
	if os.IsTimeout(err) || isDeadline(err) {
		return UpstreamFailure{Class: UpstreamTimeout, Retryable: true, Status: http.StatusGatewayTimeout, Hint: hintTimeout}
	}
	var nerr net.Error
	if errors.As(err, &nerr) && nerr.Timeout() {
		return UpstreamFailure{Class: UpstreamTimeout, Retryable: true, Status: http.StatusGatewayTimeout, Hint: hintTimeout}
	}
	text := strings.ToLower(err.Error())
	for _, s := range []string{"timeout", "timed out", "deadline exceeded", "awaiting headers", "response header", "first byte", "i/o timeout", "client.timeout"} {
		if strings.Contains(text, s) {
			return UpstreamFailure{Class: UpstreamTimeout, Retryable: true, Status: http.StatusGatewayTimeout, Hint: hintTimeout}
		}
	}
	return UpstreamFailure{Class: UpstreamUnreachable, Retryable: true, Status: http.StatusBadGateway, Hint: hintUnreachable}
}

func isDeadline(err error) bool {
	for err != nil {
		if err == context.DeadlineExceeded {
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

// firstByteTransports caches one shared RoundTripper per first-byte bound so
// per-request backends do not mint (and strand) connection pools.
var firstByteTransports sync.Map // time.Duration -> http.RoundTripper

// FirstByteTransport returns a RoundTripper that fails an upstream staying
// silent past d before its first response byte. Zero d selects
// DefaultFirstByteTimeout; a negative d disables the bound entirely and the
// upstream may take as long as the request context allows. Streaming after
// the first byte is unaffected either way. Remote fronts use the default so
// a silent upstream cannot hang a caller past this bound; the local engine
// uses a negative (or its own generous) bound because prompt prefill on a
// CPU-only host legitimately takes longer.
func FirstByteTransport(d time.Duration) http.RoundTripper {
	if d == 0 {
		d = DefaultFirstByteTimeout
	}
	if v, ok := firstByteTransports.Load(d); ok {
		return v.(http.RoundTripper)
	}
	var base *http.Transport
	if t, ok := http.DefaultTransport.(*http.Transport); ok && t != nil {
		base = t.Clone()
	} else {
		base = &http.Transport{}
	}
	if d > 0 {
		base.ResponseHeaderTimeout = d
	}
	actual, _ := firstByteTransports.LoadOrStore(d, base)
	return actual.(http.RoundTripper)
}

// writeClassifiedError answers the caller with the classified failure in the
// codec's wire shape, always preserving type, provider, model, retryable and
// hint. The Anthropic codec's generic writer would flatten those away, so
// both shapes are built here.
func writeClassifiedError(w http.ResponseWriter, codec inferenceCodec, f UpstreamFailure) {
	if f.RetryAfter != "" {
		w.Header().Set("Retry-After", f.RetryAfter)
	}
	if !codec.anthropic {
		writeJSON(w, f.Status, map[string]any{"error": map[string]any{
			"type":      string(f.Class),
			"message":   classifiedMessage(f),
			"provider":  f.Provider,
			"model":     f.Model,
			"retryable": f.Retryable,
			"hint":      f.Hint,
			"code":      f.Status,
		}})
		return
	}
	writeJSON(w, f.Status, map[string]any{
		"type": "error",
		"error": map[string]any{
			"type":      string(f.Class),
			"message":   classifiedMessage(f),
			"provider":  f.Provider,
			"model":     f.Model,
			"retryable": f.Retryable,
			"hint":      f.Hint,
		},
	})
}

func classifiedMessage(f UpstreamFailure) string {
	loc := f.Provider
	if f.Model != "" {
		loc += "/" + f.Model
	}
	if loc == "" {
		loc = "upstream"
	}
	return loc + ": " + string(f.Class)
}
