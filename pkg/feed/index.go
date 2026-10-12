package feed

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// indexTimeout bounds the registry push. The post is already live on the
// sinks when this runs, so a slow registry must never stall the command.
const indexTimeout = 10 * time.Second

// registryKeysFile records, per registry base URL, the public key that base
// has registered for this host. The pubkey is still sent on every push (the
// server needs it to verify); the file is only a local record of what each
// base last accepted.
const registryKeysFile = "registry_keys.json"

// IndexRequest is the POST /api/v1/feed/registry body. It carries hashes,
// the excerpt, the signature and the sink locations — never the post body.
type IndexRequest struct {
	Pubkey    string    `json:"pubkey"`
	ForceKey  bool      `json:"force_key"`
	SinkBases []string  `json:"sink_bases"`
	Post      IndexPost `json:"post"`
}

// IndexPost is the registry's row for one post.
type IndexPost struct {
	ID         string   `json:"id"`
	Path       string   `json:"path"`
	FileSHA256 string   `json:"file_sha256"`
	BodySHA256 string   `json:"body_sha256"`
	Headline   string   `json:"headline"`
	Dek        string   `json:"dek"`
	Labels     []string `json:"labels"`
	ReplyTo    string   `json:"reply_to"`
	Created    string   `json:"created"`
	Sig        string   `json:"sig"`
	URLs       []string `json:"urls"`
}

// IndexResponse is the registry's 200 body.
type IndexResponse struct {
	Indexed   string `json:"indexed"` // "ok" | "deferred"
	Reason    string `json:"reason"`
	UID       string `json:"uid"`
	Viewer    string `json:"viewer"`
	ExpiresAt string `json:"expires_at"`
}

// IndexError is a non-2xx registry answer with the status kept, so the
// caller can word 409 (pubkey conflict) and 410/422 (refusals) differently
// from transport failures.
type IndexError struct {
	Status int
	Reason string
}

func (e *IndexError) Error() string {
	if e.Reason != "" {
		return fmt.Sprintf("feed: registry refused the post (HTTP %d): %s", e.Status, e.Reason)
	}
	return fmt.Sprintf("feed: registry refused the post (HTTP %d)", e.Status)
}

// Indexer registers a published post with the feed registry.
type Indexer interface {
	Index(ctx context.Context, req IndexRequest) (IndexResponse, error)
}

// HTTPIndexer is an Indexer over the registry HTTP API.
type HTTPIndexer struct {
	Base  string
	Token string
	HTTP  *http.Client // nil = a client with indexTimeout
}

func (h HTTPIndexer) client() *http.Client {
	if h.HTTP != nil {
		return h.HTTP
	}
	return &http.Client{Timeout: indexTimeout}
}

// Index POSTs req as JSON to <base>/api/v1/feed/registry with a Bearer
// token. A 200 parses into IndexResponse; any other status becomes an
// *IndexError carrying the server's reason.
func (h HTTPIndexer) Index(ctx context.Context, req IndexRequest) (IndexResponse, error) {
	var out IndexResponse
	body, err := json.Marshal(req)
	if err != nil {
		return out, fmt.Errorf("feed: encoding the registry request: %w", err)
	}
	url := strings.TrimRight(h.Base, "/") + "/api/v1/feed/registry"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return out, fmt.Errorf("feed: registry request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if h.Token != "" {
		httpReq.Header.Set("Authorization", "Bearer "+h.Token)
	}
	resp, err := h.client().Do(httpReq)
	if err != nil {
		return out, fmt.Errorf("feed: registry request: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return out, fmt.Errorf("feed: reading the registry response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return out, &IndexError{Status: resp.StatusCode, Reason: serverReason(raw)}
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return out, fmt.Errorf("feed: decoding the registry response: %w", err)
	}
	return out, nil
}

// serverReason pulls the human reason out of an error response body.
func serverReason(raw []byte) string {
	var m struct {
		Reason  string `json:"reason"`
		Error   string `json:"error"`
		Message string `json:"message"`
	}
	if json.Unmarshal(raw, &m) != nil {
		return strings.TrimSpace(string(raw))
	}
	for _, s := range []string{m.Reason, m.Error, m.Message} {
		if strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
	}
	return ""
}

// SinkBase derives a sink base URL from a published post URL by removing
// the sink-relative path: the viewer reattaches the path to the base. A URL
// that does not end in rel keeps the whole URL (with a trailing slash).
func SinkBase(postURL, rel string) string {
	if rel != "" {
		if base, ok := strings.CutSuffix(postURL, rel); ok {
			return ensureTrailingSlash(base)
		}
	}
	return ensureTrailingSlash(postURL)
}

func ensureTrailingSlash(s string) string {
	if s == "" {
		return s
	}
	return strings.TrimRight(s, "/") + "/"
}

func registryKeysPath(dir string) string { return filepath.Join(dir, registryKeysFile) }

// loadRegistryKeys reads dir/registry_keys.json (missing = none).
func loadRegistryKeys(dir string) (map[string]string, error) {
	b, err := os.ReadFile(registryKeysPath(dir))
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]string{}, nil
		}
		return nil, fmt.Errorf("feed: load registry keys: %w", err)
	}
	var keys map[string]string
	if err := json.Unmarshal(b, &keys); err != nil {
		return nil, fmt.Errorf("feed: load registry keys: %w", err)
	}
	if keys == nil {
		keys = map[string]string{}
	}
	return keys, nil
}

// noteRegistryKey records that base accepted pubkey. It never fails the
// post: the registry is authoritative and the file is only a local record.
func noteRegistryKey(dir, base, pubkey string) {
	keys, err := loadRegistryKeys(dir)
	if err != nil {
		return
	}
	if keys[base] == pubkey {
		return
	}
	keys[base] = pubkey
	b, err := json.MarshalIndent(keys, "", "  ")
	if err != nil {
		return
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return
	}
	tmp, err := os.CreateTemp(dir, "registry_keys-*.json")
	if err != nil {
		return
	}
	name := tmp.Name()
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		tmp.Close()
		os.Remove(name)
		return
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return
	}
	if err := os.Chmod(name, 0o600); err != nil {
		os.Remove(name)
		return
	}
	if err := os.Rename(name, registryKeysPath(dir)); err != nil {
		os.Remove(name)
	}
}

// isPublicSinkURL reports whether s is an http(s) URL with a host. Local
// rclone paths and "<remote>#<branch>:<path>" git refs must never leak to
// the registry, which rejects non-http(s) entries with 400.
func isPublicSinkURL(s string) bool {
	u, err := url.Parse(s)
	if err != nil {
		return false
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return false
	}
	return u.Host != ""
}

// publicSinkURLs keeps only http(s) URLs with a host, preserving order.
func publicSinkURLs(urls []string) []string {
	out := make([]string, 0, len(urls))
	for _, u := range urls {
		if isPublicSinkURL(u) {
			out = append(out, u)
		}
	}
	return out
}

// buildIndexRequest assembles the registry push for a post that is already
// live on okURLs. Only http(s) URLs reach the wire: local rclone paths and
// git "<remote>#<branch>:<path>" refs are dropped from both urls and
// sink_bases. fileSHA is over data, the exact bytes written to the sinks;
// the body itself is never included.
func buildIndexRequest(p *Post, rel string, data []byte, okURLs []string, forceKey bool) IndexRequest {
	fileSum := sha256.Sum256(data)
	urls := publicSinkURLs(okURLs)
	bases := make([]string, 0, len(urls))
	for _, u := range urls {
		bases = append(bases, SinkBase(u, rel))
	}
	labels := append([]string{}, p.Labels...)
	if p.Enc == "v1" {
		labels = []string{}
	}
	if labels == nil {
		labels = []string{}
	}
	return IndexRequest{
		Pubkey:    p.Pubkey,
		ForceKey:  forceKey,
		SinkBases: bases,
		Post: IndexPost{
			ID:         p.ID,
			Path:       rel,
			FileSHA256: hex.EncodeToString(fileSum[:]),
			BodySHA256: p.BodySHA256(),
			Headline:   p.Headline,
			Dek:        p.Dek,
			Labels:     labels,
			ReplyTo:    p.ReplyTo,
			Created:    p.Created,
			Sig:        p.Sig,
			URLs:       urls,
		},
	}
}

// reportIndexResult prints one registry outcome line to warn: ok with the
// viewer path, deferred with the reason, a 409 pubkey conflict pointing at
// --force-key, a 410/422/400 refusal with the server reason, and anything
// else as a failed-but-live warning.
func reportIndexResult(warn io.Writer, resp IndexResponse, err error) {
	var ie *IndexError
	switch {
	case err == nil && resp.Indexed == "deferred":
		reason := strings.TrimSpace(resp.Reason)
		if reason == "" {
			reason = "over quota"
		}
		fmt.Fprintf(warn, "index: deferred (%s)\n", oneLine(reason))
	case err == nil:
		fmt.Fprintf(warn, "index: ok %s\n", oneLine(strings.TrimSpace(resp.Viewer)))
	case errors.As(err, &ie) && ie.Status == http.StatusConflict:
		msg := "index: pubkey conflict: the registry already holds a different key for this host; re-run with --force-key to replace it"
		if ie.Reason != "" {
			msg += ": " + ie.Reason
		}
		fmt.Fprintln(warn, oneLine(msg))
	case errors.As(err, &ie) && ie.Status == http.StatusGone ||
		errors.As(err, &ie) && ie.Status == http.StatusUnprocessableEntity ||
		errors.As(err, &ie) && ie.Status == http.StatusBadRequest:
		reason := strings.TrimSpace(ie.Reason)
		if reason == "" {
			reason = http.StatusText(ie.Status)
		}
		fmt.Fprintf(warn, "index: refused: %s\n", oneLine(reason))
	default:
		fmt.Fprintf(warn, "index: failed: %s\n", oneLine(err.Error()))
	}
}
