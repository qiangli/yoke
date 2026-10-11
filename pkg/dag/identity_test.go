// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package dag_test

import (
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/dag"
)

func TestTask_IdentityAndCapabilityFields(t *testing.T) {
	src := `## build
Ensure: test -f bin/app
Effects: write
` + "```sh\n" + `go build -o bin/app ./...
` + "```\n" + `
## test
Requires: build
Ensure: test -f test.log
` + "```sh\n" + `go test ./... > test.log
` + "```\n"

	doc, err := dag.Parse(strings.NewReader(src), "dag.md")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	buildTask, ok := doc.Lookup("build")
	if !ok {
		t.Fatal("build not found")
	}
	testTask, ok := doc.Lookup("test")
	if !ok {
		t.Fatal("test not found")
	}

	// Verify Identity field is set on parsed Task
	if !strings.HasPrefix(buildTask.Identity, "h") || len(buildTask.Identity) != 65 {
		t.Fatalf("buildTask.Identity invalid: %q", buildTask.Identity)
	}
	if !strings.HasPrefix(testTask.Identity, "h") || len(testTask.Identity) != 65 {
		t.Fatalf("testTask.Identity invalid: %q", testTask.Identity)
	}
	if buildTask.Identity == testTask.Identity {
		t.Fatal("build and test tasks should have different identities")
	}

	// Verify Capability field is set on parsed Task
	if !strings.HasPrefix(buildTask.Capability, "k") || len(buildTask.Capability) != 65 {
		t.Fatalf("buildTask.Capability invalid: %q", buildTask.Capability)
	}
	if !strings.HasPrefix(testTask.Capability, "k") || len(testTask.Capability) != 65 {
		t.Fatalf("testTask.Capability invalid: %q", testTask.Capability)
	}

	// Verify methods on Task
	if buildTask.TargetIdentity() != buildTask.Identity {
		t.Errorf("TargetIdentity() = %q, want %q", buildTask.TargetIdentity(), buildTask.Identity)
	}
	if buildTask.TargetCapability() != buildTask.Capability {
		t.Errorf("TargetCapability() = %q, want %q", buildTask.TargetCapability(), buildTask.Capability)
	}
}

func TestTask_OneBodyByteChangesOnlyThatTask(t *testing.T) {
	srcA := `## t1
` + "```sh\n" + `echo 1
` + "```\n" + `
## t2
` + "```sh\n" + `echo 2
` + "```\n"

	srcB := `## t1
` + "```sh\n" + `echo 9
` + "```\n" + `
## t2
` + "```sh\n" + `echo 2
` + "```\n"

	docA, err := dag.Parse(strings.NewReader(srcA), "dag.md")
	if err != nil {
		t.Fatalf("Parse A: %v", err)
	}
	docB, err := dag.Parse(strings.NewReader(srcB), "dag.md")
	if err != nil {
		t.Fatalf("Parse B: %v", err)
	}

	t1A, _ := docA.Lookup("t1")
	t2A, _ := docA.Lookup("t2")

	t1B, _ := docB.Lookup("t1")
	t2B, _ := docB.Lookup("t2")

	if t1A.Identity == t1B.Identity {
		t.Fatalf("t1 identity should have changed: %q", t1A.Identity)
	}
	if t2A.Identity != t2B.Identity {
		t.Fatalf("t2 identity should NOT have changed: %q != %q", t2A.Identity, t2B.Identity)
	}
}
