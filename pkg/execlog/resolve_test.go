// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package execlog

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/ref"
)

// refStore lays out a shard store in a tempdir and points the env ladder at
// it, so RegisterRefs with an empty root exercises the same resolution the
// writer and readers use. Hermetic: BASHY_HOME and the store var both land
// inside the tempdir.
func refStore(t *testing.T) (string, *ref.Registry) {
	t.Helper()
	home := t.TempDir()
	root := filepath.Join(home, "exec")
	t.Setenv("BASHY_HOME", home)
	t.Setenv("BASHY_EXECHIST", root)

	g := ref.NewRegistry()
	RegisterRefs(g, "")
	return root, g
}

func writeShard(t *testing.T, root, day, episode string, lines ...string) string {
	t.Helper()
	dir := filepath.Join(root, day)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, safeName(episode)+".jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRefEpisodeResolvesFromShards(t *testing.T) {
	root, g := refStore(t)
	writeShard(t, root, "2026-09-12", "ep1",
		`{"at":"2026-09-12T10:00:00Z","episode":"ep1","cmd":"ls","seq":1}`,
		`{"at":"2026-09-12T10:05:00Z","episode":"ep1","cmd":"cat","seq":2}`,
	)

	n, err := g.Resolve("episode:ep1")
	if err != nil {
		t.Fatalf("Resolve(episode:ep1): %v", err)
	}
	if n.Ref != "episode:ep1" || n.Status != "observed" {
		t.Errorf("node = ref %q status %q, want episode:ep1 / observed", n.Ref, n.Status)
	}
	if n.Title != "2 commands, 2026-09-12T10:00:00Z → 2026-09-12T10:05:00Z" {
		t.Errorf("Title = %q, want the span and count", n.Title)
	}
	if n.Open != "bashy graph history --episode ep1" {
		t.Errorf("Open = %q, want the graph history reader", n.Open)
	}
	if !strings.Contains(n.Where, filepath.Join("2026-09-12", "ep1.jsonl")) {
		t.Errorf("Where = %q, want the shard path", n.Where)
	}
}

func TestRefEpisodeZeroShardsIsNotFound(t *testing.T) {
	root, g := refStore(t)
	if _, err := g.Resolve("episode:ghost"); !errors.Is(err, ref.ErrNotFound) {
		t.Errorf("empty store err = %v, want ErrNotFound", err)
	}

	// A shard whose FILENAME collides via safeName but whose records belong
	// to another episode is still "not observed" for this one.
	writeShard(t, root, "2026-09-12", "ep/x",
		`{"at":"2026-09-12T10:00:00Z","episode":"ep/x","cmd":"ls","seq":1}`,
	)
	if _, err := g.Resolve("episode:ep_x"); !errors.Is(err, ref.ErrNotFound) {
		t.Errorf("collided shard err = %v, want ErrNotFound", err)
	}
}

// The edge the story names: one episode spanning two day dirs yields ONE
// node listing both shards, with the span and count merged across them.
func TestRefEpisodeSpansDayDirs(t *testing.T) {
	root, g := refStore(t)
	p1 := writeShard(t, root, "2026-09-12", "ep1",
		`{"at":"2026-09-12T23:59:00Z","episode":"ep1","cmd":"ls","seq":1}`,
	)
	p2 := writeShard(t, root, "2026-09-13", "ep1",
		`{"at":"2026-09-13T00:01:00Z","episode":"ep1","cmd":"cat","seq":2}`,
		`{"at":"2026-09-13T00:02:00Z","episode":"ep1","cmd":"go","seq":3}`,
	)

	n, err := g.Resolve("episode:ep1")
	if err != nil {
		t.Fatalf("Resolve(episode:ep1): %v", err)
	}
	if !strings.Contains(n.Where, p1) || !strings.Contains(n.Where, p2) {
		t.Errorf("Where = %q, want both shard paths", n.Where)
	}
	if !strings.HasPrefix(n.Title, "3 commands, 2026-09-12T23:59:00Z → 2026-09-13T00:02:00Z") {
		t.Errorf("Title = %q, want the merged span and count", n.Title)
	}
}
