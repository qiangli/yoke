package live

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHubServesRegisteredSheetsOnly(t *testing.T) {
	dir := t.TempDir()
	saved := sheetDir
	sheetDir = func() string { return dir }
	t.Cleanup(func() { sheetDir = saved })
	sheet := filepath.Join(dir, "form.html")
	if err := os.WriteFile(sheet, []byte("<p>hello sheet</p>"), 0o600); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "secret.html")
	_ = os.WriteFile(outside, []byte("nope"), 0o600)

	h := newHub(0)
	h.addr = "127.0.0.1:58082"
	register := func(host, path string) *httptest.ResponseRecorder {
		b, _ := json.Marshal(map[string]string{"path": path})
		r := httptest.NewRequest(http.MethodPost, "/sheet", bytes.NewReader(b))
		r.Host = host
		w := httptest.NewRecorder()
		h.handleSheetRegister(w, r)
		return w
	}
	if w := register("127.0.0.1:58082", outside); w.Code != http.StatusForbidden {
		t.Fatalf("a file outside the sheet dir was registered: %d", w.Code)
	}
	if w := register("127.0.0.1:58082", filepath.Join(dir, "..", filepath.Base(dir), "form.html")); w.Code != http.StatusOK {
		t.Fatalf("a clean path inside the dir was refused: %d", w.Code)
	}
	if w := register("evil.example:58082", sheet); w.Code != http.StatusForbidden {
		t.Fatalf("a rebound Host registered a sheet: %d", w.Code)
	}
	w := register("127.0.0.1:58082", sheet)
	var reg struct{ URL string }
	_ = json.Unmarshal(w.Body.Bytes(), &reg)
	if !strings.HasPrefix(reg.URL, "http://127.0.0.1:58082/sheet/") || len(reg.URL) < len("http://127.0.0.1:58082/sheet/")+32 {
		t.Fatalf("url = %q", reg.URL)
	}
	get := func(host, path string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, path, nil)
		r.Host = host
		w := httptest.NewRecorder()
		h.handleSheet(w, r)
		return w
	}
	path := strings.TrimPrefix(reg.URL, "http://127.0.0.1:58082")
	if w := get("127.0.0.1:58082", path); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "hello sheet") || w.Header().Get("Cache-Control") != "no-store" {
		b, _ := io.ReadAll(w.Body)
		t.Fatalf("registered sheet not served: %d %s", w.Code, b)
	}
	if w := get("evil.example:58082", path); w.Code != http.StatusNotFound {
		t.Fatalf("a rebound Host read the sheet: %d", w.Code)
	}
	if w := get("127.0.0.1:58082", "/sheet/0123456789abcdef0123456789abcdef"); w.Code != http.StatusNotFound {
		t.Fatalf("an unregistered token was served: %d", w.Code)
	}
}
