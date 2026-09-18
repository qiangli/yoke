package meet

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/ref"
)

func TestRegisterRefsMeetFound(t *testing.T) {
	t.Setenv("BASHY_HOME", t.TempDir())
	t.Setenv("BASHY_MEET_DIR", t.TempDir())
	st := saveMeeting(t, "20260914-a2", 3, "open")
	st.Name = "lane a2"
	if err := st.save(); err != nil {
		t.Fatal(err)
	}

	g := ref.NewRegistry()
	RegisterRefs(g)
	n, err := g.Resolve("meet:" + st.ID)
	if err != nil {
		t.Fatal(err)
	}
	if n.Ref != "meet:"+st.ID || n.ID != st.ID || n.Title != "lane a2" || n.Status != "open" {
		t.Fatalf("node = %+v", n)
	}
	if n.Where != os.Getenv("BASHY_MEET_DIR") {
		t.Fatalf("where = %q", n.Where)
	}
	if n.Open != "bashy meet show "+st.ID {
		t.Fatalf("open = %q", n.Open)
	}
}

func TestRegisterRefsMeetNotFound(t *testing.T) {
	t.Setenv("BASHY_HOME", t.TempDir())
	t.Setenv("BASHY_MEET_DIR", t.TempDir())

	g := ref.NewRegistry()
	RegisterRefs(g)
	_, err := g.Resolve("meet:missing")
	if !errors.Is(err, ref.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestRegisterRefsMeetRoomNumberReturnsDurableID(t *testing.T) {
	t.Setenv("BASHY_HOME", t.TempDir())
	t.Setenv("BASHY_MEET_DIR", t.TempDir())
	st := saveMeeting(t, "durable-room-id", 7, "open")

	g := ref.NewRegistry()
	RegisterRefs(g)
	byRoom, err := g.Resolve("meet:7")
	if err != nil {
		t.Fatal(err)
	}
	byID, err := g.Resolve("meet:" + st.ID)
	if err != nil {
		t.Fatal(err)
	}
	if byRoom != byID {
		t.Fatalf("room ref = %+v, id ref = %+v", byRoom, byID)
	}
	if byRoom.Ref != "meet:"+st.ID {
		t.Fatalf("room emitted %q, want durable ref", byRoom.Ref)
	}
}

func TestMeetShowJSONIncludesLinks(t *testing.T) {
	t.Setenv("BASHY_HOME", t.TempDir())
	t.Setenv("BASHY_MEET_DIR", t.TempDir())
	st := saveMeeting(t, "show-links-room", 1, "open")
	if err := appendEvent(st.ID, Event{Kind: "human", Speaker: "human", Text: "see [[kb:missing-page]] and [[bus:12]]", TS: time.Now()}); err != nil {
		t.Fatal(err)
	}

	cmd := newShowCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{st.ID, "--json", "--links"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	var got struct {
		Links []meetLinkRef `json:"links"`
	}
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Links) != 2 {
		t.Fatalf("links = %+v, want two", got.Links)
	}
	statuses := map[string]string{}
	for _, l := range got.Links {
		statuses[l.Ref] = l.Status
	}
	if statuses["kb:missing-page"] != "dangling" || statuses["bus:12"] != "external" {
		t.Fatalf("statuses = %+v", statuses)
	}

	text := newShowCmd()
	out.Reset()
	text.SetOut(&out)
	text.SetArgs([]string{st.ID, "--links"})
	if err := text.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "kb:missing-page") || !strings.Contains(out.String(), "dangling") {
		t.Fatalf("text links missing:\n%s", out.String())
	}
}
