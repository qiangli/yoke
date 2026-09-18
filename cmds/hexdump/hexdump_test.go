package hexdumpcmd

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qiangli/coreutils/tool"
)

func runTool(t *testing.T, dir, stdin string, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	if dir == "" {
		dir = t.TempDir()
	}
	var out, errb bytes.Buffer
	rc := &tool.RunContext{
		Ctx:   context.Background(),
		Dir:   dir,
		Stdio: tool.Stdio{In: strings.NewReader(stdin), Out: &out, Err: &errb},
	}
	code = cmd.Run(rc, args)
	return out.String(), errb.String(), code
}

// expected builds the canonical dump for inputs with no repeated
// 16-byte lines: encoding/hex.Dump emits the same line format as
// hexdump -C; the tool additionally appends the final offset line.
func expected(data string) string {
	if data == "" {
		return ""
	}
	return hex.Dump([]byte(data)) + fmt.Sprintf("%08x\n", len(data))
}

func TestHexdumpCanonical(t *testing.T) {
	cases := []string{
		"hello world\n",
		"exactly sixteen!",       // exactly one full line
		"0123456789abcdef0123",   // full line + partial
		"\x00\x01\x02\xfe\xff~ ", // non-printables map to '.'
	}
	for _, data := range cases {
		out, errb, code := runTool(t, "", data, "-C")
		if want := expected(data); out != want || code != 0 {
			t.Errorf("hexdump -C %q = (%q, %q, %d), want %q", data, out, errb, code, want)
		}
	}
}

func TestHexdumpEmpty(t *testing.T) {
	out, _, code := runTool(t, "", "", "-C")
	if out != "" || code != 0 {
		t.Errorf("empty input: out=%q code=%d", out, code)
	}
}

func TestHexdumpSqueeze(t *testing.T) {
	data := strings.Repeat("\x00", 48)
	out, _, code := runTool(t, "", data, "-C")
	want := "00000000  00 00 00 00 00 00 00 00  00 00 00 00 00 00 00 00  |................|\n" +
		"*\n" +
		"00000030\n"
	if out != want || code != 0 {
		t.Errorf("squeeze: got (%q, %d), want %q", out, code, want)
	}

	// A differing line ends the squeeze run.
	data = strings.Repeat("\x00", 32) + "tail"
	out, _, _ = runTool(t, "", data, "-C")
	want = "00000000  00 00 00 00 00 00 00 00  00 00 00 00 00 00 00 00  |................|\n" +
		"*\n" +
		"00000020  74 61 69 6c                                       |tail|\n" +
		"00000024\n"
	if out != want {
		t.Errorf("squeeze then tail: got %q, want %q", out, want)
	}
}

func TestHexdumpFiles(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a"), []byte("abc"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "b"), []byte("def"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Multiple files are concatenated into one dump.
	out, _, code := runTool(t, dir, "", "-C", "a", "b")
	if want := expected("abcdef"); out != want || code != 0 {
		t.Errorf("two files: got (%q, %d), want %q", out, code, want)
	}
}

func TestHexdumpErrors(t *testing.T) {
	// Default (non -C) formats are deliberately unsupported.
	_, errb, code := runTool(t, "", "x")
	if code != 2 || !strings.Contains(errb, "not supported") {
		t.Errorf("no -C: err=%q code=%d", errb, code)
	}

	_, errb, code = runTool(t, "", "", "-C", "missing")
	if code != 1 || !strings.Contains(errb, "hexdump: missing:") {
		t.Errorf("missing file: err=%q code=%d", errb, code)
	}

	_, errb, code = runTool(t, "", "", "-x")
	if code != 2 || !strings.Contains(errb, "x") {
		t.Errorf("unknown flag -x: err=%q code=%d", errb, code)
	}

	_, errb, code = runTool(t, "", "", "--frobnicate")
	if code != 2 || !strings.Contains(errb, "frobnicate") || !strings.Contains(errb, "pure-Go") {
		t.Errorf("unknown flag: err=%q code=%d", errb, code)
	}
}

func TestHexdumpHelpVersion(t *testing.T) {
	out, _, code := runTool(t, "", "", "--help")
	if code != 0 || !strings.Contains(out, "Usage: hexdump") {
		t.Errorf("--help: code=%d out=%q", code, out)
	}
	out, _, code = runTool(t, "", "", "--version")
	if code != 0 || !strings.Contains(out, "hexdump") {
		t.Errorf("--version: code=%d out=%q", code, out)
	}
}
