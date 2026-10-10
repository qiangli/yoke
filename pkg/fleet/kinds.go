package fleet

import (
	"reflect"
	"sync"
)

// kindSpec declares one fleet noun. The registry below is the single place a
// kind's identity lives: its schema type, its store directory and env knobs,
// how a name resolves to its canonical holder, and which rings it carries.
// Adding a kind is one registerKind call — schema, name-collision checks,
// `verify` name enumeration and ring assembly all read the table.
type kindSpec struct {
	Name      string // singular noun: "tool"
	Plural    string // store/baseline directory: "tools"
	DirEnv    string // env var redirecting the local store
	PathEnv   string // env var listing read-only shared dirs
	Lifecycle func(*Catalog, string) (RecordLifecycle, bool)
	Type      reflect.Type // entry struct, walked for `schema` and --set paths
	// Lookup resolves a name or alias to the canonical name holding it.
	Lookup func(c *Catalog, name string) (canonical string, ok bool)
	// Names lists the canonical names `verify` walks when given none.
	Names func(c *Catalog) []string
	// Seeded: the kind has an embedded baseline ring. Mechanism: that ring
	// survives SeedsEnv=off (launch contracts, not roster data).
	Seeded, Mechanism bool
	// Overlay: local files marked as overlays merge into the lower ring.
	Overlay bool
	// Record is the write surface the generic show / add / set verbs are
	// built from (kind_verbs.go). Nil: the kind has no generic verbs yet.
	Record *recordSpec
	// Envelope is the document list key for kinds stored in bundled files.
	Envelope string
}

var (
	kindsMu sync.RWMutex
	kinds   []kindSpec
)

func registerKind(s kindSpec) {
	if s.Lifecycle != nil {
		s.Overlay = true
	}
	kindsMu.Lock()
	defer kindsMu.Unlock()
	kinds = append(kinds, s)
}

func unregisterKind(name string) {
	kindsMu.Lock()
	defer kindsMu.Unlock()
	for i, k := range kinds {
		if k.Name == name {
			kinds = append(kinds[:i:i], kinds[i+1:]...)
			return
		}
	}
}

func kindByName(name string) (kindSpec, bool) {
	kindsMu.RLock()
	defer kindsMu.RUnlock()
	for _, k := range kinds {
		if k.Name == name {
			return k, true
		}
	}
	return kindSpec{}, false
}

func kindByDir(dir string) (kindSpec, bool) {
	kindsMu.RLock()
	defer kindsMu.RUnlock()
	for _, k := range kinds {
		if k.Plural == dir {
			return k, true
		}
	}
	return kindSpec{}, false
}

// holder adapts a typed single-entry lookup (Catalog.Tool, …) to a kindSpec
// Lookup, given how the entry reports its canonical name.
func holder[T any](get func(*Catalog, string) (T, bool), canonical func(T) string) func(*Catalog, string) (string, bool) {
	return func(c *Catalog, name string) (string, bool) {
		if e, ok := get(c, name); ok {
			return canonical(e), true
		}
		return "", false
	}
}

// listed adapts a typed lister (Catalog.Models, …) to a kindSpec Names.
func listed[T any](list func(*Catalog) ([]T, []error), canonical func(T) string) func(*Catalog) []string {
	return func(c *Catalog) []string {
		rows, _ := list(c)
		var out []string
		for _, r := range rows {
			out = append(out, canonical(r))
		}
		return out
	}
}

func init() {
	for _, s := range []kindSpec{
		{
			Name: KindTool, Plural: dirTools, DirEnv: "BASHY_TOOLS_DIR", PathEnv: "BASHY_TOOLS_PATH",
			Type:      reflect.TypeOf(Tool{}),
			Lifecycle: lifecycleOf((*Catalog).Tool),
			Lookup:    holder((*Catalog).Tool, func(t Tool) string { return t.Name }),
			Names: listed(func(c *Catalog) ([]Tool, []error) { return c.Tools(false) },
				func(t Tool) string { return t.Name }),
			Seeded: true, Mechanism: true, Overlay: true,
			Record: toolRecord(),
		},
		{
			Name: KindModel, Plural: dirModels, DirEnv: "BASHY_MODELS_DIR", PathEnv: "BASHY_MODELS_PATH",
			Type:      reflect.TypeOf(Model{}),
			Lifecycle: lifecycleOf((*Catalog).Model),
			Lookup:    holder((*Catalog).Model, func(m Model) string { return m.Name }),
			Names:     listed((*Catalog).Models, func(m Model) string { return m.Name }),
			Seeded:    true, Overlay: true,
			Record: modelRecord(),
		},
		{
			Envelope: dirAgents,
			Name:     KindAgent, Plural: dirAgents, DirEnv: "BASHY_AGENTS_DIR", PathEnv: "BASHY_AGENTS_PATH",
			Type:      reflect.TypeOf(Agent{}),
			Lifecycle: lifecycleOf((*Catalog).Agent),
			Lookup:    holder((*Catalog).Agent, func(a Agent) string { return a.Name }),
			Names:     listed((*Catalog).Agents, func(a Agent) string { return a.Name }),
			Seeded:    true, Overlay: true,
			Record: agentRecord(),
		},
		{
			Name: KindPerson, Plural: dirPeople, DirEnv: "BASHY_PEOPLE_DIR", PathEnv: "BASHY_PEOPLE_PATH",
			Type:      reflect.TypeOf(Person{}),
			Lifecycle: lifecycleOf((*Catalog).Person),
			Lookup:    holder((*Catalog).Person, func(p Person) string { return p.Handle }),
			Names:     listed((*Catalog).People, func(p Person) string { return p.Handle }),
			Seeded:    true,
			Record:    personRecord(),
		},
		{
			Name: KindHost, Plural: dirHosts, DirEnv: "BASHY_HOSTS_DIR", PathEnv: "BASHY_HOSTS_PATH",
			Type:      reflect.TypeOf(Host{}),
			Lifecycle: lifecycleOf((*Catalog).Host),
			Lookup:    holder((*Catalog).Host, func(h Host) string { return h.Name }),
			Names:     listed((*Catalog).Hosts, func(h Host) string { return h.Name }),
			Seeded:    true,
			Record:    hostRecord(),
		},
		{
			Name: "plan", Plural: dirPlans, DirEnv: "BASHY_PLANS_DIR", PathEnv: "BASHY_PLANS_PATH",
			Type:      reflect.TypeOf(Plan{}),
			Lifecycle: lifecycleOf((*Catalog).Plan),
			Lookup:    holder((*Catalog).Plan, func(p Plan) string { return p.Name }),
			Names:     listed((*Catalog).Plans, func(p Plan) string { return p.Name }),
			Seeded:    true,
		},
		// Registered commands and apps have NO embedded ring by design (rod,
		// not fish): bashy ships the mechanism and never a catalog of them.
		{
			Name: KindCommand, Plural: dirCommands, DirEnv: "BASHY_COMMANDS_DIR", PathEnv: "BASHY_COMMANDS_PATH",
			Type:      reflect.TypeOf(Command{}),
			Lifecycle: lifecycleOf((*Catalog).Command),
			Lookup:    holder((*Catalog).Command, func(r Command) string { return r.Name }),
			Names:     listed((*Catalog).Commands, func(r Command) string { return r.Name }),
			Record:    commandRecord(),
		},
		{
			Name: KindApp, Plural: dirApps, DirEnv: "BASHY_APPS_DIR", PathEnv: "BASHY_APPS_PATH",
			Type:      reflect.TypeOf(App{}),
			Lifecycle: lifecycleOf((*Catalog).App),
			Lookup:    holder((*Catalog).App, func(a App) string { return a.Name }),
			Names:     listed((*Catalog).Apps, func(a App) string { return a.Name }),
			Record:    appRecord(),
		},
	} {
		registerKind(s)
	}
}

// pathEnvOf names the env var that mounts shared dirs for a kind.
func pathEnvOf(kind string) string {
	k, _ := kindByName(kind)
	return k.PathEnv
}
