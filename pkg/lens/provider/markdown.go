package provider

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"github.com/qiangli/yoke/pkg/ref"
)

const mdFence = "```"

var _ Provider = (*Markdown)(nil)

// Markdown exposes the heading outline of a Markdown document as a read-only
// tree. The document is the directory row; ATX headings (outside fenced code
// blocks) are heading rows in document order, nested by heading level.
// Duplicate titles stay distinct through a per-slug occurrence suffix.
type Markdown struct {
	FS   fs.FS
	Root string
}

// NewMarkdown returns a read-only Markdown outline provider rooted at root.
func NewMarkdown(root string) *Markdown {
	if root == "" {
		root = "."
	}
	return &Markdown{FS: os.DirFS(root), Root: root}
}

// fsPath maps an md: locator to a path inside the provider root. Any locator
// outside the md: scheme, or carrying a location scope, is unsupported.
func (m *Markdown) fsPath(loc ref.Locator) (string, error) {
	if loc.Scheme != "md" || loc.ScopeKind != "" {
		return "", ErrUnsupported
	}
	p := filepath.ToSlash(loc.Target)
	if p == "" || p == "." {
		return "", fmt.Errorf("provider: markdown target required")
	}
	if path.IsAbs(p) {
		rel, err := filepath.Rel(m.Root, loc.Target)
		if err != nil {
			return "", err
		}
		p = filepath.ToSlash(rel)
	}
	if !fs.ValidPath(p) {
		return "", fmt.Errorf("provider: invalid markdown path %q", p)
	}
	return p, nil
}

type mdHeading struct {
	level      int
	title      string
	line       int
	slug       string
	occurrence int
	address    string
	children   []*mdHeading
}

type mdOutline struct {
	info  fs.FileInfo
	roots []*mdHeading
	total int
}

// scanMDHeadings reports ATX headings in document order, skipping fenced code
// blocks and trailing closing hash sequences.
func scanMDHeadings(r io.Reader) ([]mdHeading, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	var out []mdHeading
	inFence := false
	line := 0
	for sc.Scan() {
		line++
		s := strings.TrimLeft(sc.Text(), " ")
		if inFence {
			if isMDFence(s) {
				inFence = false
			}
			continue
		}
		if isMDFence(s) {
			inFence = true
			continue
		}
		level, title, ok := parseMDATX(s)
		if !ok {
			continue
		}
		out = append(out, mdHeading{level: level, title: title, line: line})
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func isMDFence(s string) bool {
	return strings.HasPrefix(s, "```") || strings.HasPrefix(s, "~~~")
}

func parseMDATX(s string) (int, string, bool) {
	n := 0
	for n < len(s) && s[n] == '#' {
		n++
	}
	if n < 1 || n > 6 {
		return 0, "", false
	}
	rest := s[n:]
	if rest != "" && rest[0] != ' ' && rest[0] != '\t' {
		return 0, "", false
	}
	title := strings.TrimSpace(rest)
	// An optional closing hash sequence must be preceded by whitespace.
	j := len(title)
	for j > 0 && title[j-1] == '#' {
		j--
	}
	if j < len(title) && j > 0 && (title[j-1] == ' ' || title[j-1] == '\t') {
		title = strings.TrimRight(title[:j], " \t")
	}
	return n, title, true
}

func mdSlug(title string) string {
	var b strings.Builder
	sep := false
	for _, r := range strings.ToLower(title) {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r) || r == '-' || r == '_':
			b.WriteRune(r)
			sep = false
		default:
			if !sep && b.Len() > 0 {
				b.WriteByte('-')
				sep = true
			}
		}
	}
	s := strings.Trim(b.String(), "-")
	if s == "" {
		s = "heading"
	}
	return s
}

func buildMDOutline(headings []mdHeading) *mdOutline {
	ol := &mdOutline{total: len(headings)}
	occurrence := map[string]int{}
	var stack []*mdHeading
	for i := range headings {
		h := &headings[i]
		h.slug = mdSlug(h.title)
		occurrence[h.slug]++
		h.occurrence = occurrence[h.slug]
		h.address = fmt.Sprintf("%s#%d", h.slug, h.occurrence)
		for len(stack) > 0 && stack[len(stack)-1].level >= h.level {
			stack = stack[:len(stack)-1]
		}
		if len(stack) == 0 {
			ol.roots = append(ol.roots, h)
		} else {
			parent := stack[len(stack)-1]
			parent.children = append(parent.children, h)
		}
		stack = append(stack, h)
	}
	return ol
}

// mdSegments flattens the locator inner path into heading addresses. Inner
// components are "!/"-separated, while emitted refs join addresses with "/", so
// both spellings resolve to the same address sequence.
func mdSegments(inner []string) []string {
	var segs []string
	for _, c := range inner {
		for _, part := range strings.Split(c, "/") {
			if part != "" {
				segs = append(segs, part)
			}
		}
	}
	return segs
}

// mdResolve returns the heading whose address path equals the inner path.
func mdResolve(inner []string, headings []*mdHeading) (*mdHeading, error) {
	segs := mdSegments(inner)
	if len(segs) == 0 {
		return nil, fmt.Errorf("provider: heading path required: %w", fs.ErrNotExist)
	}
	for _, h := range headings {
		if h.address != segs[0] {
			continue
		}
		if len(segs) == 1 {
			return h, nil
		}
		return mdResolve([]string{strings.Join(segs[1:], "/")}, h.children)
	}
	return nil, fmt.Errorf("provider: heading %q: %w", segs[0], fs.ErrNotExist)
}

func (ol *mdOutline) children(loc ref.Locator) ([]*mdHeading, error) {
	if len(loc.Inner) == 0 {
		return ol.roots, nil
	}
	h, err := mdResolve(loc.Inner, ol.roots)
	if err != nil {
		return nil, err
	}
	return h.children, nil
}

func (m *Markdown) load(ctx context.Context, loc ref.Locator, p string) (*mdOutline, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	info, err := fs.Stat(m.FS, p)
	if err != nil {
		return nil, err
	}
	if info.IsDir() {
		return nil, fmt.Errorf("provider: markdown target %q is a directory", p)
	}
	f, err := m.FS.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	headings, err := scanMDHeadings(f)
	if err != nil {
		return nil, err
	}
	ol := buildMDOutline(headings)
	ol.info = info
	return ol, nil
}

func (ol *mdOutline) docNode(loc ref.Locator, p string) Node {
	roots := uint64(len(ol.roots))
	size := ol.info.Size()
	return Node{
		SchemaVersion: NodeSchema,
		Ref:           loc.String(),
		Path:          p,
		Name:          ol.info.Name(),
		Kind:          "directory",
		Type:          "text/markdown",
		Size:          &size,
		Mode:          ol.info.Mode().String(),
		Mtime:         ol.info.ModTime().UTC().Format(time.RFC3339Nano),
		Children:      &roots,
		Attrs: map[string]any{
			"headings":      ol.total,
			"root_headings": len(ol.roots),
		},
	}
}

func (ol *mdOutline) headingNode(loc ref.Locator, h *mdHeading) Node {
	name := h.title
	if name == "" {
		name = fmt.Sprintf("h%d", h.level)
	}
	n := Node{
		SchemaVersion: NodeSchema,
		Ref:           loc.String(),
		Path:          strings.Join(loc.Inner, "/"),
		Name:          name,
		Kind:          "heading",
		Type:          "text/markdown",
		Attrs: map[string]any{
			"level":      h.level,
			"line":       h.line,
			"title":      h.title,
			"slug":       h.slug,
			"occurrence": h.occurrence,
		},
	}
	if count := uint64(len(h.children)); count > 0 {
		n.Children = &count
	}
	return n
}

// mdChild extends the locator inner path with one more heading address. Each
// emitted ref appends the address to the parent inner component, so refs read
// md:doc.md!/intro#1/details#1 and stay a single inner segment.
func mdChild(loc ref.Locator, address string) ref.Locator {
	child := loc
	if len(child.Inner) == 0 {
		child.Inner = []string{address}
	} else {
		last := len(child.Inner) - 1
		joined := append([]string{}, child.Inner[:last]...)
		joined = append(joined, child.Inner[last]+"/"+address)
		child.Inner = joined
	}
	return child
}

// Stat returns the document row, or the heading row named by the inner path.
func (m *Markdown) Stat(ctx context.Context, loc ref.Locator) (Node, error) {
	if err := ctx.Err(); err != nil {
		return Node{}, err
	}
	p, err := m.fsPath(loc)
	if err != nil {
		return Node{}, err
	}
	ol, err := m.load(ctx, loc, p)
	if err != nil {
		return Node{}, err
	}
	if len(loc.Inner) == 0 {
		return ol.docNode(loc, p), nil
	}
	h, err := mdResolve(loc.Inner, ol.roots)
	if err != nil {
		return Node{}, err
	}
	return ol.headingNode(loc, h), nil
}

// List returns the direct heading children in document order.
func (m *Markdown) List(ctx context.Context, loc ref.Locator) ([]Node, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	p, err := m.fsPath(loc)
	if err != nil {
		return nil, err
	}
	ol, err := m.load(ctx, loc, p)
	if err != nil {
		return nil, err
	}
	children, err := ol.children(loc)
	if err != nil {
		return nil, err
	}
	out := make([]Node, 0, len(children))
	for _, h := range children {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		out = append(out, ol.headingNode(mdChild(loc, h.address), h))
	}
	return out, nil
}

// Edges returns contains edges to the direct heading children, in order.
func (m *Markdown) Edges(ctx context.Context, loc ref.Locator) ([]Edge, error) {
	children, err := m.List(ctx, loc)
	if err != nil {
		return nil, err
	}
	edges := make([]Edge, 0, len(children))
	for _, child := range children {
		edges = append(edges, Edge{
			SchemaVersion: EdgeSchema, Src: loc.String(), Rel: "contains",
			Dst: child.Ref, Attrs: map[string]any{},
		})
	}
	return edges, nil
}

// Open is unsupported: the provider exposes only the heading outline.
func (m *Markdown) Open(ctx context.Context, loc ref.Locator) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if _, err := m.fsPath(loc); err != nil {
		return nil, err
	}
	return nil, ErrUnsupported
}
