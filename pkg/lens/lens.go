// Package lens is the representation filter behind `lens`: iconv for formats.
// One job — change how bytes are represented — through one declared table of
// rows. A row is FROM→TO with a declared fidelity and a converter that is
// either built-in pure Go or a recipe over a REGISTERED command (never PATH).
//
// Row families in this package (Sprint 446 S3):
//
//   - data: json↔yaml↔toml, json↔jsonl, csv↔tsv↔json, csv/tsv→md table;
//   - documents: html→md (the same converter as `fetch --md`), html→txt,
//     pdf/docx/xlsx→md|txt as text-only Go fallbacks, with markitdown/docling
//     recipe rows preferred when those commands are registered;
//   - contents projection: tar/pax/zip→listing (uniform JSONL Entry rows) and
//     →md.
//
// Lossless rows promise A→B→A byte identity for canonical input; anything a
// particular input loses is reported as a runtime loss so `--strict` can
// refuse it.
package lens

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/qiangli/coreutils/tool"
)

// Exit codes, shared by the library errors and the CLI.
const (
	ExitOK            = 0
	ExitInvalid       = 1   // input invalid for FROM (or not representable in TO)
	ExitUsage         = 2   // bad flags, unknown format, FROM not resolvable
	ExitNoRoute       = 3   // no row FROM→TO
	ExitLossy         = 4   // --strict and the conversion lost something
	ExitRecipeMissing = 127 // only recipe rows exist and none is registered
)

// Error carries the exit code a failure maps to.
type Error struct {
	Code int
	Err  error
}

func (e *Error) Error() string { return e.Err.Error() }
func (e *Error) Unwrap() error { return e.Err }

func errorf(code int, format string, a ...any) *Error {
	return &Error{Code: code, Err: fmt.Errorf(format, a...)}
}

// Fidelity is a row's declared promise.
type Fidelity string

const (
	Lossless Fidelity = "lossless"
	Lossy    Fidelity = "lossy"
)

// Recipe runs a registered command. Args may use {in} (the input file),
// {outdir} (a scratch directory) and {stem} (the input's base name without
// extension). With Output empty the command's stdout is the result; otherwise
// Output names the file it writes.
type Recipe struct {
	Command string
	Args    []string
	Output  string
	Hint    string // printed on exit 127
}

// Row is one declared conversion.
type Row struct {
	From     string
	To       string
	Fidelity Fidelity
	Recipe   *Recipe // nil for built-in rows
	conv     func(data []byte, l *losses) ([]byte, error)
	// argv is set on the command-line rows instead of conv: they take the
	// agent-chosen knobs and report the executable verdict (argv.go).
	argv func(data []byte, opt ArgvOptions, l *losses) ([]byte, string, error)
}

// IsArgv reports whether the row is a command-line (argv) row.
func (r Row) IsArgv() bool { return r.argv != nil }

// Via names the converter: "go" or "recipe:<command>".
func (r Row) Via() string {
	if r.Recipe != nil {
		return "recipe:" + r.Recipe.Command
	}
	return "go"
}

// LookupCommand resolves a recipe's command in the tool registry. It is a
// variable so tests can stand in a fake converter.
var LookupCommand = tool.Lookup

// Installed reports whether the row can run here.
func (r Row) Installed() bool {
	return r.Recipe == nil || LookupCommand(r.Recipe.Command) != nil
}

var markitdown = &Recipe{
	Command: "markitdown",
	Args:    []string{"{in}"},
	Hint:    "markitdown is not a registered bashy command",
}

var docling = &Recipe{
	Command: "docling",
	Args:    []string{"--to", "md", "--output", "{outdir}", "{in}"},
	Output:  "{outdir}/{stem}.md",
	Hint:    "docling is not a registered bashy command",
}

var rows = buildRows()

func buildRows() []Row {
	var t []Row
	add := func(from, to string, f Fidelity, conv func([]byte, *losses) ([]byte, error)) {
		t = append(t, Row{From: from, To: to, Fidelity: f, conv: conv})
	}
	recipe := func(from, to string, r *Recipe) {
		t = append(t, Row{From: from, To: to, Fidelity: Lossy, Recipe: r})
	}

	// data
	for _, p := range [][2]string{
		{"json", "yaml"}, {"json", "toml"}, {"json", "jsonl"},
		{"yaml", "json"}, {"yaml", "toml"},
		{"toml", "json"}, {"toml", "yaml"},
		{"jsonl", "json"},
	} {
		from, to := p[0], p[1]
		add(from, to, Lossless, func(data []byte, l *losses) ([]byte, error) { return convertData(from, to, data, l) })
	}
	for _, p := range [][2]string{{"csv", "tsv"}, {"tsv", "csv"}} {
		from, to := p[0], p[1]
		add(from, to, Lossless, func(data []byte, l *losses) ([]byte, error) {
			recs, err := readRecords(data, from)
			if err != nil {
				return nil, err
			}
			var b bytes.Buffer
			err = writeRecords(&b, recs, to)
			return b.Bytes(), err
		})
	}
	for _, from := range []string{"csv", "tsv"} {
		from := from
		add(from, "json", Lossless, func(data []byte, l *losses) ([]byte, error) {
			recs, err := readRecords(data, from)
			if err != nil {
				return nil, err
			}
			var b bytes.Buffer
			encodeJSON(&b, recordsToJSON(recs, l), false)
			return b.Bytes(), nil
		})
		add(from, "md", Lossless, func(data []byte, l *losses) ([]byte, error) {
			recs, err := readRecords(data, from)
			if err != nil {
				return nil, err
			}
			var b bytes.Buffer
			recordsToMarkdown(&b, recs, l)
			return b.Bytes(), nil
		})
	}
	for _, to := range []string{"csv", "tsv"} {
		to := to
		add("json", to, Lossless, func(data []byte, l *losses) ([]byte, error) {
			v, err := decodeJSON(data, l)
			if err != nil {
				return nil, err
			}
			recs, err := jsonToRecords(v, l)
			if err != nil {
				return nil, err
			}
			var b bytes.Buffer
			err = writeRecords(&b, recs, to)
			return b.Bytes(), err
		})
	}

	// documents: recipe rows first so an installed recipe is preferred
	for _, from := range []string{"pdf", "docx", "xlsx", "pptx"} {
		recipe(from, "md", markitdown)
	}
	for _, from := range []string{"pdf", "docx", "pptx"} {
		recipe(from, "md", docling)
	}
	add("html", "md", Lossy, func(data []byte, _ *losses) ([]byte, error) {
		s, err := HTMLToMarkdown(string(data))
		return text(s), err
	})
	add("html", "txt", Lossy, textConv(htmlText))
	add("pdf", "md", Lossy, textConv(pdfMarkdown))
	add("pdf", "txt", Lossy, textConv(pdfPlainText))
	add("docx", "md", Lossy, textConv(docxMarkdown))
	add("docx", "txt", Lossy, textConv(docxText))
	add("xlsx", "md", Lossy, textConv(xlsxMarkdown))
	add("xlsx", "txt", Lossy, textConv(xlsxText))

	// contents projection
	for _, p := range []struct {
		from    string
		entries func([]byte) ([]Entry, error)
	}{{"tar", tarEntries}, {"zip", zipEntries}} {
		entries := p.entries
		add(p.from, "listing", Lossless, func(data []byte, _ *losses) ([]byte, error) {
			es, err := entries(data)
			var b bytes.Buffer
			writeListing(&b, es)
			return b.Bytes(), err
		})
		add(p.from, "md", Lossy, func(data []byte, _ *losses) ([]byte, error) {
			es, err := entries(data)
			var b bytes.Buffer
			writeListingMarkdown(&b, es)
			return b.Bytes(), err
		})
	}

	// command output -> data (jc-style)
	for _, cmd := range []string{
		"cmd:ps", "cmd:df", "cmd:ls-l", "cmd:du", "cmd:wc",
		"cmd:stat", "cmd:id", "cmd:uname", "cmd:tar-tv",
	} {
		cmd := cmd
		add(cmd, "json", Lossy, func(data []byte, l *losses) ([]byte, error) {
			return parseCmdToJSON(cmd, data, l)
		})
	}

	// command lines: every ordered pair of argv dialects; json↔yaml stays the
	// data row above (a command record is data too, and that row is exact).
	for _, from := range argvFormats {
		for _, to := range argvFormats {
			if from == to || !argvOnly[from] && !argvOnly[to] {
				continue
			}
			t = append(t, Row{From: from, To: to, Fidelity: Lossless, argv: argvRow(from, to)})
		}
	}
	return t
}

func textConv(f func([]byte) (string, error)) func([]byte, *losses) ([]byte, error) {
	return func(data []byte, _ *losses) ([]byte, error) {
		s, err := f(data)
		return text(s), err
	}
}

// text ends non-empty output with exactly one newline.
func text(s string) []byte {
	s = strings.TrimRight(s, "\n")
	if s == "" {
		return nil
	}
	return []byte(s + "\n")
}

func convertData(from, to string, data []byte, l *losses) ([]byte, error) {
	var docs []any
	switch from {
	case "json":
		v, err := decodeJSON(data, l)
		if err != nil {
			return nil, err
		}
		docs = []any{v}
	case "jsonl":
		var err error
		if docs, err = decodeJSONL(data, l); err != nil {
			return nil, err
		}
	case "yaml":
		var err error
		if docs, err = decodeYAML(data, l); err != nil {
			return nil, err
		}
	case "toml":
		v, err := decodeTOML(data, l)
		if err != nil {
			return nil, err
		}
		docs = []any{v}
	}
	var one any // the single document json/toml want
	switch {
	case from == "jsonl":
		arr := docs
		if arr == nil {
			arr = []any{}
		}
		one = arr
	case len(docs) == 1:
		one = docs[0]
	case len(docs) > 1:
		l.add("%d yaml documents wrapped in one array", len(docs))
		one = docs
	}
	var b bytes.Buffer
	switch to {
	case "json":
		encodeJSON(&b, one, false)
	case "jsonl":
		arr, ok := one.([]any)
		if !ok {
			l.add("a non-array value is written as a single line")
			arr = []any{one}
		}
		for _, e := range arr {
			encodeJSON(&b, e, true)
		}
	case "yaml":
		if err := encodeYAML(&b, one); err != nil {
			return nil, err
		}
	case "toml":
		if err := encodeTOML(&b, one, l); err != nil {
			return nil, &Error{Code: ExitInvalid, Err: err}
		}
	}
	return b.Bytes(), nil
}

// Rows returns the declared table in preference order.
func Rows() []Row { return append([]Row(nil), rows...) }

// Route picks the row for FROM→TO: the first installed recipe, else the
// built-in row. With no built-in row and no installed recipe it fails with
// ExitRecipeMissing; with no row at all, ExitNoRoute.
func Route(from, to string) (Row, error) {
	var builtin *Row
	var missing []*Recipe
	for i := range rows {
		r := &rows[i]
		if r.From != from || r.To != to {
			continue
		}
		if r.Recipe == nil {
			if builtin == nil {
				builtin = r
			}
			continue
		}
		if r.Installed() {
			return *r, nil
		}
		missing = append(missing, r.Recipe)
	}
	if builtin != nil {
		return *builtin, nil
	}
	if len(missing) > 0 {
		var hints []string
		for _, m := range missing {
			hints = append(hints, m.Hint)
		}
		return Row{}, errorf(ExitRecipeMissing, "no converter installed for %s -> %s (%s)", from, to, strings.Join(hints, "; "))
	}
	return Row{}, errorf(ExitNoRoute, "no route %s -> %s (lens -l lists the rows)", from, to)
}

// Request is one conversion.
type Request struct {
	From string // canonical format; "" = detect from Name and Data
	To   string // canonical format
	Name string // input file name, for detection and recipe temp files
	Data []byte
	// Base is copied into the RunContext of a recipe command (Env, FS, …).
	Base *tool.RunContext
	// Argv carries the command-line knobs; setting either on a row that is
	// not an argv row is a usage error.
	Argv ArgvOptions
}

// Result is a finished conversion.
type Result struct {
	Data   []byte
	Row    Row
	From   string
	Losses []string // what this input lost on top of the row's declaration
	// Executable is SchemaVerified or Unverified on argv rows, "" otherwise.
	Executable string
}

// Lossy reports whether the result is anything but exact.
func (r Result) Lossy() bool { return r.Row.Fidelity == Lossy || len(r.Losses) > 0 }

// Convert resolves FROM, routes and converts.
func Convert(ctx context.Context, req Request) (Result, error) {
	from := req.From
	if from == "" {
		from = Detect(req.Name, req.Data)
		if from == "" {
			return Result{}, errorf(ExitUsage, "cannot detect the input format; pass -f FROM")
		}
	}
	row, err := Route(from, req.To)
	if err != nil {
		return Result{From: from}, err
	}
	var l losses
	var out []byte
	var executable string
	switch {
	case row.argv != nil:
		out, executable, err = row.argv(req.Data, req.Argv, &l)
	case !req.Argv.empty():
		return Result{From: from, Row: row}, errorf(ExitUsage, "--switch and --drive apply to command-line rows, not %s -> %s", from, req.To)
	case row.Recipe != nil:
		out, err = runRecipe(ctx, row, req)
	default:
		out, err = row.conv(req.Data, &l)
	}
	if err != nil {
		var le *Error
		if errors.As(err, &le) {
			return Result{From: from, Row: row}, le
		}
		return Result{From: from, Row: row}, &Error{Code: ExitInvalid, Err: fmt.Errorf("invalid %s input: %w", from, err)}
	}
	return Result{Data: out, Row: row, From: from, Losses: l.notes, Executable: executable}, nil
}

func runRecipe(ctx context.Context, row Row, req Request) ([]byte, error) {
	t := LookupCommand(row.Recipe.Command)
	if t == nil {
		return nil, errorf(ExitRecipeMissing, "%s", row.Recipe.Hint)
	}
	dir, err := os.MkdirTemp("", "lens-recipe-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	stem := strings.TrimSuffix(filepath.Base(req.Name), filepath.Ext(req.Name))
	if stem == "" || stem == "." || stem == string(filepath.Separator) {
		stem = "input"
	}
	in := filepath.Join(dir, stem+"."+row.From)
	outdir := filepath.Join(dir, "out")
	if err := os.WriteFile(in, req.Data, 0o600); err != nil {
		return nil, err
	}
	if err := os.Mkdir(outdir, 0o700); err != nil {
		return nil, err
	}
	expand := strings.NewReplacer("{in}", in, "{outdir}", outdir, "{stem}", stem).Replace
	args := make([]string, len(row.Recipe.Args))
	for i, a := range row.Recipe.Args {
		args[i] = expand(a)
	}
	var rc tool.RunContext
	if req.Base != nil {
		rc = *req.Base
	}
	var stdout, stderr bytes.Buffer
	rc.Ctx, rc.Dir = ctx, dir
	rc.Stdio = tool.Stdio{In: strings.NewReader(""), Out: &stdout, Err: &stderr}
	if code := t.Run(&rc, args); code != 0 {
		msg := strings.TrimSpace(stderr.String())
		return nil, errorf(ExitInvalid, "recipe %s exited %d: %s", row.Recipe.Command, code, msg)
	}
	if row.Recipe.Output == "" {
		return stdout.Bytes(), nil
	}
	return os.ReadFile(expand(row.Recipe.Output))
}
