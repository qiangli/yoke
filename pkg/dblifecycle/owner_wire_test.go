package dblifecycle

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/dbstore"
)

// TestOwnerWireCap: a request over the old 64 KiB read bound succeeds, and one
// over MaxWireBytes is refused client-side with limit_exceeded before the
// owner runs anything.
func TestOwnerWireCap(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BASHY_DB_SESSION_DIR", filepath.Join(dir, "sessions"))
	t.Setenv("BASHY_COORD_DIR", filepath.Join(dir, "coord"))
	var runs atomic.Int64
	RegisterCategoryOp("test.size", CategoryOp{
		Access: func(json.RawMessage) (Access, error) { return AccessRead, nil },
		Spec: func(dbstore.Mode, Access) Operation {
			return Operation{Name: "test.size", MutationClass: "read", Effects: []string{}}
		},
		Run: func(_ context.Context, _ dbstore.Handle, input json.RawMessage, _ Access) (Outcome, error) {
			runs.Add(1)
			return Outcome{Result: len(input)}, nil
		},
	})
	t.Cleanup(func() { categoryOps.Delete("test.size") })
	ready := filepath.Join(dir, "ready")
	serveCtx, stopServe := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- Serve(serveCtx, ready, 0) }()
	t.Cleanup(func() { stopServe(); <-served })
	var id string
	for deadline := time.Now().Add(5 * time.Second); id == "" && time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if b, err := os.ReadFile(ready); err == nil && strings.HasPrefix(string(b), "s-") {
			id = string(b)
		}
	}
	if id == "" {
		t.Fatal("owner did not become ready")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	target := dbstore.Target{Engine: "sqlite", Mode: dbstore.Memory, Session: id, Name: "wire"}
	if r, err := Call(ctx, id, Request{Op: "create", Target: target}); err != nil || !r.OK {
		t.Fatalf("create: %+v %v", r, err)
	}
	quoted := func(n int) json.RawMessage { return json.RawMessage(`"` + strings.Repeat("a", n) + `"`) }
	big, err := Call(ctx, id, Request{Op: "test.size", Target: target, Input: quoted(200 << 10)})
	if err != nil || !big.OK || runs.Load() != 1 {
		t.Fatalf(">64KiB request: %+v %v runs=%d", big, err, runs.Load())
	}
	_, err = Call(ctx, id, Request{Op: "test.size", Target: target, Input: quoted(MaxWireBytes)})
	if !errors.Is(err, dbstore.ErrLimitExceeded) || runs.Load() != 1 {
		t.Fatalf("oversized request: err=%v runs=%d, want limit_exceeded and no run", err, runs.Load())
	}
}
