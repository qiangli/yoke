package fleet

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/qiangli/yoke/pkg/assetring"
	"github.com/qiangli/yoke/pkg/fleet/fleettest"
)

// store builds a catalog whose local ring is a scratch dir, over the real
// embedded tools and the test ring's models and agents — the shape an
// operator runs with once an overlay ring is mounted.
func store(t *testing.T) (*Catalog, string) {
	t.Helper()
	fleettest.Ring(t)
	root := t.TempDir()
	return New(WithRoot(root)), root
}

func TestSaveAndResolveAgent(t *testing.T) {
	c, root := store(t)
	a := Agent{Name: "007", Aliases: []string{"smarty"}, Tool: "claude", Model: "fable"}
	if err := c.SaveAgent(a); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "agents", "007.yaml")); err != nil {
		t.Fatalf("entry not written: %v", err)
	}
	got, ok := c.Agent("smarty")
	if !ok || got.Name != "007" || got.MatrixKey() != "claude:fable5" {
		t.Fatalf("alias did not resolve to the saved agent: %+v %v", got, ok)
	}
	if got.Ring != assetring.RingLocal {
		t.Fatalf("saved agent ring = %v, want local", got.Ring)
	}
}

// A saved agent's file is a valid catalog entry on either side: the
// envelope round-trips through the same parser the org catalog uses.
func TestSavedAgentIsAnAssetDocument(t *testing.T) {
	c, root := store(t)
	if err := c.SaveAgent(Agent{Name: "007", Tool: "claude", Model: "fable"}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(root, "agents", "007.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	f, err := ParseAgentFile("007", data, nil)
	if err != nil || len(f.Agents) != 1 || f.Agents[0].Tool != "claude" {
		t.Fatalf("saved file did not re-parse: %v %+v", err, f)
	}
}

// Modifying an entry that lives in a lower ring copies it into the local
// store. The operator's edit shadows the baseline; the baseline is never
// mutated.
func TestSetCopiesOnWriteFromBaseline(t *testing.T) {
	c, root := store(t)

	before, ok := c.Tool("codex")
	if !ok || before.Ring != assetring.RingEmbedded {
		t.Fatalf("codex should start in the embedded ring, got %v", before.Ring)
	}

	before.CLI.Binary = "my-codex"
	if err := c.SaveTool(before); err != nil {
		t.Fatal(err)
	}

	after, _ := c.Tool("codex")
	if after.Ring != assetring.RingLocal || after.CLI.Binary != "my-codex" {
		t.Fatalf("local override not in effect: %+v", after)
	}
	// The launch template survived the copy — a partial write would silently
	// break every agent bound to this tool.
	if after.CLI.Launch.Exec != before.CLI.Launch.Exec {
		t.Fatalf("launch template lost on copy-on-write: %q", after.CLI.Launch.Exec)
	}
	if _, err := os.Stat(filepath.Join(root, "tools", "codex.yaml")); err != nil {
		t.Fatalf("copy-on-write did not land in the local store: %v", err)
	}
}

func TestMaterializeIsIdempotent(t *testing.T) {
	c, _ := store(t)
	p1, err := c.MaterializeTool("codex")
	if err != nil {
		t.Fatal(err)
	}
	first, err := os.ReadFile(p1)
	if err != nil {
		t.Fatal(err)
	}
	p2, err := c.MaterializeTool("codex")
	if err != nil || p2 != p1 {
		t.Fatalf("second materialize moved the file: %q vs %q (%v)", p2, p1, err)
	}
	second, _ := os.ReadFile(p2)
	if string(first) != string(second) {
		t.Fatal("materializing an already-local entry rewrote it")
	}
}

// Removing a local entry unshadows the lower ring rather than deleting the
// concept.
func TestRemoveUnshadowsRatherThanDeletes(t *testing.T) {
	c, _ := store(t)
	tl, _ := c.Tool("codex")
	tl.CLI.Binary = "my-codex"
	if err := c.SaveTool(tl); err != nil {
		t.Fatal(err)
	}
	if err := c.RemoveTool("codex"); err != nil {
		t.Fatal(err)
	}
	back, ok := c.Tool("codex")
	if !ok {
		t.Fatal("removing the local override deleted the baseline entry")
	}
	if back.Ring != assetring.RingEmbedded || back.CLI.Binary != "codex" {
		t.Fatalf("baseline did not reappear: %+v", back)
	}
}

// Removing something the local store does not own is an error, not a
// silent no-op: the caller asked to delete a file that is not theirs.
func TestRemoveNonLocalIsAnError(t *testing.T) {
	c, _ := store(t)
	err := c.RemoveTool("codex") // only in the embedded ring
	if err == nil || !strings.Contains(err.Error(), "lower ring") {
		t.Fatalf("err = %v, want a message about the lower ring", err)
	}
}

// Aliasing one entry many times is free. One name meaning two things is not.
func TestClaimNameRejectsACollision(t *testing.T) {
	c, _ := store(t)
	if err := c.SaveAgent(Agent{Name: "alpha", Aliases: []string{"ace"}, Tool: "claude", Model: "opus"}); err != nil {
		t.Fatal(err)
	}

	// Re-aliasing the SAME agent is fine.
	if err := c.claimName(KindAgent, "alpha", []string{"ace", "one"}, false); err != nil {
		t.Fatalf("re-aliasing an entry to itself must be allowed: %v", err)
	}
	// A different agent taking "ace" is not.
	err := c.claimName(KindAgent, "beta", []string{"ace"}, false)
	if err == nil || !strings.Contains(err.Error(), "alpha") {
		t.Fatalf("err = %v, want it to name the existing holder", err)
	}
	// --force takes it anyway.
	if err := c.claimName(KindAgent, "beta", []string{"ace"}, true); err != nil {
		t.Fatalf("--force must override: %v", err)
	}
}

// A nickname that collides with the canonical name of another entry is the
// same violation as an alias collision.
func TestClaimNameRejectsCanonicalCollision(t *testing.T) {
	c, _ := store(t)
	if err := c.SaveAgent(Agent{Name: "alpha", Tool: "claude", Model: "opus"}); err != nil {
		t.Fatal(err)
	}
	err := c.claimName(KindAgent, "beta", []string{"alpha"}, false)
	if err == nil {
		t.Fatal("aliasing beta to the existing name alpha must be rejected")
	}
}

func TestMergeAliases(t *testing.T) {
	got := mergeAliases([]string{"a", "b"}, []string{"c", "a"}, []string{"b"})
	if strings.Join(got, ",") != "a,c" {
		t.Fatalf("mergeAliases = %v, want [a c] (dedup, drop rm, keep order)", got)
	}
}

// Entry names are identifiers, not paths.
func TestValidNameRejectsTraversal(t *testing.T) {
	for _, bad := range []string{"", "../etc/passwd", "a/b", `a\b`, ".hidden", "x..y"} {
		if err := validName(bad); err == nil {
			t.Errorf("validName(%q) = nil, want an error", bad)
		}
	}
	if err := validName("kimi-k2.7-code"); err != nil {
		t.Errorf("validName rejected a legitimate name: %v", err)
	}
}

func TestLooksLikePath(t *testing.T) {
	for _, p := range []string{"-", "./a.yaml", "dir/x", "x.yaml", "x.yml"} {
		if !looksLikePath(p) {
			t.Errorf("looksLikePath(%q) = false", p)
		}
	}
	for _, n := range []string{"007", "smarty", "claude-opus", "gpt-5.5"} {
		if looksLikePath(n) {
			t.Errorf("looksLikePath(%q) = true — a bare nickname is not a path", n)
		}
	}
}

// --- verify --------------------------------------------------------------

func TestVerifyAgentReportsDanglingBinding(t *testing.T) {
	c, _ := store(t)
	if err := c.SaveAgent(Agent{Name: "ghost", Tool: "claude", Model: "no-such"}); err != nil {
		t.Fatal(err)
	}
	chk := c.VerifyAgent("ghost", Probes(nil))
	if chk.OK || !strings.Contains(chk.Reason, "no-such") {
		t.Fatalf("check = %+v, want a failure naming the missing model", chk)
	}
}

// Binding a model to a tool that cannot select one is a label, not a
// selection — and verify must say so rather than pretend it works.
func TestVerifyAgentRejectsToolThatCannotSelectAModel(t *testing.T) {
	c, _ := store(t)
	if err := c.SaveTool(Tool{
		Name: "dumb", Kind: ToolKindCLI,
		CLI: ToolCLI{Binary: "sh", Launch: ToolLaunch{Exec: "sh {prompt}"}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := c.SaveAgent(Agent{Name: "mislabelled", Tool: "dumb", Model: "opus"}); err != nil {
		t.Fatal(err)
	}
	chk := c.VerifyAgent("mislabelled", Probes(nil))
	if chk.OK || !strings.Contains(chk.Reason, "label, not a selection") {
		t.Fatalf("check = %+v", chk)
	}
}

func TestVerifyAgentRequiresDirectProviderCredential(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("ANTHROPIC_TOKEN", "")
	t.Setenv("ANTHROPIC_KEY", "")
	t.Setenv("ANTHROPIC", "")

	c := New(WithRoot(t.TempDir()), WithBaselineFS(fstest.MapFS{}))
	if err := c.SaveTool(Tool{
		Name: "direct", Kind: ToolKindCLI,
		CLI: ToolCLI{Binary: "sh", Launch: ToolLaunch{
			Exec: "sh --model {model} {prompt}", Credential: ToolCredentialModelProvider,
		}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := c.SaveModel(Model{
		Name: "frontier", Kind: ModelKindSubscription, Provider: "anthropic", UpstreamID: "frontier-1",
	}); err != nil {
		t.Fatal(err)
	}
	if err := c.SaveAgent(Agent{Name: "reviewer", Tool: "direct", Model: "frontier"}); err != nil {
		t.Fatal(err)
	}

	if chk := c.VerifyAgent("reviewer", Probes(nil)); chk.OK ||
		!strings.Contains(chk.Reason, "anthropic provider credential") {
		t.Fatalf("missing provider credential check = %+v", chk)
	}
	t.Setenv("ANTHROPIC_API_KEY", "available")
	if chk := c.VerifyAgent("reviewer", Probes(nil)); !chk.OK {
		t.Fatalf("provider credential did not make binding launchable: %+v", chk)
	}
}

// A recognized-but-not-drivable harness (detection only) is not launchable.
func TestVerifyToolWithoutLaunchTemplate(t *testing.T) {
	c, _ := store(t)
	chk := c.VerifyTool("cline", Probes(nil))
	if chk.OK || !strings.Contains(chk.Reason, "no launch template") {
		t.Fatalf("check = %+v", chk)
	}
}

func TestVerifyToolNotInstalled(t *testing.T) {
	c, _ := store(t)
	if err := c.SaveTool(Tool{
		Name: "nope", Kind: ToolKindCLI,
		CLI: ToolCLI{Binary: "definitely-not-a-real-binary-xyz", Launch: ToolLaunch{Exec: "x --model {model} {prompt}"}},
	}); err != nil {
		t.Fatal(err)
	}
	chk := c.VerifyTool("nope", Probes(nil))
	if chk.OK || !strings.Contains(chk.Reason, "not installed") {
		t.Fatalf("check = %+v", chk)
	}
}

// An api model with no key ref has nothing to bill against.
func TestVerifyModelKinds(t *testing.T) {
	c, _ := store(t)
	if err := c.SaveModel(Model{Name: "keyless", Kind: ModelKindAPI}); err != nil {
		t.Fatal(err)
	}
	if chk := c.VerifyModel("keyless", Probes(nil)); chk.OK {
		t.Fatalf("an api model without api_key_ref must fail: %+v", chk)
	}
	if chk := c.VerifyModel("opus", Probes(nil)); !chk.OK || chk.Detail != "claude-opus-5" {
		t.Fatalf("subscription model check = %+v", chk)
	}
	// An unpegged model still verifies — it is usable, just not band-routable.
	if err := c.SaveModel(Model{Name: "unpegged", Kind: ModelKindSubscription}); err != nil {
		t.Fatal(err)
	}
	if chk := c.VerifyModel("unpegged", Probes(nil)); !chk.OK || chk.Warn == "" {
		t.Fatalf("an unpegged model must pass with a warning: %+v", chk)
	}
	if err := c.SaveModel(Model{Name: "weird", Kind: "telepathy"}); err != nil {
		t.Fatal(err)
	}
	if chk := c.VerifyModel("weird", Probes(nil)); chk.OK {
		t.Fatalf("an unknown kind must fail: %+v", chk)
	}
}

// The local store is honored through $BASHY_FLEET_DIR with no options.
func TestDefaultRootHonorsEnv(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BASHY_FLEET_DIR", dir)
	if got := DefaultRoot(); got != dir {
		t.Fatalf("DefaultRoot = %q, want %q", got, dir)
	}
	t.Setenv("BASHY_AGENTS_DIR", filepath.Join(dir, "elsewhere"))
	if got := NounDir(dir, dirAgents); got != filepath.Join(dir, "elsewhere") {
		t.Fatalf("per-noun override ignored: %q", got)
	}
}

// Names are unique within a kind, not across kinds — so an agent may be called
// `claude`. But every launcher resolves an agent before a tool, so the nickname
// silently shadows the harness. Warn rather than let it pass unremarked.
func TestCrossKindNameIsWarned(t *testing.T) {
	c, _ := store(t)

	if w := c.crossKindWarnings(KindAgent, "claude", nil); len(w) != 1 ||
		!strings.Contains(w[0], "also names a tool") || !strings.Contains(w[0], "prefer this agent") {
		t.Fatalf("warnings = %v", w)
	}
	if w := c.crossKindWarnings(KindAgent, "007", []string{"opus"}); len(w) != 1 ||
		!strings.Contains(w[0], "also names a model") {
		t.Fatalf("an ALIAS colliding across kinds must warn too: %v", w)
	}
	if w := c.crossKindWarnings(KindAgent, "007", []string{"smarty"}); len(w) != 0 {
		t.Fatalf("a clean name warns about nothing: %v", w)
	}
	// A tool named after itself is not a cross-kind collision.
	if w := c.crossKindWarnings(KindTool, "claude", nil); len(w) != 0 {
		t.Fatalf("warnings = %v", w)
	}
}
