// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package chat

import (
	"os"
	"testing"

	"github.com/qiangli/yoke/pkg/room"
)

// A mirror replays the raw capture at the card's geometry, so the card must
// carry the PTY size from the start and follow every resize.
func TestPublishGeometryTracksThePTYSize(t *testing.T) {
	t.Setenv("BASHY_ROOM_DIR", t.TempDir())
	card := room.Card{ID: "geom-agent", Tool: "claude", Binding: "claude:x", Mode: "interactive", PID: os.Getpid(), LogPath: "/tmp/x.log"}
	if err := room.Join(card); err != nil {
		t.Fatal(err)
	}
	resize := publishGeometry(card)
	for _, want := range []struct{ rows, cols uint16 }{{40, 120}, {50, 200}} {
		resize(want.rows, want.cols)
		got, ok, err := room.Find(card.ID)
		if err != nil || !ok {
			t.Fatalf("find: ok=%v err=%v", ok, err)
		}
		if got.Rows != int(want.rows) || got.Cols != int(want.cols) {
			t.Fatalf("card geometry = %dx%d, want %dx%d", got.Cols, got.Rows, want.cols, want.rows)
		}
		if got.LogPath != card.LogPath {
			t.Fatalf("resize dropped the log path: %q", got.LogPath)
		}
	}
}
