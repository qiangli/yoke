// PDF exposes a PDF file as a read-only tree of Node rows with graph edges:
//
//	pdf:/path/to/file.pdf                  document root
//	pdf:/path/to/file.pdf!/page/1           page 1 (1-based)
//	pdf:/path/to/file.pdf!/outline          outline group (only when the file has one)
//	pdf:/path/to/file.pdf!/outline/0/1      outline entry by child-index path
//
// Text comes from embedded page content only (no OCR): a page node reports
// whether it has extractable text via attrs. Outline entries come from the
// document's /Outlines dictionary, likewise available without OCR. Open is
// not supported; the structure is exposed as Node/Edge rows only.
package provider

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	pdflib "github.com/ledongthuc/pdf"
	"github.com/qiangli/yoke/pkg/ref"
)

var _ Provider = (*PDF)(nil)

// PDF is a read-only Provider over PDF files. Scheme is "pdf".
type PDF struct{ Root string }

// NewPDF returns a pdf: provider.
func NewPDF() *PDF { return &PDF{} }

// pdfDoc is a parsed PDF: trimmed embedded text per page plus the raw
// outline tree (root typically untitled; its Child holds top-level entries).
type pdfDoc struct {
	pages   []string
	outline pdflib.Outline
	size    int64
}

// pdfLoad checks the locator, reads the file, and parses it. It mirrors the
// text-extraction behavior of pkg/lens/pdf.go (embedded text only, no OCR,
// per-page loop tolerating blank pages). ledongthuc/pdf panics on some
// malformed inputs; that is invalid input, not a crash.
func (p *PDF) pdfLoad(loc ref.Locator) (doc *pdfDoc, err error) {
	if loc.Scheme != "pdf" || loc.ScopeKind != "" || loc.Target == "" {
		return nil, ErrUnsupported
	}
	filePath := loc.Target
	if p.Root != "" && !filepath.IsAbs(filePath) {
		filePath = filepath.Join(p.Root, filePath)
	}
	raw, rerr := os.ReadFile(filePath)
	if rerr != nil {
		return nil, rerr
	}
	defer func() {
		if r := recover(); r != nil {
			doc, err = nil, fmt.Errorf("provider: pdf: %v", r)
		}
	}()
	r, rerr := pdflib.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if rerr != nil {
		return nil, fmt.Errorf("provider: pdf: %w", rerr)
	}
	total := r.NumPage()
	pages := make([]string, 0, total)
	for i := 1; i <= total; i++ {
		pg := r.Page(i)
		if pg.V.IsNull() {
			pages = append(pages, "")
			continue
		}
		text, terr := pg.GetPlainText(nil)
		if terr != nil {
			pages = append(pages, "")
			continue
		}
		pages = append(pages, strings.TrimSpace(text))
	}
	return &pdfDoc{pages: pages, outline: r.Outline(), size: int64(len(raw))}, nil
}

// pdfRef builds the stable ref for target with the given inner path.
func pdfRef(target string, inner ...string) string {
	return ref.Locator{Scheme: "pdf", Target: target, Inner: inner}.String()
}

// pdfParsePage validates inner as ["page", N] and returns N (1-based).
func pdfParsePage(inner []string, total int) (int, error) {
	n, err := strconv.Atoi(inner[1])
	if err != nil || n < 1 || n > total {
		return 0, fmt.Errorf("provider: pdf page %q out of range [1,%d]: %w", inner[1], total, os.ErrNotExist)
	}
	return n, nil
}

// pdfParseOutline validates inner as ["outline", idx...] and walks the
// outline tree to the addressed entry.
func pdfParseOutline(inner []string, root pdflib.Outline) (pdflib.Outline, error) {
	cur := root
	for _, seg := range inner[1:] {
		i, err := strconv.Atoi(seg)
		if err != nil || i < 0 || i >= len(cur.Child) {
			return pdflib.Outline{}, fmt.Errorf("provider: pdf outline %q has no index %q: %w", strings.Join(inner, "/"), seg, os.ErrNotExist)
		}
		cur = cur.Child[i]
	}
	return cur, nil
}

// pdfRootNode builds the document node.
func pdfRootNode(target string, doc *pdfDoc) Node {
	size := doc.size
	count := uint64(len(doc.pages))
	var outline int
	if len(doc.outline.Child) > 0 {
		count++
		outline = len(doc.outline.Child)
	}
	return Node{
		SchemaVersion: NodeSchema,
		Ref:           pdfRef(target),
		Path:          target,
		Name:          filepath.Base(target),
		Kind:          "document",
		Type:          "pdf",
		Size:          &size,
		Children:      &count,
		Attrs: map[string]any{
			"page_count":    len(doc.pages),
			"outline_count": outline,
		},
	}
}

// pdfPageNode builds the node for page n (1-based).
func pdfPageNode(target string, n, total int, text string) Node {
	return Node{
		SchemaVersion: NodeSchema,
		Ref:           pdfRef(target, "page", strconv.Itoa(n)),
		Path:          target + "!/page/" + strconv.Itoa(n),
		Name:          fmt.Sprintf("Page %d", n),
		Kind:          "page",
		Type:          "pdf/page",
		Attrs: map[string]any{
			"page_number": n,
			"page_count":  total,
			"has_text":    text != "",
			"text_bytes":  len(text),
		},
	}
}

// pdfOutlineNode builds the outline group node (inner ["outline"]).
func pdfOutlineNode(target string, n int) Node {
	count := uint64(n)
	return Node{
		SchemaVersion: NodeSchema,
		Ref:           pdfRef(target, "outline"),
		Path:          target + "!/outline",
		Name:          "outline",
		Kind:          "outline",
		Type:          "pdf/outline",
		Children:      &count,
		Attrs:         map[string]any{"count": n},
	}
}

// pdfEntryNode builds the node for one outline entry at index path idx.
func pdfEntryNode(target string, entry pdflib.Outline, idx []string) Node {
	name := entry.Title
	if name == "" {
		name = "section " + strings.Join(idx, ".")
	}
	inner := append([]string{"outline"}, idx...)
	n := Node{
		SchemaVersion: NodeSchema,
		Ref:           pdfRef(target, inner...),
		Path:          target + "!/outline/" + strings.Join(idx, "/"),
		Name:          name,
		Kind:          "outline",
		Type:          "pdf/outline",
		Attrs: map[string]any{
			"title": entry.Title,
			"depth": len(idx),
		},
	}
	if len(entry.Child) > 0 {
		count := uint64(len(entry.Child))
		n.Children = &count
	}
	return n
}

// pdfStat resolves loc to its Node row.
func (p *PDF) pdfStat(loc ref.Locator) (Node, error) {
	doc, err := p.pdfLoad(loc)
	if err != nil {
		return Node{}, err
	}
	inner := loc.Inner
	switch {
	case len(inner) == 0:
		return pdfRootNode(loc.Target, doc), nil
	case len(inner) == 2 && inner[0] == "page":
		n, err := pdfParsePage(inner, len(doc.pages))
		if err != nil {
			return Node{}, err
		}
		return pdfPageNode(loc.Target, n, len(doc.pages), doc.pages[n-1]), nil
	case len(inner) == 1 && inner[0] == "outline":
		if len(doc.outline.Child) == 0 {
			return Node{}, fmt.Errorf("provider: pdf %s has no outline: %w", loc.Target, os.ErrNotExist)
		}
		return pdfOutlineNode(loc.Target, len(doc.outline.Child)), nil
	case len(inner) >= 2 && inner[0] == "outline":
		entry, err := pdfParseOutline(inner, doc.outline)
		if err != nil {
			return Node{}, err
		}
		return pdfEntryNode(loc.Target, entry, inner[1:]), nil
	default:
		return Node{}, fmt.Errorf("provider: pdf: unknown path %q: %w", strings.Join(inner, "/"), os.ErrNotExist)
	}
}

// Stat returns the Node row for the document root, a page, the outline
// group, or an outline entry.
func (p *PDF) Stat(_ context.Context, loc ref.Locator) (Node, error) {
	return p.pdfStat(loc)
}

// List returns one Node row per child: pages (plus the outline group when
// present) at the root, top-level entries under the outline group, and child
// entries under an outline entry. Pages are leaves and have no children.
func (p *PDF) List(_ context.Context, loc ref.Locator) ([]Node, error) {
	doc, err := p.pdfLoad(loc)
	if err != nil {
		return nil, err
	}
	inner := loc.Inner
	switch {
	case len(inner) == 0:
		out := make([]Node, 0, len(doc.pages)+1)
		for i, text := range doc.pages {
			out = append(out, pdfPageNode(loc.Target, i+1, len(doc.pages), text))
		}
		if len(doc.outline.Child) > 0 {
			out = append(out, pdfOutlineNode(loc.Target, len(doc.outline.Child)))
		}
		return out, nil
	case len(inner) == 2 && inner[0] == "page":
		if _, err := pdfParsePage(inner, len(doc.pages)); err != nil {
			return nil, err
		}
		return nil, ErrUnsupported
	case len(inner) == 1 && inner[0] == "outline":
		if len(doc.outline.Child) == 0 {
			return nil, fmt.Errorf("provider: pdf %s has no outline: %w", loc.Target, os.ErrNotExist)
		}
		out := make([]Node, 0, len(doc.outline.Child))
		for i, child := range doc.outline.Child {
			out = append(out, pdfEntryNode(loc.Target, child, []string{strconv.Itoa(i)}))
		}
		return out, nil
	case len(inner) >= 2 && inner[0] == "outline":
		entry, err := pdfParseOutline(inner, doc.outline)
		if err != nil {
			return nil, err
		}
		out := make([]Node, 0, len(entry.Child))
		for i, child := range entry.Child {
			idx := append(append([]string{}, inner[1:]...), strconv.Itoa(i))
			out = append(out, pdfEntryNode(loc.Target, child, idx))
		}
		return out, nil
	default:
		return nil, fmt.Errorf("provider: pdf: unknown path %q: %w", strings.Join(inner, "/"), os.ErrNotExist)
	}
}

// Open is not supported: PDF content is exposed as Node/Edge rows only.
func (p *PDF) Open(context.Context, ref.Locator) (io.ReadCloser, error) {
	return nil, ErrUnsupported
}

// Edges returns "contains" edges from a container to its children, or the
// single "part_of" edge from a leaf (a page or childless outline entry) to
// its parent.
func (p *PDF) Edges(_ context.Context, loc ref.Locator) ([]Edge, error) {
	doc, err := p.pdfLoad(loc)
	if err != nil {
		return nil, err
	}
	contains := func(src string, dsts []string, attrs []map[string]any) ([]Edge, error) {
		out := make([]Edge, 0, len(dsts))
		for i, dst := range dsts {
			out = append(out, Edge{SchemaVersion: EdgeSchema, Src: src, Rel: "contains", Dst: dst, Attrs: attrs[i]})
		}
		return out, nil
	}
	inner := loc.Inner
	switch {
	case len(inner) == 0:
		var dsts []string
		var attrs []map[string]any
		for i := range doc.pages {
			dsts = append(dsts, pdfRef(loc.Target, "page", strconv.Itoa(i+1)))
			attrs = append(attrs, map[string]any{"page_number": i + 1})
		}
		if len(doc.outline.Child) > 0 {
			dsts = append(dsts, pdfRef(loc.Target, "outline"))
			attrs = append(attrs, map[string]any{})
		}
		return contains(pdfRef(loc.Target), dsts, attrs)
	case len(inner) == 2 && inner[0] == "page":
		if _, err := pdfParsePage(inner, len(doc.pages)); err != nil {
			return nil, err
		}
		return []Edge{{
			SchemaVersion: EdgeSchema,
			Src:           pdfRef(loc.Target, inner...),
			Rel:           "part_of",
			Dst:           pdfRef(loc.Target),
			Attrs:         map[string]any{},
		}}, nil
	case len(inner) == 1 && inner[0] == "outline":
		if len(doc.outline.Child) == 0 {
			return nil, fmt.Errorf("provider: pdf %s has no outline: %w", loc.Target, os.ErrNotExist)
		}
		var dsts []string
		var attrs []map[string]any
		for i := range doc.outline.Child {
			dsts = append(dsts, pdfRef(loc.Target, "outline", strconv.Itoa(i)))
			attrs = append(attrs, map[string]any{"index": i})
		}
		return contains(pdfRef(loc.Target, inner...), dsts, attrs)
	case len(inner) >= 2 && inner[0] == "outline":
		entry, err := pdfParseOutline(inner, doc.outline)
		if err != nil {
			return nil, err
		}
		if len(entry.Child) == 0 {
			parent := []string{"outline"}
			if len(inner) > 2 {
				parent = append(parent, inner[1:len(inner)-1]...)
			}
			return []Edge{{
				SchemaVersion: EdgeSchema,
				Src:           pdfRef(loc.Target, inner...),
				Rel:           "part_of",
				Dst:           pdfRef(loc.Target, parent...),
				Attrs:         map[string]any{},
			}}, nil
		}
		var dsts []string
		var attrs []map[string]any
		for i := range entry.Child {
			idx := append(append([]string{}, inner[1:]...), strconv.Itoa(i))
			dsts = append(dsts, pdfRef(loc.Target, append([]string{"outline"}, idx...)...))
			attrs = append(attrs, map[string]any{"index": i})
		}
		return contains(pdfRef(loc.Target, inner...), dsts, attrs)
	default:
		return nil, fmt.Errorf("provider: pdf: unknown path %q: %w", strings.Join(inner, "/"), os.ErrNotExist)
	}
}
