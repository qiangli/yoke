// Package nftcmd implements nft(8) for the subset sigs.k8s.io/knftables
// drives — kube-proxy's nftables mode and flannel's nftables traffic
// manager — over github.com/google/nftables (pkg/nft):
//
//	nft --version
//	nft [--check] -f FILE|-               (one atomic transaction)
//	nft [--check] COMMAND...              (same language, as operands)
//	nft --json [--terse] list table|chain|set|map|counters ...
//
// Anything outside that subset (other statements, text listings, handles,
// insert/replace/destroy) fails loudly. Errors keep nft's stderr shape —
// "Error: Could not process rule: No such file or directory" — because
// knftables classifies not-found / already-exists by those phrases.
// Linux only: nftables is a Linux kernel facility.
package nftcmd

import (
	"fmt"
	"io"
	"strings"

	"github.com/qiangli/coreutils/tool"
	"github.com/qiangli/yoke/pkg/nft"
)

var cmd = &tool.Tool{
	Name:     "nft",
	Synopsis: "Apply and list nftables rules (the knftables subset; pure Go, netlink).",
	Usage: "nft [-c] -f FILE|-\n   or: nft [-c] COMMAND...\n" +
		"   or: nft -j [-t] list table [FAMILY] TABLE\n" +
		"   or: nft -j list chain|set|map [FAMILY] TABLE NAME\n" +
		"   or: nft -j list counters table [FAMILY] TABLE\n   or: nft -v",
}

func init() { cmd.Run = run; tool.Register(cmd) }

// newBackend is swapped by tests for an in-memory backend.
var newBackend = nft.NewNetlink

func run(rc *tool.RunContext, args []string) int {
	// nft's own version line (knftables rejects < v1.0.1 by prefix), not
	// the userland's universal --version text.
	for _, a := range args {
		if a == "-v" || a == "--version" {
			fmt.Fprintf(rc.Out, "nftables v%s (bashy)\n", nft.Version)
			return 0
		}
		if !strings.HasPrefix(a, "-") {
			break
		}
	}
	fs := tool.NewFlags(cmd.Name)
	check := fs.BoolP("check", "c", false, "check commands' validity without applying them")
	file := fs.StringP("file", "f", "", "read commands from FILE (- for standard input)")
	jsonOut := fs.BoolP("json", "j", false, "format list output as JSON")
	terse := fs.BoolP("terse", "t", false, "omit set elements when listing")
	operands, code := tool.ParseRequireOrder(rc, cmd, fs, args)
	if code >= 0 {
		return code
	}

	if fs.Changed("file") {
		if len(operands) > 0 {
			return tool.UsageError(rc, cmd, "extra operand %q", operands[0])
		}
		var in io.Reader = rc.In
		if *file != "-" {
			f, err := rc.FS.Open(rc.Path(*file))
			if err != nil {
				fmt.Fprintf(rc.Err, "Error: Could not open file %q: %v\n", *file, err)
				return 1
			}
			defer f.Close()
			in = f
		}
		script, err := io.ReadAll(in)
		if err != nil {
			fmt.Fprintf(rc.Err, "Error: %v\n", err)
			return 1
		}
		return apply(rc, string(script), *check)
	}
	if len(operands) == 0 {
		return tool.UsageError(rc, cmd, "missing command")
	}
	if operands[0] == "list" {
		if *check {
			return tool.NotSupported(rc, cmd, "--check with list")
		}
		if !*jsonOut {
			return tool.NotSupported(rc, cmd, "text list output (use --json)")
		}
		return list(rc, operands[1:], *terse)
	}
	return apply(rc, strings.Join(operands, " ")+"\n", *check)
}

func apply(rc *tool.RunContext, script string, check bool) int {
	b, err := newBackend()
	if err != nil {
		fmt.Fprintf(rc.Err, "Error: %v\n", err)
		return 1
	}
	if err := nft.Run(b, script, check); err != nil {
		fmt.Fprintln(rc.Err, nft.ErrorText(err))
		return 1
	}
	return 0
}

// list parses "table [FAM] T", "chain|set|map [FAM] T NAME" and
// "counters table [FAM] T"; the family defaults to ip, as in nft.
func list(rc *tool.RunContext, ops []string, terse bool) int {
	if len(ops) == 0 {
		return tool.UsageError(rc, cmd, "list: missing object")
	}
	a := nft.ListArgs{What: ops[0], Terse: terse, Family: nft.FamilyIP}
	rest := ops[1:]
	if a.What == "counters" {
		if len(rest) == 0 || rest[0] != "table" {
			return tool.NotSupported(rc, cmd, "list counters without 'table'")
		}
		rest = rest[1:]
	}
	if len(rest) > 0 {
		switch nft.Family(rest[0]) {
		case nft.FamilyIP, nft.FamilyIP6, nft.FamilyInet:
			a.Family = nft.Family(rest[0])
			rest = rest[1:]
		}
	}
	want := 1
	switch a.What {
	case "table", "counters":
	case "chain", "set", "map":
		want = 2
	default:
		return tool.NotSupported(rc, cmd, "list "+a.What)
	}
	if len(rest) != want {
		return tool.UsageError(rc, cmd, "list %s: wrong number of operands", a.What)
	}
	a.Table = rest[0]
	if want == 2 {
		a.Name = rest[1]
	}
	b, err := newBackend()
	if err != nil {
		fmt.Fprintf(rc.Err, "Error: %v\n", err)
		return 1
	}
	out, err := nft.ListJSON(b, a)
	if err != nil {
		fmt.Fprintln(rc.Err, nft.ErrorText(err))
		return 1
	}
	_, _ = rc.Out.Write(out)
	return 0
}
