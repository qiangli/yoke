// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package unit_test

import (
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/dag"
	"github.com/qiangli/yoke/pkg/unit"
)

func TestUnit_PackageExports(t *testing.T) {
	doc, err := dag.Parse(strings.NewReader("## build\nEnsure: ok\n```sh\necho hi\n```\n"), "dag.md")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	task, _ := doc.Lookup("build")

	id := unit.DagTargetIdentity(task)
	if !strings.HasPrefix(id, "h") {
		t.Errorf("DagTargetIdentity = %q, want 'h' prefix", id)
	}

	cap := unit.DagTargetCapability(task)
	if !strings.HasPrefix(cap, "k") {
		t.Errorf("DagTargetCapability = %q, want 'k' prefix", cap)
	}

	capKey, err := unit.DagTargetCapabilityKey(task)
	if err != nil || capKey != cap {
		t.Errorf("DagTargetCapabilityKey error = %v, key = %q", err, capKey)
	}

	script := []byte("# name: test\n# description: desc\n# type: bsh\necho 1\n")
	sid := unit.ScriptIdentity(script)
	if !strings.HasPrefix(sid, "h") {
		t.Errorf("ScriptIdentity = %q, want 'h' prefix", sid)
	}

	hdr := unit.ScriptHeader(script)
	if hdr.Name != "test" || hdr.Description != "desc" || hdr.Type != "bsh" {
		t.Errorf("ScriptHeader = %+v", hdr)
	}
}
