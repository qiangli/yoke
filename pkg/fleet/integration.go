package fleet

import (
	"os"
	"path/filepath"
	"strings"
)

// ToolIntegration declares how a tool takes outside guidance and tools, so
// bashy can enable its skills in ANY tool, built in or custom, from data
// rather than per-tool code. It is read by `bashy skill export --tool` and
// `--user`. Paths may use ~ and $VAR / ${VAR} / ${VAR:-default}, so a tool
// whose home is relocatable (CODEX_HOME, HERMES_HOME, CLAUDE_CONFIG_DIR) is
// described once and follows the variable.
type ToolIntegration struct {
	// Detect is a path whose existence means this user has set the tool up
	// (its config home, ~/.claude). User-scope exports write a tool's roots
	// only when it is detected, or when the tool is named explicitly — bashy
	// never creates a config dir for a tool the user does not have.
	Detect string `yaml:"detect,omitempty" json:"detect,omitempty" doc:"path whose existence means the tool is set up for this user"`
	// Skills are the Agent Skills (SKILL.md folder) roots the tool reads.
	Skills IntegrationSkills `yaml:"skills,omitempty" json:"skills" doc:"Agent Skills roots the tool reads"`
	// Instructions are instruction files the tool loads (AGENTS.md, CLAUDE.md,
	// QWEN.md, .goosehints, ...). An export appends one bashy-managed block,
	// replaced in place on re-export, never touching the rest of the file.
	Instructions []IntegrationFile `yaml:"instructions,omitempty" json:"instructions,omitempty" doc:"instruction files the tool loads"`
	// MCP is where the tool's MCP server map lives. Declared data; the
	// writer is not wired yet (a user's own config is not rewritten blind).
	MCP *IntegrationMCP `yaml:"mcp,omitempty" json:"mcp,omitempty" doc:"the tool's MCP server configuration (declared)"`
	// ShellEnv is the environment variable that makes the tool run its shell
	// commands through a given shell (CLAUDE_CODE_SHELL, GOOSE_SHELL, SHELL).
	ShellEnv string `yaml:"shell_env,omitempty" json:"shell_env,omitempty" doc:"env var naming the shell the tool runs commands with"`
}

// IntegrationSkills lists skill roots by scope.
type IntegrationSkills struct {
	User    []string `yaml:"user,omitempty" json:"user,omitempty" doc:"user-scope skill roots"`
	Project []string `yaml:"project,omitempty" json:"project,omitempty" doc:"project-scope skill roots, relative to the repo root"`
}

// IntegrationFile is one instruction file and its scope.
type IntegrationFile struct {
	File  string `yaml:"file" json:"file" doc:"instruction file path (user scope) or repo-relative path (project scope)"`
	Scope string `yaml:"scope,omitempty" json:"scope,omitempty" doc:"user (default) or project"`
}

// IntegrationMCP locates a tool's MCP server map.
type IntegrationMCP struct {
	Config string   `yaml:"config" json:"config" doc:"config file path"`
	Format string   `yaml:"format" json:"format" doc:"json, jsonc, json5, toml or yaml"`
	Key    string   `yaml:"key" json:"key" doc:"dotted path of the server map"`
	CLI    []string `yaml:"cli,omitempty" json:"cli,omitempty" doc:"the tool's own command that adds a server (preferred over editing the file)"`
}

// Scope values for IntegrationFile.
const (
	ScopeUser    = "user"
	ScopeProject = "project"
)

// ExpandIntegrationPath expands ~ and $VAR, ${VAR}, ${VAR:-default} in an
// integration path against getenv (os.Getenv when nil). An unset variable
// with no default expands to "", so "${CODEX_HOME:-~/.codex}/skills" follows
// the variable and falls back to the documented home.
func ExpandIntegrationPath(p, home string, getenv func(string) string) string {
	if getenv == nil {
		getenv = os.Getenv
	}
	p = os.Expand(strings.TrimSpace(p), func(v string) string {
		name, def, hasDef := strings.Cut(v, ":-")
		if val := getenv(name); val != "" {
			return val
		}
		if hasDef {
			return def
		}
		return ""
	})
	if p == "~" {
		return home
	}
	if strings.HasPrefix(p, "~/") {
		return filepath.Join(home, p[2:])
	}
	return p
}

// Integrated reports whether the tool declares any integration surface.
func (t Tool) Integrated() bool {
	i := t.Integration
	return i.Detect != "" || len(i.Skills.User) > 0 || len(i.Skills.Project) > 0 ||
		len(i.Instructions) > 0 || i.MCP != nil || i.ShellEnv != ""
}
