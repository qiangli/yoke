// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package dag

import (
	"bytes"
	"context"
	"strings"
	"testing"

	_ "github.com/qiangli/coreutils/cmds/all"
)

func TestSecretsInjectedAndRedacted(t *testing.T) {
	dir := t.TempDir()
	md := "## Tasks\n\n### use\nSecrets: TOKEN\n" +
		block("bash", "echo \"got=${TOKEN}\"")
	g, err := BuildGraph(doc(t, md))
	if err != nil {
		t.Fatalf("BuildGraph: %v", err)
	}
	eng := &Engine{
		Graph: g, Dir: dir,
		Env:         []string{"TOKEN=s3cr3t"},
		Concurrency: 1, FailFast: true, Capture: true,
		Stdout: new(bytes.Buffer), Stderr: new(bytes.Buffer),
	}
	report, err := eng.Run(context.Background(), "use")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	r := report.Results[0]
	if r.Status != StatusDone {
		t.Fatalf("want done, got %s (%v)", r.Status, r.Err)
	}
	// The secret was injected ($TOKEN expanded) but its VALUE is redacted.
	if strings.Contains(r.Stdout, "s3cr3t") {
		t.Errorf("secret value leaked into captured stdout: %q", r.Stdout)
	}
	if !strings.Contains(r.Stdout, "got=***") {
		t.Errorf("want redacted got=***, stdout=%q", r.Stdout)
	}
}

func TestSecretsRedactedInPlainStream(t *testing.T) {
	// Even when the engine is not capturing (plain serial run), a secret target
	// is captured-then-redacted so the value never reaches the real stdout.
	dir := t.TempDir()
	md := "## Tasks\n\n### use\nSecrets: TOKEN\n" +
		block("bash", "echo leak=${TOKEN}")
	g, err := BuildGraph(doc(t, md))
	if err != nil {
		t.Fatalf("BuildGraph: %v", err)
	}
	out := new(bytes.Buffer)
	eng := &Engine{
		Graph: g, Dir: dir, Env: []string{"TOKEN=hunter2"},
		Concurrency: 1, FailFast: true,
		Stdout: out, Stderr: new(bytes.Buffer),
	}
	if _, err := eng.Run(context.Background(), "use"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if strings.Contains(out.String(), "hunter2") {
		t.Errorf("secret leaked to plain stdout: %q", out.String())
	}
	if !strings.Contains(out.String(), "leak=***") {
		t.Errorf("want leak=*** in plain stream, got %q", out.String())
	}
}
