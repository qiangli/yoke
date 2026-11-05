package fleet

import (
	"strconv"
	"strings"
	"testing"
)

func TestAppShapingTrustedSchema(t *testing.T) {
	app := App{Name: "hello", Type: AppTypeShaped, Source: t.TempDir(), Shaping: &AppShaping{Kind: "cli", Build: []string{"go", "build", "-o", "{output}", "."}, Test: []string{"{artifact}", "--help"}}}
	if err := app.Validate(); err != nil {
		t.Fatal(err)
	}
	app.Shaping.Kind = "service"
	app.Shaping.Service = "hello-unit"
	if err := app.Validate(); err != nil {
		t.Fatal(err)
	}
	app.Shaping.Service = ""
	if err := app.Validate(); err == nil {
		t.Fatal("missing service accepted")
	}
	app.Shaping.Kind = "cli"
	app.Shaping.Build = []string{"go", "build", "."}
	if err := app.Validate(); err == nil {
		t.Fatal("missing output accepted")
	}
}
func TestAppShapingSetUsesExistingSchema(t *testing.T) {
	// Nested shaping fields remain data on the app record and round-trip strictly.
	body := `name: hello
kind: app
type: shaped
source: SOURCE
shaping:
  kind: cli
  build: [go, build, -o, "{output}", .]
  test: ["{artifact}", --help]
`
	body = strings.Replace(body, "SOURCE", strconv.Quote(t.TempDir()), 1)
	a, err := ParseApp("hello", []byte(body), nil)
	if err != nil {
		t.Fatal(err)
	}
	if a.ShapingKind() != "cli" || a.Shaping.Build[3] != "{output}" {
		t.Fatal(a)
	}
	if _, err = ParseApp("hello", []byte(strings.Replace(body, "  kind: cli", "  unknown: cli", 1)), nil); err == nil {
		t.Fatal("unknown shaping policy accepted")
	}
}
