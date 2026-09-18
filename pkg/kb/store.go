package kb

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/qiangli/yoke/git"
	"github.com/qiangli/yoke/pkg/ref"
)

// Store is the host-scope kb directory: one wiki of OKF-style pages shared
// by every agent tool and every repo on this machine. Mutations are atomic
// per-page writes plus an O_APPEND journal line (the same lock-free
// multi-writer model as the repo graph's contrib.jsonl), so any number of
// agents write concurrently without a lock.
//
// Layout:
//
//	<dir>/index.md       generated one-line-per-page index (the always-load surface)
//	<dir>/pages/<slug>.md the concept pages
//	<dir>/journal.jsonl  append-only mutation log (provenance/audit, future sync feed)
//	<dir>/.git           best-effort history/blame via the pure-Go git package
type Store struct {
	dir string
	// owner, when set, scopes reads to a single principal: Load and List
	// return only pages this principal wrote (Source.Tool match). It is set
	// for the AGENT ring, whose store is owner-only — another principal
	// sharing the physical directory sees nothing, not an error. Empty (the
	// repo and host rings) means every page is visible.
	owner string
}

// RepoSub is the committed per-repo kb: docs/kb/, inside the repo and CHECKED
// IN — the structured replacement for ad-hoc docs/*.md notes, for knowledge that
// is TRUE OF THIS REPO (its gate command, its conventions) and should travel with
// the clone. The host store (DefaultDir) keeps cross-repo / this-machine
// knowledge. Same wiki format at both scopes; `kb search --federate` reads across.
// Visible on purpose (not hidden under .bashy/), because it replaces a file a
// human reads and reviews.
const RepoSub = "docs/kb"

// DefaultDir resolves the HOST store location: $BASHY_KB_DIR, else ~/.bashy/kb
// (the host data dotdir, beside ~/.bashy/weave).
func DefaultDir() string {
	if d := strings.TrimSpace(os.Getenv("BASHY_KB_DIR")); d != "" {
		return d
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".bashy-kb"
	}
	return filepath.Join(home, ".bashy", "kb")
}

// AgentDataDirEnv names the per-identity data directory bashy relocates for a
// ycode-family agent (agentlaunch.YcodeDataDir sets it at launch). The AGENT
// ring lives under it. kb reads the env rather than importing agentlaunch, so
// pkg/kb stays an import leaf (scope/bus/git only) — this reuses the directory
// the launcher already derived, it never recomputes it.
const AgentDataDirEnv = "YCODE_DATA_DIR"

// AgentRingDir resolves the agent ring root: <agent-data>/kb, or "" when this
// process was not launched with a per-agent store (AgentDataDirEnv unset).
func AgentRingDir() string {
	d := strings.TrimSpace(os.Getenv(AgentDataDirEnv))
	if d == "" {
		return ""
	}
	return filepath.Join(d, "kb")
}

// Open returns a Store rooted at dir ("" = DefaultDir). The directory is
// created lazily on first write; Open never fails on a missing store.
func Open(dir string) *Store {
	if dir == "" {
		dir = DefaultDir()
	}
	return &Store{dir: dir}
}

// OpenAgentRing opens the agent ring at dir for principal owner. Reads are
// scoped to owner's own pages (Source.Tool match), so another principal
// sharing the directory sees nothing. Writes are unchanged — buildPage stamps
// Source.Tool, which is the same owner.
func OpenAgentRing(dir, owner string) *Store {
	s := Open(dir)
	s.owner = strings.TrimSpace(owner)
	return s
}

// ownedBy reports whether page p was written by principal owner.
func ownedBy(p *Page, owner string) bool {
	return p != nil && p.Source != nil && p.Source.Tool == owner
}

// Dir returns the store root.
func (s *Store) Dir() string { return s.dir }

func (s *Store) pagesDir() string            { return filepath.Join(s.dir, "pages") }
func (s *Store) PagePath(slug string) string { return filepath.Join(s.pagesDir(), slug+".md") }
func (s *Store) indexPath() string           { return filepath.Join(s.dir, "index.md") }
func (s *Store) journalPath() string         { return filepath.Join(s.dir, "journal.jsonl") }

// Load reads one page by slug. On an owner-scoped store (the agent ring) a page
// written by another principal is invisible: Load reports os.ErrNotExist, the
// same as an absent page, so another principal sees nothing rather than an
// error that would reveal the page exists.
func (s *Store) Load(slug string) (*Page, error) {
	b, err := os.ReadFile(s.PagePath(slug))
	if err != nil {
		return nil, err
	}
	p, err := ParsePage(slug, b)
	if err != nil {
		return nil, err
	}
	if s.owner != "" && !ownedBy(p, s.owner) {
		return nil, os.ErrNotExist
	}
	return p, nil
}

// List reads every page, sorted by slug. A corrupt page is skipped rather
// than failing the whole read (mirror of the journal-replay tolerance).
func (s *Store) List() ([]*Page, error) {
	entries, err := os.ReadDir(s.pagesDir())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []*Page
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".md") {
			continue
		}
		slug := strings.TrimSuffix(name, ".md")
		p, err := s.Load(slug)
		if err != nil {
			// On an owner-scoped store this also skips another principal's
			// pages (Load reports them as absent).
			continue
		}
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Slug < out[j].Slug })
	return out, nil
}

// Write persists a page (atomic temp+rename), stamps timestamps, appends a
// journal record, regenerates the index, and best-effort git-commits the
// store. op names the mutation for the journal ("add", "update",
// "supersede", "validate").
func (s *Store) Write(p *Page, op string) error {
	if p.Slug == "" {
		return fmt.Errorf("kb: page has no slug")
	}
	now := time.Now().UTC().Format(time.RFC3339)
	if p.Created == "" {
		p.Created = now
	} else {
		p.Updated = now
	}
	if p.Status == "" {
		p.Status = StatusCandidate
	}
	// Mint the two derived handles ON WRITE ONLY: an id the page keeps for
	// life, and the next running number in this ring. A read never mints — a
	// repo-ring read must not dirty the checkout.
	if p.ID == "" {
		p.ID = uuid.Must(uuid.NewV7()).String()
	}
	if p.Seq == 0 {
		if max, err := s.MaxSeq(); err == nil {
			p.Seq = max + 1
		}
	}
	// The writer always emits form:. A record built without one (or an older
	// page loaded before the facet existed) is a page.
	if p.Form == "" {
		p.Form = FormPage
	}
	if err := os.MkdirAll(s.pagesDir(), 0o755); err != nil {
		return err
	}
	b, err := p.Marshal()
	if err != nil {
		return err
	}
	if err := atomicWrite(s.PagePath(p.Slug), b); err != nil {
		return err
	}
	s.journal(op, p.Slug, p.ID)
	if err := s.RebuildIndex(); err != nil {
		return err
	}
	s.gitSnapshot(fmt.Sprintf("kb: %s %s", op, p.Slug))
	return nil
}

// atomicWrite is temp+rename in the target dir, so readers never observe a
// partial page.
func atomicWrite(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".kb-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return err
	}
	err = os.Rename(name, path)
	// Windows rename-over-existing fails with a sharing violation while any
	// concurrent reader/writer holds the target open; the window is brief,
	// so retry with backoff instead of failing the lock-free write model.
	if err != nil && runtime.GOOS == "windows" {
		for i := 0; i < 50 && err != nil; i++ {
			time.Sleep(10 * time.Millisecond)
			err = os.Rename(name, path)
		}
	}
	if err != nil {
		os.Remove(name)
		return err
	}
	return nil
}

// journalRecord is one appended mutation line — provenance for the audit
// trail and the future cloudbox sync feed.
type journalRecord struct {
	Op      string    `json:"op"`
	Slug    string    `json:"slug"`
	ID      string    `json:"id,omitempty"` // the page's uuid, so a renamed or superseded slug still joins
	Tool    string    `json:"tool,omitempty"`
	Host    string    `json:"host,omitempty"`
	Episode string    `json:"episode,omitempty"`
	At      time.Time `json:"at"`
}

// journal appends one record. O_APPEND keeps concurrent multi-agent writes
// safe without a lock; failures are swallowed — the page write is the
// operation, the journal is the trail.
func (s *Store) journal(op, slug, id string) {
	rec := journalRecord{Op: op, Slug: slug, ID: id, Tool: ToolID(), Host: HostID(), Episode: EpisodeID(), At: time.Now().UTC()}
	b, err := json.Marshal(rec)
	if err != nil {
		return
	}
	f, err := os.OpenFile(s.journalPath(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(append(b, '\n'))
}

// JournalTail returns the last n raw journal lines.
func (s *Store) JournalTail(n int) ([]string, error) {
	b, err := os.ReadFile(s.journalPath())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if len(lines) == 1 && lines[0] == "" {
		return nil, nil
	}
	if n > 0 && len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return lines, nil
}

// RebuildIndex regenerates index.md: one line per live page (validated
// first, then candidate, then stale; superseded pages are reachable only
// through their successor's link). This is the always-load surface — an
// agent reads it whole, then opens only the pages whose description
// matches the task.
func (s *Store) RebuildIndex() error {
	pages, err := s.List()
	if err != nil {
		return err
	}
	rank := map[string]int{StatusValidated: 0, StatusCandidate: 1, StatusStale: 2}
	var live []*Page
	for _, p := range pages {
		if p.Status == StatusSuperseded {
			continue
		}
		live = append(live, p)
	}
	sort.SliceStable(live, func(i, j int) bool {
		ri, rj := rank[live[i].Status], rank[live[j].Status]
		if ri != rj {
			return ri < rj
		}
		return live[i].Slug < live[j].Slug
	})
	var b strings.Builder
	b.WriteString("# kb index\n\n")
	fmt.Fprintf(&b, "%d page(s). Search: `bashy kb search <query>` — check before starting a task; `bashy kb retro` after. Pages live under pages/.\n\n", len(live))
	for _, p := range live {
		fmt.Fprintf(&b, "- %s[%s](pages/%s.md) `%s/%s` %s — %s\n", seqTag(p), p.Slug, p.Slug, p.Status, p.Type, p.Title, p.Description)
	}
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return err
	}
	return atomicWrite(s.indexPath(), []byte(b.String()))
}

// seqTag renders the running number a listing shows beside the slug ("#22 "),
// or nothing for a page that has not been written since seq existed. It is a
// display of the handle, never the ref: the ref stays kb:<slug>.
func seqTag(p *Page) string {
	if p.Seq == 0 {
		return ""
	}
	return fmt.Sprintf("#%d ", p.Seq)
}

// MaxSeq is the highest running number in this ring (0 when none). Superseded
// pages count: a number is never reused, so a tombstone keeps its seq.
func (s *Store) MaxSeq() (int, error) {
	pages, err := s.List()
	if err != nil {
		return 0, err
	}
	max := 0
	for _, p := range pages {
		if p.Seq > max {
			max = p.Seq
		}
	}
	return max, nil
}

// ErrAmbiguous marks a handle that matches MORE than one page in a ring — a
// uuid prefix too short, or a seq two branches each minted (doctor reports it).
// It is deliberately not os.ErrNotExist: the page is not absent, the query is
// under-specified, and the message names the candidates.
var ErrAmbiguous = errors.New("kb: ambiguous")

// LoadByHandle reads one page by whichever handle the local part spells
// (ref.ShapeOf): seq → the running number; uid → the uuid, exact then unique
// prefix; slug → Load. Absent is os.ErrNotExist exactly as Load reports it;
// more than one match wraps ErrAmbiguous.
func (s *Store) LoadByHandle(local string) (*Page, error) {
	local = strings.TrimPrefix(strings.TrimSpace(local), "#")
	shape := ref.ShapeOf(local)
	if shape == ref.ShapeSlug {
		return s.Load(local)
	}
	pages, err := s.List()
	if err != nil {
		return nil, err
	}
	var hits []*Page
	switch shape {
	case ref.ShapeSeq:
		n, _ := strconv.Atoi(local)
		for _, p := range pages {
			if p.Seq == n {
				hits = append(hits, p)
			}
		}
	case ref.ShapeUID:
		want := strings.ToLower(local)
		for _, p := range pages {
			if p.ID == want {
				return p, nil
			}
		}
		for _, p := range pages {
			if strings.HasPrefix(p.ID, want) {
				hits = append(hits, p)
			}
		}
	}
	switch len(hits) {
	case 0:
		// A hex-looking slug (`cafe`, `deadbeef`) is still a slug on disk.
		if p, err := s.Load(local); err == nil {
			return p, nil
		}
		return nil, os.ErrNotExist
	case 1:
		return hits[0], nil
	}
	names := make([]string, 0, len(hits))
	for _, p := range hits {
		names = append(names, fmt.Sprintf("#%d %s %s", p.Seq, p.ID, p.Slug))
	}
	return nil, fmt.Errorf("%w — kb:%s matches %d pages:\n  %s", ErrAmbiguous, local, len(hits), strings.Join(names, "\n  "))
}

// gitSnapshot best-effort commits the store via the pure-Go git package
// (never shells out): auto-init on first use, author = tool@host. Any
// failure is silently skipped — journal.jsonl remains the durable trail.
func (s *Store) gitSnapshot(msg string) {
	if _, err := os.Stat(filepath.Join(s.dir, ".git")); err != nil {
		// No own repo yet. If an ANCESTOR already versions this dir — the
		// committed repo store, docs/kb/ inside a project — do NOT nest a .git:
		// the parent repo is the version control, and a nested repo would make
		// git treat docs/kb/ as an embedded repo whose files never track. The
		// journal.jsonl + the parent's own commits are the trail. Only the
		// otherwise-unversioned host store (~/.bashy/kb) self-inits.
		if repoRootOf(filepath.Dir(s.dir)) != "" {
			return
		}
		if _, err := git.Init(git.InitOptions{Path: s.dir}); err != nil {
			return
		}
	}
	if _, err := git.Add(git.AddOptions{RepoPath: s.dir, All: true}); err != nil {
		return
	}
	tool, host := ToolID(), HostID()
	_, _ = git.Commit(git.CommitOptions{
		RepoPath:    s.dir,
		Message:     msg,
		AuthorName:  tool,
		AuthorEmail: tool + "@" + host,
	})
}

// --- identity ------------------------------------------------------------

// ToolID identifies the contributing agent tool. Best-effort, never fails:
// weave workers carry WEAVE_AGENT, bashy sessions may set BASHY_AGENT_ID,
// Claude Code sets CLAUDECODE.
func ToolID() string {
	for _, k := range []string{"WEAVE_AGENT", "BASHY_AGENT_ID", "BASHY_AGENT"} {
		if v := strings.TrimSpace(os.Getenv(k)); v != "" {
			return v
		}
	}
	if os.Getenv("CLAUDECODE") != "" {
		return "claude"
	}
	for _, k := range []string{"USER", "LOGNAME"} {
		if v := strings.TrimSpace(os.Getenv(k)); v != "" {
			return v
		}
	}
	return "agent"
}

// HostID is the host identity a page is stamped with: the
// cloudbox-registered agent name when this machine is paired (outpost's
// agent.json), else the hostname. Stamping the paired identity from day one
// keeps the later cloudbox sync purely additive.
func HostID() string {
	if home, err := os.UserHomeDir(); err == nil {
		path := filepath.Join(home, ".config", "outpost", "agent.json")
		if b, err := os.ReadFile(path); err == nil {
			var conf struct {
				AgentName string `json:"agent_name"`
			}
			if json.Unmarshal(b, &conf) == nil && strings.TrimSpace(conf.AgentName) != "" {
				return strings.TrimSpace(conf.AgentName)
			}
		}
	}
	if h, err := os.Hostname(); err == nil && h != "" {
		return h
	}
	return "host"
}

// EpisodeID ties a mutation to a run/session when the launcher provides one.
func EpisodeID() string {
	for _, k := range []string{"BASHY_EPISODE", "BASHY_SESSION", "WEAVE_ID"} {
		if v := strings.TrimSpace(os.Getenv(k)); v != "" {
			return v
		}
	}
	return ""
}
