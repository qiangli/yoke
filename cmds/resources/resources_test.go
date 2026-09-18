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
