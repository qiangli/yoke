package broker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/secrets"
)

// APIUpstream is an OpenAI-compatible endpoint: requests go to BaseURL plus
// the route suffix (/chat/completions, ...), with the model renamed to
// ModelID and Key as the bearer. Key never leaves this host.
type APIUpstream struct {
	BaseURL string
	ModelID string
	Key     string
}

// APIResolver maps a requested model to an API upstream. found=false means
// the model is not an API model (the door routes it as before); an error
// means it is one but cannot be served now (missing credential, ...).
type APIResolver interface {
	ResolveAPI(ctx context.Context, model string) (up APIUpstream, found bool, err error)
}

// apiPassthroughPath reports the OpenAI routes an API model is served on.
func apiPassthroughPath(p string) bool {
	switch apiSuffix(p) {
	case "/chat/completions", "/completions", "/embeddings":
		return true
	}
	return false
}

func apiSuffix(p string) string {
	return strings.TrimPrefix(strings.TrimPrefix(p, "/openai"), "/v1")
}

var apiClient = &http.Client{}

// serveAPI forwards the request body unchanged (only the model renamed) and
// streams the upstream's status, content type and body back. The door's
// gate, peer limits and audit have already run; usage is read from the
// captured tail like any other backend.
func (b *Broker) serveAPI(w http.ResponseWriter, r *http.Request, ri *reqInfo, payload map[string]json.RawMessage, model string, up APIUpstream) {
	ri.rec.Model, ri.rec.UpstreamModel = model, up.ModelID
	if up.ModelID != "" && up.ModelID != model {
		payload["model"], _ = json.Marshal(up.ModelID)
	}
	body, err := json.Marshal(payload)
	if err != nil {
		writeErr(w, r.URL.Path, http.StatusBadRequest, err.Error())
		return
	}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, strings.TrimRight(up.BaseURL, "/")+apiSuffix(r.URL.Path), bytes.NewReader(body))
	if err != nil {
		writeErr(w, r.URL.Path, http.StatusBadGateway, err.Error())
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+up.Key)
	if a := r.Header.Get("Accept"); a != "" {
		req.Header.Set("Accept", a)
	}
	resp, err := apiClient.Do(req)
	if err != nil {
		writeErr(w, r.URL.Path, http.StatusBadGateway, "api upstream: "+err.Error())
		return
	}
	defer resp.Body.Close()
	for _, k := range []string{"Content-Type", "Cache-Control"} {
		if v := resp.Header.Get(k); v != "" {
			w.Header().Set(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	fl, _ := w.(http.Flusher)
	buf := make([]byte, 32<<10)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			if fl != nil {
				fl.Flush()
			}
		}
		if rerr != nil {
			return
		}
	}
}

// FleetAPI resolves fleet API models: kind api with a base_url and an
// api_key_ref. The key resolves like a launch does (environment, then the
// host's secrets.map binding, which may be a minting !command) and is reused
// for apiKeyTTL so a minting command does not run per request.
type FleetAPI struct {
	mu   sync.Mutex
	keys map[string]apiKey
	// lookup and resolveKey are seams for tests (nil = fleet + secrets).
	lookup     func(name string) (fleet.Model, bool)
	resolveKey func(ref string) (string, bool)
}

type apiKey struct {
	value string
	at    time.Time
}

const apiKeyTTL = 30 * time.Minute

func (f *FleetAPI) ResolveAPI(_ context.Context, model string) (APIUpstream, bool, error) {
	lookup := f.lookup
	if lookup == nil {
		lookup = func(name string) (fleet.Model, bool) { return fleet.New().Model(name) }
	}
	m, ok := lookup(strings.TrimSpace(model))
	if !ok || m.Kind != fleet.ModelKindAPI || strings.TrimSpace(m.BaseURL) == "" || strings.TrimSpace(m.APIKeyRef) == "" {
		return APIUpstream{}, false, nil
	}
	key, err := f.key(m.APIKeyRef)
	if err != nil {
		return APIUpstream{}, true, fmt.Errorf("model %s: %w", m.Name, err)
	}
	return APIUpstream{BaseURL: m.BaseURL, ModelID: m.Target(), Key: key}, true, nil
}

func (f *FleetAPI) key(ref string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if k, ok := f.keys[ref]; ok && time.Since(k.at) < apiKeyTTL {
		return k.value, nil
	}
	resolve := f.resolveKey
	if resolve == nil {
		resolve = func(ref string) (string, bool) {
			kv, ok := secrets.ResolveAgentKey(os.Environ(), ref)
			if !ok {
				return "", false
			}
			_, v, _ := strings.Cut(kv, "=")
			return strings.TrimSpace(v), v != ""
		}
	}
	v, ok := resolve(ref)
	if !ok || v == "" {
		return "", fmt.Errorf("credential %s is not bound on this host (secrets.map or environment)", ref)
	}
	if f.keys == nil {
		f.keys = map[string]apiKey{}
	}
	f.keys[ref] = apiKey{value: v, at: time.Now()}
	return v, nil
}
