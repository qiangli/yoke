package steward_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/bus"
	_ "github.com/qiangli/yoke/pkg/steward"
	_ "github.com/qiangli/yoke/pkg/weave"
)

// Exercise the production role sources and authorizer against a sprint lease,
// without replacing HostRoles or RoleReaderAuthorizer with test implementations.
func TestConductorMail_CurrentLeaseHolderAndHandoff(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BASHY_SPRINT_DIR", dir)
	t.Setenv("BASHY_MB_DIR", t.TempDir())
	t.Setenv("BASHY_ROOM_DIR", t.TempDir())
	setHolder := func(holder string) {
		t.Helper()
		queue := map[string]any{"stories": []any{map[string]any{
			"id": 22, "title": "Role mail test", "lease": map[string]any{"holder": holder, "at": time.Now()},
		}}}
		b, err := json.Marshal(queue)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "queue.json"), b, 0600); err != nil {
			t.Fatal(err)
		}
	}
	setHolder("first-holder")
	rec, err := bus.ResolveRecipient("conductor:22")
	if err != nil {
		t.Fatal(err)
	}
	if rec.Kind != bus.TargetRole {
		t.Fatalf("not a role: %+v", rec)
	}
	if err := bus.PostMessage(bus.Post{From: "tester", To: rec.Addr, Body: "survive handoff"}); err != nil {
		t.Fatal(err)
	}
	check := func(holder, other string) {
		t.Helper()
		posts, err := bus.Posts()
		if err != nil {
			t.Fatal(err)
		}
		if len(posts) != 1 || posts[0].To != rec.Addr {
			t.Fatalf("stored mail changed: %+v", posts)
		}
		if got := bus.FilterPostsForReader(posts, holder); len(got) != 1 {
			t.Fatalf("current lease holder %s cannot read: %+v", holder, got)
		}
		for _, reader := range []string{other, "intruder", rec.Addr} {
			if got := bus.FilterPostsForReader(posts, reader); len(got) != 0 {
				t.Fatalf("non-holder %s reads mail: %+v", reader, got)
			}
		}
	}
	check("first-holder", "next-holder")
	setHolder("next-holder")
	check("next-holder", "first-holder")
}
