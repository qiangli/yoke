package weave

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/room"

	todopkg "github.com/qiangli/yoke/pkg/todo"
)

// Sprint: #467; Story: #2369; Story-ID: 962f0c7d61df
// A verified per-issue clone of the holder may claim the holder's story; a
// foreign agent, and a lookalike name that is not catalog-cloned from the
// holder, stay refused.
func TestStoryClaimHolderCloneLineage(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("WEAVE_CONDUCTOR", "boss")
	seedLiveAgent(t, "boss")
	seedLiveAgent(t, "parent")
	seedLiveAgent(t, "foreign")
	seedLiveAgent(t, "parent-w9")

	cat := fleetCatalog()
	clone, err := cat.CloneAgent("parent", "parent-w7", true, "weave #7")
	if err != nil {
		t.Skipf("clone unavailable: %v", err)
	}
	if err := cat.SaveAgent(clone); err != nil {
		t.Fatal(err)
	}
	if err := room.Join(room.Card{ID: "parent-w7", Nick: "parent-w7", Tool: "claude", Model: "opus5",
		Binding: "claude:opus5", Role: "conductor", Mode: "weave", Caps: []string{room.CapInboxDelivery},
		PID: os.Getpid(), Joined: time.Now().UTC().Format(time.RFC3339)}); err != nil {
		t.Skipf("host room unavailable: %v", err)
	}
	t.Cleanup(func() { room.Leave("parent-w7") })

	root := mustStoryRoot(t)
	if out, code := runSprint(t, "add", "lineage"); code != 0 {
		t.Fatalf("add exit=%d: %s", code, out)
	}
	if out, code := runSprint(t, "start", "1", "--owner", "boss", "--for", "1h"); code != 0 {
		t.Fatalf("start exit=%d: %s", code, out)
	}
	store := todopkg.RepoStore(root)
	it, err := todopkg.Add(store, "held story", "", "p0", nil, "", "")
	if err != nil {
		t.Skipf("todo store unavailable here: %v", err)
	}
	it.Sprint = 1
	if _, err := store.Save(it); err != nil {
		t.Fatal(err)
	}
	if out, code := runSprint(t, "track", "1"); code != 0 {
		t.Fatalf("track exit=%d: %s", code, out)
	}
	if out, code := runSprint(t, "claim", "1", it.ID, "--owner", "parent"); code != 0 {
		t.Fatalf("parent claim exit=%d: %s", code, out)
	}

	for _, who := range []string{"foreign", "parent-w9"} {
		out, code := runSprint(t, "claim", "1", it.ID, "--owner", who)
		if code == 0 || !strings.Contains(out, "held by parent") {
			t.Fatalf("%s must be refused (exit=%d): %s", who, code, out)
		}
	}
	if out, code := runSprint(t, "claim", "1", it.ID, "--owner", "parent-w7"); code != 0 {
		t.Fatalf("holder's verified clone refused (exit=%d): %s", code, out)
	}
	got, err := todopkg.ResolveRef(store, it.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Assignee != "parent-w7" {
		t.Fatalf("assignee = %q, want parent-w7", got.Assignee)
	}
}
