package provider

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/ref"
)

// pdfStreamObj wraps raw stream bytes in a content-stream object with a
// correct /Length so the fixture never drifts from its cross-reference.
func pdfStreamObj(data string) string {
	return fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(data), data)
}

// pdfTextPage returns a content stream drawing text on one page.
func pdfTextPage(text string) string {
	return pdfStreamObj(fmt.Sprintf("BT /F1 24 Tf 100 700 Td (%s) Tj ET", text))
}

// pdfAssemble numbers objs from 1 and builds a header, xref table, and
// trailer around them with exact byte offsets.
func pdfAssemble(objs ...string) []byte {
	var b strings.Builder
	b.WriteString("%PDF-1.4\n")
	offsets := make([]int, len(objs))
	for i, o := range objs {
		offsets[i] = len(b.String())
		fmt.Fprintf(&b, "%d 0 obj\n%s\nendobj\n", i+1, o)
	}
	xref := len(b.String())
	fmt.Fprintf(&b, "xref\n0 %d\n", len(objs)+1)
	b.WriteString("0000000000 65535 f \n")
	for _, off := range offsets {
		fmt.Fprintf(&b, "%010d 00000 n \n", off)
	}
	fmt.Fprintf(&b, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF", len(objs)+1, xref)
	return []byte(b.String())
}

// pdfWrite builds a pages-long PDF (one content stream per entry of texts;
// an empty entry draws nothing) and optional outline objects, writes it to a
// temp file, and returns its path.
func pdfWrite(t *testing.T, name string, texts []string, withOutline bool) string {
	t.Helper()
	var objs []string
	catalog := "<< /Type /Catalog /Pages 2 0 R"
	if withOutline {
		catalog += " /Outlines 8 0 R"
	}
	catalog += " >>"
	objs = append(objs, catalog)
	kids := make([]string, 0, len(texts))
	for i := range texts {
		kids = append(kids, fmt.Sprintf("%d 0 R", 3+i))
	}
	objs = append(objs, fmt.Sprintf("<< /Type /Pages /Kids [%s] /Count %d >>", strings.Join(kids, " "), len(texts)))
	for i := range texts {
		objs = append(objs, fmt.Sprintf("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Contents %d 0 R /Resources << /Font << /F1 7 0 R >> >> >>", 5+i))
	}
	for _, text := range texts {
		if text == "" {
			objs = append(objs, pdfStreamObj(""))
		} else {
			objs = append(objs, pdfTextPage(text))
		}
	}
	objs = append(objs, "<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>")
	if withOutline {
		objs = append(objs,
			"<< /Type /Outlines /First 9 0 R /Last 10 0 R /Count 2 >>",
			"<< /Title (Chapter One) /Parent 8 0 R /Next 10 0 R /First 11 0 R /Last 11 0 R /Count 1 /Dest [3 0 R /Fit] >>",
			"<< /Title (Chapter Two) /Parent 8 0 R /Prev 9 0 R /Dest [4 0 R /Fit] >>",
			"<< /Title (Section 1.1) /Parent 9 0 R /Dest [3 0 R /Fit] >>",
		)
	}
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, pdfAssemble(objs...), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func pdfLoc(target string, inner ...string) ref.Locator {
	return ref.Locator{Scheme: "pdf", Target: target, Inner: inner}
}

func TestPDFRootAndPages(t *testing.T) {
	target := pdfWrite(t, "two.pdf", []string{"Hello page one", "Second page here"}, false)
	ctx := context.Background()
	p := NewPDF()

	root, err := p.Stat(ctx, pdfLoc(target))
	if err != nil {
		t.Fatal(err)
	}
	if root.Kind != "document" || root.Type != "pdf" {
		t.Fatalf("root kind/type = %q/%q, want document/pdf", root.Kind, root.Type)
	}
	if root.Attrs["page_count"] != 2 || root.Attrs["outline_count"] != 0 {
		t.Fatalf("root attrs = %v, want page_count 2 outline_count 0", root.Attrs)
	}
	if root.SchemaVersion != NodeSchema || root.Children == nil || *root.Children != 2 {
		t.Fatalf("root schema/children = %q/%v, want %q/2", root.SchemaVersion, root.Children, NodeSchema)
	}

	nodes, err := p.List(ctx, pdfLoc(target))
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 2 {
		t.Fatalf("root list = %d nodes, want 2", len(nodes))
	}
	for i, n := range nodes {
		wantNum := i + 1
		if n.Kind != "page" || n.Type != "pdf/page" {
			t.Fatalf("node %d kind/type = %q/%q, want page/pdf/page", i, n.Kind, n.Type)
		}
		if n.Attrs["page_number"] != wantNum || n.Attrs["page_count"] != 2 {
			t.Fatalf("node %d attrs = %v, want page %d of 2", i, n.Attrs, wantNum)
		}
		if n.Attrs["has_text"] != true {
			t.Fatalf("node %d has_text = %v, want true", i, n.Attrs["has_text"])
		}
		wantRef := pdfRef(target, "page", strconv.Itoa(wantNum))
		if n.Ref != wantRef {
			t.Fatalf("node %d ref = %q, want %q", i, n.Ref, wantRef)
		}
		got, err := p.Stat(ctx, pdfLoc(target, "page", strconv.Itoa(wantNum)))
		if err != nil {
			t.Fatal(err)
		}
		if got.Ref != n.Ref {
			t.Fatalf("stat ref = %q, list ref = %q, want stable refs", got.Ref, n.Ref)
		}
	}

	edges, err := p.Edges(ctx, pdfLoc(target))
	if err != nil {
		t.Fatal(err)
	}
	if len(edges) != 2 {
		t.Fatalf("root edges = %d, want 2", len(edges))
	}
	for i, e := range edges {
		if e.Rel != "contains" || e.Src != root.Ref || e.Dst != nodes[i].Ref {
			t.Fatalf("edge %d = %s %s %s, want contains root->page", i, e.Src, e.Rel, e.Dst)
		}
		if e.SchemaVersion != EdgeSchema {
			t.Fatalf("edge %d schema = %q, want %q", i, e.SchemaVersion, EdgeSchema)
		}
	}
	back, err := p.Edges(ctx, pdfLoc(target, "page", "1"))
	if err != nil {
		t.Fatal(err)
	}
	if len(back) != 1 || back[0].Rel != "part_of" || back[0].Dst != root.Ref {
		t.Fatalf("page edges = %+v, want one part_of edge to root", back)
	}
}

func TestPDFOutline(t *testing.T) {
	target := pdfWrite(t, "outline.pdf", []string{"Hello page one", "Second page here"}, true)
	ctx := context.Background()
	p := NewPDF()

	root, err := p.Stat(ctx, pdfLoc(target))
	if err != nil {
		t.Fatal(err)
	}
	if root.Attrs["outline_count"] != 2 {
		t.Fatalf("root outline_count = %v, want 2", root.Attrs["outline_count"])
	}
	if root.Children == nil || *root.Children != 3 {
		t.Fatalf("root children = %v, want 3 (2 pages + outline)", root.Children)
	}

	group, err := p.Stat(ctx, pdfLoc(target, "outline"))
	if err != nil {
		t.Fatal(err)
	}
	if group.Kind != "outline" || group.Attrs["count"] != 2 {
		t.Fatalf("group kind/count = %q/%v, want outline/2", group.Kind, group.Attrs["count"])
	}

	top, err := p.List(ctx, pdfLoc(target, "outline"))
	if err != nil {
		t.Fatal(err)
	}
	if len(top) != 2 || top[0].Name != "Chapter One" || top[1].Name != "Chapter Two" {
		t.Fatalf("top outline = %+v, want Chapter One/Chapter Two", top)
	}
	if top[0].Attrs["depth"] != 1 {
		t.Fatalf("top entry depth = %v, want 1", top[0].Attrs["depth"])
	}

	nested, err := p.List(ctx, pdfLoc(target, "outline", "0"))
	if err != nil {
		t.Fatal(err)
	}
	if len(nested) != 1 || nested[0].Name != "Section 1.1" {
		t.Fatalf("nested outline = %+v, want Section 1.1", nested)
	}
	leaf, err := p.Stat(ctx, pdfLoc(target, "outline", "0", "0"))
	if err != nil {
		t.Fatal(err)
	}
	if leaf.Ref != nested[0].Ref {
		t.Fatalf("leaf ref = %q, list ref = %q, want stable refs", leaf.Ref, nested[0].Ref)
	}
	edges, err := p.Edges(ctx, pdfLoc(target, "outline", "0", "0"))
	if err != nil {
		t.Fatal(err)
	}
	if len(edges) != 1 || edges[0].Rel != "part_of" || edges[0].Dst != pdfRef(target, "outline", "0") {
		t.Fatalf("leaf outline edges = %+v, want part_of to parent", edges)
	}
}

func TestPDFBlankPageHasNoText(t *testing.T) {
	target := pdfWrite(t, "blank.pdf", []string{""}, false)
	ctx := context.Background()
	p := NewPDF()

	n, err := p.Stat(ctx, pdfLoc(target, "page", "1"))
	if err != nil {
		t.Fatal(err)
	}
	if n.Attrs["has_text"] != false {
		t.Fatalf("blank page has_text = %v, want false", n.Attrs["has_text"])
	}
}

func TestPDFMalformed(t *testing.T) {
	p := filepath.Join(t.TempDir(), "bad.pdf")
	if err := os.WriteFile(p, []byte("this is not a pdf at all %PDF-zzz"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	prov := NewPDF()
	for _, loc := range []ref.Locator{pdfLoc(p), pdfLoc(p, "page", "1"), pdfLoc(p, "outline")} {
		if _, err := prov.Stat(ctx, loc); err == nil {
			t.Fatalf("stat %v: want error for malformed PDF", loc.Inner)
		}
		if _, err := prov.List(ctx, loc); err == nil {
			t.Fatalf("list %v: want error for malformed PDF", loc.Inner)
		}
		if _, err := prov.Edges(ctx, loc); err == nil {
			t.Fatalf("edges %v: want error for malformed PDF", loc.Inner)
		}
	}
}

func TestPDFMissingAndBadAddresses(t *testing.T) {
	ctx := context.Background()
	p := NewPDF()
	missing := filepath.Join(t.TempDir(), "nope.pdf")
	if _, err := p.Stat(ctx, pdfLoc(missing)); err == nil {
		t.Fatal("stat missing file: want error")
	}

	target := pdfWrite(t, "two.pdf", []string{"Hello page one", "Second page here"}, false)
	for _, inner := range [][]string{{"page", "0"}, {"page", "3"}, {"page", "x"}, {"page"}, {"bogus"}, {"outline"}, {"outline", "0"}} {
		loc := pdfLoc(target, inner...)
		if _, err := p.Stat(ctx, loc); err == nil {
			t.Fatalf("stat %v: want error", inner)
		}
		if _, err := p.Edges(ctx, loc); err == nil {
			t.Fatalf("edges %v: want error", inner)
		}
	}
	if _, err := p.List(ctx, pdfLoc(target, "page", "3")); err == nil {
		t.Fatal("list bad page: want error")
	}
}

func TestPDFUnsupported(t *testing.T) {
	ctx := context.Background()
	p := NewPDF()
	target := pdfWrite(t, "two.pdf", []string{"Hello page one", "Second page here"}, false)

	if _, err := p.Open(ctx, pdfLoc(target)); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("open root err = %v, want ErrUnsupported", err)
	}
	if _, err := p.Open(ctx, pdfLoc(target, "page", "1")); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("open page err = %v, want ErrUnsupported", err)
	}
	if _, err := p.List(ctx, pdfLoc(target, "page", "1")); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("list page err = %v, want ErrUnsupported", err)
	}
	if _, err := p.Stat(ctx, ref.Locator{Scheme: "dir", Target: target}); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("wrong scheme err = %v, want ErrUnsupported", err)
	}
	if _, err := p.Stat(ctx, ref.Locator{Scheme: "pdf"}); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("empty target err = %v, want ErrUnsupported", err)
	}
}
