// Package live is bashy's "live" browser mode — an MV3 Chrome
// extension paired with a Go WebSocket hub, used to drive the user's
// real, logged-in Chrome (cookies, SSO, fingerprint).
//
// The hub (server.go) binds 127.0.0.1:<port> (default 58082) and waits
// for the extension to connect. Once connected, every wire.Action is
// translated into a JSON request, sent over WebSocket, and the response
// is unmarshaled back into a wire.Result.
//
// The extension source lives under ./extension/ and is bundled into the
// binary via go:embed. `bashy browser setup live` extracts the files so
// the user can load them via chrome://extensions → Developer mode →
// Load unpacked.
//
// Migrated from ycode's internal/runtime/mcpservers/live (Apache-2.0);
// only the action↔wire mapping in this file was adapted.
package live

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/qiangli/yoke/pkg/browser/wire"
)

// DefaultPort is the well-known loopback port the live extension
// connects to.
const DefaultPort = 58082

// LiveExtensionMinVersion is the minimum acceptable extension version.
// The hub refuses to dispatch to any older version with an actionable
// "reload at chrome://extensions" error. Reported by the extension's
// `_hello` frame.
// 0.7.0 is a HARD floor, not a nicety: before it, `screenshot`
// captured the window's foreground tab rather than the tab every other
// method drives, so a stale extension hands back a well-formed PNG of
// a page the caller never asked for. No Go-side change can detect that
// from the outside — the image is real, just of the wrong page — so
// the version gate is the only place it can be caught.
// Deliberately NOT raised to 0.7.2. The floor exists to catch SILENT
// wrongness — a stale extension handing back a real PNG of the wrong page.
// 0.7.1 does not do that: its background capture fails LOUDLY with a timeout,
// which is a bad experience but an honest one, and forcing every agent
// mid-task through another reload to fix a loud failure is the worse trade.
const LiveExtensionMinVersion = "0.7.1"

// LiveHandshakeTimeout caps how long hub.call waits for the extension
// to send its _hello frame before treating the connection as too old.
const LiveHandshakeTimeout = 3 * time.Second

// roleKind selects how a Service routes actions: it either owns the hub
// locally (roleHub) or forwards every call to a hub already running in
// another process (roleClient).
type roleKind int

const (
	roleUnset  roleKind = iota
	roleHub             // this process binds 127.0.0.1:<port> and owns the WS
	roleClient          // another process owns the hub; we POST /dispatch
)

// Service is the live-mode backend. It satisfies browser.Client.
type Service struct {
	port int

	mu   sync.Mutex
	role roleKind
	hub  *hub         // populated when role == roleHub
	http *http.Client // populated when role == roleClient
}

// New returns a live-mode service.
func New(port int) *Service {
	if port == 0 {
		port = DefaultPort
	}
	return &Service{port: port}
}

// NewClient returns a ready live Service as a browser.Client. It picks
// the hub/client role automatically (see EnsureReady).
func NewClient(ctx context.Context, port int) (*Service, error) {
	s := New(port)
	if err := s.EnsureReady(ctx); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Service) Port() int { return s.port }

// EnsureReady picks a role based on whether the live port is in use:
//
//   - port free → bind the hub locally
//   - port in use by a live hub → switch to client role and forward
//     every Execute via HTTP POST /dispatch
func (s *Service) EnsureReady(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.role != roleUnset {
		return nil
	}
	if portInUse(s.port) {
		if probeHealth(s.port) {
			s.role = roleClient
			s.http = &http.Client{Timeout: 35 * time.Second}
			logInfo("live: hub already owned by another process; using client role", "port", s.port)
			return nil
		}
	}
	h := newHub(s.port)
	if err := h.start(ctx); err != nil {
		return err
	}
	s.role = roleHub
	s.hub = h
	return nil
}

// Close stops the hub (hub role) or drops the client (client role).
func (s *Service) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch s.role {
	case roleHub:
		err := s.hub.stop(context.Background())
		s.hub = nil
		s.role = roleUnset
		return err
	case roleClient:
		s.http = nil
		s.role = roleUnset
	}
	return nil
}

// RunHub binds the hub and blocks until ctx is cancelled. Used by the
// long-running `bashy browser hub` command so the extension has a stable
// endpoint to connect to across many client dispatches.
func (s *Service) RunHub(ctx context.Context) error {
	if err := s.EnsureReady(ctx); err != nil {
		return err
	}
	<-ctx.Done()
	return s.Close()
}

// Connected reports whether the extension is currently attached.
func (s *Service) Connected() bool {
	s.mu.Lock()
	role := s.role
	h := s.hub
	s.mu.Unlock()
	switch role {
	case roleHub:
		return h != nil && h.connected()
	case roleClient:
		return probeHealth(s.port)
	}
	return false
}

func portInUse(port int) bool {
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return true
	}
	_ = ln.Close()
	return false
}

func probeHealth(port int) bool {
	c := &http.Client{Timeout: 500 * time.Millisecond}
	resp, err := c.Get(fmt.Sprintf("http://127.0.0.1:%d/health", port))
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// Execute satisfies browser.Client.
func (s *Service) Execute(ctx context.Context, action wire.Action) (*wire.Result, error) {
	s.mu.Lock()
	role := s.role
	h := s.hub
	client := s.http
	s.mu.Unlock()

	method, params, err := actionToParams(action)
	if err != nil {
		return &wire.Result{Error: err.Error()}, nil
	}

	// Hard staleness + per-method gate, only meaningful in roleHub (in
	// roleClient the hub-owning process runs the same checks). Allow
	// `capabilities` through so doctor can introspect a stale extension.
	if role == roleHub && h != nil && action.Type != wire.ActionCapabilities {
		if ver := h.ExtVersion(); ver == "" {
			// Conn up but no _hello yet — awaitHello surfaces the timeout.
		} else if versionLess(ver, LiveExtensionMinVersion) {
			return &wire.Result{Error: staleExtensionError(ver)}, nil
		} else if methods := h.ExtMethods(); len(methods) > 0 && !slices.Contains(methods, method) {
			return &wire.Result{Error: methodNotAdvertisedError(method, ver)}, nil
		}
	}

	// wait-for-selector callers pass timeout_ms; respect it as the outer
	// deadline (plus a buffer) so the call doesn't time out early.
	timeout := 30 * time.Second
	if action.TimeoutMs > 0 {
		t := time.Duration(action.TimeoutMs)*time.Millisecond + 5*time.Second
		if t > timeout {
			timeout = t
		}
	}
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var res *wire.Result
	switch role {
	case roleHub:
		res, err = s.executeHub(callCtx, h, method, params)
	case roleClient:
		res, err = s.executeClient(callCtx, client, method, params)
	default:
		return nil, errors.New("live: not ready (call EnsureReady first)")
	}
	if res != nil && h != nil && res.URL != "" {
		h.RecordLastTab(res.URL)
	}
	if res != nil && res.Success && action.Type == wire.ActionScreenshot {
		if perr := finishScreenshot(res, action); perr != nil {
			return &wire.Result{Error: perr.Error()}, nil
		}
	}
	return res, err
}

// finishScreenshot applies SavePath / MaxBytes to a live capture.
//
// The extension deliberately always returns raw base64 and documents
// that "the Go side is responsible for MaxBytes / SavePath" — but live
// mode never actually did it, so `screenshot /tmp/x.png` wrote no
// file, exited 0, and dumped 223 KB of base64 to stdout. probe/solo
// have always honoured both (internal/cdpactions.Screenshot); this
// closes the gap in the one mode that skipped it.
func finishScreenshot(res *wire.Result, a wire.Action) error {
	if res.Image == "" {
		return nil
	}
	needFile := a.SavePath != ""
	if !needFile && a.MaxBytes > 0 && len(res.Image) > a.MaxBytes {
		needFile = true
	}
	if !needFile {
		return nil
	}
	raw, err := base64.StdEncoding.DecodeString(res.Image)
	if err != nil {
		return fmt.Errorf("live: screenshot: decode image: %w", err)
	}
	path := a.SavePath
	if path == "" {
		f, err := os.CreateTemp("", "bashy-browser-*.png")
		if err != nil {
			return err
		}
		path = f.Name()
		if _, err := f.Write(raw); err != nil {
			_ = f.Close()
			return err
		}
		if err := f.Close(); err != nil {
			return err
		}
	} else {
		abs, err := filepath.Abs(path)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(abs, raw, 0o644); err != nil {
			return err
		}
		path = abs
	}
	res.Image = ""
	res.Path = path
	return nil
}

func staleExtensionError(ver string) string {
	return fmt.Sprintf("live: extension stale (v%s < required v%s). "+
		"Reload it at chrome://extensions, or run: bashy browser setup live",
		ver, LiveExtensionMinVersion)
}

func methodNotAdvertisedError(method, ver string) string {
	return fmt.Sprintf("live: method %q not advertised by extension v%s. "+
		"Reload it at chrome://extensions, or run: bashy browser setup live",
		method, ver)
}

func (s *Service) executeHub(ctx context.Context, h *hub, method string, params map[string]any) (*wire.Result, error) {
	resp, err := h.call(ctx, method, params)
	if err != nil {
		return &wire.Result{Error: err.Error()}, nil
	}
	if resp.Error != "" {
		return &wire.Result{Error: resp.Error}, nil
	}
	return unmarshalExt(resp.Result)
}

func (s *Service) executeClient(ctx context.Context, c *http.Client, method string, params map[string]any) (*wire.Result, error) {
	body, err := json.Marshal(map[string]any{"method": method, "params": params})
	if err != nil {
		return &wire.Result{Error: err.Error()}, nil
	}
	url := fmt.Sprintf("http://127.0.0.1:%d/dispatch", s.port)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return &wire.Result{Error: err.Error()}, nil
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.Do(req)
	if err != nil {
		return &wire.Result{Error: fmt.Sprintf("live: dispatch to hub: %v", err)}, nil
	}
	defer resp.Body.Close()
	rawBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return &wire.Result{Error: fmt.Sprintf("live: hub returned %d: %s", resp.StatusCode, string(rawBody))}, nil
	}
	var dispatchResp struct {
		Result json.RawMessage `json:"result"`
		Error  string          `json:"error"`
	}
	if err := json.Unmarshal(rawBody, &dispatchResp); err != nil {
		return &wire.Result{Error: fmt.Sprintf("live: bad dispatch payload: %v", err)}, nil
	}
	if dispatchResp.Error != "" {
		return &wire.Result{Error: dispatchResp.Error}, nil
	}
	return unmarshalExt(dispatchResp.Result)
}

func unmarshalExt(raw json.RawMessage) (*wire.Result, error) {
	var inner extResult
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &inner); err != nil {
			return &wire.Result{Error: fmt.Sprintf("live: bad result payload: %v", err)}, nil
		}
	}
	return &wire.Result{
		Success:   true,
		Title:     inner.Title,
		URL:       inner.URL,
		Content:   inner.Content,
		Elements:  inner.Elements,
		Data:      inner.Data,
		Image:     inner.Image,
		Path:      inner.Path,
		Total:     inner.Total,
		Truncated: inner.Truncated,
		TabID:     inner.TabID,
		Viewport:  inner.Viewport,
	}, nil
}

// versionLess returns true when a < b using a dotted-numeric compare.
func versionLess(a, b string) bool {
	as := strings.Split(a, ".")
	bs := strings.Split(b, ".")
	for i := 0; i < len(as) || i < len(bs); i++ {
		var an, bn int
		if i < len(as) {
			an, _ = strconv.Atoi(as[i])
		}
		if i < len(bs) {
			bn, _ = strconv.Atoi(bs[i])
		}
		if an < bn {
			return true
		}
		if an > bn {
			return false
		}
	}
	return false
}

// actionToParams translates a wire.Action into a {method, params} pair
// for the WebSocket protocol. Keep in sync with the extension's
// background.js dispatch table.
func actionToParams(a wire.Action) (string, map[string]any, error) {
	switch a.Type {
	case wire.ActionNavigate:
		return "navigate", map[string]any{"url": a.URL}, nil
	case wire.ActionClick:
		return "click", map[string]any{
			"selector":       a.Selector,
			"element_id":     a.ElementID,
			"match_text":     a.MatchText,
			"scope":          a.Scope,
			"include_hidden": a.IncludeHidden,
		}, nil
	case wire.ActionType:
		return "type", map[string]any{"selector": a.Selector, "element_id": a.ElementID, "text": a.Text}, nil
	case wire.ActionScroll:
		return "scroll", map[string]any{"direction": a.Direction, "amount": a.Amount}, nil
	case wire.ActionScreenshot:
		return "screenshot", map[string]any{
			"full_page": a.FullPage,
			"settle_ms": a.SettleMs,
		}, nil
	case wire.ActionExtract:
		return "extract", map[string]any{
			"goal":           a.Goal,
			"match_text":     a.MatchText,
			"scope":          a.Scope,
			"limit":          a.Limit,
			"offset":         a.Offset,
			"include_hidden": a.IncludeHidden,
		}, nil
	case wire.ActionBack:
		return "back", map[string]any{}, nil
	case wire.ActionTabs:
		return "tabs", map[string]any{
			"action":      a.TabAction,
			"tab_id":      a.TabID,
			"by_id":       a.State == "id",
			"url":         a.URL,
			"match_url":   a.MatchURL,
			"match_title": a.MatchTitle,
		}, nil
	case wire.ActionEvaluate:
		return "evaluate", map[string]any{"script": a.Script}, nil
	case wire.ActionWaitForSelector:
		return "wait_for_selector", map[string]any{
			"selector":   a.Selector,
			"timeout_ms": a.TimeoutMs,
			"state":      a.State,
		}, nil
	case wire.ActionKeyboardPress:
		return "keyboard_press", map[string]any{
			"key":       a.Key,
			"modifiers": a.Modifiers,
			"selector":  a.Selector,
		}, nil
	case wire.ActionClipboardRead:
		return "clipboard_read", map[string]any{}, nil
	case wire.ActionClipboardWrite:
		return "clipboard_write", map[string]any{"text": a.Text}, nil
	case wire.ActionCookiesGet:
		return "cookies_get", map[string]any{"name": a.Name, "domain": a.Domain}, nil
	case wire.ActionStorageGet:
		return "storage_get", map[string]any{"storage": a.Storage, "key": a.StorageKey}, nil
	case wire.ActionCapabilities:
		return "capabilities", map[string]any{}, nil
	case wire.ActionDispatchEvent:
		return "dispatch_event", map[string]any{
			"event":    a.Event,
			"detail":   a.Detail,
			"selector": a.Selector,
		}, nil
	case wire.ActionNetworkList:
		return "network_list", map[string]any{}, nil
	case wire.ActionConsoleGet:
		return "console_get", map[string]any{}, nil
	case wire.ActionPerfStart:
		return "perf_start", map[string]any{}, nil
	case wire.ActionPerfStop:
		return "perf_stop", map[string]any{}, nil
	case wire.ActionLighthouse:
		return "lighthouse", map[string]any{}, nil
	}
	return "", nil, fmt.Errorf("live: action %q not supported", a.Type)
}

// Status is what `browser status --mode live` reports. Every field is
// observed, not defaulted: `status` previously answered
// `{"mode":"live","reachable":false,"message":"mode \"live\" is not
// supported"}` while every sibling subcommand drove 51 tabs happily —
// a refusal that misroutes the caller to a weaker channel is worse
// than no status at all.
type Status struct {
	Mode      string `json:"mode"`
	HubPort   int    `json:"hub_port"`
	HubUp     bool   `json:"hub_up"`
	Reachable bool   `json:"reachable"`

	Connected     bool   `json:"extension_connected"`
	ExtVersion    string `json:"extension_version,omitempty"`
	MinVersion    string `json:"extension_min_version"`
	Stale         bool   `json:"extension_stale"`
	MethodsCount  int    `json:"extension_methods,omitempty"`
	Message       string `json:"message,omitempty"`
	LastTabURL    string `json:"last_tab_url,omitempty"`
	OwnedInThisPS bool   `json:"hub_owned_by_this_process"`
}

// LiveStatus probes the hub on port without binding one. It never
// starts a hub as a side effect — asking a question must not change
// the answer.
func LiveStatus(ctx context.Context, port int) Status {
	if port == 0 {
		port = DefaultPort
	}
	st := Status{Mode: "live", HubPort: port, MinVersion: LiveExtensionMinVersion}
	st.HubUp = probeHealth(port)
	if !st.HubUp {
		st.Message = fmt.Sprintf("no live hub on 127.0.0.1:%d — start one with: bashy browser hub", port)
		return st
	}
	c := &http.Client{Timeout: 2 * time.Second}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/connected", port), nil)
	if err != nil {
		st.Message = err.Error()
		return st
	}
	resp, err := c.Do(req)
	if err != nil {
		st.Message = fmt.Sprintf("hub on 127.0.0.1:%d did not answer /connected: %v", port, err)
		return st
	}
	defer resp.Body.Close()
	var payload struct {
		Connected    bool   `json:"connected"`
		Version      string `json:"version"`
		MethodsCount int    `json:"methods_count"`
		Stale        bool   `json:"stale"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		st.Message = fmt.Sprintf("hub on 127.0.0.1:%d returned an unreadable /connected payload: %v", port, err)
		return st
	}
	st.Connected = payload.Connected
	st.ExtVersion = payload.Version
	st.MethodsCount = payload.MethodsCount
	st.Stale = payload.Stale
	// "Reachable" means an action issued right now would reach a page.
	st.Reachable = st.Connected && !st.Stale
	switch {
	case !st.Connected:
		st.Message = "hub is up but no extension is attached — open the extension popup on your target tab and click Connect (first-time setup: bashy browser setup live)"
	case st.Stale:
		st.Message = staleExtensionError(st.ExtVersion)
	}
	return st
}
