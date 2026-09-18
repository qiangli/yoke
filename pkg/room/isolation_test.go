package room

import (
	"path/filepath"
	"testing"
)

// $BASHY_HOME is the documented way to relocate a whole bashy home, and the
// sprint store, foreman and webconsole all honour it. Room did not, so a run
// sandboxed with it got an isolated sprint store whose announcements still
// landed on the SHARED host board — two smoke tests posted a throwaway sprint
// onto the operator's real board that way, and mb is append-only.
func TestBashyHomeRelocatesTheRoomRoot(t *testing.T) {
	home := t.TempDir()
	t.Setenv("BASHY_ROOM_DIR", "")
	t.Setenv("BASHY_HOME", home)

	want := filepath.Join(home, "room")
	if got := Dir(); got != want {
		t.Fatalf("Dir() = %q, want %q — a $BASHY_HOME sandbox must not reach the shared board", got, want)
	}
}

// Precedence is unchanged: the specific override still wins over the home.
func TestRoomDirOverrideBeatsBashyHome(t *testing.T) {
	specific := t.TempDir()
	t.Setenv("BASHY_HOME", t.TempDir())
	t.Setenv("BASHY_ROOM_DIR", specific)

	if got := Dir(); got != specific {
		t.Fatalf("Dir() = %q, want the explicit BASHY_ROOM_DIR %q", got, specific)
	}
}
