package ladder

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStoreRoundTripAndTruncatedLine(t *testing.T) {
	home := t.TempDir()
	t.Setenv("BASHY_HOME", home)
	path := DefaultStorePath()
	if path != filepath.Join(home, "ladder", "events.jsonl") {
		t.Fatal(path)
	}
	s, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	e := eventTestDelivery("id-1", "agent-a", "story-a", 1, 1)
	if err = s.Append(e); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("file mode: %v %v", info, err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.WriteString("{\"kind\":"); err != nil {
		t.Fatal(err)
	}
	f.Close()
	got, err := s.Read()
	if len(got) != 1 || err == nil || !strings.Contains(err.Error(), "truncated") {
		t.Fatalf("got=%+v err=%v", got, err)
	}
	if got[0].ID != e.ID {
		t.Fatal(got[0])
	}
	if err = s.Append(Event{Kind: "unknown", Agent: "agent-a", Season: 1}); err == nil {
		t.Fatal("accepted unknown kind")
	}
}
