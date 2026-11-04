package browsercmd

import (
	"bytes"
	"context"
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/qiangli/coreutils/tool"
)

// `browser --live-port N hub` must listen on N. The top-level parser takes
// GNU-interspersed flags, so `hub --port N` never reached the hub's own flag
// set and the hub always bound 58082 (S563 F18).
func TestHubListensOnLivePort(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var out, errb bytes.Buffer
	rc := &tool.RunContext{Ctx: ctx, Stdio: tool.Stdio{Out: &out, Err: &errb}}
	done := make(chan int, 1)
	go func() { done <- browserHub(rc, ctx, nil, port, false) }()

	addr := "127.0.0.1:" + strconv.Itoa(port)
	deadline := time.Now().Add(5 * time.Second)
	for {
		c, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			c.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("hub not listening on %s: %v\nstdout: %s\nstderr: %s", addr, err, out.String(), errb.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
	cancel()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("hub exit = %d, stderr: %s", code, errb.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("hub did not stop on cancel")
	}
}
