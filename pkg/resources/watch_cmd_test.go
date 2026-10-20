package resources

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/qiangli/yoke/pkg/fleet"
	_ "github.com/qiangli/yoke/pkg/fleet/fleetkinds"
)

func watchRoots(t *testing.T) {
	t.Helper()
	t.Setenv("BASHY_FLEET_DIR", t.TempDir())
	t.Setenv("BASHY_COORD_DIR", t.TempDir())
	t.Setenv("BASHY_RESOURCES_DIR", t.TempDir())
	t.Setenv("BASHY_MB_DIR", t.TempDir())
}

// --- auto-registration --------------------------------------------------------

func TestAutoRegisterIdempotent(t *testing.T) {
	watchRoots(t)
	cat := fleet.New()
	sys := fakeSystem()
	added, err := AutoRegisterHost(cat, sys)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"host-a.example", "host-a.example.cpu", "host-a.example.mem", "host-a.example.disk-data", "host-a.example.net-en0"}
	if strings.Join(added, ",") != strings.Join(want, ",") {
		t.Fatalf("first pass added %v, want %v", added, want)
	}
	added2, err := AutoRegisterHost(cat, sys)
	if err != nil {
		t.Fatal(err)
	}
	if len(added2) != 0 {
		t.Fatalf("second pass added %v, want nothing", added2)
	}
	all, errs := cat.Resources()
	if len(errs) > 0 {
		t.Fatalf("registry errors: %v", errs)
	}
	if len(all) != len(want) {
		t.Fatalf("registry holds %d records, want %d", len(all), len(want))
	}
	for _, name := range want {
		rec, ok := cat.Resource(name)
		if !ok {
			t.Fatalf("record %s missing", name)
		}
		if rec.Kind != "name" {
			t.Errorf("%s kind = %q, want name", name, rec.Kind)
		}
		if !strings.Contains(rec.Notes, "resource watch") {
			t.Errorf("%s notes = %q, want the auto-register note", name, rec.Notes)
		}
	}
}

func TestAutoRegisterAddsNewDevicesOnly(t *testing.T) {
	watchRoots(t)
	cat := fleet.New()
	first := fakeSystem()
	if _, err := AutoRegisterHost(cat, first); err != nil {
		t.Fatal(err)
	}
	// A disk appears (an external volume mounts): exactly it is added.
	grown := fakeSystem()
	grown.Disks = append(grown.Disks, fakeDisk("/mnt/usb", 40, 100*gib))
	added, err := AutoRegisterHost(cat, grown)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(added, ",") != "host-a.example.disk-mnt-usb" {
		t.Fatalf("grown pass added %v", added)
	}
}

func TestResolveWatchConfigsLayers(t *testing.T) {
	watchRoots(t)
	cat := fleet.New()
	if _, err := AutoRegisterHost(cat, fakeSystem()); err != nil {
		t.Fatal(err)
	}
	host, ok := cat.Resource("host-a.example")
	if !ok {
		t.Fatal("host record missing")
	}
	host.Watch = map[string]string{"disk-free-warn": "100G", "notify-to": "operator-seat-a"}
	if err := cat.SaveResource(host); err != nil {
		t.Fatal(err)
	}
	disk, ok := cat.Resource("host-a.example.disk-data")
	if !ok {
		t.Fatal("disk record missing")
	}
	disk.Watch = map[string]string{"disk-warn-used": "80"}
	if err := cat.SaveResource(disk); err != nil {
		t.Fatal(err)
	}
	// A foreign resource (another host's) must not configure this watcher.
	foreign := fleet.Resource{Name: "host-b.example.disk-data", Kind: "name",
		Members: []string{"/data"}, Watch: map[string]string{"disk-warn-used": "1"}}
	if err := cat.SaveResource(foreign); err != nil {
		t.Fatal(err)
	}

	base, byName := ResolveWatchConfigs(cat, "host-a.example")
	if base.DiskFreeWarnBytes != 100*gib {
		t.Fatalf("host floor override lost: %d", base.DiskFreeWarnBytes)
	}
	if base.NotifyTo != "operator-seat-a" {
		t.Fatalf("host notify-to lost: %q", base.NotifyTo)
	}
	diskCfg, ok := byName["host-a.example.disk-data"]
	if !ok {
		t.Fatal("per-resource config missing")
	}
	if diskCfg.DiskWarnUsed != 80 {
		t.Fatalf("resource override lost: %v", diskCfg.DiskWarnUsed)
	}
	if diskCfg.DiskFreeWarnBytes != 100*gib {
		t.Fatalf("resource config must inherit the host floor: %d", diskCfg.DiskFreeWarnBytes)
	}
	if _, ok := byName["host-b.example.disk-data"]; ok {
		t.Fatal("foreign resource leaked into this host's watcher config")
	}
}

func TestResourceSetWatchFlags(t *testing.T) {
	watchRoots(t)
	if out, err := run(t, "add", "host-a.example", "--kind", "name", "--watch", "disk-free-warn=100G"); err != nil {
		t.Fatalf("add: %v\n%s", err, out)
	}
	if out, err := run(t, "show", "host-a.example"); err != nil || !strings.Contains(out, "watch:   disk-free-warn=100G") {
		t.Fatalf("show missing watch map:\n%s (%v)", out, err)
	}
	if out, err := run(t, "set", "host-a.example", "--watch", "cpu-warn=90", "--watch", "renotify=15m"); err != nil {
		t.Fatalf("set --watch: %v\n%s", err, out)
	}
	if out, err := run(t, "show", "host-a.example"); err != nil ||
		!strings.Contains(out, "cpu-warn=90") || !strings.Contains(out, "renotify=15m") || !strings.Contains(out, "disk-free-warn=100G") {
		t.Fatalf("set --watch did not accumulate:\n%s (%v)", out, err)
	}
	if _, err := run(t, "set", "host-a.example", "--rm-watch", "cpu-warn"); err != nil {
		t.Fatalf("set --rm-watch: %v", err)
	}
	if out, err := run(t, "show", "host-a.example"); err != nil || strings.Contains(out, "cpu-warn") {
		t.Fatalf("--rm-watch did not drop the key:\n%s (%v)", out, err)
	}
	if _, err := run(t, "set", "host-a.example", "--watch", "bogus=1"); err == nil {
		t.Fatal("set --watch accepted an unknown key")
	}
	if _, err := run(t, "set", "host-a.example", "--watch", "disk-free-warn=12X"); err == nil {
		t.Fatal("set --watch accepted a malformed value")
	}
	// The YAML round-trip must carry the map.
	if out, err := run(t, "show", "host-a.example", "--yaml"); err != nil || !strings.Contains(out, "disk-free-warn: 100G") {
		t.Fatalf("yaml missing watch map:\n%s (%v)", out, err)
	}
}

// --- dry-run fixture: fake disk through warning, critical and clear ----------

func TestWatchDryRunFixture(t *testing.T) {
	watchRoots(t)

	var mu sync.Mutex
	stage := 0.70
	prevSampler := WatchSampler
	WatchSampler = func(context.Context) (*System, error) {
		mu.Lock()
		defer mu.Unlock()
		sys := fakeSystem()
		sys.Disks = []Disk{fakeDisk("/data", stage*100, 500*gib)}
		return sys, nil
	}
	t.Cleanup(func() { WatchSampler = prevSampler })

	var sent []Event
	var errBuf, outBuf bytes.Buffer
	runOnce := func() error {
		errBuf.Reset()
		outBuf.Reset()
		return RunWatch(context.Background(), WatchOptions{
			Once: true, JSON: true, Register: true,
			Notify: func(ev Event) error {
				sent = append(sent, ev)
				return nil
			},
		}, &errBuf, &outBuf)
	}

	// 86% for three samples: one warning line, no inbox message yet? The
	// fixture contract: the warning transition is delivered too (one
	// message per transition).
	stage = 0.86
	for range 3 {
		if err := runOnce(); err != nil {
			t.Fatal(err)
		}
	}
	if !strings.Contains(errBuf.String(), "level=warning") {
		t.Fatalf("86%% sample: no warning line:\n%s", errBuf.String())
	}
	if len(sent) != 1 || sent[0].Level != LevelWarning {
		t.Fatalf("86%% sample: notifier saw %d events", len(sent))
	}

	// 96%: a critical line plus one inbox message for it (the tight free
	// space — 20 GiB on the floor — alarms too: whichever trips first).
	stage = 0.96
	for range 3 {
		if err := runOnce(); err != nil {
			t.Fatal(err)
		}
	}
	if !strings.Contains(errBuf.String(), "level=critical") {
		t.Fatalf("96%% sample: no critical line:\n%s", errBuf.String())
	}
	crits := 0
	for _, ev := range sent[1:] {
		if ev.Level == LevelCritical {
			crits++
		}
	}
	if len(sent) != 3 || crits != 1 {
		t.Fatalf("96%% sample: notifier saw %d events (%d critical), want the critical one", len(sent), crits)
	}

	// NDJSON: every emitted line parses and carries the contract fields.
	var ev Event
	var found bool
	for _, line := range strings.Split(strings.TrimSpace(ndjsonOf(t, errBuf, outBuf)), "\n") {
		if !strings.Contains(line, `"disk-used"`) {
			continue
		}
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("stdout is not NDJSON events: %v\n%s", err, outBuf.String())
		}
		found = true
		break
	}
	if !found {
		t.Fatalf("no disk-used event in NDJSON:\n%s", outBuf.String())
	}
	if ev.SchemaVersion != WatchSchemaVersion || ev.Type != "alarm" || ev.Host != "host-a.example" ||
		ev.Resource != "host-a.example.disk-data" || ev.Metric != "disk-used" ||
		ev.Level != LevelCritical || ev.Unit != "percent" || ev.Threshold != 95 || ev.At.IsZero() {
		t.Fatalf("event envelope wrong: %+v", ev)
	}

	// Back to 70%: a clear line for every metric that alarmed.
	stage = 0.70
	for range 3 {
		if err := runOnce(); err != nil {
			t.Fatal(err)
		}
	}
	if !strings.Contains(errBuf.String(), "CLEAR host=host-a.example") {
		t.Fatalf("70%% sample: no clear line:\n%s", errBuf.String())
	}
	if len(sent) != 5 {
		t.Fatalf("clear stage: notifier saw %d events, want both clears", len(sent))
	}
}

func ndjsonOf(t *testing.T, errBuf, outBuf bytes.Buffer) string {
	t.Helper()
	if strings.TrimSpace(outBuf.String()) == "" {
		t.Fatalf("expected NDJSON on stdout, got none (stderr:\n%s)", errBuf.String())
	}
	return outBuf.String()
}

// --- the cobra front door ------------------------------------------------------

func TestWatchCommandOnceRuns(t *testing.T) {
	watchRoots(t)
	prevSampler := WatchSampler
	WatchSampler = func(context.Context) (*System, error) { return fakeSystem(), nil }
	t.Cleanup(func() { WatchSampler = prevSampler })
	out, err := run(t, "watch", "--once")
	if err != nil {
		t.Fatalf("resource watch --once: %v\n%s", err, out)
	}
	if !strings.Contains(out, "registered host-a.example") {
		t.Fatalf("first run must auto-register:\n%s", out)
	}
	// Second run registers nothing new and exits clean.
	out2, err := run(t, "watch", "--once")
	if err != nil {
		t.Fatalf("second watch --once: %v\n%s", err, out2)
	}
	if strings.Contains(out2, "registered ") {
		t.Fatalf("second run re-registered:\n%s", out2)
	}
}

func TestWatchCommandRejectsBadInterval(t *testing.T) {
	watchRoots(t)
	if _, err := run(t, "watch", "--interval", "0"); err == nil {
		t.Fatal("continuous watch with zero interval must be a usage error")
	}
}
