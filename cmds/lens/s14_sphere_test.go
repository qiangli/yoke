package lenscmd

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/ref"
	"github.com/qiangli/yoke/pkg/spacegraph"
)

func TestSphereRecipeReadsLiveSpaceGraph(t *testing.T) {
	at := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	store := spacegraph.Open(t.TempDir())
	if err := store.Record(spacegraph.Observation{
		Src: "host:dragon", Dst: "host:novi", Rel: spacegraph.RelReached,
		OK: true, At: at, Via: "account:qiangli",
	}); err != nil {
		t.Fatal(err)
	}
	p := sphereRecipe(store, func() time.Time { return at.Add(time.Minute) })
	root := ref.Locator{Scheme: "sphere"}
	n, err := p.Stat(context.Background(), root)
	if err != nil || n.Children == nil || *n.Children != 3 {
		t.Fatalf("root=%+v err=%v", n, err)
	}
	rows, err := p.List(context.Background(), root)
	if err != nil || len(rows) != 3 {
		t.Fatalf("rows=%+v err=%v", rows, err)
	}
	edges, err := p.Edges(context.Background(), root)
	if err != nil || len(edges) != 1 {
		t.Fatalf("edges=%+v err=%v", edges, err)
	}
	if edges[0].Src != sphereRef("host:dragon") || edges[0].Dst != sphereRef("host:novi") || edges[0].Rel != "reached" {
		t.Fatalf("edge=%+v", edges[0])
	}
	child := ref.Locator{Scheme: "sphere", Target: "host:novi"}
	one, err := p.Stat(context.Background(), child)
	if err != nil || one.Ref != sphereRef("host:novi") || one.Kind != "host" {
		t.Fatalf("child=%+v err=%v", one, err)
	}
	if _, err := p.Stat(context.Background(), ref.Locator{Scheme: "sphere", Target: "host:missing"}); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing error=%v", err)
	}
}

func TestSphereRecipeRegistered(t *testing.T) {
	if providers["sphere"] == nil {
		t.Fatal("sphere provider is not registered")
	}
}
