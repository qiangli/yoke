package memory

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const defaultRecallLimit = 10

type JSONLStore struct {
	dir string
	mu  sync.Mutex
}

func NewJSONLStore(dir string) *JSONLStore {
	return &JSONLStore{dir: dir}
}

func (s *JSONLStore) Caps() Caps {
	return Caps{Persistent: true}
}

func (s *JSONLStore) path() string {
	return filepath.Join(s.dir, "memory.jsonl")
}

func (s *JSONLStore) Remember(ctx context.Context, o Observation) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if o.CreatedAt.IsZero() {
		o.CreatedAt = time.Now().UTC()
	}
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return err
	}
	b, err := json.Marshal(o)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := os.OpenFile(s.path(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(b)
	return err
}

func (s *JSONLStore) Recall(ctx context.Context, q Query) ([]Observation, error) {
	all, err := s.ReadAll(ctx)
	if err != nil {
		return nil, err
	}
	limit := q.Limit
	if limit <= 0 {
		limit = defaultRecallLimit
	}
	type hit struct {
		o     Observation
		score float64
	}
	hits := make([]hit, 0, len(all))
	for _, o := range all {
		score := scoreObservation(o, q)
		if score <= 0 && (len(q.Files) > 0 || q.Title != "" || len(q.Tags) > 0 || q.IssueID > 0) {
			continue
		}
		hits = append(hits, hit{o: o, score: score})
	}
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].score != hits[j].score {
			return hits[i].score > hits[j].score
		}
		return hits[i].o.CreatedAt.After(hits[j].o.CreatedAt)
	})
	if len(hits) > limit {
		hits = hits[:limit]
	}
	out := make([]Observation, 0, len(hits))
	for _, h := range hits {
		out = append(out, h.o)
	}
	return out, nil
}

func (s *JSONLStore) ReadAll(ctx context.Context) ([]Observation, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f, err := os.Open(s.path())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []Observation
	sc := bufio.NewScanner(f)
	buf := make([]byte, 0, 64*1024)
	sc.Buffer(buf, 1024*1024)
	for sc.Scan() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var o Observation
		if err := json.Unmarshal([]byte(line), &o); err != nil {
			continue
		}
		out = append(out, o)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func scoreObservation(o Observation, q Query) float64 {
	score := fileJaccard(o.FilesTouched, q.Files)
	if q.Title != "" && titleMatches(strings.TrimSpace(o.Title+" "+o.Summary), q.Title) {
		score += 0.35
	}
	score += 0.2 * float64(tagMatches(o.Tags, q.Tags))
	if q.IssueID > 0 && o.IssueID == q.IssueID {
		score += 1
	}
	return score
}

func fileJaccard(a, b []string) float64 {
	aa := stringSet(a)
	bb := stringSet(b)
	if len(aa) == 0 || len(bb) == 0 {
		return 0
	}
	inter := 0
	for k := range aa {
		if bb[k] {
			inter++
		}
	}
	union := len(aa) + len(bb) - inter
	if union == 0 {
		return 0
	}
	return float64(inter) / float64(union)
}

func tagMatches(a, b []string) int {
	aa := stringSet(a)
	matches := 0
	for _, tag := range b {
		if aa[strings.ToLower(strings.TrimSpace(tag))] {
			matches++
		}
	}
	return matches
}

func stringSet(vals []string) map[string]bool {
	m := make(map[string]bool, len(vals))
	for _, v := range vals {
		v = strings.ToLower(strings.TrimSpace(v))
		if v != "" {
			m[v] = true
		}
	}
	return m
}

func titleMatches(a, b string) bool {
	a = strings.ToLower(strings.TrimSpace(a))
	b = strings.ToLower(strings.TrimSpace(b))
	if a == "" || b == "" {
		return false
	}
	return strings.Contains(a, b) || strings.Contains(b, a)
}
