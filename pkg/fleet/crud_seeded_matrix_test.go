package fleet

// Sprint #328 story #1178: deterministic seeded CRUD matrix.
//
// add/show/set/edit/rm/verify for agent, tool, and model against a scratch
// local store over seeded lower rings — embedded (tools), shared fixtures
// (models/agents via fleettest.Ring), and an injected cloud source. The
// seeded semantics under test:
//
//   - set is copy-on-write: the seed stays readable and a sparse local
//     overlay (overlay: true, changed fields only) shadows it;
//   - the shadow reports RING local;
//   - rm removes only the local shadow and the seed shows through again
//     with its original ring and values;
//   - removing an unshadowed seed fails closed per the existing contract
//     ("not in the local store") and the seed still resolves.
//
// Everything runs on scratch roots with pinned options: no real home is
// touched, no live credentials are needed, and edit never opens a real
// editor ($EDITOR=true or empty). Structural verify only — never --live.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/spf13/cobra"

	"github.com/qiangli/yoke/pkg/assetring"
	"github.com/qiangli/yoke/pkg/fleet/fleettest"
)

const (
	matrixToolSeed   = "codex"         // embedded baseline
	matrixModelSeed  = "fable5"        // shared fixture ring
	matrixAgentSeed  = "claude-fable5" // shared fixture ring
	matrixCloudTool  = "cloud-seed-tool"
	matrixCloudModel = "cloud-seed-model"
	matrixCloudAgent = "cloud-seed-agent"
)

func matrixCloudBodies() map[string]string {
	return map[string]string{
		matrixCloudTool:  "name: " + matrixCloudTool + "\nkind: cli\ncli:\n  binary: cloud-seed-tool\n  launch:\n    exec: cloud-seed-tool --model {model} {prompt}\n",
		matrixCloudModel: "name: " + matrixCloudModel + "\nkind: subscription\nprovider: anthropic\nupstream_id: cloud-seed-upstream-1\nband: 2\nband_source: declared\ndisplay: Cloud Seed Model\n",
		matrixCloudAgent: "agents:\n  - name: " + matrixCloudAgent + "\n    tool: codex\n    model: " + matrixCloudModel + "\n    display: cloud seed agent\n",
	}
}

// matrixOpts fences one subtest: a scratch local store plus the seeded lower
// rings. WithRoot pins writes to root (disabling ambient $BASHY_*_DIR
// redirects); WithoutCloudOverlay drops the operator's real org cache while
// the injected cloud seed still exercises the cloud ring.
func matrixOpts(root string) []Option {
	bodies := matrixCloudBodies()
	return []Option{
		WithRoot(root),
		WithoutCloudOverlay(),
		WithSource(dirTools, assetring.FileFS(fstest.MapFS{matrixCloudTool + ".yaml": {Data: []byte(bodies[matrixCloudTool])}}, assetring.RingCloud, ext)),
		WithSource(dirModels, assetring.FileFS(fstest.MapFS{matrixCloudModel + ".yaml": {Data: []byte(bodies[matrixCloudModel])}}, assetring.RingCloud, ext)),
		WithSource(dirAgents, assetring.FileFS(fstest.MapFS{matrixCloudAgent + ".yaml": {Data: []byte(bodies[matrixCloudAgent])}}, assetring.RingCloud, ext)),
	}
}

// matrixOverlay reads back the local file for a shadowed entry and requires
// the sparse-overlay marker.
func matrixOverlay(t *testing.T, root, noun, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, noun, name+".yaml"))
	if err != nil {
		t.Fatalf("local %s %q not written: %v", noun, name, err)
	}
	if !strings.Contains(string(data), "overlay: true") {
		t.Fatalf("local %s %q is not a sparse overlay:\n%s", noun, name, data)
	}
	return string(data)
}

// matrixRingLocalRow requires name to be listed with ring local: the shadow
// must REPORT local, not just behave like it.
func matrixRingLocalRow(t *testing.T, newList func(...Option) *cobra.Command, rowJSON, name string, opts []Option, decode func([]byte) (bool, string)) {
	t.Helper()
	out, err := runCmd(t, newList(opts...), "list", "--ring", "local", "--json")
	if err != nil {
		t.Fatal(err)
	}
	_ = rowJSON
	found, ring := decode([]byte(out))
	if !found {
		t.Fatalf("%s: shadowed %q missing from list --ring local:\n%s", rowJSON, name, out)
	}
	if ring != "local" {
		t.Fatalf("%s: shadowed %q ring = %q, want local", rowJSON, name, ring)
	}
}

func decodeToolRow(name string, data []byte) (bool, string) {
	var rows []toolRow
	if err := json.Unmarshal(data, &rows); err != nil {
		return false, "unparseable: " + err.Error()
	}
	for _, r := range rows {
		if r.Name == name {
			return true, r.Ring
		}
	}
	return false, ""
}

func decodeModelRow(name string, data []byte) (bool, string) {
	var rows []modelRow
	if err := json.Unmarshal(data, &rows); err != nil {
		return false, "unparseable: " + err.Error()
	}
	for _, r := range rows {
		if r.Name == name {
			return true, r.Ring
		}
	}
	return false, ""
}

func decodeAgentRow(name string, data []byte) (bool, string) {
	var rows []agentRow
	if err := json.Unmarshal(data, &rows); err != nil {
		return false, "unparseable: " + err.Error()
	}
	for _, r := range rows {
		if r.Name == name {
			return true, r.Ring
		}
	}
	return false, ""
}

func TestSeededCRUDTools(t *testing.T) {
	fleettest.Ring(t)

	t.Run("add/show", func(t *testing.T) {
		root := t.TempDir()
		opts := matrixOpts(root)
		if _, err := runCmd(t, NewToolsCmd(opts...), "add", "matrix-tool",
			"--set", "cli.binary=matrix-tool",
			"--set", "cli.launch.exec=matrix-tool --model {model} {prompt}"); err != nil {
			t.Fatal(err)
		}
		got, err := runCmd(t, NewToolsCmd(opts...), "show", "matrix-tool")
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(got, "matrix-tool") {
			t.Fatalf("show did not print the added tool:\n%s", got)
		}
		if tl, ok := New(opts...).Tool("matrix-tool"); !ok || tl.Ring != assetring.RingLocal {
			t.Fatalf("added tool ring = %v, want local", tl.Ring)
		}
	})

	t.Run("add/upsert-over-seed-is-copy-on-write", func(t *testing.T) {
		root := t.TempDir()
		opts := matrixOpts(root)
		// Adding under a seeded name is an upsert, not a collision: the
		// same canonical name is "self" per claimName, and SaveTool forks
		// a sparse local shadow rather than mutating the seed. Unmentioned
		// seed fields must inherit, not null out.
		before, _ := New(opts...).Tool(matrixToolSeed)
		if _, err := runCmd(t, NewToolsCmd(opts...), "add", matrixToolSeed,
			"--set", "cli.binary=matrix-upsert"); err != nil {
			t.Fatal(err)
		}
		after, _ := New(opts...).Tool(matrixToolSeed)
		if after.Ring != assetring.RingLocal || after.CLI.Binary != "matrix-upsert" {
			t.Fatalf("upsert shadow not in effect: %+v", after)
		}
		if after.CLI.Launch.Exec != before.CLI.Launch.Exec {
			t.Fatalf("upsert wiped the seed launch template: %q", after.CLI.Launch.Exec)
		}
		data := matrixOverlay(t, root, dirTools, matrixToolSeed)
		if strings.Contains(data, "launch:") {
			t.Fatalf("upsert froze the seed launch contract:\n%s", data)
		}
		// Re-adding the local entry stays an upsert.
		if _, err := runCmd(t, NewToolsCmd(opts...), "add", matrixToolSeed,
			"--set", "cli.binary=matrix-upsert"); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("add/bad-path-is-red", func(t *testing.T) {
		root := t.TempDir()
		opts := matrixOpts(root)
		if _, err := runCmd(t, NewToolsCmd(opts...), "add", "matrix-tool",
			"--set", "bogus.path=1"); err == nil {
			t.Fatal("add with an unknown --set path succeeded")
		}
	})

	t.Run("show/seed-and-missing", func(t *testing.T) {
		root := t.TempDir()
		opts := matrixOpts(root)
		got, err := runCmd(t, NewToolsCmd(opts...), "show", matrixToolSeed)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(got, matrixToolSeed) {
			t.Fatalf("show did not print the seeded tool:\n%s", got)
		}
		if tl, ok := New(opts...).Tool(matrixToolSeed); !ok || tl.Ring != assetring.RingEmbedded {
			t.Fatalf("seed tool ring = %v, want embedded", tl.Ring)
		}
		if _, err := runCmd(t, NewToolsCmd(opts...), "show", "no-such-tool"); err == nil {
			t.Fatal("show of a missing tool succeeded")
		}
	})

	t.Run("set/is-copy-on-write", func(t *testing.T) {
		root := t.TempDir()
		opts := matrixOpts(root)
		before, _ := New(opts...).Tool(matrixToolSeed)
		if _, err := runCmd(t, NewToolsCmd(opts...), "set", matrixToolSeed, "--display", "Matrix Display"); err != nil {
			t.Fatal(err)
		}
		after, ok := New(opts...).Tool(matrixToolSeed)
		if !ok || after.Ring != assetring.RingLocal {
			t.Fatalf("shadowed tool ring = %v, want local", after.Ring)
		}
		if after.Display != "Matrix Display" {
			t.Fatalf("set did not take: %+v", after)
		}
		if after.CLI.Launch.Exec != before.CLI.Launch.Exec {
			t.Fatalf("copy-on-write lost the seed launch template: %q", after.CLI.Launch.Exec)
		}
		data := matrixOverlay(t, root, dirTools, matrixToolSeed)
		if strings.Contains(data, "launch:") {
			t.Fatalf("set froze the seed launch contract into the overlay:\n%s", data)
		}
		matrixRingLocalRow(t, NewToolsCmd, "tool", matrixToolSeed, opts, func(b []byte) (bool, string) {
			return decodeToolRow(matrixToolSeed, b)
		})
	})

	t.Run("set/cloud-seed-is-copy-on-write", func(t *testing.T) {
		root := t.TempDir()
		opts := matrixOpts(root)
		if tl, ok := New(opts...).Tool(matrixCloudTool); !ok || tl.Ring != assetring.RingCloud {
			t.Fatalf("cloud tool ring = %v, want cloud", tl.Ring)
		}
		if _, err := runCmd(t, NewToolsCmd(opts...), "set", matrixCloudTool, "--display", "Cloud Tweaked"); err != nil {
			t.Fatal(err)
		}
		after, _ := New(opts...).Tool(matrixCloudTool)
		if after.Ring != assetring.RingLocal || after.Display != "Cloud Tweaked" {
			t.Fatalf("cloud shadow not in effect: %+v", after)
		}
		data := matrixOverlay(t, root, dirTools, matrixCloudTool)
		if strings.Contains(data, "launch:") {
			t.Fatalf("set froze the cloud launch contract:\n%s", data)
		}
	})

	t.Run("set/missing-is-red", func(t *testing.T) {
		root := t.TempDir()
		opts := matrixOpts(root)
		if _, err := runCmd(t, NewToolsCmd(opts...), "set", "no-such-tool", "--display", "x"); err == nil {
			t.Fatal("set of a missing tool succeeded")
		}
	})

	t.Run("edit/materializes-with-stub-editor", func(t *testing.T) {
		t.Setenv("EDITOR", "true")
		t.Setenv("VISUAL", "")
		root := t.TempDir()
		opts := matrixOpts(root)
		if _, err := runCmd(t, NewToolsCmd(opts...), "edit", matrixToolSeed); err != nil {
			t.Fatal(err)
		}
		matrixOverlay(t, root, dirTools, matrixToolSeed)
		if tl, _ := New(opts...).Tool(matrixToolSeed); tl.Ring != assetring.RingLocal {
			t.Fatalf("edited tool ring = %v, want local", tl.Ring)
		}
	})

	t.Run("edit/no-editor-still-materializes", func(t *testing.T) {
		t.Setenv("EDITOR", "")
		t.Setenv("VISUAL", "")
		root := t.TempDir()
		opts := matrixOpts(root)
		out, err := runCmd(t, NewToolsCmd(opts...), "edit", matrixToolSeed)
		if err == nil || !strings.Contains(err.Error(), "EDITOR") {
			t.Fatalf("err = %v, want the missing-editor failure", err)
		}
		if !strings.Contains(out, matrixToolSeed+".yaml") {
			t.Fatalf("edit did not print the materialized path:\n%s", out)
		}
		matrixOverlay(t, root, dirTools, matrixToolSeed)
	})

	t.Run("rm/shadow-restores-seed", func(t *testing.T) {
		root := t.TempDir()
		opts := matrixOpts(root)
		before, _ := New(opts...).Tool(matrixToolSeed)
		if _, err := runCmd(t, NewToolsCmd(opts...), "set", matrixToolSeed, "--display", "Matrix Display"); err != nil {
			t.Fatal(err)
		}
		if _, err := runCmd(t, NewToolsCmd(opts...), "rm", matrixToolSeed); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(root, dirTools, matrixToolSeed+".yaml")); !os.IsNotExist(err) {
			t.Fatalf("rm left the local shadow behind: %v", err)
		}
		back, ok := New(opts...).Tool(matrixToolSeed)
		if !ok {
			t.Fatal("rm of the shadow deleted the seeded tool")
		}
		if back.Ring != assetring.RingEmbedded || back.CLI.Binary != before.CLI.Binary || back.Display != before.Display {
			t.Fatalf("seed did not show through: %+v", back)
		}
	})

	t.Run("rm/cloud-shadow-restores-cloud-seed", func(t *testing.T) {
		root := t.TempDir()
		opts := matrixOpts(root)
		if _, err := runCmd(t, NewToolsCmd(opts...), "set", matrixCloudTool, "--display", "Cloud Tweaked"); err != nil {
			t.Fatal(err)
		}
		if _, err := runCmd(t, NewToolsCmd(opts...), "rm", matrixCloudTool); err != nil {
			t.Fatal(err)
		}
		back, ok := New(opts...).Tool(matrixCloudTool)
		if !ok || back.Ring != assetring.RingCloud || back.Display != "" {
			t.Fatalf("cloud seed did not show through: %+v %v", back, ok)
		}
	})

	t.Run("rm/unshadowed-seed-fails-closed", func(t *testing.T) {
		root := t.TempDir()
		opts := matrixOpts(root)
		if _, err := runCmd(t, NewToolsCmd(opts...), "rm", matrixToolSeed); err == nil {
			t.Fatal("rm of an unshadowed seed succeeded")
		} else if !strings.Contains(err.Error(), "not in the local store") {
			t.Fatalf("err = %v, want the fail-closed local-store contract", err)
		}
		if tl, ok := New(opts...).Tool(matrixToolSeed); !ok || tl.Ring != assetring.RingEmbedded {
			t.Fatalf("failed rm disturbed the seed: %+v %v", tl, ok)
		}
	})

	t.Run("verify/green-and-red", func(t *testing.T) {
		root := t.TempDir()
		opts := matrixOpts(root)
		// `sh` is on every host this suite runs on; the point is the
		// structural verdict, not the binary.
		if _, err := runCmd(t, NewToolsCmd(opts...), "add", "matrix-sh",
			"--set", "cli.binary=sh",
			"--set", "cli.launch.exec=sh --model {model} {prompt}"); err != nil {
			t.Fatal(err)
		}
		if _, err := runCmd(t, NewToolsCmd(opts...), "verify", "matrix-sh"); err != nil {
			t.Fatalf("drivable tool did not verify: %v", err)
		}
		if _, err := runCmd(t, NewToolsCmd(opts...), "verify", "no-such-tool"); err == nil {
			t.Fatal("verify of a missing tool succeeded")
		}
		if _, err := runCmd(t, NewToolsCmd(opts...), "add", "matrix-unshipped",
			"--set", "cli.binary=definitely-not-a-real-binary-xyz",
			"--set", "cli.launch.exec=x --model {model} {prompt}"); err != nil {
			t.Fatal(err)
		}
		if _, err := runCmd(t, NewToolsCmd(opts...), "verify", "matrix-unshipped"); err == nil {
			t.Fatal("verify of an uninstalled tool succeeded")
		}
	})
}

func TestSeededCRUDModels(t *testing.T) {
	fleettest.Ring(t)

	t.Run("add/show", func(t *testing.T) {
		root := t.TempDir()
		opts := matrixOpts(root)
		if _, err := runCmd(t, NewModelsCmd(opts...), "add", "matrix-model",
			"--provider", "anthropic", "--kind", "subscription", "--upstream", "u1"); err != nil {
			t.Fatal(err)
		}
		got, err := runCmd(t, NewModelsCmd(opts...), "show", "matrix-model")
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(got, "matrix-model") {
			t.Fatalf("show did not print the added model:\n%s", got)
		}
		if m, ok := New(opts...).Model("matrix-model"); !ok || m.Ring != assetring.RingLocal {
			t.Fatalf("added model ring = %v, want local", m.Ring)
		}
	})

	t.Run("add/upsert-over-seed-is-copy-on-write", func(t *testing.T) {
		root := t.TempDir()
		opts := matrixOpts(root)
		// Same upsert contract as tools: the seeded canonical name is
		// "self", and SaveModel forks a sparse local shadow. The band peg,
		// upstream id, and provider must inherit, not null out.
		if _, err := runCmd(t, NewModelsCmd(opts...), "add", matrixModelSeed,
			"--provider", "anthropic", "--kind", "subscription"); err != nil {
			t.Fatal(err)
		}
		after, _ := New(opts...).Model(matrixModelSeed)
		if after.Ring != assetring.RingLocal {
			t.Fatalf("upsert shadow ring = %v, want local", after.Ring)
		}
		if after.Band != 4 || after.Provider != "anthropic" || after.Display != "Claude Fable 5" {
			t.Fatalf("upsert wiped seed model fields: %+v", after)
		}
		if !strings.Contains(after.Target(), "claude-fable-5") {
			t.Fatalf("upsert wiped the seed upstream id: %+v", after)
		}
		matrixOverlay(t, root, dirModels, matrixModelSeed)
	})

	t.Run("add/alias-theft-is-red", func(t *testing.T) {
		root := t.TempDir()
		opts := matrixOpts(root)
		if _, err := runCmd(t, NewModelsCmd(opts...), "add", "matrix-poacher",
			"--provider", "anthropic", "--kind", "subscription",
			"--alias", matrixModelSeed); err == nil {
			t.Fatal("aliasing a new model at the seeded name succeeded without --force")
		} else if !strings.Contains(err.Error(), "already belongs") {
			t.Fatalf("err = %v, want the name-claim refusal", err)
		}
		if _, err := runCmd(t, NewModelsCmd(opts...), "add", "matrix-poacher",
			"--provider", "anthropic", "--kind", "subscription",
			"--alias", matrixModelSeed, "--force"); err != nil {
			t.Fatalf("--force did not take the alias: %v", err)
		}
	})

	t.Run("show/seed-and-missing", func(t *testing.T) {
		root := t.TempDir()
		opts := matrixOpts(root)
		got, err := runCmd(t, NewModelsCmd(opts...), "show", matrixModelSeed)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(got, matrixModelSeed) {
			t.Fatalf("show did not print the seeded model:\n%s", got)
		}
		if m, ok := New(opts...).Model(matrixModelSeed); !ok || m.Ring != assetring.RingShared {
			t.Fatalf("seed model ring = %v, want shared", m.Ring)
		}
		if _, err := runCmd(t, NewModelsCmd(opts...), "show", "no-such-model"); err == nil {
			t.Fatal("show of a missing model succeeded")
		}
	})

	t.Run("set/is-copy-on-write", func(t *testing.T) {
		root := t.TempDir()
		opts := matrixOpts(root)
		if _, err := runCmd(t, NewModelsCmd(opts...), "set", matrixModelSeed, "--display", "Matrix Model"); err != nil {
			t.Fatal(err)
		}
		after, ok := New(opts...).Model(matrixModelSeed)
		if !ok || after.Ring != assetring.RingLocal {
			t.Fatalf("shadowed model ring = %v, want local", after.Ring)
		}
		if after.Display != "Matrix Model" {
			t.Fatalf("set did not take: %+v", after)
		}
		if after.Target() == "" || !strings.Contains(after.Target(), "claude-fable-5") {
			t.Fatalf("copy-on-write lost the seed upstream id: %+v", after)
		}
		data := matrixOverlay(t, root, dirModels, matrixModelSeed)
		if strings.Contains(data, "claude-fable-5") {
			t.Fatalf("set froze the seed upstream id into the overlay:\n%s", data)
		}
		matrixRingLocalRow(t, NewModelsCmd, "model", matrixModelSeed, opts, func(b []byte) (bool, string) {
			return decodeModelRow(matrixModelSeed, b)
		})
	})

	t.Run("set/cloud-seed-is-copy-on-write", func(t *testing.T) {
		root := t.TempDir()
		opts := matrixOpts(root)
		if m, ok := New(opts...).Model(matrixCloudModel); !ok || m.Ring != assetring.RingCloud {
			t.Fatalf("cloud model ring = %v, want cloud", m.Ring)
		}
		if _, err := runCmd(t, NewModelsCmd(opts...), "set", matrixCloudModel, "--display", "Cloud Tweaked"); err != nil {
			t.Fatal(err)
		}
		after, _ := New(opts...).Model(matrixCloudModel)
		if after.Ring != assetring.RingLocal || after.Display != "Cloud Tweaked" {
			t.Fatalf("cloud shadow not in effect: %+v", after)
		}
		data := matrixOverlay(t, root, dirModels, matrixCloudModel)
		if strings.Contains(data, "cloud-seed-upstream-1") {
			t.Fatalf("set froze the cloud upstream id:\n%s", data)
		}
	})

	t.Run("set/missing-is-red", func(t *testing.T) {
		root := t.TempDir()
		opts := matrixOpts(root)
		if _, err := runCmd(t, NewModelsCmd(opts...), "set", "no-such-model", "--display", "x"); err == nil {
			t.Fatal("set of a missing model succeeded")
		}
	})

	t.Run("edit/materializes-with-stub-editor", func(t *testing.T) {
		t.Setenv("EDITOR", "true")
		t.Setenv("VISUAL", "")
		root := t.TempDir()
		opts := matrixOpts(root)
		if _, err := runCmd(t, NewModelsCmd(opts...), "edit", matrixModelSeed); err != nil {
			t.Fatal(err)
		}
		matrixOverlay(t, root, dirModels, matrixModelSeed)
		if m, _ := New(opts...).Model(matrixModelSeed); m.Ring != assetring.RingLocal {
			t.Fatalf("edited model ring = %v, want local", m.Ring)
		}
	})

	t.Run("edit/no-editor-still-materializes", func(t *testing.T) {
		t.Setenv("EDITOR", "")
		t.Setenv("VISUAL", "")
		root := t.TempDir()
		opts := matrixOpts(root)
		out, err := runCmd(t, NewModelsCmd(opts...), "edit", matrixModelSeed)
		if err == nil || !strings.Contains(err.Error(), "EDITOR") {
			t.Fatalf("err = %v, want the missing-editor failure", err)
		}
		if !strings.Contains(out, matrixModelSeed+".yaml") {
			t.Fatalf("edit did not print the materialized path:\n%s", out)
		}
		matrixOverlay(t, root, dirModels, matrixModelSeed)
	})

	t.Run("rm/shadow-restores-seed", func(t *testing.T) {
		root := t.TempDir()
		opts := matrixOpts(root)
		if _, err := runCmd(t, NewModelsCmd(opts...), "set", matrixModelSeed, "--display", "Matrix Model"); err != nil {
			t.Fatal(err)
		}
		if _, err := runCmd(t, NewModelsCmd(opts...), "rm", matrixModelSeed); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(root, dirModels, matrixModelSeed+".yaml")); !os.IsNotExist(err) {
			t.Fatalf("rm left the local shadow behind: %v", err)
		}
		back, ok := New(opts...).Model(matrixModelSeed)
		if !ok {
			t.Fatal("rm of the shadow deleted the seeded model")
		}
		if back.Ring != assetring.RingShared || back.Display != "Claude Fable 5" {
			t.Fatalf("seed did not show through: %+v", back)
		}
	})

	t.Run("rm/cloud-shadow-restores-cloud-seed", func(t *testing.T) {
		root := t.TempDir()
		opts := matrixOpts(root)
		if _, err := runCmd(t, NewModelsCmd(opts...), "set", matrixCloudModel, "--display", "Cloud Tweaked"); err != nil {
			t.Fatal(err)
		}
		if _, err := runCmd(t, NewModelsCmd(opts...), "rm", matrixCloudModel); err != nil {
			t.Fatal(err)
		}
		back, ok := New(opts...).Model(matrixCloudModel)
		if !ok || back.Ring != assetring.RingCloud || back.Display != "Cloud Seed Model" {
			t.Fatalf("cloud seed did not show through: %+v %v", back, ok)
		}
	})

	t.Run("rm/unshadowed-seed-fails-closed", func(t *testing.T) {
		root := t.TempDir()
		opts := matrixOpts(root)
		if _, err := runCmd(t, NewModelsCmd(opts...), "rm", matrixModelSeed); err == nil {
			t.Fatal("rm of an unshadowed seed succeeded")
		} else if !strings.Contains(err.Error(), "not in the local store") {
			t.Fatalf("err = %v, want the fail-closed local-store contract", err)
		}
		if m, ok := New(opts...).Model(matrixModelSeed); !ok || m.Ring != assetring.RingShared {
			t.Fatalf("failed rm disturbed the seed: %+v %v", m, ok)
		}
	})

	t.Run("verify/green-and-red", func(t *testing.T) {
		root := t.TempDir()
		opts := matrixOpts(root)
		if _, err := runCmd(t, NewModelsCmd(opts...), "add", "matrix-sub",
			"--provider", "anthropic", "--kind", "subscription", "--upstream", "u1"); err != nil {
			t.Fatal(err)
		}
		if _, err := runCmd(t, NewModelsCmd(opts...), "verify", "matrix-sub"); err != nil {
			t.Fatalf("subscription model did not verify: %v", err)
		}
		if _, err := runCmd(t, NewModelsCmd(opts...), "verify", "no-such-model"); err == nil {
			t.Fatal("verify of a missing model succeeded")
		}
		if _, err := runCmd(t, NewModelsCmd(opts...), "add", "matrix-keyless",
			"--provider", "openai", "--kind", "api", "--upstream", "u2"); err != nil {
			t.Fatal(err)
		}
		if _, err := runCmd(t, NewModelsCmd(opts...), "verify", "matrix-keyless"); err == nil {
			t.Fatal("verify of an api model without a key ref succeeded")
		}
	})
}

func TestSeededCRUDAgents(t *testing.T) {
	fleettest.Ring(t)

	t.Run("add/show", func(t *testing.T) {
		root := t.TempDir()
		opts := matrixOpts(root)
		if _, err := runCmd(t, NewAgentsCmd(opts...), "add", "matrix-agent",
			"--tool", matrixToolSeed, "--model", matrixModelSeed); err != nil {
			t.Fatal(err)
		}
		got, err := runCmd(t, NewAgentsCmd(opts...), "show", "matrix-agent")
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(got, "matrix-agent") {
			t.Fatalf("show did not print the added agent:\n%s", got)
		}
		if a, ok := New(opts...).Agent("matrix-agent"); !ok || a.Ring != assetring.RingLocal {
			t.Fatalf("added agent ring = %v, want local", a.Ring)
		}
	})

	t.Run("add/upsert-over-seed-is-copy-on-write", func(t *testing.T) {
		root := t.TempDir()
		opts := matrixOpts(root)
		// Same upsert contract as tools and models: minting under the
		// seeded name rebinds through a sparse local shadow, leaving the
		// shared seed itself untouched. Unmentioned seed fields (display,
		// ledger notes) must inherit, not null out.
		if _, err := runCmd(t, NewAgentsCmd(opts...), "add", matrixAgentSeed,
			"--tool", matrixToolSeed, "--model", matrixModelSeed); err != nil {
			t.Fatal(err)
		}
		after, _ := New(opts...).Agent(matrixAgentSeed)
		if after.Ring != assetring.RingLocal {
			t.Fatalf("upsert shadow ring = %v, want local", after.Ring)
		}
		// The mentioned halves rebind (codex was named); the unmentioned
		// display inherits from the seed.
		if after.Tool != "codex" || after.Model != "fable5" || after.Display != "claude · fable5" {
			t.Fatalf("upsert mangled seed agent fields: %+v", after)
		}
		matrixOverlay(t, root, dirAgents, matrixAgentSeed)
	})

	t.Run("add/alias-theft-and-arity-are-red", func(t *testing.T) {
		root := t.TempDir()
		opts := matrixOpts(root)
		if _, err := runCmd(t, NewAgentsCmd(opts...), "add", "matrix-poacher",
			"--tool", matrixToolSeed, "--model", matrixModelSeed,
			"--alias", matrixAgentSeed); err == nil {
			t.Fatal("aliasing a new agent at the seeded name succeeded without --force")
		} else if !strings.Contains(err.Error(), "already belongs") {
			t.Fatalf("err = %v, want the name-claim refusal", err)
		}
		if _, err := runCmd(t, NewAgentsCmd(opts...), "add", "matrix-lonely"); err == nil {
			t.Fatal("adding an agent without --tool and --model succeeded")
		}
	})

	t.Run("show/seed-and-missing", func(t *testing.T) {
		root := t.TempDir()
		opts := matrixOpts(root)
		got, err := runCmd(t, NewAgentsCmd(opts...), "show", matrixAgentSeed)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(got, matrixAgentSeed) {
			t.Fatalf("show did not print the seeded agent:\n%s", got)
		}
		if a, ok := New(opts...).Agent(matrixAgentSeed); !ok || a.Ring != assetring.RingShared {
			t.Fatalf("seed agent ring = %v, want shared", a.Ring)
		}
		if _, err := runCmd(t, NewAgentsCmd(opts...), "show", "no-such-agent"); err == nil {
			t.Fatal("show of a missing agent succeeded")
		}
	})

	t.Run("set/is-copy-on-write", func(t *testing.T) {
		root := t.TempDir()
		opts := matrixOpts(root)
		if _, err := runCmd(t, NewAgentsCmd(opts...), "set", matrixAgentSeed, "--nick", "Matrix"); err != nil {
			t.Fatal(err)
		}
		after, ok := New(opts...).Agent(matrixAgentSeed)
		if !ok || after.Ring != assetring.RingLocal {
			t.Fatalf("shadowed agent ring = %v, want local", after.Ring)
		}
		if after.Nick != "Matrix" {
			t.Fatalf("set did not take: %+v", after)
		}
		if after.Tool != "claude" || after.Model != "fable5" {
			t.Fatalf("copy-on-write lost the seed binding: %+v", after)
		}
		data := matrixOverlay(t, root, dirAgents, matrixAgentSeed)
		if strings.Contains(data, "ledger") {
			t.Fatalf("set froze the seed ledger block into the overlay:\n%s", data)
		}
		matrixRingLocalRow(t, NewAgentsCmd, "agent", matrixAgentSeed, opts, func(b []byte) (bool, string) {
			return decodeAgentRow(matrixAgentSeed, b)
		})
	})

	t.Run("set/cloud-seed-is-copy-on-write", func(t *testing.T) {
		root := t.TempDir()
		opts := matrixOpts(root)
		if a, ok := New(opts...).Agent(matrixCloudAgent); !ok || a.Ring != assetring.RingCloud {
			t.Fatalf("cloud agent ring = %v, want cloud", a.Ring)
		}
		if _, err := runCmd(t, NewAgentsCmd(opts...), "set", matrixCloudAgent, "--nick", "Cloudy"); err != nil {
			t.Fatal(err)
		}
		after, _ := New(opts...).Agent(matrixCloudAgent)
		if after.Ring != assetring.RingLocal || after.Nick != "Cloudy" {
			t.Fatalf("cloud shadow not in effect: %+v", after)
		}
		if after.Tool != "codex" || after.Model != matrixCloudModel {
			t.Fatalf("copy-on-write lost the cloud binding: %+v", after)
		}
		matrixOverlay(t, root, dirAgents, matrixCloudAgent)
	})

	t.Run("set/missing-is-red", func(t *testing.T) {
		root := t.TempDir()
		opts := matrixOpts(root)
		if _, err := runCmd(t, NewAgentsCmd(opts...), "set", "no-such-agent", "--nick", "x"); err == nil {
			t.Fatal("set of a missing agent succeeded")
		}
	})

	t.Run("edit/materializes-with-stub-editor", func(t *testing.T) {
		t.Setenv("EDITOR", "true")
		t.Setenv("VISUAL", "")
		root := t.TempDir()
		opts := matrixOpts(root)
		if _, err := runCmd(t, NewAgentsCmd(opts...), "edit", matrixAgentSeed); err != nil {
			t.Fatal(err)
		}
		matrixOverlay(t, root, dirAgents, matrixAgentSeed)
		if a, _ := New(opts...).Agent(matrixAgentSeed); a.Ring != assetring.RingLocal {
			t.Fatalf("edited agent ring = %v, want local", a.Ring)
		}
	})

	t.Run("edit/no-editor-still-materializes", func(t *testing.T) {
		t.Setenv("EDITOR", "")
		t.Setenv("VISUAL", "")
		root := t.TempDir()
		opts := matrixOpts(root)
		out, err := runCmd(t, NewAgentsCmd(opts...), "edit", matrixAgentSeed)
		if err == nil || !strings.Contains(err.Error(), "EDITOR") {
			t.Fatalf("err = %v, want the missing-editor failure", err)
		}
		if !strings.Contains(out, matrixAgentSeed+".yaml") {
			t.Fatalf("edit did not print the materialized path:\n%s", out)
		}
		matrixOverlay(t, root, dirAgents, matrixAgentSeed)
	})

	t.Run("rm/shadow-restores-seed", func(t *testing.T) {
		root := t.TempDir()
		opts := matrixOpts(root)
		if _, err := runCmd(t, NewAgentsCmd(opts...), "set", matrixAgentSeed, "--nick", "Matrix"); err != nil {
			t.Fatal(err)
		}
		if _, err := runCmd(t, NewAgentsCmd(opts...), "rm", matrixAgentSeed); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(root, dirAgents, matrixAgentSeed+".yaml")); !os.IsNotExist(err) {
			t.Fatalf("rm left the local shadow behind: %v", err)
		}
		back, ok := New(opts...).Agent(matrixAgentSeed)
		if !ok {
			t.Fatal("rm of the shadow deleted the seeded agent")
		}
		if back.Ring != assetring.RingShared || back.Nick != "" || back.Tool != "claude" || back.Model != "fable5" {
			t.Fatalf("seed did not show through: %+v", back)
		}
	})

	t.Run("rm/cloud-shadow-restores-cloud-seed", func(t *testing.T) {
		root := t.TempDir()
		opts := matrixOpts(root)
		if _, err := runCmd(t, NewAgentsCmd(opts...), "set", matrixCloudAgent, "--nick", "Cloudy"); err != nil {
			t.Fatal(err)
		}
		if _, err := runCmd(t, NewAgentsCmd(opts...), "rm", matrixCloudAgent); err != nil {
			t.Fatal(err)
		}
		back, ok := New(opts...).Agent(matrixCloudAgent)
		if !ok || back.Ring != assetring.RingCloud || back.Nick != "" {
			t.Fatalf("cloud seed did not show through: %+v %v", back, ok)
		}
	})

	t.Run("rm/unshadowed-seed-fails-closed", func(t *testing.T) {
		root := t.TempDir()
		opts := matrixOpts(root)
		if _, err := runCmd(t, NewAgentsCmd(opts...), "rm", matrixAgentSeed); err == nil {
			t.Fatal("rm of an unshadowed seed succeeded")
		} else if !strings.Contains(err.Error(), "local") {
			t.Fatalf("err = %v, want the fail-closed local-store contract", err)
		}
		if a, ok := New(opts...).Agent(matrixAgentSeed); !ok || a.Ring != assetring.RingShared {
			t.Fatalf("failed rm disturbed the seed: %+v %v", a, ok)
		}
	})

	t.Run("verify/green-and-red", func(t *testing.T) {
		root := t.TempDir()
		opts := matrixOpts(root)
		if _, err := runCmd(t, NewToolsCmd(opts...), "add", "matrix-vtool",
			"--set", "cli.binary=sh",
			"--set", "cli.launch.exec=sh --model {model} {prompt}"); err != nil {
			t.Fatal(err)
		}
		if _, err := runCmd(t, NewModelsCmd(opts...), "add", "matrix-vmodel",
			"--provider", "anthropic", "--kind", "subscription", "--upstream", "u1"); err != nil {
			t.Fatal(err)
		}
		if _, err := runCmd(t, NewAgentsCmd(opts...), "add", "matrix-vagent",
			"--tool", "matrix-vtool", "--model", "matrix-vmodel"); err != nil {
			t.Fatal(err)
		}
		if _, err := runCmd(t, NewAgentsCmd(opts...), "verify", "matrix-vagent"); err != nil {
			t.Fatalf("launchable agent did not verify: %v", err)
		}
		if _, err := runCmd(t, NewAgentsCmd(opts...), "verify", "no-such-agent"); err == nil {
			t.Fatal("verify of a missing agent succeeded")
		}
		if _, err := runCmd(t, NewAgentsCmd(opts...), "add", "matrix-ghost",
			"--tool", "matrix-vtool", "--model", "no-such-model"); err != nil {
			t.Fatal(err)
		}
		if _, err := runCmd(t, NewAgentsCmd(opts...), "verify", "matrix-ghost"); err == nil {
			t.Fatal("verify of a dangling binding succeeded")
		}
	})
}
