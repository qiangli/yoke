package resourcescmd

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/qiangli/coreutils/tool"
	"github.com/qiangli/yoke/pkg/resources"
)

func TestResourcesFleetCommand(t *testing.T) {
	rc := &tool.RunContext{
		Ctx: context.Background(),
	}
	var outBuf, errBuf bytes.Buffer
	rc.Out = &outBuf
	rc.Err = &errBuf

	// Test text table mode
	code := run(rc, []string{"fleet"})
	if code != 0 {
		t.Fatalf("resources fleet exit code = %d, err = %s", code, errBuf.String())
	}
	outStr := outBuf.String()
	if !strings.Contains(outStr, "PROVIDER") || !strings.Contains(outStr, "BAND") {
		t.Errorf("resources fleet output missing headers:\n%s", outStr)
	}

	// Test --json mode
	outBuf.Reset()
	errBuf.Reset()
	code = run(rc, []string{"fleet", "--json"})
	if code != 0 {
		t.Fatalf("resources fleet --json exit code = %d, err = %s", code, errBuf.String())
	}
	jsonStr := outBuf.String()
	var env resources.FleetResources
	if err := json.Unmarshal([]byte(jsonStr), &env); err != nil {
		t.Fatalf("resources fleet --json parse error: %v\nOutput: %s", err, jsonStr)
	}
	if env.SchemaVersion != resources.SchemaVersion {
		t.Errorf("SchemaVersion = %q, want %q", env.SchemaVersion, resources.SchemaVersion)
	}
}

func TestResourcesSystemCommand(t *testing.T) {
	rc := &tool.RunContext{
		Ctx: context.Background(),
	}
	var outBuf, errBuf bytes.Buffer
	rc.Out = &outBuf
	rc.Err = &errBuf

	// Test basic resources system
	code := run(rc, []string{"system", "--interval", "0"})
	if code != 0 {
		t.Fatalf("resources system exit code = %d, err = %s", code, errBuf.String())
	}
	if !strings.Contains(outBuf.String(), "DISK") {
		t.Errorf("resources system output missing DISK header:\n%s", outBuf.String())
	}

	// Test resources system --min-free impossible floor fails with actionable alarm
	outBuf.Reset()
	errBuf.Reset()
	code = run(rc, []string{"system", "--interval", "0", "--min-free", "10000000G"})
	if code == 0 {
		t.Fatal("expected non-zero exit code for impossible 10000000G floor")
	}
	if !strings.Contains(errBuf.String(), "disk floor alarm:") {
		t.Fatalf("expected stderr to contain 'disk floor alarm:', got: %q", errBuf.String())
	}
}

func TestResourcesWatchOnce(t *testing.T) {
	t.Setenv("BASHY_FLEET_DIR", t.TempDir())
	t.Setenv("BASHY_COORD_DIR", t.TempDir())
	t.Setenv("BASHY_RESOURCES_DIR", t.TempDir())
	t.Setenv("BASHY_MB_DIR", t.TempDir())

	prev := resources.WatchSampler
	resources.WatchSampler = func(context.Context) (*resources.System, error) {
		return &resources.System{
			SchemaVersion: resources.SchemaVersion,
			Host:          "host-a.example",
			OS:            "darwin",
			CPU:           resources.CPU{UsagePercent: 3, Source: "ticks"},
			Memory:        resources.Memory{TotalBytes: 64 << 30, UsedBytes: 19 << 30, UsedPercent: 30},
			Disks: []resources.Disk{{
				Mount: "/data", TotalBytes: 500 << 30, UsedBytes: 430 << 30,
				FreeBytes: 70 << 30, UsedPercent: 86,
			}},
			Network: []resources.Interface{{Name: "en0", RxBytes: 1, TxBytes: 1, Up: true}},
		}, nil
	}
	t.Cleanup(func() { resources.WatchSampler = prev })

	rc := &tool.RunContext{Ctx: context.Background()}
	var outBuf, errBuf bytes.Buffer
	rc.Out = &outBuf
	rc.Err = &errBuf

	code := run(rc, []string{"watch", "--once"})
	if code != 0 {
		t.Fatalf("resources watch --once exit = %d, err = %s", code, errBuf.String())
	}
	if !strings.Contains(errBuf.String(), "registered host-a.example") {
		t.Errorf("watch --once must auto-register host resources:\n%s", errBuf.String())
	}
	// Second run is quiet: idempotent registration, no duplicate alarm.
	errBuf.Reset()
	code = run(rc, []string{"watch", "--once"})
	if code != 0 {
		t.Fatalf("second watch --once exit = %d, err = %s", code, errBuf.String())
	}
	if strings.Contains(errBuf.String(), "registered ") {
		t.Errorf("second run re-registered:\n%s", errBuf.String())
	}
}

func TestResourcesWatchRejectsZeroInterval(t *testing.T) {
	rc := &tool.RunContext{Ctx: context.Background()}
	var outBuf, errBuf bytes.Buffer
	rc.Out = &outBuf
	rc.Err = &errBuf
	if code := run(rc, []string{"watch", "--interval", "0"}); code == 0 {
		t.Fatal("watch with zero interval and no --once must fail")
	}
}
