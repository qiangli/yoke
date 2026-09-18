// Package wire is the build-tag-free transport schema for browser
// automation. Both the tool shims (stable build) and the backend
// services (experimental build) share these types — no duplication,
// no hand-copy.
package wire

import (
	"encoding/json"
	"strings"
	"unicode"
)

// Action is the unified browser action. Each backend translates it
// into the right native primitive (CDP command for probe/solo,
// WebSocket message for live).
type Action struct {
	Type      string   `json:"action"`
	URL       string   `json:"url,omitempty"`
	ElementID int      `json:"element_id,omitempty"`
	Selector  string   `json:"selector,omitempty"`
	Text      string   `json:"text,omitempty"`
	Direction string   `json:"direction,omitempty"`
	Amount    int      `json:"amount,omitempty"`
	Goal      string   `json:"goal,omitempty"`
	TabID     int      `json:"tab_id,omitempty"`
	TabAction string   `json:"tab_action,omitempty"`
	Script    string   `json:"script,omitempty"`
	URLs      []string `json:"urls,omitempty"`

	// Extract: optional CSS scope to constrain the query root, and a
	// MatchText to filter elements by visible text. Goal is the
	// natural-language form already in use; MatchText is the
	// machine-friendly exact substring used by the click-by-text
	// strategy. Limit/Offset control pagination (default 50/0).
	Scope     string `json:"scope,omitempty"`
	MatchText string `json:"match_text,omitempty"`
	Limit     int    `json:"limit,omitempty"`
	Offset    int    `json:"offset,omitempty"`

	// Screenshot: opt-in size cap and save-to-file path. When MaxBytes
	// > 0 the backend tries to shrink the inline base64 PNG (JPEG
	// re-encode at decreasing qualities); if it cannot fit or
	// SavePath is set, the image is written to disk and the path is
	// returned in Result.Path instead of Result.Image.
	MaxBytes int    `json:"max_bytes,omitempty"`
	SavePath string `json:"save_path,omitempty"`

	// FullPage captures beyond the viewport (live mode routes this
	// through CDP; probe/solo use chromedp's full-page capture).
	FullPage bool `json:"full_page,omitempty"`

	// SettleMs is how long a backend waits for the renderer to paint
	// before capturing. A screenshot taken immediately after a click
	// can otherwise return the pre-click frame while the DOM already
	// reports the post-click state. Zero means "backend default".
	SettleMs int `json:"settle_ms,omitempty"`

	// IncludeHidden keeps elements that are present but not displayed
	// in an extract, so "absent" and "not visible" stay separable.
	IncludeHidden bool `json:"include_hidden,omitempty"`

	// MatchURL / MatchTitle address a tab by substring instead of by
	// index. An index captured at the start of a script is stale by
	// the end whenever a tab opens or closes, so scripts should
	// prefer these. An ambiguous match is an error, never a guess.
	MatchURL   string `json:"match_url,omitempty"`
	MatchTitle string `json:"match_title,omitempty"`

	// DispatchEvent: Event is the event name, Detail an optional JSON
	// document used as CustomEvent detail, and Target one of
	// "window" (default) / "document" / a CSS selector via Selector.
	Event  string `json:"event,omitempty"`
	Detail string `json:"detail,omitempty"`

	// Wait/timeout. TimeoutMs applies to wait_for_selector (default
	// 5000ms) and is honoured by future polled actions. State is one
	// of "visible", "attached", "detached" (default "visible").
	TimeoutMs int    `json:"timeout_ms,omitempty"`
	State     string `json:"state,omitempty"`

	// Keyboard: Key is a DOM-event key name ("Enter", "Tab", "Escape",
	// "a"), Modifiers is any subset of {"Shift","Control","Alt","Meta"}.
	Key       string   `json:"key,omitempty"`
	Modifiers []string `json:"modifiers,omitempty"`

	// Cookies. Name + Domain filter the returned set (both optional).
	Name   string `json:"name,omitempty"`
	Domain string `json:"domain,omitempty"`

	// Storage: kind is "local" or "session"; StorageKey filters to a
	// single entry, empty returns the full key/value dump.
	Storage    string `json:"storage,omitempty"`
	StorageKey string `json:"storage_key,omitempty"`
}

// UnmarshalJSON accepts `expression` as an alias for `script` on
// Evaluate actions. Chrome DevTools-flavored callers reach for
// `expression` (matching CDP's Runtime.evaluate), while the rest of
// ycode uses `script`. Only one is canonical (`script`); the alias
// folds into the canonical field iff the canonical one is empty.
func (a *Action) UnmarshalJSON(data []byte) error {
	type rawAction Action
	var raw rawAction
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	*a = Action(raw)
	if a.Script == "" {
		var aux struct {
			Expression string `json:"expression"`
		}
		if err := json.Unmarshal(data, &aux); err == nil {
			a.Script = aux.Expression
		}
	}
	return nil
}

// Result is the unified browser result.
//
// Data carries a tool-specific payload. Plain-text tools (clipboard
// text, "pressed=Enter", "tracing started", evaluate scalars) store
// human-readable strings here. Structured tools (cookies_get,
// capabilities, network_list, console_get, storage_get, lighthouse,
// perf_stop) store a pre-marshaled JSON object/array.
//
// The custom MarshalJSON below detects the structured case and emits
// it as a decoded JSON object/array in the agent-facing envelope —
// so agents read `data` as native JSON instead of a stringified
// string-of-JSON. Plain-text payloads serialize as JSON strings
// (unchanged from the legacy shape).
type Result struct {
	Success      bool     `json:"success"`
	Title        string   `json:"title,omitempty"`
	URL          string   `json:"url,omitempty"`
	Content      string   `json:"content,omitempty"`
	Elements     string   `json:"elements,omitempty"`
	Data         string   `json:"-"`
	Image        string   `json:"image,omitempty"`
	Error        string   `json:"error,omitempty"`
	Hints        []string `json:"hints,omitempty"`
	OutcomeClass string   `json:"outcome_class,omitempty"`

	// Path is set instead of Image when the screenshot exceeded
	// MaxBytes (or SavePath was requested) and the backend wrote it to
	// disk. Always absolute.
	Path string `json:"path,omitempty"`

	// Total + Truncated describe a paginated extract: Total is the
	// element count before the Limit cap, Truncated is true when the
	// returned set is a prefix.
	Total     int  `json:"total,omitempty"`
	Truncated bool `json:"truncated,omitempty"`

	// TabID is the browser tab the action actually ran against. Every
	// live-mode action resolves its target through one shared source
	// of truth and reports it here, so a caller can tell a capture of
	// the wrong page from a capture of the right one.
	TabID int `json:"tab_id,omitempty"`

	// Viewport is the target page's CSS-pixel viewport at the time of
	// the action. One integer settles "is this hidden by a responsive
	// breakpoint?", which is otherwise unanswerable when page CSP
	// blocks eval.
	Viewport *Viewport `json:"viewport,omitempty"`
}

// Viewport is the CSS-pixel size of the driven page plus its device
// pixel ratio.
type Viewport struct {
	Width  int     `json:"width"`
	Height int     `json:"height"`
	DPR    float64 `json:"dpr,omitempty"`
}

// MarshalJSON serializes Result with `data` rendered as decoded JSON
// when Data holds a JSON object or array, and as a JSON string
// otherwise. Empty Data is omitted. See the Result doc comment for
// the rationale.
func (r Result) MarshalJSON() ([]byte, error) {
	type alias Result
	base := (*alias)(&r)
	if r.Data == "" {
		return json.Marshal(struct{ *alias }{base})
	}
	var dataField json.RawMessage
	if looksLikeStructuredJSON(r.Data) {
		dataField = json.RawMessage(r.Data)
	} else {
		b, err := json.Marshal(r.Data)
		if err != nil {
			return nil, err
		}
		dataField = b
	}
	return json.Marshal(struct {
		*alias
		Data json.RawMessage `json:"data,omitempty"`
	}{base, dataField})
}

// looksLikeStructuredJSON returns true when s parses as a JSON object
// or array. Bare JSON scalars (numbers, "quoted", true/false/null)
// are intentionally excluded — plain text like "tracing started" must
// continue to round-trip as a JSON string.
func looksLikeStructuredJSON(s string) bool {
	trimmed := strings.TrimLeftFunc(s, unicode.IsSpace)
	if trimmed == "" {
		return false
	}
	if trimmed[0] != '{' && trimmed[0] != '[' {
		return false
	}
	return json.Valid([]byte(s))
}

// Action types recognized across modes. Backends that do not
// implement a given action return a clear "unsupported" error.
const (
	ActionNavigate   = "navigate"
	ActionClick      = "click"
	ActionType       = "type"
	ActionScroll     = "scroll"
	ActionScreenshot = "screenshot"
	ActionExtract    = "extract"
	ActionBack       = "back"
	ActionTabs       = "tabs"

	// DevTools-flavored. Evaluate is supported in all three modes
	// (live uses chrome.scripting.executeScript with world:"MAIN");
	// the perf/network/console/lighthouse actions require CDP and
	// stay probe+solo-only.
	ActionEvaluate    = "evaluate"
	ActionPerfStart   = "perf_start"
	ActionPerfStop    = "perf_stop"
	ActionNetworkList = "network_list"
	ActionConsoleGet  = "console_get"
	ActionLighthouse  = "lighthouse"

	// Robustness additions. WaitForSelector and KeyboardPress are
	// no-permission primitives that work on every mode. Clipboard /
	// Cookies / Storage require chrome.* permissions in the live
	// extension (manifest 0.3.0); probe+solo reach them via
	// CDP/evaluate. Capabilities is a session-startup probe so
	// foreign agents can discover what the connected extension
	// supports instead of guessing.
	ActionWaitForSelector = "wait_for_selector"
	ActionKeyboardPress   = "keyboard_press"
	ActionClipboardRead   = "clipboard_read"
	ActionClipboardWrite  = "clipboard_write"
	ActionCookiesGet      = "cookies_get"
	ActionStorageGet      = "storage_get"
	ActionCapabilities    = "capabilities"

	// ActionDispatchEvent fires a named DOM event from the extension's
	// isolated world (live) or via CDP (probe/solo). It exists because
	// a page whose Content-Security-Policy omits 'unsafe-eval' — the
	// increasingly common default — disables ActionEvaluate entirely,
	// and "click whichever element happens to share the handler" is
	// not always available as a workaround.
	ActionDispatchEvent = "dispatch_event"
)
