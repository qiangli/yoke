package live

// The hub's logical on/off gate (Sprint 529, S529.10). The hub process stays
// up (outpost's browser-hub service, or `bashy browser hub`); "off" is a gate
// inside it, so turning it on or off never starts or stops a process. While
// paused the hub refuses the extension (close code CloseHubPaused), every
// /dispatch call and the copy sheets. The state persists across restarts.
//
// Controls: `bashy browser hub pause|resume|status` and the optional tray
// item, through GET/POST /gate on loopback. A change needs the control token
// from a 0600 file under $BASHY_HOME/browser, so a web page or a process of
// another user cannot re-enable browser control.

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// CloseHubPaused is the close code for an extension refused while paused.
const CloseHubPaused = 4423

// GateTokenHeader carries the control token on POST /gate.
const GateTokenHeader = "X-Bashy-Gate-Token"

// GateState is the persisted gate.
type GateState struct {
	Paused bool      `json:"paused"`
	By     string    `json:"by,omitempty"` // tray, cli, agent, …
	At     time.Time `json:"at,omitempty"`
}

// gateDir holds the gate state and control token: $BASHY_HOME/browser
// (default ~/.bashy/browser). A variable only so tests can redirect it.
var gateDir = func() string {
	if h := strings.TrimSpace(os.Getenv("BASHY_HOME")); h != "" {
		return filepath.Join(h, "browser")
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		home = "."
	}
	return filepath.Join(home, ".bashy", "browser")
}

func gateStatePath(port int) string {
	return filepath.Join(gateDir(), fmt.Sprintf("hub-gate-%d.json", port))
}

func gateTokenPath() string { return filepath.Join(gateDir(), "hub-control.token") }

// ControlToken returns the gate control token, creating it (0600) on first
// use. The hub and its controllers (CLI, tray) run as the same user.
func ControlToken() (string, error) {
	p := gateTokenPath()
	if b, err := os.ReadFile(p); err == nil && len(strings.TrimSpace(string(b))) >= 32 {
		return strings.TrimSpace(string(b)), nil
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return "", err
	}
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	tok := hex.EncodeToString(b)
	if err := os.WriteFile(p, []byte(tok+"\n"), 0o600); err != nil {
		return "", err
	}
	return tok, nil
}

func loadGate(port int) GateState {
	var g GateState
	if b, err := os.ReadFile(gateStatePath(port)); err == nil {
		_ = json.Unmarshal(b, &g)
	}
	return g // default: open
}

func saveGate(port int, g GateState) error {
	if err := os.MkdirAll(gateDir(), 0o700); err != nil {
		return err
	}
	b, _ := json.Marshal(g)
	return os.WriteFile(gateStatePath(port), b, 0o600)
}

// pausedMessage is what a refused caller reads.
const pausedMessage = "browser hub is paused by the user — turn it on from the tray or run: bashy browser hub resume"

func (h *hub) isPaused() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.gate.Paused
}

// handleGate serves GET /gate (state) and POST /gate {"paused","by"}.
func (h *hub) handleGate(w http.ResponseWriter, r *http.Request) {
	if !loopbackHost(r) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	switch r.Method {
	case http.MethodGet:
	case http.MethodPost:
		want, err := ControlToken()
		got := r.Header.Get(GateTokenHeader)
		if err != nil || subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
			http.Error(w, "bad control token", http.StatusForbidden)
			return
		}
		var req struct {
			Paused bool   `json:"paused"`
			By     string `json:"by"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		g := GateState{Paused: req.Paused, By: req.By, At: time.Now().UTC()}
		h.mu.Lock()
		h.gate = g
		conn := h.conn
		if g.Paused {
			h.conn = nil
		}
		h.mu.Unlock()
		if g.Paused && conn != nil {
			closeWithCode(conn, CloseHubPaused, pausedMessage)
		}
		if err := saveGate(h.port, g); err != nil {
			logWarn("live: persist gate", "error", err)
		}
		state := "open"
		if g.Paused {
			state = "paused"
		}
		logInfo("live: hub gate "+state, "by", g.By)
	default:
		http.Error(w, "GET or POST", http.StatusMethodNotAllowed)
		return
	}
	h.mu.Lock()
	g := h.gate
	h.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(g)
}

// GateStatus reads the gate of the hub on port.
func GateStatus(ctx context.Context, port int) (GateState, error) {
	var g GateState
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/gate", port), nil)
	if err != nil {
		return g, err
	}
	resp, err := (&http.Client{Timeout: 3 * time.Second}).Do(req)
	if err != nil {
		return g, fmt.Errorf("no live hub on 127.0.0.1:%d", port)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return g, fmt.Errorf("hub on 127.0.0.1:%d has no gate (an older bashy): %s", port, resp.Status)
	}
	return g, json.NewDecoder(resp.Body).Decode(&g)
}

// SetGate pauses or resumes the hub on port; by names the controller.
func SetGate(ctx context.Context, port int, paused bool, by string) (GateState, error) {
	var g GateState
	tok, err := ControlToken()
	if err != nil {
		return g, err
	}
	body, _ := json.Marshal(map[string]any{"paused": paused, "by": by})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, fmt.Sprintf("http://127.0.0.1:%d/gate", port), strings.NewReader(string(body)))
	if err != nil {
		return g, err
	}
	req.Header.Set(GateTokenHeader, tok)
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 3 * time.Second}).Do(req)
	if err != nil {
		return g, fmt.Errorf("no live hub on 127.0.0.1:%d", port)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return g, errors.New("hub refused the gate change: " + resp.Status)
	}
	return g, json.NewDecoder(resp.Body).Decode(&g)
}

// WriteGate records the gate for the hub on port without a running hub, so
// a later hub honours a pause given while it was down.
func WriteGate(port int, paused bool, by string) (GateState, error) {
	g := GateState{Paused: paused, By: by, At: time.Now().UTC()}
	return g, saveGate(port, g)
}

// ReadGate is the recorded gate for port (what a hub would start with).
func ReadGate(port int) GateState { return loadGate(port) }
