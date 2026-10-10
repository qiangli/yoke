package fleet

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCopyToolPin_CopiesHelperWhenPresent(t *testing.T) {
	srcDir := t.TempDir()
	srcBin := filepath.Join(srcDir, "codex")
	srcHelper := filepath.Join(srcDir, "codex-code-mode-host")

	if err := os.WriteFile(srcBin, []byte("#!/bin/sh\necho codex\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(srcHelper, []byte("#!/bin/sh\necho host\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	destDir := t.TempDir()
	destBin, copied, err := CopyToolPin("codex", srcBin, destDir)
	if err != nil {
		t.Fatalf("CopyToolPin failed: %v", err)
	}

	wantBin := filepath.Join(destDir, "codex")
	if destBin != wantBin {
		t.Fatalf("destBin = %q, want %q", destBin, wantBin)
	}
	if !fileExists(destBin) {
		t.Fatal("destBin does not exist")
	}

	wantHelper := filepath.Join(destDir, "codex-code-mode-host")
	if !fileExists(wantHelper) {
		t.Fatal("helper codex-code-mode-host was not copied beside binary")
	}
	if len(copied) != 1 || copied[0] != "codex-code-mode-host" {
		t.Fatalf("copied = %v, want [codex-code-mode-host]", copied)
	}
}

func TestCopyToolPin_CopiesHelperFromSymlinkTarget(t *testing.T) {
	// Mirrors Homebrew cask: /opt/homebrew/bin/codex -> /opt/homebrew/Caskroom/codex/.../bin/codex
	// The helper sits in the real directory beside the target, not in the symlink directory.
	realDir := t.TempDir()
	realBin := filepath.Join(realDir, "codex")
	realHelper := filepath.Join(realDir, "codex-code-mode-host")

	if err := os.WriteFile(realBin, []byte("#!/bin/sh\necho codex\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(realHelper, []byte("#!/bin/sh\necho host\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	linkDir := t.TempDir()
	linkBin := filepath.Join(linkDir, "codex")
	if err := os.Symlink(realBin, linkBin); err != nil {
		t.Fatal(err)
	}

	destDir := t.TempDir()
	destBin, copied, err := CopyToolPin("codex", linkBin, destDir)
	if err != nil {
		t.Fatalf("CopyToolPin via symlink failed: %v", err)
	}

	wantHelper := filepath.Join(destDir, "codex-code-mode-host")
	if !fileExists(wantHelper) {
		t.Fatal("helper codex-code-mode-host was not copied from symlink real target directory")
	}
	if len(copied) != 1 || copied[0] != "codex-code-mode-host" {
		t.Fatalf("copied = %v, want [codex-code-mode-host]", copied)
	}
	if !fileExists(destBin) {
		t.Fatal("destBin does not exist")
	}
}

func TestCopyToolPin_NoHelperWhenAbsent(t *testing.T) {
	srcDir := t.TempDir()
	srcBin := filepath.Join(srcDir, "codex")
	if err := os.WriteFile(srcBin, []byte("#!/bin/sh\necho codex\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	destDir := t.TempDir()
	destBin, copied, err := CopyToolPin("codex", srcBin, destDir)
	if err != nil {
		t.Fatalf("CopyToolPin failed: %v", err)
	}
	if !fileExists(destBin) {
		t.Fatal("destBin does not exist")
	}
	if len(copied) != 0 {
		t.Fatalf("copied = %v, want empty", copied)
	}
	if fileExists(filepath.Join(destDir, "codex-code-mode-host")) {
		t.Fatal("helper unexpectedly exists")
	}
}

func TestToolShow_FlagsMissingHelperWithFixCommand(t *testing.T) {
	root := t.TempDir()
	cat := New(WithRoot(root))

	pinDir := filepath.Join(t.TempDir(), "tools", "codex", "0.157.1")
	if err := os.MkdirAll(pinDir, 0o755); err != nil {
		t.Fatal(err)
	}
	pinnedBin := filepath.Join(pinDir, "codex")
	if err := os.WriteFile(pinnedBin, []byte("#!/bin/sh\necho codex\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	// codex-code-mode-host is NOT in pinDir.
	if err := cat.SaveTool(Tool{
		Name: "codex", Kind: ToolKindCLI, Display: "Codex",
		CLI: ToolCLI{
			Binary: pinnedBin,
			Versions: []ToolVersion{
				{Version: "0.157.1"},
			},
		},
	}); err != nil {
		t.Fatal(err)
	}

	cmd := NewToolsCmd(WithRoot(root))
	out, err := runCmd(t, cmd, "show", "codex")
	if err != nil {
		t.Fatalf("show codex failed: %v\n%s", err, out)
	}

	if !strings.Contains(out, "warning:") {
		t.Fatalf("output missing 'warning:':\n%s", out)
	}
	if !strings.Contains(out, "codex-code-mode-host") {
		t.Fatalf("output missing 'codex-code-mode-host':\n%s", out)
	}
	if !strings.Contains(out, "missing known helper") {
		t.Fatalf("output missing 'missing known helper':\n%s", out)
	}
	if !strings.Contains(out, "fix:") {
		t.Fatalf("output missing 'fix:':\n%s", out)
	}
	if !strings.Contains(out, "cp ") || !strings.Contains(out, pinDir) {
		t.Fatalf("fix command does not contain cp to %s:\n%s", pinDir, out)
	}
}

func TestToolShow_NoFlagWhenHelperPresent(t *testing.T) {
	root := t.TempDir()
	cat := New(WithRoot(root))

	pinDir := filepath.Join(t.TempDir(), "tools", "codex", "0.157.1")
	if err := os.MkdirAll(pinDir, 0o755); err != nil {
		t.Fatal(err)
	}
	pinnedBin := filepath.Join(pinDir, "codex")
	helperBin := filepath.Join(pinDir, "codex-code-mode-host")
	if err := os.WriteFile(pinnedBin, []byte("#!/bin/sh\necho codex\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(helperBin, []byte("#!/bin/sh\necho host\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := cat.SaveTool(Tool{
		Name: "codex", Kind: ToolKindCLI, Display: "Codex",
		CLI: ToolCLI{
			Binary: pinnedBin,
		},
	}); err != nil {
		t.Fatal(err)
	}

	cmd := NewToolsCmd(WithRoot(root))
	out, err := runCmd(t, cmd, "show", "codex")
	if err != nil {
		t.Fatalf("show codex failed: %v\n%s", err, out)
	}

	if strings.Contains(out, "warning:") || strings.Contains(out, "missing known helper") {
		t.Fatalf("unexpected warning when helper is present:\n%s", out)
	}
}

func TestToolShow_NoFlagForUnpinnedTool(t *testing.T) {
	root := t.TempDir()
	cat := New(WithRoot(root))

	if err := cat.SaveTool(Tool{
		Name: "codex", Kind: ToolKindCLI, Display: "Codex",
		CLI: ToolCLI{
			Binary: "codex", // bare name on PATH, not a pinned path
		},
	}); err != nil {
		t.Fatal(err)
	}

	cmd := NewToolsCmd(WithRoot(root))
	out, err := runCmd(t, cmd, "show", "codex")
	if err != nil {
		t.Fatalf("show codex failed: %v\n%s", err, out)
	}

	if strings.Contains(out, "warning:") || strings.Contains(out, "missing known helper") {
		t.Fatalf("unexpected warning for unpinned tool:\n%s", out)
	}
}

func TestToolInstall_CopiesHelperAndUpdatesCatalog(t *testing.T) {
	root := t.TempDir()
	cat := New(WithRoot(root))

	// Register baseline tool
	if err := cat.SaveTool(Tool{
		Name: "codex", Kind: ToolKindCLI, Display: "Codex",
		CLI: ToolCLI{Binary: "codex"},
	}); err != nil {
		t.Fatal(err)
	}

	srcDir := t.TempDir()
	srcBin := filepath.Join(srcDir, "codex")
	srcHelper := filepath.Join(srcDir, "codex-code-mode-host")
	if err := os.WriteFile(srcBin, []byte("#!/bin/sh\necho codex 0.157.1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(srcHelper, []byte("#!/bin/sh\necho host\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	toolsDir := filepath.Join(t.TempDir(), "tools")
	cmd := NewToolsCmd(WithRoot(root))
	out, err := runCmd(t, cmd, "install", "codex", "--source", srcBin, "--version", "0.157.1", "--tools-dir", toolsDir)
	if err != nil {
		t.Fatalf("install command failed: %v\n%s", err, out)
	}

	if !strings.Contains(out, "installed codex ->") {
		t.Fatalf("output missing 'installed codex ->':\n%s", out)
	}
	if !strings.Contains(out, "copied helpers: codex-code-mode-host") {
		t.Fatalf("output missing 'copied helpers: codex-code-mode-host':\n%s", out)
	}

	// Verify catalog was updated
	tUpdated, ok := cat.Tool("codex")
	if !ok {
		t.Fatal("codex not found in catalog after install")
	}
	wantDest := filepath.Join(toolsDir, "codex", "0.157.1", "codex")
	if tUpdated.CLI.Binary != wantDest {
		t.Fatalf("tUpdated.CLI.Binary = %q, want %q", tUpdated.CLI.Binary, wantDest)
	}

	// Verify helper file exists beside binary
	wantHelper := filepath.Join(toolsDir, "codex", "0.157.1", "codex-code-mode-host")
	if !fileExists(wantHelper) {
		t.Fatal("helper not found in destination directory")
	}

	// Verify tool show does NOT flag missing helpers now
	showOut, err := runCmd(t, NewToolsCmd(WithRoot(root)), "show", "codex")
	if err != nil {
		t.Fatalf("show codex failed: %v\n%s", err, showOut)
	}
	if strings.Contains(showOut, "warning:") || strings.Contains(showOut, "missing known helper") {
		t.Fatalf("show unexpectedly warned after install:\n%s", showOut)
	}
}

func TestRegisterKnownHelper_Extendable(t *testing.T) {
	RegisterKnownHelper("customtool", "custom-helper")
	t.Cleanup(func() {
		delete(KnownToolHelpers, "customtool")
	})

	if helpers := KnownToolHelpers["customtool"]; len(helpers) != 1 || helpers[0] != "custom-helper" {
		t.Fatalf("KnownToolHelpers[customtool] = %v, want [custom-helper]", helpers)
	}

	srcDir := t.TempDir()
	srcBin := filepath.Join(srcDir, "customtool")
	srcHelper := filepath.Join(srcDir, "custom-helper")
	if err := os.WriteFile(srcBin, []byte("#!/bin/sh\necho custom\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(srcHelper, []byte("#!/bin/sh\necho custom-helper\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	destDir := t.TempDir()
	_, copied, err := CopyToolPin("customtool", srcBin, destDir)
	if err != nil {
		t.Fatalf("CopyToolPin failed: %v", err)
	}
	if len(copied) != 1 || copied[0] != "custom-helper" {
		t.Fatalf("copied = %v, want [custom-helper]", copied)
	}
	if !fileExists(filepath.Join(destDir, "custom-helper")) {
		t.Fatal("custom-helper was not copied")
	}

	// When missing, tool show flags it
	root := t.TempDir()
	cat := New(WithRoot(root))
	missDir := t.TempDir()
	missBin := filepath.Join(missDir, "customtool")
	if err := os.WriteFile(missBin, []byte("#!/bin/sh\necho custom\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := cat.SaveTool(Tool{
		Name: "customtool", Kind: ToolKindCLI,
		CLI: ToolCLI{Binary: missBin},
	}); err != nil {
		t.Fatal(err)
	}

	out, err := runCmd(t, NewToolsCmd(WithRoot(root)), "show", "customtool")
	if err != nil {
		t.Fatalf("show customtool failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "warning:") || !strings.Contains(out, "custom-helper") {
		t.Fatalf("expected warning for missing custom-helper, got:\n%s", out)
	}
}
