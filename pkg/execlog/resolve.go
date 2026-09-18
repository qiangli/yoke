// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package execlog

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/qiangli/yoke/pkg/ref"
)

// RegisterRefs installs the episode resolver into the uniform-ref registry,
// over the store at root. An empty root resolves through DefaultRoot() —
// the same ladder the writer and every reader use.
func RegisterRefs(g *ref.Registry, root string) {
	g.Register(ref.Episode, ref.ResolverFunc(func(id string) (ref.Node, error) {
		return resolveEpisode(root, id)
	}))
}

// resolveEpisode answers for one episode. There is no episode store: an
// episode is OBSERVED, as the shards `<root>/<YYYY-MM-DD>/<episode>.jsonl`
// it left across day dirs. Zero observed records is ref.ErrNotFound; a
// shard that exists but cannot be read is a read error, never "not found".
func resolveEpisode(root, id string) (ref.Node, error) {
	if root == "" {
		root = DefaultRoot()
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return ref.Node{}, fmt.Errorf("execlog: episode: %w", ref.ErrNotFound)
	}

	days, err := dayDirs(root)
	if err != nil {
		return ref.Node{}, err
	}

	var (
		shards      []string
		count       int
		first, last time.Time
	)
	for _, day := range days {
		path := filepath.Join(day, safeName(id)+".jsonl")
		recs, err := readShard(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return ref.Node{}, err
		}
		matched := 0
		for _, r := range recs {
			// safeName collapses distinct raw ids onto one filename; the
			// record's own field says which episode a row belongs to.
			if r.Episode != id {
				continue
			}
			matched++
			if !r.At.IsZero() && (first.IsZero() || r.At.Before(first)) {
				first = r.At
			}
			if !r.At.IsZero() && (last.IsZero() || r.At.After(last)) {
				last = r.At
			}
		}
		if matched > 0 {
			shards = append(shards, path)
			count += matched
		}
	}
	if count == 0 {
		return ref.Node{}, fmt.Errorf("execlog: episode %q: %w", id, ref.ErrNotFound)
	}

	n := ref.NewNode(ref.Episode, id)
	n.Title = fmt.Sprintf("%d commands, %s → %s", count,
		first.UTC().Format(time.RFC3339), last.UTC().Format(time.RFC3339))
	n.Status = "observed"
	n.Where = strings.Join(shards, ", ")
	n.Open = "bashy graph history --episode " + id
	return n, nil
}

// readShard is readFile with the open failure surfaced: for resolution, a
// shard that exists but cannot be read must be an error, never a quiet
// zero that reads as "not observed". Malformed lines are skipped — the
// span and count only need the rows that survived — while a scan failure
// (a truncated read) is reported like an open failure.
func readShard(path string) ([]Record, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var out []Record
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), maxArgv*4)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var r Record
		if json.Unmarshal([]byte(line), &r) != nil {
			continue
		}
		out = append(out, r)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return out, nil
}
