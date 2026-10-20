// Package attach streams a dagrun session over a WebSocket.
//
// Wire protocol.
//
//	GET .../attach?client=<id>&since=<offset>[&viewer=1] upgrades.
//	Server: Hub.Attach(client, since, now, opts) with takeover the
//	default for non-viewers; viewers pass dagrun.ReadOnly() and never
//	take over. Attaching to an exited or lost session replays the log
//	from since without subscribing.
//
//	Server -> client binary frames carry output bytes in log order.
//	Server -> client text frames are JSON control frames:
//	  {"type":"offset","offset":N}  sent once after the replay, so the
//	    client knows the byte offset it can resume from.
//	  {"type":"exit","exit_code":N} the session exited; terminal.
//	  {"type":"evicted"}            a takeover moved this connection's
//	    holder aside; only the previous holder's connection closes.
//	  {"type":"lost"}               the executor is gone; terminal and
//	    distinct from exit (no exit code).
//	  {"type":"error","error":"msg"} a client frame was rejected (for
//	    example input from a viewer, or input with no PTY).
//	Exit and lost are terminal: the server sends the frame, then closes
//	with a normal close code. Eviction closes the previous holder only;
//	the new holder stays connected.
//
//	Client -> server binary frames are PTY input (viewers: ignored and
//	answered with an error frame, never forwarded). Client -> server
//	text frames are JSON:
//	  {"type":"resize","cols":C,"rows":R}  terminal geometry; the same
//	    shape webterm's sizeMsg speaks, so clients are shared. A "size"
//	    type is accepted as an alias.
//	  {"type":"signal","signal":"INT|TERM|HUP"}  deliver a named signal.
//
// Cross-origin upgrades are refused with the same coopauth Origin check
// webterm uses: an absent Origin (a non-browser client) is allowed; a
// present one must match the address the browser thinks it reaches.
package attach

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/qiangli/yoke/pkg/coopauth"
	"github.com/qiangli/yoke/pkg/dagrun"
)

// upgrader refuses cross-origin upgrades, like webterm: an absent Origin (a
// non-browser client) is allowed; a present one must match the external
// base the browser thinks it is talking to.
var upgrader = websocket.Upgrader{
	ReadBufferSize:  4096,
	WriteBufferSize: 4096,
	CheckOrigin: func(r *http.Request) bool {
		o := r.Header.Get("Origin")
		return o == "" || o == coopauth.ExternalBase(r)
	},
}

// Control frame types sent by the server.
const (
	frameOffset  = "offset"
	frameExit    = "exit"
	frameEvicted = "evicted"
	frameLost    = "lost"
	frameError   = "error"
)

// Control frame types accepted from the client.
const (
	frameResize = "resize"
	frameSize   = "size" // alias webterm-style clients send; same shape.
	frameSignal = "signal"
)

// serverControl is a server -> client JSON text frame.
type serverControl struct {
	Type     string `json:"type"`
	Offset   *int64 `json:"offset,omitempty"`
	ExitCode *int   `json:"exit_code,omitempty"`
	Error    string `json:"error,omitempty"`
}

// clientControl is a client -> server JSON text frame. The resize shape
// {"type":"resize","cols":C,"rows":R} is shared with webterm clients.
type clientControl struct {
	Type   string `json:"type"`
	Cols   uint16 `json:"cols,omitempty"`
	Rows   uint16 `json:"rows,omitempty"`
	Signal string `json:"signal,omitempty"`
}

// Handler serves session attach over WebSocket on any path: the session is
// named by the Hub, the client by ?client=. Query parameters are
// client=<id> (required), since=<byte offset> (default 0) and viewer=1 for
// a read-only attachment that never takes over.
func Handler(hub *dagrun.Hub) http.Handler {
	return &handler{hub: hub}
}

type handler struct {
	hub *dagrun.Hub
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	clientID := q.Get("client")
	if clientID == "" {
		http.Error(w, "attach: missing client id", http.StatusBadRequest)
		return
	}
	var since int64
	if raw := q.Get("since"); raw != "" {
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || n < 0 {
			http.Error(w, "attach: bad since offset", http.StatusBadRequest)
			return
		}
		since = n
	}
	viewer := q.Get("viewer") == "1"

	// Validate the offset before upgrading so range errors stay plain
	// HTTP errors instead of a torn-down socket.
	replay, end, err := h.hub.ReadFrom(since)
	if err != nil {
		if errors.Is(err, dagrun.ErrBadOffset) {
			http.Error(w, "attach: bad since offset", http.StatusBadRequest)
			return
		}
		http.Error(w, "attach: cannot replay log", http.StatusInternalServerError)
		return
	}

	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		// Upgrade already wrote the error response (403 on origin).
		return
	}

	if snap := h.hub.Session(); snap.State.Terminal() {
		serveTerminal(ws, replay, end, snap)
		return
	}

	var opts []dagrun.AttachOption
	if viewer {
		opts = append(opts, dagrun.ReadOnly())
	}
	att, err := h.hub.Attach(clientID, since, time.Now(), opts...)
	if err != nil {
		// The session exited between the check above and the attach:
		// fall back to the terminal replay instead of failing.
		if errors.Is(err, dagrun.ErrSessionClosed) {
			if snap := h.hub.Session(); snap.State.Terminal() {
				if replay, end, rerr := h.hub.ReadFrom(since); rerr == nil {
					serveTerminal(ws, replay, end, snap)
					return
				}
			}
		}
		writeControl(ws, &wsMu{}, serverControl{Type: frameError, Error: err.Error()})
		_ = ws.WriteControl(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseInternalServerErr, "attach failed"),
			time.Now().Add(time.Second))
		_ = ws.Close()
		return
	}
	serveLive(h.hub, ws, att, clientID, viewer, replay, end)
}

// serveTerminal replays a finished session: bytes, the offset frame, then
// the exit or lost frame and a normal close. Incoming frames are drained
// and ignored so a client that writes during replay never blocks the close.
func serveTerminal(ws *websocket.Conn, replay []byte, end int64, snap dagrun.Session) {
	mu := &wsMu{}
	go func() {
		for {
			if _, _, err := ws.ReadMessage(); err != nil {
				return
			}
		}
	}()
	if len(replay) > 0 {
		if !writeBinary(ws, mu, replay) {
			return
		}
	}
	writeControl(ws, mu, serverControl{Type: frameOffset, Offset: &end})
	switch snap.State {
	case dagrun.StateLost:
		writeControl(ws, mu, serverControl{Type: frameLost})
	default:
		code := 0
		if snap.ExitCode != nil {
			code = *snap.ExitCode
		}
		writeControl(ws, mu, serverControl{Type: frameExit, ExitCode: &code})
	}
	normalClose(ws)
}

// serveLive streams a live attachment: the replay captured before subscribing,
// the offset frame, then live bytes until exit, loss, eviction, or disconnect.
// replay is the log prefix already sent here; the first len(replay) bytes of
// the attachment's own replay (a superset, the log only grows) are skipped so
// no byte is duplicated.
func serveLive(hub *dagrun.Hub, ws *websocket.Conn, att *dagrun.Attachment, clientID string, viewer bool, replay []byte, end int64) {
	mu := &wsMu{}
	done := make(chan struct{})
	var once sync.Once
	stop := func() { once.Do(func() { close(done) }) }

	// Client -> session. A broken socket unblocks the pump below by
	// closing the attachment; closing never touches the session record.
	go func() {
		defer stop()
		defer att.Close()
		for {
			typ, msg, err := ws.ReadMessage()
			if err != nil {
				return
			}
			switch typ {
			case websocket.BinaryMessage:
				if viewer {
					writeControl(ws, mu, serverControl{Type: frameError, Error: "attach: viewer is read-only"})
					continue
				}
				if _, err := hub.Input(msg); err != nil {
					writeControl(ws, mu, serverControl{Type: frameError, Error: err.Error()})
				}
			case websocket.TextMessage:
				var m clientControl
				if err := json.Unmarshal(msg, &m); err != nil {
					continue
				}
				switch m.Type {
				case frameResize, frameSize:
					if err := hub.Resize(m.Rows, m.Cols); err != nil {
						writeControl(ws, mu, serverControl{Type: frameError, Error: err.Error()})
					}
				case frameSignal:
					if m.Signal == "" {
						continue
					}
					if err := hub.SendSignal(m.Signal); err != nil {
						writeControl(ws, mu, serverControl{Type: frameError, Error: err.Error()})
					}
				}
			}
		}
	}()

	// Session -> client. The deferred close drops the attachment from the
	// Hub's live set on every path; a broken-socket disconnect also
	// records the holder's disconnect, while terminal and evicted ends
	// leave the session record to its new owner.
	evicted := false
	terminal := false
	clientGone := false
	defer func() {
		att.Close()
		if clientGone && !terminal && !evicted && !viewer {
			_ = hub.Detach(clientID, time.Now())
		}
	}()
	if len(replay) > 0 {
		if !writeBinary(ws, mu, replay) {
			clientGone = true
			return
		}
	}
	writeControl(ws, mu, serverControl{Type: frameOffset, Offset: &end})
	skip := len(replay)
	buf := make([]byte, 4096)
	for {
		n, err := att.Read(buf)
		if n > 0 {
			p := buf[:n]
			if skip >= n {
				skip -= n
				continue
			}
			if skip > 0 {
				p = p[skip:]
				skip = 0
			}
			if !writeBinary(ws, mu, p) {
				clientGone = true
				return
			}
			continue
		}
		switch {
		case errors.Is(err, dagrun.ErrEvicted):
			// Takeover: this frame closes the previous holder only.
			evicted = true
			writeControl(ws, mu, serverControl{Type: frameEvicted})
			normalClose(ws)
			return
		case errors.Is(err, io.EOF):
			terminal = true
			code := 0
			if snap := hub.Session(); snap.ExitCode != nil {
				code = *snap.ExitCode
			}
			writeControl(ws, mu, serverControl{Type: frameExit, ExitCode: &code})
			normalClose(ws)
			return
		case errors.Is(err, dagrun.ErrSessionLost):
			terminal = true
			writeControl(ws, mu, serverControl{Type: frameLost})
			normalClose(ws)
			return
		case errors.Is(err, dagrun.ErrAttachmentClosed):
			// The socket broke (or the Hub closed).
			select {
			case <-done:
				clientGone = true
			default:
			}
			return
		case err != nil:
			return
		}
	}
}

// wsMu serializes concurrent websocket writers: the pump and the read loop
// both send frames on one connection.
type wsMu struct {
	mu sync.Mutex
}

func writeBinary(ws *websocket.Conn, mu *wsMu, p []byte) bool {
	mu.mu.Lock()
	defer mu.mu.Unlock()
	_ = ws.SetWriteDeadline(time.Now().Add(10 * time.Second))
	return ws.WriteMessage(websocket.BinaryMessage, p) == nil
}

func writeControl(ws *websocket.Conn, mu *wsMu, c serverControl) {
	raw, err := json.Marshal(c)
	if err != nil {
		return
	}
	mu.mu.Lock()
	defer mu.mu.Unlock()
	_ = ws.SetWriteDeadline(time.Now().Add(10 * time.Second))
	_ = ws.WriteMessage(websocket.TextMessage, raw)
}

// normalClose ends a terminal or evicted stream with a normal close code, so
// the client sees an orderly end rather than a dropped connection.
func normalClose(ws *websocket.Conn) {
	_ = ws.WriteControl(websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseNormalClosure, "attach ended"),
		time.Now().Add(time.Second))
	_ = ws.Close()
}
