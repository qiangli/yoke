package lenscmd

import (
	"encoding/json"
	"testing"

	"github.com/qiangli/yoke/pkg/lens/provider"
	"github.com/qiangli/yoke/pkg/lens/stream"
	"github.com/qiangli/yoke/pkg/ref"
)

func TestEveryAgentWorldDeltaEmitsBoundCursor(t *testing.T) {
	old, story, claim := AgentWorldRefs, StoryResolver, ClaimResolver
	AgentWorldRefs = ref.NewRegistry()
	t.Cleanup(func() { AgentWorldRefs, StoryResolver, ClaimResolver = old, story, claim })
	isolateRoom(t)
	for _, scheme := range provider.AgentWorldSchemes {
		target := scheme + ":item"
		if scheme == "bus" {
			target = "bus:events"
		} else {
			RegisterEntityResolver(scheme, ref.ResolverFunc(func(id string) (ref.Node, error) {
				return ref.NewNode(ref.Todo, id), nil
			}))
		}
		for _, since := range []string{"start", ""} {
			// Check the initial stream and the empty resumed stream alike.
			if since == "" {
				since = cursorFor(t, target)
			}
			out, stderr, code := runTool(t, t.TempDir(), "", "-t", "jsonl", "--since", since, target)
			if code != 0 {
				t.Fatalf("%s: %s", target, stderr)
			}
			_, raw := deltaCursor(t, out)
			var c stream.Cursor
			if err := json.Unmarshal([]byte(raw), &c); err != nil {
				t.Fatal(err)
			}
			if err := c.Validate(); err != nil {
				t.Fatal(err)
			}
			if err := c.Check(scheme, target, "delta:jsonl"); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func cursorFor(t *testing.T, target string) string {
	t.Helper()
	out, stderr, code := runTool(t, t.TempDir(), "", "-t", "jsonl", "--since=start", target)
	if code != 0 {
		t.Fatal(stderr)
	}
	_, cursor := deltaCursor(t, out)
	return cursor
}
