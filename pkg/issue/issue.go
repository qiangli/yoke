// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

// Package issue is the project's ISSUE REGISTER: the durable, committed record of
// what is wrong, what is wanted, and what is required — before anyone starts work.
//
// # The hole it fills
//
// bashy could track work an agent was ACTIVELY DOING (`weave`, a per-machine queue of
// runnable items with a workspace, a branch and a verify command) and work a conductor
// was PLANNING RIGHT NOW (`sprint`, a host-global continuity board). It could not
// record a bug nobody has triaged, a requirement nobody has scheduled, or a feature
// somebody merely asked for. Those lived as bullet points in docs/TODO.md — invisible
// to every verb, unqueryable, and impossible to close.
//
// `sdlc issue` looked like this, and wasn't: SaveLocalIssue() writes {timestamp, title,
// body} into .bashy/GENERATED/sdlc/issues and has NO counterpart anywhere in the tree —
// no List, no Load, no Close. Nothing ever reads it back. It is a drop box, not a
// register. This package is the read side that was never built, plus the fields that
// make a record formal enough to act on.
//
// # Three decisions, and why
//
// COMMITTED, NOT HOST-LOCAL. The store is `.bashy/issues/` inside the repo — source,
// not scratch. A requirement must travel with the clone, show up in a diff, be
// reviewable in a pull request, survive the machine it was typed on, and need no forge
// to exist. That is the difference between this and weave's queue (per-machine,
// ephemeral, execution state), and it is why the store is NOT under `.bashy/generated/`.
//
// CONTENT-ADDRESSED IDs, NOT A COUNTER. Because the register is committed, it MERGES.
// A monotonic `#1, #2, #3` counter is a merge-conflict generator: two branches both
// file "#7" and one of them has to be renumbered, breaking every reference to it. This
// is precisely why git-bug and Fossil use hashes, and why GitHub can get away with
// integers (a server hands them out; a git repo has no server). Short hex, referenced
// by unique prefix, exactly like a git commit.
//
// ONE HOME REPO, MANY REFS. An issue is FILED in one repo — the one whose `.bashy/`
// holds it — but its scope may span the project. `Refs` names the other modules or
// repos it touches. This is the same path-set boundary the claim and handoff verbs use:
// the .git root is where the record lives, not the limit of what it is about.
package issue

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Dir is the register's location inside the repo. Deliberately NOT under
// `.bashy/generated/` — that tree is derived scratch, and this is source.
const Dir = ".bashy/issues"

// Kinds — what sort of thing is being recorded: WHY the item exists, never
// how big it is (docs/work-tracking-model.md §2).
//
// The vocabulary is OPEN. These four are the suggested words — the ones the
// help text and `schema` print — not a closed list: `doc`, `enhancement`,
// `spike`, `chore`, `question` are all accepted as written, and a new word
// never needs a code change. What is checked is the SHAPE of the word
// (ValidWord); what keeps a vocabulary coherent is discovery (`todo kinds`
// shows the words in use with counts, so a writer sees the spelling already
// taken before inventing one), not enforcement.
const (
	KindBug         = "bug"         // it is broken
	KindFeature     = "feature"     // it would be good to have
	KindEnhancement = "enhancement" // it exists and should be better
	KindDoc         = "doc"         // words, not code: a page, a runbook, a README
	KindRequirement = "requirement" // it must hold (a constraint, a compliance obligation)
	KindTask        = "task"        // it must be done (chores, migrations)
	KindChore       = "chore"       // upkeep with no user-visible outcome (pins, CI, cleanup)
	KindSpike       = "spike"       // find out; the deliverable is knowledge
	KindTest        = "test"        // coverage or a gate, not a behaviour change
	KindRefactor    = "refactor"    // same behaviour, better shape
	KindQuestion    = "question"    // needs an answer or a decision before work
)

// SuggestedKinds is the common vocabulary printed by help text — a menu of
// familiar words, not a whitelist: any other word of the same shape is
// accepted as written.
var SuggestedKinds = []string{
	KindBug, KindFeature, KindEnhancement, KindDoc, KindTask, KindChore,
	KindSpike, KindTest, KindRefactor, KindRequirement, KindQuestion,
}

// KindHelp is the one-line flag description shared by every front door that
// takes a kind.
const KindHelp = "why it exists — one word; common: bug|feature|enhancement|doc|task|chore|spike|test|refactor|requirement|question (any other word is accepted; `todo kinds` shows the words in use)"

// LabelHelp likewise, for --label.
const LabelHelp = "a label word — an area, OS or surface such as windows, app, release, docs (repeatable or comma-separated; `todo labels` shows the words in use)"

// Statuses — the triage ladder, and nothing more.
//
// Deliberately three, not seven. "in progress" and "done" are NOT statuses here: work
// in flight is weave's business, and duplicating it would create two sources of truth
// that disagree the moment an agent crashes. The register says whether an issue has
// been ACCEPTED and whether it is SETTLED; weave says whether it is running.
const (
	StatusOpen    = "open"    // filed, not yet triaged
	StatusTriaged = "triaged" // accepted, scoped, ready to be worked
	StatusClosed  = "closed"  // settled (fixed, shipped, or declined — see Resolution)
)

var (
	kinds    = SuggestedKinds
	statuses = []string{StatusOpen, StatusTriaged, StatusClosed}
)

// ValidKind reports whether k is a well-formed kind word. Any word of the
// right shape is valid — membership in Kinds() is a suggestion, not a rule.
func ValidKind(k string) bool   { return ValidWord(k) }
func ValidStatus(s string) bool { return slices.Contains(statuses, s) }

// Kinds returns the SUGGESTED kind vocabulary (SuggestedKinds — help text and
// schema); the words actually in use in a store come from KindsInUse.
func Kinds() []string    { return append([]string(nil), kinds...) }
func Statuses() []string { return append([]string(nil), statuses...) }

// WordMaxLen bounds a kind or label: a classifier is a word, not a sentence.
const WordMaxLen = 32

// ValidWord is the one shape rule for a classifier word (a kind or a label):
// lowercase ASCII letters and digits, with `-` `_` `.` allowed inside,
// starting with a letter or digit, at most WordMaxLen bytes. One token: no
// spaces, no uppercase (so `Bug` and `bug` cannot become two kinds).
func ValidWord(w string) bool {
	if w == "" || len(w) > WordMaxLen {
		return false
	}
	for i := 0; i < len(w); i++ {
		c := w[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		case i > 0 && (c == '-' || c == '_' || c == '.'):
		default:
			return false
		}
	}
	return true
}

// WordRule is the shape rule in one line, for error messages and help.
const WordRule = "one lowercase word: [a-z0-9][a-z0-9._-]*, at most 32 bytes"

// NormalizeWord trims and lowercases a classifier word so `Bug ` files as
// `bug`; it returns an error naming the rule when the result is still not a
// word. Lowercasing is the only normalisation — no synonym table.
func NormalizeWord(w string) (string, error) {
	w = strings.ToLower(strings.TrimSpace(w))
	if !ValidWord(w) {
		return "", fmt.Errorf("%q is not a valid word (%s)", w, WordRule)
	}
	return w, nil
}

// NormalizeWords normalises a list of classifier words (labels), accepting
// comma-separated entries, dropping empties and duplicates, keeping order.
func NormalizeWords(in []string) ([]string, error) {
	var out []string
	seen := map[string]bool{}
	for _, raw := range in {
		for _, w := range strings.Split(raw, ",") {
			if strings.TrimSpace(w) == "" {
				continue
			}
			n, err := NormalizeWord(w)
			if err != nil {
				return nil, err
			}
			if !seen[n] {
				seen[n] = true
				out = append(out, n)
			}
		}
	}
	return out, nil
}

// AddLabels / RemoveLabels edit an issue's labels set-wise, preserving order.
func AddLabels(it *Issue, add []string) {
	for _, l := range add {
		if !slices.Contains(it.Labels, l) {
			it.Labels = append(it.Labels, l)
		}
	}
}

func RemoveLabels(it *Issue, rm []string) {
	it.Labels = slices.DeleteFunc(it.Labels, func(l string) bool { return slices.Contains(rm, l) })
	if len(it.Labels) == 0 {
		it.Labels = nil
	}
}

// WordCount is one row of a vocabulary-in-use listing.
type WordCount struct {
	Word  string `json:"word"`
	Count int    `json:"count"`
}

// CountWords tallies classifier words across items — the discovery half of an
// open vocabulary. Sorted by count (desc) then word, so the established
// spelling comes first and a lone typo sits visibly beside it.
func CountWords(items []*Issue, pick func(*Issue) []string) []WordCount {
	counts := map[string]int{}
	for _, it := range items {
		for _, w := range pick(it) {
			if w != "" {
				counts[w]++
			}
		}
	}
	out := make([]WordCount, 0, len(counts))
	for w, n := range counts {
		out = append(out, WordCount{Word: w, Count: n})
	}
	slices.SortFunc(out, func(a, b WordCount) int {
		if a.Count != b.Count {
			return b.Count - a.Count
		}
		return strings.Compare(a.Word, b.Word)
	})
	return out
}

// KindsInUse and LabelsInUse are the two vocabularies a store actually holds.
func KindsInUse(items []*Issue) []WordCount {
	return CountWords(items, func(it *Issue) []string { return []string{it.Kind} })
}

func LabelsInUse(items []*Issue) []WordCount {
	return CountWords(items, func(it *Issue) []string { return it.Labels })
}

// Issue is one record in the register.
//
// It is a markdown file with a YAML frontmatter block — the same shape kb uses for its
// pages — so it is human-editable, git-diffable, and reviewable in a pull request
// without any tool at all. A register nobody can read with `cat` is a register that
// rots.
type Issue struct {
	ID    string `yaml:"id" json:"id"`
	Kind  string `yaml:"kind" json:"kind"`
	Title string `yaml:"title" json:"title"`

	// Seq is a stable, human-friendly running number within a store (like an issue
	// number: #1, #2, …), assigned on creation and never reused. The content-addressed
	// ID stays the canonical identity; Seq is the short handle a human types to
	// reference an item ("todo show 3"). 0 means unassigned (legacy items get one
	// backfilled in creation order).
	Seq int `yaml:"seq,omitempty" json:"seq,omitempty"`

	Status string `yaml:"status" json:"status"`
	// Stage is the SDLC stage the WORK will belong to, using the atlas vocabulary
	// (plan|code|test|deploy). Set at triage: deciding what part of the lifecycle an
	// issue belongs to IS triage.
	Stage    string `yaml:"stage,omitempty" json:"stage,omitempty"`
	Priority string `yaml:"priority,omitempty" json:"priority,omitempty"`

	// Refs are other modules or repos this issue touches. The issue lives in ONE
	// repo; it may be ABOUT several. A bug whose fix spans a library and its
	// consumer is one issue, not two — and this is the field that says so.
	Refs   []string `yaml:"refs,omitempty" json:"refs,omitempty"`
	Labels []string `yaml:"labels,omitempty" json:"labels,omitempty"`

	Reporter string    `yaml:"reporter,omitempty" json:"reporter,omitempty"`
	Created  time.Time `yaml:"created" json:"created"`

	// Weave is the weave queue item implementing this issue on some machine, once
	// one exists. The register is the durable truth; weave is the execution. This is
	// the join between them, and it is what stops the two from becoming parallel
	// universes.
	Weave     int64      `yaml:"weave,omitempty" json:"weave,omitempty"`
	Due       *time.Time `yaml:"due,omitempty" json:"due,omitempty"`
	Recurring string     `yaml:"recurring,omitempty" json:"recurring,omitempty"`
	Assignee  string     `yaml:"assignee,omitempty" json:"assignee,omitempty"`

	// Sprint is the sprint card this item is a story of, once one claims it.
	// Exactly the shape and rationale as Weave above: the register is the
	// durable truth, the sprint is the time-box, and this is the join that
	// stops them becoming parallel universes.
	//
	// OPTIONAL AND NEVER REQUIRED. Absent means unlinked, which is a perfectly
	// valid item — `todo` does not require a sprint and must not start to. A
	// dangling reference is not an error either: a reader shows it, nothing
	// validates it at write time, because write-time validation is enforcement
	// by another name and these tools are deliberately uncoupled.
	Sprint int64 `yaml:"sprint,omitempty" json:"sprint,omitempty"`

	Closed     *time.Time `yaml:"closed,omitempty" json:"closed,omitempty"`
	Resolution string     `yaml:"resolution,omitempty" json:"resolution,omitempty"` // fixed | declined | duplicate | obsolete
	ClosedBy   string     `yaml:"closed_by,omitempty" json:"closed_by,omitempty"`

	Body string `yaml:"-" json:"body,omitempty"`
}

// NewID mints a short content-free hex id, git-style.
//
// Random, not sequential, and that is the point — see the package doc. Six bytes is
// 2^48: a repo would need on the order of ten million issues before a collision became
// likely, and Store.Add checks for one anyway.
func NewID() string {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		// A register that silently reuses ids is worse than one that refuses to file.
		panic("issue: no entropy for an id: " + err.Error())
	}
	return hex.EncodeToString(b)
}

// Parse reads an issue file: YAML frontmatter, then the markdown body.
func Parse(b []byte) (*Issue, error) {
	s := string(b)
	if !strings.HasPrefix(s, "---\n") && !strings.HasPrefix(s, "---\r\n") {
		return nil, fmt.Errorf("no frontmatter block")
	}
	_, rest, _ := strings.Cut(s, "\n")
	fm, body, ok := strings.Cut(rest, "\n---")
	if !ok {
		return nil, fmt.Errorf("unterminated frontmatter block")
	}
	var it Issue
	if err := yaml.Unmarshal([]byte(fm), &it); err != nil {
		return nil, fmt.Errorf("frontmatter: %w", err)
	}
	body = strings.TrimPrefix(body, "\r")
	body = strings.TrimPrefix(body, "\n")
	it.Body = strings.TrimRight(strings.TrimPrefix(body, "\n"), "\n")
	if it.Sprint == 0 {
		it.Sprint = sprintFromBody(it.Body)
	}
	return &it, nil
}

// sprintRef matches the sprint marker agents have been typing into item
// bodies by convention: "SPRINT: #87", "SPRINT #87", "RELATED: SPRINT #87".
var sprintRef = regexp.MustCompile(`(?i)\bsprint:?\s*#(\d+)`)

// sprintFromBody recovers the sprint link from the body when the frontmatter
// field is absent.
//
// The convention came FIRST and the field came second: eight items were filed
// against sprint #87 with the link hand-typed into prose on both sides, and
// nothing could answer "which items belong to #87?" without grepping. Deriving
// on read means every one of those items links itself the moment this ships —
// no migration, no bulk edit, and no agent has to learn a new spelling.
//
// The frontmatter field always wins when set, so an explicit link is never
// overridden by a stray mention. The first marker wins; a body discussing
// several sprints links to the one it leads with, which is how these items
// actually read.
func sprintFromBody(body string) int64 {
	m := sprintRef.FindStringSubmatch(body)
	if m == nil {
		return 0
	}
	n, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil || n <= 0 {
		return 0
	}
	return n
}

// Marshal renders the issue back to its on-disk form.
func (it *Issue) Marshal() ([]byte, error) {
	fm, err := yaml.Marshal(it)
	if err != nil {
		return nil, err
	}
	var b strings.Builder
	b.WriteString("---\n")
	b.Write(fm)
	b.WriteString("---\n")
	if body := strings.TrimSpace(it.Body); body != "" {
		b.WriteString("\n")
		b.WriteString(body)
		b.WriteString("\n")
	}
	return []byte(b.String()), nil
}

func (it *Issue) Open() bool { return it.Status != StatusClosed }

// Store is the register. Root is the base directory the register lives under; Sub
// is the subdirectory beneath it that holds the item files. Sub defaults to Dir
// (".bashy/issues") — the committed, per-repo register. A host-scoped, personal
// register (see pkg/todo) sets Root to a home directory and Sub to its own subtree,
// reusing every method here without duplicating the parse/marshal/store mechanics.
type Store struct {
	Root string
	Sub  string // "" → Dir (the committed per-repo register)
}

func New(repoRoot string) *Store { return &Store{Root: repoRoot} }

// Dir is the directory the store reads and writes, exported so a resource map
// can name the list that applies to a scope without recomputing it.
func (s *Store) Dir() string { return s.dir() }

func (s *Store) dir() string {
	sub := s.Sub
	if sub == "" {
		sub = Dir
	}
	return filepath.Join(s.Root, sub)
}

func (s *Store) path(it *Issue) string {
	return filepath.Join(s.dir(), it.ID+"-"+it.Slug()+".md")
}

// Slug is the readable half of the item's filename (`<id>-<slug>.md`): the
// title slugified. It is derived, never stored, so it follows a retitle — which
// is why the ID stays the identity and the slug is only a handle.
func (it *Issue) Slug() string { return slugify(it.Title) }

// List returns every issue in the register, newest first.
func (s *Store) List() ([]*Issue, error) {
	ents, err := os.ReadDir(s.dir())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil // an empty register is not an error; it is a new project
		}
		return nil, err
	}
	var out []*Issue
	for _, e := range ents {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(s.dir(), e.Name()))
		if err != nil {
			return nil, err
		}
		it, err := Parse(b)
		if err != nil {
			// One malformed file must not blind the register. Say which, keep going:
			// a hand-edited issue with a typo should not hide the other forty.
			fmt.Fprintf(os.Stderr, "issue: skipping %s: %v\n", e.Name(), err)
			continue
		}
		out = append(out, it)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Created.After(out[j].Created) })
	return out, nil
}

// Resolve finds an issue by id or unique PREFIX — the git convention. `issue show a3f2`
// beats `issue show a3f2c1d40b9e` for the same reason `git show a3f2` does.
func (s *Store) Resolve(ref string) (*Issue, error) {
	ref = strings.TrimPrefix(strings.TrimSpace(ref), "#")
	if ref == "" {
		return nil, fmt.Errorf("no issue given")
	}
	all, err := s.List()
	if err != nil {
		return nil, err
	}
	var hits []*Issue
	for _, it := range all {
		if it.ID == ref {
			return it, nil // an exact id always wins over a prefix
		}
		if strings.HasPrefix(it.ID, ref) {
			hits = append(hits, it)
		}
	}
	switch len(hits) {
	case 0:
		return nil, fmt.Errorf("no issue %q in the register (`bashy issue list`)", ref)
	case 1:
		return hits[0], nil
	}
	var ids []string
	for _, h := range hits {
		ids = append(ids, h.ID[:8]+" "+h.Title)
	}
	return nil, fmt.Errorf("%q is ambiguous — %d issues match:\n  %s", ref, len(hits), strings.Join(ids, "\n  "))
}

// Save writes an issue, creating the register if it does not exist.
// normalizeClassifiers applies the one shape rule to kind and labels on every
// write path (Add and Save alike), so a record on disk always holds words.
func normalizeClassifiers(it *Issue) error {
	if it.Kind == "" {
		it.Kind = KindTask
	}
	k, err := NormalizeWord(it.Kind)
	if err != nil {
		return fmt.Errorf("kind: %w (suggested: %s — any word of that shape is accepted)", err, strings.Join(kinds, ", "))
	}
	it.Kind = k
	labels, err := NormalizeWords(it.Labels)
	if err != nil {
		return fmt.Errorf("label: %w", err)
	}
	it.Labels = labels
	return nil
}

func (s *Store) Save(it *Issue) (string, error) {
	if err := normalizeClassifiers(it); err != nil {
		return "", err
	}
	if err := os.MkdirAll(s.dir(), 0o755); err != nil {
		return "", err
	}
	b, err := it.Marshal()
	if err != nil {
		return "", err
	}
	p := s.path(it)
	// A retitled issue must not leave its old file behind as a duplicate record.
	if old, err := s.find(it.ID); err == nil && old != "" && old != p {
		_ = os.Remove(old)
	}
	return p, os.WriteFile(p, b, 0o644)
}

func (s *Store) find(id string) (string, error) {
	ents, err := os.ReadDir(s.dir())
	if err != nil {
		return "", err
	}
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), id+"-") {
			return filepath.Join(s.dir(), e.Name()), nil
		}
	}
	return "", nil
}

// Add files a new issue.
func (s *Store) Add(it *Issue) (string, error) {
	if strings.TrimSpace(it.Title) == "" {
		return "", fmt.Errorf("a title is required: an issue nobody can identify is a note, not a record")
	}
	if it.Kind == "" {
		it.Kind = KindTask
	}
	if err := normalizeClassifiers(it); err != nil {
		return "", err
	}
	if it.ID == "" {
		it.ID = NewID()
	}
	if existing, _ := s.find(it.ID); existing != "" {
		return "", fmt.Errorf("issue %s already exists", it.ID)
	}
	it.Status = StatusOpen
	if it.Created.IsZero() {
		it.Created = time.Now().UTC()
	}
	return s.Save(it)
}

// Remove deletes an issue's file. The committed register prefers Close (a settled
// record still travels with the clone); a host-scoped personal register (pkg/todo)
// uses this to drop an item outright.
func (s *Store) Remove(it *Issue) error {
	p, err := s.find(it.ID)
	if err != nil {
		return err
	}
	if p == "" {
		return fmt.Errorf("issue %s not on the register", it.ID)
	}
	return os.Remove(p)
}

func slugify(s string) string {
	var b strings.Builder
	prevDash := false
	for _, r := range strings.ToLower(strings.TrimSpace(s)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			prevDash = false
		default:
			if !prevDash && b.Len() > 0 {
				b.WriteByte('-')
				prevDash = true
			}
		}
	}
	out := strings.Trim(b.String(), "-")
	if len(out) > 50 {
		out = strings.Trim(out[:50], "-")
	}
	if out == "" {
		return "issue"
	}
	return out
}
