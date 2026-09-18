// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package dag

import (
	"strings"

	"github.com/qiangli/coreutils/pkg/weavecli"
)

// Status is a node's lifecycle state.
type Status int

const (
	StatusPending Status = iota
	StatusRunning
	StatusDone
	StatusFailed
	StatusSkipped          // a dependency did not succeed
	StatusUpToDate         //
	StatusConditionSkipped // P1 #10 — a `When:` condition was false (NOT a failure)
)

func (s Status) String() string {
	switch s {
	case StatusPending:
		return "pending"
	case StatusRunning:
		return "running"
	case StatusDone:
		return "done"
	case StatusFailed:
		return "failed"
	case StatusSkipped:
		return "skipped"
	case StatusUpToDate:
		return "up-to-date"
	case StatusConditionSkipped:
		return "condition-skipped"
	default:
		return "unknown"
	}
}

// Node is one target in the dependency graph.
type Node struct {
	Task       *Task
	Deps       []*Node // resolved out-edges (this target's Requires)
	Dependents []*Node // in-edges (targets that require this one)

	Status Status
	Result *TaskResult
}

// Graph is the full set of targets with resolved edges.
type Graph struct {
	Nodes map[string]*Node
	Order []string // declaration order, for deterministic scheduling/listing
}

// BuildGraph resolves Requires into edges. Unknown deps and self-deps are
// rejected with ExitInvalidArg.
func BuildGraph(d *Document) (*Graph, error) {
	g := &Graph{
		Nodes: make(map[string]*Node, len(d.Tasks)),
		Order: append([]string(nil), d.Order...),
	}
	for _, t := range d.Tasks {
		for _, ef := range t.Effects {
			if !knownEffects[ef] {
				return nil, errf(weavecli.ExitInvalidArg,
					"target %q declares unknown effect %q (want read/write/net/spend/destroy/time)", t.Name, ef)
			}
		}
		g.Nodes[t.Name] = &Node{Task: t, Status: StatusPending}
	}
	for _, t := range d.Tasks {
		n := g.Nodes[t.Name]
		seen := map[string]bool{}
		for _, dep := range t.Requires {
			if dep == t.Name {
				return nil, errf(weavecli.ExitInvalidArg, "target %q requires itself", t.Name)
			}
			dn, ok := g.Nodes[dep]
			if !ok {
				return nil, unknownTargetError(dep, g.Order,
					"target %q requires unknown target %q", t.Name, dep)
			}
			if seen[dep] {
				continue
			}
			seen[dep] = true
			n.Deps = append(n.Deps, dn)
			dn.Dependents = append(dn.Dependents, n)
		}
	}
	return g, nil
}

// Subgraph returns the transitive-dependency closure of targets, sharing the
// same *Node pointers (so status updates are visible on the parent graph) with
// Order filtered to the included set in declaration order.
func (g *Graph) Subgraph(targets ...string) (*Graph, error) {
	include := map[string]bool{}
	var stack []string
	for _, t := range targets {
		if _, ok := g.Nodes[t]; !ok {
			return nil, unknownTargetError(t, g.Order, "unknown target %q", t)
		}
		stack = append(stack, t)
	}
	for len(stack) > 0 {
		name := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if include[name] {
			continue
		}
		include[name] = true
		for _, d := range g.Nodes[name].Deps {
			stack = append(stack, d.Task.Name)
		}
	}
	sub := &Graph{Nodes: make(map[string]*Node, len(include))}
	for _, name := range g.Order {
		if include[name] {
			sub.Nodes[name] = g.Nodes[name]
			sub.Order = append(sub.Order, name)
		}
	}
	return sub, nil
}

func unknownTargetError(name string, known []string, format string, a ...any) *Error {
	msg := errf(weavecli.ExitInvalidArg, format, a...).Msg
	if suggestion := nearestTarget(name, known); suggestion != "" {
		msg += " (did you mean " + strconvQuote(suggestion) + "?)"
	}
	return &Error{Code: weavecli.ExitInvalidArg, Msg: msg}
}

func nearestTarget(name string, known []string) string {
	best := ""
	bestDist := 3
	for _, candidate := range known {
		d := editDistance(name, candidate)
		if d < bestDist {
			best = candidate
			bestDist = d
		}
	}
	return best
}

func editDistance(a, b string) int {
	ar, br := []rune(a), []rune(b)
	prev := make([]int, len(br)+1)
	for j := range prev {
		prev[j] = j
	}
	for i, ca := range ar {
		cur := make([]int, len(br)+1)
		cur[0] = i + 1
		for j, cb := range br {
			cost := 0
			if ca != cb {
				cost = 1
			}
			cur[j+1] = min3(cur[j]+1, prev[j+1]+1, prev[j]+cost)
		}
		prev = cur
	}
	return prev[len(br)]
}

func min3(a, b, c int) int {
	if b < a {
		a = b
	}
	if c < a {
		a = c
	}
	return a
}

func strconvQuote(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `\"`) + `"`
}

// TopoSort returns a topological ordering (Kahn's algorithm) with ties broken
// by declaration order for determinism. A cycle yields ExitStateConflict with
// the offending path in the message.
func (g *Graph) TopoSort() ([]*Node, error) {
	indeg := make(map[string]int, len(g.Nodes))
	for name, n := range g.Nodes {
		for _, d := range n.Deps {
			if _, ok := g.Nodes[d.Task.Name]; ok {
				indeg[name]++
			}
		}
	}
	var ready []*Node
	for _, name := range g.Order {
		if indeg[name] == 0 {
			ready = append(ready, g.Nodes[name])
		}
	}
	orderIndex := make(map[string]int, len(g.Order))
	for i, name := range g.Order {
		orderIndex[name] = i
	}

	var out []*Node
	for len(ready) > 0 {
		// pop the ready node with the smallest declaration index (determinism)
		min := 0
		for i := 1; i < len(ready); i++ {
			if orderIndex[ready[i].Task.Name] < orderIndex[ready[min].Task.Name] {
				min = i
			}
		}
		n := ready[min]
		ready = append(ready[:min], ready[min+1:]...)
		out = append(out, n)
		for _, dep := range n.Dependents {
			dn, ok := g.Nodes[dep.Task.Name]
			if !ok {
				continue
			}
			indeg[dn.Task.Name]--
			if indeg[dn.Task.Name] == 0 {
				ready = append(ready, dn)
			}
		}
	}
	if len(out) != len(g.Nodes) {
		cyc, _ := g.DetectCycle()
		return nil, errf(weavecli.ExitStateConflict,
			"dependency cycle: %s", strings.Join(cyc, " -> "))
	}
	return out, nil
}

// DetectCycle returns a cycle path (DFS three-color) and whether one exists.
func (g *Graph) DetectCycle() ([]string, bool) {
	const (
		white = 0
		gray  = 1
		black = 2
	)
	color := make(map[string]int, len(g.Nodes))
	var path, cycle []string
	var dfs func(name string) bool
	dfs = func(name string) bool {
		color[name] = gray
		path = append(path, name)
		for _, d := range g.Nodes[name].Deps {
			dn := d.Task.Name
			if _, ok := g.Nodes[dn]; !ok {
				continue
			}
			switch color[dn] {
			case gray:
				start := 0
				for i, p := range path {
					if p == dn {
						start = i
						break
					}
				}
				cycle = append(append([]string{}, path[start:]...), dn)
				return true
			case white:
				if dfs(dn) {
					return true
				}
			}
		}
		color[name] = black
		path = path[:len(path)-1]
		return false
	}
	for _, name := range g.Order {
		if color[name] == white {
			if dfs(name) {
				return cycle, true
			}
		}
	}
	return nil, false
}
