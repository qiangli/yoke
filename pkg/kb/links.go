package kb

import (
	"errors"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/qiangli/yoke/pkg/ref"
	"gopkg.in/yaml.v3"
)

// Links become real at READ TIME. A record's body is a cache of prose, never a
// store: the graph between pages is parsed out of the markdown every time it is
// consumed, and the body is never rewritten to "materialise" a link (that is
// resolve-at-consumption — docs/kb-self-organization.md). This file is the pure
// parser + resolver both `kb backlinks`/`kb doctor` and `bashy todo show
// --links` call. pkg/kb is an import LEAF with respect to its consumers (todo,
// weave, chat, recall, lexicon, …): it parses EVERY kind in the ref vocabulary
// but resolves only kb + todo nodes, and TestKBIsALeaf pins that none of its
// consumers can ever be imported here.
//
// The link grammar is pkg/ref's — one `<kind>:<id>` for everything bashy can
// name — read through the prose spellings:
//
//	[[slug]]                a bare kb slug
//	[[<kind>:<id>]]         any ref: kb:slug, todo:<id> (id or unique prefix,
//	                        git-style), sprint:<n>, run:…, meet:…, agent:… —
//	                        kb resolves kb + todo; every other kind is classified
//	                        and left for its own store (`external` at this layer)
//	[[urn:dhnt:<kind>:<id>]] the fully-qualified spelling, identical to the above
//	[[<other>:<x>]]         an unknown scheme → LinkUnknown, reported, never
//	                        dropped and never mistaken for a slug
//	[text](pages/x.md)        → kb slug x
//	[text](../todo/<id>-….md) → todo id

// LinkKind is the namespace a parsed link points into. It IS the ref
// vocabulary (pkg/ref): the constants below are the three kinds kb has always
// named, kept for the callers that switch on them; every other vocabulary kind
// comes through as its ref.Kind value.
type LinkKind = ref.Kind

const (
	LinkKB     LinkKind = ref.KB
	LinkTodo   LinkKind = ref.Todo
	LinkSprint LinkKind = ref.Sprint
	// LinkUnknown is a `<scheme>:<rest>` whose scheme is not in the vocabulary.
	// Target keeps the whole inner text so a report can show what was written.
	LinkUnknown LinkKind = ref.Unknown
)

// Link is one reference parsed out of a record body.
type Link struct {
	Kind   LinkKind `json:"kind"`
	Target string   `json:"target"` // slug (kb), id (todo), number (sprint)
	Raw    string   `json:"raw"`    // the literal text matched, for reporting
}

// Ref is the canonical address a link resolves to, e.g. "kb:never-pkill".
func (l Link) Ref() string { return string(l.Kind) + ":" + l.Target }

// Local reports whether kb itself can resolve this link's kind (kb and todo).
// Every other vocabulary kind is EXTERNAL at this layer — its own store
// resolves it — and LinkUnknown is neither.
func (l Link) Local() bool { return l.Kind == LinkKB || l.Kind == LinkTodo }

// Status classifies a link a consumer could not resolve against its nodes:
// "external" for a vocabulary kind kb does not hold, "unknown" for a scheme
// outside the vocabulary, "dangling" for a local kind that resolved to nothing.
// The one word every --links renderer prints, so they cannot drift apart.
func (l Link) Status() string {
	switch {
	case l.Kind == LinkUnknown:
		return "unknown"
	case !l.Local():
		return "external"
	}
	return "dangling"
}

// LinkNode is a record in the link graph — a kb page or a todo/issue — viewed as
// both a potential link source (Body) and a potential target (Ref). The page
// pointer is set for kb nodes so doctor can read form/description/status without
// a second load; todo nodes carry only the generic fields kb can read without
// importing pkg/issue.
type LinkNode struct {
	Kind  LinkKind
	ID    string // kb slug | todo id
	Seq   int    // kb page seq (the ring-local human handle, `kb:22`); 0 for todo nodes or a page written before seqs
	Title string
	Type  string // kb page type (lesson|gotcha|runbook|…); empty for todo nodes
	Body  string

	page *Page // set for kb nodes only
}

// Ref is the canonical address another record would cite this node by.
func (n LinkNode) Ref() string { return string(n.Kind) + ":" + n.ID }

// KBNode wraps a kb page as a link-graph node.
func KBNode(p *Page) LinkNode {
	return LinkNode{Kind: LinkKB, ID: p.Slug, Seq: p.Seq, Title: p.Title, Type: p.Type, Body: p.Body, page: p}
}

// KBNodes wraps every page as a link-graph node.
func KBNodes(pages []*Page) []LinkNode {
	out := make([]LinkNode, 0, len(pages))
	for _, p := range pages {
		out = append(out, KBNode(p))
	}
	return out
}

// TodoNode builds a link-graph node for a todo/issue record. pkg/todo (which
// may import kb) calls this with its own issues; kb builds them itself from disk
// via TodoNodesFromDir so it never imports pkg/issue.
func TodoNode(id, title, body string) LinkNode {
	return LinkNode{Kind: LinkTodo, ID: strings.TrimSpace(id), Title: title, Body: body}
}

var (
	wikiRe = regexp.MustCompile(`\[\[([^\]]+)\]\]`)
	mdRe   = regexp.MustCompile(`\[[^\]]*\]\(([^)\s]+)\)`)
)

// ParseLinks extracts every resolvable link from a markdown body, de-duplicated
// by (kind, target) so one page citing the same page twice reports once.
func ParseLinks(body string) []Link {
	var out []Link
	seen := map[string]bool{}
	add := func(l Link, ok bool) {
		if !ok {
			return
		}
		key := string(l.Kind) + "\x00" + l.Target
		if seen[key] {
			return
		}
		seen[key] = true
		out = append(out, l)
	}
	for _, m := range wikiRe.FindAllStringSubmatch(body, -1) {
		add(classifyWiki(m[1], m[0]))
	}
	for _, m := range mdRe.FindAllStringSubmatch(body, -1) {
		add(classifyRel(m[1]))
	}
	return out
}

// classifyWiki turns the inside of a [[…]] into a Link. A bare token is a kb
// slug (slugs are [a-z0-9-], so a colon is always a scheme attempt); anything
// with a colon goes through the one grammar. A known kind selects the
// namespace; an unknown scheme is LinkUnknown — classified so doctor can report
// it, never filed as a slug and never dropped. An empty id ([[kb:]]) is not a
// link.
func classifyWiki(inner, raw string) (Link, bool) {
	inner = strings.TrimSpace(inner)
	if inner == "" {
		return Link{}, false
	}
	if !strings.Contains(inner, ":") {
		return Link{Kind: LinkKB, Target: inner, Raw: raw}, true
	}
	r, err := ref.Parse(inner)
	switch {
	case err == nil:
		return Link{Kind: r.Kind, Target: r.ID, Raw: raw}, true
	case errors.Is(err, ref.ErrEmptyID):
		return Link{}, false
	default: // ErrNotRef (unknown scheme) or ErrUnknownKind (urn:dhnt:<unknown>)
		return Link{Kind: LinkUnknown, Target: inner, Raw: raw}, true
	}
}

// classifyRel turns a relative markdown target into a Link. Only repo-relative
// .md paths under pages/ (kb) or todo/ resolve; external URLs and anchors are
// ignored.
func classifyRel(raw string) (Link, bool) {
	u := strings.TrimSpace(raw)
	if u == "" || strings.Contains(u, "://") || strings.HasPrefix(u, "#") || strings.HasPrefix(u, "mailto:") {
		return Link{}, false
	}
	if !strings.HasSuffix(u, ".md") {
		return Link{}, false
	}
	clean := path.Clean(u)
	stem := strings.TrimSuffix(path.Base(clean), ".md")
	dir := path.Dir(clean)
	switch {
	case strings.Contains(dir, "todo"):
		// todo files are "<id>-<slug>.md"; the id is the hex prefix.
		id := stem
		if i := strings.IndexByte(stem, '-'); i > 0 {
			id = stem[:i]
		}
		return Link{Kind: LinkTodo, Target: id, Raw: raw}, id != ""
	case strings.Contains(dir, "pages") || strings.Contains(dir, "kb"):
		return Link{Kind: LinkKB, Target: stem, Raw: raw}, stem != ""
	}
	return Link{}, false
}

// matches reports whether link l points at node n. kb matches by exact slug;
// todo matches by exact id or a git-style unique prefix; every other kind never
// matches a node here (kb holds no sprint/run/meet/… records — their stores
// resolve them, and `bashy define` is where all of them meet).
func (l Link) matches(n LinkNode) bool {
	switch l.Kind {
	case LinkKB:
		return n.Kind == LinkKB && n.ID == l.Target
	case LinkTodo:
		return n.Kind == LinkTodo && (n.ID == l.Target || strings.HasPrefix(n.ID, l.Target))
	}
	return false
}

// ResolveLink returns the node link l points at within nodes, or (zero, false)
// when it is dangling or external (any kind kb does not hold).
func ResolveLink(l Link, nodes []LinkNode) (LinkNode, bool) {
	for _, n := range nodes {
		if l.matches(n) {
			return n, true
		}
	}
	return LinkNode{}, false
}

// Backlinks returns every node whose body links to target (itself excluded).
func Backlinks(target LinkNode, nodes []LinkNode) []LinkNode {
	var out []LinkNode
	for _, n := range nodes {
		if n.Ref() == target.Ref() {
			continue
		}
		for _, l := range ParseLinks(n.Body) {
			if l.matches(target) {
				out = append(out, n)
				break
			}
		}
	}
	return out
}

// BodyLinkRelation is the relation type a body link ([[slug]], a pages/x.md
// link) carries in `kb backlinks`: prose links are untyped citations.
const BodyLinkRelation = "link"

// TypedBacklink is one inbound edge to a kb page: a body link (Relation
// "link") or a live graph.jsonl link relation whose dst is the page.
type TypedBacklink struct {
	Ref      string
	Kind     string
	Title    string
	Relation string
}

// TypedBacklinks returns the body backlinks of target (in node order) followed
// by every live link relation whose dst resolves to it, sorted by (relation,
// source ref, id). titles for relation sources come from nodes when the source
// is itself a known record.
func TypedBacklinks(target LinkNode, nodes []LinkNode, relations []Relation) []TypedBacklink {
	var out []TypedBacklink
	for _, n := range Backlinks(target, nodes) {
		out = append(out, TypedBacklink{Ref: n.Ref(), Kind: string(n.Kind), Title: n.Title, Relation: BodyLinkRelation})
	}
	byRef := map[string]LinkNode{}
	for _, n := range nodes {
		byRef[n.Ref()] = n
	}
	type relRow struct {
		TypedBacklink
		id string
	}
	var rows []relRow
	for _, rel := range relations {
		if rel.Op != "link" || strings.TrimSpace(rel.Relation) == "" || relationRef(rel.Dst) != target.Ref() {
			continue
		}
		src := relationRef(rel.Target)
		if src == "" {
			src = strings.TrimSpace(rel.Target)
		}
		row := relRow{TypedBacklink: TypedBacklink{Ref: src, Kind: entityKind(src), Relation: strings.TrimSpace(rel.Relation)}, id: rel.ID}
		if r, err := ref.Parse(src); err == nil {
			row.Kind = string(r.Kind)
		}
		if n, ok := byRef[src]; ok {
			row.Title = n.Title
		}
		rows = append(rows, row)
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].Relation != rows[j].Relation {
			return rows[i].Relation < rows[j].Relation
		}
		if rows[i].Ref != rows[j].Ref {
			return rows[i].Ref < rows[j].Ref
		}
		return rows[i].id < rows[j].id
	})
	for _, r := range rows {
		out = append(out, r.TypedBacklink)
	}
	return out
}

// relationRef canonicalises a relation endpoint that names a vocabulary ref
// (kb:x, todo:x, urn:dhnt:kb:x) to "<kind>:<id>"; anything else (a code path,
// a free entity name) yields "".
func relationRef(s string) string {
	s = strings.TrimSpace(s)
	if !strings.Contains(s, ":") {
		return ""
	}
	r, err := ref.Parse(s)
	if err != nil {
		return ""
	}
	return string(r.Kind) + ":" + r.ID
}

// ResolveOutbound splits a node's outbound links into the nodes they resolve to
// and the links that resolve to nothing (dangling or external). Order follows
// the body.
func ResolveOutbound(node LinkNode, nodes []LinkNode) (resolved []LinkNode, dangling []Link) {
	for _, l := range ParseLinks(node.Body) {
		if n, ok := ResolveLink(l, nodes); ok && n.Ref() != node.Ref() {
			resolved = append(resolved, n)
		} else {
			dangling = append(dangling, l)
		}
	}
	return resolved, dangling
}

// todoRepoSub mirrors todo.RepoSub ("docs/todo"). kb cannot import pkg/todo —
// todo imports kb for the resolver, so the reverse would cycle — hence the one
// spelled constant here, kept in step with pkg/todo.RepoSub.
const todoRepoSub = "docs/todo"

// siblingTodoDir locates the repo's docs/todo beside a repo-ring kb store, so
// backlinks and doctor see todo→kb citations. Returns "" for the host/agent
// rings or an arbitrary --dir (no enclosing repo), which callers treat as "todo
// links are not enumerable here" rather than "every todo link is dangling".
func siblingTodoDir(kbDir string) string {
	root := repoRootOf(kbDir)
	if root == "" {
		return ""
	}
	return filepath.Join(root, todoRepoSub)
}

// TodoNodesFromDir reads a directory of todo/issue markdown files into link
// nodes WITHOUT importing pkg/issue (the leaf pin): it splits the frontmatter
// for the id/title and takes the body generically. A missing directory yields
// no nodes and no error.
func TodoNodesFromDir(dir string) ([]LinkNode, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []LinkNode
	for _, e := range ents {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		id, title, body, ok := parseTodoRecord(e.Name(), b)
		if !ok || id == "" {
			continue
		}
		out = append(out, TodoNode(id, title, body))
	}
	return out, nil
}

// parseTodoRecord pulls the id, title and body out of a todo/issue file. The id
// comes from the frontmatter, falling back to the filename's hex prefix
// ("<id>-<slug>.md").
func parseTodoRecord(filename string, b []byte) (id, title, body string, ok bool) {
	fm, bd, fok := splitFrontmatter(b)
	if !fok {
		return "", "", "", false
	}
	var meta struct {
		ID    string `yaml:"id"`
		Title string `yaml:"title"`
	}
	_ = yaml.Unmarshal([]byte(fm), &meta)
	id = strings.TrimSpace(meta.ID)
	if id == "" {
		stem := strings.TrimSuffix(filename, ".md")
		if i := strings.IndexByte(stem, '-'); i > 0 {
			id = stem[:i]
		} else {
			id = stem
		}
	}
	return id, strings.TrimSpace(meta.Title), bd, true
}

// splitFrontmatter splits a "--- … ---" framed markdown file into its raw
// frontmatter and body, mirroring ParsePage's framing exactly.
func splitFrontmatter(b []byte) (fm, body string, ok bool) {
	s := string(b)
	if !strings.HasPrefix(s, "---\n") && !strings.HasPrefix(s, "---\r\n") {
		return "", "", false
	}
	_, rest, _ := strings.Cut(s, "\n")
	fm, body, ok = strings.Cut(rest, "\n---")
	if !ok {
		return "", "", false
	}
	body = strings.TrimPrefix(body, "\r")
	body = strings.TrimPrefix(body, "\n")
	body = strings.TrimRight(strings.TrimPrefix(body, "\n"), "\n")
	return fm, body, true
}
