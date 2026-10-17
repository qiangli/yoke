package broker

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/broker/door"
)

func TestProbeDoorDownIsFast(t *testing.T) {
	t.Setenv("BASHY_HOME", t.TempDir())
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	t.Setenv(door.PortEnv, strconv.Itoa(port))

	start := time.Now()
	st, err := door.Probe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("probe took %s", time.Since(start))
	}
	if st.Up || !strings.Contains(st.Error, "connection refused") || st.SchemaVersion != door.StatusSchemaVersion {
		t.Fatalf("down status wrong: %+v", st)
	}

	cmd := newStatusCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"--json"})
	if err := cmd.ExecuteContext(context.Background()); err != ErrDoorDown {
		t.Fatalf("want ErrDoorDown, got %v", err)
	}
	var got door.Status
	if json.Unmarshal(out.Bytes(), &got) != nil || got.Up {
		t.Fatalf("bad json: %s", out.String())
	}
}

func TestProbeDoorUp(t *testing.T) {
	home := t.TempDir()
	t.Setenv("BASHY_HOME", home)
	token, err := door.Token()
	if err != nil {
		t.Fatal(err)
	}
	cli := &fakeCLI{}
	b, err := New(context.Background(), Options{Engine: StaticEngine(newFakeEngine(t).server.URL), CLI: cli, Token: token, MemoryBytes: 64 << 30, StateDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { b.Close(context.Background()) })
	srv := httptest.NewUnstartedServer(b)
	srv.Listener.Close()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv.Listener = ln
	srv.Start()
	t.Cleanup(srv.Close)
	t.Setenv(door.PortEnv, strconv.Itoa(ln.Addr().(*net.TCPAddr).Port))

	st, err := door.Probe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !st.Up || st.PID == 0 || st.Error != "" {
		t.Fatalf("up status wrong: %+v", st)
	}
	if len(st.Backends) != 2 || st.Backends[0].Name != "ollama-local" || !st.Backends[0].Present || st.Backends[1].Name != "cligw" || !st.Backends[1].Present {
		t.Fatalf("backends: %+v", st.Backends)
	}
	if st.AgentEntries != 1 {
		t.Fatalf("agent entries: %+v", st)
	}

	cmd := newStatusCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	if err := cmd.ExecuteContext(context.Background()); err != nil || !strings.Contains(out.String(), "model door up") {
		t.Fatalf("err=%v out=%s", err, out.String())
	}
}
