package foreman

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type recordingRunner struct {
	mu      sync.Mutex
	prompts []string
	seen    chan string
}

func (r *recordingRunner) Run(ctx context.Context, agent string, args []string, cwd string) (string, int, error) {
	prompt := ""
	if len(args) > 0 {
		prompt = args[len(args)-1]
	}
	r.mu.Lock()
	r.prompts = append(r.prompts, prompt)
	r.mu.Unlock()
	select {
	case r.seen <- prompt:
	default:
	}
	return "ack", 0, nil
}

func (r *recordingRunner) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.prompts...)
}

func TestRunDAGAppliesSteerBetweenNodes(t *testing.T) {
	root := t.TempDir()
	dagPath := filepath.Join(root, "dag.md")
	if err := os.WriteFile(dagPath, []byte("## Tasks\n\n### a\n```bash\necho a\n```\n\n### b\nRequires: a\n```bash\necho b\n```\n"), 0o600); err != nil {
		t.Fatalf("write dag: %v", err)
	}
	r := &recordingRunner{seen: make(chan string, 8)}
	s, err := Start(context.Background(), Options{ID: "dag", Goal: "run dag", Agent: "stub", Root: root, Runner: r})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready := make(chan string, 1)
	errc := make(chan error, 1)
	go func() { errc <- s.ServeControl(ctx, ready) }()
	select {
	case <-ready:
	case err := <-errc:
		t.Fatalf("ServeControl: %v", err)
	case <-time.After(3 * time.Second):
		t.Fatal("control socket did not become ready")
	}

	done := make(chan error, 1)
	go func() {
		_, err := s.RunDAG(ctx, DAGOptions{Path: dagPath, SteerPause: 300 * time.Millisecond})
		done <- err
	}()
	select {
	case p := <-r.seen:
		if !strings.Contains(p, "DAG target: a") {
			t.Fatalf("first prompt = %q, want target a", p)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("target a did not run")
	}
	if _, err := Tell(root, "dag", "steer between nodes"); err != nil {
		t.Fatalf("Tell: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("RunDAG: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("RunDAG did not finish")
	}
	got := r.snapshot()
	if len(got) != 3 {
		t.Fatalf("prompts len = %d, want 3: %#v", len(got), got)
	}
	if !strings.Contains(got[0], "DAG target: a") {
		t.Fatalf("prompt[0] = %q, want target a", got[0])
	}
	if !strings.Contains(got[1], "steer between nodes") {
		t.Fatalf("prompt[1] = %q, want steer", got[1])
	}
	if !strings.Contains(got[2], "DAG target: b") || !strings.Contains(got[2], "steer between nodes") {
		t.Fatalf("prompt[2] = %q, want target b with steer history", got[2])
	}
	cancel()
}
