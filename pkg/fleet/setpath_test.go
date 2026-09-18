package fleet

import (
	"reflect"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/qiangli/yoke/pkg/fleet/fleettest"
)

func isolatedFleetRoot(t *testing.T) string {
	t.Helper()
	fleettest.Ring(t)
	for _, key := range []string{
		"BASHY_HOME", "BASHY_FLEET_DIR", "BASHY_SKILLS_DIR", "BASHY_TOOLS_DIR",
		"BASHY_MODELS_DIR", "BASHY_AGENTS_DIR",
	} {
		t.Setenv(key, t.TempDir())
	}
	return t.TempDir()
}

func TestAddFromGenericFields(t *testing.T) {
	root := isolatedFleetRoot(t)
	tests := []struct {
		name  string
		cmd   func(...Option) *cobra.Command
		args  []string
		check func(*Catalog)
	}{
		{
			name: "tool", cmd: NewToolsCmd,
			args: []string{"add", "rod-tool", "--set", "cli.binary=rod", "--set", "cli.launch.exec=rod {prompt}", "--hidden"},
			check: func(c *Catalog) {
				got, ok := c.Tool("rod-tool")
				if !ok || got.CLI.Binary != "rod" || !got.Hidden {
					t.Fatalf("tool = %+v, %v", got, ok)
				}
			},
		},
		{
			name: "model", cmd: NewModelsCmd,
			args: []string{"add", "rod-model", "--set", "provider=example", "--band", "3", "--band-source", "operator", "--id", "rod-tool=upstream"},
			check: func(c *Catalog) {
				got, ok := c.Model("rod-model")
				if !ok || got.Provider != "example" || got.Source != ModelSourceCloud || got.Band != 3 || got.ToolIDs["rod-tool"] != "upstream" {
					t.Fatalf("model = %+v, %v", got, ok)
				}
			},
		},
		{
			name: "agent", cmd: NewAgentsCmd,
			args: []string{"add", "rod-agent", "--set", "tool=codex", "--set", "model=gpt5.6-sol", "--ephemeral"},
			check: func(c *Catalog) {
				got, ok := c.Agent("rod-agent")
				if !ok || got.Tool != "codex" || got.Model != "gpt5.6-sol" || !got.Ephemeral {
					t.Fatalf("agent = %+v, %v", got, ok)
				}
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := runCmd(t, tc.cmd(WithRoot(root)), tc.args...); err != nil {
				t.Fatal(err)
			}
			tc.check(New(WithRoot(root)))
		})
	}
}

func TestSetUsesExistingValidation(t *testing.T) {
	root := isolatedFleetRoot(t)
	_, err := runCmd(t, NewModelsCmd(WithRoot(root)), "set", "fable5", "--set", "band=9")
	if err == nil || !strings.Contains(err.Error(), "out of range") {
		t.Fatalf("err = %v, want band validation", err)
	}
}

func TestUnsetClearsAnExistingValue(t *testing.T) {
	isolatedFleetRoot(t)
	model := Model{Name: "x", Display: "old"}
	if err := editPaths(KindModel, &model, nil, []string{"display"}); err != nil {
		t.Fatal(err)
	}
	if model.Display != "" {
		t.Fatalf("display survived unset: %q", model.Display)
	}
}

func TestSetCopiesEveryNounIntoLocalRing(t *testing.T) {
	root := isolatedFleetRoot(t)
	// Tools ship embedded; models and agents come from the test ring, which is
	// mounted as a shared dir — the note must name the ring the copy came from.
	for _, tc := range []struct {
		name, ring string
		cmd        *cobra.Command
	}{
		{"tool", "embedded", NewToolsCmd(WithRoot(root))},
		{"model", "shared", NewModelsCmd(WithRoot(root))},
		{"agent", "shared", NewAgentsCmd(WithRoot(root))},
	} {
		entry := map[string]string{"tool": "codex", "model": "fable5", "agent": "codex-gpt5.6-sol"}[tc.name]
		out, err := runCmd(t, tc.cmd, "set", entry, "--set", "display=local override")
		if err != nil {
			t.Fatalf("%s set: %v", tc.name, err)
		}
		if !strings.Contains(out, "note: copied "+entry+" from the "+tc.ring+" ring into the local store") {
			t.Errorf("%s copy note missing from %q", tc.name, out)
		}
	}
}

func TestUnknownPathPrintsSchema(t *testing.T) {
	root := isolatedFleetRoot(t)
	want, err := runCmd(t, NewToolsCmd(WithRoot(root)), "schema")
	if err != nil {
		t.Fatal(err)
	}
	got, Lel := runCmd(t, NewToolsCmd(WithRoot(root)), "set", "codex", "--set", "not_a_field=x")
	if Lel == nil {
		t.Fatal("unknown path was accepted")
	}
	if got != want {
		t.Fatalf("unknown-path table differs from schema\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

func TestShowFieldScalarAndSubtree(t *testing.T) {
	root := isolatedFleetRoot(t)
	scalar, err := runCmd(t, NewToolsCmd(WithRoot(root)), "show", "codex", "--field", "cli.binary")
	if err != nil {
		t.Fatal(err)
	}
	if scalar != "codex\n" {
		t.Fatalf("scalar = %q", scalar)
	}
	subtree, err := runCmd(t, NewToolsCmd(WithRoot(root)), "show", "codex", "--field", "cli.launch")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(subtree, "exec:") {
		t.Fatalf("subtree = %q", subtree)
	}
	jsonValue, err := runCmd(t, NewModelsCmd(WithRoot(root)), "show", "fable5", "--field", "band", "--json")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(jsonValue) != "4" {
		t.Fatalf("JSON field = %q", jsonValue)
	}
}

func TestSchemaCoversEverySerializedField(t *testing.T) {
	isolatedFleetRoot(t)
	for _, noun := range []string{KindTool, KindModel, KindAgent} {
		fields := schemaFields(noun)
		seen := make(map[string]schemaField, len(fields))
		for _, field := range fields {
			seen[field.Path] = field
			if field.Description == "" {
				t.Errorf("%s schema path %s has no meaning", noun, field.Path)
			}
		}
		assertSchemaFields(t, nounType(noun), "", seen)
	}
}

func assertSchemaFields(t *testing.T, typ reflect.Type, prefix string, seen map[string]schemaField) {
	t.Helper()
	typ = indirectType(typ)
	if typ.Kind() != reflect.Struct {
		return
	}
	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		name := yamlName(field)
		if name == "-" || name == "" {
			continue
		}
		path := name
		if prefix != "" {
			path = prefix + "." + name
		}
		if _, ok := seen[path]; !ok {
			t.Errorf("schema omitted %s", path)
		}
		child := indirectType(field.Type)
		if child.Kind() == reflect.Struct {
			assertSchemaFields(t, child, path, seen)
		} else if child.Kind() == reflect.Slice && indirectType(child.Elem()).Kind() == reflect.Struct {
			assertSchemaFields(t, child.Elem(), path+".<index>", seen)
		}
	}
}

func TestSetUnsetTopLevelAndLaunchPaths(t *testing.T) {
	isolatedFleetRoot(t)
	for _, tc := range []struct {
		noun   string
		record func() any
	}{
		{KindTool, func() any { return &Tool{Name: "x"} }},
		{KindModel, func() any { return &Model{Name: "x"} }},
		{KindAgent, func() any { return &Agent{Name: "x"} }},
	} {
		for _, field := range schemaFields(tc.noun) {
			if strings.Contains(field.Path, ".") && !strings.HasPrefix(field.Path, "cli.launch.") {
				continue
			}
			if strings.Contains(field.Path, "<") {
				continue
			}
			value := schemaTestValue(field.Type)
			if err := editPaths(tc.noun, tc.record(), []string{field.Path + "=" + value}, nil); err != nil {
				t.Errorf("set %s %s: %v", tc.noun, field.Path, err)
			}
			if err := editPaths(tc.noun, tc.record(), nil, []string{field.Path}); err != nil {
				t.Errorf("unset %s %s: %v", tc.noun, field.Path, err)
			}
		}
	}
}

func schemaTestValue(typ string) string {
	switch {
	case typ == "bool":
		return "true"
	case typ == "int" || typ == "uint" || typ == "float":
		return "1"
	case typ == "[]object":
		return "[]"
	case strings.HasPrefix(typ, "[]"):
		return "[a,b]"
	case strings.HasPrefix(typ, "map[") || typ == "object":
		return "{}"
	default:
		return "x"
	}
}

func TestNamedListPathsUseDotsNotPlatformSeparators(t *testing.T) {
	isolatedFleetRoot(t)
	tool := Tool{Name: "x"}
	if err := editPaths(KindTool, &tool, []string{"cli.versions.name=v1.2.download=https://example.invalid/v1"}, nil); err != nil {
		t.Fatal(err)
	}
	if len(tool.CLI.Versions) != 1 || tool.CLI.Versions[0].Version != "v1.2" {
		t.Fatalf("versions = %+v", tool.CLI.Versions)
	}
	if err := editPaths(KindTool, &tool, nil, []string{"cli.versions.name=v1.2"}); err != nil {
		t.Fatal(err)
	}
	if len(tool.CLI.Versions) != 0 {
		t.Fatalf("named unset left %+v", tool.CLI.Versions)
	}
	model := Model{Name: "x"}
	if err := editPaths(KindModel, &model, []string{"x_hosts.name=fixture.1.owner=team"}, nil); err != nil {
		t.Fatal(err)
	}
	if len(model.XHosts) != 1 || model.XHosts[0].Host != "fixture.1" || model.XHosts[0].Owner != "team" {
		t.Fatalf("hosts = %+v", model.XHosts)
	}
}
