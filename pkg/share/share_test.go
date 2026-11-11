// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

package share

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func run(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var out, errb bytes.Buffer
	cmd := NewCmd()
	cmd.SetOut(&out)
	cmd.SetErr(&errb)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), err
}

func TestQRPrintsURLThenCode(t *testing.T) {
	out, err := run(t, "qr", "https://example.org/x")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if lines[0] != "https://example.org/x" {
		t.Fatalf("first line = %q, want the URL", lines[0])
	}
	if len(lines) < 13 {
		t.Fatalf("only %d lines; the QR block is missing", len(lines))
	}
	for i, l := range lines[1:] {
		if strings.TrimSpace(l) == "" {
			t.Fatalf("QR line %d is empty", i+1)
		}
	}
}

func TestQRRefusesBadURL(t *testing.T) {
	for _, bad := range []string{"", "example.org/x", "https://", "not a url", "mailto:"} {
		if _, err := run(t, "qr", bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestQRWritesPNGAndJSON(t *testing.T) {
	png := filepath.Join(t.TempDir(), "x.png")
	out, err := run(t, "qr", "https://example.org/x", "--png", png, "--json")
	if err != nil {
		t.Fatal(err)
	}
	var got struct{ URL, PNG string }
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("not JSON: %q: %v", out, err)
	}
	if got.URL != "https://example.org/x" || got.PNG != png {
		t.Fatalf("json = %+v", got)
	}
	b, err := os.ReadFile(png)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(b, []byte("\x89PNG")) {
		t.Fatal("--png did not write a PNG")
	}
}

func TestQRPNGWithoutJSONNamesTheFile(t *testing.T) {
	png := filepath.Join(t.TempDir(), "x.png")
	out, err := run(t, "qr", "https://example.org/x", "--png", png)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(out, "png: "+png+"\n") {
		t.Fatalf("output does not name the PNG: %q", out)
	}
	if _, err := os.Stat(png); err != nil {
		t.Fatal(err)
	}
}
