package lenscmd

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qiangli/coreutils/tool"
)

func runTool(t *testing.T, dir, stdin string, args ...string) (string, string, int) {
	t.Helper()
	var out, errb bytes.Buffer
	rc := &tool.RunContext{Ctx: context.Background(), Dir: dir, Stdio: tool.Stdio{Out: &out, Err: &errb, In: strings.NewReader(stdin)}}
	code := cmd.Run(rc, args)
	return out.String(), errb.String(), code
}

func TestConvertStdinAndFile(t *testing.T) {
	dir := t.TempDir()
	out, errb, code := runTool(t, dir, `{"a": [1, "x"]}`, "-f", "json", "-t", "yaml")
	if code != 0 || errb != "" || out != "a:\n  - 1\n  - x\n" {
		t.Fatalf("stdin: code=%d out=%q err=%q", code, out, errb)
	}
	// FROM from the extension; "-" is stdin.
	if err := os.WriteFile(filepath.Join(dir, "in.yml"), []byte(out), 0o644); err != nil {
		t.Fatal(err)
	}
	out, errb, code = runTool(t, dir, "", "-t", "json", "in.yml")
	if code != 0 || errb != "" || out != "{\n  \"a\": [\n    1,\n    \"x\"\n  ]\n}\n" {
		t.Fatalf("file: code=%d out=%q err=%q", code, out, errb)
	}
	out2, _, _ := runTool(t, dir, "", "-t", "json", "in.yml")
	if out2 != out {
		t.Fatal("not deterministic")
	}
	if out, _, code := runTool(t, dir, "a,b\n1,2\n", "-f", "csv", "-t", "md", "-"); code != 0 || out != "| a | b |\n| --- | --- |\n| 1 | 2 |\n" {
		t.Fatalf("csv md: %d %q", code, out)
	}
}

func TestExitCodes(t *testing.T) {
	dir := t.TempDir()
	for _, c := range []struct {
		stdin string
		args  []string
		code  int
		err   string
	}{
		{`{"a":`, []string{"-f", "json", "-t", "yaml"}, 1, "invalid json input"},
		{"", []string{"-t", "json", "missing.yaml"}, 1, "missing.yaml"},
		{"", []string{"-f", "json"}, 2, "-t TO is required"},
		{"", []string{"-t", "nope"}, 2, "unknown format"},
		{"", []string{"-f", "nope", "-t", "json"}, 2, "unknown format"},
		{"", []string{"-t", "json", "a", "b"}, 2, "at most one FILE"},
		{"a: 1\n", []string{"-t", "json"}, 2, "pass -f FROM"},
		{"", []string{"-l", "-t", "json"}, 2, "-l takes no"},
		{"", []string{"--bogus"}, 2, ""},
		{"x", []string{"-f", "pdf", "-t", "yaml"}, 3, "no route pdf -> yaml"},
		{`{"a": null}`, []string{"--strict", "-f", "json", "-t", "toml"}, 4, "no null"},
		{"<p>x</p>", []string{"--strict", "-f", "html", "-t", "md"}, 4, "lossy row"},
		{"x", []string{"-f", "pptx", "-t", "md"}, 127, "markitdown"},
	} {
		out, errb, code := runTool(t, dir, c.stdin, c.args...)
		if code != c.code || !strings.Contains(errb, c.err) || out != "" {
			t.Errorf("%v: code=%d (want %d) out=%q err=%q (want %q)", c.args, code, c.code, out, errb, c.err)
		}
	}
}

func TestLossyWithoutStrictWritesDataAndWarns(t *testing.T) {
	out, errb, code := runTool(t, t.TempDir(), `{"a": null, "b": 1}`, "-f", "json", "-t", "toml")
	if code != 0 || out != "b = 1\n" || !strings.Contains(errb, "lens: lossy:") {
		t.Fatalf("code=%d out=%q err=%q", code, out, errb)
	}
	_, errb, _ = runTool(t, t.TempDir(), `{"b": 1}`, "--verbose", "-f", "json", "-t", "yaml")
	if errb != "lens: route json -> yaml via go (lossless)\n" {
		t.Fatalf("verbose: %q", errb)
	}
	if _, errb, code := runTool(t, t.TempDir(), `{"b": 1}`, "--strict", "-f", "json", "-t", "yaml"); code != 0 || errb != "" {
		t.Fatalf("strict lossless: %d %q", code, errb)
	}
}

func TestList(t *testing.T) {
	out, errb, code := runTool(t, t.TempDir(), "", "-l")
	if code != 0 || errb != "" || !strings.Contains(out, "json\tyaml\tlossless\tgo\tinstalled\n") ||
		!strings.Contains(out, "tar\tlisting\tlossless\tgo\tinstalled\n") {
		t.Fatalf("code=%d out=%q", code, out)
	}
	out, _, code = runTool(t, t.TempDir(), "", "-l", "--json")
	lines := strings.Split(strings.TrimSpace(out), "\n")
	for _, l := range lines {
		var row struct {
			From, To, Fidelity, Via string
			Installed               *bool
		}
		if json.Unmarshal([]byte(l), &row) != nil || row.From == "" || row.Installed == nil {
			t.Fatalf("bad row %q", l)
		}
	}
	if code != 0 || len(lines) < 30 {
		t.Fatalf("code=%d rows=%d", code, len(lines))
	}
	if _, _, code := runTool(t, t.TempDir(), "", "--json", "-t", "yaml"); code != 2 {
		t.Fatalf("--json without -l: %d", code)
	}
}

// TestPipeHygiene is `lens … | head -1`: the reader closes after one line; the
// writer must end quietly with exit 0 and nothing on stderr.
func TestPipeHygiene(t *testing.T) {
	var rows strings.Builder
	rows.WriteString("[")
	for i := 0; i < 50000; i++ {
		if i > 0 {
			rows.WriteString(",")
		}
		rows.WriteString(`{"id": 1, "name": "row"}`)
	}
	rows.WriteString("]")

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	first := make(chan string)
	go func() {
		line, _ := bufio.NewReader(r).ReadString('\n')
		r.Close()
		first <- line
	}()
	var errb bytes.Buffer
	rc := &tool.RunContext{Ctx: context.Background(), Dir: t.TempDir(), Stdio: tool.Stdio{Out: w, Err: &errb, In: strings.NewReader(rows.String())}}
	code := cmd.Run(rc, []string{"-f", "json", "-t", "jsonl"})
	w.Close()
	if line := <-first; line != "{\"id\":1,\"name\":\"row\"}\n" {
		t.Fatalf("first line %q", line)
	}
	if code != 0 || errb.Len() != 0 {
		t.Fatalf("code=%d err=%q", code, errb.String())
	}
}
