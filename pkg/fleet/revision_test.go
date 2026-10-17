package fleet

import (
	"os"
	"path/filepath"
	"testing"
	"testing/fstest"

	"github.com/qiangli/yoke/pkg/assetring"
)

func TestRevisionTracksEveryRingAndContent(t *testing.T) {
	root := t.TempDir()
	shared := fstest.MapFS{"shared.yaml": &fstest.MapFile{Data: []byte("name: shared\n")}}
	cloud := fstest.MapFS{"cloud.yaml": &fstest.MapFile{Data: []byte("name: cloud\n")}}
	cat := New(WithRoot(root), WithBaselineFS(fstest.MapFS{}), WithoutCloudOverlay(),
		WithSource("models", assetring.FileFS(shared, assetring.RingShared, ".yaml")),
		WithSource("agents", assetring.FileFS(cloud, assetring.RingCloud, ".yaml")))
	revision := func() [32]byte {
		t.Helper()
		r, err := cat.Revision()
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	previous := revision()
	changed := func() {
		t.Helper()
		next := revision()
		if next == previous {
			t.Fatal("ring edit left revision unchanged")
		}
		previous = next
	}
	shared["shared.yaml"].Data = []byte("name: edited\n")
	changed()
	delete(cloud, "cloud.yaml")
	changed()
	if err := cat.SaveModel(Model{Name: "local", Kind: ModelKindSubscription}); err != nil {
		t.Fatal(err)
	}
	changed()
	path := filepath.Join(root, "models", "local.yaml")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(body, []byte("# changed\n")...), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	changed()
	if next := revision(); next != previous {
		t.Fatal("unchanged rings changed revision")
	}
}
