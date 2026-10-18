package kb

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const tx = `{"episode":"ep-1","turn":3,"messages":[{"role":"assistant","content":"hi"},{"role":"user","content":"  How do I   fix\nthe build? "},{"role":"tool","content":"ok"}]}`

type noteEnv struct {
	Pages []struct {
		Slug, Ref, Title, Status, Body string
		Tags                           []string
	} `json:"pages"`
}

func addTx(t *testing.T, dir, in string, extra ...string) (noteEnv, string, error) {
	t.Helper()
	args := append([]string{"--ring", "agent", "note", "add", "--candidate", "--json", "--transcript", "-"}, extra...)
	out, err := run(t, dir, in, args...)
	var env noteEnv
	if err == nil {
		if e := json.Unmarshal([]byte(out), &env); e != nil {
			t.Fatalf("bad json %q: %v", out, e)
		}
	}
	return env, out, err
}

func TestNoteAddJSONShape(t *testing.T) {
	dir := t.TempDir()
	out := mustRun(t, dir, "--ring", "agent", "note", "add", "--candidate", "--json", "--title", "T one", "--body", "b")
	var env noteEnv
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatal(err)
	}
	if len(env.Pages) != 1 || env.Pages[0].Ref != "kb:t-one" || env.Pages[0].Status != StatusCandidate || env.Pages[0].Body != "b" {
		t.Fatalf("shape: %s", out)
	}
}

func TestNoteTranscriptDistillDeterministic(t *testing.T) {
	a, outA, err := addTx(t, t.TempDir(), tx)
	if err != nil {
		t.Fatal(err)
	}
	_, outB, err := addTx(t, t.TempDir(), tx)
	if err != nil {
		t.Fatal(err)
	}
	strip := func(s string) string {
		var out []string
		for _, l := range strings.Split(s, "\n") {
			if !strings.Contains(l, `"id":`) {
				out = append(out, l)
			}
		}
		return strings.Join(out, "\n")
	}
	if strip(outA) != strip(outB) {
		t.Fatalf("nondeterministic:\n%s\n%s", outA, outB)
	}
	p := a.Pages[0]
	if p.Title != "How do I fix the build?" {
		t.Fatalf("title %q", p.Title)
	}
	if !strings.Contains(p.Body, "assistant: hi") || !strings.Contains(p.Body, "tool: ok") {
		t.Fatalf("body %q", p.Body)
	}
	if len(p.Tags) != 1 || p.Tags[0] != "episode:ep-1" {
		t.Fatalf("tags %v", p.Tags)
	}
}

func TestNoteTranscriptOverrides(t *testing.T) {
	env, _, err := addTx(t, t.TempDir(), tx, "--title", "Mine", "--body", "my body", "--episode", "ep-9")
	if err != nil {
		t.Fatal(err)
	}
	p := env.Pages[0]
	if p.Title != "Mine" || p.Body != "my body" || p.Tags[0] != "episode:ep-9" {
		t.Fatalf("%+v", p)
	}
}

func TestNoteTranscriptInvalid(t *testing.T) {
	for _, in := range []string{"not json", `{"messages":[]}`, tx + tx} {
		dir := t.TempDir()
		if _, _, err := addTx(t, dir, in); err == nil {
			t.Fatalf("expected error for %q", in)
		}
		if ents, _ := os.ReadDir(dir); len(ents) != 0 {
			t.Fatalf("wrote something for %q: %v", in, ents)
		}
	}
}

func TestNoteTranscriptMasksSecrets(t *testing.T) {
	gh := "ghp_" + strings.Repeat("aB3dE5gH7k", 3) + "aB3dE5"
	aws := "AKIAIOSFODNN7EXAMPLE"
	in := `{"episode":"ep-1","messages":[{"role":"user","content":"token ` + gh + `"},{"role":"tool","content":"key ` + aws + ` and ` + gh + `"}]}`
	dir := t.TempDir()
	env, out, err := addTx(t, dir, in)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, gh) || strings.Contains(out, aws) {
		t.Fatalf("secret leaked in output: %s", out)
	}
	filepath.WalkDir(dir, func(p string, d fs.DirEntry, e error) error {
		if e == nil && !d.IsDir() {
			b, _ := os.ReadFile(p)
			if strings.Contains(string(b), gh) || strings.Contains(string(b), aws) {
				t.Errorf("secret leaked in %s", p)
			}
		}
		return nil
	})
	if len(env.Pages) != 1 || strings.Contains(env.Pages[0].Title, gh) {
		t.Fatalf("title: %+v", env.Pages)
	}
}
