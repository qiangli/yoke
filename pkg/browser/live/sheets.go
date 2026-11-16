package live

// Copy sheets served by the hub (Sprint 529). `bashy browser copysheet`
// writes a sheet into SheetDir; the hub serves it on an unguessable
// loopback URL so the extension can open it beside the form — Chrome's
// split view when available (tabs.create splitWithTabId, Chrome 155+).
// Extensions may not open file:// pages, and a 127.0.0.1 page is a secure
// context, so the sheet's clipboard API works there.

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

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
	h.mu.Lock()
	if h.sheets == nil {
		h.sheets = map[string]string{}
	}
	h.sheets[token] = p
	h.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"url": "http://" + h.addr + "/sheet/" + token})
}

// handleSheet serves a registered sheet by token: GET /sheet/TOKEN.
func (h *hub) handleSheet(w http.ResponseWriter, r *http.Request) {
	if !loopbackHost(r) {
		http.NotFound(w, r)
		return
	}
	if h.isPaused() {
		http.Error(w, pausedMessage, http.StatusServiceUnavailable)
		return
	}
	token := strings.TrimPrefix(r.URL.Path, "/sheet/")
	h.mu.Lock()
	p, ok := h.sheets[token]
	h.mu.Unlock()
	if !ok || r.Method != http.MethodGet {
		http.NotFound(w, r)
		return
	}
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
	_, _ = w.Write(b)
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
