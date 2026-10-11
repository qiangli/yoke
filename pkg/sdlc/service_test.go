package sdlc

import (
	"bytes"
	"context"
	"sync/atomic"
	"testing"
	"time"
)

func TestServeLoopRunsTicksUntilCancelled(t *testing.T) {
	opt := ServiceOptions{RunsDir: t.TempDir() + "/runs", Interval: 5 * time.Millisecond}
	var ticks int32
	ctx, cancel := context.WithCancel(context.Background())
	tick := func(ctx context.Context) (DelegateResult, error) {
		if atomic.AddInt32(&ticks, 1) >= 3 {
			cancel()
		}
		return DelegateResult{Status: "idle"}, nil
	}
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, opt, tick) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Serve did not stop after context cancel")
	}
	if atomic.LoadInt32(&ticks) < 3 {
		t.Fatalf("expected >=3 ticks, got %d", ticks)
	}
}

func TestSDLCServiceCommandIsForegroundServe(t *testing.T) {
	cmd := NewSDLCCmd()
	serve, _, err := cmd.Find([]string{"serve"})
	if err != nil {
		t.Fatal(err)
	}
	if serve == nil {
		t.Fatal("sdlc serve command missing")
	}
	for _, name := range []string{"config", "runs-dir", "interval", "cwd", "sandbox", "timeout"} {
		if serve.Flags().Lookup(name) == nil {
			t.Errorf("sdlc serve lost --%s", name)
		}
	}
	for _, args := range [][]string{{"service", "start"}, {"service", "status"}, {"service", "stop"}} {
		c := NewSDLCCmd()
		c.SetArgs(args)
		var out bytes.Buffer
		c.SetOut(&out)
		if err := c.Execute(); err == nil {
			t.Errorf("legacy detached command %v still works", args)
		}
	}
}
