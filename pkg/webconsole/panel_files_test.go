// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package webconsole

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestFilesBrandingUsesTheFolderIcon(t *testing.T) {
	h, closer, err := filesPanel(t.TempDir(), false)
	if err != nil {
		t.Fatalf("filesPanel: %v", err)
	}
	t.Cleanup(func() { _ = closer() })

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/public/static/favicon.svg", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("logo status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "image/svg+xml") {
		t.Fatalf("logo content-type = %q, want image/svg+xml", ct)
	}
	body := rec.Body.String()
	if !strings.Contains(body, filesLogoPath) {
		t.Errorf("branded logo does not carry the folder icon path:\n%s", body)
	}
	for _, stock := range []string{"prefix__", "#2bbcff", "#006498", "<rect"} {
		if strings.Contains(body, stock) {
			t.Errorf("branded icon still looks like the stock one (%q)", stock)
		}
	}
}

func TestFilesBrandingLeavesEverythingElseAlone(t *testing.T) {
	h, closer, err := filesPanel(t.TempDir(), false)
	if err != nil {
		t.Fatalf("filesPanel: %v", err)
	}
	t.Cleanup(func() { _ = closer() })

	// The SPA shell and its other assets must pass through to fbembed.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("index status = %d, want 200", rec.Code)
	}
	if strings.Contains(rec.Body.String(), filesLogoPath) {
		t.Error("index page unexpectedly contains the folder icon path")
	}
}
