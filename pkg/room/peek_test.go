package room

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPeekMembersDoesNotCreateRoom(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, "room")
	t.Setenv("BASHY_ROOM_DIR", dir)
	cards, err := PeekMembers()
	if err != nil || len(cards) != 0 {
		t.Fatalf("peek = %+v, %v", cards, err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("peek created room: %v", err)
	}
}
