// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package dag

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// testdata/chunks.json is a verbatim copy of the manifest bashy commits for the
// bash-5.3 conformance corpus: 8 LPT-packed chunks over 86 fixtures. Parsing the
// real file (rather than a shape invented for the test) is the point — a loader
// that only reads its own fixtures proves nothing about the committed manifest.
func realManifest(t *testing.T) *ChunkManifest {
	t.Helper()
	m, err := LoadChunkManifest(filepath.Join("testdata", "chunks.json"))
	if err != nil {
		t.Fatalf("LoadChunkManifest: %v", err)
	}
	return m
}

func TestLoadChunkManifestReadsTheCommittedCorpus(t *testing.T) {
	m := realManifest(t)

	if m.Suite != "bash-5.3" {
		t.Errorf("suite = %q, want bash-5.3", m.Suite)
	}
	if m.ChunkCount != 8 || len(m.Chunks) != 8 {
		t.Fatalf("chunk_count = %d, chunks = %d, want 8 and 8", m.ChunkCount, len(m.Chunks))
	}

	fixtures := 0
	for _, c := range m.Chunks {
		fixtures += len(c.Fixtures)
	}
	if fixtures != 86 {
		t.Errorf("fixtures = %d, want the 86-fixture corpus", fixtures)
	}

	// Chunk 1 is `jobs` alone (112s — the longest atom, and the makespan floor).
	// It is a one-fixture chunk *because the corpus says so*, not because some
	// scheduler decided a worker was busy.
	c1, ok := m.Chunk(1)
	if !ok {
		t.Fatal("chunk 1 missing")
	}
	if got := c1.Names(); len(got) != 1 || got[0] != "jobs" {
		t.Errorf("chunk 1 = %v, want [jobs]", got)
	}
}

// The central invariant: membership is a function of the corpus alone. No fleet
// size, worker count, slot count, or NumCPU is an input to it — so `shard=7`
// names the same cases no matter who is online.
func TestChunkMembershipIsIndependentOfFleetCapacity(t *testing.T) {
	want := realManifest(t).MembershipHash()

	for _, capacity := range []int{1, 2, 8, 64} {
		pool := LocalPool(capacity)
		if got := pool.Capacity(); got != capacity {
			t.Fatalf("LocalPool(%d).Capacity() = %d", capacity, got)
		}
		// Reload with a pool of a different size alive and in scope. The
		// manifest API takes no pool, which is the property under test; this
		// asserts nothing sneaks one in through global state.
		if got := realManifest(t).MembershipHash(); got != want {
			t.Fatalf("membership hash changed at capacity %d: %s != %s", capacity, got, want)
		}
	}
}

func TestMembershipHashIgnoresDurationsAndOrder(t *testing.T) {
	m := realManifest(t)
	want := m.MembershipHash()

	// Re-timing the corpus must not reshuffle it: durations inform the *plan*,
	// they are not part of the membership the cache and selective re-run key on.
	m.Chunks[0].DurationSeconds = 999
	m.Chunks[0].Fixtures[0].DurationSeconds = 999
	m.Measurement.MeasuredAt = "2030-01-01"
	if got := m.MembershipHash(); got != want {
		t.Errorf("hash changed after re-timing: %s != %s", got, want)
	}

	// Listing the same chunks in a different order is the same membership.
	m.Chunks[0], m.Chunks[7] = m.Chunks[7], m.Chunks[0]
	if got := m.MembershipHash(); got != want {
		t.Errorf("hash changed after reordering chunks: %s != %s", got, want)
	}

	// Moving one fixture between chunks is NOT the same membership.
	moved := m.Chunks[0].Fixtures[0]
	m.Chunks[0].Fixtures = m.Chunks[0].Fixtures[1:]
	m.Chunks[1].Fixtures = append(m.Chunks[1].Fixtures, moved)
	if got := m.MembershipHash(); got == want {
		t.Error("hash unchanged after moving a fixture between chunks")
	}
}

func TestLoadChunkManifestRejectsUnusableManifests(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{{
		name: "count disagrees with chunks",
		raw:  `{"schema_version":1,"chunk_count":3,"chunks":[{"id":1,"fixtures":[{"name":"a"}]}]}`,
		want: "chunk_count is 3 but 1 chunks are listed",
	}, {
		name: "duplicate chunk id",
		raw:  `{"schema_version":1,"chunk_count":2,"chunks":[{"id":1,"fixtures":[{"name":"a"}]},{"id":1,"fixtures":[{"name":"b"}]}]}`,
		want: "chunk id 1 appears twice",
	}, {
		// A fixture in two chunks runs twice and is counted twice — a scoreboard
		// that reports more cases than the corpus has.
		name: "fixture claimed twice",
		raw:  `{"schema_version":1,"chunk_count":2,"chunks":[{"id":1,"fixtures":[{"name":"a"}]},{"id":2,"fixtures":[{"name":"a"}]}]}`,
		want: `fixture "a" is in chunk 1 and chunk 2`,
	}, {
		name: "empty chunk",
		raw:  `{"schema_version":1,"chunk_count":1,"chunks":[{"id":1,"fixtures":[]}]}`,
		want: "chunk 1 has no fixtures",
	}, {
		name: "no chunks",
		raw:  `{"schema_version":1,"chunk_count":0,"chunks":[]}`,
		want: "no chunks",
	}}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "chunks.json")
			if err := os.WriteFile(path, []byte(tc.raw), 0o644); err != nil {
				t.Fatal(err)
			}
			_, err := LoadChunkManifest(path)
			if err == nil {
				t.Fatal("want error, got nil")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to mention %q", err, tc.want)
			}
		})
	}
}

// BindChunks hands each chunk's case list to the shard target that runs it. The
// body reads DAG_CHUNK_MEMBERS instead of recomputing a slice, which is what
// keeps membership a corpus property end to end.
func TestBindChunksGivesEachShardItsCommittedCases(t *testing.T) {
	d := doc(t, "## Tasks\n\n### suite\nMatrix: shard=1,2\n"+block("bash", "run $DAG_CHUNK_MEMBERS"))
	d.expandMatrix()

	m := &ChunkManifest{SchemaVersion: 1, ChunkCount: 2, Chunks: []Chunk{
		{ID: 1, Fixtures: []Fixture{{Name: "jobs"}}},
		{ID: 2, Fixtures: []Fixture{{Name: "trap"}, {Name: "func"}}},
	}}
	if err := BindChunks(d, m); err != nil {
		t.Fatalf("BindChunks: %v", err)
	}

	want := map[string]string{
		"suite:shard=1": "jobs",
		"suite:shard=2": "trap func",
	}
	for name, members := range want {
		task := d.byName[name]
		if task == nil {
			t.Fatalf("target %q not expanded", name)
		}
		env := envMap(task.Env)
		if env["DAG_CHUNK_MEMBERS"] != members {
			t.Errorf("%s members = %q, want %q", name, env["DAG_CHUNK_MEMBERS"], members)
		}
		if env["DAG_CHUNK_COUNT"] != "2" {
			t.Errorf("%s chunk count = %q, want 2 (chunk identity is the pair (n,i))", name, env["DAG_CHUNK_COUNT"])
		}
	}
}

// A manifest whose chunks have no target would silently drop corpus and produce
// a flattering pass rate. It must be an error, loudly, before anything runs.
func TestBindChunksRefusesToTruncateTheCorpus(t *testing.T) {
	d := doc(t, "## Tasks\n\n### suite\nMatrix: shard=1,2\n"+block("bash", "run"))
	d.expandMatrix()

	m := &ChunkManifest{SchemaVersion: 1, ChunkCount: 4, Chunks: []Chunk{
		{ID: 1, Fixtures: []Fixture{{Name: "a"}}},
		{ID: 2, Fixtures: []Fixture{{Name: "b"}}},
		{ID: 3, Fixtures: []Fixture{{Name: "c"}}},
		{ID: 4, Fixtures: []Fixture{{Name: "d"}}},
	}}
	err := BindChunks(d, m)
	if err == nil {
		t.Fatal("want error when chunks 3 and 4 have no target")
	}
	if !strings.Contains(err.Error(), "[3 4]") {
		t.Errorf("error = %q, want it to name the unrun chunks", err)
	}
}

// The mirror image: a target asking for a shard the manifest does not pin means
// the two have drifted apart. Guessing is what produces ghost regressions.
func TestBindChunksRefusesAShardTheManifestDoesNotPin(t *testing.T) {
	d := doc(t, "## Tasks\n\n### suite\nMatrix: shard=1,2,3\n"+block("bash", "run"))
	d.expandMatrix()

	m := &ChunkManifest{SchemaVersion: 1, ChunkCount: 2, Chunks: []Chunk{
		{ID: 1, Fixtures: []Fixture{{Name: "a"}}},
		{ID: 2, Fixtures: []Fixture{{Name: "b"}}},
	}}
	err := BindChunks(d, m)
	if err == nil || !strings.Contains(err.Error(), "shard 3") {
		t.Fatalf("want an error naming shard 3, got %v", err)
	}
}

// The committed manifest is an artifact too: it must not carry the identity of
// the machine that produced the measurement.
func TestCommittedManifestCarriesNoHostFacts(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "chunks.json"))
	if err != nil {
		t.Fatal(err)
	}
	var pretty any
	if err := json.Unmarshal(raw, &pretty); err != nil {
		t.Fatalf("manifest is not valid JSON: %v", err)
	}
	assertNoHostFacts(t, "committed manifest", string(raw))
}
