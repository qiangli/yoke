package skills

// Tool integration: where a skill export lands for each agentic tool comes
// from the tool definitions' `integration:` block (pkg/fleet/integration.go),
// not from a list in this package. A tool is a target when it is named
// explicitly (--tool, the user's consent) or, for --user, when its detect path
// exists on this host — bashy never creates a config dir for a tool the user
// does not have. Instruction files get one bashy-managed block between
// markers, replaced in place on re-export; the rest of the file is the
// user's and is never rewritten.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/qiangli/yoke/pkg/fleet"
)

// integrationTarget is where one export writes for one tool.
type integrationTarget struct {
	Tool         string
	SkillRoots   []string
	Instructions []string
	MCP          *fleet.IntegrationMCP
}

// toolTargets resolves the integration targets for the named tools (always)
// and, when detected is set, every tool whose integration.detect path exists.
// scope is fleet.ScopeUser or fleet.ScopeProject; project paths are joined to
// repoRoot. Unknown names are an error: a typo must not export nowhere.
func toolTargets(tools []fleet.Tool, names []string, detected bool, scope, home, repoRoot string, getenv func(string) string) ([]integrationTarget, error) {
	byName := map[string]fleet.Tool{}
	for _, t := range tools {
		byName[t.Name] = t
		for _, a := range t.Aliases {
			if _, taken := byName[a]; !taken {
				byName[a] = t
			}
		}
	}
	want := map[string]bool{}
	for _, n := range names {
		t, ok := byName[n]
		if !ok {
			return nil, fmt.Errorf("skills: no tool %q in the fleet catalog (see `bashy tool list --all`)", n)
		}
		if !t.Integrated() {
			return nil, fmt.Errorf("skills: tool %q declares no integration (add an integration: block to its definition)", t.Name)
		}
		want[t.Name] = true
	}
	if detected {
		for _, t := range tools {
			if d := strings.TrimSpace(t.Integration.Detect); d != "" {
				if _, err := os.Stat(fleet.ExpandIntegrationPath(d, home, getenv)); err == nil {
					want[t.Name] = true
				}
			}
		}
	}
	var out []integrationTarget
	for _, t := range tools {
		if !want[t.Name] {
			continue
		}
		want[t.Name] = false // aliases resolve to the same tool once
		tg := integrationTarget{Tool: t.Name, MCP: t.Integration.MCP}
		roots := t.Integration.Skills.User
		if scope == fleet.ScopeProject {
			roots = t.Integration.Skills.Project
		}
		for _, r := range roots {
			if p := scopedPath(r, scope, home, repoRoot, getenv); p != "" {
				tg.SkillRoots = append(tg.SkillRoots, p)
			}
		}
		for _, f := range t.Integration.Instructions {
			s := f.Scope
			if s == "" {
				s = fleet.ScopeUser
			}
			if s != scope {
				continue
			}
			if p := scopedPath(f.File, scope, home, repoRoot, getenv); p != "" {
				tg.Instructions = append(tg.Instructions, p)
			}
		}
		out = append(out, tg)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Tool < out[j].Tool })
	return out, nil
}

func scopedPath(p, scope, home, repoRoot string, getenv func(string) string) string {
	p = fleet.ExpandIntegrationPath(p, home, getenv)
	if p == "" {
		return ""
	}
	if scope == fleet.ScopeProject && !filepath.IsAbs(p) {
		if repoRoot == "" {
			return ""
		}
		p = filepath.Join(repoRoot, p)
	}
	return filepath.Clean(p)
}

// instructionMarkers bound the bashy-managed block for one skill.
func instructionMarkers(skill string) (begin, end string) {
	return "<!-- bashy:skill:" + skill + ":begin (managed by `bashy skill export`; edit outside these markers) -->",
		"<!-- bashy:skill:" + skill + ":end -->"
}

// instructionBody is the pointer an instruction file carries: short, so it
// costs the tool almost no context, and it names the one way to load more.
func instructionBody(skill, description string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Skill `%s` is installed on this machine", skill)
	if d := strings.TrimSpace(description); d != "" {
		fmt.Fprintf(&b, ": %s", oneLine(d))
	}
	b.WriteString("\n")
	fmt.Fprintf(&b, "Load it before relevant work: `bashy skill show %s` (or read the `%s` skill folder in your skills directory).\n", skill, skill)
	return b.String()
}

func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 240 {
		s = s[:237] + "..."
	}
	return s
}

// UpsertInstructionBlock writes the bashy-managed block for skill into the
// file at path: replaced in place when the markers exist, appended otherwise.
// Everything outside the markers is preserved byte for byte. The file and its
// directory are created when missing. Returns whether the file changed.
func UpsertInstructionBlock(path, skill, body string) (bool, error) {
	begin, end := instructionMarkers(skill)
	block := begin + "\n" + strings.TrimRight(body, "\n") + "\n" + end + "\n"
	old, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return false, err
	}
	var next []byte
	if i := bytes.Index(old, []byte(begin)); i >= 0 {
		j := bytes.Index(old[i:], []byte(end))
		if j < 0 {
			return false, fmt.Errorf("skills: %s has a bashy begin marker for %q without its end marker; fix the file by hand", path, skill)
		}
		tail := old[i+j+len(end):]
		tail = bytes.TrimPrefix(tail, []byte("\n"))
		next = append(append(append([]byte{}, old[:i]...), block...), tail...)
	} else {
		next = append([]byte{}, old...)
		if len(next) > 0 && !bytes.HasSuffix(next, []byte("\n")) {
			next = append(next, '\n')
		}
		if len(next) > 0 {
			next = append(next, '\n')
		}
		next = append(next, block...)
	}
	if bytes.Equal(old, next) {
		return false, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false, err
	}
	mode := os.FileMode(0o644)
	if fi, err := os.Stat(path); err == nil {
		mode = fi.Mode().Perm()
	}
	return true, os.WriteFile(path, next, mode)
}

// --- MCP registration ---------------------------------------------------------

// mcpServerName and the entry below are the bashy MCP server every tool is
// offered: `bashy mcp serve`, speaking MCP over stdio.
const mcpServerName = "bashy"

var mcpServerEntry = map[string]any{
	"command": "bashy",
	"args":    []string{"mcp", "serve"},
}

// runMCPRegistration execs a tool's OWN MCP-registration command, the argv
// its integration.mcp.cli declares. A variable so tests capture the argv
// instead of running a real tool; a real registration is the tool's business,
// never a config file bashy rewrites blind.
var runMCPRegistration = func(argv []string, stdout, stderr io.Writer) error {
	c := exec.Command(argv[0], argv[1:]...)
	c.Stdout, c.Stderr = stdout, stderr
	return c.Run()
}

// registerToolMCP registers the bashy MCP server with one tool. The tool's
// own declared command is preferred and run verbatim (--mcp is the consent;
// --dry-run prints it instead); a tool that declares no command is TOLD what
// to add — the exact entry, the config path and the key — and its config file
// is never touched.
func registerToolMCP(w, errw io.Writer, tool string, mcp *fleet.IntegrationMCP, home string, getenv func(string) string, dryRun bool) error {
	if mcp == nil || (len(mcp.CLI) == 0 && (strings.TrimSpace(mcp.Config) == "" || strings.TrimSpace(mcp.Key) == "")) {
		return fmt.Errorf("skills: tool %q declares no MCP registration (add integration.mcp.cli, or integration.mcp.config and .key for the entry to print)", tool)
	}
	if len(mcp.CLI) > 0 {
		if dryRun {
			fmt.Fprintf(w, "mcp (dry run): %s\n", displayArgv(mcp.CLI))
			return nil
		}
		if err := runMCPRegistration(mcp.CLI, w, errw); err != nil {
			return fmt.Errorf("skills: %s's own registration command failed: %w", tool, err)
		}
		fmt.Fprintf(w, "mcp: registered the %s server with %s via its own command (%s)\n", mcpServerName, tool, displayArgv(mcp.CLI))
		return nil
	}
	doc, err := mcpEntryDoc(mcp.Format, mcp.Key)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "mcp: %s declares no registration command; add this entry yourself (its config file is never rewritten)\n", tool)
	fmt.Fprintf(w, "  config: %s\n", fleet.ExpandIntegrationPath(mcp.Config, home, getenv))
	fmt.Fprintf(w, "  key: %s\n", mcp.Key)
	for _, line := range strings.Split(strings.TrimRight(doc, "\n"), "\n") {
		fmt.Fprintf(w, "    %s\n", line)
	}
	return nil
}

// mcpEntryDoc renders the server entry under the tool's dotted key —
// "mcp_servers" nests the server one level down, "a.b" two — in the tool's
// declared config format (json, jsonc, json5, toml or yaml; json is the
// default).
func mcpEntryDoc(format, key string) (string, error) {
	var doc any = map[string]any{mcpServerName: mcpServerEntry}
	segs := strings.Split(strings.TrimSpace(key), ".")
	for i := len(segs) - 1; i >= 0; i-- {
		if segs[i] != "" {
			doc = map[string]any{segs[i]: doc}
		}
	}
	switch strings.ToLower(strings.TrimSpace(format)) {
	case "", "json", "jsonc", "json5":
		b, err := json.MarshalIndent(doc, "", "  ")
		if err != nil {
			return "", err
		}
		return string(b) + "\n", nil
	case "yaml", "yml":
		b, err := yaml.Marshal(doc)
		if err != nil {
			return "", err
		}
		return string(b), nil
	case "toml":
		var b strings.Builder
		writeTOML(&b, doc, nil)
		return b.String(), nil
	default:
		return "", fmt.Errorf("skills: unsupported MCP config format %q (json, jsonc, json5, toml or yaml)", format)
	}
}

// writeTOML emits the nested tables and scalar assignments of doc (maps,
// strings and string slices — the shapes an MCP entry produces), sorted by
// key so the output is stable.
func writeTOML(b *strings.Builder, doc any, path []string) {
	m, ok := doc.(map[string]any)
	if !ok {
		return
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	header := len(path) > 0
	for _, k := range keys {
		if _, isMap := m[k].(map[string]any); !isMap {
			header = false
			break
		}
	}
	if header {
		fmt.Fprintf(b, "[%s]\n", strings.Join(path, "."))
	}
	for _, k := range keys {
		switch v := m[k].(type) {
		case map[string]any:
			writeTOML(b, v, append(path, k))
		case string:
			fmt.Fprintf(b, "%s = %q\n", k, v)
		case []string:
			quoted := make([]string, len(v))
			for i, s := range v {
				quoted[i] = strconv.Quote(s)
			}
			fmt.Fprintf(b, "%s = [%s]\n", k, strings.Join(quoted, ", "))
		}
	}
}

// displayArgv joins an argv for display, quoting only what a copy-paste
// would split on.
func displayArgv(argv []string) string {
	parts := make([]string, len(argv))
	for i, a := range argv {
		if strings.ContainsAny(a, " \t\n\"'") {
			parts[i] = strconv.Quote(a)
		} else {
			parts[i] = a
		}
	}
	return strings.Join(parts, " ")
}
