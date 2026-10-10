package chat

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/fleet/fleettest"
	"github.com/qiangli/yoke/pkg/llmbudget"
	"github.com/qiangli/yoke/pkg/policy/coord"
	"github.com/qiangli/yoke/pkg/principal"
)

// pinClaimCatalog is pinCatalog plus two bindings whose models another
// episode may claim, in the SAME store llmbudget's catalog reads (the test
// ring's root), so the launcher and the gate see one fleet:
//
//   - genie-gpt-oss-20b: genie on a door-served LOCAL model (unmetered lane),
//     registry name "gpt-oss:20b" — the Sprint 408 smoke case;
//   - idtool-two: a tool handed a provider-side id ("vendor/two-id") that
//     differs from the model's registry name ("two").
func pinClaimCatalog(t *testing.T) {
	t.Helper()
	root := fleettest.Ring(t)
	cat := fleet.New(fleet.WithRoot(root))
	if err := cat.SaveModel(fleet.Model{Name: "gpt-oss:20b", Kind: fleet.ModelKindLocal, Provider: "ollama"}); err != nil {
		t.Fatal(err)
	}
	if err := cat.SaveAgent(fleet.Agent{Name: "genie-gpt-oss-20b", Tool: "genie", Model: "gpt-oss:20b"}); err != nil {
		t.Fatal(err)
	}
	if err := cat.SaveTool(fleet.Tool{
		Name: "idtool", Kind: fleet.ToolKindCLI,
		CLI: fleet.ToolCLI{Binary: "idtool", Launch: fleet.ToolLaunch{Exec: "idtool --model {model} {prompt}"}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := cat.SaveModel(fleet.Model{Name: "two", Kind: fleet.ModelKindAPI, Provider: "vendor", UpstreamID: "vendor/two-id"}); err != nil {
		t.Fatal(err)
	}
	if err := cat.SaveAgent(fleet.Agent{Name: "idtool-two", Tool: "idtool", Model: "two"}); err != nil {
		t.Fatal(err)
	}
	prev := newCatalog
	newCatalog = func() *fleet.Catalog { return fleet.New(fleet.WithRoot(root)) }
	t.Cleanup(func() { newCatalog = prev })
}

// claimLedger isolates the coord registry and the budget gate, makes this
// process episode "ep-me", and returns the OTHER episode's holder.
func claimLedger(t *testing.T) principal.Ref {
	t.Helper()
	t.Setenv("BASHY_COORD_DIR", t.TempDir())
	t.Setenv("BASHY_EPISODE", "ep-me")
	restore := llmbudget.SetDefault(llmbudget.New(llmbudget.Config{StatePath: filepath.Join(t.TempDir(), "meter.json")}))
	t.Cleanup(restore)
	return principal.Ref{Name: "other", Episode: "ep-other", Host: "h"}
}

func mustClaim(t *testing.T, holder principal.Ref, ref string) {
	t.Helper()
	if _, err := coord.AcquireRef(context.Background(), coord.Request{Ref: coord.ParseRef(ref), Holder: holder, Intent: "eval"}); err != nil {
		t.Fatal(err)
	}
}

// A launch of a binding whose model another episode holds returns the
// Conflict WITHOUT spawning, whatever the model's billing lane — a local
// door-served model is still a claimable model. Sprint 408's smoke: A holds
// model:gpt-oss:20b, B's `bashy chat --agent genie-gpt-oss-20b` must exit with
// the conflict, not answer.
func TestInvokeRefusesClaimedLocalModelBeforeRunner(t *testing.T) {
	permitUnsafeLaunch(t)
	pinClaimCatalog(t)
	isolatedRoom(t)
	other := claimLedger(t)
	mustClaim(t, other, "model:gpt-oss:20b")

	r := &fakeRunner{}
	res, err := Invoke(context.Background(), Options{Agent: "genie-gpt-oss-20b", Instruction: "reply OK", Cwd: t.TempDir()}, r)
	var c *coord.Conflict
	if !errors.As(err, &c) {
		t.Fatalf("err = %v, want *coord.Conflict", err)
	}
	if c.Claim.Holder.Episode != "ep-other" || c.Claim.Ref() != (coord.Ref{Kind: "model", Name: "gpt-oss:20b"}) {
		t.Fatalf("conflict = %+v", c.Claim)
	}
	if r.agent != "" {
		t.Fatalf("runner spawned %q despite the claim", r.agent)
	}
	if res.ExitCode == 0 {
		t.Fatalf("exit = %d, want non-zero", res.ExitCode)
	}
	// The holder's own launch passes.
	t.Setenv("BASHY_EPISODE", "ep-other")
	if _, err := Invoke(context.Background(), Options{Agent: "genie-gpt-oss-20b", Instruction: "reply OK", Cwd: t.TempDir()}, &fakeRunner{}); err != nil {
		t.Fatalf("the holder was refused its own model: %v", err)
	}
}

// A claim typed against the PROVIDER-SIDE id refuses the launch too: the
// binding is checked under both its registry name and its resolved id.
func TestInvokeRefusesClaimedResolvedModelID(t *testing.T) {
	permitUnsafeLaunch(t)
	pinClaimCatalog(t)
	isolatedRoom(t)
	other := claimLedger(t)

	l, err := resolveLaunch("idtool-two", Options{})
	if err != nil {
		t.Fatal(err)
	}
	if l.ModelName != "two" || l.Model != "vendor/two-id" {
		t.Fatalf("launch names = %q/%q, want two/vendor/two-id", l.ModelName, l.Model)
	}
	for _, ref := range []string{"model:vendor/two-id", "model:two"} {
		mustClaim(t, other, ref)
		r := &fakeRunner{}
		_, err := Invoke(context.Background(), Options{Agent: "idtool-two", Instruction: "hi", Cwd: t.TempDir()}, r)
		var c *coord.Conflict
		if !errors.As(err, &c) {
			t.Fatalf("%s: err = %v, want *coord.Conflict", ref, err)
		}
		if c.Claim.Ref() != coord.ParseRef(ref) {
			t.Fatalf("%s: conflict = %+v", ref, c.Claim)
		}
		if r.agent != "" {
			t.Fatalf("%s: runner spawned %q despite the claim", ref, r.agent)
		}
		if err := coord.ReleaseRef(context.Background(), coord.ParseRef(ref), other, 0); err != nil {
			t.Fatal(err)
		}
	}
	// Unclaimed: the launch proceeds to the runner.
	r := &fakeRunner{}
	if _, err := Invoke(context.Background(), Options{Agent: "idtool-two", Instruction: "hi", Cwd: t.TempDir()}, r); err != nil {
		t.Fatal(err)
	}
	if r.agent == "" {
		t.Fatal("unclaimed launch never reached the runner")
	}
}
