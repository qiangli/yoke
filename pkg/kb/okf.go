package kb

import (
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/qiangli/yoke/pkg/ref"
)

// OKF v0.1 conformance (design §6.4). kb pages are already OKF concept pages;
// this file closes the gaps: the conventional `timestamp`/`resource` fields,
// the reserved `log.md` (a view over journal.jsonl, like index.md), relations
// rendered as relative markdown links under one typed heading per relation,
// and the bundle verbs `kb export --okf DIR` / `kb import --okf DIR`.
//
// Bundle layout (mirrors a ring, so a bundle is browsable as-is):
//
//	DIR/index.md        type: index — one line per page
//	DIR/log.md          type: log — journal.jsonl, oldest first
//	DIR/pages/<slug>.md one OKF page per kb page
//
// The agent ring is never exported (owner-only, I-kb-6), and craft facts are
// not kb records — they live in the skill store, which export never reads.

// okfRelationsMarker opens the rendered relation section of an exported page.
// Import splits on it, so the relations round-trip into graph.jsonl and the
// page body comes back byte-for-byte.
const okfRelationsMarker = "<!-- okf:relations -->"

// okfPageDir is the bundle subdirectory holding the pages.
const okfPageDir = "pages"

// OKFPage returns the OKF rendering of p: a copy with timestamp/resource set
// and, when rels is non-empty, the relations rendered under typed headings.
// exported is the set of slugs present in the same bundle: a relation whose
// dst is one of them renders as a relative link; any other dst renders as a
// code span (it has no file to link to).
func OKFPage(p *Page, rels []Relation, exported map[string]*Page) *Page {
	cp := *p
	cp.Timestamp = p.Updated
	if cp.Timestamp == "" {
		cp.Timestamp = p.Created
	}
	cp.Resource = okfResource(p, rels)
	cp.Export = false
	if sec := okfRelationSection(p.Slug, rels, exported); sec != "" {
		body := strings.TrimSpace(cp.Body)
		if body != "" {
			body += "\n\n"
		}
		cp.Body = body + sec
	}
	return &cp
}

// okfResource names the thing the page describes: a resource the page already
// carries (an imported page), else its first `about` target, else the page
// itself.
func okfResource(p *Page, rels []Relation) string {
	if p.Resource != "" {
		return p.Resource
	}
	var about []string
	for _, r := range rels {
		if r.Relation == "about" && okfSource(r) == "kb:"+p.Slug {
			about = append(about, strings.TrimSpace(r.Dst))
		}
	}
	if len(about) > 0 {
		sort.Strings(about)
		return about[0]
	}
	return "kb:" + p.Slug
}

// okfSource is the canonical source ref of a link relation ("" for a record
// that is not a typed link).
func okfSource(r Relation) string {
	if r.Op != "link" || strings.TrimSpace(r.Relation) == "" {
		return ""
	}
	if s := relationRef(r.Target); s != "" {
		return s
	}
	return strings.TrimSpace(r.Target)
}

func okfRelationSection(slug string, rels []Relation, exported map[string]*Page) string {
	byType := map[string][]string{}
	for _, r := range rels {
		if okfSource(r) != "kb:"+slug {
			continue
		}
		rel := strings.TrimSpace(r.Relation)
		dst := strings.TrimSpace(r.Dst)
		item := "- `" + dst + "`"
		if d := relationRef(dst); strings.HasPrefix(d, "kb:") {
			if tp, ok := exported[strings.TrimPrefix(d, "kb:")]; ok {
				item = fmt.Sprintf("- [%s](%s.md)", okfLinkText(tp), tp.Slug)
			}
		}
		byType[rel] = append(byType[rel], item)
	}
	if len(byType) == 0 {
		return ""
	}
	types := make([]string, 0, len(byType))
	for t := range byType {
		types = append(types, t)
	}
	sort.Strings(types)
	var b strings.Builder
	b.WriteString(okfRelationsMarker + "\n")
	for _, t := range types {
		items := byType[t]
		sort.Strings(items)
		items = slicesCompact(items)
		fmt.Fprintf(&b, "\n## %s\n\n%s\n", t, strings.Join(items, "\n"))
	}
	return strings.TrimRight(b.String(), "\n")
}

func okfLinkText(p *Page) string {
	t := strings.TrimSpace(p.Title)
	if t == "" {
		t = p.Slug
	}
	return strings.NewReplacer("[", "(", "]", ")").Replace(t)
}

func slicesCompact(s []string) []string {
	out := s[:0]
	for i, v := range s {
		if i == 0 || v != s[i-1] {
			out = append(out, v)
		}
	}
	return out
}

// okfRelation is one relation recovered from a page's rendered section.
type okfRelation struct {
	Relation string
	Dst      string
}

// splitOKFRelations separates an exported page body from its rendered
// relation section and parses the section back. A body without the marker
// (any foreign bundle) is returned unchanged with no relations.
func splitOKFRelations(body string) (string, []okfRelation) {
	head, sec, ok := strings.Cut(body, okfRelationsMarker)
	if !ok {
		return body, nil
	}
	var rels []okfRelation
	cur := ""
	for _, line := range strings.Split(sec, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "## "):
			cur = strings.TrimSpace(strings.TrimPrefix(line, "## "))
		case cur == "" || !strings.HasPrefix(line, "- "):
		case strings.HasPrefix(line, "- `") && strings.HasSuffix(line, "`"):
			rels = append(rels, okfRelation{cur, strings.TrimSuffix(strings.TrimPrefix(line, "- `"), "`")})
		case strings.HasPrefix(line, "- ["):
			i := strings.LastIndex(line, "](")
			if i < 0 || !strings.HasSuffix(line, ")") {
				continue
			}
			target := line[i+2 : len(line)-1]
			slug := strings.TrimSuffix(path.Base(target), ".md")
			rels = append(rels, okfRelation{cur, "kb:" + slug})
		}
	}
	return strings.TrimRight(head, "\n"), rels
}

// RenderOKFLog renders journal.jsonl as the OKF reserved log.md: one line per
// journal record, oldest first. linked is the set of slugs with a file in the
// bundle — only those become links, so every link resolves.
func RenderOKFLog(lines []string, linked map[string]*Page) string {
	var b strings.Builder
	b.WriteString("---\ntype: log\ntitle: kb log\ndescription: Chronological change log of this kb ring, generated from journal.jsonl.\n---\n\n# kb log\n\n")
	n := 0
	for _, l := range lines {
		var rec struct {
			Op    string `json:"op"`
			Slug  string `json:"slug"`
			Tool  string `json:"tool"`
			At    string `json:"at"`
			Event string `json:"event"`
			Kind  string `json:"kind"`
			Ref   string `json:"ref"`
		}
		if json.Unmarshal([]byte(l), &rec) != nil {
			continue
		}
		var what string
		switch {
		case rec.Op != "" && rec.Slug != "":
			what = rec.Op + " " + rec.Slug
			if _, ok := linked[rec.Slug]; ok {
				what = fmt.Sprintf("%s [%s](%s/%s.md)", rec.Op, rec.Slug, okfPageDir, rec.Slug)
			}
		case rec.Event != "":
			what = strings.TrimSpace(rec.Event + " " + rec.Kind + " `" + rec.Ref + "`")
		default:
			continue
		}
		if rec.Tool != "" {
			what += " — " + rec.Tool
		}
		fmt.Fprintf(&b, "- %s %s\n", rec.At, what)
		n++
	}
	if n == 0 {
		b.WriteString("(no entries)\n")
	}
	return b.String()
}

// renderOKFIndex renders the bundle index.md: one line per page, by slug.
func renderOKFIndex(pages []*Page) string {
	var b strings.Builder
	b.WriteString("---\ntype: index\ntitle: kb index\ndescription: One line per page in this OKF bundle exported by bashy kb.\n---\n\n# kb index\n\n")
	fmt.Fprintf(&b, "%d page(s). Change history: [log](log.md).\n\n", len(pages))
	for _, p := range pages {
		fmt.Fprintf(&b, "- [%s](%s/%s.md) `%s/%s` %s — %s\n", p.Slug, okfPageDir, p.Slug, p.Status, p.Type, p.Title, p.Description)
	}
	return b.String()
}

// ExportOKF writes store as a self-contained OKF bundle under dir and returns
// the number of pages written. An owner-scoped store (the agent ring) is
// refused: it is never exported.
func ExportOKF(store *Store, dir string) (int, error) {
	if store.ownerScoped {
		return 0, fmt.Errorf("kb: the agent ring is never exported")
	}
	pages, err := store.List()
	if err != nil {
		return 0, err
	}
	sort.Slice(pages, func(i, j int) bool { return pages[i].Slug < pages[j].Slug })
	rels, err := store.LiveRelations()
	if err != nil {
		return 0, err
	}
	bySlug := map[string]*Page{}
	for _, p := range pages {
		bySlug[p.Slug] = p
	}
	if err := os.MkdirAll(filepath.Join(dir, okfPageDir), 0o755); err != nil {
		return 0, err
	}
	for _, p := range pages {
		b, err := OKFPage(p, rels, bySlug).Marshal()
		if err != nil {
			return 0, err
		}
		if err := atomicWrite(filepath.Join(dir, okfPageDir, p.Slug+".md"), b); err != nil {
			return 0, err
		}
	}
	if err := atomicWrite(filepath.Join(dir, "index.md"), []byte(renderOKFIndex(pages))); err != nil {
		return 0, err
	}
	lines, err := store.JournalTail(0)
	if err != nil {
		return 0, err
	}
	if err := atomicWrite(filepath.Join(dir, "log.md"), []byte(RenderOKFLog(lines, bySlug))); err != nil {
		return 0, err
	}
	return len(pages), nil
}

// OKFImportResult reports one import.
type OKFImportResult struct {
	Imported  []string
	Skipped   []string // slug already present in the ring
	Relations int
}

// ImportOKF reads the OKF bundle at dir into store as candidate pages: status
// is always candidate (import never validates), the source is stamped with
// this principal, and the page is tagged xfer:okf. A page type outside kb's
// vocabulary reads as fact and keeps the original as an okf-type:<t> tag.
// Rendered relation sections come back as link relations in graph.jsonl.
// The reserved index.md and log.md are views and are not imported.
func ImportOKF(store *Store, dir string) (OKFImportResult, error) {
	var res OKFImportResult
	if store.ownerScoped {
		return res, fmt.Errorf("kb: import into the agent ring is not supported")
	}
	var files []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if p != dir && strings.HasPrefix(d.Name(), ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.EqualFold(filepath.Ext(p), ".md") {
			return nil
		}
		if rel, _ := filepath.Rel(dir, p); rel == "index.md" || rel == "log.md" {
			return nil
		}
		files = append(files, p)
		return nil
	})
	if err != nil {
		return res, err
	}
	sort.Strings(files)
	source := "okf:" + filepath.Base(filepath.Clean(dir))
	var newRels []Relation
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			return res, err
		}
		slug := Slugify(strings.TrimSuffix(filepath.Base(f), filepath.Ext(f)))
		p, err := ParsePage(slug, b)
		if err != nil {
			return res, err
		}
		if strings.TrimSpace(p.Type) == "" {
			return res, fmt.Errorf("kb: %s: OKF requires type", f)
		}
		if _, err := store.Load(slug); err == nil {
			res.Skipped = append(res.Skipped, slug)
			continue
		}
		body, rels := splitOKFRelations(p.Body)
		p.Body = body
		if !ValidType(p.Type) {
			p.Tags = append(p.Tags, "okf-type:"+p.Type)
			p.Type = TypeFact
		}
		if p.Title == "" {
			p.Title = slug
		}
		p.Status = StatusCandidate
		p.Seq = 0
		p.Timestamp = ""
		p.Export = false
		p.Tags = withXferTag(p.Tags, "okf")
		p.Source = &Source{Tool: ToolID(), Host: HostID(), Episode: EpisodeID(), ParentEpisode: ParentEpisodeID()}
		if err := store.Write(p, "import"); err != nil {
			return res, err
		}
		res.Imported = append(res.Imported, slug)
		for _, r := range rels {
			newRels = append(newRels, okfLinkRelation("kb:"+slug, r.Relation, r.Dst, source))
		}
	}
	if err := appendRelations(store.Dir(), newRels); err != nil {
		return res, err
	}
	res.Relations = len(newRels)
	return res, nil
}

// okfLinkRelation builds an imported link relation with a deterministic id,
// so importing the same bundle twice replays to one record.
func okfLinkRelation(target, rel, dst, source string) Relation {
	h := sha1.New()
	for i, part := range []string{"link", target, rel, dst} {
		if i > 0 {
			h.Write([]byte{0})
		}
		h.Write([]byte(part))
	}
	return Relation{
		ID: hex.EncodeToString(h.Sum(nil))[:16], Op: "link", By: ToolID(), At: time.Now().UTC(),
		Source: source, Episode: EpisodeID(), Target: target, Relation: rel, Dst: dst,
	}
}

// appendRelations O_APPENDs rels to the ring's graph.jsonl.
func appendRelations(dir string, rels []Relation) error {
	if len(rels) == 0 {
		return nil
	}
	var b strings.Builder
	for _, r := range rels {
		line, err := json.Marshal(r)
		if err != nil {
			return err
		}
		b.Write(line)
		b.WriteByte('\n')
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(RelationPath(dir), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(b.String())
	return err
}

// okfForeignTypes maps the foreign ref kinds `kb show --okf` renders onto
// their OKF document type (design §5.1, §6.4).
var okfForeignTypes = map[ref.Kind]string{
	ref.Episode: "episode",
	ref.Out:     "output",
	ref.Skill:   "skill",
}

// okfForeignDoc is the frontmatter of a foreign ref rendered as OKF.
type okfForeignDoc struct {
	Type        string `yaml:"type"`
	Title       string `yaml:"title"`
	Description string `yaml:"description,omitempty"`
	Status      string `yaml:"status,omitempty"`
	Timestamp   string `yaml:"timestamp,omitempty"`
	Resource    string `yaml:"resource"`
	Snapshot    string `yaml:"snapshot,omitempty"`
	Where       string `yaml:"where,omitempty"`
	Open        string `yaml:"open,omitempty"`
}

// RenderOKFForeign renders a resolved foreign node and its content as an OKF
// concept document. Content that carries its own frontmatter (a SKILL.md) is
// unwrapped: its description moves into the OKF frontmatter, its body becomes
// the document body. Nothing is stored.
func RenderOKFForeign(n ref.Node, c ShowContent) (string, error) {
	typ, ok := okfForeignTypes[n.Kind]
	if !ok {
		return "", fmt.Errorf("kb: --okf renders episode, skill and out refs, not %s", n.Kind)
	}
	doc := okfForeignDoc{
		Type: typ, Title: n.Title, Status: n.Status, Timestamp: c.ObservedAt,
		Resource: n.Ref, Snapshot: c.Snapshot, Where: n.Where, Open: n.Open,
	}
	if doc.Title == "" {
		doc.Title = n.Ref
	}
	body := c.Content
	if fm, rest, ok := splitFrontmatter([]byte(body)); ok {
		var meta struct {
			Description string `yaml:"description"`
		}
		if yaml.Unmarshal([]byte(fm), &meta) == nil {
			doc.Description = strings.TrimSpace(meta.Description)
		}
		body = rest
	}
	fmb, err := yaml.Marshal(doc)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString("---\n")
	b.Write(fmb)
	b.WriteString("---\n")
	if body = strings.TrimSpace(body); body != "" {
		b.WriteString("\n" + body + "\n")
	}
	return b.String(), nil
}

func newExportCmd(dir, ring *string) *cobra.Command {
	var okf string
	cmd := &cobra.Command{
		Use:   "export --okf DIR",
		Short: "Write this ring as a self-contained OKF v0.1 bundle (pages + index.md + log.md + typed relation links)",
		Long: `Writes one OKF page per kb page under DIR/pages/, a generated DIR/index.md
and DIR/log.md (from journal.jsonl), with every page's relations rendered as
relative markdown links under one heading per relation type. Each file
carries type. The agent ring is never exported; craft facts are not kb
records and are never read.`,
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			if strings.TrimSpace(okf) == "" {
				return fmt.Errorf("kb: export needs --okf DIR")
			}
			if *ring == "agent" {
				return fmt.Errorf("kb: the agent ring is never exported")
			}
			n, err := ExportOKF(openRing(*dir, *ring), okf)
			if err != nil {
				return err
			}
			fmt.Fprintf(c.OutOrStdout(), "exported %d page(s) to %s\n", n, okf)
			return nil
		},
	}
	cmd.Flags().StringVar(&okf, "okf", "", "bundle directory to write (required)")
	return cmd
}

func newImportCmd(dir, ring *string) *cobra.Command {
	var okf string
	cmd := &cobra.Command{
		Use:   "import --okf DIR",
		Short: "Read an OKF bundle into this ring as candidate pages (never validated by import)",
		Long: `Reads every *.md under DIR (except the reserved index.md and log.md) as a
candidate page tagged xfer:okf with this principal as source. A slug already
in the ring is skipped. Relations rendered by kb export come back as link
relations. Validate imported pages through use: bashy kb validate <slug>.`,
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			if strings.TrimSpace(okf) == "" {
				return fmt.Errorf("kb: import needs --okf DIR")
			}
			if *ring == "agent" {
				return fmt.Errorf("kb: import into the agent ring is not supported")
			}
			res, err := ImportOKF(openRing(*dir, *ring), okf)
			if err != nil {
				return err
			}
			out := c.OutOrStdout()
			fmt.Fprintf(out, "imported %d page(s) as candidate, %d relation(s)\n", len(res.Imported), res.Relations)
			for _, s := range res.Skipped {
				fmt.Fprintf(out, "skipped %s (already in this ring)\n", s)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&okf, "okf", "", "bundle directory to read (required)")
	return cmd
}
