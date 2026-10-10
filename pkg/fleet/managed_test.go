package fleet

import (
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/qiangli/yoke/pkg/binmgr"
	"github.com/qiangli/yoke/pkg/fleet/fleettest"
)

const managedDoc = `name: pinned
kind: cli
cli:
  binary: pinned
  managed:
    source: github-release
    version: 1.2.3
    license: MIT
    env:
      - PINNED_DISABLE_AUTOUPDATE=1
    assets:
      darwin/arm64:
        url: https://example.test/dl/v{version}/pinned-darwin-arm64.zip
        sha256: 0000000000000000000000000000000000000000000000000000000000000001
        member: pinned
      darwin/amd64:
        url: https://example.test/dl/v{version}/pinned-darwin-x64.zip
        sha256: 0000000000000000000000000000000000000000000000000000000000000002
        member: pinned
      linux/amd64:
        url: https://example.test/dl/v{version}/pinned-linux-x64.tar.gz
        sha256: 0000000000000000000000000000000000000000000000000000000000000003
        tree: true
        entrypoint: bin/pinned
      linux/arm64:
        url: https://example.test/dl/v{version}/pinned-linux-arm64.tar.gz
        sha256: 0000000000000000000000000000000000000000000000000000000000000004
        member: pinned
      windows/amd64:
        url: https://example.test/dl/v{version}/pinned-windows-x64.zip
        sha512: 00000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000005
        member: pinned.exe
  launch:
    exec: pinned --model {model} {prompt}
    events_fail:
      field: type
      values: [error]
`

func parseManaged(t *testing.T, doc string) Tool {
	t.Helper()
	tool, err := ParseTool("pinned", []byte(doc), nil)
	if err != nil {
		t.Fatalf("ParseTool: %v", err)
	}
	return tool
}

// The recipe block is read, validates, and renders as the binmgr declaration
// Ensure consumes: {version} expanded, digests lower-cased, member/tree kept.
func TestManagedRecipeRendersBinmgrTool(t *testing.T) {
	tool := parseManaged(t, managedDoc)
	if !tool.IsManaged() {
		t.Fatal("IsManaged = false for a recipe with cli.managed")
	}
	bt, err := tool.BinmgrTool()
	if err != nil {
		t.Fatalf("BinmgrTool: %v", err)
	}
	if bt.Name != "pinned" || bt.Version != "1.2.3" {
		t.Fatalf("binmgr tool = %+v", bt)
	}
	if len(bt.Assets) != 5 {
		t.Fatalf("assets = %d, want 5", len(bt.Assets))
	}
	a := bt.Assets["darwin/arm64"]
	if a.URL != "https://example.test/dl/v1.2.3/pinned-darwin-arm64.zip" || a.Binary != "pinned" || a.SHA256 == "" {
		t.Fatalf("darwin/arm64 asset = %+v", a)
	}
	if l := bt.Assets["linux/amd64"]; !l.Tree || l.Entrypoint != "bin/pinned" {
		t.Fatalf("linux/amd64 asset = %+v", l)
	}
	if w := bt.Assets["windows/amd64"]; w.SHA512 == "" || w.SHA256 != "" || w.Binary != "pinned.exe" {
		t.Fatalf("windows/amd64 asset = %+v", w)
	}
}

// A recipe that is not a real pin is refused, with the reason, rather than
// resolving to something unverified.
func TestManagedValidateRefusesIncompletePins(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(m *ToolManaged)
		want string
	}{
		{"no version", func(m *ToolManaged) { m.Version = "" }, "version is required"},
		{"latest is not a pin", func(m *ToolManaged) { m.Version = "latest" }, "not a pin"},
		{"no license", func(m *ToolManaged) { m.License = "" }, "license is required"},
		{"bad env", func(m *ToolManaged) { m.Env = []string{"NOEQUALS"} }, "not KEY=VALUE"},
		{"no assets", func(m *ToolManaged) { m.Assets = nil }, "no assets"},
		{"bad platform key", func(m *ToolManaged) { m.Assets["macos/arm64"] = m.Assets["darwin/arm64"] }, "not goos/goarch"},
		{"no digest", func(m *ToolManaged) { a := m.Assets["darwin/arm64"]; a.SHA256 = ""; m.Assets["darwin/arm64"] = a }, "no digest"},
		{"both digests", func(m *ToolManaged) {
			a := m.Assets["darwin/arm64"]
			a.SHA512 = strings.Repeat("a", 128)
			m.Assets["darwin/arm64"] = a
		}, "both sha256 and sha512"},
		{"short sha256", func(m *ToolManaged) { a := m.Assets["darwin/arm64"]; a.SHA256 = "abc"; m.Assets["darwin/arm64"] = a }, "not 64 hex"},
		{"short sha512", func(m *ToolManaged) { a := m.Assets["windows/amd64"]; a.SHA512 = "abc"; m.Assets["windows/amd64"] = a }, "not 128 hex"},
		{"tree without entrypoint", func(m *ToolManaged) { a := m.Assets["linux/amd64"]; a.Entrypoint = ""; m.Assets["linux/amd64"] = a }, "tree without an entrypoint"},
		{"http url", func(m *ToolManaged) {
			a := m.Assets["darwin/arm64"]
			a.URL = "http://example.test/x"
			m.Assets["darwin/arm64"] = a
		}, "must be https"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tool := parseManaged(t, managedDoc)
			tc.edit(tool.CLI.Managed)
			err := tool.CLI.Managed.Validate(tool.Name)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Validate = %v, want %q", err, tc.want)
			}
			if _, err := tool.BinmgrTool(); err == nil {
				t.Fatal("BinmgrTool accepted an invalid pin")
			}
		})
	}
	// A nil block is not a managed tool and never an error.
	if err := (*ToolManaged)(nil).Validate("x"); err != nil {
		t.Fatalf("nil Validate = %v", err)
	}
}

// The resolved path is the pin's cache slot — computed, not downloaded — and
// the recipe's binary name (not the tool name) is the cached basename.
func TestManagedBinaryPathIsThePinnedCacheSlot(t *testing.T) {
	cache := t.TempDir()
	t.Setenv("BASHY_BIN_CACHE", cache)
	tool := parseManaged(t, managedDoc)
	got, err := tool.ManagedBinaryPath()
	if err != nil {
		t.Fatalf("ManagedBinaryPath: %v", err)
	}
	want := filepath.Join(cache, "pinned", "1.2.3", binmgr.BinaryName("pinned"))
	if runtime.GOOS == "linux" && runtime.GOARCH == "amd64" {
		want = filepath.Join(cache, "pinned", "1.2.3", "bin", "pinned") // the tree entrypoint
	}
	if got != want {
		t.Fatalf("path = %q, want %q", got, want)
	}
	if _, ok := tool.ManagedCached(); ok {
		t.Fatal("ManagedCached reports an install that was never downloaded")
	}
	// kimi-code's binary is `kimi`: the cache slot follows the binary.
	doc := strings.Replace(managedDoc, "name: pinned", "name: pinned-code", 1)
	tool = parseManaged(t, doc)
	got, err = tool.ManagedBinaryPath()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, filepath.Join(cache, "pinned", "1.2.3")) {
		t.Fatalf("path %q does not follow the binary name", got)
	}
}

// The override is explicit — the per-tool variable or a path in cli.binary —
// and a bare cli.binary name is NOT one (it is just the cache basename).
func TestManagedOverrideIsExplicitOnly(t *testing.T) {
	tool := parseManaged(t, managedDoc)
	if p, ok := tool.ManagedOverride(func(string) string { return "" }); ok {
		t.Fatalf("bare binary name counted as an override: %q", p)
	}
	env := map[string]string{"BASHY_TOOL_BINARY_PINNED": "/opt/homebrew/bin/pinned"}
	p, ok := tool.ManagedOverride(func(k string) string { return env[k] })
	if !ok || p != "/opt/homebrew/bin/pinned" {
		t.Fatalf("env override = %q, %v", p, ok)
	}
	tool.CLI.Binary = "/usr/local/bin/pinned"
	p, ok = tool.ManagedOverride(func(string) string { return "" })
	if !ok || p != "/usr/local/bin/pinned" {
		t.Fatalf("path override = %q, %v", p, ok)
	}
	if got := ToolBinaryOverrideEnv("kimi-cli"); got != "BASHY_TOOL_BINARY_KIMI_CLI" {
		t.Fatalf("override env = %q", got)
	}
}

// Catalog.Tools reports a malformed pin like a malformed command: the tool
// stays listed, the error is not hidden.
func TestCatalogReportsMalformedManagedPin(t *testing.T) {
	fleettest.Ring(t)
	c := New(WithRoot(t.TempDir()), WithBaselineFS(fstest.MapFS{}))
	tool := parseManaged(t, managedDoc)
	tool.CLI.Managed.License = ""
	if err := c.SaveTool(tool); err != nil {
		t.Fatal(err)
	}
	tools, errs := c.Tools(false)
	found := false
	for _, tl := range tools {
		if tl.Name == "pinned" {
			found = true
		}
	}
	if !found {
		t.Fatal("a malformed pin hid its tool")
	}
	var hit bool
	for _, err := range errs {
		if strings.Contains(err.Error(), `tool "pinned" managed install`) && strings.Contains(err.Error(), "license is required") {
			hit = true
		}
	}
	if !hit {
		t.Fatalf("errors did not report the malformed pin: %v", errs)
	}
}

// The MEASURED opencode line (eventsfail.go) matches the recipe's events_fail;
// a healthy stream, banner noise and an undeclared matcher do not.
func TestFailedEventMatchesMeasuredOpencodeLine(t *testing.T) {
	const failing = `{"type":"step_start","timestamp":1}
{"type":"error","error":{"name":"UnknownError","data":{"message":"Unexpected server error. Check server logs for details."}}}
`
	const healthy = `opencode 1.18.35
{"type":"step_start","timestamp":1}
{"type":"text","part":{"text":"SMOKE-OK"}}
{"type":"step_finish","part":{"reason":"stop"}}
`
	tool := parseManaged(t, managedDoc)
	line, ok := tool.FailedEvent([]byte(failing))
	if !ok || !strings.Contains(line, `"UnknownError"`) {
		t.Fatalf("FailedEvent = %q, %v", line, ok)
	}
	if line, ok := tool.FailedEvent([]byte(healthy)); ok {
		t.Fatalf("healthy stream read as failure: %q", line)
	}
	tool.CLI.Launch.EventsFail = EventsDone{}
	if _, ok := tool.FailedEvent([]byte(failing)); ok {
		t.Fatal("undeclared events_fail matched")
	}
}

// Every baseline recipe that declares a managed install is a complete pin for
// the five fleet platforms, records a licence, and the opencode recipe carries
// the measured failure kind. This is the test that keeps D12 honest: a recipe
// cannot ship a version without digests, or digests without a licence.
func TestBaselineManagedRecipesArePins(t *testing.T) {
	c := baseline(t)
	tools, errs := c.Tools(false)
	if len(errs) != 0 {
		t.Fatalf("tool parse errors: %v", errs)
	}
	platforms := []string{"darwin/arm64", "darwin/amd64", "linux/amd64", "linux/arm64", "windows/amd64"}
	want := map[string]string{ // tool -> self-update switch (empty = the vendor has none)
		"opencode": "OPENCODE_DISABLE_AUTOUPDATE=1",
		"codex":    "",
		"claude":   "DISABLE_AUTOUPDATER=1",
		"muse":     "",
		"agy":      "AGY_CLI_DISABLE_AUTO_UPDATE=1",
		"kimi-cli": "KIMI_CODE_NO_AUTO_UPDATE=1",
	}
	seen := map[string]bool{}
	for _, tl := range tools {
		if !tl.IsManaged() {
			continue
		}
		seen[tl.Name] = true
		m := tl.CLI.Managed
		if err := m.Validate(tl.Name); err != nil {
			t.Errorf("%s: %v", tl.Name, err)
		}
		for _, p := range platforms {
			if _, ok := m.Assets[p]; !ok {
				t.Errorf("%s: no asset for %s", tl.Name, p)
			}
		}
		if m.Source == "" || m.LicenseURL == "" {
			t.Errorf("%s: source and license_url must be recorded", tl.Name)
		}
		sw, known := want[tl.Name]
		if !known {
			t.Errorf("%s: managed recipe not in this test's roster — add its self-update switch", tl.Name)
		}
		if sw != "" && !contains(m.Env, sw) {
			t.Errorf("%s: env %v lacks the self-update switch %s", tl.Name, m.Env, sw)
		}
		if _, err := tl.BinmgrTool(); err != nil {
			t.Errorf("%s: BinmgrTool: %v", tl.Name, err)
		}
	}
	for name := range want {
		if !seen[name] {
			t.Errorf("baseline %s is not managed", name)
		}
	}
	oc, _ := c.Tool("opencode")
	if !oc.CLI.Launch.EventsFail.Declared() {
		t.Fatal("opencode declares no events_fail")
	}
	if _, ok := oc.FailedEvent([]byte(`{"type":"error","error":{"name":"UnknownError","data":{"message":"Unexpected server error. Check server logs for details."}}}`)); !ok {
		t.Fatal("opencode events_fail does not match the measured failure line")
	}
}
