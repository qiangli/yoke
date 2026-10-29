package verify

import (
	"slices"
	"strings"
	"testing"
)

// The unit-backed reader honours the full A1 vocabulary — all six
// decorators in both the positional and the keyword spelling — while a file
// where pkg/unit finds no contract still answers the minimal way.
func TestUnitReaderBothFormsAndFallback(t *testing.T) {
	body := `@require("test -d .")
@ensure(check: "test -f out")
@effects("read, write")
@guard(effects: "read,write,net")
@confirm()
@timeout("30s")
work() { touch out; }
work
`
	dir := tree(t, map[string]string{
		"app.bsh":          body,
		"checks/smoke.bsh": "run work\n",
	})
	c, err := UnitReader{}.ReadContract(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := c.FunctionNames(); strings.Join(got, ",") != "work" {
		t.Fatalf("functions = %v, want work", got)
	}
	if len(c.Functions) != 1 || len(c.Functions[0].Decorators) != 6 {
		t.Fatalf("decorators = %+v, want all six contract lines", c.Functions)
	}
	if c.Cap() != "net,read,write" {
		t.Fatalf("cap = %q, want the effects/keyword-guard union", c.Cap())
	}
	if len(c.Checks) != 1 || c.Checks[0].Name != "checks/smoke.bsh" {
		t.Fatalf("checks = %+v, want checks/smoke.bsh", c.Checks)
	}

	// Confirm and timeout are contract: they move the digest.
	bare, err := UnitReader{}.ReadContract(tree(t, map[string]string{
		"app.bsh":          "@require('true')\n@ensure('true')\nwork() { true; }\n",
		"checks/smoke.bsh": "run work\n",
	}))
	if err != nil {
		t.Fatal(err)
	}
	confirmed, err := UnitReader{}.ReadContract(tree(t, map[string]string{
		"app.bsh":          "@require('true')\n@ensure('true')\n@confirm()\nwork() { true; }\n",
		"checks/smoke.bsh": "run work\n",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if bare.Digest() == confirmed.Digest() {
		t.Fatal("adding @confirm did not move the contract digest")
	}

	// A file the unit grammar cannot read still answers the minimal way.
	legacy, err := UnitReader{}.ReadContract(tree(t, map[string]string{
		"legacy.bsh": "@require('true')\nbuild() {\n",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if got := legacy.FunctionNames(); strings.Join(got, ",") != "build" {
		t.Fatalf("fallback functions = %v, want build", got)
	}
}

// On the shared four-decorator vocabulary both readers answer alike: the
// unified reader changes nothing for trees the minimal one already read.
func TestUnitReaderAgreesWithMinimalReader(t *testing.T) {
	files := map[string]string{"app.bsh": unit, "checks/smoke.bsh": "run build\n"}
	minimal, err := DecoratorReader{}.ReadContract(tree(t, files))
	if err != nil {
		t.Fatal(err)
	}
	unified, err := UnitReader{}.ReadContract(tree(t, files))
	if err != nil {
		t.Fatal(err)
	}
	if minimal.Digest() != unified.Digest() {
		t.Fatal("the unified reader moved a digest the minimal reader fixed")
	}
	if !slices.Equal(minimal.FunctionNames(), unified.FunctionNames()) {
		t.Fatalf("functions differ: %v vs %v", minimal.FunctionNames(), unified.FunctionNames())
	}
}
