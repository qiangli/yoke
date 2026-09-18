// Package search is bashy's find-things primitive: query -> results. This file is
// the WEB half (P0a) — a PROVIDER LADDER over key-based search APIs, in the same
// spirit as bashy's execution-path ladder: use whatever backend is configured,
// return a uniform, CITED result the rest of the fleet (and `bashy sota`) consumes.
//
// The key for a backend comes from the environment, which is how the secrets
// vault projects it (`eval "$(bashy secret env)"`). No key on disk here.
//
// Web search is `net`, but it is NOT a lifecycle verb, so it never touches the
// local-first floor (which governs the loop verbs only).
package search

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/qiangli/yoke/pkg/secrets"
)

// Result is one web hit, carrying its provenance (URL + when it was retrieved) so
// a caller can cite it and a reader can verify it resolves.
type Result struct {
	Title       string    `json:"title"`
	URL         string    `json:"url"`
	Snippet     string    `json:"snippet,omitempty"`
	RetrievedAt time.Time `json:"retrieved_at"`
	Backend     string    `json:"backend"`
}

// Options configures a web search.
type Options struct {
	MaxResults int           // default 8
	Backend    string        // "" = auto (ladder by available key); else force one
	Timeout    time.Duration // default 20s
	NoVault    bool          // do not consult the secrets vault (env-only) — keeps tests hermetic
}

// ErrNoBackend is returned when no web-search backend is configured.
var ErrNoBackend = errors.New("search: no web backend configured — set one of " +
	"TAVILY_API_KEY / BRAVE_API_KEY / SERPER_API_KEY (via `bashy secret`), " +
	"or BASHY_SEARCH_BACKEND to name one")

// backend is one rung of the provider ladder. The key is resolved from the
// environment first (the conventional API-key vars), then from the secrets vault
// under `secret` — so a key stored with `bashy secret set brave …` is found with
// no manual `BRAVE_API_KEY=…` mapping.
type backend struct {
	name    string
	envKeys []string
	secret  string // vault secret name
	run     func(ctx context.Context, client *http.Client, key, query string, max int) ([]Result, error)
}

// ladder is the preference order. Tavily first (it is built for agent research and
// returns clean summaries), then Brave (independent index), then Serper (Google).
var ladder = []backend{
	{name: "tavily", envKeys: []string{"TAVILY_API_KEY"}, secret: "tavily", run: runTavily},
	{name: "brave", envKeys: []string{"BRAVE_API_KEY", "BRAVE_SEARCH_API_KEY"}, secret: "brave", run: runBrave},
	{name: "serper", envKeys: []string{"SERPER_API_KEY"}, secret: "serper", run: runSerper},
	// searxng: the keyless, self-hosted rung — last, so a reliable keyed backend
	// is always preferred when a key exists. Its "key" is the ENDPOINT URL, not a
	// secret: set BASHY_SEARXNG_URL (e.g. http://localhost:8888) to enable it,
	// pointing at a SearXNG instance you run (e.g. `bashy podman run … searxng/
	// searxng` — download+exec of the unmodified official image, so its AGPL-3.0
	// stays on that separate program; see docs/licensing-supply-chain-policy.md).
	{name: "searxng", envKeys: []string{"BASHY_SEARXNG_URL", "SEARXNG_URL"}, secret: "searxng-url", run: runSearxng},
}

// Web runs a web search through the first available backend. It returns the
// results and the backend that served them.
func Web(ctx context.Context, query string, opt Options) ([]Result, string, error) {
	query = normalizeQuery(query)
	if query == "" {
		return nil, "", errors.New("search: empty query")
	}
	max := opt.MaxResults
	if max <= 0 {
		max = 8
	}
	timeout := opt.Timeout
	if timeout <= 0 {
		timeout = 20 * time.Second
	}
	client := &http.Client{Timeout: timeout}

	forced := strings.ToLower(strings.TrimSpace(firstNonEmpty(opt.Backend, os.Getenv("BASHY_SEARCH_BACKEND"))))
	keyFor := newKeyResolver(!opt.NoVault)
	var lastErr error
	tried := false
	for _, b := range ladder {
		if forced != "" && forced != b.name {
			continue
		}
		key := keyFor(b)
		if key == "" {
			if forced == b.name {
				return nil, "", fmt.Errorf("search: backend %q selected but its key is not set (env %s or vault secret %q)", b.name, strings.Join(b.envKeys, "/"), b.secret)
			}
			continue
		}
		tried = true
		res, err := b.run(ctx, client, key, query, max)
		if err != nil {
			lastErr = fmt.Errorf("%s: %w", b.name, err)
			continue // fall through the ladder
		}
		return res, b.name, nil
	}
	if lastErr != nil {
		return nil, "", lastErr
	}
	if forced != "" && !tried {
		return nil, "", fmt.Errorf("search: unknown backend %q (have: tavily, brave, serper)", forced)
	}
	return nil, "", ErrNoBackend
}

// --- Tavily: POST https://api.tavily.com/search ---

func runTavily(ctx context.Context, client *http.Client, key, query string, max int) ([]Result, error) {
	body, _ := json.Marshal(map[string]any{
		"api_key":      key,
		"query":        query,
		"max_results":  max,
		"search_depth": "basic",
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.tavily.com/search", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	raw, err := do(client, req)
	if err != nil {
		return nil, err
	}
	return parseTavily(raw)
}

func parseTavily(raw []byte) ([]Result, error) {
	var r struct {
		Results []struct {
			Title   string `json:"title"`
			URL     string `json:"url"`
			Content string `json:"content"`
		} `json:"results"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	out := make([]Result, 0, len(r.Results))
	for _, x := range r.Results {
		out = append(out, Result{Title: cleanSnippet(x.Title), URL: x.URL, Snippet: cleanSnippet(x.Content), RetrievedAt: now, Backend: "tavily"})
	}
	return out, nil
}

// --- Brave: GET https://api.search.brave.com/res/v1/web/search ---

func runBrave(ctx context.Context, client *http.Client, key, query string, max int) ([]Result, error) {
	u := fmt.Sprintf("https://api.search.brave.com/res/v1/web/search?q=%s&count=%d", url.QueryEscape(query), max)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Subscription-Token", key)
	raw, err := do(client, req)
	if err != nil {
		return nil, err
	}
	return parseBrave(raw)
}

func parseBrave(raw []byte) ([]Result, error) {
	var r struct {
		Web struct {
			Results []struct {
				Title       string `json:"title"`
				URL         string `json:"url"`
				Description string `json:"description"`
			} `json:"results"`
		} `json:"web"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	out := make([]Result, 0, len(r.Web.Results))
	for _, x := range r.Web.Results {
		out = append(out, Result{Title: cleanSnippet(x.Title), URL: x.URL, Snippet: cleanSnippet(x.Description), RetrievedAt: now, Backend: "brave"})
	}
	return out, nil
}

// --- Serper: POST https://google.serper.dev/search ---

func runSerper(ctx context.Context, client *http.Client, key, query string, max int) ([]Result, error) {
	body, _ := json.Marshal(map[string]any{"q": query, "num": max})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://google.serper.dev/search", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-KEY", key)
	raw, err := do(client, req)
	if err != nil {
		return nil, err
	}
	return parseSerper(raw)
}

func parseSerper(raw []byte) ([]Result, error) {
	var r struct {
		Organic []struct {
			Title   string `json:"title"`
			Link    string `json:"link"`
			Snippet string `json:"snippet"`
		} `json:"organic"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	out := make([]Result, 0, len(r.Organic))
	for _, x := range r.Organic {
		out = append(out, Result{Title: cleanSnippet(x.Title), URL: x.Link, Snippet: cleanSnippet(x.Snippet), RetrievedAt: now, Backend: "serper"})
	}
	return out, nil
}

// --- SearXNG: GET <endpoint>/search?q=…&format=json (keyless, self-hosted) ---
//
// The "key" here is the endpoint URL (from BASHY_SEARXNG_URL), not a secret —
// SearXNG needs no API key. The instance must have the JSON format enabled
// (search.formats includes `json` in its settings.yml). bashy only speaks HTTP
// to it; the AGPL-3.0 program runs as a separate process.
func runSearxng(ctx context.Context, client *http.Client, endpoint, query string, max int) ([]Result, error) {
	base := strings.TrimRight(strings.TrimSpace(endpoint), "/")
	if base == "" {
		return nil, errors.New("searxng: no endpoint (set BASHY_SEARXNG_URL)")
	}
	u := fmt.Sprintf("%s/search?q=%s&format=json", base, url.QueryEscape(query))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	raw, err := do(client, req)
	if err != nil {
		return nil, err
	}
	return parseSearxng(raw, max)
}

func parseSearxng(raw []byte, max int) ([]Result, error) {
	var r struct {
		Results []struct {
			Title   string `json:"title"`
			URL     string `json:"url"`
			Content string `json:"content"`
		} `json:"results"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	out := make([]Result, 0, max)
	for i, x := range r.Results {
		if i >= max {
			break
		}
		out = append(out, Result{Title: cleanSnippet(x.Title), URL: x.URL, Snippet: cleanSnippet(x.Content), RetrievedAt: now, Backend: "searxng"})
	}
	return out, nil
}

// do runs the request and returns the body, mapping a non-2xx to a clear error.
func do(client *http.Client, req *http.Request) ([]byte, error) {
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("http %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	return raw, nil
}

// Brave rejects queries over 400 characters AND over 50 words (both HTTP 422);
// Tavily/Serper are more lenient, but a search query is keywords not prose, so
// one conservative cap serves them all. A caller that hands over a long research
// question (e.g. `bashy sota`) gets a usable search instead of a 422 — the
// proper fix (decompose the question into several short queries) belongs in the
// caller; this is the defensive floor.
const (
	maxQueryLen   = 380 // chars, under Brave's 400
	maxQueryWords = 48  // under Brave's 50
)

// normalizeQuery collapses whitespace and caps the query by both word count and
// character length (truncating at a word boundary so no term is split), so it is
// accepted by every backend on the ladder.
func normalizeQuery(q string) string {
	fields := strings.Fields(q)
	if len(fields) > maxQueryWords {
		fields = fields[:maxQueryWords]
	}
	q = strings.Join(fields, " ")
	if len(q) <= maxQueryLen {
		return q
	}
	q = q[:maxQueryLen]
	if i := strings.LastIndexByte(q, ' '); i > 0 {
		q = q[:i]
	}
	return q
}

func firstEnv(names []string) string {
	for _, n := range names {
		if v := strings.TrimSpace(os.Getenv(n)); v != "" {
			return v
		}
	}
	return ""
}

// newKeyResolver returns a per-call resolver: environment first, then the secrets
// vault (client resolved once, lazily — a missing pairing simply means env-only).
func newKeyResolver(useVault bool) func(backend) string {
	var client secrets.Client
	var have, triedVault bool
	return func(b backend) string {
		if k := firstEnv(b.envKeys); k != "" {
			return k
		}
		if !useVault {
			return ""
		}
		if !triedVault {
			triedVault = true
			if c, err := (secrets.Config{}).Resolve(); err == nil {
				client, have = c, true
			}
		}
		if !have || b.secret == "" {
			return ""
		}
		if v, err := client.Get(b.secret); err == nil {
			return strings.TrimSpace(v)
		}
		return ""
	}
}

// cleanSnippet strips the HTML some backends (Brave) return in descriptions.
func cleanSnippet(s string) string {
	s = htmlTag.ReplaceAllString(s, "")
	return strings.TrimSpace(html.UnescapeString(s))
}

var htmlTag = regexp.MustCompile(`<[^>]*>`)

func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
