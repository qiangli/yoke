package agentlaunch

import (
	"strings"

	"github.com/qiangli/yoke/pkg/fleet"
)

// EventStdoutArgs returns the registry-declared argv that asks this launch's
// tool to stream structured events on stdout. Callers do not inspect tool
// names or spell provider flags; the fleet declaration is the adapter.
func EventStdoutArgs(l Launch) []string {
	return EventStdoutArgsWithCatalog(l, NewCatalog)
}

func EventStdoutArgsWithCatalog(l Launch, newCatalog CatalogFunc) []string {
	tool, ok := launchTool(l, newCatalog)
	if !ok {
		return nil
	}
	return tool.EventsStdoutArgv()
}

// EventFileArgs returns the registry-declared argv that asks this launch's
// tool to write structured events to path.
func EventFileArgs(l Launch, path string) []string {
	return EventFileArgsWithCatalog(l, path, NewCatalog)
}

func EventFileArgsWithCatalog(l Launch, path string, newCatalog CatalogFunc) []string {
	if strings.TrimSpace(path) == "" {
		return nil
	}
	tool, ok := launchTool(l, newCatalog)
	if !ok || !tool.HasEventsArg() {
		return nil
	}
	return tool.EventsArgv(path)
}

func launchTool(l Launch, newCatalog CatalogFunc) (fleet.Tool, bool) {
	if newCatalog == nil {
		newCatalog = NewCatalog
	}
	return newCatalog().Tool(l.ToolName)
}

// InsertBeforePrompt adds launch decoration without separating a trailing
// prompt-consuming option from its value. That argv shape is shared by AGY's
// `-p PROMPT`, aider's `--message PROMPT`, and similar registry templates.
func InsertBeforePrompt(argv, extra []string) []string {
	if len(argv) < 2 || len(extra) == 0 || ContainsArgSequence(argv, extra) {
		return argv
	}
	insertAt := len(argv) - 1
	if insertAt > 1 && strings.HasPrefix(argv[insertAt-1], "-") {
		insertAt--
	}
	out := make([]string, 0, len(argv)+len(extra))
	out = append(out, argv[:insertAt]...)
	out = append(out, extra...)
	out = append(out, argv[insertAt:]...)
	return out
}

func ContainsArgSequence(argv, want []string) bool {
	for i := 0; i+len(want) <= len(argv); i++ {
		match := true
		for j := range want {
			if argv[i+j] != want[j] {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}
