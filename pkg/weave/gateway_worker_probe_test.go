package weave

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/fleet"
)

// A delegate to a worker whose subscription check fails reports the honest
// state and never launches the worker.
func TestGatewayDelegateProbesWorkerBeforeLaunch(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "launched")
	bin := filepath.Join(dir, "codex")
	script := "#!/bin/sh\nif [ \"$1 $2\" = \"login status\" ]; then echo 'Not logged in'; exit 1; fi\ntouch " + marker + "\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	cat := fleet.New(fleet.WithRoot(t.TempDir()))
	tool := fleet.Tool{Name: "codex", Kind: "cli", CLI: fleet.ToolCLI{Binary: bin}}
	tool.CLI.Launch.Exec = "{binary} exec {prompt}"
	tool.CLI.Launch.Auth.CheckExec = "login status"
	if err := cat.SaveTool(tool); err != nil {
		t.Fatal(err)
	}
	prev, prevTimeout := fleetCatalog, gatewayWorkerProbeTimeout
	fleetCatalog = func() *fleet.Catalog { return cat }
	gatewayWorkerProbeTimeout = 5 * time.Second
	t.Cleanup(func() { fleetCatalog, gatewayWorkerProbeTimeout = prev, prevTimeout })

	var d gatewayDirective
	if err := json.Unmarshal([]byte(`{"verb":"delegate","host":"h","task_id":"t","idempotency_key":"k","args":{"worker":"codex","prompt":"do it"}}`), &d); err != nil {
		t.Fatal(err)
	}
	text, exit, verified := executeGatewayDirective(context.Background(), d, dir)
	if exit == 0 || !verified || !strings.Contains(text, "credential state missing") {
		t.Fatalf("text %q exit %d verified %v", text, exit, verified)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("worker launched despite a failed subscription check")
	}
}
