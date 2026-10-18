package lenscmd

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/qiangli/coreutils/tool"
	"github.com/qiangli/yoke/mcp"
)

func TestArgvRowsCLI(t *testing.T) {
	dir := t.TempDir()
	out, errb, code := runTool(t, dir, "cp -r --dry-run \"my dir\" /c/x\n", "-f", "bash", "-t", "pwsh", "--switch=pascal", "--drive=C", "--verbose")
	if code != 0 || out != `& 'cp' '-r' '-DryRun' 'my dir' 'C:\x'`+"\n" {
		t.Fatalf("pwsh: code=%d out=%q err=%q", code, out, errb)
	}
	if errb != "lens: route bash -> pwsh via go (lossless)\nlens: executable: unverified\n" {
		t.Fatalf("verbose: %q", errb)
	}
	out, errb, code = runTool(t, dir, "echo \"a b\" 100%\n", "-f", "bash", "-t", "cmd", "--switch", "slash")
	if code != 0 || errb != "" || out != `echo ^"a b^" 100^%`+"\n" {
		t.Fatalf("cmd: code=%d out=%q err=%q", code, out, errb)
	}
	out, errb, code = runTool(t, dir, out, "-f", "cmd", "-t", "skill")
	if code != 0 || errb != "" || out != "```bsh\necho 'a b' 100%\n```\n" {
		t.Fatalf("cmd -> skill: code=%d out=%q err=%q", code, out, errb)
	}
	for _, c := range []struct {
		stdin string
		args  []string
		code  int
		err   string
	}{
		{"ls\n", []string{"-f", "bash", "-t", "cmd", "--switch=camel"}, 2, "--switch=camel"},
		{"ls\n", []string{"-f", "bash", "-t", "cmd", "--drive=9"}, 2, "--drive=9"},
		{"{}", []string{"-f", "json", "-t", "yaml", "--switch=keep"}, 2, "apply to command-line rows"},
		{"ls $HOME\n", []string{"-f", "bash", "-t", "pwsh"}, 1, "parameter expansion"},
		{"echo a\nb\n", []string{"-f", "bash", "-t", "cmd"}, 1, "exactly one command"},
		{"ls\n", []string{"-f", "bash", "-t", "toml"}, 3, "no route bash -> toml"},
		{"ls\n", []string{"-f", "pwsh", "-t", "csv"}, 3, "no route pwsh -> csv"},
	} {
		out, errb, code := runTool(t, dir, c.stdin, c.args...)
		if code != c.code || !strings.Contains(errb, c.err) || out != "" {
			t.Errorf("%v: code=%d (want %d) out=%q err=%q (want %q)", c.args, code, c.code, out, errb, c.err)
		}
	}
	// A shell script by extension is never guessed as a command line.
	if err := os.WriteFile(filepath.Join(dir, "run.sh"), []byte("echo hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, errb, code := runTool(t, dir, "", "-t", "pwsh", "run.sh"); code != 2 || !strings.Contains(errb, "pass -f FROM") {
		t.Fatalf("x.sh: %d %q", code, errb)
	}
	out, _, _ = runTool(t, dir, "", "-l")
	for _, row := range []string{"bash\tpwsh\tlossless\tgo\tinstalled\n", "mcp-call\tbash\tlossless\tgo\tinstalled\n", "json\tyaml\tlossless\tgo\tinstalled\n"} {
		if !strings.Contains(out, row) {
			t.Errorf("-l lacks %q", row)
		}
	}
	if strings.Contains(out, "bash\tbash\t") || strings.Contains(out, "yaml\tjson\tlossless\tgo\tinstalled\nyaml\tjson") {
		t.Fatal("-l has a self row or a duplicate json/yaml row")
	}
}

// echoArgvTool is a registered command whose output is its argv, so the MCP
// gate below compares what run_tool ran with what lens rendered.
var echoArgvTool = &tool.Tool{
	Name: "lens-echo-argv-test", Synopsis: "print argv as JSON", Usage: "lens-echo-argv-test ARG...",
	Run: func(rc *tool.RunContext, args []string) int {
		enc := json.NewEncoder(rc.Out)
		enc.SetEscapeHTML(false)
		if err := enc.Encode(args); err != nil {
			return 1
		}
		if len(args) > 0 && args[len(args)-1] == "fail" {
			return 3
		}
		return 0
	},
}

func init() { tool.Register(echoArgvTool) }

// TestMCPCallEqualsDirectRun: `lens -t mcp-call` fed to the MCP server's
// run_tool gives the same stdout and exit as the direct run, and the call
// renders back to the same bash line.
func TestMCPCallEqualsDirectRun(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	st, ct := mcpsdk.NewInMemoryTransports()
	ss, err := mcp.NewServer("lens-test", "test").Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ss.Close()
	client := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "lens", Version: "test"}, nil)
	session, err := client.Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	for _, line := range []string{
		"lens-echo-argv-test 'two words' '' %PATH% 'a&b' $'new\\nline' é -- -x\n",
		"lens-echo-argv-test fail\n",
	} {
		call, errb, code := runTool(t, t.TempDir(), line, "-f", "bash", "-t", "mcp-call")
		if code != 0 || errb != "" {
			t.Fatalf("render: %d %q", code, errb)
		}
		var req tool.MCPToolCall
		if err := json.Unmarshal([]byte(call), &req); err != nil {
			t.Fatal(err)
		}
		result, err := session.CallTool(ctx, &mcpsdk.CallToolParams{Name: req.Params.Name, Arguments: req.Params.Arguments})
		if err != nil {
			t.Fatal(err)
		}
		var out mcp.RunToolOutput
		raw, _ := json.Marshal(result.StructuredContent)
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatal(err)
		}
		direct, directErr, directCode := runEcho(t, req.Params.Arguments["args"])
		if out.Stdout != direct || out.ExitCode != directCode || out.Stderr != directErr {
			t.Fatalf("run_tool %+v != direct %q/%d", out, direct, directCode)
		}
		back, _, code := runTool(t, t.TempDir(), call, "-f", "mcp-call", "-t", "bash")
		if code != 0 || back != line {
			t.Fatalf("mcp-call -> bash: %q (want %q)", back, line)
		}
	}
}

func runEcho(t *testing.T, args any) (string, string, int) {
	t.Helper()
	var argv []string
	for _, a := range args.([]any) {
		argv = append(argv, a.(string))
	}
	var out, errb strings.Builder
	rc := &tool.RunContext{Ctx: context.Background(), Dir: t.TempDir(), Stdio: tool.Stdio{Out: &out, Err: &errb, In: strings.NewReader("")}}
	code := echoArgvTool.Run(rc, argv)
	return out.String(), errb.String(), code
}
