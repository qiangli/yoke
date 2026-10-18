package lens

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/qiangli/coreutils/tool"
	"mvdan.cc/sh/v3/syntax"
)

// hostile is the argv every dialect must carry back unchanged (the plan's
// list — spaces, quotes, %, ^, !, $, newline, empty, non-ASCII — and the
// characters each dialect treats specially).
var hostile = []string{
	"plain", "two words", "tab\there", "a  double  space", " leading", "trailing ",
	"sq'uote", `dq"uote`, `it's "both"`, `back\slash`, `trail\`, `\"slashquote`, `end\\`,
	"%PATH%", "100%", "%", "car^et", "^", "bang!", "!delayed!", "$HOME", "${x}", "$(pwd)",
	"`tick`", "a&b|c<d>e", "(paren)", "semi;colon", "*glob?[x]", "~tilde", "#hash",
	"{a,b}", "{1..3}", "k=v", "-flag", "--long=v", "--", "-", "", "@at", ",comma", "{args}",
	"non-ascii é 日本語", "emoji 🙂", "ctrl\x01char", "esc\x1b[0m", "new\nline", "cr\rlf",
	`C:\Program Files\x`, "/c/Users/x", "--%", "--% rest",
}

func without(args []string, drop func(string) bool) []string {
	var out []string
	for _, a := range args {
		if !drop(a) {
			out = append(out, a)
		}
	}
	return out
}

func hasLineBreak(s string) bool { return strings.ContainsAny(s, "\n\r") }

func hasControl(s string) bool {
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}

// hostileFor is the hostile set a dialect can carry: cmd.exe has no line
// breaks and POSIX has no escape for control characters.
func hostileFor(format string) []string {
	switch format {
	case "cmd":
		return without(hostile, hasLineBreak)
	case "posix":
		return without(hostile, hasControl)
	}
	return hostile
}

func argvConvert(t *testing.T, from, to string, data []byte, opt ArgvOptions) Result {
	t.Helper()
	req := Request{From: from, To: to, Data: data, Argv: opt}
	res, err := Convert(context.Background(), req)
	if err != nil {
		t.Fatalf("%s -> %s: %v", from, to, err)
	}
	again, err := Convert(context.Background(), req)
	if err != nil || !bytes.Equal(res.Data, again.Data) {
		t.Fatalf("%s -> %s is not deterministic", from, to)
	}
	return res
}

func wantExit(t *testing.T, err error, want int) {
	t.Helper()
	var le *Error
	if !errors.As(err, &le) || le.Code != want {
		t.Fatalf("want exit %d, got %v", want, err)
	}
}

// TestArgvRoundTripEveryDialect renders the hostile argv into each dialect
// and parses it back through the public rows: argv → dialect → argv identity.
func TestArgvRoundTripEveryDialect(t *testing.T) {
	for _, format := range argvFormats {
		t.Run(format, func(t *testing.T) {
			argv := append([]string{"echo-argv"}, hostileFor(format)...)
			var l losses
			rendered, err := renderArgv(format, argv, false)
			if err != nil {
				t.Fatal(err)
			}
			back, err := parseArgv(format, rendered, &l)
			if err != nil {
				t.Fatalf("%s: %v\n%s", format, err, rendered)
			}
			if !slices.Equal(back, argv) {
				t.Fatalf("%s round trip\n got %q\nwant %q\nvia %s", format, back, argv, rendered)
			}
			if len(l.notes) != 0 {
				t.Fatalf("%s: unexpected losses %v", format, l.notes)
			}
			if format == "bash" || format == "json" || format == "yaml" {
				return // bash is the pivot below; the json/yaml pair is the data row
			}
			// The public path, both directions through bash.
			line := argvConvert(t, "bash", format, mustRender(t, "bash", argv), ArgvOptions{})
			if !bytes.Equal(line.Data, rendered) || line.Executable != Unverified || line.Row.Fidelity != Lossless {
				t.Fatalf("bash -> %s: %q executable=%q", format, line.Data, line.Executable)
			}
			bash := argvConvert(t, format, "bash", rendered, ArgvOptions{})
			if !bytes.Equal(bash.Data, mustRender(t, "bash", argv)) {
				t.Fatalf("%s -> bash: %q", format, bash.Data)
			}
		})
	}
}

func mustRender(t *testing.T, format string, argv []string) []byte {
	t.Helper()
	out, err := renderArgv(format, argv, false)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// TestArgvUnrepresentable: what a dialect cannot carry is an error, not a guess.
func TestArgvUnrepresentable(t *testing.T) {
	for _, c := range []struct {
		to   string
		argv []string
		msg  string
	}{
		{"cmd", []string{"echo", "new\nline"}, "line break"},
		{"cmd", []string{"echo", "cr\r"}, "line break"},
		{"posix", []string{"echo", "ctrl\x01"}, "not representable in posix"},
		{"bash", []string{"echo", "nul\x00"}, "not representable in bash"},
		{"pwsh", []string{"echo", "bad\xff"}, "not valid UTF-8"},
		{"cmd", []string{"echo", "bad\xff"}, "not valid UTF-8"},
	} {
		_, err := renderArgv(c.to, c.argv, false)
		if err == nil || !strings.Contains(err.Error(), c.msg) {
			t.Errorf("%s %q: %v", c.to, c.argv, err)
		}
		wantExit(t, err, ExitInvalid)
	}
}

func TestShellGoldens(t *testing.T) {
	got, err := renderShell([]string{"echo", "two words", "it's", "", "new\nline", "$HOME"}, syntax.LangBash)
	if err != nil || string(got) != `echo 'two words' 'it'\''s' '' $'new\nline' '$HOME'`+"\n" {
		t.Fatalf("bash: %q %v", got, err)
	}
	bsh, _ := renderShell([]string{"echo", "it's"}, syntax.LangBashPP)
	if string(bsh) != string(mustRender(t, "bash", []string{"echo", "it's"})) {
		t.Fatalf("bsh quotes like bash: %q", bsh)
	}
	posix, _ := renderShell([]string{"echo", "it's"}, syntax.LangPOSIX)
	if string(posix) != `echo "it's"`+"\n" {
		t.Fatalf("posix: %q", posix)
	}
}

func TestShellParseLiteralOnly(t *testing.T) {
	argv, err := parseShell([]byte(`ls -l "my file" 'x'y a\ b "q\"q" $'t\tab' "\$lit" -- # comment`+"\n"), syntax.LangBash)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"ls", "-l", "my file", "xy", "a b", `q"q`, "t\tab", "$lit", "--"}
	if !slices.Equal(argv, want) {
		t.Fatalf("got %q want %q", argv, want)
	}
	for _, c := range []struct{ in, msg string }{
		{"ls $HOME", "parameter expansion"},
		{"ls $(pwd)", "command substitution"},
		{"ls $((1+2))", "arithmetic expansion"},
		{`ls "a $HOME"`, "inside double quotes"},
		{"ls *.go", "glob"},
		{"ls ~/x", "tilde"},
		{"ls {a,b}", "brace expansion"},
		{"ls a > b", "redirections"},
		{"ls; pwd", "exactly one command"},
		{"ls\npwd", "exactly one command"},
		{"ls | wc", "one simple command"},
		{"FOO=1 ls", "environment assignments"},
		{"! ls", "one simple command"},
		{"ls &", "one simple command"},
		{"if x; then y; fi", "one simple command"},
		{"", "empty command line"},
		{"ls 'unterminated", "invalid bash input"},
	} {
		_, err := parseArgv("bash", []byte(c.in), &losses{})
		if err == nil || !strings.Contains(err.Error(), c.msg) {
			t.Errorf("%q: %v (want %q)", c.in, err, c.msg)
		}
		wantExit(t, err, ExitInvalid)
	}
}

func TestCmdLineGoldens(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"plain", "plain"},
		{"", `""`},
		{"a b", `"a b"`},
		{`a"b`, `"a\"b"`},
		{`a\`, `a\`},
		{`a b\`, `"a b\\"`},
		{`\"x`, `"\\\"x"`},
		{"tab\there", "\"tab\there\""},
	} {
		if got := crtQuote(c.in); got != c.want {
			t.Errorf("crtQuote(%q) = %s, want %s", c.in, got, c.want)
		}
	}
	got, err := renderCmd([]string{"echo", "two words", "%PATH%", "a&b", "it's", `say "hi"`, "100%", "(x)", "!d!"})
	want := `echo ^"two words^" ^%PATH^% a^&b it's ^"say \^"hi\^"^" 100^% ^(x^) ^!d^!` + "\n"
	if err != nil || string(got) != want {
		t.Fatalf("cmd:\n got %s\nwant %s", got, want)
	}
	if cmdLineFor(string(got)) != "cmd.exe /d /c "+strings.TrimSuffix(want, "\n") {
		t.Fatal(cmdLineFor(string(got)))
	}
}

func TestCmdParseHandWritten(t *testing.T) {
	for _, c := range []struct {
		in   string
		want []string
	}{
		{`echo "a b" c\d "e\"f" ""`, []string{"echo", "a b", `c\d`, `e"f`, ""}},
		{`echo ^"quoted^" x`, []string{"echo", "quoted", "x"}},
		{`echo "a^b" a^^b`, []string{"echo", "a^b", "a^b"}},
		{`echo "a&b|c" ^&`, []string{"echo", "a&b|c", "&"}},
		{"echo  a\t b \r\n", []string{"echo", "a", "b"}},
		{`echo "a""b" "c" "" d\\"e f"`, []string{"echo", `a"b`, "c", "", `d\e f`}},
		{`echo trailing^`, []string{"echo", "trailing"}},
	} {
		got, err := parseCmd(c.in)
		if err != nil || !slices.Equal(got, c.want) {
			t.Errorf("%s: %q %v (want %q)", c.in, got, err, c.want)
		}
	}
	for _, in := range []string{"echo a & b", "echo a | b", "echo a > b", "echo (x)", "echo a\necho b"} {
		_, err := parseCmd(in)
		if err == nil {
			t.Errorf("%q parsed", in)
		}
		wantExit(t, err, ExitInvalid)
	}
}

func TestPwshGoldensAndParse(t *testing.T) {
	if got := renderPwsh([]string{"x", "--%", "--% rest"}); got != "& 'x' ([System.Text.StringBuilder]'--%') '--% rest'" {
		t.Errorf("literal --%% must not be a plain string argument: %q", got)
	}
	if got := renderPwsh([]string{"echo", "it's", "", "a b", "new\nline"}); got != "& 'echo' 'it''s' '' 'a b' 'new\nline'" {
		t.Fatalf("pwsh: %q", got)
	}
	for _, c := range []struct {
		in   string
		want []string
	}{
		{"echo \"a`\"b\" c'd' e `t", []string{"echo", `a"b`, "cd", "e", "\t"}},
		{"& 'x y' \"t`tab\" \"\"\"q\"\"\"", []string{"x y", "t\tab", `"q"`}},
		{"&'cmd' 'a'", nil}, // & glued to a word is not the call operator
		{"  echo   plain  \r\n", []string{"echo", "plain"}},
		{"echo 'a''b'c\"d\"", []string{"echo", "a'bcd"}},
	} {
		got, err := parsePwsh(c.in)
		if c.want == nil {
			if err == nil {
				t.Errorf("%q parsed as %q", c.in, got)
			}
			continue
		}
		if err != nil || !slices.Equal(got, c.want) {
			t.Errorf("%q: %q %v (want %q)", c.in, got, err, c.want)
		}
	}
	for _, c := range []struct{ in, msg string }{
		{"echo $x", "PowerShell syntax"},
		{`echo "$x"`, "expands"},
		{"echo a; b", "PowerShell syntax"},
		{"echo a | b", "PowerShell syntax"},
		{"echo (x)", "PowerShell syntax"},
		{"echo --% raw", "--%"},
		{"echo 'open", "unterminated single"},
		{`echo "open`, "unterminated double"},
		{"echo a\necho b", "exactly one command line"},
	} {
		_, err := parsePwsh(c.in)
		if err == nil || !strings.Contains(err.Error(), c.msg) {
			t.Errorf("%q: %v (want %q)", c.in, err, c.msg)
		}
		wantExit(t, err, ExitInvalid)
	}
}

func TestRecordRows(t *testing.T) {
	argv := []string{"/usr/bin/git", "status", "--short"}
	js := mustRender(t, "json", argv)
	if string(js) != "{\n  \"name\": \"git\",\n  \"kind\": \"command\",\n  \"exec\": [\n    \"/usr/bin/git\",\n    \"status\",\n    \"--short\"\n  ]\n}\n" {
		t.Fatalf("json: %s", js)
	}
	ya := mustRender(t, "yaml", argv)
	if string(ya) != "name: git\nkind: command\nexec:\n  - /usr/bin/git\n  - status\n  - --short\n" {
		t.Fatalf("yaml: %s", ya)
	}
	// json -> yaml is the data row and still a valid record.
	res := convert(t, "json", "yaml", js)
	if res.Row.IsArgv() || !bytes.Equal(res.Data, ya) {
		t.Fatalf("json -> yaml via data row: %s", res.Data)
	}
	// A richer record: other fields are reported dropped.
	var l losses
	got, err := parseRecordJSON([]byte(`{"name":"g","kind":"command","exec":["git","log"],"synopsis":"x","effects":["read"]}`), &l)
	if err != nil || !slices.Equal(got, []string{"git", "log"}) || len(l.notes) != 1 || !strings.Contains(l.notes[0], "effects, synopsis") {
		t.Fatalf("json record: %q %v %v", got, err, l.notes)
	}
	l = losses{}
	got, err = parseRecordYAML([]byte("name: g\nexec: [git, log]\nsynopsis: x\n"), &l)
	if err != nil || !slices.Equal(got, []string{"git", "log"}) || len(l.notes) != 1 {
		t.Fatalf("yaml record: %q %v %v", got, err, l.notes)
	}
	for _, c := range []struct{ from, in, msg string }{
		{"json", `{"name":"x","script":"echo hi"}`, "script body"},
		{"json", `{"name":"x"}`, "no exec argv"},
		{"json", `{"name":"x","kind":"model","exec":["a"]}`, "not a command"},
		{"json", `["ls","-l"]`, "not a command record"},
		{"json", `{"bogus":1,"exec":["a"]}`, "not a command record"},
		{"yaml", "- ls\n- -l\n", "want a mapping"},
		{"yaml", "name: x\nbogus: 1\nexec: [a]\n", "not a command record"},
	} {
		_, err := parseArgv(c.from, []byte(c.in), &losses{})
		if err == nil || !strings.Contains(err.Error(), c.msg) {
			t.Errorf("%s %s: %v (want %q)", c.from, c.in, err, c.msg)
		}
		wantExit(t, err, ExitInvalid)
	}
}

func TestMCPRows(t *testing.T) {
	argv := []string{"echo", "-n", "hi there"}
	call := mustRender(t, "mcp-call", argv)
	var c tool.MCPToolCall
	if err := json.Unmarshal(call, &c); err != nil {
		t.Fatal(err)
	}
	if c.JSONRPC != "2.0" || c.Method != "tools/call" || c.Params.Name != "run_tool" ||
		c.Params.Arguments["name"] != "echo" || len(c.Params.Arguments) != 2 {
		t.Fatalf("mcp-call: %s", call)
	}
	doc := mustRender(t, "mcp-tool", argv)
	var d tool.MCPCommandDocumentation
	if err := json.Unmarshal(doc, &d); err != nil {
		t.Fatal(err)
	}
	if d.Tool.Name != "echo" || d.Call.Params.Name != "echo" || d.Tool.InputSchema["type"] != "object" {
		t.Fatalf("mcp-tool: %s", doc)
	}
	// Both call shapes parse; stdin/dir/env are reported dropped.
	var l losses
	got, err := parseMCPCall([]byte(`{"jsonrpc":"2.0","method":"tools/call","params":{"name":"echo","arguments":{"args":["a","b"],"stdin":"x","dir":"/tmp"}}}`), &l)
	if err != nil || !slices.Equal(got, []string{"echo", "a", "b"}) || len(l.notes) != 1 || !strings.Contains(l.notes[0], "dir, stdin") {
		t.Fatalf("direct call: %q %v %v", got, err, l.notes)
	}
	got, err = parseMCPCall([]byte(`{"params":{"name":"run_tool","arguments":{"name":"ls","args":[]}}}`), &losses{})
	if err != nil || !slices.Equal(got, []string{"ls"}) {
		t.Fatalf("run_tool call: %q %v", got, err)
	}
	got, err = parseMCPCall([]byte(`{"params":{"name":"ls"}}`), &losses{})
	if err != nil || !slices.Equal(got, []string{"ls"}) {
		t.Fatalf("bare call: %q %v", got, err)
	}
	for _, c := range []struct{ in, msg string }{
		{`{"method":"tools/list","params":{"name":"x"}}`, "not tools/call"},
		{`{"params":{"arguments":{"args":["a"]}}}`, "no tool name"},
		{`{"params":{"name":"run_tool","arguments":{"args":["a"]}}}`, "no command name"},
		{`{"params":{"name":"ls","arguments":{"args":"a"}}}`, "not an array"},
		{`{"params":{"name":"ls","arguments":{"args":[1]}}}`, "not a string"},
		{`{"params":{"name":"ls","arguments":{"args":["a"],"long":true}}}`, "mixes args"},
		{`{"params":{"name":"ls","arguments":{"long":true}}}`, "no schema"},
		{`{"params":{"name":"ls"}} trailing`, "trailing data"},
	} {
		_, err := parseMCPCall([]byte(c.in), &losses{})
		if err == nil || !strings.Contains(err.Error(), c.msg) {
			t.Errorf("%s: %v (want %q)", c.in, err, c.msg)
		}
		wantExit(t, err, ExitInvalid)
	}
}

var fixtureSchema = tool.ArgSchema{
	Positionals: []tool.ArgParameter{{Name: "path"}},
	Flags:       []tool.ArgFlag{{Name: "depth", Type: "int"}, {Name: "long", Type: "bool"}},
}

func withSchema(t *testing.T) {
	t.Helper()
	prev := SchemaFor
	SchemaFor = func(name string) (tool.ArgSchema, bool) {
		if name == "lsx" {
			return fixtureSchema, true
		}
		return tool.ArgSchema{}, false
	}
	t.Cleanup(func() { SchemaFor = prev })
}

// TestSchemaVerified: with a host schema, an argv the schema reproduces
// exactly is schema-verified and mcp-tool goes typed; anything else stays
// literal and unverified.
func TestSchemaVerified(t *testing.T) {
	withSchema(t)
	in := []byte("lsx --depth 2 --long src\n")
	res := argvConvert(t, "bash", "mcp-tool", in, ArgvOptions{})
	if res.Executable != SchemaVerified {
		t.Fatalf("executable %q", res.Executable)
	}
	var d tool.MCPCommandDocumentation
	if err := json.Unmarshal(res.Data, &d); err != nil {
		t.Fatal(err)
	}
	props := d.Tool.InputSchema["properties"].(map[string]any)
	if _, ok := props["args"]; ok || props["depth"] == nil || d.Call.Params.Arguments["path"] != "src" || d.Call.Params.Arguments["long"] != true {
		t.Fatalf("typed mcp-tool: %s", res.Data)
	}
	back := argvConvert(t, "mcp-tool", "bash", res.Data, ArgvOptions{})
	if !bytes.Equal(back.Data, in) || back.Executable != SchemaVerified {
		t.Fatalf("typed -> bash: %q %q", back.Data, back.Executable)
	}
	// A typed mcp-call to a tool with a schema binds through ArgSchema.Argv.
	typed := argvConvert(t, "mcp-call", "bash", []byte(`{"params":{"name":"lsx","arguments":{"path":"src","depth":2,"long":true}}}`), ArgvOptions{})
	if !bytes.Equal(typed.Data, in) {
		t.Fatalf("typed call: %q", typed.Data)
	}
	for _, line := range []string{"lsx --long --depth 2 src", "lsx -l src", "lsx --depth=2 src", "lsx --depth 2 src extra", "lsx --depth x src", "other --long"} {
		res := argvConvert(t, "bash", "mcp-tool", []byte(line), ArgvOptions{})
		if res.Executable != Unverified {
			t.Errorf("%q verified", line)
		}
		if err := json.Unmarshal(res.Data, &d); err != nil {
			t.Fatal(err)
		}
		if _, ok := d.Tool.InputSchema["properties"].(map[string]any)["args"]; !ok {
			t.Errorf("%q: literal args expected: %s", line, res.Data)
		}
	}
}

func TestSkillRow(t *testing.T) {
	argv := []string{"bashy", "kb", "search", "lens rows"}
	got := mustRender(t, "skill", argv)
	if string(got) != "```bsh\nbashy kb search 'lens rows'\n```\n" {
		t.Fatalf("skill: %q", got)
	}
	doc := "# Skill\n\n```json\n{\"not\": \"this\"}\n```\n\nRun:\n\n````bash title=x\ngit status --short\n```\nstill body? no: four backticks close\n````\n\n```bsh\nignored second\n```\n"
	back, err := parseSkill([]byte(doc))
	if err == nil {
		t.Fatalf("four-backtick body has two statements: %q", back)
	}
	doc = "text\n```sh\n  ls -l 'a b'\n  ```\n"
	back, err = parseSkill([]byte(doc))
	if err != nil || !slices.Equal(back, []string{"ls", "-l", "a b"}) {
		t.Fatalf("indented close: %q %v", back, err)
	}
	for _, c := range []struct{ in, msg string }{
		{"no fence here\n", "no ```bsh fence"},
		{"```json\n{}\n```\n", "no ```bsh fence"},
		{"```bsh\nls\n", "unterminated"},
		{"```bsh\nls\npwd\n```\n", "exactly one command"},
	} {
		_, err := parseSkill([]byte(c.in))
		if err == nil || !strings.Contains(err.Error(), c.msg) {
			t.Errorf("%q: %v (want %q)", c.in, err, c.msg)
		}
	}
}

func TestSwitchAndDrive(t *testing.T) {
	argv := []string{"cp", "--dry-run", "-r", "--depth=2", "-1", "--", "--not"}
	if got := applySwitch("slash", argv); !slices.Equal(got, []string{"cp", "/dry-run", "/r", "/depth:2", "-1", "--", "--not"}) {
		t.Fatalf("slash: %q", got)
	}
	if got := applySwitch("pascal", argv); !slices.Equal(got, []string{"cp", "-DryRun", "-r", "-Depth:2", "-1", "--", "--not"}) {
		t.Fatalf("pascal: %q", got)
	}
	if got := applySwitch("keep", argv); !slices.Equal(got, argv) {
		t.Fatalf("keep: %q", got)
	}
	if pascalCase("no_input") != "NoInput" || pascalCase("x") != "X" {
		t.Fatal("pascalCase")
	}
	paths := []string{"/c/tools/cp", "/c/src", "--dir=/c/dst", "/d/x", "/cx", "rel/c/x"}
	if got := applyDrive("C", paths); !slices.Equal(got, []string{`C:\tools\cp`, `C:\src`, `--dir=C:\dst`, "/d/x", "/cx", "rel/c/x"}) {
		t.Fatalf("drive C: %q", got)
	}
	if got := applyDrive("CD", paths); got[3] != `D:\x` {
		t.Fatalf("drive CD: %q", got)
	}
	res := argvConvert(t, "bash", "pwsh", []byte("cp -r --dry-run /c/src /c/dst\n"), ArgvOptions{Switch: "pascal", Drive: "c:"})
	if string(res.Data) != `& 'cp' '-r' '-DryRun' 'C:\src' 'C:\dst'`+"\n" {
		t.Fatalf("pwsh pascal drive: %q", res.Data)
	}
	res = argvConvert(t, "bash", "cmd", []byte("cp -r --dry-run /c/src /c/dst\n"), ArgvOptions{Switch: "slash", Drive: "C"})
	if string(res.Data) != `cp /r /dry-run C:\src C:\dst`+"\n" {
		t.Fatalf("cmd slash drive: %q", res.Data)
	}
	// The knobs are usage errors when misspelled or off a command-line row.
	_, err := Convert(context.Background(), Request{From: "bash", To: "cmd", Data: []byte("ls"), Argv: ArgvOptions{Switch: "camel"}})
	wantExit(t, err, ExitUsage)
	_, err = Convert(context.Background(), Request{From: "bash", To: "cmd", Data: []byte("ls"), Argv: ArgvOptions{Drive: "1"}})
	wantExit(t, err, ExitUsage)
	_, err = Convert(context.Background(), Request{From: "json", To: "yaml", Data: []byte("{}"), Argv: ArgvOptions{Switch: "keep"}})
	wantExit(t, err, ExitUsage)
}

func TestArgvTableAndDetection(t *testing.T) {
	n := 0
	for _, r := range Rows() {
		if r.IsArgv() {
			n++
			if r.Fidelity != Lossless || r.Via() != "go" || !r.Installed() {
				t.Fatalf("row %+v", r)
			}
		}
	}
	if n != 10*9-2 {
		t.Fatalf("%d argv rows", n)
	}
	if r, err := Route("json", "yaml"); err != nil || r.IsArgv() {
		t.Fatal("json -> yaml must stay the data row")
	}
	if r, err := Route("json", "bash"); err != nil || !r.IsArgv() {
		t.Fatal("json -> bash is an argv row")
	}
	if _, err := Route("pwsh", "toml"); err == nil {
		t.Fatal("no route pwsh -> toml")
	}
	for _, name := range []string{"x.sh", "x.bash", "x.cmd", "x.bat", "x.ps1", "x.pwsh", "x.skill", "x.mcp-call"} {
		if f := Detect(name, []byte("echo hi\n")); f != "" {
			t.Errorf("%s detected as %s: command lines need -f", name, f)
		}
	}
	for in, want := range map[string]string{"sh": "posix", "powershell": "pwsh", "bashpp": "bsh", "mcp-call": "mcp-call", "CMD": "cmd"} {
		if Canonical(in) != want {
			t.Errorf("Canonical(%s) = %s", in, Canonical(in))
		}
	}
}
