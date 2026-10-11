package meet

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/service"
)

func TestMeetServiceLifecycleIsForegroundServe(t *testing.T) {
	cmd := NewMeetCmd()
	serve, _, err := cmd.Find([]string{"serve"})
	if err != nil {
		t.Fatal(err)
	}
	if serve == nil {
		t.Fatal("meet serve command missing")
	}
	for _, flag := range []string{"port", "bind"} {
		if serve.Flags().Lookup(flag) == nil {
			t.Errorf("meet serve lost --%s", flag)
		}
	}
	for _, args := range [][]string{{"service", "start"}, {"service", "status"}, {"service", "stop"}} {
		c := NewMeetCmd()
		c.SetArgs(args)
		if err := c.Execute(); err == nil {
			t.Errorf("legacy detached command %v still works", args)
		}
	}
}

func runMeet(t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmd := NewMeetCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), err
}

// The events feed is governed by the monitor-managed unit: served until a
// monitor says otherwise, refused while the unit is off, served again when
// it is applied.
func TestEventsFeedFollowsUnitState(t *testing.T) {
	t.Cleanup(func() { _ = meetEvents.Deactivate(context.Background()); meetEvents.state.Store(0) })
	h := newServeHandler(context.Background(), MountOptions{})
	get := func() int {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		req := httptest.NewRequest("GET", "/api/events", nil).WithContext(ctx)
		req.RemoteAddr = "127.0.0.1:1"
		rec := httptest.NewRecorder()
		done := make(chan struct{})
		go func() { defer close(done); h.ServeHTTP(rec, req) }()
		time.Sleep(50 * time.Millisecond)
		cancel()
		<-done
		return rec.Code
	}
	if got := get(); got != 200 {
		t.Fatalf("unmanaged feed = %d", got)
	}
	_ = meetEvents.Deactivate(context.Background())
	if got := get(); got != 404 {
		t.Fatalf("off feed = %d", got)
	}
	_ = meetEvents.Activate(context.Background(), service.Config{})
	if got := get(); got != 200 {
		t.Fatalf("on feed = %d", got)
	}
}

// The meet owner registers the events capability and applies the monitor's
// desired state to it over the control socket.
func TestMeetOwnerAppliesEventsUnit(t *testing.T) {
	t.Cleanup(func() { meetEvents.state.Store(0) })
	sock := filepath.Join(shortTemp(t), "s.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	var mu sync.Mutex
	enabled := true
	var reports []service.Status
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				var req struct {
					Op     string         `json:"op"`
					Status service.Status `json:"status"`
				}
				if json.NewDecoder(c).Decode(&req) != nil {
					return
				}
				mu.Lock()
				defer mu.Unlock()
				resp := map[string]any{"ok": true}
				if req.Op == "desired" {
					resp["units"] = []service.Status{{Unit: "events", Enabled: enabled}}
				} else {
					reports = append(reports, req.Status)
				}
				_ = json.NewEncoder(c).Encode(resp)
			}()
		}
	}()
	o, err := newServiceOwner(nil)
	if err != nil {
		t.Fatal(err)
	}
	o.Client = service.Client{Socket: sock}
	o.Resolve = func(string) (fleet.Service, error) {
		return fleet.Service{Name: "events", Provider: fleet.ServiceProviderInternal, Capability: EventsCapabilityID, Owner: "meet"}, nil
	}
	if err := o.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !meetEvents.serving() || meetEvents.state.Load() != 1 {
		t.Fatal("enabled events unit not applied")
	}
	mu.Lock()
	enabled = false
	mu.Unlock()
	if err := o.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if meetEvents.serving() {
		t.Fatal("disabled events unit still serving")
	}
	mu.Lock()
	defer mu.Unlock()
	if n := len(reports); n < 2 || reports[0].State != service.Active || reports[n-1].State != service.Inactive {
		t.Fatalf("reports = %+v", reports)
	}
}

func shortTemp(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("", "mo")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	return d
}
