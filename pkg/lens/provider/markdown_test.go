package provider

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/ref"
)

var sampleMD = strings.Join([]string{
	"# Intro", "## Details", "Some prose.", "### Step one", "### Step two",
	"## Results", "More prose.", "# Intro", mdFence, "## Fenced", mdFence,
	"", "## Colons are fine", "",
}, "\n")

func writeMD(t *testing.T, body string) (*Markdown, string) {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "doc.md"), []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
	return NewMarkdown(root), "doc.md"
}

func TestMarkdownDocumentStat(t *testing.T) {
	ctx := context.Background()
	m, _ := writeMD(t, sampleMD)
	r := NewRegistry()
	if err := r.Register("md", m); err != nil {
		t.Fatal(err)
	}
	loc := mustParse(t, r, "md:doc.md")
	n, err := m.Stat(ctx, loc)
	if err != nil {
		t.Fatal(err)
	}
	if n.SchemaVersion != NodeSchema || n.Ref != "md:doc.md" {
		t.Fatalf("node=%+v", n)
	}
	if n.Kind != "directory" || n.Type != "text/markdown" {
		t.Fatalf("kind=%q type=%q", n.Kind, n.Type)
	}
	if n.Children == nil || *n.Children != 2 {
		t.Fatalf("children=%v", n.Children)
	}
	if n.Attrs["headings"] != 7 {
		t.Fatalf("attrs=%+v", n.Attrs)
	}
	if n.Size == nil || *n.Size != int64(len(sampleMD)) {
		t.Fatalf("size=%v", n.Size)
	}
	b, err := json.Marshal(n)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatal(err)
	}
	if raw["schema_version"] != NodeSchema || raw["attrs"] == nil {
		t.Fatalf("json=%s", b)
	}
}

func mustParse(t *testing.T, r *Registry, s string) ref.Locator {
	t.Helper()
	loc, err := r.Parse(s)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return loc
}

func TestMarkdownRootListPreservesOrderAndDuplicates(t *testing.T) {
	ctx := context.Background()
	m, _ := writeMD(t, sampleMD)
	r := NewRegistry()
	if err := r.Register("md", m); err != nil {
		t.Fatal(err)
	}
	rows, err := m.List(ctx, mustParse(t, r, "md:doc.md"))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("len=%d rows=%+v", len(rows), rows)
	}
	wantNames := []string{"Intro", "Intro"}
	wantRefs := []string{
		"md:doc.md!/intro#1",
		"md:doc.md!/intro#2",
	}
	for i := range rows {
		if rows[i].Name != wantNames[i] || rows[i].Ref != wantRefs[i] {
			t.Errorf("rows[%d]=%q %s want %q %s", i, rows[i].Name, rows[i].Ref, wantNames[i], wantRefs[i])
		}
		if rows[i].Kind != "heading" || rows[i].Type != "text/markdown" {
			t.Errorf("rows[%d] kind=%q type=%q", i, rows[i].Kind, rows[i].Type)
		}
		if rows[i].Attrs == nil {
			t.Errorf("rows[%d] attrs missing", i)
		}
	}
	if rows[0].Children == nil || *rows[0].Children != 2 { // Details, Results
		t.Fatalf("rows[0].children=%v", rows[0].Children)
	}
	if rows[1].Children == nil || *rows[1].Children != 1 { // Colons are fine
		t.Fatalf("rows[1].children=%v", rows[1].Children)
	}
	if rows[0].Ref == rows[1].Ref {
		t.Fatalf("duplicate headings collapsed: %s", rows[0].Ref)
	}
	if got, _ := rows[0].Attrs["occurrence"].(int); got != 1 {
		t.Fatalf("occurrence=%v", rows[0].Attrs["occurrence"])
	}
	if got, _ := rows[1].Attrs["occurrence"].(int); got != 2 {
		t.Fatalf("occurrence=%v", rows[1].Attrs["occurrence"])
	}
	if _, err := json.Marshal(rows[0]); err != nil {
		t.Fatal(err)
	}
}

func TestMarkdownNestedLevels(t *testing.T) {
	ctx := context.Background()
	m, _ := writeMD(t, sampleMD)
	r := NewRegistry()
	if err := r.Register("md", m); err != nil {
		t.Fatal(err)
	}
	details := mustParse(t, r, "md:doc.md!/intro#1/details#1")
	rows, err := m.List(ctx, details)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].Name != "Step one" || rows[1].Name != "Step two" {
		t.Fatalf("rows=%+v", rows)
	}
	if got, _ := rows[0].Attrs["level"].(int); got != 3 {
		t.Fatalf("level=%v", rows[0].Attrs["level"])
	}
	if got, _ := rows[0].Attrs["line"].(int); got != 4 {
		t.Fatalf("line=%v", rows[0].Attrs["line"])
	}
	if !strings.HasPrefix(rows[0].Ref, "md:doc.md!/intro#1/details#1/") {
		t.Fatalf("child ref=%s", rows[0].Ref)
	}
	stepTwo := mustParse(t, r, "md:doc.md!/intro#1/details#1/step-two#1")
	n, err := m.Stat(ctx, stepTwo)
	if err != nil {
		t.Fatal(err)
	}
	if n.Kind != "heading" || n.Path != "intro#1/details#1/step-two#1" {
		t.Fatalf("node=%+v", n)
	}
	if leaf, err := m.List(ctx, stepTwo); err != nil || len(leaf) != 0 {
		t.Fatalf("leaf list=%+v err=%v", leaf, err)
	}
	intro2 := mustParse(t, r, "md:doc.md!/intro#2")
	if n, err := m.Stat(ctx, intro2); err != nil || n.Kind != "heading" {
		t.Fatalf("second intro: %+v err=%v", n, err)
	}
}

func TestMarkdownEdgesMatchList(t *testing.T) {
	ctx := context.Background()
	m, _ := writeMD(t, sampleMD)
	r := NewRegistry()
	if err := r.Register("md", m); err != nil {
		t.Fatal(err)
	}
	loc := mustParse(t, r, "md:doc.md")
	edges, err := m.Edges(ctx, loc)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := m.List(ctx, loc)
	if err != nil {
		t.Fatal(err)
	}
	if len(edges) != len(rows) {
		t.Fatalf("edges=%d rows=%d", len(edges), len(rows))
	}
	for i := range edges {
		if edges[i].SchemaVersion != EdgeSchema || edges[i].Rel != "contains" {
			t.Fatalf("edge=%+v", edges[i])
		}
		if edges[i].Src != loc.String() || edges[i].Dst != rows[i].Ref {
			t.Fatalf("edge=%+v row=%+v", edges[i], rows[i])
		}
		if edges[i].Attrs == nil {
			t.Fatalf("edge attrs missing: %+v", edges[i])
		}
	}
	nested := mustParse(t, r, "md:doc.md!/intro#1/details#1")
	nestedEdges, err := m.Edges(ctx, nested)
	if err != nil || len(nestedEdges) != 2 {
		t.Fatalf("nested edges=%+v err=%v", nestedEdges, err)
	}
	if nestedEdges[0].Src != "md:doc.md!/intro#1/details#1" {
		t.Fatalf("nested src=%s", nestedEdges[0].Src)
	}
}

func TestMarkdownFencedCodeIgnored(t *testing.T) {
	ctx := context.Background()
	m, _ := writeMD(t, sampleMD)
	r := NewRegistry()
	if err := r.Register("md", m); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Stat(ctx, mustParse(t, r, "md:doc.md!/fenced#1")); err == nil {
		t.Fatal("fenced heading was indexed")
	}
}

func TestMarkdownClosingHashesAndSetext(t *testing.T) {
	body := "## Closed ##\n\nSetext Title\n===========\n\n#### level4 #####\n\n####### seven\n"
	m, _ := writeMD(t, body)
	ctx := context.Background()
	r := NewRegistry()
	if err := r.Register("md", m); err != nil {
		t.Fatal(err)
	}
	rows, err := m.List(ctx, mustParse(t, r, "md:doc.md"))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Name != "Closed" {
		t.Fatalf("rows=%+v", rows)
	}
	if rows[0].Children == nil || *rows[0].Children != 1 {
		t.Fatalf("closed children=%v", rows[0].Children)
	}
	closed := ref.Locator{Scheme: "md", Target: "doc.md", Inner: []string{"closed#1"}}
	nested, err := m.List(ctx, closed)
	if err != nil {
		t.Fatal(err)
	}
	if len(nested) != 1 || nested[0].Name != "level4" {
		t.Fatalf("nested=%+v", nested)
	}
	if got, _ := nested[0].Attrs["level"].(int); got != 4 {
		t.Fatalf("level=%v", nested[0].Attrs["level"])
	}
}

func TestMarkdownEscapedInnerPath(t *testing.T) {
	ctx := context.Background()
	m, _ := writeMD(t, sampleMD)
	r := NewRegistry()
	if err := r.Register("md", m); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Parse("md:doc.md!/intro"); err != nil {
		t.Fatalf("parse inner: %v", err)
	}
	loc, err := ref.ParseLocator("md:doc.md!/intro%231")
	if err != nil {
		t.Fatal(err)
	}
	if loc.Scheme != "md" || loc.Target != "doc.md" || len(loc.Inner) != 1 || loc.Inner[0] != "intro#1" {
		t.Fatalf("loc=%+v", loc)
	}
	n, err := m.Stat(ctx, loc)
	if err != nil {
		t.Fatal(err)
	}
	if n.Kind != "heading" || n.Name != "Intro" {
		t.Fatalf("node=%+v", n)
	}
	if loc.String() != "md:doc.md!/intro#1" {
		t.Fatalf("round trip=%s", loc.String())
	}
}

func TestMarkdownMissingAndInvalid(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "doc.md"), []byte(sampleMD), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "sub"), 0755); err != nil {
		t.Fatal(err)
	}
	m := NewMarkdown(root)
	if _, err := m.Stat(ctx, ref.Locator{Scheme: "md", Target: "missing.md"}); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing: %v", err)
	}
	if _, err := m.Stat(ctx, ref.Locator{Scheme: "md", Target: "sub"}); err == nil {
		t.Fatal("directory accepted as markdown document")
	}
	if _, err := m.Stat(ctx, ref.Locator{Scheme: "md"}); err == nil {
		t.Fatal("empty target accepted")
	}
	if _, err := m.Stat(ctx, ref.Locator{Scheme: "dir", Target: "doc.md"}); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("dir scheme: %v", err)
	}
	if _, err := m.List(ctx, ref.Locator{Scheme: "md", Target: "doc.md", ScopeKind: "host", ScopeValue: "elsewhere"}); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("scope: %v", err)
	}
	if _, err := m.Stat(ctx, ref.Locator{Scheme: "md", Target: "doc.md", Inner: []string{"nope#1"}}); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("unknown heading: %v", err)
	}
	if _, err := m.List(ctx, ref.Locator{Scheme: "md", Target: "doc.md", Inner: []string{"intro#1", "nope#1"}}); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("deep unknown heading: %v", err)
	}
}

func TestMarkdownOpenUnsupported(t *testing.T) {
	ctx := context.Background()
	m, _ := writeMD(t, sampleMD)
	if _, err := m.Open(ctx, ref.Locator{Scheme: "md", Target: "doc.md"}); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("open: %v", err)
	}
	if _, err := m.Open(ctx, ref.Locator{Scheme: "md", Target: "doc.md", Inner: []string{"intro#1"}}); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("open heading: %v", err)
	}
	if _, err := m.Open(ctx, ref.Locator{Scheme: "dir", Target: "doc.md"}); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("open dir scheme: %v", err)
	}
	if _, err := m.Open(ctx, ref.Locator{Scheme: "md", Target: ""}); err == nil {
		t.Fatal("open empty target accepted")
	}
}

func TestMarkdownInterface(t *testing.T) {
	var _ Provider = (*Markdown)(nil)
}
