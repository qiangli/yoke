package weave

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type fakeArenaBackend struct {
	orgs    map[string]bool
	repos   map[string]bool
	bases   map[string]string
	pushes  int
	bundles []string
}

func (f *fakeArenaBackend) EnsureOrg(org string) error {
	if f.orgs == nil {
		f.orgs = map[string]bool{}
	}
	f.orgs[org] = true
	return nil
}
func (f *fakeArenaBackend) EnsureRepo(org, repo string) error {
	if f.repos == nil {
		f.repos = map[string]bool{}
	}
	f.repos[org+"/"+repo] = true
	return nil
}
func (f *fakeArenaBackend) PushBase(org, repo, root, sha string) error {
	if f.bases == nil {
		f.bases = map[string]string{}
	}
	f.bases[org+"/"+repo] = sha
	f.pushes++
	return nil
}
func (f *fakeArenaBackend) BaseSHA(org, repo string) (string, error) {
	if !f.orgs[org] || !f.repos[org+"/"+repo] {
		return "", os.ErrNotExist
	}
	return f.bases[org+"/"+repo], nil
}
func (f *fakeArenaBackend) Bundle(org, repo, path string) error {
	f.bundles = append(f.bundles, path)
	return os.WriteFile(path, []byte("bundle"), 0600)
}
func (f *fakeArenaBackend) DeleteRepo(org, repo string) error {
	delete(f.repos, org+"/"+repo)
	return nil
}
func (f *fakeArenaBackend) DeleteOrg(org string) error { delete(f.orgs, org); return nil }

func TestArenaUpIdempotentAndRebase(t *testing.T) {
	f := &fakeArenaBackend{}
	s := &weaveStory{ID: 12, UUID: "12345678-1234-1234-1234-123456789abc"}
	repos := []arenaSource{{Name: "z", Root: "/tmp/z", SHA: strings.Repeat("a", 40)}, {Name: "a", Root: "/tmp/a", SHA: strings.Repeat("b", 40)}}
	if _, err := arenaUp(s, repos, f, false); err != nil {
		t.Fatal(err)
	}
	first := s.Arena.Digest
	if s.Arena.Org != "sprint-12-12345678" || len(s.Arena.Repos) != 2 || f.pushes != 2 || len(s.Thread) != 1 {
		t.Fatalf("state: %#v, pushes=%d", s.Arena, f.pushes)
	}
	card, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(card), "/tmp/a") || strings.Contains(string(card), "/tmp/z") {
		t.Fatalf("arena card contains an absolute repo path: %s", card)
	}
	sourcesReversed := []arenaSource{repos[1], repos[0]}
	if _, err := arenaUp(s, sourcesReversed, f, false); err != nil {
		t.Fatal(err)
	}
	if _, err := arenaUp(s, repos, f, false); err != nil {
		t.Fatal(err)
	}
	if s.Arena.Digest != first || f.pushes != 2 || len(s.Thread) != 1 {
		t.Fatal("unchanged up was not a no-op")
	}
	repos[0].SHA = strings.Repeat("c", 40)
	if _, err := arenaUp(s, repos, f, false); err == nil {
		t.Fatal("changed base accepted")
	}
	if f.pushes != 2 || s.Arena.Digest != first {
		t.Fatal("refusal changed arena")
	}
	if _, err := arenaUp(s, repos, f, true); err != nil {
		t.Fatal(err)
	}
	if f.pushes != 4 || s.Arena.Digest == first || len(s.Thread) != 2 {
		t.Fatal("rebase did not update arena")
	}
}

func TestArenaUpRepairsMissingRepo(t *testing.T) {
	f := &fakeArenaBackend{}
	s := &weaveStory{ID: 1, UUID: "abcdef01-1234-1234-1234-123456789abc"}
	sources := []arenaSource{{Name: "repo", Root: "/tmp/repo", SHA: strings.Repeat("a", 40)}}
	if _, err := arenaUp(s, sources, f, false); err != nil {
		t.Fatal(err)
	}
	delete(f.repos, s.Arena.Org+"/repo")
	if repaired, err := arenaUp(s, sources, f, false); err != nil || !repaired {
		t.Fatal(err)
	}
	if !f.repos[s.Arena.Org+"/repo"] || f.pushes != 2 {
		t.Fatalf("missing repo was not repaired: %#v, pushes=%d", f.repos, f.pushes)
	}
}

func TestArenaNamesIncludeSprintUUID(t *testing.T) {
	first := &weaveStory{ID: 1, UUID: "12345678-1234-1234-1234-123456789abc"}
	second := &weaveStory{ID: 1, UUID: "abcdef01-1234-1234-1234-123456789abc"}
	if arenaNewOrg(first) == arenaNewOrg(second) {
		t.Fatalf("same sprint IDs collide: %q", arenaNewOrg(first))
	}
	f := &fakeArenaBackend{}
	sources := []arenaSource{{Name: "repo", Root: "/tmp/repo", SHA: strings.Repeat("a", 40)}}
	if _, err := arenaUp(first, sources, f, false); err != nil {
		t.Fatal(err)
	}
	if _, err := arenaUp(second, sources, f, false); err != nil {
		t.Fatal(err)
	}
	if !f.repos[first.Arena.Org+"/repo"] || !f.repos[second.Arena.Org+"/repo"] {
		t.Fatalf("arenas did not remain separate: %#v", f.repos)
	}
}

func TestArenaStatusJSON(t *testing.T) {
	s := &weaveStory{ID: 7, Arena: &sprintArena{Org: "sprint-7-12345678", Digest: "digest", Repos: []arenaRepo{{Repo: "a", Base: strings.Repeat("a", 40)}}}}
	var b bytes.Buffer
	if err := arenaWriteStatus(&b, s, "http://localhost:31880", true, true, nil); err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(b.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got["digest"] != "digest" || got["running"] != true || got["org_url"] != "http://localhost:31880/sprint-7-12345678" {
		t.Fatalf("%s", b.String())
	}
}

func TestArenaStatusReportsBaseDrift(t *testing.T) {
	f := &fakeArenaBackend{orgs: map[string]bool{"sprint-4-12345678": true}, repos: map[string]bool{"sprint-4-12345678/a": true}, bases: map[string]string{"sprint-4-12345678/a": strings.Repeat("b", 40)}}
	s := &weaveStory{ID: 4, Arena: &sprintArena{Org: "sprint-4-12345678", Repos: []arenaRepo{{Repo: "a", Base: strings.Repeat("a", 40)}}}}
	drift, err := arenaDrift(s, f)
	if err != nil {
		t.Fatal(err)
	}
	if len(drift) != 1 || !strings.Contains(drift[0], "base mismatch") {
		t.Fatalf("drift = %#v", drift)
	}
	var out bytes.Buffer
	if err := arenaWriteStatus(&out, s, "http://localhost", true, false, drift); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "drift: a: base mismatch") {
		t.Fatalf("status = %q", out.String())
	}
}

func TestArenaDownRelativeBundlePaths(t *testing.T) {
	f := &fakeArenaBackend{orgs: map[string]bool{"sprint-4": true}, repos: map[string]bool{"sprint-4/a": true}}
	s := &weaveStory{ID: 4, Arena: &sprintArena{Repos: []arenaRepo{{Repo: "a", Base: strings.Repeat("a", 40)}}}}
	store := t.TempDir()
	if err := arenaDown(s, store, f); err != nil {
		t.Fatal(err)
	}
	if len(f.bundles) != 1 || !strings.HasPrefix(f.bundles[0], store) {
		t.Fatal(f.bundles)
	}
	if len(f.orgs) != 0 || len(f.repos) != 0 || s.Arena != nil {
		t.Fatal("arena not removed")
	}
	if _, err := os.Stat(filepath.Join(store, "arena", "4", "a.bundle")); err != nil {
		t.Fatal(err)
	}
	if len(s.Thread) != 1 || strings.Contains(s.Thread[0].Body, store) || !strings.Contains(s.Thread[0].Body, "arena/4/a.bundle") {
		t.Fatalf("thread: %#v", s.Thread)
	}
}
