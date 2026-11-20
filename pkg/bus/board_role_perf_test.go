package bus

import (
	"fmt"
	"sync/atomic"
	"testing"
	"time"
)

// Regression for 682f7e66 (1a): Unseen over a large board used to invoke
// HostRoles() on EVERY post with a non-empty To (twice per matching post),
// re-reading and unmarshaling the 18 MB sprint queue thousands of times during
// one `sprint show` (~122s). A single board read or archive scan must snapshot
// HostRoles() at most once while preserving exact role-holder routing.
func TestUnseenMemoizesHostRolesAcrossManyDirectedPosts(t *testing.T) {
	t.Setenv("BASHY_MB_DIR", t.TempDir())
	t.Setenv("BASHY_ROOM_DIR", t.TempDir())

	prevRoles := HostRoles
	prevAuth := RoleReaderAuthorizer
	t.Cleanup(func() {
		HostRoles = prevRoles
		RoleReaderAuthorizer = prevAuth
	})

	var roleCalls atomic.Int32
	HostRoles = func() []HostRole {
		roleCalls.Add(1)
		return []HostRole{
			{Label: "conductor:576", Topic: "conductor.sprint.576", Holder: "claude-opus5.5"},
			{Label: "conductor:999", Topic: "conductor.sprint.999", Holder: ""},
		}
	}
	RoleReaderAuthorizer = AuthorizeByHolder

	// Seed >64 KB / hundreds of directed posts so postsAfter reads from the top
	// of posts.jsonl rather than hitting the short tail shortcut.
	padding := fmt.Sprintf("%0400d", 0)
	for i := 0; i < 220; i++ {
		to := "other-worker"
		switch {
		case i == 10 || i == 150:
			to = "claude-opus5.5"
		case i == 25 || i == 180:
			to = "conductor.sprint.576"
		case i == 40:
			to = "conductor.sprint.999"
		}
		if err := PostMessage(Post{
			From: "worker",
			To:   to,
			Body: fmt.Sprintf("post-%d-%s", i, padding),
		}); err != nil {
			t.Fatalf("PostMessage(%d): %v", i, err)
		}
	}

	roleCalls.Store(0)
	directed, _, _, err := Unseen("claude-opus5.5", 0)
	if err != nil {
		t.Fatalf("Unseen: %v", err)
	}
	if len(directed) != 4 {
		t.Fatalf("directed count = %d, want 4 (2 direct + 2 held role posts)", len(directed))
	}
	if got := roleCalls.Load(); got != 1 {
		t.Fatalf("HostRoles() called %d times during one Unseen scan of 220 posts, want 1", got)
	}

	// Archive check over the same snapshot also reuses the memoized role table.
	all, err := Posts()
	if err != nil {
		t.Fatal(err)
	}
	roleCalls.Store(0)
	snap := newAudienceSnapshot()
	now := time.Now().UTC()
	for _, p := range all {
		_ = canArchivePost(p, []boardReaderState{{name: "claude-opus5.5", cursor: 0, lastPoll: now}}, now, time.Hour, snap)
	}
	if got := roleCalls.Load(); got != 1 {
		t.Fatalf("HostRoles() called %d times during archive scan with shared snapshot, want 1", got)
	}
}
