package fleet

import (
	"os"
	"sort"
	"sync"
)

// marker pairs an environment variable with the tool whose presence it proves.
type marker struct{ env, tool string }

// markers returns the detection index in a deterministic order: a process that
// somehow sets two harnesses' markers must resolve the same way on every host.
func (c *Catalog) markers() []marker {
	tools, _ := c.Tools(true)
	sort.Slice(tools, func(i, j int) bool { return tools[i].Name < tools[j].Name })
	var out []marker
	for _, t := range tools {
		for _, env := range t.CLI.Launch.EnvMarkers {
			out = append(out, marker{env, t.Name})
		}
	}
	return out
}

// detectIn reads the environment against a marker index.
func detectIn(index []marker) (string, bool) {
	for _, m := range index {
		if os.Getenv(m.env) != "" {
			return m.tool, true
		}
	}
	// Name-valued conventions (AGENT=goose, AGENT=amp; Vercel's AI_AGENT).
	// These carry the name directly, so they need no registry entry.
	for _, env := range []string{"AGENT", "AI_AGENT"} {
		if v := os.Getenv(env); v != "" {
			return v, true
		}
	}
	return "", false
}

// DetectTool reports the agentic harness driving this process, from the
// environment markers each one sets (the CI=true analog of the agent world).
//
// The marker table used to be a Go literal in pkg/skills. It now lives beside
// every other fact about a tool, so teaching bashy to recognize a new harness
// is `bashy tool add`, not a code change.
//
// Detection yields a TOOL, never an agent. A running claude is not `007` — a
// nickname is minted by whoever launched it, and inventing one here would put
// a name in the record that resolves to nothing.
func (c *Catalog) DetectTool() (string, bool) { return detectIn(c.markers()) }

// DetectTool reports the harness driving this process, using the default
// catalog.
//
// bashy calls this on every start, so the marker index is built at most once
// per process. Only the INDEX is cached — it comes from the registry and does
// not change under a running process. The environment is read on every call,
// because that is the question being asked.
func DetectTool() (string, bool) {
	indexOnce.Do(func() { markerIndex = New().markers() })
	return detectIn(markerIndex)
}

var (
	indexOnce   sync.Once
	markerIndex []marker
)

// MarkerEnvs lists every environment variable DetectTool consults, plus the two
// name-valued conventions.
//
// Exported because the marker set is DATA (it comes from the tool registry, so
// `bashy tool add` can extend it) and callers need to enumerate it rather than
// hardcode it:
//
//   - a test that wants a genuinely agent-free environment must clear all of them,
//     and a hardcoded list would silently rot the first time a harness is added;
//   - `bashy doctor` can say WHY it believes an agent is driving the shell.
func MarkerEnvs() []string {
	indexOnce.Do(func() { markerIndex = New().markers() })
	out := make([]string, 0, len(markerIndex)+2)
	for _, m := range markerIndex {
		out = append(out, m.env)
	}
	return append(out, "AGENT", "AI_AGENT")
}
