package handoff

import (
	"os"
	"testing"
)

// TestMain keeps the host event log (room timeline) these commands now write to
// out of the operator's real ~/.bashy/room.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "handoff-room-test-*")
	if err != nil {
		panic(err)
	}
	os.Setenv("BASHY_ROOM_DIR", dir)
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}
