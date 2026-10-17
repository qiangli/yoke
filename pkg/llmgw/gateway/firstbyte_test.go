package gateway

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// A negative bound disables the first-byte fail-fast: a slow (local)
// upstream is served. The sub-second delay stands in for a CPU-prefill
// first byte that arrives past the remote 30 s bound.
func TestFirstByteTransportDisabledServesSlowUpstream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(300 * time.Millisecond):
			_, _ = io.WriteString(w, `ok`)
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(srv.Close)
	client := &http.Client{Transport: FirstByteTransport(-1), Timeout: 10 * time.Second}
	resp, err := client.Get(srv.URL)
	if err != nil {
		t.Fatalf("disabled bound must serve the slow upstream: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || string(raw) != "ok" {
		t.Fatalf("status=%d body=%q, want 200 ok", resp.StatusCode, raw)
	}
	if tr, ok := FirstByteTransport(-1).(*http.Transport); !ok || tr.ResponseHeaderTimeout != 0 {
		t.Fatalf("disabled transport must carry no header timeout")
	}
}

// The remote default is unchanged: a silent upstream still fails at its
// bound with a timeout classification.
func TestFirstByteTransportRemoteDefaultStillFailsFast(t *testing.T) {
	if tr, ok := FirstByteTransport(0).(*http.Transport); !ok || tr.ResponseHeaderTimeout != DefaultFirstByteTimeout {
		t.Fatalf("zero bound must select DefaultFirstByteTimeout (%v)", DefaultFirstByteTimeout)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(5 * time.Second):
			_, _ = io.WriteString(w, `too late`)
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(srv.Close)
	client := &http.Client{Transport: FirstByteTransport(100 * time.Millisecond), Timeout: 10 * time.Second}
	start := time.Now()
	_, err := client.Get(srv.URL)
	if err == nil {
		t.Fatal("silent upstream must fail at its first-byte bound")
	}
	if got := ClassifyTransportError(err); got.Class != UpstreamTimeout || got.Status != http.StatusGatewayTimeout {
		t.Fatalf("class=%q status=%d, want timeout/504", got.Class, got.Status)
	}
	if elapsed := time.Since(start); elapsed >= 3*time.Second {
		t.Fatalf("elapsed=%v, want the first-byte bound, not the upstream silence", elapsed)
	}
}
