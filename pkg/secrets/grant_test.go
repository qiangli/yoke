package secrets

import (
	"slices"
	"strings"
	"testing"
)

// A metered model needs exactly one credential to answer, and the registry says
// which. The firewall strips the whole keyring; this hands back the one key —
// and no others, which is the difference between a boundary and an outage.
func TestGrantAgentKeyResolvesTheDeclaredRef(t *testing.T) {
	env := []string{
		"PATH=/usr/bin",
		"MOONSHOT_API_KEY=m-secret",
		"DEEPSEEK_API_KEY=d-secret",
		"ANTHROPIC_TOKEN=a-secret",
		"GITHUB_TOKEN=g-secret",
	}
	cases := map[string]string{
		"moonshot":  "MOONSHOT_API_KEY=m-secret",
		"deepseek":  "DEEPSEEK_API_KEY=d-secret",
		"anthropic": "ANTHROPIC_TOKEN=a-secret", // falls through _API_KEY to _TOKEN
	}
	for ref, want := range cases {
		got, ok := GrantAgentKey(env, ref)
		if !ok || got != want {
			t.Errorf("GrantAgentKey(%q) = %q, %v; want %q", ref, got, ok, want)
		}
	}
}

// A ref this host has no key for is a real answer, not a crash: the agent will
// fail to authenticate and SAY so, which is what `agents verify --live` reads.
func TestGrantAgentKeyMissingRef(t *testing.T) {
	if _, ok := GrantAgentKey([]string{"PATH=/usr/bin"}, "moonshot"); ok {
		t.Error("a ref with no key on this host must not resolve")
	}
	if _, ok := GrantAgentKey([]string{"MOONSHOT_API_KEY="}, "moonshot"); ok {
		t.Error("an empty credential must not make a provider operable")
	}
	if _, ok := GrantAgentKey([]string{"X=1"}, ""); ok {
		t.Error("an empty ref must not resolve")
	}
}

// It grants ONE key. An agent asking for moonshot must not be handed github's.
func TestGrantAgentKeyGrantsNothingElse(t *testing.T) {
	env := []string{"MOONSHOT_API_KEY=m", "GITHUB_TOKEN=g", "OPENAI_API_KEY=o"}
	got, ok := GrantAgentKey(env, "moonshot")
	if !ok || got != "MOONSHOT_API_KEY=m" {
		t.Fatalf("got %q", got)
	}
}

func TestCredentialEnvNamesAreGenericAndNamesOnly(t *testing.T) {
	got := CredentialEnvNames("z.ai/provider")
	want := []string{"Z_AI_PROVIDER_API_KEY", "Z_AI_PROVIDER_TOKEN", "Z_AI_PROVIDER_KEY", "Z_AI_PROVIDER"}
	if !slices.Equal(got, want) {
		t.Fatalf("names = %v, want %v", got, want)
	}
	if got := CredentialEnvNames("  "); got != nil {
		t.Fatalf("empty ref produced names: %v", got)
	}
}

func TestPreserveEnvNamesCopiesOnlyDeclaredNames(t *testing.T) {
	child := []string{"PATH=/bin", "SELECTED_API_KEY=existing"}
	parent := []string{
		"SELECTED_API_KEY=parent",
		"ENDPOINT_URL=https://example.invalid",
		"OTHER_API_KEY=unrelated",
	}
	got := PreserveEnvNames(child, parent, []string{"SELECTED_API_KEY", "ENDPOINT_URL"})
	if countEnvName(got, "SELECTED_API_KEY") != 1 || !hasEnvName(got, "ENDPOINT_URL") {
		t.Fatalf("preserved names = %v", envNames(got))
	}
	if hasEnvName(got, "OTHER_API_KEY") {
		t.Fatalf("undeclared name was preserved: names=%v", envNames(got))
	}
}

func TestPreserveEnvNamesAbsentAndDuplicateInputs(t *testing.T) {
	child := []string{"PATH=/bin"}
	if got := PreserveEnvNames(child, []string{"OTHER=1"}, []string{"MISSING_API_KEY"}); len(got) != 1 {
		t.Fatalf("absent name changed child env: names=%v", envNames(got))
	}
	parent := []string{"SELECTED_API_KEY=first", "SELECTED_API_KEY=second"}
	got := PreserveEnvNames(child, parent, []string{"SELECTED_API_KEY", "SELECTED_API_KEY", ""})
	if countEnvName(got, "SELECTED_API_KEY") != 1 {
		t.Fatalf("duplicate input produced duplicate env entries: names=%v", envNames(got))
	}
}

func hasEnvName(env []string, name string) bool { return countEnvName(env, name) != 0 }

func countEnvName(env []string, name string) int {
	prefix := name + "="
	count := 0
	for _, kv := range env {
		if strings.HasPrefix(kv, prefix) {
			count++
		}
	}
	return count
}

func envNames(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		if i := strings.IndexByte(kv, '='); i > 0 {
			out = append(out, kv[:i])
		}
	}
	return out
}
