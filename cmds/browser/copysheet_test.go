package browsercmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qiangli/coreutils/tool"
	"github.com/qiangli/yoke/pkg/browser/wire"
)

// isolateCopySheet points sheets at a temp dir and records opened URLs.
func isolateCopySheet(t *testing.T) (dir string, opened *[]string) {
	t.Helper()
	dir = t.TempDir()
	savedDir, savedHooks := copySheetDir, hooks
	t.Cleanup(func() { copySheetDir, hooks = savedDir, savedHooks })
	copySheetDir = func() string { return dir }
	var urls []string
	hooks.openURL = func(u string) error { urls = append(urls, u); return nil }
	hooks.openBeside = func(int, string) (string, error) { return "", errors.New("no hub") }
	return dir, &urls
}

func writeSheetInput(t *testing.T, v any) string {
	t.Helper()
	b, _ := json.Marshal(v)
	p := filepath.Join(t.TempDir(), "fields.json")
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestCopySheetRendersSectionsAndEscapes(t *testing.T) {
	dir, opened := isolateCopySheet(t)
	in := writeSheetInput(t, map[string]any{
		"title": "Store <fields>",
		"sections": []map[string]any{
			{"name": "Listing", "fields": []map[string]any{
				{"label": "Description", "value": "line one\n<script>alert(1)</script>", "note": "paste as is"},
				{"label": "Category", "value": "Developer Tools"},
			}},
		},
	})
	out, errb, code := runTool(t, "--json", "copysheet", in)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errb)
	}
	var env struct {
		Path     string `json:"path"`
		Fields   int    `json:"fields"`
		Withheld []any  `json:"withheld"`
		Opened   bool   `json:"opened"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("bad json %q: %v", out, err)
	}
	if env.Fields != 2 || len(env.Withheld) != 0 || !env.Opened || filepath.Dir(env.Path) != dir {
		t.Fatalf("envelope = %+v", env)
	}
	page, _ := os.ReadFile(env.Path)
	s := string(page)
	if strings.Contains(s, "<script>alert(1)") || !strings.Contains(s, "&lt;script&gt;alert(1)") {
		t.Fatal("field value not HTML-escaped")
	}
	if strings.Count(s, `data-i="`) != 2 || !strings.Contains(s, "<h2>Listing</h2>") || !strings.Contains(s, "paste as is") {
		t.Fatalf("page lacks the fields, section or note:\n%s", s)
	}
	if strings.Contains(s, `http-equiv="refresh"`) {
		t.Fatal("a prepared sheet must not auto-refresh")
	}
	if len(*opened) != 1 || !strings.HasPrefix((*opened)[0], "file:///") {
		t.Fatalf("opened = %v", *opened)
	}
}

func TestCopySheetFlatFieldsNoOpenAndStdin(t *testing.T) {
	_, opened := isolateCopySheet(t)
	var out, errb bytes.Buffer
	rc := &tool.RunContext{Ctx: context.Background(), Dir: t.TempDir(), Stdio: tool.Stdio{
		In:  strings.NewReader(`{"fields":[{"section":"A","label":"x","value":"1"},{"section":"B","label":"y","value":"2"},{"section":"A","label":"z","value":"3"}]}`),
		Out: &out, Err: &errb,
	}}
	if code := cmd.Run(rc, []string{"--no-open", "copysheet", "-"}); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	page, err := os.ReadFile(strings.TrimSpace(out.String()))
	if err != nil {
		t.Fatal(err)
	}
	s := string(page)
	if a, b, z := strings.Index(s, "<h2>A</h2>"), strings.Index(s, "<h2>B</h2>"), strings.Index(s, ">z</b>"); a < 0 || b < 0 || z < a || z > b {
		t.Fatal("flat fields not grouped by section in first-appearance order")
	}
	if len(*opened) != 0 {
		t.Fatal("--no-open opened the page")
	}
}

func TestCopySheetWithholdsGuardedValues(t *testing.T) {
	isolateCopySheet(t)
	in := writeSheetInput(t, map[string]any{"fields": []map[string]any{
		{"label": "Card", "value": "4111 1111 1111 1111"},
		{"label": "Account password", "value": "hunter2-correct-horse"},
		{"label": "Company", "value": "Example Co"},
	}})
	out, errb, code := runTool(t, "--json", "--no-open", "copysheet", in)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errb)
	}
	var env struct {
		Path     string `json:"path"`
		Withheld []struct{ Label, Reason string }
	}
	_ = json.Unmarshal([]byte(out), &env)
	page, _ := os.ReadFile(env.Path)
	s := string(page)
	for _, secret := range []string{"4111 1111 1111 1111", "hunter2-correct-horse"} {
		if strings.Contains(s, secret) || strings.Contains(out, secret) {
			t.Fatalf("guarded value %q reached the sheet or the envelope", secret)
		}
	}
	if len(env.Withheld) != 2 || env.Withheld[0].Reason != "value:luhn" || env.Withheld[1].Reason != "field:label" {
		t.Fatalf("withheld = %+v", env.Withheld)
	}
	if !strings.Contains(s, "Example Co") || strings.Count(s, "Enter this yourself") != 2 {
		t.Fatal("clean field missing or guarded fields not marked for the human")
	}
}

type fakeTabs struct{ url string }

func (f fakeTabs) Execute(_ context.Context, a wire.Action) (*wire.Result, error) {
	if a.Type != wire.ActionTabs {
		return &wire.Result{Error: "unexpected"}, nil
	}
	b, _ := json.Marshal([]map[string]any{{"url": "https://other.example/", "driven": false}, {"url": f.url, "driven": true}})
	return &wire.Result{Success: true, Data: string(b)}, nil
}
func (fakeTabs) Close() error { return nil }

// A type/select the page refuses to script queues the value on that page's
// sheet: opened once, refreshing itself, guarded values blanked, exit 7.
func TestUnscriptableTypeQueuesOnTheCopySheet(t *testing.T) {
	dir, opened := isolateCopySheet(t)
	c := fakeTabs{url: "https://store.example/devconsole/settings?x=1"}
	cause := "guard: field check failed, refusing type: injection failed: The extensions gallery cannot be scripted."
	run := func(a wire.Action) (string, int) {
		var out, errb bytes.Buffer
		rc := &tool.RunContext{Ctx: context.Background(), Stdio: tool.Stdio{In: strings.NewReader(""), Out: &out, Err: &errb}}
		code, ok := queueCopyField(rc, context.Background(), c, a, cause, 0, false, false)
		if !ok {
			t.Fatalf("%+v not queued", a)
		}
		return out.String(), code
	}
	out, code := run(wire.Action{Type: wire.ActionType, Selector: "#name", Text: "DHNT Example"})
	if code != ExitCopySheetQueued || strings.TrimSpace(out) != "copysheet:queued" {
		t.Fatalf("first queue: exit %d out %q", code, out)
	}
	run(wire.Action{Type: wire.ActionSelect, Selector: "#category", Text: "Developer Tools"})
	run(wire.Action{Type: wire.ActionType, Selector: "#password", Text: "s3cret-value"})
	run(wire.Action{Type: wire.ActionType, Selector: "#name", Text: "DHNT Example Inc"})

	if len(*opened) != 1 {
		t.Fatalf("sheet opened %d times, want once", len(*opened))
	}
	files, _ := filepath.Glob(filepath.Join(dir, "queue-*"))
	if len(files) != 2 {
		t.Fatalf("queue files = %v", files)
	}
	for _, f := range files {
		b, _ := os.ReadFile(f)
		if strings.Contains(string(b), "s3cret-value") {
			t.Fatalf("guarded value written to %s", f)
		}
	}
	page, _ := os.ReadFile(strings.TrimSuffix(files[0], filepath.Ext(files[0])) + ".html")
	s := string(page)
	if !strings.Contains(s, `http-equiv="refresh"`) || !strings.Contains(s, "DHNT Example Inc") || strings.Contains(s, ">DHNT Example</pre>") {
		t.Fatalf("queued sheet wrong (refresh, or re-typed field not replaced):\n%s", s)
	}
	if strings.Count(s, `data-i="`) != 2 || !strings.Contains(s, "store.example/devconsole/settings") {
		t.Fatal("queued sheet lacks its two clean fields or the page it belongs to")
	}

	var out2, errb2 bytes.Buffer
	rc := &tool.RunContext{Ctx: context.Background(), Stdio: tool.Stdio{Out: &out2, Err: &errb2}}
	if _, ok := queueCopyField(rc, context.Background(), c, wire.Action{Type: wire.ActionType, Selector: "#q", Text: "x"}, "no element matches selector", 0, false, false); ok {
		t.Fatal("an ordinary failure must not be queued")
	}
	if _, ok := queueCopyField(rc, context.Background(), c, wire.Action{Type: wire.ActionClick, Selector: "#q"}, cause, 0, false, false); ok {
		t.Fatal("only type/select carry a value to paste")
	}
	// --json carries the sentence the agent relays; --no-open on a new page
	// opens nothing.
	var out3 bytes.Buffer
	rc3 := &tool.RunContext{Ctx: context.Background(), Stdio: tool.Stdio{Out: &out3, Err: &errb2}}
	code, ok := queueCopyField(rc3, context.Background(), fakeTabs{url: "https://second.example/form"}, wire.Action{Type: wire.ActionType, Selector: "#city", Text: "Springfield"}, cause, 0, true, true)
	var env struct {
		OutcomeClass string `json:"outcome_class"`
		TellUser     string `json:"tell_user"`
	}
	if !ok || code != ExitCopySheetQueued || json.Unmarshal(out3.Bytes(), &env) != nil || env.OutcomeClass != "copysheet_queued" || !strings.Contains(env.TellUser, "Copy buttons") {
		t.Fatalf("json queue: ok=%t code=%d %s", ok, code, out3.String())
	}
	if len(*opened) != 1 {
		t.Fatal("--no-open opened the new page's sheet")
	}
}

// With a connected hub the sheet opens beside the form, not as a file.
func TestCopySheetOpensBesideTheFormThroughTheHub(t *testing.T) {
	_, opened := isolateCopySheet(t)
	var gotPort int
	hooks.openBeside = func(port int, path string) (string, error) { gotPort = port; return "split", nil }
	in := writeSheetInput(t, map[string]any{"fields": []map[string]any{{"label": "Name", "value": "Ada"}}})
	out, errb, code := runTool(t, "--json", "--live-port", "58099", "copysheet", in)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errb)
	}
	var env struct {
		OpenedAs string `json:"opened_as"`
	}
	_ = json.Unmarshal([]byte(out), &env)
	if env.OpenedAs != "split" || gotPort != 58099 || len(*opened) != 0 {
		t.Fatalf("opened_as=%q port=%d file opens=%v", env.OpenedAs, gotPort, *opened)
	}
}
