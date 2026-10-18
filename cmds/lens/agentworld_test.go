package lenscmd

import (
	"fmt"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/lens/provider"
	"github.com/qiangli/yoke/pkg/ref"
)

// wireFakeSprint installs a sprint resolver on AgentWorldRefs for the pull
// tests below. The lens test binary wires nothing itself, and no other test
// in this package touches AgentWorldRefs or the sprint scheme, so this
// registration cannot leak into unrelated tests.
func wireFakeSprint(t *testing.T) {
	t.Helper()
	AgentWorldRefs.Register(ref.Sprint, ref.ResolverFunc(func(id string) (ref.Node, error) {
		if id != "446" {
			return ref.Node{}, fmt.Errorf("sprint:%s: %w", id, ref.ErrNotFound)
		}
		n := ref.NewNode(ref.Sprint, "446")
		n.Title = "Agent-friendly common options + lens + adapt"
		n.Status = "doing"
		return n, nil
	}))
}

func TestAgentWorldSchemesRegistered(t *testing.T) {
	for _, s := range provider.AgentWorldSchemes {
		if _, ok := providers[s]; !ok {
			t.Errorf("scheme %q not registered", s)
		}
	}
	if _, ok := providers["dir"]; !ok {
		t.Error("dir provider lost its registration")
	}
}

func TestAgentWorldPullListing(t *testing.T) {
	wireFakeSprint(t)
	out, errb, code := runTool(t, t.TempDir(), "", "-t", "listing", "sprint:446")
	if code != 0 {
		t.Fatalf("code=%d err=%q", code, errb)
	}
	if !strings.Contains(out, `"ref":"sprint:446"`) || !strings.Contains(out, `"kind":"record"`) {
		t.Fatalf("out=%q", out)
	}
}

func TestAgentWorldPullPaths(t *testing.T) {
	wireFakeSprint(t)
	out, _, code := runTool(t, t.TempDir(), "", "-t", "paths", "sprint:446")
	if code != 0 || out != "sprint:446\n" {
		t.Fatalf("code=%d out=%q", code, out)
	}
}

func TestAgentWorldPullNotFound(t *testing.T) {
	wireFakeSprint(t)
	out, errb, code := runTool(t, t.TempDir(), "", "-t", "listing", "sprint:999")
	if code == 0 || out != "" || !strings.Contains(errb, "not found") {
		t.Fatalf("code=%d out=%q err=%q", code, out, errb)
	}
	if strings.Contains(errb, "unsupported") || strings.Contains(errb, "no resolver") {
		t.Fatalf("not-found misfiled: %q", errb)
	}
}

func TestAgentWorldStoryMissingResolver(t *testing.T) {
	old, oldStory := AgentWorldRefs, StoryResolver
	AgentWorldRefs, StoryResolver = ref.NewRegistry(), nil
	t.Cleanup(func() { AgentWorldRefs, StoryResolver = old, oldStory })
	out, errb, code := runTool(t, t.TempDir(), "", "-t", "listing", "story:abc")
	if code == 0 || out != "" || !strings.Contains(errb, "no resolver") {
		t.Fatalf("code=%d out=%q err=%q", code, out, errb)
	}
}

func TestAgentWorldFactoryScheme(t *testing.T) {
	wireFakeSprint(t)
	f, ok := providers["sprint"]
	if !ok {
		t.Fatal("sprint not registered")
	}
	p, err := f(nil, ref.Locator{Scheme: "sprint", Target: "446"})
	if err != nil {
		t.Fatal(err)
	}
	e, ok := p.(*provider.Entity)
	if !ok || e.Scheme != "sprint" {
		t.Fatalf("factory built %T", p)
	}
}
