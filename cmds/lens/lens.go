// Package lenscmd implements `lens`, the representation filter: iconv for
// formats. It reads FILE (or stdin), converts FROM→TO through one declared
// row of pkg/lens, and writes the raw result to stdout; every diagnostic goes
// to stderr.
//
//	lens [-f FROM] -t TO [FILE]   convert (FROM: a declared bashy-graph-v1
//	                              header, else -f, else extension, else magic)
//	lens -t VIEW|graph LOCATOR    pull a registered provider (dir:, …)
//	lens -l [--json]              list the rows
//
// Command lines are rows too (bash/posix/bsh/cmd/pwsh/json/yaml/mcp-tool/
// mcp-call/skill); they are never detected, so they need -f. Two knobs are
// the agent's: --switch=keep|slash|pascal respells options for a Windows
// peer and --drive=C spells /c/… paths natively. --verbose adds
// "executable: schema-verified|unverified"; for a locator pull it also prints
// the resolved tier and transport (never credentials); nothing is ever run.
//
// Pull and push are the same bytes: a LOCATOR is walked into the
// bashy-graph-v1 stream (`-t graph` prints it) and that stream is then
// converted exactly as a piped producer's stream would be.
//
// Exit: 0 converted; 1 input invalid for FROM; 2 usage; 3 no route;
// 4 lossy under --strict; 127 recipe converter not registered.
package lenscmd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/qiangli/coreutils/tool"
	"github.com/qiangli/yoke/pkg/lens"
	"github.com/qiangli/yoke/pkg/lens/provider"
	"github.com/qiangli/yoke/pkg/lens/stream"
	"github.com/qiangli/yoke/pkg/ref"
)

var cmd = &tool.Tool{
	Name:     "lens",
	Synopsis: "Convert a file between representations (json/yaml/toml/csv/md, pdf/docx/xlsx/html → md|txt, archive → listing, command line → bash/cmd/pwsh/mcp/skill, bashy-graph-v1 stream → views).",
	Usage:    "lens [-f FROM] -t TO [--strict] [--verbose] [--switch=keep|slash|pascal] [--drive=C] [--depth=N] [--null] [--since=start|CURSOR|@FILE] [--cursor-out=FILE] [FILE|LOCATOR] | lens -l [--json]",
}

// ProviderFactory builds the provider that serves one pulled locator.
type ProviderFactory func(rc *tool.RunContext, loc ref.Locator) (provider.Provider, error)

var providers = map[string]ProviderFactory{"dir": dirProvider}

// RegisterProvider makes `lens … SCHEME:…` pull through f. A producer that is
// linked only into some hosts (graph: lives with cmds/graph) registers here
// at init so its pull form and its piped --json form share one library.
func RegisterProvider(scheme string, f ProviderFactory) {
	if _, dup := providers[scheme]; dup || scheme == "" || f == nil {
		panic("lens: invalid or duplicate provider " + scheme)
	}
	providers[scheme] = f
}

func dirProvider(rc *tool.RunContext, loc ref.Locator) (provider.Provider, error) {
	base := rc.Path(".")
	if filepath.IsAbs(loc.Target) {
		base = string(filepath.Separator)
	}
	return provider.NewDir(base, false), nil
}

// locatorOperand reports an operand spelled as an explicit provider locator
// (SCHEME:…). A bare path stays a FILE, exactly as before S11.
func locatorOperand(op string) (ref.Locator, bool) {
	extra := make(map[string]bool, len(providers))
	for k := range providers {
		extra[k] = true
	}
	loc, err := ref.ParseLocatorWithSchemes(op, extra)
	if err != nil || !strings.HasPrefix(op, loc.Scheme+":") || len(op) >= 3 && op[1] == ':' {
		return ref.Locator{}, false
	}
	return loc, true
}

func init() { cmd.Run = run; tool.Register(cmd) }

func run(rc *tool.RunContext, args []string) int {
	fs := tool.NewFlags(cmd.Name)
	from := fs.StringP("from", "f", "", "input format (default: by extension, then content magic)")
	to := fs.StringP("to", "t", "", "output format")
	list := fs.BoolP("list", "l", false, "list the conversion rows")
	asJSON := fs.Bool("json", false, "with -l, one JSON object per row")
	strict := fs.Bool("strict", false, "exit 4 instead of writing a lossy result")
	verbose := fs.Bool("verbose", false, "print the route, fidelity and (command lines) executable verdict on stderr")
	sw := fs.String("switch", "", "command lines: option spelling keep|slash|pascal (--dry-run, /dry-run, -DryRun)")
	drive := fs.String("drive", "", "command lines: drive letters present on the target (C, CD); /c/… paths become C:\\…")
	depth := fs.Int("depth", -1, "graph views and locator pulls: maximum descent depth (-1 = unbounded)")
	since := fs.String("since", "", "file deltas: start, cursor envelope, or @cursor-file (rotation requires explicit restart)")
	cursorOut := fs.String("cursor-out", "", "save file delta cursor after output succeeds; otherwise append JSONL cursor trailer")
	nul := fs.Bool("null", false, "paths view: NUL-terminated")

	operands, code := tool.Parse(rc, cmd, fs, args)
	if code >= 0 {
		return code
	}
	if fs.Changed("since") || fs.Changed("cursor-out") {
		if *since == "" || *list || *asJSON || len(operands) != 1 || operands[0] == "-" {
			return tool.UsageError(rc, cmd, "--since requires start or a cursor and one regular FILE")
		}
		loc, isLoc := locatorOperand(operands[0])
		if isLoc && !isAgentWorldScheme(loc.Scheme) {
			return tool.UsageError(rc, cmd, "--since file deltas do not support this provider")
		}
		format := lens.Canonical(*to)
		if format != "jsonl" && format != "txt" || *from != "" && lens.Canonical(*from) != format {
			return tool.UsageError(rc, cmd, "--since requires -t jsonl or -t txt, without format conversion")
		}
		if format == "txt" && *cursorOut == "" {
			return tool.UsageError(rc, cmd, "--since -t txt requires --cursor-out")
		}
		for _, flag := range []string{"strict", "verbose", "switch", "drive", "depth", "null"} {
			if fs.Changed(flag) {
				return tool.UsageError(rc, cmd, "--%s does not apply to file deltas", flag)
			}
		}
		if isLoc {
			return sinceAgentWorld(rc, loc, format, *since, *cursorOut)
		}
		return sinceFile(rc, operands[0], format, *since, *cursorOut)
	}
	if *list {
		if *to != "" || *from != "" || len(operands) > 0 {
			return tool.UsageError(rc, cmd, "-l takes no other operands")
		}
		return write(rc, listRows(*asJSON))
	}
	if *asJSON {
		return tool.UsageError(rc, cmd, "--json applies to -l")
	}
	if *to == "" {
		return tool.UsageError(rc, cmd, "-t TO is required")
	}
	toFmt := lens.Canonical(*to)
	if toFmt == "" {
		return tool.UsageError(rc, cmd, "unknown format %q (lens -l lists the rows)", *to)
	}
	fromFmt := ""
	if *from != "" {
		if fromFmt = lens.Canonical(*from); fromFmt == "" {
			return tool.UsageError(rc, cmd, "unknown format %q (lens -l lists the rows)", *from)
		}
	}
	if len(operands) > 1 {
		return tool.UsageError(rc, cmd, "at most one FILE")
	}

	view := lens.ViewOptions{Null: *nul}
	if fs.Changed("depth") {
		view.Depth = depth
	}

	name := ""
	var data []byte
	var err error
	if len(operands) == 1 && operands[0] != "-" {
		if loc, ok := locatorOperand(operands[0]); ok {
			if fromFmt != "" {
				return tool.UsageError(rc, cmd, "-f does not apply to a locator")
			}
			if *verbose {
				reportTier(rc, loc)
			}
			if data, code = pull(rc, loc, *depth); code != 0 {
				return code
			}
			if toFmt == "graph" {
				return write(rc, data)
			}
			operands = nil
		}
	}
	// A pulled stream is converted exactly as a piped producer's would be.
	switch {
	case data != nil:
	case len(operands) == 0 || operands[0] == "-":
		data, err = io.ReadAll(rc.In)
	default:
		name = operands[0]
		data, err = os.ReadFile(rc.Path(name))
	}
	if err != nil {
		fmt.Fprintf(rc.Err, "lens: %v\n", err)
		return lens.ExitInvalid
	}

	req := lens.Request{From: fromFmt, To: toFmt, Name: name, Data: data, Base: rc, Argv: lens.ArgvOptions{Switch: *sw, Drive: *drive}, View: view}
	res, err := lens.Convert(rc.Ctx, req)
	if err != nil {
		if le, ok := err.(*lens.Error); ok {
			fmt.Fprintf(rc.Err, "lens: %v\n", le)
			return le.Code
		}
		fmt.Fprintf(rc.Err, "lens: %v\n", err)
		return lens.ExitInvalid
	}
	if *verbose {
		fmt.Fprintf(rc.Err, "lens: route %s -> %s via %s (%s)\n", res.From, res.Row.To, res.Row.Via(), res.Row.Fidelity)
		if res.Executable != "" {
			fmt.Fprintf(rc.Err, "lens: executable: %s\n", res.Executable)
		}
	}
	for _, n := range res.Losses {
		fmt.Fprintf(rc.Err, "lens: lossy: %s\n", n)
	}
	if *strict && res.Lossy() {
		if len(res.Losses) == 0 {
			fmt.Fprintf(rc.Err, "lens: --strict: %s -> %s is a lossy row\n", res.From, res.Row.To)
		} else {
			fmt.Fprintf(rc.Err, "lens: --strict: refusing a lossy result\n")
		}
		return lens.ExitLossy
	}
	return write(rc, res.Data)
}

// pull walks loc into its bashy-graph-v1 stream.
func pull(rc *tool.RunContext, loc ref.Locator, depth int) ([]byte, int) {
	f, ok := providers[loc.Scheme]
	if !ok {
		return nil, tool.UsageError(rc, cmd, "%s: provider %q is not registered", loc, loc.Scheme)
	}
	var p provider.Provider
	var err error
	if loc.ScopeKind != "" {
		p, err = scopedProvider(rc, loc)
	} else {
		p, err = f(rc, loc)
	}
	if err == nil {
		var g *stream.Graph
		if g, err = lens.PullGraph(rc.Ctx, p, loc, depth); err == nil {
			var b bytes.Buffer
			if err = stream.Encode(&b, g, loc.Scheme, lens.PullQuery(depth)); err == nil {
				return b.Bytes(), 0
			}
		}
	}
	fmt.Fprintf(rc.Err, "lens: %s: %v\n", loc, err)
	return nil, lens.ExitInvalid
}

func listRows(asJSON bool) []byte {
	var b bytes.Buffer
	for _, r := range lens.Rows() {
		status := "installed"
		if !r.Installed() {
			status = "missing"
		}
		if asJSON {
			line, _ := json.Marshal(struct {
				From      string `json:"from"`
				To        string `json:"to"`
				Fidelity  string `json:"fidelity"`
				Via       string `json:"via"`
				Installed bool   `json:"installed"`
			}{r.From, r.To, string(r.Fidelity), r.Via(), r.Installed()})
			b.Write(line)
			b.WriteByte('\n')
			continue
		}
		fmt.Fprintln(&b, strings.Join([]string{r.From, r.To, string(r.Fidelity), r.Via(), status}, "\t"))
	}
	return b.Bytes()
}

// write follows cat: a downstream reader that exits early (`| head -1`) is a
// normal end, silent and exit 0, unless the shell ignores SIGPIPE.
//
// The Go runtime turns EPIPE on fd 1/2 into a raised SIGPIPE — a standalone
// binary would die with 141 before seeing the error — unless SIGPIPE is under
// signal.Notify. So the write holds a Notify for its duration, letting EPIPE
// come back as an error; an inherited or host-set SIG_IGN is left alone.
func write(rc *tool.RunContext, data []byte) int {
	if _, isFile := rc.Out.(*os.File); isFile && !rc.SIGPIPEIgnored && !signal.Ignored(syscall.SIGPIPE) {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGPIPE)
		defer signal.Stop(sig)
	}
	if _, err := rc.Out.Write(data); err != nil {
		if tool.IsClosedPipeError(err) && !rc.SIGPIPEIgnored {
			return 0
		}
		fmt.Fprintf(rc.Err, "lens: write error: %v\n", err)
		return 1
	}
	return 0
}
