package dblifecycle

import (
	"path/filepath"
	"testing"

	"github.com/qiangli/yoke/pkg/dbstore"
	"github.com/qiangli/yoke/pkg/ref"
)

func TestParseTargetUsesSharedLocator(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a:b@c.db")
	raw := (ref.Locator{Scheme: "db", Target: "sqlite:file:" + path}).String()
	got, err := ParseTarget(raw)
	if err != nil || got.Engine != "sqlite" || got.Mode != dbstore.Persistent || got.Path != path {
		t.Fatalf("parse %q = %+v, %v", raw, got, err)
	}
	for _, invalid := range []string{"db:sqlite:file:" + path, "db:sqlite%3Aremote%3Ahost", raw + "@peer", raw + "!/inner"} {
		if _, err := ParseTarget(invalid); err == nil {
			t.Errorf("accepted %q", invalid)
		}
	}
}
