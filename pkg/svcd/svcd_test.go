// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package svcd

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

func testSpec(t *testing.T, port int) Spec {
	t.Helper()
	dir := t.TempDir()
	return Spec{
		Name: "test", Schema: "test-v1", Health: "test-health",
		Dir:         func() (string, error) { return dir, nil },
		Argv:        []string{"true"},
		DefaultPort: port,
	}
}

// A free port must read as NOT RUNNING, and the words matter: outpost restarts
// on "stopped" / "not running", so a stopped daemon that failed to say so would
// never be restarted.
func TestStatusOfFreePortIsNotRunning(t *testing.T) {
	s := testSpec(t, freePort(t))
	st, err := s.StatusOf(Options{})
	if err != nil {
		t.Fatalf("StatusOf: %v", err)
	}
	if st.Running || st.State != StateNotRunning {
		t.Fatalf("status = %+v, want not running", st)
	}
}

// A stale pidfile is SUCCESS for stop. The state the caller asked for holds;
// erroring would make every stop after a crash look broken and loop the
// supervisor forever.
func TestStopWithStalePidfileSucceeds(t *testing.T) {
	s := testSpec(t, freePort(t))
	p, _ := s.pidPath()
	// A pid that cannot be alive: 0 is never a real process, and writing a
	// plausible-but-dead pid would make the test depend on the host's pid space.
	if err := writePid(p, 999999); err != nil {
		t.Fatal(err)
	}
	st, err := s.Stop(Options{})
	if err != nil {
		t.Fatalf("stop over a stale pidfile errored: %v", err)
	}
	if st.State != StateStaleRemoved {
		t.Fatalf("state = %q, want %q", st.State, StateStaleRemoved)
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Fatal("the stale pidfile was not removed")
	}
}

// An occupied port that is NOT ours must be reported, never signalled. This is
// the property that stops a service from killing an unrelated process that
// happens to hold its port.
func TestUnidentifiedListenerIsRefusedNotKilled(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go http.Serve(ln, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"ok":true,"service":"something-else"}`)
	}))
	_, portStr, _ := net.SplitHostPort(ln.Addr().String())
	port, _ := strconv.Atoi(portStr)

	s := testSpec(t, port)
	st, err := s.StatusOf(Options{})
	if err != ErrUnidentified {
		t.Fatalf("StatusOf over a foreign listener = %v, want ErrUnidentified", err)
	}
	if st.State != StateUnidentified {
		t.Fatalf("state = %q, want %q", st.State, StateUnidentified)
	}
	if _, err := s.Stop(Options{}); err != ErrUnidentified {
		t.Fatalf("Stop over a foreign listener = %v, want a refusal", err)
	}
	// It is still listening: stop reported rather than acted.
	if _, err := net.Dial("tcp", ln.Addr().String()); err != nil {
		t.Fatal("the foreign listener was taken down; stop must never signal what it cannot identify")
	}
}

// Our own daemon answering /healthz is recognised even with no visible pidfile,
// so a service started under a different store still stops cleanly.
func TestOurHealthEndpointIdentifiesTheDaemon(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go http.Serve(ln, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"ok":true,"service":"test-health"}`)
	}))
	_, portStr, _ := net.SplitHostPort(ln.Addr().String())
	port, _ := strconv.Atoi(portStr)

	st, err := testSpec(t, port).StatusOf(Options{})
	if err != nil || !st.Running {
		t.Fatalf("status = %+v err=%v, want running (identified by /healthz)", st, err)
	}
}

func TestPidAndLogLiveUnderTheServiceDir(t *testing.T) {
	s := testSpec(t, freePort(t))
	dir, _ := s.Dir()
	p, _ := s.pidPath()
	l, _ := s.logPath()
	if filepath.Dir(p) != dir || filepath.Dir(l) != dir {
		t.Fatalf("pid=%q log=%q, want both under %q — relocating the store must relocate the daemon", p, l, dir)
	}
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	_, p, _ := net.SplitHostPort(ln.Addr().String())
	n, _ := strconv.Atoi(p)
	return n
}
