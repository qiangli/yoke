package broker

import (
	"context"
	"net"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/broker/door"
)

func TestEnsureUpFailsFastWhenChildExits(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses sh")
	}
	t.Setenv("BASHY_HOME", t.TempDir())
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	t.Setenv(door.PortEnv, strconv.Itoa(port))
	old := SelfArgv
	SelfArgv = func() []string { return []string{"sh", "-c", "echo 'engine runner missing' >&2; exit 3"} }
	t.Cleanup(func() { SelfArgv = old })

	start := time.Now()
	err = EnsureUp(context.Background())
	if err == nil || !strings.Contains(err.Error(), "engine runner missing") || !strings.Contains(err.Error(), "exit status 3") {
		t.Fatalf("want the child's message and status, got %v", err)
	}
	if time.Since(start) > 10*time.Second {
		t.Fatalf("took %s, should fail at once", time.Since(start))
	}
}
