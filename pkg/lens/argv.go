package lens

// The command-line (argv) rows (Sprint 446 S4). A command line is one
// literal argv — the words a program receives, no expansion, no execution —
// and every row here changes only how that argv is spelled:
//
//	bash | posix | bsh   a shell word list (sh/syntax.Quote per dialect)
//	cmd                  a cmd.exe command line (CRT quoting + ^ escapes)
//	pwsh                 a PowerShell command line (& 'word' 'word')
//	json | yaml          a fleet.Command record: name, kind, exec
//	mcp-tool             tool.MCPCommandDocumentation (tools/list entry + call)
//	mcp-call             a tools/call to run_tool, the one every bashy MCP
//	                     server has (name + args), as tool.MCPToolCall
//	skill                a snippet.Snippet rendered as a ```bsh fence
//
// Round trips are lossless: argv → dialect → argv is identity on anything a
// dialect can carry, and what it cannot carry (a newline on a cmd.exe line, a
// control character in POSIX) is an ExitInvalid error, never a guess. A
// command line is never detected: these formats need -f.
//
// Rendering never runs anything and never consults the environment. The two
// agent-chosen knobs are ArgvOptions.Switch (option spelling for a Windows
// peer) and ArgvOptions.Drive (POSIX drive paths to native spelling through
// pathconv.NativeArgsIn). A rendered argv is reported as executable
// "schema-verified" only when the host supplied a schema (SchemaFor) that
// binds the argv and reproduces it exactly; otherwise "unverified".

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/qiangli/coreutils/tool"
	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/snippet"
	"gopkg.in/yaml.v3"
	"mvdan.cc/sh/v3/pathconv"
	"mvdan.cc/sh/v3/syntax"
)

// The argv formats, in the order their rows are declared.
var argvFormats = []string{"bash", "posix", "bsh", "cmd", "pwsh", "json", "yaml", "mcp-tool", "mcp-call", "skill"}

// argvOnly are the formats that exist only as command lines: never detected
// from a name or content, never a data row.
var argvOnly = map[string]bool{"bash": true, "posix": true, "bsh": true, "cmd": true, "pwsh": true, "mcp-tool": true, "mcp-call": true, "skill": true}

// IsArgvFormat reports whether a canonical format name is a command-line
// dialect (json and yaml count: the record rows read and write argv).
func IsArgvFormat(name string) bool { return slices.Contains(argvFormats, name) }

// Executable verdicts reported by Result.Executable for argv rows.
const (
	SchemaVerified = "schema-verified"
	Unverified     = "unverified"
)

// ArgvOptions are the agent-chosen knobs of the argv rows. Both apply after
// parsing and before rendering, to the whole argv.
type ArgvOptions struct {
	// Switch respells option words: "" or "keep" leaves them; "slash" turns
	// --name / --name=value / -x into /name / /name:value / /x; "pascal"
	// turns --dry-run / --name=value into -DryRun / -Name:value and leaves
	// short options alone. Words after a bare "--" are never touched.
	Switch string
	// Drive lists the drive letters present on the target ("C", "CD", "C:"):
	// a POSIX drive path of one of them (/c/Users/x, --dir=/c/x) becomes its
	// native spelling (C:\Users\x) through pathconv.NativeArgsIn.
	Drive string
}

func (o ArgvOptions) empty() bool { return o.Switch == "" && o.Drive == "" }

// SchemaFor is the host's typed-schema seam: given a command name it returns
// the argument schema the host derived for it (bashy: tool.SchemaFromFlags
// and the cobra walker). nil — the default in this module — means no command
// is schema-verified and mcp-tool/mcp-call stay on the literal args form.
var SchemaFor func(name string) (tool.ArgSchema, bool)

// argvRow builds the FROM→TO converter of one argv pair.
func argvRow(from, to string) func([]byte, ArgvOptions, *losses) ([]byte, string, error) {
	return func(data []byte, opt ArgvOptions, l *losses) ([]byte, string, error) {
		if err := validateArgvOptions(opt); err != nil {
			return nil, "", err
		}
		argv, err := parseArgv(from, data, l)
		if err != nil {
			return nil, "", err
		}
		argv = applyDrive(opt.Drive, argv)
		argv = applySwitch(opt.Switch, argv)
		exe := verifyExecutable(argv)
		out, err := renderArgv(to, argv, exe == SchemaVerified)
		return out, exe, err
	}
}

func validateArgvOptions(opt ArgvOptions) error {
	switch opt.Switch {
	case "", "keep", "slash", "pascal":
	default:
		return errorf(ExitUsage, "--switch=%s: want keep, slash or pascal", opt.Switch)
	}
	if opt.Drive != "" && pathconv.DrivesOf(opt.Drive) == 0 {
		return errorf(ExitUsage, "--drive=%s: want drive letters such as C or CD", opt.Drive)
	}
	return nil
}

// ---- dispatch ---------------------------------------------------------------

func parseArgv(from string, data []byte, l *losses) ([]string, error) {
	var argv []string
	var err error
	switch from {
	case "bash":
		argv, err = parseShell(data, syntax.LangBash)
	case "posix":
		argv, err = parseShell(data, syntax.LangPOSIX)
	case "bsh":
		argv, err = parseShell(data, syntax.LangBashPP)
	case "cmd":
		argv, err = parseCmd(string(data))
	case "pwsh":
		argv, err = parsePwsh(string(data))
	case "json":
		argv, err = parseRecordJSON(data, l)
	case "yaml":
		argv, err = parseRecordYAML(data, l)
	case "mcp-tool":
		argv, err = parseMCPTool(data, l)
	case "mcp-call":
		argv, err = parseMCPCall(data, l)
	case "skill":
		argv, err = parseSkill(data)
	default:
		return nil, errorf(ExitNoRoute, "no argv parser for %s", from)
	}
	if err != nil {
		return nil, err
	}
	if len(argv) == 0 {
		return nil, errorf(ExitInvalid, "invalid %s input: empty command line", from)
	}
	return argv, nil
}

func renderArgv(to string, argv []string, typed bool) ([]byte, error) {
	switch to {
	case "bash":
		return renderShell(argv, syntax.LangBash)
	case "posix":
		return renderShell(argv, syntax.LangPOSIX)
	case "bsh":
		return renderShell(argv, syntax.LangBashPP)
	case "cmd":
		return renderCmd(argv)
	case "pwsh":
		if err := validUTF8(argv); err != nil {
			return nil, err
		}
		return []byte(renderPwsh(argv) + "\n"), nil
	case "json":
		return renderRecordJSON(argv)
	case "yaml":
		return renderRecordYAML(argv)
	case "mcp-tool":
		return renderMCPTool(argv, typed)
	case "mcp-call":
		return renderMCPCall(argv)
	case "skill":
		return renderSkill(argv)
	}
	return nil, errorf(ExitNoRoute, "no argv renderer for %s", to)
}

// ---- shell dialects (bash / posix / bsh) -------------------------------------

func renderShell(argv []string, lang syntax.LangVariant) ([]byte, error) {
	words := make([]string, len(argv))
	for i, a := range argv {
		q, err := syntax.Quote(a, lang)
		if err != nil {
			return nil, errorf(ExitInvalid, "argument %d is not representable in %s: %v", i, lang, err)
		}
		words[i] = q
	}
	return []byte(strings.Join(words, " ") + "\n"), nil
}

// parseShell reads exactly one simple command whose words are all literal.
// Nothing is expanded: a parameter, command substitution, arithmetic, brace,
// glob or tilde that would need the shell is refused, because lens has no
// environment and a guessed value would be a different argv.
func parseShell(data []byte, lang syntax.LangVariant) ([]string, error) {
	p := syntax.NewParser(syntax.Variant(lang))
	f, err := p.Parse(bytes.NewReader(data), "")
	if err != nil {
		return nil, errorf(ExitInvalid, "invalid %s input: %v", lang, err)
	}
	if len(f.Stmts) == 0 {
		return nil, nil // reported as an empty command line by parseArgv
	}
	if len(f.Stmts) != 1 {
		return nil, errorf(ExitInvalid, "invalid %s input: want exactly one command, got %d", lang, len(f.Stmts))
	}
	st := f.Stmts[0]
	if st.Negated || st.Background || st.Coprocess || len(st.Redirs) > 0 {
		return nil, errorf(ExitInvalid, "invalid %s input: one simple command without redirections", lang)
	}
	call, ok := st.Cmd.(*syntax.CallExpr)
	if !ok {
		return nil, errorf(ExitInvalid, "invalid %s input: one simple command, not a %T", lang, st.Cmd)
	}
	if len(call.Assigns) > 0 {
		return nil, errorf(ExitInvalid, "invalid %s input: environment assignments are not part of an argv", lang)
	}
	argv := make([]string, 0, len(call.Args))
	for _, w := range call.Args {
		s, err := literalWord(w, lang)
		if err != nil {
			return nil, errorf(ExitInvalid, "invalid %s input: %v", lang, err)
		}
		argv = append(argv, s)
	}
	return argv, nil
}

func literalWord(w *syntax.Word, lang syntax.LangVariant) (string, error) {
	if lang != syntax.LangPOSIX {
		syntax.SplitBraces(w) // so {a,b} and {1..3} surface as BraceExp parts
	}
	var b strings.Builder
	for i, part := range w.Parts {
		switch p := part.(type) {
		case *syntax.Lit:
			if i == 0 && strings.HasPrefix(p.Value, "~") {
				return "", fmt.Errorf("%s: tilde expansion is not literal (quote it)", wordText(w))
			}
			if err := unquotedLit(&b, p.Value, lang); err != nil {
				return "", err
			}
		case *syntax.SglQuoted:
			if p.Dollar {
				ansiC(&b, p.Value)
			} else {
				b.WriteString(p.Value)
			}
		case *syntax.DblQuoted:
			for _, in := range p.Parts {
				lit, ok := in.(*syntax.Lit)
				if !ok {
					return "", fmt.Errorf("%s: %s inside double quotes is not literal", wordText(w), partName(in))
				}
				dblQuotedLit(&b, lit.Value)
			}
		default:
			return "", fmt.Errorf("%s: %s is not literal (lens does not expand)", wordText(w), partName(part))
		}
	}
	return b.String(), nil
}

// wordText is the word as written, for messages (Word.Lit is empty on expansions).
func wordText(w *syntax.Word) string {
	var b bytes.Buffer
	if err := syntax.NewPrinter().Print(&b, w); err != nil {
		return w.Lit()
	}
	return b.String()
}

func partName(p syntax.WordPart) string {
	switch p.(type) {
	case *syntax.ParamExp:
		return "a parameter expansion"
	case *syntax.CmdSubst:
		return "a command substitution"
	case *syntax.ArithmExp:
		return "an arithmetic expansion"
	case *syntax.ProcSubst:
		return "a process substitution"
	case *syntax.ExtGlob:
		return "an extended glob"
	case *syntax.BraceExp:
		return "a brace expansion"
	}
	return fmt.Sprintf("%T", p)
}

// unquotedLit applies quote removal to an unquoted literal: \X is X, and a
// glob character is refused since the shell would expand it.
func unquotedLit(b *strings.Builder, s string, lang syntax.LangVariant) error {
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch c {
		case '\\':
			if i+1 < len(s) {
				i++
				b.WriteByte(s[i])
			}
			continue
		case '*', '?', '[':
			return fmt.Errorf("%s: unquoted %c is a glob, not a literal word", s, c)
		}
		b.WriteByte(c)
	}
	return nil
}

// dblQuotedLit applies quote removal inside double quotes: only \" \\ \$ \`
// and \<newline> are escapes; any other backslash stays.
func dblQuotedLit(b *strings.Builder, s string) {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '\\' && i+1 < len(s) {
			switch s[i+1] {
			case '"', '\\', '$', '`':
				i++
				b.WriteByte(s[i])
				continue
			case '\n':
				i++
				continue
			}
		}
		b.WriteByte(c)
	}
}

// ansiC decodes the body of a $'…' word the way bash does; an unknown escape
// keeps its backslash.
func ansiC(b *strings.Builder, s string) {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c != '\\' || i+1 >= len(s) {
			b.WriteByte(c)
			continue
		}
		i++
		switch e := s[i]; e {
		case 'a':
			b.WriteByte('\a')
		case 'b':
			b.WriteByte('\b')
		case 'e', 'E':
			b.WriteByte(0x1b)
		case 'f':
			b.WriteByte('\f')
		case 'n':
			b.WriteByte('\n')
		case 'r':
			b.WriteByte('\r')
		case 't':
			b.WriteByte('\t')
		case 'v':
			b.WriteByte('\v')
		case '\\', '\'', '"', '?':
			b.WriteByte(e)
		case '0', '1', '2', '3', '4', '5', '6', '7':
			n, j := 0, i
			for ; j < len(s) && j < i+3 && s[j] >= '0' && s[j] <= '7'; j++ {
				n = n*8 + int(s[j]-'0')
			}
			b.WriteByte(byte(n))
			i = j - 1
		case 'x':
			n, j := 0, i+1
			for ; j < len(s) && j < i+3 && isHexDigit(s[j]); j++ {
				n = n*16 + hexVal(s[j])
			}
			if j == i+1 {
				b.WriteString(`\x`)
				continue
			}
			b.WriteByte(byte(n))
			i = j - 1
		case 'u', 'U':
			width := 4
			if e == 'U' {
				width = 8
			}
			n, j := 0, i+1
			for ; j < len(s) && j < i+1+width && isHexDigit(s[j]); j++ {
				n = n*16 + hexVal(s[j])
			}
			if j == i+1 {
				b.WriteByte('\\')
				b.WriteByte(e)
				continue
			}
			b.WriteRune(rune(n))
			i = j - 1
		default:
			b.WriteByte('\\')
			b.WriteByte(e)
		}
	}
}

func isHexDigit(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'
}

func hexVal(c byte) int {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0')
	case c >= 'a' && c <= 'f':
		return int(c-'a') + 10
	}
	return int(c-'A') + 10
}

// ---- fleet.Command records (json / yaml) -------------------------------------

// recordFields are the keys a rendered record carries; anything else on an
// input record is reported as dropped.
var recordFields = map[string]bool{"name": true, "kind": true, "exec": true}

func commandRecord(argv []string) fleet.Command {
	return fleet.Command{Name: baseName(argv[0]), Kind: fleet.KindCommand, Exec: append([]string(nil), argv...)}
}

// baseName is the last path segment of a program spelling, either separator.
func baseName(s string) string {
	if i := strings.LastIndexAny(s, `/\`); i >= 0 && i+1 < len(s) {
		return s[i+1:]
	}
	return s
}

func renderRecordJSON(argv []string) ([]byte, error) {
	c := commandRecord(argv)
	return marshalJSON(struct {
		Name string   `json:"name"`
		Kind string   `json:"kind"`
		Exec []string `json:"exec"`
	}{c.Name, c.Kind, c.Exec})
}

func renderRecordYAML(argv []string) ([]byte, error) {
	c := commandRecord(argv)
	var b bytes.Buffer
	enc := yaml.NewEncoder(&b)
	enc.SetIndent(2)
	if err := enc.Encode(&c); err != nil {
		return nil, err
	}
	return b.Bytes(), enc.Close()
}

func parseRecordJSON(data []byte, l *losses) ([]string, error) {
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(data, &keys); err != nil {
		return nil, errorf(ExitInvalid, "invalid json input: not a command record: %v", err)
	}
	var c fleet.Command
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return nil, errorf(ExitInvalid, "invalid json input: not a command record: %v", err)
	}
	var extra []string
	for k := range keys {
		if !recordFields[k] {
			extra = append(extra, k)
		}
	}
	return recordArgv(c, extra, l)
}

func parseRecordYAML(data []byte, l *losses) ([]string, error) {
	var node yaml.Node
	if err := yaml.Unmarshal(data, &node); err != nil {
		return nil, errorf(ExitInvalid, "invalid yaml input: %v", err)
	}
	if node.Kind != yaml.DocumentNode || len(node.Content) != 1 || node.Content[0].Kind != yaml.MappingNode {
		return nil, errorf(ExitInvalid, "invalid yaml input: not a command record (want a mapping)")
	}
	var extra []string
	m := node.Content[0]
	for i := 0; i+1 < len(m.Content); i += 2 {
		if k := m.Content[i].Value; !recordFields[k] {
			extra = append(extra, k)
		}
	}
	c, err := fleet.ParseCommand("", data, nil)
	if err != nil {
		return nil, errorf(ExitInvalid, "invalid yaml input: not a command record: %v", err)
	}
	return recordArgv(c, extra, l)
}

func recordArgv(c fleet.Command, extra []string, l *losses) ([]string, error) {
	if c.Kind != "" && c.Kind != fleet.KindCommand {
		return nil, errorf(ExitInvalid, "record kind %q is not a command", c.Kind)
	}
	if len(c.Exec) == 0 {
		if c.Script != "" {
			return nil, errorf(ExitInvalid, "record %q has a script body, not an exec argv", c.Name)
		}
		return nil, errorf(ExitInvalid, "record %q has no exec argv", c.Name)
	}
	if len(extra) > 0 {
		slices.Sort(extra)
		l.add("record fields other than name, kind and exec are dropped: %s", strings.Join(extra, ", "))
	}
	return c.Exec, nil
}

func marshalJSON(v any) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// ---- MCP (mcp-tool / mcp-call) -----------------------------------------------

// mcpDescription is the description the bashy MCP server gives a registered
// tool (synopsis — first usage line); empty for an unregistered name.
func mcpDescription(name string) string {
	t := LookupCommand(name)
	if t == nil {
		return ""
	}
	usage, _, _ := strings.Cut(t.Usage, "\n")
	return t.Synopsis + " — " + usage
}

func renderMCPTool(argv []string, typed bool) ([]byte, error) {
	name := argv[0]
	doc := tool.DocumentAsMCP(name, mcpDescription(name))
	doc.Call.Params.Arguments = map[string]any{"args": append([]string{}, argv[1:]...)}
	if typed {
		if schema, ok := SchemaFor(name); ok {
			if bound, ok := bindArgv(schema, argv[1:]); ok {
				doc = tool.DocumentAsMCPSchema(name, mcpDescription(name), schema)
				doc.Call.Params.Arguments = bound
			}
		}
	}
	return marshalJSON(doc)
}

// runToolCall is the tools/call every bashy MCP server accepts: run_tool with
// the command name and its literal args.
func runToolCall(argv []string) tool.MCPToolCall {
	var call tool.MCPToolCall
	call.JSONRPC = "2.0"
	call.Method = "tools/call"
	call.Params.Name = "run_tool"
	call.Params.Arguments = map[string]any{"name": argv[0], "args": append([]string{}, argv[1:]...)}
	return call
}

func renderMCPCall(argv []string) ([]byte, error) { return marshalJSON(runToolCall(argv)) }

func decodeMCP(data []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if _, err := dec.Token(); err != io.EOF {
		return errors.New("trailing data after the JSON value")
	}
	return nil
}

func parseMCPTool(data []byte, l *losses) ([]string, error) {
	var doc struct {
		Tool struct {
			Name string `json:"name"`
		} `json:"tool"`
		Call tool.MCPToolCall `json:"call"`
	}
	if err := decodeMCP(data, &doc); err != nil {
		return nil, errorf(ExitInvalid, "invalid mcp-tool input: %v", err)
	}
	if doc.Call.Params.Name == "" {
		if doc.Tool.Name == "" {
			return nil, errorf(ExitInvalid, "invalid mcp-tool input: no tool name")
		}
		return []string{doc.Tool.Name}, nil
	}
	return callArgv(doc.Call, l)
}

func parseMCPCall(data []byte, l *losses) ([]string, error) {
	var call tool.MCPToolCall
	if err := decodeMCP(data, &call); err != nil {
		return nil, errorf(ExitInvalid, "invalid mcp-call input: %v", err)
	}
	return callArgv(call, l)
}

// callArgv recovers the argv of a tools/call: run_tool's name + args, or a
// direct tool's args, or — with a host schema — a direct tool's typed
// arguments through ArgSchema.Argv. stdin/dir/env are not argv and are
// reported dropped.
func callArgv(call tool.MCPToolCall, l *losses) ([]string, error) {
	if call.Method != "" && call.Method != "tools/call" {
		return nil, errorf(ExitInvalid, "mcp method %q is not tools/call", call.Method)
	}
	name := call.Params.Name
	if name == "" {
		return nil, errorf(ExitInvalid, "mcp call has no tool name")
	}
	args := call.Params.Arguments
	if args == nil {
		args = map[string]any{}
	}
	if name == "run_tool" {
		n, _ := args["name"].(string)
		if n == "" {
			return nil, errorf(ExitInvalid, "run_tool call has no command name")
		}
		name = n
		delete(args, "name")
	}
	var dropped []string
	for _, k := range []string{"stdin", "dir", "env"} {
		if _, ok := args[k]; ok {
			dropped = append(dropped, k)
			delete(args, k)
		}
	}
	if len(dropped) > 0 {
		slices.Sort(dropped)
		l.add("mcp call fields that are not argv are dropped: %s", strings.Join(dropped, ", "))
	}
	if raw, ok := args["args"]; ok {
		list, ok := raw.([]any)
		if !ok {
			return nil, errorf(ExitInvalid, "mcp call args is not an array")
		}
		if len(args) > 1 {
			return nil, errorf(ExitInvalid, "mcp call mixes args with typed arguments")
		}
		argv := []string{name}
		for i, v := range list {
			s, ok := v.(string)
			if !ok {
				return nil, errorf(ExitInvalid, "mcp call args[%d] is not a string", i)
			}
			argv = append(argv, s)
		}
		return argv, nil
	}
	if SchemaFor == nil {
		if len(args) == 0 {
			return []string{name}, nil
		}
		return nil, errorf(ExitInvalid, "mcp call to %q has typed arguments and this host has no schema for it; pass args", name)
	}
	schema, ok := SchemaFor(name)
	if !ok {
		if len(args) == 0 {
			return []string{name}, nil
		}
		return nil, errorf(ExitInvalid, "mcp call to %q has typed arguments and no schema is known for it; pass args", name)
	}
	rest, err := schema.Argv(args)
	if err != nil {
		return nil, errorf(ExitInvalid, "mcp call to %q: %v", name, err)
	}
	return append([]string{name}, rest...), nil
}

// ---- schema verification -----------------------------------------------------

// verifyExecutable reports SchemaVerified when the host's schema for argv[0]
// binds the arguments and ArgSchema.Argv reproduces them byte for byte —
// the one way a literal argv is known to be exactly what the typed surface
// would run. Everything else is Unverified; nothing is run.
func verifyExecutable(argv []string) string {
	if SchemaFor == nil {
		return Unverified
	}
	schema, ok := SchemaFor(argv[0])
	if !ok {
		return Unverified
	}
	if _, ok := bindArgv(schema, argv[1:]); ok {
		return SchemaVerified
	}
	return Unverified
}

// bindArgv is the inverse of ArgSchema.Argv for the spellings Argv emits:
// --name for a true bool, --name value otherwise, positionals in order. It
// succeeds only when Argv maps the bound values back to exactly args.
func bindArgv(schema tool.ArgSchema, args []string) (map[string]any, bool) {
	flags := map[string]tool.ArgFlag{}
	for _, f := range schema.Flags {
		flags[f.Name] = f
	}
	bound := map[string]any{}
	pos := 0
	for i := 0; i < len(args); i++ {
		a := args[i]
		if strings.HasPrefix(a, "--") && len(a) > 2 {
			name, _, hasEq := strings.Cut(a[2:], "=")
			f, ok := flags[name]
			if !ok || hasEq {
				return nil, false
			}
			if f.Type == "bool" {
				bound[name] = true
				continue
			}
			if i+1 >= len(args) {
				return nil, false
			}
			i++
			v, ok := typedValue(f.Type, args[i])
			if !ok {
				return nil, false
			}
			bound[name] = v
			continue
		}
		if pos >= len(schema.Positionals) {
			return nil, false
		}
		p := schema.Positionals[pos]
		pos++
		v, ok := typedValue(p.Type, a)
		if !ok {
			return nil, false
		}
		bound[p.Name] = v
	}
	again, err := schema.Argv(bound)
	if err != nil || !slices.Equal(again, args) {
		return nil, false
	}
	return bound, true
}

func typedValue(typ, text string) (any, bool) {
	switch typ {
	case "", "string":
		return text, true
	case "bool":
		if text == "true" || text == "false" {
			return text == "true", true
		}
	case "int", "float":
		if _, err := strconv.ParseFloat(text, 64); err == nil {
			return json.Number(text), true
		}
	}
	return nil, false
}

// ---- skill (a ```bsh fence) --------------------------------------------------

func renderSkill(argv []string) ([]byte, error) {
	line, err := renderShell(argv, syntax.LangBashPP)
	if err != nil {
		return nil, err
	}
	s := snippet.Snippet{Lang: "bsh", Body: strings.TrimSuffix(string(line), "\n")}
	return []byte("```" + s.Lang + "\n" + s.Body + "\n```\n"), nil
}

// fenceLangs maps a fence info word to the dialect its body is parsed in.
var fenceLangs = map[string]syntax.LangVariant{
	"bsh": syntax.LangBashPP, "bash": syntax.LangBash, "sh": syntax.LangPOSIX, "posix": syntax.LangPOSIX,
}

// parseSkill takes the first shell fence of a markdown document (```bsh,
// ```bash, ```sh) as a snippet and parses its body as one command.
func parseSkill(data []byte) ([]string, error) {
	lines := strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
	for i := 0; i < len(lines); i++ {
		fence, info := fenceOpen(lines[i])
		if fence == "" {
			continue
		}
		lang, ok := fenceLangs[info]
		body := []string(nil)
		closed := false
		for j := i + 1; j < len(lines); j++ {
			if strings.HasPrefix(strings.TrimLeft(lines[j], " "), fence) && strings.Trim(strings.TrimLeft(lines[j], " "), "`") == "" {
				i, closed = j, true
				break
			}
			body = append(body, lines[j])
		}
		if !closed {
			return nil, errorf(ExitInvalid, "invalid skill input: unterminated %s fence", fence)
		}
		if !ok {
			continue
		}
		s := snippet.Snippet{Lang: info, Body: strings.Join(body, "\n")}
		return parseShell([]byte(s.Body), lang)
	}
	return nil, errorf(ExitInvalid, "invalid skill input: no ```bsh fence")
}

// fenceOpen returns the backtick run and the first info word of an opening
// fence line, or "" when the line is not one.
func fenceOpen(line string) (fence, info string) {
	t := strings.TrimLeft(line, " ")
	n := 0
	for n < len(t) && t[n] == '`' {
		n++
	}
	if n < 3 {
		return "", ""
	}
	rest := strings.TrimSpace(t[n:])
	if strings.Contains(rest, "`") {
		return "", ""
	}
	info, _, _ = strings.Cut(rest, " ")
	return t[:n], info
}

// ---- the knobs -----------------------------------------------------------------

func applyDrive(drive string, argv []string) []string {
	if drive == "" {
		return argv
	}
	return pathconv.NativeArgsIn(pathconv.DrivesOf(drive), argv)
}

func applySwitch(style string, argv []string) []string {
	if style == "" || style == "keep" {
		return argv
	}
	out := append([]string(nil), argv...)
	for i := 1; i < len(out); i++ {
		a := out[i]
		if a == "--" {
			break
		}
		switch {
		case strings.HasPrefix(a, "--") && len(a) > 2 && isOptionStart(a[2]):
			name, val, hasEq := strings.Cut(a[2:], "=")
			if hasEq {
				val = ":" + val
			}
			if style == "slash" {
				out[i] = "/" + name + val
			} else {
				out[i] = "-" + pascalCase(name) + val
			}
		case len(a) >= 2 && a[0] == '-' && isOptionStart(a[1]):
			if style == "slash" {
				out[i] = "/" + a[1:]
			}
		}
	}
	return out
}

func isOptionStart(c byte) bool { return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' }

// pascalCase turns dry-run / no_input into DryRun / NoInput.
func pascalCase(name string) string {
	var b strings.Builder
	upper := true
	for _, r := range name {
		if r == '-' || r == '_' {
			upper = true
			continue
		}
		if upper {
			r = unicode.ToUpper(r)
			upper = false
		}
		b.WriteRune(r)
	}
	return b.String()
}

// validUTF8 is the one shape check shared by the Windows dialects: a command
// line is text, so a byte sequence that is not UTF-8 cannot be spelled.
func validUTF8(argv []string) error {
	for i, a := range argv {
		if !utf8.ValidString(a) {
			return errorf(ExitInvalid, "argument %d is not valid UTF-8", i)
		}
	}
	return nil
}
