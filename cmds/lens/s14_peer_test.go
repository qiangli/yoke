package lenscmd

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/lens"
	"github.com/qiangli/yoke/pkg/ref"
)

func TestPeerCaptureBoundsOutput(t *testing.T) {
	var capture peerCapture
	input := bytes.Repeat([]byte("x"), peerMaxOutput+7)
	n, err := capture.Write(input)
	if err != nil || n != len(input) || !capture.overflow || capture.Len() != peerMaxOutput {
		t.Fatalf("write n=%d err=%v overflow=%t retained=%d", n, err, capture.overflow, capture.Len())
	}
}

// peerFixture mirrors `outpost peers status --json`: one row per peer with
// host, alias, ownership, reachability and platform fields.
const peerFixture = `[
  {"host":"dragon","owned":true,"online":true,"location":"same_lan","version":"e3788fb","commit":"e3788fb","os":"darwin","arch":"arm64","os_version":"macOS 26.5.1","update_mode":"auto"},
  {"host":"lern","owned":true,"online":true,"location":"remote","version":"v0.32.0-dev","commit":"ca9a85b","os":"linux","arch":"amd64","os_version":"Ubuntu 26.04 LTS","update_mode":"auto"},
  {"host":"lj2ivy","owned":false,"shared":true,"online":false,"location":"remote","version":"v0.32.0-dev","commit":"ca9a85b","os":"windows","arch":"amd64","os_version":"Microsoft Windows 10","update_mode":"auto"},
  {"host":"novicortex","alias":"monkey","owned":true,"online":false,"location":"remote","version":"v0.14.27","commit":"d1ddfb4","os":"darwin","arch":"arm64","os_version":"macOS 26.2","update_mode":"auto"},
  {"host":"","owned":true,"online":true}
]`

// withPeerJSON injects canned outpost output; no live peer is contacted.
func withPeerJSON(t *testing.T, body string, err error) {
	t.Helper()
	prev := peerRunner
	peerRunner = func(context.Context) ([]byte, error) {
		if err != nil {
			return nil, err
		}
		return []byte(body), nil
	}
	t.Cleanup(func() { peerRunner = prev })
}

func mustParsePeerLocator(t *testing.T, s string) ref.Locator {
	t.Helper()
	loc, err := ref.ParseLocator(s)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return loc
}

func TestPeerProviderRegistered(t *testing.T) {
	if providers["peer"] == nil {
		t.Fatal("peer provider is not registered")
	}
}

func TestPeerRecipeListStatEdges(t *testing.T) {
	withPeerJSON(t, peerFixture, nil)
	ctx := context.Background()
	p := peerRecipe()

	rows, err := p.List(ctx, mustParsePeerLocator(t, "peer:"))
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 4 {
		t.Fatalf("list rows=%d, want 4 (hostless row dropped)", len(rows))
	}
	wantRefs := []string{"peer:dragon", "peer:lern", "peer:lj2ivy", "peer:novicortex"}
	for i, want := range wantRefs {
		if rows[i].Ref != want {
			t.Fatalf("row %d ref=%q, want %q", i, rows[i].Ref, want)
		}
		if rows[i].SchemaVersion != "bashy-node-v1" || rows[i].Kind == "" || rows[i].Type == "" || rows[i].Attrs == nil {
			t.Fatalf("row %d is not a canonical node: %+v", i, rows[i])
		}
	}
	if rows[0].Attrs["os"] != "darwin" || rows[0].Attrs["online"] != true {
		t.Fatalf("dragon attrs lost board fields: %+v", rows[0].Attrs)
	}
	if rows[3].Attrs["alias"] != "monkey" {
		t.Fatalf("novicortex attrs lost alias: %+v", rows[3].Attrs)
	}
	if _, ok := rows[1].Attrs["alias"]; ok {
		t.Fatalf("lern attrs carry an alias it does not have: %+v", rows[1].Attrs)
	}

	root, err := p.Stat(ctx, mustParsePeerLocator(t, "peer:"))
	if err != nil {
		t.Fatalf("stat root: %v", err)
	}
	if root.Ref != "peer:" || root.Kind != "directory" || root.Children == nil || *root.Children != 4 {
		t.Fatalf("bad root node: %+v", root)
	}

	one, err := p.Stat(ctx, mustParsePeerLocator(t, "peer:novicortex"))
	if err != nil {
		t.Fatalf("stat peer: %v", err)
	}
	if one.Ref != "peer:novicortex" || one.Name != "novicortex" || one.Kind != "peer" {
		t.Fatalf("bad peer node: %+v", one)
	}

	aliased, err := p.Stat(ctx, mustParsePeerLocator(t, "peer:monkey"))
	if err != nil {
		t.Fatalf("stat alias: %v", err)
	}
	if aliased.Ref != "peer:novicortex" {
		t.Fatalf("alias did not resolve canonically: %+v", aliased)
	}

	if _, err := p.Stat(ctx, mustParsePeerLocator(t, "peer:ghost")); err == nil {
		t.Fatal("stat of unknown peer succeeded")
	}

	leaf, err := p.List(ctx, mustParsePeerLocator(t, "peer:dragon"))
	if err != nil || len(leaf) != 0 {
		t.Fatalf("leaf list=%v err=%v, want empty", leaf, err)
	}

	edges, err := p.Edges(ctx, mustParsePeerLocator(t, "peer:"))
	if err != nil {
		t.Fatalf("edges: %v", err)
	}
	if len(edges) != 4 {
		t.Fatalf("edges=%d, want 4", len(edges))
	}
	for _, e := range edges {
		if e.SchemaVersion != "bashy-edge-v1" || e.Src != "peer:" || e.Rel != "contains" || !strings.HasPrefix(e.Dst, "peer:") || e.Attrs == nil {
			t.Fatalf("bad edge: %+v", e)
		}
	}

	if _, err := p.Open(ctx, mustParsePeerLocator(t, "peer:")); err == nil {
		t.Fatal("open unexpectedly supported")
	}
}

func TestPeerRecipeRunnerErrors(t *testing.T) {
	ctx := context.Background()
	p := peerRecipe()

	withPeerJSON(t, "", context.DeadlineExceeded)
	if _, err := p.List(ctx, mustParsePeerLocator(t, "peer:")); err == nil {
		t.Fatal("list hid the runner error")
	}

	withPeerJSON(t, "not json", nil)
	if _, err := p.List(ctx, mustParsePeerLocator(t, "peer:")); err == nil {
		t.Fatal("list accepted malformed board JSON")
	}
}

func TestPeerThroughLens(t *testing.T) {
	withPeerJSON(t, peerFixture, nil)
	dir := t.TempDir()

	out, errb, code := runTool(t, dir, "", "-t", "paths", "peer:")
	if code != 0 || errb != "" {
		t.Fatalf("paths peer:: %d %q", code, errb)
	}
	wantPaths := "peer:\npeer:dragon\npeer:lern\npeer:lj2ivy\npeer:novicortex\n"
	if out != wantPaths {
		t.Fatalf("paths peer:\n%s\nwant:\n%s", out, wantPaths)
	}

	out, errb, code = runTool(t, dir, "", "-t", "paths", "peer:monkey")
	if code != 0 || errb != "" || out != "peer:novicortex\n" {
		t.Fatalf("paths peer:monkey: %d %q out=%q", code, errb, out)
	}

	produced, errb, code := runTool(t, dir, "", "-t", "graph", "peer:")
	if code != 0 || errb != "" {
		t.Fatalf("produce graph: %d %q", code, errb)
	}
	for _, view := range lens.GraphViews {
		pulled, _, c1 := runTool(t, dir, "", "-t", view, "peer:")
		pushed, _, c2 := runTool(t, dir, produced, "-t", view)
		if c1 != 0 || c2 != 0 || pulled != pushed {
			t.Errorf("view %s: pull/push differ (c1=%d c2=%d)\npulled:\n%s\npushed:\n%s", view, c1, c2, pulled, pushed)
		}
	}

	_, errb, code = runTool(t, dir, "", "-t", "paths", "peer:ghost")
	if code == 0 {
		t.Fatal("unknown peer unexpectedly listed")
	}
	if errb == "" {
		t.Fatal("unknown peer gave no diagnostic")
	}
}
