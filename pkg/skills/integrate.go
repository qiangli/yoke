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
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/qiangli/yoke/pkg/fleet"
)

// integrationTarget is where one export writes for one tool.
type integrationTarget struct {
	Tool         string
	SkillRoots   []string
	Instructions []string
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
		tg := integrationTarget{Tool: t.Name}
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
