package coord

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/principal"
	"github.com/qiangli/yoke/pkg/prune"
)

func TestClaimStoryFlagAndEnvPersistence(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BASHY_COORD_DIR", dir)
	t.Setenv("BASHY_AGENT", "alice")
	t.Setenv("WEAVE_STORY", "652/a52829eaaa35")

	run := func(args ...string) string {
		t.Helper()
		cmd := NewClaimCmd(func() []string { return []string{"/w/proj"} })
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetErr(&out)
		cmd.SetArgs(args)
		if err := cmd.Execute(); err != nil {
			t.Fatalf("claim %v: %v\n%s", args, err, out.String())
		}
		return out.String()
	}

	// Default from $WEAVE_STORY when --story is omitted.
	var c1 Claim
	if err := json.Unmarshal([]byte(run("lane-one", "--json")), &c1); err != nil {
		t.Fatal(err)
	}
	if c1.Story != "652/a52829eaaa35" {
		t.Fatalf("Story from WEAVE_STORY = %q, want 652/a52829eaaa35", c1.Story)
	}

	// Refresh / re-acquire without repeating --story preserves existing Story.
	t.Setenv("WEAVE_STORY", "")
	var c2 Claim
	if err := json.Unmarshal([]byte(run("lane-one", "--intent", "still testing", "--json")), &c2); err != nil {
		t.Fatal(err)
	}
	if c2.Story != "652/a52829eaaa35" || c2.Epoch != c1.Epoch {
		t.Fatalf("reused claim = %+v, want preserved Story and Epoch %d", c2, c1.Epoch)
	}

	// Explicit --story overrides env on project claim too.
	var proj Claim
	if err := json.Unmarshal([]byte(run("--story", "652/bf2f149daa87", "--json")), &proj); err != nil {
		t.Fatal(err)
	}
	if proj.Story != "652/bf2f149daa87" {
		t.Fatalf("project claim Story = %q, want 652/bf2f149daa87", proj.Story)
	}
}

func TestReclaimClaimsRemovesAnnounceAndLeasePreservingEpochAndAttached(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BASHY_COORD_DIR", dir)
	RegisterKind(Kind{Name: "story", Match: MatchName, Modes: []string{ModeAnnounce}})

	// 1. Announce-mode story claim held by a departed session.
	gStory, err := AcquireRef(context.Background(), Request{
		Ref:    Ref{Kind: "story", Name: "652/a52829eaaa35"},
		Holder: principal.Ref{Name: "worker-old", Episode: "ep-gone", Host: "host-a"},
		Mode:   ModeAnnounce,
	})
	if err != nil {
		t.Fatal(err)
	}
	if gStory.Epoch != 1 {
		t.Fatalf("initial story epoch = %d, want 1", gStory.Epoch)
	}

	// 2. Detached lease claim naming the story, held by a second episode.
	if _, err := AcquireRef(context.Background(), Request{
		Ref:    Ref{Kind: KindName, Name: "lane-alpha"},
		Holder: principal.Ref{Name: "worker-old", Episode: "ep-two", Host: "host-a"},
		Story:  "652/a52829eaaa35",
		Mode:   ModeLease,
	}); err != nil {
		t.Fatal(err)
	}

	// 3. Unrelated lease claim that must survive.
	if _, err := AcquireRef(context.Background(), Request{
		Ref:    Ref{Kind: KindName, Name: "lane-unrelated"},
		Holder: principal.Ref{Name: "other-agent", Episode: "ep-three", Host: "host-a"},
		Mode:   ModeLease,
	}); err != nil {
		t.Fatal(err)
	}

	// 4. Live kernel-locked attached hold naming the story: must never be broken
	// while the child process still holds the kernel lock.
	_, attachedLock, err := AcquireAttachedRef(context.Background(), Request{
		Ref:    Ref{Kind: KindName, Name: "lane-attached"},
		Holder: principal.Ref{Name: "worker-child", Episode: "ep-child", Host: "host-a"},
		Story:  "652/a52829eaaa35",
		Mode:   ModeAttached,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer attachedLock.Release()

	reclaimed, err := ReclaimClaims(dir, func(c *Claim) (string, bool) {
		if (c.Kind == "story" && c.Resource == "652/a52829eaaa35") || c.Story == "652/a52829eaaa35" {
			return "sprint accept", true
		}
		return "", false
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(reclaimed) != 2 {
		t.Fatalf("reclaimed %d claims (%+v), want 2 (story announce + detached lease)", len(reclaimed), reclaimed)
	}

	remaining, err := List(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 2 {
		t.Fatalf("remaining claims = %d (%+v), want 2 (unrelated + live attached)", len(remaining), remaining)
	}

	// High-water fencing token must survive deletion so a re-claim advances to epoch 2.
	reacquired, err := AcquireRef(context.Background(), Request{
		Ref:    Ref{Kind: "story", Name: "652/a52829eaaa35"},
		Holder: principal.Ref{Name: "worker-new", Episode: "ep-new", Host: "host-a"},
		Mode:   ModeAnnounce,
	})
	if err != nil {
		t.Fatal(err)
	}
	if reacquired.Epoch != 2 {
		t.Fatalf("reacquired epoch = %d, want 2 (high-water fence preserved)", reacquired.Epoch)
	}
}

type stubClosedStoryPruner struct {
	items   []prune.Item
	applied int
}

func (s *stubClosedStoryPruner) DryRunClosedStories(_ context.Context, _ string, _ prune.Options) ([]prune.Item, error) {
	return append([]prune.Item(nil), s.items...), nil
}

func (s *stubClosedStoryPruner) ApplyClosedStories(_ context.Context, _ string, items []prune.Item) (prune.Counts, error) {
	s.applied += len(items)
	return prune.Counts{Removed: len(items)}, nil
}

func TestClaimPruneStoriesClosedCLIDispatch(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BASHY_COORD_DIR", dir)

	prev := ClosedStories
	stub := &stubClosedStoryPruner{
		items: []prune.Item{{Noun: "claim", ID: "story:652/a52829eaaa35", Reason: "story a52829ea is done"}},
	}
	ClosedStories = stub
	t.Cleanup(func() { ClosedStories = prev })

	run := func(args ...string) string {
		t.Helper()
		cmd := NewClaimCmd(func() []string { return nil })
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetErr(&out)
		cmd.SetArgs(args)
		if err := cmd.Execute(); err != nil {
			t.Fatalf("claim %v: %v\n%s", args, err, out.String())
		}
		return out.String()
	}

	dryOut := run("prune", "--stories-closed")
	if !strings.Contains(dryOut, "would remove claim story:652/a52829eaaa35") || stub.applied != 0 {
		t.Fatalf("dry-run output = %q, applied = %d", dryOut, stub.applied)
	}

	jsonOut := run("prune", "--stories-closed", "--json")
	var res prune.Result
	if err := json.Unmarshal([]byte(jsonOut), &res); err != nil {
		t.Fatal(err)
	}
	if res.Applied || len(res.Items) != 1 || res.Items[0].ID != "story:652/a52829eaaa35" {
		t.Fatalf("json dry-run = %+v", res)
	}

	applyOut := run("prune", "--stories-closed", "--apply")
	if !strings.Contains(applyOut, "removed claim story:652/a52829eaaa35") || stub.applied != 1 {
		t.Fatalf("apply output = %q, applied = %d", applyOut, stub.applied)
	}
}
