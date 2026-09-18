// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package dag

import (
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/qiangli/coreutils/pkg/weavecli"
)

// Task is one target parsed from the markdown source.
type Task struct {
	Name string
	Desc string
	Lang string // interpreter tag from the fenced-code info string ("" => bash)
	Body string

	Requires  []string
	Inputs    []string
	Sources   []string
	Generates []string
	Env       []string // per-target KEY=VALUE overrides (make's target-specific vars)

	// P1 #5 — Matrix: parameterized target. `Matrix: os=linux,darwin arch=amd64`
	// is space-separated key=csv pairs; expandMatrix replaces this target with one
	// concrete node per combination (see expand.go). Empty on a non-matrix target.
	Matrix map[string][]string

	// P1 #7 — Secrets: names resolved (process env first) and injected into the
	// target's env, with their VALUES redacted from captured output (see engine.go).
	Secrets []string

	// P1 #8 — Artifacts: declared output paths/globs recorded in the result after
	// the target succeeds (and copied to $DAG_ARTIFACTS_DIR when set).
	Artifacts []string

	// P1 #10 — When: a shell condition; the engine skips the target (not a failure)
	// when it exits non-zero (see engine.go).
	When string

	// P2 #15 — ExitCodes maps a body exit code to ok|skip|retry|fail.
	ExitCodes map[int]string

	// P2 #13 — Host is placement intent for a future remote executor.
	Host string

	// Fleet placement requirements. These describe capability, never reach:
	// Venue selects an isolation lane, Match names host facts, Exclusive drains
	// the selected worker, and MemPerTask reserves memory capacity.
	Venue           string
	Distribution    Distribution
	Match           map[string]string
	Exclusive       bool
	CPUPerTask      int
	MemPerTask      uint64
	Accelerator     AcceleratorRequest
	MinimumCapacity map[string]uint64
	TopologyClass   string
	CohortSize      int
	Reducer         string

	// P0 #2 — per-target execution policy enforced by the engine.
	Timeout time.Duration // `Timeout: 90s` — 0 means no deadline
	Retries int           // `Retries: 3` — extra attempts after the first on failure
	Backoff time.Duration // optional `Retries: 3 backoff=2s` — sleep between attempts

	// P2 (parsed-and-ignored in P1, so a contract-bearing file parses today).
	// Require is the precondition set (Sprint 203): shell checks evaluated
	// BEFORE the body; one failing means the body never runs. Its plural
	// neighbour Requires is make's prerequisite list — a prerequisite is a
	// precondition the engine can satisfy by running a target, a Require: is
	// one it cannot.
	Require []string
	Ensure  []string
	Effects []string
	Tools   []string // builtin tool preflight: `Tools: git go:1.25 podman`

	Line int // 1-based source line of the heading, for error messages
}

// Document is a parsed DAG markdown file.
type Document struct {
	Path     string
	Name     string   // optional file-level frontmatter `name`
	Desc     string   // optional file-level frontmatter `description`
	Default  string   // optional frontmatter `default` — make's .DEFAULT_GOAL
	Includes []string // optional frontmatter `include` — files merged in (make's `include`)
	Tasks    []*Task
	Order    []string // task names in declaration order (deterministic listing)

	// P1 #6 — frontmatter `vars:` block of NAME=value defaults, in declaration
	// order. expandVars resolves these (CLI KEY=VALUE wins) and substitutes
	// ${NAME} in metadata before BuildGraph (see expand.go).
	Vars []DocVar

	byName map[string]*Task
}

// DocVar is one frontmatter `vars:` entry. Op is the assignment operator:
// "=" / ":=" set a default value (overriding the process env), "?=" sets it
// only when the name is not already defined.
type DocVar struct {
	Name  string
	Op    string
	Value string
}

// Lookup returns the named task and whether it exists.
func (d *Document) Lookup(name string) (*Task, bool) {
	t, ok := d.byName[name]
	return t, ok
}

var metaKeys = map[string]bool{
	"requires": true, "inputs": true, "sources": true,
	"generates": true, "env": true, "require": true, "ensure": true, "effects": true, "tools": true,
	"timeout": true, "retries": true,
	// P1 metadata.
	"matrix": true, "secrets": true, "artifacts": true, "when": true,
	// P2 metadata.
	"exitcodes": true, "host": true,
	"venue": true, "lane": true, "match": true, "requires-host": true,
	"distribution": true,
	"exclusive":    true, "cpupertask": true, "cpu-per-task": true,
	"mempertask": true, "mem-per-task": true, "memory": true,
	"accelerator": true, "acceleratorfamily": true, "accelerator-family": true,
	"acceleratorcount": true, "accelerator-count": true,
	"acceleratormemory": true, "accelerator-memory": true,
	"capacity": true, "resources": true,
	"topology": true, "topologyclass": true, "topology-class": true,
	"cohort": true, "cohortsize": true, "cohort-size": true,
	"reducer": true, "reduce": true,
}

// Parse reads a DAG markdown document. The format:
//   - Optional YAML frontmatter (`---` … `---`) with `name`/`description`.
//   - If a `## Tasks` heading exists, targets are its `### name` children;
//     otherwise targets are top-level `## name` headings.
//   - Under each target heading: prose description, metadata lines
//     (`Requires:`/`Inputs:`/`Sources:`/`Generates:`/`Ensure:`/`Effects:` —
//     only these keys are metadata, every other line is description), and a
//     fenced code block whose info string selects the interpreter (default bash).
func Parse(r io.Reader, path string) (*Document, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, errf(weavecli.ExitInvalidArg, "read %s: %v", path, err)
	}
	lines := strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
	doc := &Document{Path: path, byName: map[string]*Task{}}

	start := 0
	if len(lines) > 0 && strings.TrimSpace(lines[0]) == "---" {
		j := 1
		inVars := false
		inIncludes := false
		for j < len(lines) && strings.TrimSpace(lines[j]) != "---" {
			ln := lines[j]
			// An `include:` block: indented `- entry` items until the next
			// non-indented key. The inline form (`include: a.md b.md`) still
			// works; this is the readable shape once there is more than one.
			if inIncludes {
				if t := strings.TrimSpace(ln); indented(ln) && t != "" {
					if item := strings.TrimSpace(strings.TrimPrefix(t, "-")); t != item && item != "" {
						doc.Includes = append(doc.Includes, item)
						j++
						continue
					}
				}
				inIncludes = false // dedent, blank, or a non-item line ends the block
			}
			// A `vars:` block: indented `NAME = value` lines until the next
			// non-indented key. Parsed case-sensitively (var names keep their case).
			if inVars {
				if indented(ln) && strings.TrimSpace(ln) != "" {
					if name, op, val, ok := parseVarLine(ln); ok {
						doc.Vars = append(doc.Vars, DocVar{Name: name, Op: op, Value: val})
					}
					j++
					continue
				}
				inVars = false // dedent (or blank handled below) ends the block
			}
			if k, v, ok := frontmatterKV(ln); ok {
				switch k {
				case "name":
					doc.Name = v
				case "description":
					doc.Desc = v
				case "default", "default_goal":
					doc.Default = v
				case "include", "includes":
					// `include:` with nothing after it opens a `- entry` block;
					// otherwise the value is an inline list.
					if strings.TrimSpace(v) == "" {
						inIncludes = true
					} else {
						doc.Includes = append(doc.Includes, splitList(v)...)
					}
				case "vars", "variables":
					inVars = true
					// Allow an inline form too: `vars: A=1 B=2`.
					for _, f := range strings.Fields(v) {
						if name, op, val, ok := parseVarLine(f); ok {
							doc.Vars = append(doc.Vars, DocVar{Name: name, Op: op, Value: val})
						}
					}
				}
			}
			j++
		}
		if j < len(lines) {
			j++ // consume closing ---
		}
		start = j
	}

	nested := false
	for _, ln := range lines[start:] {
		if lvl, text, ok := parseHeading(ln); ok && lvl == 2 && strings.EqualFold(text, "Tasks") {
			nested = true
			break
		}
	}
	taskLevel := 2
	if nested {
		taskLevel = 3
	}

	var (
		cur      *Task
		curLines []string
		inTasks  = !nested
		perr     *Error
	)
	flush := func() {
		if cur == nil {
			return
		}
		cur.absorb(curLines)
		if _, dup := doc.byName[cur.Name]; dup {
			if perr == nil {
				perr = errf(weavecli.ExitInvalidArg, "duplicate target %q (line %d)", cur.Name, cur.Line)
			}
		} else {
			doc.byName[cur.Name] = cur
			doc.Tasks = append(doc.Tasks, cur)
			doc.Order = append(doc.Order, cur.Name)
		}
		cur, curLines = nil, nil
	}

	// inFence tracks whether we're inside a target's fenced code block, so a
	// shell `#`/`##` comment (or any ```-info line) inside a body is never
	// mistaken for a markdown heading.
	inFence := false
	fenceMarker := ""
	for idx := start; idx < len(lines); idx++ {
		ln := lines[idx]
		if marker, info, ok := fenceOpen(ln); ok {
			if !inFence {
				inFence, fenceMarker = true, marker
			} else if marker == fenceMarker && info == "" {
				inFence = false
			}
			if cur != nil {
				curLines = append(curLines, ln)
			}
			continue
		}
		if inFence {
			if cur != nil {
				curLines = append(curLines, ln)
			}
			continue
		}
		if lvl, text, ok := parseHeading(ln); ok {
			if nested {
				if lvl == 2 && strings.EqualFold(text, "Tasks") {
					flush()
					inTasks = true
					continue
				}
				if lvl <= 2 {
					flush()
					inTasks = false
					continue
				}
			}
			if lvl == taskLevel && inTasks {
				flush()
				cur = &Task{Name: text, Line: idx + 1}
				continue
			}
			if !nested && lvl <= taskLevel {
				flush() // a level-1 title (or sibling level-2) ends the current target
				continue
			}
			if cur != nil {
				curLines = append(curLines, ln) // deeper heading inside a body region
			}
			continue
		}
		if cur != nil {
			curLines = append(curLines, ln)
		}
	}
	flush()

	if perr != nil {
		return nil, perr
	}
	return doc, nil
}

// ParseFile parses the DAG markdown at path, resolving any frontmatter
// `include:` files (paths relative to the including file). Like make's
// `include`, included targets are merged in; the including file (and an
// earlier-listed include) wins a name collision. A file is parsed at most once,
// so diamonds and include cycles terminate safely.
func ParseFile(path string) (*Document, error) {
	return parseFileSeen(path, map[string]bool{})
}

func parseFileSeen(path string, seen map[string]bool) (*Document, error) {
	abs, _ := filepath.Abs(path)
	seen[abs] = true
	f, err := os.Open(path)
	if err != nil {
		return nil, errf(weavecli.ExitInvalidArg, "%v", err)
	}
	doc, err := Parse(f, path)
	f.Close()
	if err != nil {
		return nil, err
	}
	dir := filepath.Dir(path)
	for _, inc := range doc.Includes {
		incPath := inc
		if isRemoteInclude(inc) {
			// Remote includes are keyed by their pinned spec, not by the
			// cache path: the same pin reached from two files must dedupe
			// even though the cache file is shared.
			spec, err := parseRemoteInclude(inc)
			if err != nil {
				return nil, err
			}
			if seen[spec.key()] {
				continue
			}
			seen[spec.key()] = true
			if incPath, err = resolveRemoteInclude(spec); err != nil {
				return nil, err
			}
		} else {
			// An absolute include is used as-is; filepath.Join would splice
			// it onto the including file's directory and lose the root.
			if !filepath.IsAbs(inc) {
				incPath = filepath.Join(dir, inc)
			}
			if cabs, _ := filepath.Abs(incPath); seen[cabs] {
				continue // already merged (dedupe + cycle break)
			}
		}
		child, err := parseFileSeen(incPath, seen)
		if err != nil {
			return nil, err
		}
		doc.mergeFrom(child)
	}
	return doc, nil
}

// mergeFrom adds child's targets that aren't already defined (this document and
// earlier includes win name collisions).
func (d *Document) mergeFrom(child *Document) {
	for _, name := range child.Order {
		if _, exists := d.byName[name]; exists {
			continue
		}
		t := child.byName[name]
		d.byName[name] = t
		d.Tasks = append(d.Tasks, t)
		d.Order = append(d.Order, name)
	}
}

// Discover locates the task file in dir: a file named dag.md in ANY
// letter case, else the first (lexical) *.md containing a `## Tasks`
// section. Case is a transcription artifact, not identity (the dhnt
// alphabet has no upper case), so dag.md ≡ DAG.md ≡ Dag.md on every
// filesystem; when a case-sensitive filesystem holds several variants,
// lowercase `dag.md` wins, then the ecosystem-caps `DAG.md`, then the
// lexically first remaining variant.
func Discover(dir string) (string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", errf(weavecli.ExitInvalidArg, "%v", err)
	}
	var variants, mds []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if strings.EqualFold(e.Name(), "dag.md") {
			variants = append(variants, e.Name())
		}
		if strings.HasSuffix(strings.ToLower(e.Name()), ".md") {
			mds = append(mds, e.Name())
		}
	}
	if len(variants) > 0 {
		sort.Slice(variants, func(i, j int) bool {
			ri, rj := dagNameRank(variants[i]), dagNameRank(variants[j])
			if ri != rj {
				return ri < rj
			}
			return variants[i] < variants[j]
		})
		return filepath.Join(dir, variants[0]), nil
	}
	sort.Strings(mds)
	for _, name := range mds {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		if hasTasksSection(data) {
			return filepath.Join(dir, name), nil
		}
	}
	return "", errf(weavecli.ExitInvalidArg,
		"no dag.md (any case) or *.md with a '## Tasks' section in %s", dir)
}

// dagNameRank orders same-name case variants: lowercase first (the
// dhnt-native spelling), the caps compat spelling second.
func dagNameRank(name string) int {
	switch name {
	case "dag.md":
		return 0
	case "DAG.md":
		return 1
	}
	return 2
}

func hasTasksSection(data []byte) bool {
	for _, ln := range strings.Split(string(data), "\n") {
		if lvl, text, ok := parseHeading(ln); ok && lvl == 2 && strings.EqualFold(text, "Tasks") {
			return true
		}
	}
	return false
}

// absorb splits a target's raw lines into description, metadata, and body.
func (t *Task) absorb(lines []string) {
	var desc, body []string
	inBody, bodyDone := false, false
	fenceCh := ""
	for _, ln := range lines {
		if bodyDone {
			break
		}
		if inBody {
			if strings.TrimSpace(ln) == fenceCh {
				inBody, bodyDone = false, true
				continue
			}
			body = append(body, ln)
			continue
		}
		if ch, _, ok := fenceOpen(ln); ok {
			inBody, fenceCh = true, ch
			_, lang, _ := fenceOpen(ln)
			t.Lang = lang
			continue
		}
		if k, v, ok := parseMeta(ln); ok {
			switch k {
			case "requires":
				t.Requires = append(t.Requires, splitList(v)...)
			case "inputs":
				t.Inputs = append(t.Inputs, splitList(v)...)
			case "sources":
				t.Sources = append(t.Sources, splitList(v)...)
			case "generates":
				t.Generates = append(t.Generates, splitList(v)...)
			case "env":
				t.Env = append(t.Env, splitList(v)...)
			case "require":
				if s := strings.TrimSpace(v); s != "" {
					t.Require = append(t.Require, s)
				}
			case "ensure":
				if s := strings.TrimSpace(v); s != "" {
					t.Ensure = append(t.Ensure, s)
				}
			case "effects":
				t.Effects = append(t.Effects, splitList(v)...)
			case "tools":
				t.Tools = append(t.Tools, splitList(v)...)
			case "matrix":
				// `Matrix: os=linux,darwin arch=amd64,arm64` — space-separated
				// key=csv pairs, each value list comma-separated.
				if t.Matrix == nil {
					t.Matrix = map[string][]string{}
				}
				for _, f := range strings.Fields(v) {
					if i := strings.IndexByte(f, '='); i > 0 {
						key := strings.TrimSpace(f[:i])
						for _, val := range strings.Split(f[i+1:], ",") {
							if val = strings.TrimSpace(val); val != "" {
								t.Matrix[key] = append(t.Matrix[key], val)
							}
						}
					}
				}
			case "secrets":
				t.Secrets = append(t.Secrets, splitList(v)...)
			case "artifacts":
				t.Artifacts = append(t.Artifacts, splitList(v)...)
			case "when":
				if s := strings.TrimSpace(v); s != "" {
					t.When = s
				}
			case "exitcodes":
				t.ExitCodes = parseExitCodes(v)
			case "host":
				t.Host = strings.TrimSpace(v)
			case "venue", "lane":
				t.Venue = strings.TrimSpace(v)
			case "distribution":
				t.Distribution = Distribution(strings.TrimSpace(v))
			case "match", "requires-host":
				if t.Match == nil {
					t.Match = map[string]string{}
				}
				for _, f := range strings.Fields(strings.ReplaceAll(v, ",", " ")) {
					if k, val, ok := strings.Cut(f, "="); ok && strings.TrimSpace(k) != "" && strings.TrimSpace(val) != "" {
						t.Match[strings.TrimSpace(k)] = strings.TrimSpace(val)
					}
				}
			case "exclusive":
				t.Exclusive = strings.EqualFold(strings.TrimSpace(v), "true") || strings.TrimSpace(v) == "1"
			case "cpupertask", "cpu-per-task":
				var err error
				t.CPUPerTask, err = strconv.Atoi(strings.TrimSpace(v))
				if err != nil {
					t.CPUPerTask = -1
				}
			case "mempertask", "mem-per-task", "memory":
				if n, ok := parseMemoryBytes(v); ok {
					t.MemPerTask = n
				}
			case "accelerator":
				t.Accelerator.Kind = strings.TrimSpace(v)
			case "acceleratorfamily", "accelerator-family":
				t.Accelerator.Family = strings.TrimSpace(v)
			case "acceleratorcount", "accelerator-count":
				var err error
				t.Accelerator.Count, err = strconv.Atoi(strings.TrimSpace(v))
				if err != nil {
					t.Accelerator.Count = -1
				}
			case "acceleratormemory", "accelerator-memory":
				if n, ok := parseMemoryBytes(v); ok {
					t.Accelerator.MemoryBytes = n
				} else {
					t.Accelerator.invalid = true
				}
			case "capacity", "resources":
				if t.MinimumCapacity == nil {
					t.MinimumCapacity = map[string]uint64{}
				}
				for _, field := range strings.Fields(strings.ReplaceAll(v, ",", " ")) {
					key, quantity, ok := strings.Cut(field, "=")
					if !ok || strings.TrimSpace(key) == "" {
						t.MinimumCapacity["invalid"] = 0
						continue
					}
					if n, ok := parseCapacityQuantity(quantity); ok {
						t.MinimumCapacity[strings.TrimSpace(key)] = n
					} else {
						t.MinimumCapacity[strings.TrimSpace(key)] = 0
					}
				}
			case "topology", "topologyclass", "topology-class":
				t.TopologyClass = strings.TrimSpace(v)
			case "cohort", "cohortsize", "cohort-size":
				t.CohortSize, _ = strconv.Atoi(strings.TrimSpace(v))
			case "reducer", "reduce":
				t.Reducer = strings.TrimSpace(v)
			case "timeout":
				if d, err := time.ParseDuration(strings.TrimSpace(v)); err == nil {
					t.Timeout = d
				}
			case "retries":
				// `Retries: 3` or `Retries: 3 backoff=2s` — first bare number is
				// the retry count, an optional backoff=<dur> sets the inter-attempt sleep.
				for _, f := range strings.Fields(v) {
					if rest, ok := strings.CutPrefix(f, "backoff="); ok {
						if d, err := time.ParseDuration(rest); err == nil {
							t.Backoff = d
						}
						continue
					}
					if n, err := strconv.Atoi(f); err == nil {
						t.Retries = n
					}
				}
			}
			continue
		}
		desc = append(desc, ln)
	}
	t.Body = strings.Join(body, "\n")
	if t.Body != "" {
		t.Body += "\n"
	}
	t.Desc = strings.TrimSpace(strings.Join(desc, "\n"))
}

// parseHeading parses an ATX heading ("##  Name"), returning the level (1-6)
// and trimmed text. A `#` run must be followed by a space and non-empty text.
func parseHeading(line string) (level int, text string, ok bool) {
	s := strings.TrimRight(line, " \t")
	i := 0
	for i < len(s) && s[i] == '#' {
		i++
	}
	if i == 0 || i > 6 || i >= len(s) || s[i] != ' ' {
		return 0, "", false
	}
	text = strings.TrimSpace(s[i:])
	if text == "" {
		return 0, "", false
	}
	return i, text, true
}

// fenceOpen reports an opening code fence, the fence marker ("```" or "~~~"),
// and the trimmed info string (interpreter tag).
func fenceOpen(line string) (marker, info string, ok bool) {
	s := strings.TrimSpace(line)
	switch {
	case strings.HasPrefix(s, "```"):
		return "```", strings.TrimSpace(strings.TrimPrefix(s, "```")), true
	case strings.HasPrefix(s, "~~~"):
		return "~~~", strings.TrimSpace(strings.TrimPrefix(s, "~~~")), true
	}
	return "", "", false
}

// parseMeta recognizes a metadata line "Key: value" where Key is one of the
// known metadata keys (case-insensitive). Non-metadata "x: y" prose lines are
// left for the description, which avoids treating a prose colon as metadata.
func parseMeta(line string) (key, value string, ok bool) {
	i := strings.IndexByte(line, ':')
	if i <= 0 {
		return "", "", false
	}
	k := strings.ToLower(strings.TrimSpace(line[:i]))
	if !metaKeys[k] {
		return "", "", false
	}
	return k, strings.TrimSpace(line[i+1:]), true
}

// parseMemoryBytes accepts an integer byte count and the compact binary units
// commonly used in task metadata (for example, 4GiB or 512MiB).
func parseMemoryBytes(v string) (uint64, bool) {
	s := strings.ToLower(strings.TrimSpace(v))
	multiplier := uint64(1)
	for _, unit := range []struct {
		suffix string
		bytes  uint64
	}{
		{"gib", 1 << 30}, {"gi", 1 << 30}, {"gb", 1 << 30},
		{"mib", 1 << 20}, {"mi", 1 << 20}, {"mb", 1 << 20},
		{"kib", 1 << 10}, {"ki", 1 << 10}, {"kb", 1 << 10},
		{"b", 1},
	} {
		suffix, n := unit.suffix, unit.bytes
		if strings.HasSuffix(s, suffix) {
			s = strings.TrimSpace(strings.TrimSuffix(s, suffix))
			multiplier = n
			break
		}
	}
	n, err := strconv.ParseUint(s, 10, 64)
	if err != nil || n > ^uint64(0)/multiplier {
		return 0, false
	}
	return n * multiplier, true
}

func parseCapacityQuantity(v string) (uint64, bool) {
	return parseMemoryBytes(v)
}

func frontmatterKV(line string) (key, value string, ok bool) {
	i := strings.IndexByte(line, ':')
	if i <= 0 {
		return "", "", false
	}
	return strings.ToLower(strings.TrimSpace(line[:i])),
		strings.Trim(strings.TrimSpace(line[i+1:]), `"'`), true
}

func splitList(v string) []string {
	return strings.FieldsFunc(v, func(r rune) bool {
		return r == ',' || unicode.IsSpace(r)
	})
}

// indented reports whether a line begins with a space or tab.
func indented(line string) bool {
	return len(line) > 0 && (line[0] == ' ' || line[0] == '\t')
}

// parseVarLine parses one `vars:` entry. Accepted forms (most specific first):
//
//	NAME ?= value   default-if-unset
//	NAME := value   immediate
//	NAME = value    set
//	NAME: value     set (YAML-style)
//
// The name keeps its original case; the value is trimmed.
func parseVarLine(line string) (name, op, value string, ok bool) {
	s := strings.TrimSpace(line)
	for _, o := range []string{"?=", ":="} {
		if i := strings.Index(s, o); i > 0 {
			return strings.TrimSpace(s[:i]), o, strings.TrimSpace(s[i+len(o):]), true
		}
	}
	if i := strings.IndexAny(s, "=:"); i > 0 {
		return strings.TrimSpace(s[:i]), "=", strings.TrimSpace(s[i+1:]), true
	}
	return "", "", "", false
}

// clone returns a deep-enough copy of a task for matrix expansion: scalar fields
// copied, slices duplicated so a per-combination Env append cannot alias the
// original. Matrix is intentionally left nil on the copy (the clone is concrete).
func (t *Task) clone() *Task {
	c := *t
	c.Matrix = nil
	c.Requires = append([]string(nil), t.Requires...)
	c.Inputs = append([]string(nil), t.Inputs...)
	c.Sources = append([]string(nil), t.Sources...)
	c.Generates = append([]string(nil), t.Generates...)
	c.Env = append([]string(nil), t.Env...)
	c.Secrets = append([]string(nil), t.Secrets...)
	c.Artifacts = append([]string(nil), t.Artifacts...)
	c.Require = append([]string(nil), t.Require...)
	c.Ensure = append([]string(nil), t.Ensure...)
	c.Effects = append([]string(nil), t.Effects...)
	if t.ExitCodes != nil {
		c.ExitCodes = make(map[int]string, len(t.ExitCodes))
		for k, v := range t.ExitCodes {
			c.ExitCodes[k] = v
		}
	}
	return &c
}

func parseExitCodes(v string) map[int]string {
	out := map[int]string{}
	for _, f := range strings.Fields(v) {
		codeS, action, ok := strings.Cut(f, "=")
		if !ok {
			continue
		}
		code, err := strconv.Atoi(strings.TrimSpace(codeS))
		if err != nil {
			continue
		}
		action = strings.ToLower(strings.TrimSpace(action))
		switch action {
		case "ok", "skip", "retry", "fail":
			out[code] = action
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
