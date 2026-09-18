package graphcmd

// The writable contribution layer: a durable, append-only, per-repo store that
// agentic tools ENRICH (notes, links, observations) — the "agentic wiki, built by
// agents, for agents, used by agents." It is deliberately SEPARATE from the
// derived code-graph cache (.agents/bashy/graph.json), which is rebuilt from source
// and overwritten wholesale — so contributions survive a code-graph rebuild
// (the clobber hazard, see docs/repo-knowledge-graph-design.md).
//
// Source of truth = an append-only JSON Lines log (O_APPEND, concurrency-safe, like
// weave's memory.jsonl). Reads replay the log, applying forgets (soft-delete) and
// last-writer-wins per deterministic id. No graph DB yet: the bonsai query index +
// code-graph unification is the next phase; here contributions are queried by
// recall/notes/pitfalls. Model-free throughout.

import (
	"bufio"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/qiangli/yoke/pkg/kb"
	"github.com/qiangli/yoke/pkg/scope"
)

// contribRel is the relation-form log under a kb ring. In the repo ring this is
// docs/kb/graph.jsonl, so authored relation claims are committed with the rest
// of the repository knowledge instead of disappearing under gitignored .agents.
const contribRel = "graph.jsonl"

// Contribution is one appended record. A single flexible shape covers every op so
// the log stays a plain JSONL stream; unused fields are omitempty.
type Contribution struct {
	ID         string    `json:"id"`
	Op         string    `json:"op"` // note|link|observe|forget
	By         string    `json:"by,omitempty"`
	At         time.Time `json:"at"`
	Source     string    `json:"source,omitempty"`
	Confidence string    `json:"confidence,omitempty"` // ASSERTED|EXTRACTED|INFERRED
	Episode    string    `json:"episode,omitempty"`

	// note / observe: the entity this is about. link: the source entity.
	Target   string `json:"target,omitempty"`
	TargetID string `json:"target_id,omitempty"`
	Text     string `json:"text,omitempty"` // note text or observation summary

	// link
	Relation string `json:"relation,omitempty"`
	Dst      string `json:"dst,omitempty"`
	DstID    string `json:"dst_id,omitempty"`

	// observe
	Kind    string            `json:"kind,omitempty"`    // build|test|run|execution|deploy|…
	Outcome string            `json:"outcome,omitempty"` // success|failure|note
	Data    map[string]string `json:"data,omitempty"`

	// forget directive (any that match retract a record)
	ForgetID      string `json:"forget_id,omitempty"`
	ForgetTarget  string `json:"forget_target,omitempty"`
	ForgetEpisode string `json:"forget_episode,omitempty"`
}

// store is the append-only contribution log for one kb ring.
type store struct{ path string }

func openStore(ringDir string) *store {
	return &store{path: filepath.Join(ringDir, contribRel)}
}

// append writes one record as a JSON line. O_APPEND makes concurrent writes from
// multiple agents safe without a lock (atomic for lines under PIPE_BUF).
func (s *store) append(c Contribution) error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(s.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	b, err := json.Marshal(c)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	_, err = f.Write(b)
	return err
}

// all reads every record in log order. A missing log is an empty slice, not error.
func (s *store) all() ([]Contribution, error) {
	f, err := os.Open(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	var out []Contribution
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var c Contribution
		if err := json.Unmarshal([]byte(line), &c); err != nil {
			continue // skip a corrupt line rather than fail the whole read
		}
		out = append(out, c)
	}
	return out, sc.Err()
}

// live replays the log into the current set of contributions: forgets soft-delete,
// and a repeated id (same note/link) is last-writer-wins. Returned in first-seen
// order for stable output.
func (s *store) live() ([]Contribution, error) {
	all, err := s.all()
	if err != nil {
		return nil, err
	}
	var forgets []Contribution
	latest := map[string]Contribution{}
	var order []string
	for _, c := range all {
		if c.Op == "forget" {
			forgets = append(forgets, c)
			continue
		}
		if _, seen := latest[c.ID]; !seen {
			order = append(order, c.ID)
		}
		latest[c.ID] = c
	}
	dead := func(c Contribution) bool {
		for _, f := range forgets {
			if f.ForgetID != "" && f.ForgetID == c.ID {
				return true
			}
			if f.ForgetTarget != "" && f.ForgetTarget == c.Target {
				return true
			}
			if f.ForgetEpisode != "" && f.ForgetEpisode == c.Episode {
				return true
			}
		}
		return false
	}
	out := make([]Contribution, 0, len(order))
	for _, id := range order {
		c := latest[id]
		if dead(c) {
			continue
		}
		out = append(out, c)
	}
	return out, nil
}

// contribID is a stable content hash. note/link records reuse the same id for the
// same content, so re-contributing is idempotent (last-writer-wins on replay).
func contribID(parts ...string) string {
	h := sha1.New()
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

func entityID(name string) string {
	kind := entityKind(name)
	h := sha1.New()
	h.Write([]byte(kind))
	h.Write([]byte{0})
	h.Write([]byte(name))
	return hex.EncodeToString(h.Sum(nil))[:16]
}

func entityKind(name string) string {
	if i := strings.IndexByte(name, ':'); i > 0 {
		prefix := strings.TrimSpace(name[:i])
		if prefix != "" && !strings.ContainsAny(prefix, `/\`) {
			return prefix
		}
	}
	return "entity"
}

// findRepoRoot walks up from start to the nearest directory containing .git so all
// agents in any subdir of a repo share ONE contribution store. Falls back to start.
func findRepoRoot(start string) string {
	dir := start
	for {
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return start
		}
		dir = parent
	}
}

func contribStoreDir(rc interface {
	Getenv(string) string
}) (string, error) {
	sc, err := scope.Resolve(scope.Options{
		RepoSub: kb.RepoSub,
		HostDir: func() (string, error) {
			if v := strings.TrimSpace(rc.Getenv("BASHY_KB_DIR")); v != "" {
				return v, nil
			}
			return kb.DefaultDir(), nil
		},
	})
	if err != nil {
		return "", err
	}
	return sc.Dir(), nil
}

// contribBy identifies the contributing agent/tool. Best-effort, never fails.
func contribBy(rc interface{ Getenv(string) string }) string {
	for _, k := range []string{"BASHY_AGENT_ID", "BASHY_AGENT", "USER", "LOGNAME"} {
		if v := strings.TrimSpace(rc.Getenv(k)); v != "" {
			return v
		}
	}
	return "agent"
}

func contribEpisode(rc interface{ Getenv(string) string }) string {
	for _, k := range []string{"BASHY_EPISODE", "BASHY_SESSION"} {
		if v := strings.TrimSpace(rc.Getenv(k)); v != "" {
			return v
		}
	}
	return ""
}
