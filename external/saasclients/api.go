// Documented API adapters for operations the admitted CLIs do not expose
// (Slack messaging/history via the Web API; Salesforce single-record upsert
// by external id via REST). One bounded, redacting HTTP core; provider
// classifiers map documented error shapes onto the shared typed codes.
package saasclients

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/qiangli/yoke/external/gws"
)

// Bounds. Limit exhaustion is an explicit output-limit error, never silent
// truncation.
const (
	MaxResponseBytes = 1 << 20
	DefaultTimeout   = 30 * time.Second
)

// API is an explicit-endpoint adapter. BaseURL has no default: the caller
// names the endpoint (e.g. https://slack.com/api, a sandbox instance URL, or
// a loopback fake in tests).
type API struct {
	Client   string
	BaseURL  string
	Token    string
	HTTP     *http.Client
	MaxBytes int64
}

// Response is a bounded provider response.
type Response struct {
	Status int
	Body   json.RawMessage
}

// Do performs one request. Mutating methods must carry their idempotency
// header via headers; Do never retries (an ambiguous write is not retried).
func (a API) Do(ctx context.Context, method, path string, query url.Values, body []byte, headers map[string]string) (*Response, *gws.Error) {
	op := a.Client + ":" + method + " " + path
	fail := func(c gws.Code, msg string) *gws.Error {
		return gws.NewError(c, a.redact(msg), nil).WithOp(a.Client+" api", path)
	}
	if a.BaseURL == "" || a.Token == "" {
		return nil, fail(gws.CodeMissingCredentials, a.Client+": api adapter needs an explicit base URL and token")
	}
	u := strings.TrimRight(a.BaseURL, "/") + "/" + strings.TrimLeft(path, "/")
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, bytes.NewReader(body))
	if err != nil {
		return nil, fail(gws.CodeInvalidInput, "bad request: "+err.Error())
	}
	req.Header.Set("Authorization", "Bearer "+a.Token)
	if len(body) > 0 {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	hc := a.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: DefaultTimeout}
	}
	resp, err := hc.Do(req)
	if err != nil {
		if ctx.Err() == context.Canceled {
			return nil, fail(gws.CodeCancelled, op+" cancelled; remote outcome unknown")
		}
		if ctx.Err() != nil {
			return nil, fail(gws.CodeTimeout, op+" timed out; remote outcome unknown")
		}
		return nil, fail(gws.CodeProviderFailure, op+": transport error: "+err.Error())
	}
	defer resp.Body.Close()
	limit := a.MaxBytes
	if limit <= 0 {
		limit = MaxResponseBytes
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, fail(gws.CodeProviderFailure, op+": read failed: "+err.Error())
	}
	if int64(len(data)) > limit {
		return nil, fail(gws.CodeOutputLimit, fmt.Sprintf("%s: response exceeds %d bytes; narrow the query", op, limit))
	}
	r := &Response{Status: resp.StatusCode, Body: data}
	if e := a.classify(resp, data); e != nil {
		e.Operation, e.Target = a.Client+" api", path
		return r, e
	}
	return r, nil
}

func (a API) redact(s string) string { return gws.RedactText(s, a.Token) }

// classify maps HTTP status and the provider's documented error body onto the
// shared typed codes. Provider text is truncated and redacted.
func (a API) classify(resp *http.Response, data []byte) *gws.Error {
	provider, msg := a.providerError(data)
	code := gws.Code("")
	switch {
	case resp.StatusCode == 401:
		code = gws.CodeExpiredCredentials
	case resp.StatusCode == 403:
		code = gws.CodePermissionDenied
	case resp.StatusCode == 404:
		code = gws.CodeNotFound
	case resp.StatusCode == 429:
		code = gws.CodeRateLimit
	case resp.StatusCode >= 500:
		code = gws.CodeProviderFailure
	case resp.StatusCode >= 400:
		code = gws.CodeInvalidInput
	}
	if provider != "" && code == "" {
		code = providerCode(provider)
		if code == "" {
			code = gws.CodeProviderFailure
		}
	} else if provider != "" {
		if c := providerCode(provider); c != "" {
			code = c
		}
	}
	if code == "" {
		return nil
	}
	if len(msg) > 200 {
		msg = msg[:200] + "…"
	}
	e := gws.NewError(code, a.redact(fmt.Sprintf("%s: %s (http %d, provider code %q) %s", a.Client, code, resp.StatusCode, provider, msg)), nil)
	e.ProviderCode = provider
	if ra := resp.Header.Get("Retry-After"); ra != "" && code == gws.CodeRateLimit {
		e.RetryAfter = ra
		e.Retryable = true
	}
	return e
}

// providerError extracts the provider's own error code/message from the
// documented shapes: Slack {"ok":false,"error":"…"}, Salesforce
// [{"errorCode":"…","message":"…"}], Notion
// {"object":"error","code":"…","message":"…"} and Airtable
// {"error":{"type":"…","message":"…"}}. Shapes for other Client values are
// unrecognized: classification falls back to the HTTP status.
func (a API) providerError(data []byte) (code, msg string) {
	switch a.Client {
	case "slack":
		var s struct {
			OK    *bool  `json:"ok"`
			Error string `json:"error"`
		}
		if json.Unmarshal(data, &s) == nil && s.OK != nil && !*s.OK {
			return s.Error, ""
		}
	case "sf":
		var s []struct {
			ErrorCode string `json:"errorCode"`
			Message   string `json:"message"`
		}
		if json.Unmarshal(data, &s) == nil && len(s) > 0 && s[0].ErrorCode != "" {
			return s[0].ErrorCode, s[0].Message
		}
	case "notion":
		var s struct {
			Object  string `json:"object"`
			Code    string `json:"code"`
			Message string `json:"message"`
		}
		if json.Unmarshal(data, &s) == nil && s.Object == "error" && s.Code != "" {
			return s.Code, s.Message
		}
	case "airtable":
		var s struct {
			Error struct {
				Type    string `json:"type"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(data, &s) == nil && s.Error.Type != "" {
			return s.Error.Type, s.Error.Message
		}
	}
	return "", ""
}

func providerCode(p string) gws.Code {
	switch p {
	case "invalid_auth", "token_expired", "token_revoked", "not_authed", "INVALID_SESSION_ID":
		return gws.CodeExpiredCredentials
	case "missing_scope", "not_allowed_token_type", "no_permission", "INSUFFICIENT_ACCESS_OR_READONLY", "INSUFFICIENT_ACCESS":
		return gws.CodePermissionDenied
	case "channel_not_found", "message_not_found", "NOT_FOUND", "ENTITY_IS_DELETED":
		return gws.CodeNotFound
	case "ratelimited", "REQUEST_LIMIT_EXCEEDED":
		return gws.CodeRateLimit
	case "invalid_arguments", "invalid_arg_name", "msg_too_long", "no_text", "MALFORMED_QUERY", "INVALID_FIELD", "REQUIRED_FIELD_MISSING":
		return gws.CodeInvalidInput
	case "unauthorized":
		return gws.CodeExpiredCredentials
	case "restricted_resource":
		return gws.CodePermissionDenied
	case "object_not_found":
		return gws.CodeNotFound
	case "rate_limited":
		return gws.CodeRateLimit
	case "invalid_request", "invalid_request_url", "invalid_json", "validation_error", "missing_version":
		return gws.CodeInvalidInput
	case "AUTHENTICATION_REQUIRED":
		return gws.CodeExpiredCredentials
	// "NOT_FOUND" already maps to target-not-found via the shared Salesforce row.
	case "INVALID_REQUEST", "INVALID_VALUE_FOR_COLUMN", "INVALID_FILTER_BY_FORMULA":
		return gws.CodeInvalidInput
	}
	return ""
}

// SlackPost posts a message through the Web API. It validates before effects
// (sandbox-only, bounded payload), claims the ledger key, and reports the
// outcome. A replayed key does not post again.
func SlackPost(ctx context.Context, a API, l *Ledger, m Mutation, channel, text string) (ts string, _ *gws.Error) {
	m.Op = "message-post"
	payload, _ := json.Marshal(map[string]string{"channel": channel, "text": text})
	m.Payload = payload
	plan, e := m.Validate()
	if e != nil {
		return "", e
	}
	if m.DryRun {
		return "", nil
	}
	if prior, replay := l.Begin(plan.Key); replay {
		return "", gws.NewError(gws.CodePartialMutation, "slack: replayed idempotency key — not posting again; prior outcome "+prior, nil).WithOp("message-post", channel)
	}
	r, e := a.Do(ctx, "POST", "chat.postMessage", nil, payload, nil)
	if e != nil {
		out := "failed"
		if e.Code == gws.CodeTimeout || e.Code == gws.CodeCancelled {
			out = "unknown"
		}
		l.Finish(plan.Key, out)
		return "", e
	}
	var res struct {
		TS string `json:"ts"`
	}
	_ = json.Unmarshal(r.Body, &res)
	l.Finish(plan.Key, "confirmed")
	return res.TS, nil
}

// SalesforceUpsert upserts one record by external id (REST PATCH), which is
// naturally idempotent: a repeat converges on the same record.
func SalesforceUpsert(ctx context.Context, a API, apiVersion string, m Mutation, object, extField, extValue string, fields map[string]any) *gws.Error {
	m.Op = "record-upsert"
	payload, _ := json.Marshal(fields)
	m.Payload = payload
	if _, e := m.Validate(); e != nil {
		return e
	}
	if m.DryRun {
		return nil
	}
	p := fmt.Sprintf("services/data/%s/sobjects/%s/%s/%s", apiVersion, url.PathEscape(object), url.PathEscape(extField), url.PathEscape(extValue))
	_, e := a.Do(ctx, "PATCH", p, nil, payload, nil)
	return e
}

// SalesforceDelete deletes one record by id (cleanup path).
func SalesforceDelete(ctx context.Context, a API, apiVersion string, m Mutation, object, id string) *gws.Error {
	m.Op = "record-delete"
	m.Payload = []byte(object + "/" + id)
	if _, e := m.Validate(); e != nil {
		return e
	}
	if m.DryRun {
		return nil
	}
	_, e := a.Do(ctx, "DELETE", fmt.Sprintf("services/data/%s/sobjects/%s/%s", apiVersion, url.PathEscape(object), url.PathEscape(id)), nil, nil, nil)
	return e
}
