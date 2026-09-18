// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package dag

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/qiangli/coreutils/pkg/weavecli"
)

// The chunk manifest is the CORPUS half of fleet execution, and it is
// deliberately in its own file: nothing here may consult a Pool, a Worker, or a
// slot count. Chunk membership — which cases belong to chunk i, out of how many
// chunks — is a property of the corpus, pinned in a committed manifest and
// changed only when the corpus changes. Fleet capacity decides how many chunks
// run *concurrently*, never how many chunks *exist* nor what is in them.
//
// If membership were derived from capacity, `suite:shard=7` would name a
// different case set depending on who happened to be online, which breaks the
// two things chunking is for: selective re-run (a failing case must map to a
// stable chunk) and the fingerprint cache (a reshuffled chunk invalidates
// everything). Chunk identity is therefore the pair (ChunkCount, ID), and both
// travel with the result.
//
// The schema is the one bashy already commits (bashy/chunks.json), written by
// its chunk planner from a measured serial run:
//
//	{
//	  "schema_version": 1,
//	  "suite": "bash-5.3",
//	  "chunk_count": 8,
//	  "measurement": {...},
//	  "chunks": [
//	    {"id": 1, "duration_seconds": 112.3, "fixtures": [{"name": "jobs", ...}]},
//	    ...
//	  ]
//	}

// ChunkManifest is a parsed, validated chunk manifest.
type ChunkManifest struct {
	SchemaVersion int         `json:"schema_version"`
	Suite         string      `json:"suite"`
	ChunkCount    int         `json:"chunk_count"`
	Measurement   Measurement `json:"measurement"`
	Chunks        []Chunk     `json:"chunks"`
}

// Measurement records the serial run the chunk plan was derived from. It is
// provenance for the plan, never an input to scheduling.
type Measurement struct {
	MeasuredAt     string `json:"measured_at"`
	Runner         string `json:"runner"`
	Command        string `json:"command"`
	Result         string `json:"result"`
	DurationSource string `json:"duration_source"`
}

// Chunk is one dispatch unit: a named subset of a suite's cases, sized so its
// wall time dwarfs dispatch cost. Fixtures is its membership.
type Chunk struct {
	ID              int       `json:"id"`
	DurationSeconds float64   `json:"duration_seconds"`
	Fixtures        []Fixture `json:"fixtures"`
}

// Fixture is one atom: the smallest unit that can run alone and still produce
// the same verdict. Slicing below an atom yields false failures, not
// approximations, so the manifest — not the scheduler — decides where the
// boundary is.
type Fixture struct {
	Name            string  `json:"name"`
	DurationSeconds float64 `json:"duration_seconds"`
}

// LoadChunkManifest reads and validates a committed chunk manifest.
func LoadChunkManifest(path string) (*ChunkManifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, errf(weavecli.ExitInvalidArg, "read chunk manifest: %v", err)
	}
	m := &ChunkManifest{}
	if err := json.Unmarshal(data, m); err != nil {
		return nil, errf(weavecli.ExitInvalidArg, "parse chunk manifest %s: %v", path, err)
	}
	if err := m.validate(); err != nil {
		return nil, errf(weavecli.ExitInvalidArg, "invalid chunk manifest %s: %v", path, err)
	}
	return m, nil
}

// validate rejects a manifest that cannot pin a stable membership: a declared
// count that disagrees with the chunks present, a duplicate or non-positive
// chunk id, an empty chunk, or a fixture claimed by two chunks. Each of these
// would silently drop or double-run corpus.
func (m *ChunkManifest) validate() error {
	if len(m.Chunks) == 0 {
		return fmt.Errorf("no chunks")
	}
	if m.ChunkCount != len(m.Chunks) {
		return fmt.Errorf("chunk_count is %d but %d chunks are listed", m.ChunkCount, len(m.Chunks))
	}
	ids := map[int]bool{}
	owner := map[string]int{}
	for _, c := range m.Chunks {
		if c.ID < 1 {
			return fmt.Errorf("chunk id %d is not positive", c.ID)
		}
		if ids[c.ID] {
			return fmt.Errorf("chunk id %d appears twice", c.ID)
		}
		ids[c.ID] = true
		if len(c.Fixtures) == 0 {
			return fmt.Errorf("chunk %d has no fixtures", c.ID)
		}
		for _, f := range c.Fixtures {
			name := strings.TrimSpace(f.Name)
			if name == "" {
				return fmt.Errorf("chunk %d has an unnamed fixture", c.ID)
			}
			if prev, dup := owner[name]; dup {
				return fmt.Errorf("fixture %q is in chunk %d and chunk %d", name, prev, c.ID)
			}
			owner[name] = c.ID
		}
	}
	return nil
}

// Chunk returns the chunk with the given id.
func (m *ChunkManifest) Chunk(id int) (Chunk, bool) {
	for _, c := range m.Chunks {
		if c.ID == id {
			return c, true
		}
	}
	return Chunk{}, false
}

// Names is the chunk's membership as plain fixture names, in manifest order.
func (c Chunk) Names() []string {
	out := make([]string, 0, len(c.Fixtures))
	for _, f := range c.Fixtures {
		out = append(out, f.Name)
	}
	return out
}

// MembershipHash fingerprints case→chunk assignment (and nothing else: not
// durations, not the measurement, not the suite). Two runs of the same corpus
// must produce the same hash on any host and at any fleet size — that is the
// property selective re-run and the fingerprint cache both rest on, so it is
// cheap to assert in a test.
func (m *ChunkManifest) MembershipHash() string {
	chunks := append([]Chunk(nil), m.Chunks...)
	sort.Slice(chunks, func(i, j int) bool { return chunks[i].ID < chunks[j].ID })

	h := sha256.New()
	fmt.Fprintf(h, "chunks=%d\n", m.ChunkCount)
	for _, c := range chunks {
		names := c.Names()
		sort.Strings(names)
		fmt.Fprintf(h, "%d=%s\n", c.ID, strings.Join(names, ","))
	}
	return "m" + hex.EncodeToString(h.Sum(nil))
}

// BindChunks hands each chunk's membership to the target that runs it.
//
// The bridge is the `shard` axis a Matrix target already injects into its Env
// (expandMatrix names each combination `<suite>:shard=<i>` and puts `shard=<i>`
// in that node's Env), so a chunked suite needs no new syntax: chunk i binds to
// the target whose shard is i. The body reads its case list from the env rather
// than recomputing a slice, which is what keeps membership a corpus property —
// there is no arithmetic here that could see a worker count.
//
// A chunk with no matching target is an error: silently dropping corpus is the
// failure mode that produces a flattering pass rate.
func BindChunks(doc *Document, m *ChunkManifest) error {
	if doc == nil || m == nil {
		return nil
	}
	bound := map[int]string{}
	for _, t := range doc.Tasks {
		shard, ok := shardOf(t)
		if !ok {
			continue
		}
		c, found := m.Chunk(shard)
		if !found {
			return errf(weavecli.ExitInvalidArg,
				"target %q is shard %d but the chunk manifest pins %d chunks", t.Name, shard, m.ChunkCount)
		}
		if prev, dup := bound[shard]; dup {
			return errf(weavecli.ExitInvalidArg, "chunk %d is claimed by both %q and %q", shard, prev, t.Name)
		}
		bound[shard] = t.Name
		t.Env = append(t.Env,
			"DAG_CHUNK_ID="+strconv.Itoa(c.ID),
			"DAG_CHUNK_COUNT="+strconv.Itoa(m.ChunkCount),
			"DAG_CHUNK_MEMBERS="+strings.Join(c.Names(), " "),
		)
	}
	if len(bound) == 0 {
		return errf(weavecli.ExitInvalidArg,
			"chunk manifest pins %d chunks but no target declares a shard (expected `Matrix: shard=…`)", m.ChunkCount)
	}
	if len(bound) != m.ChunkCount {
		var missing []int
		for _, c := range m.Chunks {
			if _, ok := bound[c.ID]; !ok {
				missing = append(missing, c.ID)
			}
		}
		sort.Ints(missing)
		return errf(weavecli.ExitInvalidArg,
			"chunk manifest pins %d chunks but no target runs chunk(s) %v — the corpus would be silently truncated",
			m.ChunkCount, missing)
	}
	return nil
}

// docHasShards reports whether any target carries a shard index, i.e. whether
// this document is chunked at all.
func docHasShards(doc *Document) bool {
	for _, t := range doc.Tasks {
		if _, ok := shardOf(t); ok {
			return true
		}
	}
	return false
}

// shardOf reads the shard index a matrix expansion injected into a task's Env.
func shardOf(t *Task) (int, bool) {
	for i := len(t.Env) - 1; i >= 0; i-- { // last wins, as with any env override
		k, v, ok := strings.Cut(t.Env[i], "=")
		if !ok || k != "shard" {
			continue
		}
		n, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil {
			return 0, false
		}
		return n, true
	}
	return 0, false
}
