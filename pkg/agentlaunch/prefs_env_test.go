package agentlaunch

import (
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/fleet"
)

func prefsEnvFixture(t *testing.T, agentPrefs fleet.ReaderPrefs) {
	t.Helper()
	root := t.TempDir()
	cat := fleet.New(fleet.WithRoot(root))
	if err := cat.SaveTool(fleet.Tool{Name: "ptool", Kind: fleet.ToolKindCLI, CLI: fleet.ToolCLI{Binary: "ptool"}}); err != nil {
		t.Fatal(err)
	}
	if err := cat.SaveModel(fleet.Model{Name: "pmodel", Kind: fleet.ModelKindLocal, Provider: "openai-compat", UpstreamID: "m", BaseURL: "http://127.0.0.1:1/v1"}); err != nil {
		t.Fatal(err)
	}
	if err := cat.SaveAgent(fleet.Agent{Name: "pagent", Tool: "ptool", Model: "pmodel", Permission: "auto", Prefs: agentPrefs}); err != nil {
		t.Fatal(err)
	}
	orig := NewCatalog
	NewCatalog = func() *fleet.Catalog { return fleet.New(fleet.WithRoot(root)) }
	t.Cleanup(func() { NewCatalog = orig })
}

func prefsValue(env []string) (string, bool) {
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, "BASHY_PREFS="); ok {
			return v, true
		}
	}
	return "", false
}

func TestPrincipalEnvProjectsDeclaredReaderPrefs(t *testing.T) {
	prefsEnvFixture(t, fleet.ReaderPrefs{BudgetBytes: 2048, View: fleet.ViewCanonical})
	l := Launch{Nick: "pagent", Tool: "ptool", Model: "pmodel"}
	env := PrincipalEnv([]string{"PATH=/bin"}, l)
	v, ok := prefsValue(env)
	if !ok {
		t.Fatalf("BASHY_PREFS not projected: %q", env)
	}
	got, src, err := fleet.ResolveReaderPrefsWithSourcesFromArgs(fleet.ReaderPrefs{}, v, nil, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got.BudgetBytes != 2048 || got.View != fleet.ViewCanonical || src.BudgetBytes != fleet.SourceEnv {
		t.Fatalf("child resolves %+v / %+v", got, src)
	}
}

func TestPrincipalEnvKeepsExplicitPrefs(t *testing.T) {
	prefsEnvFixture(t, fleet.ReaderPrefs{BudgetBytes: 2048})
	env := PrincipalEnv([]string{`BASHY_PREFS={"budget_bytes":99}`}, Launch{Nick: "pagent", Tool: "ptool", Model: "pmodel"})
	if v, _ := prefsValue(env); v != `{"budget_bytes":99}` {
		t.Fatalf("explicit BASHY_PREFS overwritten: %q", v)
	}
}

func TestPrincipalEnvNoPrefsWhenUndeclared(t *testing.T) {
	prefsEnvFixture(t, fleet.ReaderPrefs{})
	env := PrincipalEnv([]string{"PATH=/bin"}, Launch{Nick: "pagent", Tool: "ptool", Model: "pmodel"})
	if _, ok := prefsValue(env); ok {
		t.Fatalf("BASHY_PREFS projected without declared prefs: %q", env)
	}
}
