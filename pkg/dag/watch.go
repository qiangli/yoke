// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package dag

import (
	"context"
	"io"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const watchInterval = time.Second

func watchFingerprints(g *Graph, dir string) map[string]string {
	out := make(map[string]string, len(g.Nodes))
	for _, name := range g.Order {
		n := g.Nodes[name]
		paths := append(append([]string{}, n.Task.Sources...), n.Task.Inputs...)
		sort.Strings(paths)
		var b strings.Builder
		for _, p := range paths {
			b.WriteString(p)
			b.WriteByte(0)
			b.WriteString(hashPath(filepath.Join(dir, p)))
			b.WriteByte(0)
		}
		out[name] = b.String()
	}
	return out
}

func changedTargets(prev, cur map[string]string) []string {
	var out []string
	seen := map[string]bool{}
	for name, v := range cur {
		if prev[name] != v {
			out = append(out, name)
			seen[name] = true
		}
	}
	for name := range prev {
		if !seen[name] {
			if _, ok := cur[name]; !ok {
				out = append(out, name)
			}
		}
	}
	sort.Strings(out)
	return out
}

func affectedTargets(g *Graph, changed []string) []string {
	seen := map[string]bool{}
	var walk func(*Node)
	walk = func(n *Node) {
		if n == nil || seen[n.Task.Name] {
			return
		}
		seen[n.Task.Name] = true
		for _, dep := range n.Dependents {
			walk(dep)
		}
	}
	for _, name := range changed {
		walk(g.Nodes[name])
	}
	var out []string
	for _, name := range g.Order {
		if seen[name] {
			out = append(out, name)
		}
	}
	return out
}

func runWatch(ctx context.Context, out io.Writer, eng *Engine, targets []string) error {
	prev := watchFingerprints(eng.Graph, eng.Dir)
	ticker := time.NewTicker(watchInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			cur := watchFingerprints(eng.Graph, eng.Dir)
			changed := changedTargets(prev, cur)
			prev = cur
			if len(changed) == 0 {
				continue
			}
			affected := affectedTargets(eng.Graph, changed)
			if len(affected) == 0 {
				affected = targets
			}
			if _, err := io.WriteString(out, "dag: change detected; re-running affected targets\n"); err != nil {
				return err
			}
			if _, err := eng.Run(ctx, affected...); err != nil {
				return err
			}
		}
	}
}
