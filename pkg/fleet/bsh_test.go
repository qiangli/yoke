package fleet

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/qiangli/yoke/pkg/assetring"
	"github.com/qiangli/yoke/pkg/snippet"
)

// fakeBshRunner installs a snippet runner that returns a fixed record
// body and counts executions.
func fakeBshRunner(t *testing.T, body string) *int {
	t.Helper()
	calls := new(int)
	snippet.SetRunner(func(_ context.Context, s snippet.Snippet, io snippet.IO) (snippet.Result, error) {
		if s.Lang != "bsh" {
			return snippet.Result{}, errors.New("lang " + s.Lang)
		}
		if s.Timeout != bshTimeout {
			return snippet.Result{}, errors.New("timeout mismatch")
		}
		if io.Dir == "" {
			return snippet.Result{}, errors.New("no dir")
		}
		*calls++
		return snippet.Result{Stdout: []byte(body), Exit: 0}, nil
	})
	t.Cleanup(func() { snippet.SetRunner(nil) })
	return calls
}

// scratchBshCache pins the .bsh evaluation cache to a temp dir and
// returns it.
func scratchBshCache(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	old := bshCacheDir
	bshCacheDir = func() (string, error) { return dir, nil }
	t.Cleanup(func() { bshCacheDir = old })
	return dir
}

const bshModelBody = "name: dyn\nkind: api\nprovider: test\ndisplay: Dyn Model\n"

func writeBsh(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name+bshExt), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// A .bsh next to no .yaml IS the record: its stdout is the body, parsed
// exactly like the .yaml file would be.
func TestBshRecordIsCatalogEntry(t *testing.T) {
	root := t.TempDir()
	t.Setenv("BASHY_FLEET_DIR", root)
	cache := scratchBshCache(t)
	calls := fakeBshRunner(t, bshModelBody)
	writeBsh(t, filepath.Join(root, dirModels), "dyn", "echo the record")

	cat := New(WithoutCloudOverlay(), WithBaselineFS(fstest.MapFS{}))
	models, errs := cat.Models()
	if len(errs) != 0 {
		t.Fatalf("errs: %v", errs)
	}
	var dyn Model
	found := false
	for _, m := range models {
		if m.Name == "dyn" {
			dyn, found = m, true
		}
	}
	if !found {
		t.Fatalf("dyn not listed: %+v", models)
	}
	if dyn.Provider != "test" || dyn.Kind != "api" || dyn.Display != "Dyn Model" || dyn.Ring != assetring.RingLocal {
		t.Fatalf("dyn=%+v", dyn)
	}
	if *calls != 1 {
		t.Fatalf("runner calls=%d", *calls)
	}
	// The evaluated output is cached; a second catalog build never runs it.
	if _, errs := cat.Models(); len(errs) != 0 {
		t.Fatalf("second errs: %v", errs)
	}
	if *calls != 1 {
		t.Fatalf("cache miss re-executed: calls=%d", *calls)
	}
	entries, err := os.ReadDir(cache)
	if err != nil || len(entries) != 1 || !strings.HasSuffix(entries[0].Name(), ext) {
		t.Fatalf("cache entries=%v err=%v", entries, err)
	}
}

// Saving the evaluated output as a plain .yaml must load the same record.
func TestBshRecordEvaluatesLikeYAML(t *testing.T) {
	root := t.TempDir()
	scratchBshCache(t)
	calls := fakeBshRunner(t, bshModelBody)
	models := filepath.Join(root, dirModels)
	writeBsh(t, models, "dyn", "echo the record")
	if err := os.WriteFile(filepath.Join(models, "static"+ext), []byte(strings.Replace(bshModelBody, "name: dyn", "name: static", 1)), 0o644); err != nil {
		t.Fatal(err)
	}

	cat := New(WithRoot(root), WithoutCloudOverlay(), WithBaselineFS(fstest.MapFS{}))
	got, errs := cat.Models()
	if len(errs) != 0 {
		t.Fatalf("errs: %v", errs)
	}
	var dyn, static Model
	for _, m := range got {
		switch m.Name {
		case "dyn":
			dyn = m
		case "static":
			static = m
		}
	}
	if dyn.Name == "" || static.Name == "" {
		t.Fatalf("missing entries: %+v", got)
	}
	dyn.Name, static.Name = "", ""
	if !reflect.DeepEqual(dyn, static) {
		t.Fatalf("evaluated and literal records differ: %+v != %+v", dyn, static)
	}
	if *calls != 1 {
		t.Fatalf("calls=%d", *calls)
	}
}

// A .bsh may declare only pure or read; anything else is refused before
// anything runs.
func TestBshRecordRefusesNonReadEffects(t *testing.T) {
	root := t.TempDir()
	scratchBshCache(t)
	calls := fakeBshRunner(t, bshModelBody)
	models := filepath.Join(root, dirModels)
	writeBsh(t, models, "evil", "# effects: net\ncurl http://example.invalid\n")
	writeBsh(t, models, "greedy", "# effects: pure, exec\necho hi\n")

	_, errs := New(WithRoot(root), WithoutCloudOverlay(), WithBaselineFS(fstest.MapFS{})).Models()
	joined := ""
	for _, e := range errs {
		joined += e.Error() + "\n"
	}
	if !strings.Contains(joined, `"net" refused`) || !strings.Contains(joined, `"exec" refused`) {
		t.Fatalf("missing refusals: %v", errs)
	}
	if *calls != 0 {
		t.Fatalf("refused record ran: calls=%d", *calls)
	}
}

// A same-named .yaml wins over the .bsh; the shadow is reported.
func TestBshRecordShadowedByYAML(t *testing.T) {
	root := t.TempDir()
	scratchBshCache(t)
	calls := fakeBshRunner(t, bshModelBody)
	models := filepath.Join(root, dirModels)
	writeBsh(t, models, "both", "echo from bsh")
	if err := os.WriteFile(filepath.Join(models, "both"+ext), []byte("name: both\nprovider: yamlwins\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, errs := New(WithRoot(root), WithoutCloudOverlay(), WithBaselineFS(fstest.MapFS{})).Models()
	var both Model
	for _, m := range got {
		if m.Name == "both" {
			both = m
		}
	}
	if both.Provider != "yamlwins" {
		t.Fatalf("yaml did not win: %+v", both)
	}
	joined := ""
	for _, e := range errs {
		joined += e.Error() + "\n"
	}
	if !strings.Contains(joined, "shadowed by both.yaml") {
		t.Fatalf("missing shadow warning: %v", errs)
	}
	if *calls != 0 {
		t.Fatalf("shadowed record ran: calls=%d", *calls)
	}
}

// Shared dirs carry .bsh records too, and `model show --yaml` prints the
// evaluated record.
func TestBshRecordShowYAML(t *testing.T) {
	root := t.TempDir()
	shared := t.TempDir()
	scratchBshCache(t)
	calls := fakeBshRunner(t, bshModelBody)
	t.Setenv("BASHY_MODELS_PATH", shared)
	writeBsh(t, shared, "dyn", "echo the record")

	opts := []Option{WithRoot(root), WithoutCloudOverlay(), WithBaselineFS(fstest.MapFS{})}
	if _, errs := New(opts...).Models(); len(errs) != 0 {
		t.Fatalf("errs: %v", errs)
	}
	if *calls != 1 {
		t.Fatalf("calls=%d", *calls)
	}
	got, err := runCmd(t, NewModelsCmd(opts...), "show", "dyn", "--yaml")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "provider: test") || !strings.Contains(got, "name: dyn") {
		t.Fatalf("show did not print the evaluated record:\n%s", got)
	}
	if *calls != 1 {
		t.Fatalf("show re-executed: calls=%d", *calls)
	}
}

// The declared effects reach the runner: '# effects: pure' is honored,
// and without a comment the record runs as read.
func TestBshRecordEffects(t *testing.T) {
	root := t.TempDir()
	scratchBshCache(t)
	var seen [][]string
	snippet.SetRunner(func(_ context.Context, s snippet.Snippet, _ snippet.IO) (snippet.Result, error) {
		seen = append(seen, s.Effects)
		return snippet.Result{Stdout: []byte(bshModelBody)}, nil
	})
	t.Cleanup(func() { snippet.SetRunner(nil) })
	models := filepath.Join(root, dirModels)
	writeBsh(t, models, "pureone", "# effects: pure\necho hi\n")
	writeBsh(t, models, "plain", "echo hi\n")

	if _, errs := New(WithRoot(root), WithoutCloudOverlay(), WithBaselineFS(fstest.MapFS{})).Models(); len(errs) != 0 {
		t.Fatalf("errs: %v", errs)
	}
	got := map[string][]string{}
	for _, e := range seen {
		got[strings.Join(e, ",")] = e
	}
	if _, ok := got["pure"]; !ok {
		t.Fatalf("pure not declared: %v", seen)
	}
	if _, ok := got["read"]; !ok {
		t.Fatalf("default read missing: %v", seen)
	}
}

// JSON stdout is the record body too (YAML is a JSON superset).
func TestBshRecordJSONOutput(t *testing.T) {
	root := t.TempDir()
	scratchBshCache(t)
	fakeBshRunner(t, `{"name": "jm", "provider": "json", "kind": "api"}`)
	writeBsh(t, filepath.Join(root, dirModels), "jm", "emit json")

	models, errs := New(WithRoot(root), WithoutCloudOverlay(), WithBaselineFS(fstest.MapFS{})).Models()
	if len(errs) != 0 {
		t.Fatalf("errs: %v", errs)
	}
	for _, m := range models {
		if m.Name == "jm" {
			if m.Provider != "json" || m.Kind != "api" {
				t.Fatalf("jm=%+v", m)
			}
			return
		}
	}
	t.Fatal("jm not listed")
}

// A failing .bsh degrades to a name-only entry and reports the failure;
// it never hides the name entirely.
func TestBshRecordFailureReported(t *testing.T) {
	root := t.TempDir()
	scratchBshCache(t)
	snippet.SetRunner(func(_ context.Context, _ snippet.Snippet, _ snippet.IO) (snippet.Result, error) {
		return snippet.Result{Stderr: []byte("kaboom"), Exit: 3}, nil
	})
	t.Cleanup(func() { snippet.SetRunner(nil) })
	writeBsh(t, filepath.Join(root, dirModels), "broken", "false")

	models, errs := New(WithRoot(root), WithoutCloudOverlay(), WithBaselineFS(fstest.MapFS{})).Models()
	found := false
	for _, m := range models {
		if m.Name == "broken" {
			found = true
		}
	}
	if !found {
		t.Fatal("broken record vanished from the catalog")
	}
	joined := ""
	for _, e := range errs {
		joined += e.Error() + "\n"
	}
	if !strings.Contains(joined, "exit 3") || !strings.Contains(joined, "kaboom") {
		t.Fatalf("failure not reported: %v", errs)
	}
	_ = time.Second
}
