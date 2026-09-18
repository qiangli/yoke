package kb

import (
	"fmt"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

// Page is one kb entry: an OKF-style concept page — YAML frontmatter over a
// distilled markdown body. The body is strategy, not transcript: a few
// sentences plus the concrete evidence, with failure entries phrased as
// guardrails. Markdown links between pages are the only graph.
type Page struct {
	Slug string `yaml:"-"` // filename stem under pages/

	// ID and Seq are the page's other two handles beside the slug (ref.Shape):
	// ID is a UUIDv7 — the identity, universal across hosts and rings — and Seq
	// the running number unique within THIS ring, the one a human types
	// (`kb:22`). Both are minted by Store.Write when empty, never on read; a
	// page that predates them resolves by slug until it is next written.
	ID  string `yaml:"id,omitempty"`
	Seq int    `yaml:"seq,omitempty"`

	Form         string   `yaml:"form,omitempty"` // note|page (the SHAPE; a legacy record with none reads as page)
	Type         string   `yaml:"type"`           // lesson|gotcha|runbook|decision|fact (OKF: the one required field)
	Title        string   `yaml:"title"`
	Description  string   `yaml:"description"` // "what + WHEN this applies" — the routing surface
	Tags         []string `yaml:"tags,omitempty"`
	Scope        *Scope   `yaml:"scope,omitempty"`
	Status       string   `yaml:"status,omitempty"` // candidate|validated|stale|superseded
	Evidence     string   `yaml:"evidence,omitempty"`
	Source       *Source  `yaml:"source,omitempty"`
	Created      string   `yaml:"created,omitempty"` // RFC 3339
	Updated      string   `yaml:"updated,omitempty"`
	Supersedes   string   `yaml:"supersedes,omitempty"`
	SupersededBy string   `yaml:"superseded_by,omitempty"`

	Body string `yaml:"-"`
}

// Scope narrows where a page applies. Empty fields = applies everywhere; a
// page scoped to repos it doesn't match is filtered out of search results.
type Scope struct {
	Repos []string `yaml:"repos,omitempty"` // repo basenames
	OS    string   `yaml:"os,omitempty"`    // GOOS value
}

// Source records provenance: which tool wrote the page, on which host
// (the cloudbox-registered agent name when paired), in which episode.
type Source struct {
	Tool    string `yaml:"tool,omitempty"`
	Host    string `yaml:"host,omitempty"`
	Episode string `yaml:"episode,omitempty"`
}

// Page types (OKF `type`) and the validation ladder statuses.
const (
	TypeLesson   = "lesson"
	TypeGotcha   = "gotcha"
	TypeRunbook  = "runbook"
	TypeDecision = "decision"
	TypeFact     = "fact"

	StatusCandidate  = "candidate"
	StatusValidated  = "validated"
	StatusStale      = "stale"
	StatusSuperseded = "superseded"
)

// Record forms — the SHAPE of a record, orthogonal to Type (the WHY). Two are
// stored as a Page on disk: FormNote (description optional, body free — the
// memo/documentation shape) and FormPage (today's concept page). FormRelation
// is a claim stored in graph.jsonl, not a Page; FormCode is a view over the
// code graph, never a stored record. Both are in the reader vocabulary so a
// caller can name them, but neither is ever written here.
const (
	FormNote     = "note"
	FormPage     = "page"
	FormRelation = "relation"
	FormCode     = "code"
)

var pageTypes = []string{TypeLesson, TypeGotcha, TypeRunbook, TypeDecision, TypeFact}

// forms is the full reader vocabulary a --form flag may name.
var forms = []string{FormNote, FormPage, FormRelation, FormCode}

// storableForms are the forms a Page record on disk may actually carry — a
// write to any other form is refused (relation lives in the graph, code is a
// view).
var storableForms = []string{FormNote, FormPage}

// ValidType reports whether t is one of the OKF page types kb writes.
func ValidType(t string) bool { return slices.Contains(pageTypes, t) }

// ValidForm reports whether f is a recognized record form (the reader
// vocabulary: note|page|relation|code).
func ValidForm(f string) bool { return slices.Contains(forms, f) }

// StorableForm reports whether f is a form a Page may be WRITTEN as (note|page).
func StorableForm(f string) bool { return slices.Contains(storableForms, f) }

// EffForm is the page's form with the legacy default applied: a record with no
// form: reads as a page.
func (p *Page) EffForm() string {
	if p.Form == "" {
		return FormPage
	}
	return p.Form
}

// ParsePage reads a page file: a YAML frontmatter block followed by the
// markdown body. Same permissive framing as skills.ParseFrontmatter —
// unknown fields are ignored, kb reads the world's pages, it doesn't lint
// them.
func ParsePage(slug string, b []byte) (*Page, error) {
	s := string(b)
	if !strings.HasPrefix(s, "---\n") && !strings.HasPrefix(s, "---\r\n") {
		return nil, fmt.Errorf("kb: %s: no frontmatter block", slug)
	}
	_, rest, _ := strings.Cut(s, "\n")
	fm, body, ok := strings.Cut(rest, "\n---")
	if !ok {
		return nil, fmt.Errorf("kb: %s: unterminated frontmatter block", slug)
	}
	var p Page
	if err := yaml.Unmarshal([]byte(fm), &p); err != nil {
		return nil, fmt.Errorf("kb: %s: frontmatter: %w", slug, err)
	}
	p.Slug = slug
	// Legacy records predate the form facet; they read as pages. The writer
	// always emits form:, so this default only ever fills an older record.
	if p.Form == "" {
		p.Form = FormPage
	}
	// Drop the newline that closed the fence and one blank separator line.
	body = strings.TrimPrefix(body, "\r")
	body = strings.TrimPrefix(body, "\n")
	p.Body = strings.TrimRight(strings.TrimPrefix(body, "\n"), "\n")
	return &p, nil
}

// Marshal renders the page back to its on-disk form.
func (p *Page) Marshal() ([]byte, error) {
	fm, err := yaml.Marshal(p)
	if err != nil {
		return nil, err
	}
	var b strings.Builder
	b.WriteString("---\n")
	b.Write(fm)
	b.WriteString("---\n")
	if body := strings.TrimSpace(p.Body); body != "" {
		b.WriteString("\n")
		b.WriteString(body)
		b.WriteString("\n")
	}
	return []byte(b.String()), nil
}

// Slugify derives a filename stem from a title: lowercase, runs of
// non-alphanumerics collapsed to single dashes, capped for sane filenames.
func Slugify(title string) string {
	var b strings.Builder
	dash := true // suppress a leading dash
	for _, r := range strings.ToLower(title) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			dash = false
		default:
			if !dash {
				b.WriteByte('-')
				dash = true
			}
		}
	}
	s := strings.Trim(b.String(), "-")
	if len(s) > 64 {
		s = strings.Trim(s[:64], "-")
	}
	if s == "" {
		return "page"
	}
	return s
}
