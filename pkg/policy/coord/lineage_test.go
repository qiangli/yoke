package coord

import (
	"os"
	"slices"
	"strings"
	"testing"
)

func envValue(env []string, k string) string {
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, k+"="); ok {
			return v
		}
	}
	return ""
}

func TestChildEnvMintsChildWithParent(t *testing.T) {
	base := []string{"PATH=/bin", EpisodeEnv + "=ep-parent", ParentEpisodeEnv + "=ep-grand"}
	a := ChildEnv(base, "weave-issue-1")
	child := envValue(a, EpisodeEnv)
	if child == "" || child == "ep-parent" {
		t.Fatalf("child episode = %q, want a new id", child)
	}
	if got := envValue(a, ParentEpisodeEnv); got != "ep-parent" {
		t.Fatalf("parent = %q, want the caller's episode", got)
	}
	if n := len(slices.DeleteFunc(slices.Clone(a), func(kv string) bool { return !strings.HasPrefix(kv, EpisodeEnv+"=") })); n != 1 {
		t.Fatalf("%d %s entries, want exactly 1", n, EpisodeEnv)
	}
	if b := ChildEnv(base, "weave-issue-1"); envValue(b, EpisodeEnv) != child {
		t.Fatal("child episode is not deterministic for one (parent, key)")
	}
	if c := ChildEnv(base, "weave-issue-2"); envValue(c, EpisodeEnv) == child {
		t.Fatal("sibling delegates share an episode")
	}
	if out := ChildEnv([]string{"PATH=/bin"}, "k"); !slices.Equal(out, []string{"PATH=/bin"}) {
		t.Fatalf("no caller episode: env changed to %v", out)
	}
}

// TestSubshellSharesEpisode: a subshell / parallel attempt inherits the env
// unchanged, so Self resolves the SAME episode and mints nothing.
func TestSubshellSharesEpisode(t *testing.T) {
	t.Setenv(EpisodeEnv, "ep-shared")
	t.Setenv(ParentEpisodeEnv, "ep-up")
	for i := 0; i < 2; i++ {
		if got := Self().Episode; got != "ep-shared" {
			t.Fatalf("attempt %d episode = %q, want ep-shared", i, got)
		}
	}
	if os.Getenv(EpisodeEnv) != "ep-shared" || os.Getenv(ParentEpisodeEnv) != "ep-up" {
		t.Fatal("Self rewrote the inherited lineage")
	}
}
