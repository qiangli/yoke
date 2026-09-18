package live

import (
	"encoding/json"

	"github.com/qiangli/yoke/pkg/browser/wire"
)

// wsRequest is the wire envelope hub → extension. The id is generated
// server-side; the extension echoes it back so callers can correlate
// concurrent calls.
type wsRequest struct {
	ID     int64          `json:"id"`
	Method string         `json:"method"`
	Params map[string]any `json:"params,omitempty"`
}

// wsResponse is the wire envelope extension → hub. Exactly one of
// Result and Error is non-empty. id == 0 is reserved for unsolicited
// frames (currently just `_hello`).
type wsResponse struct {
	ID     int64           `json:"id"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  string          `json:"error,omitempty"`
	Method string          `json:"method,omitempty"`
}

// extResult is the inner content the extension returns for tool calls.
// Fields mirror wire.Result so we can decode directly.
type extResult struct {
	Title     string `json:"title,omitempty"`
	URL       string `json:"url,omitempty"`
	Content   string `json:"content,omitempty"`
	Elements  string `json:"elements,omitempty"`
	Data      string `json:"data,omitempty"`
	Image     string `json:"image,omitempty"`
	Path      string `json:"path,omitempty"`
	Total     int    `json:"total,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`

	// TabID is the tab the extension actually acted on — resolved
	// through its single targetTabId() source of truth, not inferred.
	TabID int `json:"tab_id,omitempty"`

	// Viewport is reported by extract so a caller can tell "the
	// element is absent" from "a responsive breakpoint hid it"
	// without an eval that page CSP may forbid.
	Viewport *wire.Viewport `json:"viewport,omitempty"`
}

// extHello is the unsolicited frame the extension sends as soon as it
// opens the WebSocket. Lets the hub diagnose version drift and know
// which dispatch methods are available without a probe round-trip.
type extHello struct {
	Version     string   `json:"version"`
	Methods     []string `json:"methods,omitempty"`
	Permissions []string `json:"permissions,omitempty"`
}
