package live

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/gorilla/websocket"
)

// The popup's Shape button is the channel in the other direction: the human
// sends the tab they are looking at, with an optional note, to an agent.
//
// The extension sends one unsolicited frame, {"id":0,"method":"shape",
// "result":{nonce,tab_id,url,title,note,to}}, and the hub answers with
// {"id":0,"method":"_shape_result","params":{nonce,ok,status|error,…}} —
// a method every older extension ignores, as none of them sends "shape".
// The hub owns the parts the extension cannot be trusted with or does not
// know (which browser this hub serves, the size cap); delivery itself is the
// handler's, so this package stays free of the chat and meet packages.

// shapeMethod and shapeResultMethod name the two frames.
const (
	shapeMethod       = "shape"
	shapeResultMethod = "_shape_result"
)

// ShapeNoteMax caps the note, in UTF-8 bytes. A longer note is cut on a rune
// boundary and the human is told; it is never dropped or split.
const ShapeNoteMax = 8 * 1024

// Caps for the fields the PAGE supplies (a tab's title and URL are whatever
// the site chose) and for the recipient the human typed.
const (
	shapeURLMax   = 2048
	shapeTitleMax = 300
	shapeToMax    = 128
)

// ShapeDeliverTimeout bounds one delivery. A variable only so tests can
// shorten it.
var ShapeDeliverTimeout = 20 * time.Second

// ShapeMessage is one press of the Shape button.
type ShapeMessage struct {
	// Browser is the browser THIS hub serves (brave, chrome, edge, safari), by
	// its port; Port is that port, for a hub on a non-default one.
	Browser string
	Port    int

	TabID int
	URL   string
	Title string

	// Note is the human's text, verbatim: untrusted data to carry, never
	// markup to render. Truncated reports it was cut at ShapeNoteMax.
	Note      string
	Truncated bool

	// To is the recipient the human typed; empty means the default.
	To string
	// Chat is the exact Bashy chat id bound to the tab by the agent driving
	// it. When set it is the only recipient: never a default, never a DM.
	Chat string
	At   time.Time
}

// ShapeReceipt is what the popup's status line says after a delivery.
type ShapeReceipt struct {
	Via    string // how it travelled: steer, dm or inbox
	To     string // who it reached
	Status string // one line for the human
}

// ErrShapeRouteStale is what a handler returns when the chat a bound tab
// names is gone: nothing was sent, and the popup says the route is stale.
var ErrShapeRouteStale = errors.New("the agent chat bound to this tab is gone; nothing was sent")

// ShapeHandler delivers a Shape message.
type ShapeHandler func(ctx context.Context, m ShapeMessage) (ShapeReceipt, error)

var shapeHandler atomic.Pointer[ShapeHandler]

// SetShapeHandler installs the delivery for Shape messages; nil removes it.
// With none installed a press is answered with an error rather than dropped.
func SetShapeHandler(fn ShapeHandler) {
	if fn == nil {
		shapeHandler.Store(nil)
		return
	}
	shapeHandler.Store(&fn)
}

// BrowserName is the browser a hub port serves by the port rule (see
// BrowserPort), or "" for a port that is no browser's default.
func BrowserName(port int) string {
	for _, n := range []string{"brave", "chrome", "edge", "safari"} {
		if BrowserPort(n) == port {
			return n
		}
	}
	return ""
}

// extShape is the payload of the extension's `shape` frame.
type extShape struct {
	// Nonce correlates the hub's answer with the press that asked.
	Nonce string `json:"nonce"`
	TabID int    `json:"tab_id"`
	URL   string `json:"url,omitempty"`
	Title string `json:"title,omitempty"`
	Note  string `json:"note,omitempty"`
	To    string `json:"to,omitempty"`
	Chat  string `json:"chat,omitempty"`
	// Browser is the extension's own guess, used only when the hub's port
	// names no browser.
	Browser string `json:"browser,omitempty"`
}

// newShapeMessage builds the message a handler sees from what the extension
// sent: the hub's browser, the note capped, the page-supplied fields cleaned.
func newShapeMessage(port int, in extShape) ShapeMessage {
	m := ShapeMessage{
		Browser: BrowserName(port),
		Port:    port,
		TabID:   in.TabID,
		URL:     clipText(oneLine(in.URL), shapeURLMax),
		Title:   clipText(oneLine(in.Title), shapeTitleMax),
		To:      clipText(oneLine(strings.TrimPrefix(strings.TrimSpace(in.To), "@")), shapeToMax),
		Chat:    clipText(oneLine(in.Chat), shapeToMax),
		At:      time.Now().UTC(),
	}
	if m.Browser == "" {
		if hint := strings.ToLower(strings.TrimSpace(in.Browser)); BrowserPort(hint) != 0 {
			m.Browser = hint
		}
	}
	m.Note, m.Truncated = capNote(in.Note)
	return m
}

// capNote returns the note as valid UTF-8 within ShapeNoteMax bytes, cut on a
// rune boundary, and whether anything was cut. Nothing else is changed: line
// breaks, spacing and markup-looking text are the human's.
func capNote(note string) (string, bool) {
	note = strings.ToValidUTF8(note, "�")
	if len(note) <= ShapeNoteMax {
		return note, false
	}
	cut := ShapeNoteMax
	for cut > 0 && !utf8.RuneStart(note[cut]) {
		cut--
	}
	return note[:cut], true
}

// oneLine makes a page-supplied field safe to carry on one line: valid UTF-8,
// control characters (a title may hold newlines or escapes) become spaces.
func oneLine(s string) string {
	s = strings.ToValidUTF8(s, "�")
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
	return strings.TrimSpace(s)
}

// clipText shortens s to at most max bytes on a rune boundary, marking a cut.
func clipText(s string, max int) string {
	if len(s) <= max {
		return s
	}
	const mark = "…"
	cut := max - len(mark)
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + mark
}

// extensionOrigin reports whether a WebSocket handshake came from a browser
// extension. The hub's socket accepts any origin, so a web page can dial it;
// a page must not be able to put words in the operator's mouth, and a page
// cannot forge the Origin header its browser sends.
func extensionOrigin(origin string) bool {
	u, err := url.Parse(strings.TrimSpace(origin))
	if err != nil || u.Host == "" {
		return false
	}
	switch u.Scheme {
	case "chrome-extension", "moz-extension", "safari-web-extension":
		return true
	}
	return false
}

// handleShape answers one `shape` frame. It runs off the read loop: a
// delivery may wait on a session, and the loop must keep routing replies.
func (h *hub) handleShape(conn *websocket.Conn, origin string, raw json.RawMessage) {
	var in extShape
	if err := json.Unmarshal(raw, &in); err != nil {
		logWarn("live: bad shape payload", "error", err)
		return
	}
	reply := func(params map[string]any) {
		params["nonce"] = in.Nonce
		frame, err := json.Marshal(wsRequest{Method: shapeResultMethod, Params: params})
		if err != nil {
			return
		}
		if err := h.write(conn, frame); err != nil {
			logWarn("live: shape reply", "error", err)
		}
	}
	fail := func(msg string) {
		logInfo("live: shape not delivered", "reason", msg)
		reply(map[string]any{"ok": false, "error": msg})
	}

	if !extensionOrigin(origin) {
		fail("Shape is accepted only from the bashy extension")
		return
	}
	if in.TabID <= 0 {
		fail("no current tab to send")
		return
	}
	fn := shapeHandler.Load()
	if fn == nil {
		fail("this bashy hub cannot deliver a Shape message")
		return
	}
	m := newShapeMessage(h.port, in)

	// One delivery at a time, so two quick presses arrive in the order pressed.
	h.shapeMu.Lock()
	defer h.shapeMu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), ShapeDeliverTimeout)
	defer cancel()
	rec, err := (*fn)(ctx, m)
	if errors.Is(err, ErrShapeRouteStale) {
		logInfo("live: shape not delivered", "reason", err.Error())
		reply(map[string]any{"ok": false, "error": err.Error(), "stale": true})
		return
	}
	if err != nil {
		fail(err.Error())
		return
	}
	status := rec.Status
	if m.Truncated {
		status += fmt.Sprintf(" (note cut to %d KB: only the first %d bytes were sent)", ShapeNoteMax/1024, len(m.Note))
	}
	logInfo("live: shape delivered", "via", rec.Via, "to", rec.To, "tab", m.TabID, "note_bytes", len(m.Note), "truncated", m.Truncated)
	reply(map[string]any{
		"ok": true, "status": status, "via": rec.Via, "to": rec.To,
		"truncated": m.Truncated, "note_bytes": len(m.Note),
	})
}
