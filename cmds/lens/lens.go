// Package lenscmd implements `lens`, the representation filter: iconv for
// formats. It reads FILE (or stdin), converts FROM→TO through one declared
// row of pkg/lens, and writes the raw result to stdout; every diagnostic goes
// to stderr.
//
//	lens [-f FROM] -t TO [FILE]   convert (FROM: -f, else extension, else magic)
//	lens -l [--json]              list the rows
//
// Command lines are rows too (bash/posix/bsh/cmd/pwsh/json/yaml/mcp-tool/
// mcp-call/skill); they are never detected, so they need -f. Two knobs are
// the agent's: --switch=keep|slash|pascal respells options for a Windows
// peer and --drive=C spells /c/… paths natively. --verbose adds
// "executable: schema-verified|unverified"; nothing is ever run.
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
	"strings"
	"syscall"

	"github.com/qiangli/coreutils/tool"
	"github.com/qiangli/yoke/pkg/lens"
)

var cmd = &tool.Tool{
	Name:     "lens",
	Synopsis: "Convert a file between representations (json/yaml/toml/csv/md, pdf/docx/xlsx/html → md|txt, archive → listing, command line → bash/cmd/pwsh/mcp/skill).",
	Usage:    "lens [-f FROM] -t TO [--strict] [--verbose] [--switch=keep|slash|pascal] [--drive=C] [FILE] | lens -l [--json]",
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

	operands, code := tool.Parse(rc, cmd, fs, args)
	if code >= 0 {
		return code
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

	name := ""
	var data []byte
	var err error
	if len(operands) == 0 || operands[0] == "-" {
		data, err = io.ReadAll(rc.In)
	} else {
		name = operands[0]
		data, err = os.ReadFile(rc.Path(name))
	}
	if err != nil {
		fmt.Fprintf(rc.Err, "lens: %v\n", err)
		return lens.ExitInvalid
	}

	req := lens.Request{From: fromFmt, To: toFmt, Name: name, Data: data, Base: rc, Argv: lens.ArgvOptions{Switch: *sw, Drive: *drive}}
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
