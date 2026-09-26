package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
)

func TestReverseProxyBackendProbesCapacityAndLoaded(t *testing.T) {
	var capacityHits, loadedHits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Probe-Hint") != "set" {
			t.Errorf("probe hint missing")
		}
		switch r.URL.Path {
		case capacityProbePath:
			capacityHits.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"version": 2, "max_parallel": 4, "in_flight": 1,
				"queued": 2, "loaded_models": []string{"model-a"},
				"swapping": false, "num_loaded_max": 3,
				"keep_alive_s": 900, "max_queue": 8,
			})
		case loadedProbePath:
			loadedHits.Add(1)
			_, _ = io.WriteString(w, `{"models":[{"name":" Model-A "},{"name":"MODEL-B"}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	target, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	backend := ReverseProxyBackend("alpha", target, func(r *http.Request) {
		r.Header.Set("X-Probe-Hint", "set")
		r.URL.Path = "/served/request"
	})

	capacity := backend.Capacity(context.Background())
	if capacity.MaxParallel != 4 || capacity.InFlight != 1 || capacity.Loaded != 0.25 {
		t.Fatalf("capacity = %+v", capacity)
	}
	if capacity.Version != 2 || capacity.Queued != 2 || capacity.NumLoadedMax != 3 || capacity.KeepAliveS != 900 || capacity.MaxQueue != 8 {
		t.Errorf("v2 capacity fields = %+v", capacity)
	}
	loaded, ok := backend.Loaded(context.Background())
	if !ok || len(loaded) != 2 {
		t.Fatalf("loaded = %v, %v", loaded, ok)
	}
	if _, ok := loaded["model-a"]; !ok {
		t.Errorf("normalized loaded set = %v", loaded)
	}
	if capacityHits.Load() != 1 || loadedHits.Load() != 1 {
		t.Errorf("probe hits capacity=%d loaded=%d", capacityHits.Load(), loadedHits.Load())
	}
}

func TestReverseProxyBackendSwappingClampsLoad(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"max_parallel":8,"in_flight":0,"swapping":true}`)
	}))
	defer server.Close()
	target, _ := url.Parse(server.URL)
	capacity := ReverseProxyBackend("alpha", target, nil).Capacity(context.Background())
	if capacity.Loaded != 1 {
		t.Fatalf("swapping load = %v, want 1", capacity.Loaded)
	}
}

func TestReverseProxyBackendServesBodyAndRunsDirector(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/backend/chat" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if r.Header.Get("X-Principal") != "p" {
			t.Errorf("principal hint missing")
		}
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	defer server.Close()
	target, _ := url.Parse(server.URL)
	backend := ReverseProxyBackend("alpha", target, func(r *http.Request) {
		r.URL.Path = "/backend/chat"
		r.Header.Set("X-Principal", "p")
	})
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader("ignored"))
	attempt := backend.Serve(recorder, request, []byte(`{"model":"m"}`), nil)
	if attempt.Status != http.StatusOK || !attempt.Committed || attempt.CanRetry {
		t.Fatalf("attempt = %+v", attempt)
	}
	if recorder.Body.String() != `{"model":"m"}` {
		t.Errorf("body = %q", recorder.Body.String())
	}
}
