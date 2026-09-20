// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

package webconsole

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Sprint 220, story 72c86b58: the Cloud section's state and its one action.

func TestCloudStateAnswersPairingAndAddresses(t *testing.T) {
	h := newTestHandler(t, Options{})
	w := do(h, "GET", "/api/cloud", "127.0.0.1:5555", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /api/cloud = %d: %s", w.Code, w.Body.String())
	}
	var v cloudView
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(v.Cloudbox, "/cloudbox/") || !strings.HasSuffix(v.Periscope, "/periscope/") || v.Portal == "" {
		t.Fatalf("addresses = %+v", v)
	}
	if v.Pair.State != "idle" {
		t.Fatalf("a fresh console reports pair state %q, want idle", v.Pair.State)
	}
}

func TestCloudPairRefusesJunkAndRunsTheSequence(t *testing.T) {
	h := newTestHandler(t, Options{})
	for _, body := range []string{`{}`, `{"code":"two words"}`, `not json`} {
		r := httptest.NewRequest("POST", "/api/cloud/pair", strings.NewReader(body))
		r.RemoteAddr = "127.0.0.1:5555"
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("POST %s = %d, want 400", body, w.Code)
		}
	}
	// The sequence itself, through the seam: each step reports, the page
	// polls the state, a second start while one runs is refused.
	p := &cloudPairer{}
	step := make(chan string, 8)
	release := make(chan struct{})
	p.run = func(ctx context.Context, code string, report func(state, msg string)) error {
		report("pairing", "exchanging "+code)
		step <- "pairing"
		<-release
		report("installing", "boot")
		step <- "installing"
		return nil
	}
	if err := p.start("abc123"); err != nil {
		t.Fatal(err)
	}
	<-step
	if st := p.get(); st.State != "pairing" || !strings.Contains(st.Message, "abc123") {
		t.Fatalf("state after step 1 = %+v", st)
	}
	if err := p.start("again"); err == nil || !strings.Contains(err.Error(), "already in progress") {
		t.Fatalf("a concurrent pairing was accepted: %v", err)
	}
	close(release)
	<-step
	deadline := time.Now().Add(2 * time.Second)
	for p.get().State != "paired" && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if st := p.get(); st.State != "paired" {
		t.Fatalf("final state = %+v", st)
	}
}

// The launcher carries the section and its switch: a Cloud section that the
// settings sheet cannot hide is not "like favorite/recent".
func TestLauncherShipsTheCloudSection(t *testing.T) {
	h := newTestHandler(t, Options{})
	w := do(h, "GET", "/app.js", "127.0.0.1:5555", nil)
	js := w.Body.String()
	for _, want := range []string{`"showCloud", "Cloud"`, `cloud-section`, `cloud-pair-btn`, `api/cloud/pair`, `showCloud: true`} {
		if !strings.Contains(js, want) {
			t.Errorf("app.js lacks %q", want)
		}
	}
}
