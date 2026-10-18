package weave

import (
	"bytes"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/lens/stream"
)

// weave status N --graph is the run's relations as a bashy-graph-v1 stream;
// nothing clock- or machine-dependent enters it, so it is byte-stable.
func TestWeaveStatusGraphStream(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	dir := weaveTestRepo(t)
	t.Chdir(dir)
	for _, title := range []string{"base work", "story work", "split child"} {
		if out, code := runWeave(t, "add", title); code != 0 {
			t.Fatalf("add %q: %d %s", title, code, out)
		}
	}
	qdir, err := weaveQueueDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	q, err := loadWeaveQueue(qdir)
	if err != nil {
		t.Fatal(err)
	}
	two := findWeaveItem(q, 2)
	two.DependsOn = []int64{1}
	two.Branch = "agent/weave-issue-2"
	two.WorkerStories = []weaveWorkerStory{{Sprint: 446, ID: "3e8962ee2915", Seq: 2}}
	findWeaveItem(q, 3).Parent = 2
	if err := saveWeaveQueue(qdir, q); err != nil {
		t.Fatal(err)
	}

	status := func(args ...string) string {
		t.Helper()
		cmd := newWeaveStatusCmd()
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetErr(&out)
		cmd.SetArgs(args)
		if err := cmd.Execute(); err != nil {
			t.Fatalf("status %v: %v\n%s", args, err, out.String())
		}
		return out.String()
	}
	out := status("2", "--graph")
	if again := status("2", "--graph"); again != out {
		t.Fatalf("not deterministic:\n%s\n---\n%s", out, again)
	}
	g, err := stream.Decode(strings.NewReader(out), stream.DecodeOptions{})
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if err := g.Verify(); err != nil {
		t.Fatal(err)
	}
	if g.Source != "run:2" || g.Cursor.Binding.Provider != "weave" || g.Cursor.Binding.Target != "run:2" || g.Cursor.Binding.Query != "op=status" {
		t.Fatalf("binding: %q %+v", g.Source, g.Cursor.Binding)
	}
	var edges []string
	for _, e := range g.Edges {
		edges = append(edges, e.Src+" "+e.Rel+" "+e.Dst)
	}
	if want := "run:2 contains run:3,run:2 depends-on run:1,run:2 implements story:3e8962ee2915"; strings.Join(edges, ",") != want {
		t.Fatalf("edges:\n%v\nwant %s", edges, want)
	}
	for _, n := range g.Nodes {
		if n.Ref == "run:2" && (n.Attrs["branch"] != "agent/weave-issue-2" || n.Attrs["state"] == nil || n.Attrs["title"] != "story work") {
			t.Fatalf("subject attrs: %+v", n.Attrs)
		}
	}
	for _, leak := range []string{"health", "pid", dir} {
		if strings.Contains(out, leak) {
			t.Fatalf("%q leaked into the stream:\n%s", leak, out)
		}
	}
	// The classic text is untouched by the new flag.
	if txt := status("2"); !strings.HasPrefix(txt, "run #2 — story work\n") {
		t.Fatalf("classic status changed:\n%s", txt)
	}
}
