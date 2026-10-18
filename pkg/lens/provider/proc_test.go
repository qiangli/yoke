package provider

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/ref"
	"github.com/tklauser/ps"
)

type mockProc struct {
	pid  int
	ppid int
	uid  int
	gid  int
	cmd  string
	exe  string
	args []string
	born time.Time
}

func (m mockProc) PID() int                 { return m.pid }
func (m mockProc) PPID() int                { return m.ppid }
func (m mockProc) UID() int                 { return m.uid }
func (m mockProc) GID() int                 { return m.gid }
func (m mockProc) Command() string          { return m.cmd }
func (m mockProc) ExecutablePath() string   { return m.exe }
func (m mockProc) ExecutableArgs() []string { return m.args }
func (m mockProc) CreationTime() time.Time  { return m.born }
func (m mockProc) Refresh() error           { return nil }

func newTestProcProvider() *Proc {
	t0 := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	procs := []ps.Process{
		mockProc{pid: 1, ppid: 0, uid: 0, gid: 0, cmd: "init", exe: "/sbin/init", args: []string{"/sbin/init"}, born: t0},
		mockProc{pid: 10, ppid: 1, uid: 1000, gid: 1000, cmd: "bash", exe: "/bin/bash", args: []string{"bash", "-l"}, born: t0},
		mockProc{pid: 20, ppid: 1, uid: 1000, gid: 1000, cmd: "server", exe: "/usr/bin/server", args: []string{"server", "--port=8080"}, born: t0},
		mockProc{pid: 30, ppid: 10, uid: 1000, gid: 1000, cmd: "grep", exe: "/usr/bin/grep", args: []string{"grep", "pattern"}, born: t0},
		mockProc{pid: 99, ppid: 9999, uid: 0, gid: 0, cmd: "orphan", exe: "/bin/orphan", born: t0}, // PPID not in process list -> root
	}
	return &Proc{
		Processes: func() ([]ps.Process, error) {
			return procs, nil
		},
	}
}

func TestProcTreeHierarchy(t *testing.T) {
	ctx := context.Background()
	p := newTestProcProvider()

	// 1. Stat root container
	rootLoc := ref.Locator{Scheme: "proc", Target: ""}
	rootNode, err := p.Stat(ctx, rootLoc)
	if err != nil {
		t.Fatalf("Stat root: %v", err)
	}
	if rootNode.SchemaVersion != NodeSchema || rootNode.Ref != "proc:" || rootNode.Kind != "directory" {
		t.Fatalf("unexpected rootNode: %+v", rootNode)
	}
	if rootNode.Children == nil || *rootNode.Children != 2 { // PID 1 and orphan PID 99
		t.Fatalf("root children count = %v, want 2", rootNode.Children)
	}

	// 2. List root -> returns roots (PID 1, PID 99) sorted by PID
	roots, err := p.List(ctx, rootLoc)
	if err != nil {
		t.Fatalf("List root: %v", err)
	}
	if len(roots) != 2 {
		t.Fatalf("len(roots) = %d, want 2", len(roots))
	}
	if roots[0].Ref != "proc:1" || roots[0].Name != "init" {
		t.Errorf("roots[0] = %+v, want proc:1 init", roots[0])
	}
	if roots[0].Children == nil || *roots[0].Children != 2 {
		t.Errorf("roots[0].Children = %v, want 2 (PIDs 10 and 20)", roots[0].Children)
	}
	if roots[1].Ref != "proc:99" || roots[1].Name != "orphan" {
		t.Errorf("roots[1] = %+v, want proc:99 orphan", roots[1])
	}
	if roots[1].Children != nil {
		t.Errorf("roots[1].Children = %v, want nil", roots[1].Children)
	}

	// 3. List children of PID 1
	p1Loc := ref.Locator{Scheme: "proc", Target: "1"}
	p1Kids, err := p.List(ctx, p1Loc)
	if err != nil {
		t.Fatalf("List proc:1: %v", err)
	}
	if len(p1Kids) != 2 {
		t.Fatalf("len(p1Kids) = %d, want 2", len(p1Kids))
	}
	if p1Kids[0].Ref != "proc:10" || p1Kids[1].Ref != "proc:20" {
		t.Fatalf("unexpected p1 kids: %+v", p1Kids)
	}

	// 4. Stat on PID 10 (has child 30)
	p10Loc := ref.Locator{Scheme: "proc", Target: "10"}
	p10Node, err := p.Stat(ctx, p10Loc)
	if err != nil {
		t.Fatalf("Stat proc:10: %v", err)
	}
	if p10Node.Kind != "process" || p10Node.Type != "process" {
		t.Errorf("p10Node kind/type = %s/%s", p10Node.Kind, p10Node.Type)
	}
	if p10Node.Attrs["command"] != "bash" || p10Node.Attrs["pid"] != 10 || p10Node.Attrs["ppid"] != 1 {
		t.Errorf("p10Node attrs = %+v", p10Node.Attrs)
	}
	if p10Node.Children == nil || *p10Node.Children != 1 {
		t.Errorf("p10Node children = %v, want 1", p10Node.Children)
	}

	// 5. Stat on leaf process PID 30
	p30Loc := ref.Locator{Scheme: "proc", Target: "30"}
	p30Node, err := p.Stat(ctx, p30Loc)
	if err != nil {
		t.Fatalf("Stat proc:30: %v", err)
	}
	if p30Node.Children != nil {
		t.Errorf("leaf proc:30 has children %v, want nil", p30Node.Children)
	}

	// 6. Edges from proc:1
	edges, err := p.Edges(ctx, p1Loc)
	if err != nil {
		t.Fatalf("Edges proc:1: %v", err)
	}
	if len(edges) != 2 {
		t.Fatalf("len(edges) = %d, want 2", len(edges))
	}
	if edges[0].SchemaVersion != EdgeSchema || edges[0].Src != "proc:1" || edges[0].Rel != "contains" || edges[0].Dst != "proc:10" {
		t.Errorf("edges[0] = %+v", edges[0])
	}
	if edges[1].Dst != "proc:20" {
		t.Errorf("edges[1] = %+v", edges[1])
	}

	// 7. Open returns ErrUnsupported
	_, err = p.Open(ctx, p1Loc)
	if !errors.Is(err, ErrUnsupported) {
		t.Errorf("Open err = %v, want ErrUnsupported", err)
	}
}

func TestProcErrorsAndUnsupported(t *testing.T) {
	ctx := context.Background()
	p := newTestProcProvider()

	// Non-existent PID
	missing := ref.Locator{Scheme: "proc", Target: "8888"}
	if _, err := p.Stat(ctx, missing); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("Stat missing = %v, want os.ErrNotExist", err)
	}
	if _, err := p.List(ctx, missing); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("List missing = %v, want os.ErrNotExist", err)
	}

	// Invalid target format
	invalid := ref.Locator{Scheme: "proc", Target: "not-a-number"}
	if _, err := p.Stat(ctx, invalid); err == nil {
		t.Error("Stat invalid should error")
	}

	// Unsupported scheme
	wrongScheme := ref.Locator{Scheme: "dir", Target: "1"}
	if _, err := p.Stat(ctx, wrongScheme); !errors.Is(err, ErrUnsupported) {
		t.Errorf("Stat wrong scheme = %v, want ErrUnsupported", err)
	}

	// Unsupported inner descent
	inner := ref.Locator{Scheme: "proc", Target: "1", Inner: []string{"sub"}}
	if _, err := p.Stat(ctx, inner); !errors.Is(err, ErrUnsupported) {
		t.Errorf("Stat inner = %v, want ErrUnsupported", err)
	}
}

func TestProcRegistryAndSerialization(t *testing.T) {
	r := NewRegistry()
	p := newTestProcProvider()
	if err := r.Register("proc", p); err != nil {
		t.Fatalf("Register proc: %v", err)
	}

	lookup, ok := r.Lookup("proc")
	if !ok || lookup == nil {
		t.Fatal("Lookup proc failed")
	}

	loc, err := r.Parse("proc:1")
	if err != nil {
		t.Fatalf("Parse proc:1: %v", err)
	}

	node, err := lookup.Stat(context.Background(), loc)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}

	b, err := json.Marshal(node)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	if m["schema_version"] != NodeSchema || m["ref"] != "proc:1" || m["attrs"] == nil {
		t.Fatalf("serialized row invalid: %s", b)
	}
}

func TestProcLiveHost(t *testing.T) {
	p := NewProc()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	rootLoc := ref.Locator{Scheme: "proc", Target: ""}
	roots, err := p.List(ctx, rootLoc)
	if err != nil {
		t.Fatalf("Live host List proc: %v", err)
	}
	if len(roots) == 0 {
		t.Fatal("Live host returned 0 root processes")
	}

	statNode, err := p.Stat(ctx, rootLoc)
	if err != nil {
		t.Fatalf("Live host Stat proc: %v", err)
	}
	if statNode.Children == nil || *statNode.Children != uint64(len(roots)) {
		t.Errorf("statNode.Children = %v, len(roots) = %d", statNode.Children, len(roots))
	}
}
