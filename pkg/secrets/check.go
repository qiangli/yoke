package secrets

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// A verdict is a claim about the CREDENTIAL, not about the request. "The
// provider rejected this key" and "I could not determine whether this key is
// good" are different claims, and collapsing them is the expensive direction
// of error: an operator told INVALID about a working key revokes it, burning
// the credential they just rotated in. Only an explicit provider rejection
// earns statusInvalid; every other outcome is statusUnknown carrying the
// reason it could not be decided.
const (
	statusValid   = "VALID"
	statusInvalid = "INVALID"
	statusUnknown = "UNKNOWN"
)

type probeAuth int

const (
	bearerAuth probeAuth = iota
	anthropicAuth
)

// probeModel is a model name no provider serves. The capability probe exists
// to make the provider run its auth and scope checks on the inference path;
// the request is then rejected at model resolution, so no completion is
// generated and nothing is billed.
const probeModel = "bashy-secrets-check-probe"

const (
	openAICapabilityBody    = `{"model":"` + probeModel + `","messages":[]}`
	anthropicCapabilityBody = `{"model":"` + probeModel + `","max_tokens":1,"messages":[]}`
)

type providerProbe struct {
	baseURL string
	// path is the cheap listing probe. It answers for a fully scoped key
	// without spending anything.
	path string
	// capability is the path the key is actually stored to exercise. It is
	// probed only when the listing path answers 403, which for a
	// scope-restricted project key means "not in scope HERE", not "bad key".
	capability     string
	capabilityBody string
	auth           probeAuth
}

// providerProbes is deliberately data-only so provider endpoints and auth
// conventions stay easy to audit. Aliases are normalized by resolveProvider.
var providerProbes = map[string]providerProbe{
	"moonshot":  {baseURL: "https://api.moonshot.ai/v1", path: "/models", capability: "/chat/completions", capabilityBody: openAICapabilityBody, auth: bearerAuth},
	"deepseek":  {baseURL: "https://api.deepseek.com/v1", path: "/models", capability: "/chat/completions", capabilityBody: openAICapabilityBody, auth: bearerAuth},
	"openai":    {baseURL: "https://api.openai.com/v1", path: "/models", capability: "/chat/completions", capabilityBody: openAICapabilityBody, auth: bearerAuth},
	"zai":       {baseURL: "https://api.z.ai/api/coding/paas/v4", path: "/models", capability: "/chat/completions", capabilityBody: openAICapabilityBody, auth: bearerAuth},
	"anthropic": {baseURL: "https://api.anthropic.com/v1", path: "/models", capability: "/messages", capabilityBody: anthropicCapabilityBody, auth: anthropicAuth},
}

var providerAliases = map[string]string{
	"kimi": "moonshot",
	"glm":  "zai",
}

type checkResult struct {
	Name     string `json:"name"`
	Provider string `json:"provider"`
	Status   string `json:"status"`
	HTTPCode int    `json:"http_code"`
	// Reason names why a verdict could not be reached, or which path
	// established a VALID one. Never carries the credential.
	Reason string `json:"reason,omitempty"`
}

func newCheckCmd(cfg *Config) *cobra.Command {
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "check NAME",
		Short: "Check whether a vault API key authenticates with its provider",
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			name := args[0]
			provider, ok := resolveProvider(name)
			if !ok {
				return fmt.Errorf("unknown provider for %q (supported: %s)", name, strings.Join(supportedProviderNames(), ", "))
			}

			key, err := lookupCheckKey(*cfg, name)
			if err != nil {
				return err
			}

			result := probeProvider(c.Context(), &http.Client{Timeout: 10 * time.Second}, name, provider, key, providerProbes[provider])
			if jsonOutput {
				return json.NewEncoder(c.OutOrStdout()).Encode(result)
			}
			fmt.Fprintf(c.OutOrStdout(), "%s (%s): %s\n", result.Name, result.Provider, result.line())
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "emit a JSON verdict")
	return cmd
}

func (r checkResult) line() string {
	if r.Reason == "" {
		return r.Status
	}
	return r.Status + " (" + r.Reason + ")"
}

func lookupCheckKey(cfg Config, name string) (string, error) {
	client, err := cfg.Resolve()
	if err == nil {
		key, getErr := client.Get(name)
		if getErr == nil {
			return key, nil
		}
		var transportErr *url.Error
		if !errors.As(getErr, &transportErr) {
			return "", getErr
		}
		err = getErr
	}

	// An inherited environment value may be the last usable copy while
	// cloudbox is down, but it must never outrank a reachable vault: rotations
	// cannot update the parent shell's environment.
	if key, ok := lookupEnvironmentKey(name); ok {
		return key, nil
	}
	return "", err
}

func lookupEnvironmentKey(name string) (string, bool) {
	if value, ok := os.LookupEnv(name); ok {
		return value, true
	}
	entry, ok := GrantAgentKey(os.Environ(), name)
	if !ok {
		return "", false
	}
	_, value, ok := strings.Cut(entry, "=")
	return value, ok
}

func resolveProvider(name string) (string, bool) {
	normalized := strings.ToLower(strings.NewReplacer("_", "-", ".", "-", "/", "-").Replace(name))
	parts := strings.FieldsFunc(normalized, func(r rune) bool { return r == '-' })
	for _, part := range parts {
		if _, ok := providerProbes[part]; ok {
			return part, true
		}
		if provider, ok := providerAliases[part]; ok {
			return provider, true
		}
	}
	return "", false
}

func supportedProviderNames() []string {
	names := make([]string, 0, len(providerProbes)+len(providerAliases))
	for name := range providerProbes {
		names = append(names, name)
	}
	for name := range providerAliases {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// request issues one probe. It returns the status code, or an error when the
// provider was never reached — a distinction the caller must preserve, since
// "no answer" is not "rejected".
func (p providerProbe) request(ctx context.Context, client *http.Client, key, path, body string) (int, error) {
	method, reader := http.MethodGet, io.Reader(nil)
	if body != "" {
		method, reader = http.MethodPost, strings.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(p.baseURL, "/")+path, reader)
	if err != nil {
		return 0, err
	}
	if p.auth == anthropicAuth {
		req.Header.Set("x-api-key", key)
		req.Header.Set("anthropic-version", "2023-06-01")
	} else {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	// Drain a bounded amount so the connection can be reused; the body is
	// never inspected, because a provider's prose is not a verdict.
	io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	return resp.StatusCode, nil
}

func probeProvider(ctx context.Context, client *http.Client, name, provider, key string, probe providerProbe) checkResult {
	result := checkResult{Name: name, Provider: provider}

	code, err := probe.request(ctx, client, key, probe.path, "")
	if err != nil {
		result.Status = statusUnknown
		result.Reason = "provider unreachable: " + sanitizeReason(err.Error(), key)
		return result
	}
	result.HTTPCode = code

	// 403 is "authenticated but not authorized for THIS path" — it is never
	// "this credential is bad". A scope-restricted project key lands here on
	// the model-listing probe while working perfectly on the inference path
	// it was stored for, so ask about that capability instead of guessing.
	if code == http.StatusForbidden && probe.capability != "" {
		capCode, capErr := probe.request(ctx, client, key, probe.capability, probe.capabilityBody)
		if capErr != nil {
			result.Status = statusUnknown
			result.Reason = "provider unreachable on " + probe.capability + ": " + sanitizeReason(capErr.Error(), key)
			return result
		}
		result.HTTPCode = capCode
		result.Status, result.Reason = classifyCapability(capCode, probe.path, probe.capability)
		return result
	}

	result.Status, result.Reason = classifyListing(code, probe.path)
	return result
}

// classifyListing judges the cheap listing probe. Only an explicit 401 is a
// rejection of the credential; everything short of a clean 2xx is undecided.
func classifyListing(code int, path string) (status, reason string) {
	switch {
	case code >= 200 && code < 300:
		return statusValid, ""
	case code == http.StatusUnauthorized:
		return statusInvalid, ""
	case code == http.StatusForbidden:
		return statusUnknown, fmt.Sprintf("provider authenticated the credential but refused %s with HTTP 403 (scope, not a bad key); no capability probe is configured for this provider", path)
	}
	return statusUnknown, undecidedReason(code)
}

// classifyCapability judges the inference-path probe that runs after a 403.
// Reaching model resolution at all proves the provider authenticated and
// authorized the credential, so the sentinel model's rejection (400/404) is a
// pass. The accept set is an allowlist on purpose: a false INVALID costs an
// operator their working key, but a false VALID costs them the outage.
func classifyCapability(code int, listingPath, capabilityPath string) (status, reason string) {
	switch {
	case code >= 200 && code < 300, code == http.StatusBadRequest, code == http.StatusNotFound:
		return statusValid, fmt.Sprintf("provider accepted the credential on %s; %s is outside this key's scope", capabilityPath, listingPath)
	case code == http.StatusUnauthorized:
		return statusInvalid, ""
	case code == http.StatusForbidden:
		return statusUnknown, fmt.Sprintf("provider authenticated the credential but refused both %s and %s with HTTP 403 (scope, not a bad key)", listingPath, capabilityPath)
	}
	return statusUnknown, undecidedReason(code)
}

func undecidedReason(code int) string {
	switch {
	case code == http.StatusTooManyRequests:
		return "rate limited by the provider (HTTP 429); the credential was not evaluated"
	case code >= 500:
		return fmt.Sprintf("provider error (HTTP %d); the credential was not evaluated", code)
	}
	return fmt.Sprintf("unexpected status HTTP %d; the credential was not evaluated", code)
}

// sanitizeReason keeps a credential out of a verdict. Transport errors carry
// only an op and a URL today, but a reason string is operator-facing output
// that gets pasted into issues, so it must not be a leak channel.
func sanitizeReason(msg, key string) string {
	if key == "" {
		return msg
	}
	return strings.ReplaceAll(msg, key, "[REDACTED]")
}
