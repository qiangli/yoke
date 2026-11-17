package live

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const hubAddr = "127.0.0.1:58082"

// secretHub is a hub whose sheet dir and store live in temp dirs, with one
// registered sheet declaring the secrets "api-key" and "unset".
func secretHub(t *testing.T) (h *hub, token string) {
	t.Helper()
	sd, gd := t.TempDir(), t.TempDir()
	savedS, savedG := sheetDir, gateDir
	sheetDir = func() string { return sd }
	gateDir = func() string { return gd }
	t.Cleanup(func() { sheetDir, gateDir = savedS, savedG })
	if err := PutSheetSecret("api-key", "sk-live-1234567890"); err != nil {
		t.Fatal(err)
	}
	if err := PutSheetSecret("other", "not-declared-value"); err != nil {
		t.Fatal(err)
	}
	sheet := filepath.Join(sd, "form.html")
	page := `<html><head><meta name="` + SheetSecretsMeta + `" content="api-key,unset"></head><body>sheet</body></html>`
	if err := os.WriteFile(sheet, []byte(page), 0o600); err != nil {
		t.Fatal(err)
	}
	h = newHub(0)
	h.addr = hubAddr
	b, _ := json.Marshal(map[string]string{"path": sheet})
	r := httptest.NewRequest(http.MethodPost, "/sheet", bytes.NewReader(b))
	r.Host = hubAddr
	w := httptest.NewRecorder()
	h.handleSheetRegister(w, r)
	var reg struct{ URL string }
	_ = json.Unmarshal(w.Body.Bytes(), &reg)
	token = strings.TrimPrefix(reg.URL, "http://"+hubAddr+"/sheet/")
	if len(token) != 32 {
		t.Fatalf("register: %d %s", w.Code, w.Body)
	}
	return h, token
}

func askSecret(h *hub, token, name string, mod func(*http.Request)) *httptest.ResponseRecorder {
	b, _ := json.Marshal(map[string]string{"name": name})
	r := httptest.NewRequest(http.MethodPost, "/sheet/"+token+"/secret", bytes.NewReader(b))
	r.Host = hubAddr
	r.Header.Set("Origin", "http://"+hubAddr)
	r.Header.Set("Sec-Fetch-Site", "same-origin")
	r.Header.Set("Content-Type", "application/json")
	if mod != nil {
		mod(r)
	}
	w := httptest.NewRecorder()
	h.handleSheet(w, r)
	return w
}

func TestSheetSecretRoundTripAndRefusals(t *testing.T) {
	h, token := secretHub(t)

	w := askSecret(h, token, "api-key", nil)
	var got struct{ Value string }
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if w.Code != http.StatusOK || got.Value != "sk-live-1234567890" || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("declared secret not served: %d %s", w.Code, w.Body)
	}
	if w := askSecret(h, token, "unset", nil); w.Code != http.StatusNotFound {
		t.Fatalf("a declared name with no value: %d", w.Code)
	}

	refusals := map[string]func(*http.Request){
		"foreign origin":   func(r *http.Request) { r.Header.Set("Origin", "https://evil.example") },
		"no origin":        func(r *http.Request) { r.Header.Del("Origin") },
		"cross-site fetch": func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "cross-site") },
		"rebound host":     func(r *http.Request) { r.Host = "evil.example:58082" },
		"form post":        func(r *http.Request) { r.Header.Set("Content-Type", "text/plain") },
	}
	for name, mod := range refusals {
		if w := askSecret(h, token, "api-key", mod); w.Code == http.StatusOK || strings.Contains(w.Body.String(), "sk-live") {
			t.Errorf("%s: served (%d)", name, w.Code)
		}
	}
	if w := askSecret(h, token, "other", nil); w.Code != http.StatusForbidden {
		t.Errorf("an undeclared name was served: %d", w.Code)
	}
	if w := askSecret(h, "0123456789abcdef0123456789abcdef", "api-key", nil); w.Code != http.StatusNotFound {
		t.Errorf("an unregistered token was served: %d", w.Code)
	}

	h.mu.Lock()
	h.sheets[token].expires = time.Now().Add(-time.Second)
	h.mu.Unlock()
	if w := askSecret(h, token, "api-key", nil); w.Code != http.StatusNotFound {
		t.Errorf("an expired token was served: %d", w.Code)
	}
}

func TestSheetSecretRefusedWhilePaused(t *testing.T) {
	h, token := secretHub(t)
	h.gate = GateState{Paused: true}
	if w := askSecret(h, token, "api-key", nil); w.Code != http.StatusServiceUnavailable || strings.Contains(w.Body.String(), "sk-live") {
		t.Fatalf("served while paused: %d", w.Code)
	}
}

func TestServedSecretIsRedactedFromExtensionResults(t *testing.T) {
	h, token := secretHub(t)
	if w := askSecret(h, token, "api-key", nil); w.Code != http.StatusOK {
		t.Fatal(w.Code)
	}
	raw := json.RawMessage(`{"text":"key: sk-live-1234567890 end","html":"<pre>sk-live-1234567890</pre>"}`)
	out := string(h.redactServed(raw))
	if strings.Contains(out, "sk-live") || strings.Count(out, RedactedSheetValue) != 2 {
		t.Fatalf("not redacted: %s", out)
	}
	if !json.Valid([]byte(out)) {
		t.Fatalf("redaction broke the JSON: %s", out)
	}
}

func TestSecretSheetGetsStrictCSPAndViewsGetNoSecrets(t *testing.T) {
	h, token := secretHub(t)
	r := httptest.NewRequest(http.MethodGet, "/sheet/"+token, nil)
	r.Host = hubAddr
	w := httptest.NewRecorder()
	h.handleSheet(w, r)
	if csp := w.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "connect-src 'self'") || !strings.Contains(csp, "default-src 'none'") {
		t.Fatalf("secret sheet CSP = %q", csp)
	}
	view := filepath.Join(sheetDir(), "view-x.html")
	_ = os.WriteFile(view, []byte(`<meta name="`+SheetSecretsMeta+`" content="api-key">`), 0o600)
	if got := sheetSecretNames(view); got != nil {
		t.Fatalf("a companion view was granted secrets: %v", got)
	}
}

func TestSheetStoreNamesAndModes(t *testing.T) {
	gd := t.TempDir()
	saved := gateDir
	gateDir = func() string { return gd }
	t.Cleanup(func() { gateDir = saved })
	for _, bad := range []string{"", ".", "..", "../x", "a/b", ".hidden", strings.Repeat("a", 65)} {
		if err := PutSheetSecret(bad, "v"); err == nil {
			t.Errorf("name %q accepted", bad)
		}
	}
	if err := PutSheetSecret("k", "v1"); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(filepath.Join(SheetSecretDir(), "k")); fi == nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("store file mode: %v", fi)
	}
	if v, err := GetSheetSecret("k"); err != nil || v != "v1" {
		t.Fatalf("get = %q %v", v, err)
	}
	_ = DeleteSheetSecret("k")
	if _, err := GetSheetSecret("k"); err != ErrNoSheetSecret {
		t.Fatalf("after delete: %v", err)
	}
}
