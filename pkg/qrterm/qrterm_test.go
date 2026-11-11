// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

package qrterm

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTerminalRendersScannableBlock(t *testing.T) {
	block, err := Terminal("https://example.org/x")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(block, "\n"), "\n")
	if len(lines) < 12 {
		t.Fatalf("QR block is only %d lines; that is not a scannable code", len(lines))
	}
	if w := len([]rune(lines[len(lines)/2])); w < 20 {
		t.Fatalf("QR block is %d columns wide", w)
	}
}

func TestTerminalRefusesEmptyPayload(t *testing.T) {
	if _, err := Terminal(""); err == nil {
		t.Fatal("empty payload rendered")
	}
}

func TestPNGWritesFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.png")
	if err := PNG("https://example.org/x", path, 0); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(b, []byte("\x89PNG")) {
		t.Fatalf("not a PNG: % x", b[:8])
	}
}
