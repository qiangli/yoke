// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package identity_test

import (
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/dag"
	"github.com/qiangli/yoke/pkg/unit/identity"
)

func TestDagTargetIdentity_Stable(t *testing.T) {
	src := `## build
Requires: prepare
Sources: src/*.go

This description is ignored for identity.

` + "```sh\n" + `go build -o bin/app ./...
` + "```\n"

	doc1, err := dag.Parse(strings.NewReader(src), "dag.md")
	if err != nil {
		t.Fatalf("Parse doc1: %v", err)
	}
	task1, ok := doc1.Lookup("build")
	if !ok {
		t.Fatal("target build not found")
	}

	id1 := identity.DagTargetIdentity(task1)
	if !strings.HasPrefix(id1, "h") || len(id1) != 65 {
		t.Fatalf("expected 65-char hex with 'h' prefix, got %q", id1)
	}

	// Repeated call is stable
	id1Repeat := identity.DagTargetIdentity(task1)
	if id1 != id1Repeat {
		t.Fatalf("id mismatch across calls: %q != %q", id1, id1Repeat)
	}

	// Separate parse of identical source produces identical identity
	doc2, err := dag.Parse(strings.NewReader(src), "dag.md")
	if err != nil {
		t.Fatalf("Parse doc2: %v", err)
	}
	task2, _ := doc2.Lookup("build")
	id2 := identity.DagTargetIdentity(task2)
	if id1 != id2 {
		t.Fatalf("expected stable identity %q, got %q", id1, id2)
	}

	// Also verify task2.Identity field was populated during Parse
	if task2.Identity != id1 {
		t.Fatalf("expected task2.Identity %q, got %q", id1, task2.Identity)
	}
}

func TestDagTargetIdentity_BodyByteChange(t *testing.T) {
	src1 := `## compile
Requires: prep
` + "```sh\n" + `go build ./...
` + "```\n" + `
## test
Requires: compile
` + "```sh\n" + `go test ./...
` + "```\n"

	src2 := `## compile
Requires: prep
` + "```sh\n" + `go build -v ./...
` + "```\n" + `
## test
Requires: compile
` + "```sh\n" + `go test ./...
` + "```\n"

	doc1, err := dag.Parse(strings.NewReader(src1), "dag.md")
	if err != nil {
		t.Fatalf("Parse doc1: %v", err)
	}
	doc2, err := dag.Parse(strings.NewReader(src2), "dag.md")
	if err != nil {
		t.Fatalf("Parse doc2: %v", err)
	}

	compile1, _ := doc1.Lookup("compile")
	test1, _ := doc1.Lookup("test")

	compile2, _ := doc2.Lookup("compile")
	test2, _ := doc2.Lookup("test")

	idCompile1 := identity.DagTargetIdentity(compile1)
	idCompile2 := identity.DagTargetIdentity(compile2)

	idTest1 := identity.DagTargetIdentity(test1)
	idTest2 := identity.DagTargetIdentity(test2)

	// Changing one byte in compile's body must change compile's identity
	if idCompile1 == idCompile2 {
		t.Fatalf("compile identity did not change after body edit: %q", idCompile1)
	}

	// Changing compile's body MUST NOT change test's identity
	if idTest1 != idTest2 {
		t.Fatalf("test identity changed when compile body changed: %q != %q", idTest1, idTest2)
	}

	// Modifying task directly updates hash
	prev := identity.DagTargetIdentity(test1)
	test1.Body = "go test -run TestFoo ./...\n"
	updated := identity.DagTargetIdentity(test1)
	if prev == updated {
		t.Fatalf("expected updated identity after modifying test1.Body directly, got %q", updated)
	}
}

func TestDagTargetIdentity_DescriptionChangeIgnored(t *testing.T) {
	src1 := `## target
First description.
Requires: prep
` + "```sh\n" + `echo 42
` + "```\n"

	src2 := `## target
Completely different documentation and notes here.
Requires: prep
` + "```sh\n" + `echo 42
` + "```\n"

	doc1, err := dag.Parse(strings.NewReader(src1), "dag.md")
	if err != nil {
		t.Fatalf("Parse doc1: %v", err)
	}
	doc2, err := dag.Parse(strings.NewReader(src2), "dag.md")
	if err != nil {
		t.Fatalf("Parse doc2: %v", err)
	}

	t1, _ := doc1.Lookup("target")
	t2, _ := doc2.Lookup("target")

	id1 := identity.DagTargetIdentity(t1)
	id2 := identity.DagTargetIdentity(t2)

	if id1 != id2 {
		t.Fatalf("description change should not change identity: %q != %q", id1, id2)
	}
}

func TestDagTargetIdentity_CRLFNormalization(t *testing.T) {
	lf := "## target\r\nRequires: prep\r\n```sh\r\necho hi\r\n```\r\n"
	crlf := "## target\nRequires: prep\n```sh\necho hi\n```\n"

	docLF, err := dag.Parse(strings.NewReader(lf), "dag.md")
	if err != nil {
		t.Fatalf("Parse LF: %v", err)
	}
	docCRLF, err := dag.Parse(strings.NewReader(crlf), "dag.md")
	if err != nil {
		t.Fatalf("Parse CRLF: %v", err)
	}

	tLF, _ := docLF.Lookup("target")
	tCRLF, _ := docCRLF.Lookup("target")

	if identity.DagTargetIdentity(tLF) != identity.DagTargetIdentity(tCRLF) {
		t.Fatalf("CRLF normalization failed: %q != %q",
			identity.DagTargetIdentity(tLF), identity.DagTargetIdentity(tCRLF))
	}
}

func TestDagTargetCapability(t *testing.T) {
	src := `## deploy
Require: test -f bin/app
Ensure: test -f deployed.flag
Ensure: http-ok http://localhost:8080/health
Effects: net,write
` + "```sh\n" + `deploy-script.sh
` + "```\n"

	doc, err := dag.Parse(strings.NewReader(src), "dag.md")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	task, _ := doc.Lookup("deploy")

	cap1 := identity.DagTargetCapability(task)
	if !strings.HasPrefix(cap1, "k") || len(cap1) != 65 {
		t.Fatalf("expected 65-char hex with 'k' prefix, got %q", cap1)
	}

	// Verify capability is exposed on task field
	if task.Capability != cap1 {
		t.Fatalf("task.Capability field mismatch: %q != %q", task.Capability, cap1)
	}

	// Changing body does NOT change capability
	srcDifferentBody := `## deploy
Require: test -f bin/app
Ensure: test -f deployed.flag
Ensure: http-ok http://localhost:8080/health
Effects: net,write
` + "```sh\n" + `echo completely different implementation
` + "```\n"
	docDiffBody, _ := dag.Parse(strings.NewReader(srcDifferentBody), "dag.md")
	taskDiffBody, _ := docDiffBody.Lookup("deploy")
	capDiffBody := identity.DagTargetCapability(taskDiffBody)
	if cap1 != capDiffBody {
		t.Fatalf("capability changed when only body changed: %q != %q", cap1, capDiffBody)
	}

	// Reordering Ensure clauses does NOT change capability
	srcReordered := `## deploy
Require: test -f bin/app
Ensure: http-ok http://localhost:8080/health
Ensure: test -f deployed.flag
Effects: write,net
` + "```sh\n" + `deploy-script.sh
` + "```\n"
	docReordered, _ := dag.Parse(strings.NewReader(srcReordered), "dag.md")
	taskReordered, _ := docReordered.Lookup("deploy")
	capReordered := identity.DagTargetCapability(taskReordered)
	if cap1 != capReordered {
		t.Fatalf("capability changed when contract clauses reordered: %q != %q", cap1, capReordered)
	}

	// Modifying a contract clause DOES change capability
	srcModifiedContract := `## deploy
Require: test -f bin/app
Ensure: test -f deployed.flag
Ensure: http-ok http://localhost:9090/health
Effects: net,write
` + "```sh\n" + `deploy-script.sh
` + "```\n"
	docMod, _ := dag.Parse(strings.NewReader(srcModifiedContract), "dag.md")
	taskMod, _ := docMod.Lookup("deploy")
	capMod := identity.DagTargetCapability(taskMod)
	if cap1 == capMod {
		t.Fatalf("capability did not change when postcondition edited: %q", cap1)
	}

	// Target without contract has empty capability and returns ErrNoContract
	srcNoContract := `## plain
` + "```sh\n" + `echo hello
` + "```\n"
	docNoContract, _ := dag.Parse(strings.NewReader(srcNoContract), "dag.md")
	taskNoContract, _ := docNoContract.Lookup("plain")
	if capNone := identity.DagTargetCapability(taskNoContract); capNone != "" {
		t.Fatalf("expected empty capability for contract-less target, got %q", capNone)
	}
	if _, err := identity.DagTargetCapabilityKey(taskNoContract); err != identity.ErrNoContract {
		t.Fatalf("expected ErrNoContract, got %v", err)
	}
}

func TestScriptIdentity_StableAndBodyByteChange(t *testing.T) {
	script1 := []byte(`#!/usr/bin/env bash
# name: worker
# description: work script
# type: bsh

echo "step 1"
echo "step 2"
`)

	script2 := []byte(`#!/usr/bin/env bash
# name: worker
# description: work script
# type: bsh

echo "step 1"
echo "step 3"
`)

	id1 := identity.ScriptIdentity(script1)
	if !strings.HasPrefix(id1, "h") || len(id1) != 65 {
		t.Fatalf("expected 65-char hex with 'h' prefix, got %q", id1)
	}

	id1Repeat := identity.ScriptIdentity(script1)
	if id1 != id1Repeat {
		t.Fatalf("script identity not stable: %q != %q", id1, id1Repeat)
	}

	id2 := identity.ScriptIdentity(script2)
	if id1 == id2 {
		t.Fatalf("script identity did not change when one body byte changed: %q", id1)
	}

	// CRLF normalization
	script1CRLF := []byte(strings.ReplaceAll(string(script1), "\n", "\r\n"))
	id1CRLF := identity.ScriptIdentity(script1CRLF)
	if id1 != id1CRLF {
		t.Fatalf("CRLF normalization failed for script: %q != %q", id1, id1CRLF)
	}
}

func TestScriptHeader_Cases(t *testing.T) {
	// 1. Shebang + comment block
	srcWithShebang := []byte(`#!/usr/bin/env bash
# name: test-tool
# description: A handy test tool
# type: bsh
# extra: ignored-as-envelope-core

echo "run"
`)
	h1 := identity.ScriptHeader(srcWithShebang)
	if h1.Name != "test-tool" {
		t.Errorf("Name = %q, want 'test-tool'", h1.Name)
	}
	if h1.Description != "A handy test tool" {
		t.Errorf("Description = %q, want 'A handy test tool'", h1.Description)
	}
	if h1.Type != "bsh" {
		t.Errorf("Type = %q, want 'bsh'", h1.Type)
	}
	if h1.Metadata["extra"] != "ignored-as-envelope-core" {
		t.Errorf("Metadata[extra] = %q, want 'ignored-as-envelope-core'", h1.Metadata["extra"])
	}

	// 2. Comment block without shebang
	srcNoShebang := []byte(`# name: no-shebang
# description: Starts immediately
# type: script

echo "hello"
`)
	h2 := identity.ScriptHeader(srcNoShebang)
	if h2.Name != "no-shebang" || h2.Description != "Starts immediately" || h2.Type != "script" {
		t.Errorf("h2 mismatch: %+v", h2)
	}

	// 3. No header (code starts immediately)
	srcCodeFirst := []byte(`echo "hello"
# name: late
`)
	h3 := identity.ScriptHeader(srcCodeFirst)
	if h3.Name != "" || h3.Description != "" || h3.Type != "" {
		t.Errorf("h3 should be empty: %+v", h3)
	}

	// 4. Shebang with code immediately (no comment block)
	srcShebangOnly := []byte(`#!/bin/bash
echo "hello"
# name: late
`)
	h4 := identity.ScriptHeader(srcShebangOnly)
	if h4.Name != "" || h4.Description != "" || h4.Type != "" {
		t.Errorf("h4 should be empty: %+v", h4)
	}

	// 5. Empty source
	h5 := identity.ScriptHeader([]byte(""))
	if h5.Name != "" || h5.Description != "" || h5.Type != "" {
		t.Errorf("h5 should be empty: %+v", h5)
	}

	// 6. Stop at first non-comment line
	srcStopEarly := []byte(`#!/bin/bash
# name: early
# description: before break

# name: late
echo "hi"
`)
	h6 := identity.ScriptHeader(srcStopEarly)
	if h6.Name != "early" {
		t.Errorf("h6.Name = %q, want 'early' (should stop at blank line)", h6.Name)
	}
}
