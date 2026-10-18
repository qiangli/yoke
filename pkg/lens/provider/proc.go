package provider

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/qiangli/yoke/pkg/ref"
	"github.com/tklauser/ps"
)

var _ Provider = (*Proc)(nil)

// Proc exposes the host process table as a read-only hierarchical tree based on PPID.
type Proc struct {
	Processes func() ([]ps.Process, error)
}

// NewProc returns a new read-only Proc provider backed by tklauser/ps.
func NewProc() *Proc {
	return &Proc{Processes: ps.Processes}
}

func (p *Proc) fetch() ([]ps.Process, error) {
	if p.Processes != nil {
		return p.Processes()
	}
	return ps.Processes()
}

func (p *Proc) parseLocator(loc ref.Locator) (targetPID int, isRoot bool, err error) {
	if loc.Scheme != "proc" || len(loc.Inner) != 0 || loc.ScopeKind != "" {
		return 0, false, ErrUnsupported
	}
	t := strings.TrimPrefix(loc.Target, "/")
	if t == "" || t == "." {
		return 0, true, nil
	}
	pid, err := strconv.Atoi(t)
	if err != nil || pid < 0 {
		return 0, false, fmt.Errorf("provider: invalid process id %q", loc.Target)
	}
	return pid, false, nil
}

func (p *Proc) rootNode(loc ref.Locator, count uint64) Node {
	n := Node{
		SchemaVersion: NodeSchema,
		Ref:           loc.String(),
		Path:          "",
		Name:          "proc",
		Kind:          "directory",
		Type:          "proc/tree",
		Children:      &count,
		Attrs:         map[string]any{},
	}
	return n
}

func (p *Proc) processNode(pr ps.Process, childCount uint64) Node {
	pid := pr.PID()
	pidStr := strconv.Itoa(pid)
	name := pr.Command()
	if name == "" {
		if exe := pr.ExecutablePath(); exe != "" {
			name = filepath.Base(exe)
		}
		if name == "" {
			name = pidStr
		}
	}

	attrs := map[string]any{
		"pid":  pid,
		"ppid": pr.PPID(),
	}
	if uid := pr.UID(); uid >= 0 {
		attrs["uid"] = uid
	}
	if gid := pr.GID(); gid >= 0 {
		attrs["gid"] = gid
	}
	if cmd := pr.Command(); cmd != "" {
		attrs["command"] = cmd
	}
	if exe := pr.ExecutablePath(); exe != "" {
		attrs["executable"] = exe
	}
	if args := pr.ExecutableArgs(); len(args) > 0 {
		attrs["args"] = args
	}

	n := Node{
		SchemaVersion: NodeSchema,
		Ref:           "proc:" + pidStr,
		Path:          pidStr,
		Name:          name,
		Kind:          "process",
		Type:          "process",
		Attrs:         attrs,
	}

	if t := pr.CreationTime(); !t.IsZero() {
		n.Mtime = t.UTC().Format(time.RFC3339Nano)
	}

	if childCount > 0 {
		n.Children = &childCount
	}

	return n
}

// Stat returns metadata for either the root process container (proc:) or a specific process (proc:<pid>).
func (p *Proc) Stat(ctx context.Context, loc ref.Locator) (Node, error) {
	if err := ctx.Err(); err != nil {
		return Node{}, err
	}
	pid, isRoot, err := p.parseLocator(loc)
	if err != nil {
		return Node{}, err
	}
	processes, err := p.fetch()
	if err != nil {
		return Node{}, err
	}

	byPID := make(map[int]ps.Process, len(processes))
	childrenCount := make(map[int]uint64)
	var rootCount uint64
	for _, pr := range processes {
		byPID[pr.PID()] = pr
		childrenCount[pr.PPID()]++
	}
	for _, pr := range processes {
		ppid := pr.PPID()
		if ppid <= 0 || byPID[ppid] == nil {
			rootCount++
		}
	}

	if isRoot {
		return p.rootNode(loc, rootCount), nil
	}

	pr, ok := byPID[pid]
	if !ok {
		return Node{}, fmt.Errorf("provider: process %d: %w", pid, os.ErrNotExist)
	}

	return p.processNode(pr, childrenCount[pid]), nil
}

// List returns child processes of either the root (proc:) or a specific process (proc:<pid>).
func (p *Proc) List(ctx context.Context, loc ref.Locator) ([]Node, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	pid, isRoot, err := p.parseLocator(loc)
	if err != nil {
		return nil, err
	}
	processes, err := p.fetch()
	if err != nil {
		return nil, err
	}

	byPID := make(map[int]ps.Process, len(processes))
	childrenByPPID := make(map[int][]ps.Process)
	for _, pr := range processes {
		byPID[pr.PID()] = pr
		childrenByPPID[pr.PPID()] = append(childrenByPPID[pr.PPID()], pr)
	}

	var procs []ps.Process
	if isRoot {
		for _, pr := range processes {
			ppid := pr.PPID()
			if ppid <= 0 || byPID[ppid] == nil {
				procs = append(procs, pr)
			}
		}
	} else {
		if _, ok := byPID[pid]; !ok {
			return nil, fmt.Errorf("provider: process %d: %w", pid, os.ErrNotExist)
		}
		procs = childrenByPPID[pid]
	}

	sort.Slice(procs, func(i, j int) bool {
		return procs[i].PID() < procs[j].PID()
	})

	nodes := make([]Node, 0, len(procs))
	for _, pr := range procs {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		childCount := uint64(len(childrenByPPID[pr.PID()]))
		nodes = append(nodes, p.processNode(pr, childCount))
	}
	return nodes, nil
}

// Edges returns contains edges from loc to each of its direct children.
func (p *Proc) Edges(ctx context.Context, loc ref.Locator) ([]Edge, error) {
	children, err := p.List(ctx, loc)
	if err != nil {
		return nil, err
	}
	edges := make([]Edge, 0, len(children))
	for _, child := range children {
		edges = append(edges, Edge{
			SchemaVersion: EdgeSchema,
			Src:           loc.String(),
			Rel:           "contains",
			Dst:           child.Ref,
			Attrs:         map[string]any{},
		})
	}
	return edges, nil
}

// Open is unsupported for process trees.
func (p *Proc) Open(ctx context.Context, loc ref.Locator) (io.ReadCloser, error) {
	return nil, ErrUnsupported
}
