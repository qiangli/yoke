// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package dag

import (
	"sort"
	"strings"
)

// expandVars resolves the document's frontmatter `vars:` against the process
// env and CLI KEY=VALUE overrides, then substitutes ${NAME} in every target's
// metadata (Requires/Inputs/Sources/Generates/Env/Ensure + Host/When) before
// the graph is built.
// Precedence, highest first: CLI overrides > `vars:` (`=`/`:=`) > process env >
// `vars:` (`?=` default-if-unset). Target bodies are left untouched — the shell
// expands ${VAR} there at run time; this pass is metadata-only.
//
// env and overrides are os.Environ()-shaped ("KEY=VALUE"). It is a no-op when
// the document declares no vars (the common case), so existing pipelines that
// never use ${NAME} are unaffected.
// Returns the resolved values of the document's own `vars:` (name→value), so the
// caller can also inject them into target bodies' environment — frontmatter vars
// are available to bodies, not just metadata. CLI overrides are NOT included
// (the engine already passes those through os.Environ()+overrides).
func (d *Document) expandVars(env, overrides []string) map[string]string {
	vals := envMap(env)

	// Apply vars in declaration order. A value may itself reference earlier
	// vars / env via ${NAME} (resolved against the map built so far).
	for _, v := range d.Vars {
		val := substVars(v.Value, vals)
		switch v.Op {
		case "?=":
			if _, ok := vals[v.Name]; !ok {
				vals[v.Name] = val
			}
		default: // "=", ":=" — set, overriding the process env default.
			vals[v.Name] = val
		}
	}

	// CLI KEY=VALUE always wins.
	for k, v := range envMap(overrides) {
		vals[k] = v
	}

	// Resolved doc vars to hand back for body-env injection (declared vars only).
	resolved := make(map[string]string, len(d.Vars))
	for _, v := range d.Vars {
		resolved[v.Name] = vals[v.Name]
	}

	if len(d.Vars) == 0 && len(overrides) == 0 {
		return resolved // nothing could change in metadata
	}
	for _, t := range d.Tasks {
		substSlice(t.Requires, vals)
		substSlice(t.Inputs, vals)
		substSlice(t.Sources, vals)
		substSlice(t.Generates, vals)
		substSlice(t.Env, vals)
		substSlice(t.Require, vals)
		substSlice(t.Ensure, vals)
		substSlice(t.Tools, vals)
		t.Host = substVars(t.Host, vals) // placement (e.g. Host: ${HOST})
		t.When = substVars(t.When, vals) // condition (e.g. When: test -n "${HOST}")
	}
	return resolved
}

// expandMatrix replaces every target that declares a Matrix with one concrete
// node per combination (`<target>:<k>=<v>,...`, keys in sorted order for
// determinism), injecting each combination's key=value into that node's Env.
// The original name survives as an aggregator that Requires all of its
// expansions, so `dag <target>` runs every combination; the original's upstream
// Requires move onto the concrete nodes. Dependents keep pointing at the
// original name — now the aggregator — so no rewiring of other targets is
// needed. A no-op when no target declares a Matrix.
func (d *Document) expandMatrix() {
	hasMatrix := false
	for _, t := range d.Tasks {
		if len(t.Matrix) > 0 {
			hasMatrix = true
			break
		}
	}
	if !hasMatrix {
		return
	}

	tasks := make([]*Task, 0, len(d.Tasks))
	order := make([]string, 0, len(d.Order))
	byName := make(map[string]*Task, len(d.byName))
	add := func(t *Task) {
		tasks = append(tasks, t)
		order = append(order, t.Name)
		byName[t.Name] = t
	}

	for _, t := range d.Tasks {
		if len(t.Matrix) == 0 {
			add(t)
			continue
		}
		var expNames []string
		for _, combo := range matrixCombos(t.Matrix) {
			child := t.clone()
			child.Name = t.Name + ":" + comboSuffix(combo)
			for _, kv := range combo {
				child.Env = append(child.Env, kv.key+"="+kv.value)
			}
			add(child)
			expNames = append(expNames, child.Name)
		}
		// The original name becomes a phony aggregator depending on every
		// expansion. It carries no body, Ensure, or Effects — those moved to the
		// concrete nodes.
		add(&Task{
			Name:     t.Name,
			Desc:     t.Desc,
			Line:     t.Line,
			Requires: expNames,
		})
	}

	d.Tasks, d.Order, d.byName = tasks, order, byName
}

// matrixKV is one resolved (key,value) for a matrix combination.
type matrixKV struct{ key, value string }

// matrixCombos returns the cartesian product of a matrix, keys in sorted order
// so the expansion is deterministic.
func matrixCombos(m map[string][]string) [][]matrixKV {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	combos := [][]matrixKV{{}}
	for _, k := range keys {
		var next [][]matrixKV
		for _, base := range combos {
			for _, v := range m[k] {
				row := append(append([]matrixKV(nil), base...), matrixKV{k, v})
				next = append(next, row)
			}
		}
		combos = next
	}
	return combos
}

// comboSuffix renders a combination as `k1=v1,k2=v2` for the node name.
func comboSuffix(combo []matrixKV) string {
	parts := make([]string, len(combo))
	for i, kv := range combo {
		parts[i] = kv.key + "=" + kv.value
	}
	return strings.Join(parts, ",")
}

// envMap turns an os.Environ()-shaped slice into a name→value map (last wins).
func envMap(env []string) map[string]string {
	m := make(map[string]string, len(env))
	for _, e := range env {
		if i := strings.IndexByte(e, '='); i > 0 {
			m[e[:i]] = e[i+1:]
		}
	}
	return m
}

// substSlice substitutes ${NAME} in-place across a metadata slice.
func substSlice(ss []string, vals map[string]string) {
	for i, s := range ss {
		ss[i] = substVars(s, vals)
	}
}

// substVars replaces every ${NAME} in s with vals[NAME] (an undefined name
// expands to empty, matching shell parameter expansion). A `${` with no closing
// `}` is left verbatim.
func substVars(s string, vals map[string]string) string {
	if !strings.Contains(s, "${") {
		return s
	}
	var b strings.Builder
	for {
		i := strings.Index(s, "${")
		if i < 0 {
			b.WriteString(s)
			break
		}
		b.WriteString(s[:i])
		j := strings.IndexByte(s[i+2:], '}')
		if j < 0 {
			b.WriteString(s[i:]) // unterminated — leave as-is
			break
		}
		name := s[i+2 : i+2+j]
		b.WriteString(vals[name])
		s = s[i+2+j+1:]
	}
	return b.String()
}
