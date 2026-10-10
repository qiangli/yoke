package agentlaunch

import (
	"slices"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/fleet"
)

// F17: a tool's cli.launch.min_context, when the bound model is served by
// the bashy door as a LOCAL engine model, is ASKED of the door — the base URL
// is rendered through a sticky binding created with num_ctx, the same call
// `bashy llm sticky create --num-ctx` makes — instead of refusing a model
// whose declared window is smaller.
func TestMinContextAsksDoorForLocalModels(t *testing.T) {
	t.Setenv("BASHY_LLM_PORT", "")
	root := t.TempDir()
	cat := fleet.New(fleet.WithRoot(root))
	if err := cat.SaveTool(fleet.Tool{Name: "bigctx", Kind: fleet.ToolKindCLI, CLI: fleet.ToolCLI{Binary: "bigctx", Launch: fleet.ToolLaunch{
		Exec: "bigctx -m {model} -z {prompt}", MinContext: 65536,
		Env: []string{"BIGCTX_BASE={base_url}"},
	}}}); err != nil {
		t.Fatal(err)
	}
	if err := cat.SaveModel(fleet.Model{Name: "small", Kind: fleet.ModelKindLocal, Provider: "openai-compat",
		UpstreamID: "qwen3:8b", ContextLength: 40960, BaseURL: "http://127.0.0.1:24556/v1"}); err != nil {
		t.Fatal(err)
	}
	wantEnv := "BIGCTX_BASE=http://127.0.0.1:24556/sticky/minctx.qwen3-8b.65536/v1"

	// Dry-run renders the ask without touching the door.
	if _, err := ResolveWithCatalog("bigctx:small", Options{AllowUnsafe: true, DryRun: true}, testCatalog(root)); err != nil {
		t.Fatalf("dry run refused: %v", err)
	}

	// A live launch creates the binding first, then carries the rendered URL.
	var gotKey, gotModel string
	var gotCtx int64
	orig := ensureDoorContextBinding
	ensureDoorContextBinding = func(key, model string, numCtx int64) error {
		gotKey, gotModel, gotCtx = key, model, numCtx
		return nil
	}
	t.Cleanup(func() { ensureDoorContextBinding = orig })
	l, err := ResolveWithCatalog("bigctx:small", Options{AllowUnsafe: true}, testCatalog(root))
	if err != nil {
		t.Fatalf("launch refused: %v", err)
	}
	if gotKey != "minctx.qwen3-8b.65536" || gotModel != "qwen3:8b" || gotCtx != 65536 {
		t.Fatalf("binding ask = %q %q %d", gotKey, gotModel, gotCtx)
	}
	if !slices.Contains(l.Env, wantEnv) {
		t.Fatalf("launch env = %q, want %q", l.Env, wantEnv)
	}
}

// A door base URL that already rides a sticky binding is a frozen identity:
// the door cannot raise its context, so F16's up-front refusal stands and the
// door is never asked.
func TestMinContextStillRefusesFrozenDoorIdentity(t *testing.T) {
	t.Setenv("BASHY_LLM_PORT", "")
	root := t.TempDir()
	cat := fleet.New(fleet.WithRoot(root))
	if err := cat.SaveTool(fleet.Tool{Name: "bigctx", Kind: fleet.ToolKindCLI, CLI: fleet.ToolCLI{Binary: "bigctx", Launch: fleet.ToolLaunch{
		Exec: "bigctx -m {model} -z {prompt}", MinContext: 131072,
		Env: []string{"BIGCTX_BASE={base_url}"},
	}}}); err != nil {
		t.Fatal(err)
	}
	if err := cat.SaveModel(fleet.Model{Name: "frozen", Kind: fleet.ModelKindAPI, Provider: "openai-compat",
		UpstreamID: "claude-opus5", ContextLength: 100000,
		BaseURL: "http://127.0.0.1:24556/sticky/genie-opus5/v1"}); err != nil {
		t.Fatal(err)
	}
	asked := false
	orig := ensureDoorContextBinding
	ensureDoorContextBinding = func(key, model string, numCtx int64) error {
		asked = true
		return nil
	}
	t.Cleanup(func() { ensureDoorContextBinding = orig })
	_, err := ResolveWithCatalog("bigctx:frozen", Options{AllowUnsafe: true}, testCatalog(root))
	if err == nil || !strings.Contains(err.Error(), "131072") || !strings.Contains(err.Error(), "100000") {
		t.Fatalf("frozen identity not refused: %v", err)
	}
	if asked {
		t.Fatal("the door was asked to raise a frozen identity")
	}
}
