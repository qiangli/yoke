package verify

import (
	"strings"
	"testing"
)

const appTree = "shaped.json"

func surfaceOf(t *testing.T, files map[string]string) Surface {
	t.Helper()
	s, err := ReadSurface(tree(t, files))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func baseFiles() map[string]string {
	return map[string]string{
		appTree:             `{"properties":["title","body"]}`,
		"api/list.md":       "---\nflags: [--all]\nargs: [limit]\n---\n# list\n",
		"public/index.html": "<h1>x</h1>",
	}
}

func TestReadSurface(t *testing.T) {
	s := surfaceOf(t, baseFiles())
	for _, k := range []string{"property:title", "property:body", "unit:list", "route:/api/list",
		"route:/index.html", "flag:list --all", "argument:list limit"} {
		if !s[k] {
			t.Errorf("surface lacks %q: %v", k, s)
		}
	}
}

func TestCompareSurface(t *testing.T) {
	base := surfaceOf(t, baseFiles())

	t.Run("rename fails", func(t *testing.T) {
		f := baseFiles()
		f[appTree] = `{"properties":["heading","body"]}`
		c := CompareSurface(base, surfaceOf(t, f), nil)
		if !c.Failed() || len(c.Removed) != 1 || c.Removed[0] != "property:title" {
			t.Fatalf("compat = %+v", c)
		}
		if d := c.Detail(); !strings.Contains(d, "property:title") || !strings.Contains(d, "property:heading") {
			t.Fatalf("detail does not name the rename: %s", d)
		}
	})
	t.Run("remove fails for each kind", func(t *testing.T) {
		f := baseFiles()
		f[appTree] = `{"properties":["title"]}`
		f["api/list.md"] = "---\nflags: []\n---\n# list\n"
		delete(f, "public/index.html")
		c := CompareSurface(base, surfaceOf(t, f), nil)
		want := "argument:list limit,flag:list --all,property:body,route:/index.html"
		if !c.Failed() || strings.Join(c.Removed, ",") != want {
			t.Fatalf("removed = %v, want %s", c.Removed, want)
		}
		delete(f, "api/list.md")
		c = CompareSurface(base, surfaceOf(t, f), nil)
		if !strings.Contains(strings.Join(c.Removed, ","), "unit:list") {
			t.Fatalf("unit removal not caught: %v", c.Removed)
		}
	})
	t.Run("add passes", func(t *testing.T) {
		f := baseFiles()
		f[appTree] = `{"properties":["title","body","heading"]}`
		f["api/new.md"] = "# new\n"
		c := CompareSurface(base, surfaceOf(t, f), nil)
		if c.Failed() || len(c.Added) == 0 {
			t.Fatalf("compat = %+v", c)
		}
		if c.Result().Status != "pass" {
			t.Fatalf("result = %+v", c.Result())
		}
	})
	t.Run("approved contract step may remove", func(t *testing.T) {
		f := baseFiles()
		f[appTree] = `{"properties":["title"]}`
		cand := surfaceOf(t, f)
		if c := CompareSurface(base, cand, nil); !c.Failed() {
			t.Fatal("unapproved removal passed")
		}
		c := CompareSurface(base, cand, []string{"property:body"})
		if c.Failed() || len(c.Contracted) != 1 {
			t.Fatalf("approved removal failed: %+v", c)
		}
	})
}
