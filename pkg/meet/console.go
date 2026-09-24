// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package meet

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/gorilla/websocket"
)

// THE CONSOLE: a live agent's own terminal, mirrored read-only.
//
// A seat launched through bashy (`bashy chat --agent X`) runs its TUI on a PTY
// bashy owns, and the wrapper tees every byte the terminal receives into the
// card's LogPath. That capture IS the tee point: this handler replays it and
// follows it, so a phone sees exactly what the operator's terminal shows —
// every turn, every tool call, every intermediate stream — at the size it was
// drawn for.
//
// Output only. Nothing a client sends here reaches the agent; input stays on
// the one path it already has (the composer → the seat's control socket), so
// the mirror adds a way to watch and no second way to type.
//
// Wire: a text frame {"type":"geometry","cols":N,"rows":N} first and on every
// resize, then the raw bytes as binary frames. {"type":"reset"} means a new
// session truncated the capture and the bytes restart from its beginning;
// {"type":"end"} means the seat is gone.

const (
	consolePoll = 100 * time.Millisecond
	// consoleSeatCheck bounds how stale a geometry change or a departure can be.
	consoleSeatCheck = time.Second
	// consoleReplayMax caps the replay. The capture restarts with every session,
	// so byte 0 is the session's start and replaying from it redraws the TUI
	// faithfully; a tail cut mid-repaint does not (Ink's relative cursor moves
	// leave stale rules). Past the cap, start at an escape so no half sequence
	// prints as text.
	consoleReplayMax = 4 << 20
	consoleChunk     = 64 << 10
)

// errNotMirrorable is what a caller sees for a seat whose terminal bashy does
// not own: nothing captured its bytes, so there is nothing to show.
var errNotMirrorable = errors.New("not mirrorable: this agent has no live session launched through bashy — start it with `bashy chat --agent <name>`")

type consoleFrame struct {
	Type string `json:"type"`
	Cols int    `json:"cols,omitempty"`
	Rows int    `json:"rows,omitempty"`
}

func handleConsoleDM(w http.ResponseWriter, r *http.Request) {
	agent := canonAgent(r.PathValue("agent"))
	card, ok := liveSeat(agent)
	if !ok || card.LogPath == "" {
		http.Error(w, errNotMirrorable.Error(), http.StatusNotFound)
		return
	}
	f, err := os.Open(card.LogPath)
	if err != nil {
		http.Error(w, errNotMirrorable.Error(), http.StatusNotFound)
		return
	}
	defer f.Close()
	conn, err := wsUpgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()

	// gorilla processes close/ping only while a reader runs; client frames are
	// otherwise ignored — this socket is output-only.
	gone := make(chan struct{})
	go func() {
		defer close(gone)
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}()

	if conn.WriteJSON(consoleFrame{Type: "geometry", Cols: card.Cols, Rows: card.Rows}) != nil {
		return
	}
	offset, err := consoleReplayStart(f)
	if err != nil {
		return
	}
	buf := make([]byte, consoleChunk)
	nextCheck := time.Now().Add(consoleSeatCheck)
	for {
		n, rerr := f.ReadAt(buf, offset)
		if n > 0 {
			if conn.WriteMessage(websocket.BinaryMessage, buf[:n]) != nil {
				return
			}
			offset += int64(n)
			continue // drain before sleeping
		}
		if rerr != nil && rerr != io.EOF {
			return
		}
		if time.Now().After(nextCheck) {
			nextCheck = time.Now().Add(consoleSeatCheck)
			now, ok := liveSeat(agent)
			if !ok || now.LogPath != card.LogPath {
				_ = conn.WriteJSON(consoleFrame{Type: "end"})
				return
			}
			if now.PID != card.PID || consoleTruncated(f, offset) {
				// A new session reuses the capture path and truncates it.
				offset = 0
				if conn.WriteJSON(consoleFrame{Type: "reset"}) != nil {
					return
				}
			}
			if now.Cols != card.Cols || now.Rows != card.Rows || now.PID != card.PID {
				if conn.WriteJSON(consoleFrame{Type: "geometry", Cols: now.Cols, Rows: now.Rows}) != nil {
					return
				}
			}
			card = now
		}
		select {
		case <-r.Context().Done():
			return
		case <-gone:
			return
		case <-time.After(consolePoll):
		}
	}
}

// consoleReplayStart is where a new viewer starts reading: byte 0, or past the
// cap the first escape at or after size-cap.
func consoleReplayStart(f *os.File) (int64, error) {
	st, err := f.Stat()
	if err != nil {
		return 0, err
	}
	if st.Size() <= consoleReplayMax {
		return 0, nil
	}
	from := st.Size() - consoleReplayMax
	probe := make([]byte, 4096)
	n, _ := f.ReadAt(probe, from)
	if i := bytes.IndexByte(probe[:n], 0x1b); i >= 0 {
		return from + int64(i), nil
	}
	return from, nil
}

func consoleTruncated(f *os.File, offset int64) bool {
	st, err := f.Stat()
	return err == nil && st.Size() < offset
}
