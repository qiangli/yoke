// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

package dag

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	// Register the coreutils userland so bodies resolve in-process.
	_ "github.com/qiangli/coreutils/cmds/all"
)

func globNode(t *testing.T, md, name string) *Node {
	t.Helper()
	g, err := BuildGraph(doc(t, md))
	if err != nil {
		t.Fatalf("BuildGraph: %v", err)
	}
	n, ok := g.Nodes[name]
	if !ok {
		t.Fatalf("no node %q", name)
	}
	return n
}

func writeSrc(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func globCache() *Cache {
	return &Cache{Hashes: map[string]string{}}
}

func TestFingerprintGlobChangeInvalidates(t *testing.T) {
	dir := t.TempDir()
	writeSrc(t, filepath.Join(dir, "src", "a.go"), "package a\n")
	writeSrc(t, filepath.Join(dir, "src", "b.go"), "package b\n")
	md := "## Tasks\n\n### build\nSources: src/*.go\nGenerates: out.txt\n" +
		block("bash", "cat src/a.go src/b.go > out.txt")
	n := globNode(t, md, "build")
	c := globCache()

	fp1 := c.Fingerprint(n, dir, nil)

	// Changing a matching file must invalidate.
	writeSrc(t, filepath.Join(dir, "src", "a.go"), "package a // changed\n")
	if fp2 := c.Fingerprint(n, dir, nil); fp2 == fp1 {
		t.Fatal("changing a file matching Sources glob did not change the fingerprint")
	}
}

func TestFingerprintGlobAddDeleteInvalidates(t *testing.T) {
	dir := t.TempDir()
	writeSrc(t, filepath.Join(dir, "src", "a.go"), "package a\n")
	md := "## Tasks\n\n### build\nSources: src/*.go\nGenerates: out.txt\n" +
		block("bash", "cat src/*.go > out.txt")
	n := globNode(t, md, "build")
	c := globCache()

	fp1 := c.Fingerprint(n, dir, nil)

	// Adding a matching file must invalidate.
	writeSrc(t, filepath.Join(dir, "src", "b.go"), "package b\n")
	fp2 := c.Fingerprint(n, dir, nil)
	if fp2 == fp1 {
		t.Fatal("adding a file matching Sources glob did not change the fingerprint")
	}

	// Deleting a matching file must invalidate.
	if err := os.Remove(filepath.Join(dir, "src", "b.go")); err != nil {
		t.Fatal(err)
	}
	if fp3 := c.Fingerprint(n, dir, nil); fp3 == fp2 {
		t.Fatal("deleting a file matching Sources glob did not change the fingerprint")
	}

	// Adding a NON-matching file must not invalidate.
	fp4 := c.Fingerprint(n, dir, nil)
	writeSrc(t, filepath.Join(dir, "src", "notes.txt"), "not go\n")
	if fp5 := c.Fingerprint(n, dir, nil); fp5 != fp4 {
		t.Fatal("adding a non-matching file changed the fingerprint")
	}
}

func TestFingerprintGlobExclusion(t *testing.T) {
	dir := t.TempDir()
	writeSrc(t, filepath.Join(dir, "src", "a.go"), "package a\n")
	writeSrc(t, filepath.Join(dir, "src", "skip.go"), "package skip\n")
	md := "## Tasks\n\n### build\nSources: src/*.go !src/skip.go\nGenerates: out.txt\n" +
		block("bash", "cat src/a.go > out.txt")
	n := globNode(t, md, "build")
	c := globCache()

	fp1 := c.Fingerprint(n, dir, nil)

	// Changing the excluded file must NOT invalidate.
	writeSrc(t, filepath.Join(dir, "src", "skip.go"), "package skip // changed\n")
	if fp2 := c.Fingerprint(n, dir, nil); fp2 != fp1 {
		t.Fatal("changing an excluded file changed the fingerprint")
	}

	// Changing an included file still invalidates.
	writeSrc(t, filepath.Join(dir, "src", "a.go"), "package a // changed\n")
	if fp3 := c.Fingerprint(n, dir, nil); fp3 == fp1 {
		t.Fatal("changing an included file did not change the fingerprint")
	}
}

func TestFingerprintDoubleStar(t *testing.T) {
	dir := t.TempDir()
	writeSrc(t, filepath.Join(dir, "src", "sub", "deep", "c.go"), "package deep\n")
	writeSrc(t, filepath.Join(dir, "src", "a.go"), "package a\n")
	md := "## Tasks\n\n### build\nSources: src/**/*.go\nGenerates: out.txt\n" +
		block("bash", "cat src/a.go > out.txt")
	n := globNode(t, md, "build")
	c := globCache()

	fp1 := c.Fingerprint(n, dir, nil)
	writeSrc(t, filepath.Join(dir, "src", "sub", "deep", "c.go"), "package deep // changed\n")
	if fp2 := c.Fingerprint(n, dir, nil); fp2 == fp1 {
		t.Fatal("changing a file matching a ** glob did not change the fingerprint")
	}
}

func TestUpToDateGlobGenerates(t *testing.T) {
	dir := t.TempDir()
	md := "## Tasks\n\n### build\nSources: src/*.go\nGenerates: dist/*.js\n" +
		block("bash", "mkdir -p dist && cp src/a.go dist/a.js")
	writeSrc(t, filepath.Join(dir, "src", "a.go"), "package a\n")
	n := globNode(t, md, "build")
	c := globCache()

	fp := c.Fingerprint(n, dir, nil)
	c.Record(n.Task.Name, fp)

	// An unmatched required Generates pattern is never a cache hit.
	if c.UpToDate(n, dir, fp) {
		t.Fatal("UpToDate with no file matching Generates glob")
	}

	writeSrc(t, filepath.Join(dir, "dist", "a.js"), "built\n")
	if !c.UpToDate(n, dir, fp) {
		t.Fatal("UpToDate = false with a file matching Generates glob")
	}
}

func TestUpToDateGlobGeneratesExclusionOnly(t *testing.T) {
	dir := t.TempDir()
	md := "## Tasks\n\n### build\nGenerates: dist/* !dist/only.js\n" +
		block("bash", "mkdir -p dist && touch dist/only.js")
	n := globNode(t, md, "build")
	c := globCache()

	fp := c.Fingerprint(n, dir, nil)
	c.Record(n.Task.Name, fp)

	// The only file present is excluded: the pattern is unmatched.
	writeSrc(t, filepath.Join(dir, "dist", "only.js"), "built\n")
	if c.UpToDate(n, dir, fp) {
		t.Fatal("UpToDate with only an excluded Generates match")
	}

	writeSrc(t, filepath.Join(dir, "dist", "other.js"), "built\n")
	if !c.UpToDate(n, dir, fp) {
		t.Fatal("UpToDate = false with a non-excluded Generates match")
	}
}

func TestFingerprintLiteralAndDirPreserved(t *testing.T) {
	dir := t.TempDir()
	writeSrc(t, filepath.Join(dir, "in.txt"), "v1")
	writeSrc(t, filepath.Join(dir, "lib", "x.go"), "package x\n")
	md := "## Tasks\n\n### build\nSources: in.txt lib\nGenerates: out.txt\n" +
		block("bash", "cat in.txt lib/x.go > out.txt")
	n := globNode(t, md, "build")
	c := globCache()

	fp1 := c.Fingerprint(n, dir, nil)

	// Literal file change invalidates.
	writeSrc(t, filepath.Join(dir, "in.txt"), "v2")
	fp2 := c.Fingerprint(n, dir, nil)
	if fp2 == fp1 {
		t.Fatal("changing a literal source file did not change the fingerprint")
	}

	// Change inside a literal source directory invalidates.
	writeSrc(t, filepath.Join(dir, "lib", "x.go"), "package x // changed\n")
	if fp3 := c.Fingerprint(n, dir, nil); fp3 == fp2 {
		t.Fatal("changing a file in a source directory did not change the fingerprint")
	}

	// Deleting a literal source invalidates (absent differs from present).
	if err := os.Remove(filepath.Join(dir, "in.txt")); err != nil {
		t.Fatal(err)
	}
	fp4 := c.Fingerprint(n, dir, nil)
	if fp4 == fp1 {
		t.Fatal("deleting a literal source file did not change the fingerprint")
	}
}

func TestWatchGlobMembership(t *testing.T) {
	dir := t.TempDir()
	writeSrc(t, filepath.Join(dir, "src", "a.go"), "package a\n")
	writeSrc(t, filepath.Join(dir, "src", "skip.go"), "package skip\n")
	md := "## Tasks\n\n### build\nSources: src/*.go !src/skip.go\nGenerates: out.txt\n" +
		block("bash", "cat src/a.go > out.txt")
	g, err := BuildGraph(doc(t, md))
	if err != nil {
		t.Fatal(err)
	}

	before := watchFingerprints(g, dir)

	// Adding a matching file must be visible to watch.
	writeSrc(t, filepath.Join(dir, "src", "b.go"), "package b\n")
	if after := watchFingerprints(g, dir); after["build"] == before["build"] {
		t.Fatal("watch did not see an added file matching Sources glob")
	}

	mid := watchFingerprints(g, dir)

	// Changing an excluded file must be invisible to watch.
	writeSrc(t, filepath.Join(dir, "src", "skip.go"), "package skip // changed\n")
	if after := watchFingerprints(g, dir); after["build"] != mid["build"] {
		t.Fatal("watch reacted to a change in an excluded file")
	}
}

func TestEngineGlobIncremental(t *testing.T) {
	dir := t.TempDir()
	writeSrc(t, filepath.Join(dir, "src", "a.go"), "package a\n")
	md := "## Tasks\n\n### gen\nSources: src/*.go\nGenerates: dist/*.js\n" +
		block("bash", "mkdir -p dist && cp src/a.go dist/a.js")
	cache := globCache()
	newEng := func() *Engine {
		g, err := BuildGraph(doc(t, md))
		if err != nil {
			t.Fatal(err)
		}
		return &Engine{Graph: g, Dir: dir, Env: os.Environ(), Concurrency: 1,
			FailFast: true, Cache: cache, Stdout: io.Discard, Stderr: io.Discard}
	}

	r1, err := newEng().Run(context.Background(), "gen")
	if err != nil {
		t.Fatal(err)
	}
	if r1.Results[0].Status != StatusDone {
		t.Fatalf("first run: want done, got %s", r1.Results[0].Status)
	}
	r2, err := newEng().Run(context.Background(), "gen")
	if err != nil {
		t.Fatal(err)
	}
	if r2.Results[0].Status != StatusUpToDate {
		t.Fatalf("unchanged rerun: want up-to-date, got %s", r2.Results[0].Status)
	}
	writeSrc(t, filepath.Join(dir, "src", "a.go"), "package a // changed\n")
	r3, err := newEng().Run(context.Background(), "gen")
	if err != nil {
		t.Fatal(err)
	}
	if r3.Results[0].Status != StatusDone {
		t.Fatalf("changed glob source: want done, got %s", r3.Results[0].Status)
	}
}
