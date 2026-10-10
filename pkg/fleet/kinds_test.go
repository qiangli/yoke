package fleet

import (
	"reflect"
	"strings"
	"testing"
)

// widget is a stand-in for a future kind: it is declared once, through the
// kind table, and nothing else in the package is edited for it.
type widget struct {
	Name  string `yaml:"name" doc:"widget id"`
	Color string `yaml:"color,omitempty" doc:"paint"`
}

func registerWidgetKind(t *testing.T, held map[string]string) {
	t.Helper()
	registerKind(kindSpec{
		Name:    "widget",
		Plural:  "widgets",
		DirEnv:  "BASHY_WIDGETS_DIR",
		PathEnv: "BASHY_WIDGETS_PATH",
		Type:    reflect.TypeOf(widget{}),
		Lookup: func(_ *Catalog, n string) (string, bool) {
			h, ok := held[n]
			return h, ok
		},
		Names: func(*Catalog) []string { return []string{"w1", "w2"} },
	})
	t.Cleanup(func() { unregisterKind("widget") })
}

func TestRegisteredKindGetsSchemaAndNameChecksForFree(t *testing.T) {
	isolatedFleetRoot(t)
	registerWidgetKind(t, map[string]string{"w1": "w1", "shiny": "w1"})

	var paths []string
	for _, f := range schemaFields("widget") {
		paths = append(paths, f.Path)
	}
	if got := strings.Join(paths, ","); got != "color,name" {
		t.Errorf("schema paths = %q, want color,name", got)
	}

	cat := New(WithoutCloudOverlay())
	if err := cat.claimName("widget", "w1", []string{"shiny"}, false); err != nil {
		t.Errorf("an entry re-claiming its own names must pass: %v", err)
	}
	err := cat.claimName("widget", "w2", []string{"shiny"}, false)
	if err == nil || !strings.Contains(err.Error(), `widget name "shiny" already belongs to "w1"`) {
		t.Errorf("collision error = %v", err)
	}
	if err := cat.claimName("widget", "w2", []string{"shiny"}, true); err != nil {
		t.Errorf("--force must take the name: %v", err)
	}

	if got := allNames(cat, "widget"); strings.Join(got, ",") != "w1,w2" {
		t.Errorf("allNames = %v", got)
	}

	dir := t.TempDir()
	t.Setenv("BASHY_WIDGETS_DIR", dir)
	if got := NounDir("/root", "widgets"); got != dir {
		t.Errorf("NounDir = %q, want the env override %q", got, dir)
	}
	shared := t.TempDir()
	t.Setenv("BASHY_WIDGETS_PATH", shared)
	if got := sharedDirs("widgets"); len(got) != 1 || got[0] != shared {
		t.Errorf("sharedDirs = %v, want [%s]", got, shared)
	}
}

func TestUnregisteredKindIsInert(t *testing.T) {
	cat := New(WithoutCloudOverlay())
	if nounType("widget") != nil || allNames(cat, "widget") != nil {
		t.Error("an unregistered kind must have no type and no names")
	}
	if err := cat.claimName("widget", "x", nil, false); err != nil {
		t.Errorf("an unregistered kind has no collisions: %v", err)
	}
}

func TestKindTableDeclaresEveryNoun(t *testing.T) {
	want := map[string]struct {
		dir, dirEnv, pathEnv        string
		seeded, mechanism, overlays bool
	}{
		KindTool:    {dirTools, "BASHY_TOOLS_DIR", "BASHY_TOOLS_PATH", true, true, true},
		KindModel:   {dirModels, "BASHY_MODELS_DIR", "BASHY_MODELS_PATH", true, false, true},
		KindAgent:   {dirAgents, "BASHY_AGENTS_DIR", "BASHY_AGENTS_PATH", true, false, true},
		KindPerson:  {dirPeople, "BASHY_PEOPLE_DIR", "BASHY_PEOPLE_PATH", true, false, true},
		KindHost:    {dirHosts, "BASHY_HOSTS_DIR", "BASHY_HOSTS_PATH", true, false, true},
		"plan":      {dirPlans, "BASHY_PLANS_DIR", "BASHY_PLANS_PATH", true, false, true},
		KindCommand: {dirCommands, "BASHY_COMMANDS_DIR", "BASHY_COMMANDS_PATH", false, false, true},
		KindApp:     {dirApps, "BASHY_APPS_DIR", "BASHY_APPS_PATH", false, false, true},
	}
	for name, w := range want {
		s, ok := kindByName(name)
		if !ok {
			t.Errorf("kind %q is not registered", name)
			continue
		}
		if s.Plural != w.dir || s.DirEnv != w.dirEnv || s.PathEnv != w.pathEnv ||
			s.Seeded != w.seeded || s.Mechanism != w.mechanism || s.Overlay != w.overlays {
			t.Errorf("kind %q = %+v, want %+v", name, s, w)
		}
		if byDir, ok := kindByDir(w.dir); !ok || byDir.Name != name {
			t.Errorf("kindByDir(%q) = %q, want %q", w.dir, byDir.Name, name)
		}
	}
}
