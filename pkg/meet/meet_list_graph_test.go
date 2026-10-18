package meet

import (
	"bytes"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/lens/stream"
)

func TestMeetListGraphStream(t *testing.T) {
	rooms := []*State{
		{ID: "z-room", Room: 2, Topic: "Later", Status: "open", Participants: []string{"steward"}},
		{ID: "a-room", Room: 1, Topic: "First", Status: "open", Participants: []string{"human"}},
	}
	encode := func(ss []*State) string {
		t.Helper()
		var out bytes.Buffer
		if err := writeMeetListGraph(&out, ss, ""); err != nil {
			t.Fatal(err)
		}
		return out.String()
	}
	first := encode(rooms)
	if got := encode([]*State{rooms[1], rooms[0]}); got != first {
		t.Fatalf("room enumeration changed stream bytes:\n%s\nversus\n%s", first, got)
	}
	g, err := stream.Decode(strings.NewReader(first), stream.DecodeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := g.Verify(); err != nil {
		t.Fatal(err)
	}
	if !g.Framed || g.Source != "meet:*" || len(g.Nodes) != 3 || len(g.Edges) != 2 {
		t.Fatalf("unexpected graph: %+v", g)
	}
	if g.Nodes[1].Ref != "meet:a-room" || g.Nodes[2].Ref != "meet:z-room" {
		t.Fatalf("room addresses are not stable: %+v", g.Nodes)
	}
	if g.Cursor == nil || g.Cursor.Binding.Provider != "meet" || g.Cursor.Binding.Revision != g.Rev {
		t.Fatalf("cursor is not bound to this snapshot: %+v", g.Cursor)
	}
}
