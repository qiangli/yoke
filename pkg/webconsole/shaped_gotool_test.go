// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

package webconsole

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestResolveGoTool(t *testing.T) {
	onPath := func(string) (string, error) { return "/usr/bin/go", nil }
	missing := func(string) (string, error) { return "", errors.New("not found") }
	self := func() (string, error) { return "/opt/bashy", nil }

	if got := resolveGoTool(onPath, self, nil); got != "go" {
		t.Errorf("go on PATH: got %q, want go", got)
	}

	root := t.TempDir()
	name := "go"
	if runtime.GOOS == "windows" {
		name = "go.exe"
	}
	tool := filepath.Join(root, "bin", name)
	if err := os.MkdirAll(filepath.Dir(tool), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tool, []byte("#!"), 0o755); err != nil {
		t.Fatal(err)
	}
	var asked string
	goroot := func(exe string) (string, error) { asked = exe; return root, nil }
	if got := resolveGoTool(missing, self, goroot); got != tool || asked != "/opt/bashy" {
		t.Errorf("bashy toolchain: got %q (asked %q), want %q", got, asked, tool)
	}

	if got := resolveGoTool(missing, self, func(string) (string, error) { return "", errors.New("no toolchain") }); got != "go" {
		t.Errorf("no toolchain: got %q, want go", got)
	}
	if got := resolveGoTool(missing, self, func(string) (string, error) { return t.TempDir(), nil }); got != "go" {
		t.Errorf("GOROOT without bin/go: got %q, want go", got)
	}
	if got := lastLine("downloading go1.27.2\r\nC:\\go\r\n"); got != `C:\go` {
		t.Errorf("lastLine = %q", got)
	}
}
