package live

// Copy sheets served by the hub (Sprint 529). `bashy browser copysheet`
// writes a sheet into SheetDir; the hub serves it on an unguessable
// loopback URL so the extension can open it beside the form — Chrome's
// split view when available (tabs.create splitWithTabId, Chrome 155+).
// Extensions may not open file:// pages, and a 127.0.0.1 page is a secure
// context, so the sheet's clipboard API works there.

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// sheetTTL bounds how long a registered sheet URL stays valid.
const sheetTTL = 30 * time.Minute

// sheetReg is one registered sheet: its file, when its token expires, and
// the secret names it may ask the hub for (S529.7).
type sheetReg struct {
	path    string
	expires time.Time
	secrets map[string]bool
}

// SheetSecretsMeta is the <meta name> a copy sheet uses to declare the
// secret names it shows by reference; the hub reads it when registering.
const SheetSecretsMeta = "bashy-sheet-secrets"

var sheetSecretsMeta = regexp.MustCompile(`<meta name="` + SheetSecretsMeta + `" content="([^"]*)">`)

// sheetSecretNames reads the declared secret names from a sheet file. Only
// copy sheets bashy renders declare names; an agent-made companion view
// (view-*.html, `bashy browser beside`) never gets secret access.
func sheetSecretNames(p string) map[string]bool {
	if strings.HasPrefix(filepath.Base(p), "view-") {
		return nil
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return nil
	}
	m := sheetSecretsMeta.FindSubmatch(b)
	if m == nil {
		return nil
	}
	out := map[string]bool{}
	for _, n := range strings.Split(string(m[1]), ",") {
		if ValidSheetSecretName(n) {
			out[n] = true
		}
	}
	return out
}

// SheetDir is where copy sheets live; the hub serves files from here only.
func SheetDir() string {
	dir, err := os.UserCacheDir()
	if err != nil {
		dir = os.TempDir()
	}
	return filepath.Join(dir, "bashy", "copysheet")
}

// sheetDir is SheetDir, a variable only so tests can point it elsewhere.
var sheetDir = SheetDir

// insideSheetDir reports whether path is an .html file directly under the
// sheet directory (no traversal, no symlink escape).
func insideSheetDir(path string) (string, bool) {
	dir, err := filepath.EvalSymlinks(sheetDir())
	if err != nil {
		return "", false
	}
	p, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", false
	}
	if filepath.Dir(p) != dir || !strings.HasSuffix(p, ".html") {
		return "", false
	}
	return p, true
}

// handleSheetRegister maps a sheet file to a fresh token and returns its URL:
// POST /sheet {"path": "..."} -> {"url": "http://127.0.0.1:PORT/sheet/TOKEN"}.
func (h *hub) handleSheetRegister(w http.ResponseWriter, r *http.Request) {
	if !loopbackHost(r) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	if h.isPaused() {
		http.Error(w, pausedMessage, http.StatusServiceUnavailable)
		return
	}
	var req struct {
		Path string `json:"path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	p, ok := insideSheetDir(req.Path)
	if !ok {
		http.Error(w, "not a copy sheet", http.StatusForbidden)
		return
	}
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		http.Error(w, "token", http.StatusInternalServerError)
		return
	}
	token := hex.EncodeToString(b)
	reg := &sheetReg{path: p, expires: time.Now().Add(sheetTTL), secrets: sheetSecretNames(p)}
	h.mu.Lock()
	if h.sheets == nil {
		h.sheets = map[string]*sheetReg{}
	}
	for t, r := range h.sheets {
		if time.Now().After(r.expires) {
			delete(h.sheets, t)
		}
	}
	h.sheets[token] = reg
	h.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"url": "http://" + h.addr + "/sheet/" + token})
}

// lookupSheet returns the live registration for token.
func (h *hub) lookupSheet(token string) (*sheetReg, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	reg, ok := h.sheets[token]
	if !ok || time.Now().After(reg.expires) {
		return nil, false
	}
	return reg, true
}

// handleSheet serves a registered sheet by token: GET /sheet/TOKEN, and its
// secrets by reference: POST /sheet/TOKEN/secret {"name": "..."}.
func (h *hub) handleSheet(w http.ResponseWriter, r *http.Request) {
	if !loopbackHost(r) {
		http.NotFound(w, r)
		return
	}
	if h.isPaused() {
		http.Error(w, pausedMessage, http.StatusServiceUnavailable)
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/sheet/")
	if token, ok := strings.CutSuffix(rest, "/secret"); ok {
		h.handleSheetSecret(w, r, token)
		return
	}
	reg, ok := h.lookupSheet(rest)
	if !ok || r.Method != http.MethodGet {
		http.NotFound(w, r)
		return
	}
	p := reg.path
	if _, ok := insideSheetDir(p); !ok {
		http.NotFound(w, r)
		return
	}
	b, err := os.ReadFile(p)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	// Same-origin framing only: another site cannot embed the sheet.
	w.Header().Set("X-Frame-Options", "SAMEORIGIN")
	// A sheet that shows secrets may talk to the hub only: nothing it holds
	// can be sent to another origin (forms, fetches, images, frames).
	if len(reg.secrets) > 0 {
		w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; img-src 'self' data:; connect-src 'self'; form-action 'none'; base-uri 'none'; frame-ancestors 'self'")
	}
	_, _ = w.Write(b)
}

// handleSheetSecret returns one secret value to the sheet page that declared
// it. Threat model: docs/browser-sheet-secrets.md. The value is never logged;
// it is remembered (in memory) only so call() can redact it from extension
// results.
func (h *hub) handleSheetSecret(w http.ResponseWriter, r *http.Request, token string) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	// Same-origin page requests only: the browser sets Origin on a POST
	// fetch, and Sec-Fetch-Site when it supports fetch metadata.
	if r.Header.Get("Origin") != "http://"+h.addr {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		http.Error(w, "json only", http.StatusUnsupportedMediaType)
		return
	}
	reg, ok := h.lookupSheet(token)
	if !ok {
		http.NotFound(w, r)
		return
	}
	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	if !reg.secrets[req.Name] {
		http.Error(w, "this sheet did not declare that secret", http.StatusForbidden)
		return
	}
	v, err := GetSheetSecret(req.Name)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if errors.Is(err, ErrNoSheetSecret) {
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "no stored value for " + req.Name})
		return
	}
	if err != nil {
		logWarn("live: sheet secret read failed", "name", req.Name)
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "store unreadable"})
		return
	}
	h.rememberServed(v)
	logInfo("live: sheet secret served", "name", req.Name)
	_ = json.NewEncoder(w).Encode(map[string]string{"value": v})
}

// RedactedSheetValue replaces a served sheet secret in extension results.
const RedactedSheetValue = "[guarded:sheet]"

// minRedactLen keeps very short values from shredding ordinary text.
const minRedactLen = 4

// rememberServed records a value the hub handed to a sheet page, so later
// extension results that contain it (the sheet read while revealed, the form
// after a paste) are redacted before any client sees them.
func (h *hub) rememberServed(v string) {
	if len(v) < minRedactLen {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.served == nil {
		h.served = map[string]bool{}
	}
	h.served[v] = true
}

// redactServed replaces every served value in a raw JSON result, matching
// both the plain and the JSON-escaped spelling.
func (h *hub) redactServed(raw []byte) []byte {
	h.mu.Lock()
	vals := make([]string, 0, len(h.served))
	for v := range h.served {
		vals = append(vals, v)
	}
	h.mu.Unlock()
	if len(vals) == 0 || len(raw) == 0 {
		return raw
	}
	for _, v := range vals {
		esc, _ := json.Marshal(v)
		forms := [][]byte{[]byte(v)}
		if e := esc[1 : len(esc)-1]; !bytes.Equal(e, []byte(v)) {
			forms = append(forms, e)
		}
		for _, f := range forms {
			if bytes.Contains(raw, f) {
				raw = bytes.ReplaceAll(raw, f, []byte(RedactedSheetValue))
			}
		}
	}
	return raw
}

// loopbackHost refuses a request whose Host is not the loopback address the
// hub listens on, so a website that rebinds its own name to 127.0.0.1 cannot
// register or read sheets through the victim's browser.
func loopbackHost(r *http.Request) bool {
	host := r.Host
	if i := strings.LastIndex(host, ":"); i >= 0 {
		host = host[:i]
	}
	return host == "127.0.0.1" || host == "localhost" || host == "[::1]"
}
