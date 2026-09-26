// Package gateway provides backend selection and failover primitives for an
// HTTP language-model gateway.
package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	"github.com/qiangli/yoke/pkg/llmgw/sched"
)

const (
	capacityProbePath    = "/app/ollama/_pool/capacity"
	loadedProbePath      = "/app/ollama/api/ps"
	capacityProbeTimeout = 300 * time.Millisecond
	loadedProbeTimeout   = 500 * time.Millisecond
	probeBodyLimit       = 64 << 10
)

// Backend is one independently selectable request destination.
type Backend interface {
	Name() string
	Capacity(context.Context) Capacity
	Loaded(context.Context) (map[string]struct{}, bool)
	Serve(http.ResponseWriter, *http.Request, []byte, func(*http.Response) error) Attempt
}

// Attempt describes the outcome of one backend request.
type Attempt struct {
	Status    int
	Committed bool
	CanRetry  bool
}

// DefaultBreaker is used by reverse-proxy backends. Applications that need an
// isolated breaker can replace it during setup.
var DefaultBreaker = sched.NewBreaker()

type reverseProxyBackend struct {
	name     string
	target   *url.URL
	director func(*http.Request)
}

// ReverseProxyBackend returns a backend that proxies to target. The optional
// director runs after the standard single-target rewrite on served requests.
func ReverseProxyBackend(name string, target *url.URL, director func(*http.Request)) Backend {
	var targetCopy url.URL
	if target != nil {
		targetCopy = *target
	}
	return &reverseProxyBackend{name: name, target: &targetCopy, director: director}
}

func (b *reverseProxyBackend) Name() string { return b.name }

func (b *reverseProxyBackend) Capacity(ctx context.Context) Capacity {
	ctx, cancel := context.WithTimeout(ctx, capacityProbeTimeout)
	defer cancel()
	req, ok := b.probeRequest(ctx, capacityProbePath)
	if !ok {
		return Capacity{}
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return Capacity{}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Capacity{}
	}
	var capacity Capacity
	if err := json.NewDecoder(io.LimitReader(resp.Body, probeBodyLimit)).Decode(&capacity); err != nil {
		return Capacity{}
	}
	capacity.setLoad()
	return capacity
}

func (b *reverseProxyBackend) Loaded(ctx context.Context) (map[string]struct{}, bool) {
	ctx, cancel := context.WithTimeout(ctx, loadedProbeTimeout)
	defer cancel()
	req, ok := b.probeRequest(ctx, loadedProbePath)
	if !ok {
		return nil, false
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, false
	}
	var body struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, probeBodyLimit)).Decode(&body); err != nil {
		return nil, false
	}
	loaded := make(map[string]struct{}, len(body.Models))
	for _, model := range body.Models {
		name := normalizeModel(model.Name)
		if name != "" {
			loaded[name] = struct{}{}
		}
	}
	return loaded, true
}

func (b *reverseProxyBackend) probeRequest(ctx context.Context, path string) (*http.Request, bool) {
	if b.target == nil || b.target.Scheme == "" || b.target.Host == "" {
		return nil, false
	}
	endpoint := *b.target
	if strings.TrimSpace(endpoint.Path) == "" || endpoint.Path == "/" {
		endpoint.Path = path
	} else {
		endpoint.Path = strings.TrimRight(endpoint.Path, "/") + strings.TrimPrefix(path, "/app/ollama")
	}
	endpoint.RawPath = ""
	endpoint.RawQuery = ""
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return nil, false
	}
	if b.director != nil {
		b.director(req)
		// Probe routes belong to the backend target even when the served-request
		// director selects a different request path.
		req.URL = &endpoint
		req.Host = endpoint.Host
	}
	return req, true
}

func (b *reverseProxyBackend) Serve(w http.ResponseWriter, r *http.Request, body []byte, modify func(*http.Response) error) Attempt {
	if b.target == nil || b.target.Scheme == "" || b.target.Host == "" {
		return Attempt{Status: http.StatusBadGateway, CanRetry: true}
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	r.ContentLength = int64(len(body))
	r.Header.Set("Content-Length", contentLengthString(len(body)))

	proxy := httputil.NewSingleHostReverseProxy(b.target)
	proxy.FlushInterval = -1
	originalDirector := proxy.Director
	proxy.Director = func(out *http.Request) {
		originalDirector(out)
		if b.director != nil {
			b.director(out)
		}
	}

	attempt := Attempt{}
	counted := &byteWriteCounter{ResponseWriter: w}
	composeFailoverHooks(proxy, &attempt, modify, b.name, &counted.bytesWritten)
	proxy.ServeHTTP(counted, r)
	if counted.bytesWritten > 0 {
		attempt.Committed = true
		attempt.CanRetry = false
	}
	return attempt
}
