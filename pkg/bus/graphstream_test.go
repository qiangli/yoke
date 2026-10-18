package bus

import (
	"bytes"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/lens/stream"
)

func runSubscriptions(t *testing.T, args ...string) (string, string, error) {
	t.Helper()
	cmd := newSubscriptionsCmd()
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), errOut.String(), err
}

// `bus subscriptions --graph` is the subscription registry as a
// bashy-graph-v1 stream; the sidecar's moving read offset does not change it.
func TestSubscriptionsGraphStream(t *testing.T) {
	isolate(t)
	subscribe(t, Subscription{Subscriber: "beta", Topics: []string{"code.api.*"}, Since: 7})
	subscribe(t, Subscription{Subscriber: "alpha", Topics: []string{"code.api.*", "sprint.446"}, Kinds: []string{"notify"},
		To: "alpha", Room: "ops", InterruptFrom: []string{"operator"}, Instance: "m1", Sink: "exec", SinkArgs: []string{"/bin/secret-path"}})

	out, _, err := runSubscriptions(t, "--graph")
	if err != nil {
		t.Fatal(err)
	}
	g, err := stream.Decode(strings.NewReader(out), stream.DecodeOptions{})
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if err := g.Verify(); err != nil {
		t.Fatal(err)
	}
	if g.Source != "bus:subscriptions" || g.Cursor.Binding.Provider != "bus" || g.Cursor.Binding.Query != "op=subscriptions" {
		t.Fatalf("binding: %q %+v", g.Source, g.Cursor.Binding)
	}
	edges := map[string]bool{}
	for _, e := range g.Edges {
		edges[e.Src+" "+e.Rel+" "+e.Dst] = true
	}
	for _, want := range []string{
		"bus:subscriptions contains bus:subscriptions!/subscriber!/alpha",
		"bus:subscriptions!/subscriber!/alpha subscribes bus:subscriptions!/topic!/code.api.*",
		"bus:subscriptions!/subscriber!/beta subscribes bus:subscriptions!/topic!/code.api.*",
		"bus:subscriptions!/subscriber!/alpha filters bus:subscriptions!/kind!/notify",
		"bus:subscriptions!/subscriber!/alpha listens bus:subscriptions!/room!/ops",
		"bus:subscriptions!/subscriber!/alpha receives bus:subscriptions!/address!/alpha",
		"bus:subscriptions!/principal!/operator may-interrupt bus:subscriptions!/subscriber!/alpha",
	} {
		if !edges[want] {
			t.Errorf("missing edge %q in\n%s", want, out)
		}
	}
	if strings.Contains(out, "secret-path") || strings.Contains(out, `"since"`) {
		t.Fatalf("sink argv or read offset leaked into the stream:\n%s", out)
	}

	// The read offset moves on every sidecar poll; the stream must not.
	subscribe(t, Subscription{Subscriber: "beta", Topics: []string{"code.api.*"}, Since: 99})
	if again, _, _ := runSubscriptions(t, "--graph"); again != out {
		t.Fatalf("read offset changed the stream:\n%s\n---\n%s", out, again)
	}

	// The native --json envelope and the table are unchanged.
	if j, _, _ := runSubscriptions(t, "--json"); !strings.Contains(j, `"subscriptions"`) || strings.Contains(j, `"t":`) {
		t.Fatalf("--json envelope changed:\n%s", j)
	}
	if txt, _, _ := runSubscriptions(t); !strings.HasPrefix(txt, "SUBSCRIBER") {
		t.Fatalf("table changed:\n%s", txt)
	}
	if _, _, err := runSubscriptions(t, "--graph", "--json"); err == nil {
		t.Fatal("--graph with --json must be refused")
	}
}

// No subscriptions is still a complete stream: the registry node alone.
func TestSubscriptionsGraphEmpty(t *testing.T) {
	isolate(t)
	out, _, err := runSubscriptions(t, "--graph")
	if err != nil {
		t.Fatal(err)
	}
	g, err := stream.Decode(strings.NewReader(out), stream.DecodeOptions{})
	if err != nil || len(g.Nodes) != 1 || len(g.Edges) != 0 {
		t.Fatalf("%v\n%s", err, out)
	}
}
