package lenscmd

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/issue"
	"github.com/qiangli/yoke/pkg/policy/coord"
	"github.com/qiangli/yoke/pkg/principal"
	"github.com/qiangli/yoke/pkg/ref"
	"github.com/qiangli/yoke/pkg/todo"
)

func TestEntityDeltaRevisions(t *testing.T) {
	for _, scheme := range []string{"story", "claim", "agent", "mb", "meet", "kb", "host"} {
		t.Run(scheme, func(t *testing.T) {
			oldRegistry, oldStory, oldClaim := AgentWorldRefs, StoryResolver, ClaimResolver
			AgentWorldRefs = ref.NewRegistry()
			t.Cleanup(func() { AgentWorldRefs, StoryResolver, ClaimResolver = oldRegistry, oldStory, oldClaim })
			n := ref.NewNode(ref.Kind(scheme), "stable-id")
			n.UID, n.Seq, n.Title, n.Status = "stable-uid", 2134, "Original title", "assigned"
			if scheme == "claim" {
				n.UID = "epoch:1:rev:1"
			}
			RegisterEntityResolver(scheme, ref.ResolverFunc(func(string) (ref.Node, error) { return n, nil }))
			dir, locator := t.TempDir(), scheme+":stable-id"
			poll := func(since string) (string, string) {
				t.Helper()
				out, stderr, code := runTool(t, dir, "", "--since", since, "-t", "jsonl", locator)
				if code != 0 || stderr != "" {
					t.Fatalf("poll: code=%d stderr=%s", code, stderr)
				}
				return deltaCursor(t, out)
			}
			data, cursor := poll("start")
			if !strings.Contains(data, "Original title") {
				t.Fatalf("initial data: %s", data)
			}
			if data, _ := poll(cursor); data != "" {
				t.Fatalf("unchanged data: %s", data)
			}
			for _, change := range []func(){
				func() { n.Title = "Changed title" },
				func() { n.Status = "done" },
				func() { n.UID = "epoch:1:rev:2" },
			} {
				change()
				data, next := poll(cursor)
				if data == "" || cursor == next {
					t.Fatalf("change lost: data=%q cursor=%s", data, next)
				}
				if data, same := poll(next); data != "" || same != next {
					t.Fatalf("unchanged revision replayed: %q", data)
				}
				cursor = next
			}
			for _, since := range []string{"10m", "2026-10-18T00:00:00Z", "2134"} {
				out, stderr, code := runTool(t, dir, "", "--since", since, "-t", "jsonl", locator)
				if code == 0 || out != "" || !strings.Contains(stderr, "does not support") {
					t.Fatalf("unsupported %s: code=%d out=%q stderr=%q", since, code, out, stderr)
				}
			}
		})
	}
}

func TestEntityDeltaRejectsLegacyTimeAndIdentityTokens(t *testing.T) {
	old := StoryResolver
	t.Cleanup(func() { StoryResolver = old })
	StoryResolver = ref.ResolverFunc(func(id string) (ref.Node, error) { return ref.NewNode(ref.Todo, id), nil })
	src := &entityDeltaSource{scheme: "story"}
	for _, token := range []string{"2026-10-18T00:00:00Z", "13778c0d7a83", "2134"} {
		data, _, _, err := src.Delta(t.Context(), ref.Locator{Scheme: "story", Target: "2134"}, token)
		if err == nil || len(data) != 0 || !strings.Contains(err.Error(), "restart") {
			t.Fatalf("token %q: %s %v", token, data, err)
		}
	}
	StoryResolver = ref.ResolverFunc(func(string) (ref.Node, error) { return ref.Node{}, fmt.Errorf("store unreadable") })
	if _, _, _, err := src.Delta(t.Context(), ref.Locator{Scheme: "story", Target: "2134"}, ""); err == nil || err.Error() != "store unreadable" {
		t.Fatalf("lost resolver error: %v", err)
	}
}

func TestEntityDeltaStoryStoreEdit(t *testing.T) {
	oldRegistry, oldStory := AgentWorldRefs, StoryResolver
	AgentWorldRefs, StoryResolver = ref.NewRegistry(), nil
	t.Cleanup(func() { AgentWorldRefs, StoryResolver = oldRegistry, oldStory })
	root := t.TempDir()
	st := &issue.Store{Root: root, Sub: "docs/todo"}
	item := &issue.Issue{ID: "13778c0d7a83", Seq: 2134, Title: "Original", Status: "todo", Kind: issue.KindTask}
	if _, err := st.Save(item); err != nil {
		t.Fatal(err)
	}
	todo.RegisterRefs(AgentWorldRefs, "", true, false, root, nil)
	src, loc := &entityDeltaSource{scheme: "story"}, ref.Locator{Scheme: "story", Target: "2134"}
	_, token, _, err := src.Delta(t.Context(), loc, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(){func() { item.Title = "Edited" }, func() { item.Status = "done" }} {
		change()
		if _, err := st.Save(item); err != nil {
			t.Fatal(err)
		}
		data, next, _, err := src.Delta(t.Context(), loc, token)
		if err != nil || len(data) == 0 || token == next {
			t.Fatalf("store edit: %s %v", data, err)
		}
		data, _, _, err = src.Delta(t.Context(), loc, next)
		if err != nil || len(data) != 0 {
			t.Fatalf("store replay: %s %v", data, err)
		}
		token = next
	}
}

func TestEntityDeltaClaimStoreRefresh(t *testing.T) {
	t.Setenv("BASHY_COORD_DIR", t.TempDir())
	old := ClaimResolver
	ClaimResolver = nil
	t.Cleanup(func() { ClaimResolver = old })
	holder := principal.Ref{Kind: principal.KindAgent, Name: "lens-test"}
	claim, err := coord.AcquireResource(coord.DefaultDir(), "lens-resource", holder, "test", false)
	if err != nil {
		t.Fatal(err)
	}
	src, loc := &entityDeltaSource{scheme: "claim"}, ref.Locator{Scheme: "claim", Target: "lens-resource"}
	_, token, _, err := src.Delta(t.Context(), loc, "")
	if err != nil {
		t.Fatal(err)
	}
	if data, _, _, err := src.Delta(t.Context(), loc, token); err != nil || len(data) != 0 {
		t.Fatalf("unchanged claim: %s %v", data, err)
	}
	grant, err := coord.Refresh(t.Context(), claim.Ref(), holder, claim.Epoch)
	if err != nil {
		t.Fatal(err)
	}
	if grant.Epoch != claim.Epoch || grant.Claim.Rev == claim.Rev {
		t.Fatal("fixture did not refresh within same epoch")
	}
	data, next, _, err := src.Delta(t.Context(), loc, token)
	if err != nil || len(data) == 0 || next == token {
		t.Fatalf("claim refresh: %s %v", data, err)
	}
	if data, _, _, err := src.Delta(t.Context(), loc, next); err != nil || len(data) != 0 {
		t.Fatalf("unchanged refresh: %s %v", data, err)
	}
}

func TestEntityDeltaStoryRequiresResolver(t *testing.T) {
	oldRegistry, old := AgentWorldRefs, StoryResolver
	AgentWorldRefs, StoryResolver = ref.NewRegistry(), nil
	t.Cleanup(func() { AgentWorldRefs, StoryResolver = oldRegistry, old })
	_, _, _, err := (&entityDeltaSource{scheme: "story"}).Delta(t.Context(), ref.Locator{Scheme: "story", Target: "2134"}, "")
	if !errors.Is(err, ref.ErrNoResolver) {
		t.Fatalf("unwired story: %v", err)
	}
}

func TestBusTimelineRejectsHumanSelectors(t *testing.T) {
	for _, since := range []string{"10m", "2026-10-18T00:00:00Z", "42"} {
		out, stderr, code := runTool(t, t.TempDir(), "", "--since", since, "-t", "jsonl", "bus:timeline")
		if code == 0 || out != "" || !strings.Contains(stderr, "does not support") {
			t.Fatalf("selector %s: code=%d out=%q stderr=%q", since, code, out, stderr)
		}
	}
}
