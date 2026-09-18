package jqcmd

import (
	"bytes"
	"context"
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
		Env:   []string{"USER=tester"},
		Stdio: tool.Stdio{In: strings.NewReader(stdin), Out: &out, Err: &errb},
	}
	code = cmd.Run(rc, args)
	return out.String(), errb.String(), code
}

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestJQFilterStdin(t *testing.T) {
	out, errb, code := runTool(t, "", `{"foo":128}`, ".foo")
	if code != 0 || out != "128\n" || errb != "" {
		t.Fatalf("jq .foo = (%q, %q, %d)", out, errb, code)
	}
}

func TestJQPrettyAndCompactOutput(t *testing.T) {
	out, _, code := runTool(t, "", `{"b":2,"a":1}`, ".")
	if code != 0 || out != "{\n  \"a\": 1,\n  \"b\": 2\n}\n" {
		t.Fatalf("pretty output = (%q, %d)", out, code)
	}

	out, _, code = runTool(t, "", `{"b":2,"a":1}`, "-c", ".")
	if code != 0 || out != "{\"a\":1,\"b\":2}\n" {
		t.Fatalf("compact output = (%q, %d)", out, code)
	}
}

func TestJQRawOutput(t *testing.T) {
	out, _, code := runTool(t, "", `{"name":"coreutils"}`, "-r", ".name")
	if code != 0 || out != "coreutils\n" {
		t.Fatalf("raw output = (%q, %d)", out, code)
	}
}

func TestJQMultipleInputsAndFiles(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "data.json", `{"id":1} {"id":2}`)

	out, _, code := runTool(t, dir, "", ".id", "data.json")
	if code != 0 || out != "1\n2\n" {
		t.Fatalf("file inputs = (%q, %d)", out, code)
	}
}

func TestJQNullInputAndEnv(t *testing.T) {
	out, _, code := runTool(t, "", "", "-n", "-r", `env.USER`)
	if code != 0 || out != "tester\n" {
		t.Fatalf("null input env = (%q, %d)", out, code)
	}
}

func TestJQExitStatus(t *testing.T) {
	_, _, code := runTool(t, "", `false`, "-e", ".")
	if code != 1 {
		t.Fatalf("false exit status = %d, want 1", code)
	}

	_, _, code = runTool(t, "", `true`, "-e", ".")
	if code != 0 {
		t.Fatalf("true exit status = %d, want 0", code)
	}
}

func TestJQErrors(t *testing.T) {
	_, errb, code := runTool(t, "", `{"x":1}`, ".foo & .bar")
	if code != 2 || !strings.Contains(errb, "invalid query") {
		t.Fatalf("query error = (%q, %d)", errb, code)
	}

	_, errb, code = runTool(t, "", `{x}`, ".")
	if code != 1 || !strings.Contains(errb, "invalid json") {
		t.Fatalf("json error = (%q, %d)", errb, code)
	}

	_, errb, code = runTool(t, "", "", "--slurp")
	if code != 2 || !strings.Contains(errb, "pure-Go") {
		t.Fatalf("unsupported flag = (%q, %d)", errb, code)
	}
}
