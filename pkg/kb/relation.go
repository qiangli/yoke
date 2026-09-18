package kb

import (
	"bufio"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const RelationFile = "graph.jsonl"

var coreRelations = map[string]bool{
	"about":      true,
	"supersedes": true,
	"depends-on": true,
	"calls":      true,
	"tested-by":  true,
	"decided-in": true,
	"observed":   true,
}

type Relation struct {
	ID         string            `json:"id"`
	Op         string            `json:"op"`
	By         string            `json:"by,omitempty"`
	At         time.Time         `json:"at"`
	Source     string            `json:"source,omitempty"`
	Confidence string            `json:"confidence,omitempty"`
	Episode    string            `json:"episode,omitempty"`
	Target     string            `json:"target,omitempty"`
	TargetID   string            `json:"target_id,omitempty"`
	Text       string            `json:"text,omitempty"`
	Relation   string            `json:"relation,omitempty"`
	Dst        string            `json:"dst,omitempty"`
	DstID      string            `json:"dst_id,omitempty"`
	Kind       string            `json:"kind,omitempty"`
	Outcome    string            `json:"outcome,omitempty"`
	Data       map[string]string `json:"data,omitempty"`

	ForgetID      string `json:"forget_id,omitempty"`
	ForgetTarget  string `json:"forget_target,omitempty"`
	ForgetEpisode string `json:"forget_episode,omitempty"`
}

type RelationRing struct {
	Dir string
}

func RelationPath(dir string) string {
	return filepath.Join(dir, RelationFile)
}

func LegacyRepoContribPath(repoRoot string) string {
	return filepath.Join(repoRoot, ".agents", "bashy", "graph", "contrib.jsonl")
}

func RelationEntityID(kind, name string) string {
	h := sha1.New()
	h.Write([]byte(kind))
	h.Write([]byte{0})
	h.Write([]byte(name))
	return hex.EncodeToString(h.Sum(nil))[:16]
}

func CoreRelation(rel string) bool {
	return coreRelations[strings.TrimSpace(rel)]
}

func (r RelationRing) All() ([]Relation, error) {
	return readRelations(RelationPath(r.Dir))
}

func (r RelationRing) Live() ([]Relation, error) {
	all, err := r.All()
	if err != nil {
		return nil, err
	}
	return ReplayRelations(all), nil
}

func ReadLegacyRelations(repoRoot string) ([]Relation, error) {
	return readRelations(LegacyRepoContribPath(repoRoot))
}

func ReplayRelations(all []Relation) []Relation {
	var forgets []Relation
	latest := map[string]Relation{}
	var order []string
	for _, r := range all {
		if r.Op == "forget" {
			forgets = append(forgets, r)
			continue
		}
		if _, seen := latest[r.ID]; !seen {
			order = append(order, r.ID)
		}
		latest[r.ID] = r
	}
	dead := func(r Relation) bool {
		for _, f := range forgets {
			if f.ForgetID != "" && f.ForgetID == r.ID {
				return true
			}
			if f.ForgetTarget != "" && f.ForgetTarget == r.Target {
				return true
			}
			if f.ForgetEpisode != "" && f.ForgetEpisode == r.Episode {
				return true
			}
		}
		return false
	}
	out := make([]Relation, 0, len(order))
	for _, id := range order {
		r := latest[id]
		if !dead(r) {
			out = append(out, r)
		}
	}
	return out
}

func SearchRelations(rs []Relation, terms []string, k int) []Relation {
	if k <= 0 {
		k = DefaultK
	}
	var out []Relation
	for _, r := range rs {
		if relationMatches(r, terms) {
			out = append(out, r)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].At.Equal(out[j].At) {
			return out[i].At.After(out[j].At)
		}
		return out[i].ID < out[j].ID
	})
	if len(out) > k {
		out = out[:k]
	}
	return out
}

func RelationText(r Relation) string {
	switch r.Op {
	case "link":
		return "link " + r.Target + " " + r.Relation + " " + r.Dst
	case "observe":
		return strings.TrimSpace("observe " + r.Kind + "/" + r.Outcome + " " + r.Target + ": " + r.Text)
	default:
		return strings.TrimSpace("note " + r.Target + ": " + r.Text)
	}
}

func readRelations(path string) ([]Relation, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	var out []Relation
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var r Relation
		if json.Unmarshal([]byte(line), &r) != nil {
			continue
		}
		if r.TargetID == "" && r.Target != "" {
			r.TargetID = RelationEntityID(entityKind(r.Target), r.Target)
		}
		if r.DstID == "" && r.Dst != "" {
			r.DstID = RelationEntityID(entityKind(r.Dst), r.Dst)
		}
		out = append(out, r)
	}
	return out, sc.Err()
}

func relationMatches(r Relation, terms []string) bool {
	if len(terms) == 0 {
		return true
	}
	text := strings.ToLower(RelationText(r))
	for _, term := range terms {
		if strings.Contains(text, strings.ToLower(term)) {
			return true
		}
	}
	return false
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
