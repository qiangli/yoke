package shape

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/issue"
	verifyevidence "github.com/qiangli/yoke/pkg/shape/evidence"
	"github.com/qiangli/yoke/pkg/todo"
)

func trackedChange(id string) issue.ShapeChange {
	return issue.ShapeChange{App: "hello-world", Candidate: id, Actor: "test-agent", RepresentedUser: "test-user"}
}
func TestTrackerCreatesOneOrdinaryTodoAndUpdatesEvidence(t *testing.T) {
	store := &issue.Store{Root: t.TempDir()}
	tracker := Tracker{Store: store}
	ctx := context.Background()
	change := trackedChange("1234567890abcdef")
	first, err := tracker.Record(ctx, change, "Change greeting", "", false)
	if err != nil {
		t.Fatal(err)
	}
	change.Version = "candidate-commit"
	change.Evidence = "evidence-ref"
	second, err := tracker.Record(ctx, change, "Change greeting", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if first != second || !strings.HasPrefix(first, "todo:") {
		t.Fatalf("unstable reference %q %q", first, second)
	}
	items, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("got %d tracked items", len(items))
	}
	got := items[0]
	if got.Sprint != 0 || got.SprintID != "" || got.Status != todo.StatusTodo || got.Origin != "shape" || len(got.Shape) != 1 || got.Shape[0].App != change.App || got.Shape[0].Version != change.Version || got.Shape[0].Evidence != "shape-evidence:"+change.Version {
		t.Fatalf("bad item %#v", got)
	}
}
func TestTrackerLinksStoryWithoutClobberingMetadata(t *testing.T) {
	store := &issue.Store{Root: t.TempDir()}
	existing := &issue.Issue{ID: "abc123", Kind: issue.KindTask, Title: "Existing", Status: "doing", Body: "Keep this body.", Labels: []string{"existing"}, Sprint: 564, SprintID: "stable", Extra: map[string]any{"custom": "keep"}}
	if _, err := store.Save(existing); err != nil {
		t.Fatal(err)
	}
	tracker := Tracker{Store: store}
	ctx := context.Background()
	for _, id := range []string{"1234567890abcdef", "fedcba0987654321"} {
		if _, err := tracker.Record(ctx, trackedChange(id), "ignored", "abc123", true); err != nil {
			t.Fatal(err)
		}
	}
	got, err := store.Resolve("abc123")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != existing.Status || got.Body != existing.Body || got.SprintID != existing.SprintID || !reflect.DeepEqual(got.Extra, existing.Extra) || len(got.Shape) != 2 || !reflect.DeepEqual(got.Labels, []string{"existing", "shaping"}) {
		t.Fatalf("clobbered: %#v", got)
	}
}
func TestTrackingFailuresDoNotProduceALink(t *testing.T) {
	ctx := context.Background()
	store := &issue.Store{Root: t.TempDir()}
	tracker := Tracker{Store: store}
	if _, err := tracker.Record(ctx, trackedChange("1234567890abcdef"), "change", "missing", false); err == nil {
		t.Fatal("missing item accepted")
	}
	blocked := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocked, []byte("file"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := (Tracker{Store: &issue.Store{Root: blocked}}).Record(ctx, trackedChange("1234567890abcdef"), "change", "", false); err == nil {
		t.Fatal("failed persistence accepted")
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := tracker.Record(cancelled, trackedChange("1234567890abcdef"), "change", "", false); err == nil {
		t.Fatal("cancelled request accepted")
	}
}
func TestTrackingCallbackKeepsStoreAndReciprocalLink(t *testing.T) {
	c := Candidate{ID: "1234567890abcdef", App: "hello-world", Actor: "agent", RepresentedUser: "person", Request: "Change greeting", TrackingRoot: t.TempDir()}
	track := NewTracking(t.TempDir())
	if err := track(context.Background(), &c); err != nil {
		t.Fatal(err)
	}
	if c.TrackingItem == "" {
		t.Fatal("candidate missing item link")
	}
	c.Rev = "version"
	c.Verdict = &Verdict{Pass: true, Evidence: "evidence"}
	if err := track(context.Background(), &c); err != nil {
		t.Fatal(err)
	}
	store := &issue.Store{Root: c.TrackingRoot, Sub: "."}
	item, err := store.Resolve(strings.TrimPrefix(c.TrackingItem, "todo:"))
	if err != nil {
		t.Fatal(err)
	}
	if len(item.Shape) != 1 || item.Shape[0].Version != "version" || item.Shape[0].Evidence != "shape-evidence:version" {
		t.Fatalf("missing reciprocal evidence %#v", item)
	}
	c.Story = "abc"
	c.Todo = "def"
	if err := track(context.Background(), &c); err == nil {
		t.Fatal("conflicting selectors accepted")
	}
}

func TestTrackerConcurrentUpdatesRetainBothCandidates(t *testing.T) {
	store := &issue.Store{Root: t.TempDir()}
	existing := &issue.Issue{ID: "abc123", Kind: issue.KindTask, Title: "Shared", Status: "todo"}
	if _, err := store.Save(existing); err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 2)
	for _, id := range []string{"1234567890abcdef", "fedcba0987654321"} {
		go func(id string) {
			_, err := (Tracker{Store: store}).Record(context.Background(), trackedChange(id), "Change", "abc123", false)
			result <- err
		}(id)
	}
	for range 2 {
		if err := <-result; err != nil {
			t.Fatal(err)
		}
	}
	got, err := store.Resolve("abc123")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Shape) != 2 {
		t.Fatalf("lost concurrent link: %#v", got.Shape)
	}
	change := trackedChange("1234567890abcdef")
	change.Actor = "other-agent"
	if _, err := (Tracker{Store: store}).Record(context.Background(), change, "Change", "abc123", false); err == nil {
		t.Fatal("creator identity rewritten")
	}
}

func TestTrackingAutomaticTodoUsesConfiguredHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("BASHY_TODO_DIR", "")
	c := Candidate{ID: "1234567890abcdef", App: "hello-world", Actor: "test-agent", RepresentedUser: "person", Request: "Change greeting"}
	if err := NewTracking(home)(context.Background(), &c); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(home, "todo", todo.SanitizeOwner(c.Actor))
	if c.TrackingRoot != want {
		t.Fatalf("auto todo escaped configured home: %s, want %s", c.TrackingRoot, want)
	}
	items, err := (&issue.Store{Root: want, Sub: "."}).List()
	if err != nil || len(items) != 1 {
		t.Fatalf("ordinary todo missing: %v %#v", err, items)
	}
}

func TestTrackingEvidenceLocatorContainsNoPrivatePath(t *testing.T) {
	home := t.TempDir()
	privatePath := filepath.Join(home, "private", "evidence.json")
	store := &issue.Store{Root: t.TempDir()}
	change := trackedChange("1234567890abcdef")
	change.Version = strings.Repeat("a", 40)
	change.Evidence = privatePath
	ref, err := (Tracker{Store: store}).Record(context.Background(), change, "Change greeting", "", false)
	if err != nil {
		t.Fatal(err)
	}
	item, err := store.Resolve(strings.TrimPrefix(ref, "todo:"))
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := item.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), home) || strings.Contains(string(encoded), privatePath) || item.Shape[0].Evidence != "shape-evidence:"+change.Version {
		t.Fatalf("private locator escaped into public item: %s", encoded)
	}
	got, err := ResolveShapeEvidence(home, item.Shape[0].Evidence)
	if err != nil {
		t.Fatal(err)
	}
	want := verifyevidence.Path(home, change.Version)
	if got != want {
		t.Fatalf("resolver differs from verifier location: %s, want %s", got, want)
	}
	for _, bad := range []string{"shape-evidence:../private", "shape-evidence:/private", "shape-evidence:a\\b", "/private/evidence.json"} {
		if _, err := ResolveShapeEvidence(home, bad); err == nil {
			t.Fatalf("unsafe locator accepted: %q", bad)
		}
	}
}
