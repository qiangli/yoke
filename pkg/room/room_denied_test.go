package room

import (
	"os"
	"runtime"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/stateaccess"
)

// Sprint: #413; Story: #1829; Story-ID: ce5985c75042
// A sandbox that denies the room state must surface as a diagnosed denial from
// both the preflight and Join, and Join must leave no member card behind.
func TestDeniedRoomStateIsDiagnosedBeforeAnyCard(t *testing.T) {
	if os.Geteuid() == 0 || runtime.GOOS == "windows" {
		t.Skip("directory permission bits do not deny root or Windows")
	}
	dir := t.TempDir()
	t.Setenv("BASHY_ROOM_DIR", dir)
	if _, err := membersDir(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })

	if err := Preflight(); !stateaccess.IsDenied(err) {
		t.Fatalf("Preflight: want denial, got %v", err)
	}
	err := Join(Card{ID: "denied-agent", PID: os.Getpid()})
	if !stateaccess.IsDenied(err) || !strings.Contains(err.Error(), "member-claims.lock") {
		t.Fatalf("Join: want diagnosed denial, got %v", err)
	}
	entries, _ := os.ReadDir(dir + "/members")
	if len(entries) != 0 {
		t.Fatalf("denied Join left %d member entries", len(entries))
	}
}
