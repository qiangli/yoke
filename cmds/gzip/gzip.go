// Package gzipcmd implements gzip(1), plus the gunzip and zcat
// aliases, per the GNU gzip manual: -d decompress, -k keep, -c stdout,
// -f force, -1..-9 / --fast / --best levels. Default behavior replaces
// FILE with FILE.gz and removes the original (the reverse for -d);
// mode and mtime of the input are preserved on the output. Exit codes
// follow gzip's manual: 0 ok, 1 error, 2 warning.
//
// Portions adapted from https://github.com/u-root/u-root
// pkg/gzip/{options,file}.go and cmds/core/gzip/gzip.go (BSD-3-Clause).
// Changes: rewired to tool framework; pgzip replaced with stdlib
// compress/gzip; GNU suffix rules (.gz/.tgz/.taz/.z), warning-vs-error
// exit codes, mode+mtime preservation, gzip header name/mtime,
// gunzip/zcat registered as alias tools.
package gzipcmd

import (
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/qiangli/coreutils/tool"
)

var (
	gzipTool = &tool.Tool{
		Name:     "gzip",
		Synopsis: "Compress FILEs in place (each FILE becomes FILE.gz); with no FILE or '-', compress stdin to stdout.",
		Usage:    "gzip [-cdfk] [-1..-9] [FILE]...",
	}
	gunzipTool = &tool.Tool{
		Name:     "gunzip",
		Synopsis: "Decompress FILEs in place (equivalent to gzip -d).",
		Usage:    "gunzip [-cfk] [FILE]...",
	}
	zcatTool = &tool.Tool{
		Name:     "zcat",
		Synopsis: "Decompress FILEs to standard output (equivalent to gzip -dc).",
		Usage:    "zcat [FILE]...",
	}
)

// Run funcs are wired in init: literals would create initialization
// cycles (each run's flag-error paths reference its tool).
func init() {
	gzipTool.Run = func(rc *tool.RunContext, args []string) int {
		return run(rc, gzipTool, args, false, false)
	}
	gunzipTool.Run = func(rc *tool.RunContext, args []string) int {
		return run(rc, gunzipTool, args, true, false)
	}
	zcatTool.Run = func(rc *tool.RunContext, args []string) int {
		return run(rc, zcatTool, args, true, true)
	}
	tool.Register(gzipTool)
	tool.Register(gunzipTool)
	tool.Register(zcatTool)
}

// gzip exit codes (per the manual): errors trump warnings.
const (
	exOK      = 0
	exError   = 1
	exWarning = 2
)

func worse(a, b int) int {
	if a == exError || b == exError {
		return exError
	}
	if a == exWarning || b == exWarning {
		return exWarning
	}
	return exOK
}

func run(rc *tool.RunContext, t *tool.Tool, args []string, aliasDecompress, aliasStdout bool) int {
	level, args := extractLevel(args)

	fs := tool.NewFlags(t.Name)
	decompress := fs.BoolP("decompress", "d", false, "decompress")
	stdout := fs.BoolP("stdout", "c", false, "write on standard output, keep original files unchanged")
	force := fs.BoolP("force", "f", false, "force overwrite of output file")
	keep := fs.BoolP("keep", "k", false, "keep (don't delete) input files")
	fast := fs.Bool("fast", false, "compress faster (same as -1)")
	best := fs.Bool("best", false, "compress better (same as -9)")
	noName := fs.BoolP("no-name", "n", false, "do not save or restore the original file name and time stamp")
	name := fs.BoolP("name", "N", false, "save or restore the original file name and time stamp")

	operands, code := tool.Parse(rc, t, fs, args)
	if code >= 0 {
		return code
	}

	if level < 0 {
		if *fast {
			level = 1
		}
		if *best {
			level = 9
		}
	}
	o := opts{
		decompress: *decompress || aliasDecompress,
		stdout:     *stdout || aliasStdout,
		force:      *force,
		keep:       *keep,
		level:      level,
		noName:     *noName,
		name:       *name,
	}

	if len(operands) == 0 {
		operands = []string{"-"}
	}
	exit := exOK
	for _, op := range operands {
		exit = worse(exit, processOperand(rc, t, op, o))
	}
	return exit
}

type opts struct {
	decompress bool
	stdout     bool
	force      bool
	keep       bool
	level      int // 1..9, or -1 = gzip default (6)
	noName     bool
	name       bool
}

// extractLevel pre-scans args for the -1..-9 numeric shorthands (which
// have no long-option spelling we could give pflag) and removes them,
// including digits embedded in short-flag clusters ("-9c"). Scanning
// stops at the "--" terminator; operands are never touched.
func extractLevel(args []string) (int, []string) {
	level := -1
	out := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			out = append(out, args[i:]...)
			break
		}
		if len(a) >= 2 && a[0] == '-' && a[1] != '-' {
			kept := []byte{'-'}
			for j := 1; j < len(a); j++ {
				if a[j] >= '1' && a[j] <= '9' {
					level = int(a[j] - '0')
					continue
				}
				kept = append(kept, a[j])
			}
			if len(kept) > 1 {
				out = append(out, string(kept))
			}
			continue
		}
		out = append(out, a)
	}
	return level, out
}

// decompSuffixes are the suffixes gunzip knows, with their
// replacements (GNU gzip manual: .gz, .z stripped; .tgz/.taz → .tar).
// Most specific first: ".tgz" also ends in ".gz".
var decompSuffixes = []struct{ suf, repl string }{
	{".tgz", ".tar"},
	{".taz", ".tar"},
	{".gz", ""},
	{".z", ""},
}

func processOperand(rc *tool.RunContext, t *tool.Tool, op string, o opts) int {
	if op == "-" {
		return processStream(rc, t, "stdin", rc.In, o)
	}

	path := rc.Path(op)
	fi, err := os.Stat(path)
	if err != nil {
		fmt.Fprintf(rc.Err, "%s: %s: No such file or directory\n", t.Name, op)
		return exError
	}
	if fi.IsDir() {
		fmt.Fprintf(rc.Err, "%s: %s is a directory -- ignored\n", t.Name, op)
		return exWarning
	}

	// Derive the output path (suffix added on compress, removed on
	// decompress). With -c the name is irrelevant: zcat & gzip -dc
	// work on any filename.
	var outPath, outDisplay string
	if !o.stdout {
		if o.decompress {
			found := ""
			repl := ""
			for _, s := range decompSuffixes {
				if strings.HasSuffix(op, s.suf) && len(op) > len(s.suf) {
					found, repl = s.suf, s.repl
					break
				}
			}
			if found == "" {
				fmt.Fprintf(rc.Err, "%s: %s: unknown suffix -- ignored\n", t.Name, op)
				return exWarning
			}
			outPath = strings.TrimSuffix(path, found) + repl
			outDisplay = strings.TrimSuffix(op, found) + repl
		} else {
			if !o.force {
				for _, s := range decompSuffixes {
					if strings.HasSuffix(op, s.suf) && len(op) > len(s.suf) {
						fmt.Fprintf(rc.Err, "%s: %s already has %s suffix -- unchanged\n", t.Name, op, s.suf)
						return exWarning
					}
				}
			}
			outPath = path + ".gz"
			outDisplay = op + ".gz"
		}
		if _, err := os.Lstat(outPath); err == nil && !o.force {
			fmt.Fprintf(rc.Err, "%s: %s already exists; not overwritten\n", t.Name, outDisplay)
			return exWarning
		}
	}

	in, err := os.Open(path)
	if err != nil {
		fmt.Fprintf(rc.Err, "%s: %s: %v\n", t.Name, op, err)
		return exError
	}
	defer in.Close()

	var out io.Writer = rc.Out
	var outFile *os.File
	if !o.stdout {
		outFile, err = os.OpenFile(outPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
		if err != nil {
			fmt.Fprintf(rc.Err, "%s: %s: %v\n", t.Name, outDisplay, err)
			return exError
		}
		out = outFile
	}

	res := pump(rc, t, op, in, out, o, fi)
	code := res.code

	if outFile != nil {
		if err := outFile.Close(); err != nil && code == exOK {
			fmt.Fprintf(rc.Err, "%s: %s: %v\n", t.Name, outDisplay, err)
			code = exError
		}
		if code == exError {
			os.Remove(outPath) // never leave a truncated output behind
			return code
		}
		// gzip preserves the mode and timestamps of the input file.
		os.Chmod(outPath, fi.Mode().Perm())

		mtime := fi.ModTime()
		if o.decompress && o.name && !res.modTime.IsZero() {
			mtime = res.modTime
		}
		os.Chtimes(outPath, mtime, mtime)

		if o.decompress && o.name && res.name != "" {
			dir := filepath.Dir(outPath)
			targetPath := filepath.Join(dir, res.name)
			if targetPath != outPath {
				if _, err := os.Lstat(targetPath); err == nil && !o.force {
					fmt.Fprintf(rc.Err, "%s: %s: file already exists; not restoring name\n", t.Name, res.name)
					code = worse(code, exWarning)
				} else {
					os.Remove(targetPath)
					if err := os.Rename(outPath, targetPath); err != nil {
						fmt.Fprintf(rc.Err, "%s: restoring name failed: %v\n", t.Name, err)
						code = worse(code, exWarning)
					} else {
						outPath = targetPath
					}
				}
			}
		}

		if !o.keep {
			// Close the source before removing it: Windows refuses to remove a
			// file that is still open (unix allows the unlink-while-open). The
			// deferred in.Close() above then no-ops on the already-closed file.
			in.Close()
			if err := os.Remove(path); err != nil {
				fmt.Fprintf(rc.Err, "%s: %s: %v\n", t.Name, op, err)
				code = worse(code, exWarning)
			}
		}
	}
	return code
}

// processStream handles stdin → stdout ('-' or no operands).
func processStream(rc *tool.RunContext, t *tool.Tool, label string, in io.Reader, o opts) int {
	if in == nil {
		fmt.Fprintf(rc.Err, "%s: no input available\n", t.Name)
		return exError
	}
	return pump(rc, t, label, in, rc.Out, o, nil).code
}

type pumpResult struct {
	code    int
	name    string
	modTime time.Time
}

// pump runs one compression or decompression copy. fi is the input
// file's metadata for the gzip header (nil for streams).
func pump(rc *tool.RunContext, t *tool.Tool, label string, in io.Reader, out io.Writer, o opts, fi os.FileInfo) pumpResult {
	if o.decompress {
		zr, err := gzip.NewReader(in)
		if err != nil {
			return pumpResult{code: readErr(rc, t, label, err)}
		}
		// multistream (concatenated members) is the gzip.Reader
		// default, matching gunzip.
		if _, err := io.Copy(out, zr); err != nil {
			return pumpResult{code: readErr(rc, t, label, err)}
		}
		name := zr.Name
		modTime := zr.ModTime
		if err := zr.Close(); err != nil {
			return pumpResult{code: readErr(rc, t, label, err)}
		}
		return pumpResult{code: exOK, name: name, modTime: modTime}
	}

	level := o.level
	if level < 0 {
		level = gzip.DefaultCompression
	}
	zw, err := gzip.NewWriterLevel(out, level)
	if err != nil {
		fmt.Fprintf(rc.Err, "%s: %v\n", t.Name, err)
		return pumpResult{code: exError}
	}
	saveHeader := true
	if o.noName && !o.name {
		saveHeader = false
	}
	if fi != nil && saveHeader {
		// Default --name behavior: store original base name + mtime.
		zw.Name = filepath.Base(fi.Name())
		zw.ModTime = fi.ModTime()
	}
	if _, err := io.Copy(zw, in); err != nil {
		fmt.Fprintf(rc.Err, "%s: %s: %v\n", t.Name, label, err)
		return pumpResult{code: exError}
	}
	if err := zw.Close(); err != nil {
		fmt.Fprintf(rc.Err, "%s: %s: %v\n", t.Name, label, err)
		return pumpResult{code: exError}
	}
	return pumpResult{code: exOK}
}

func readErr(rc *tool.RunContext, t *tool.Tool, label string, err error) int {
	switch {
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		fmt.Fprintf(rc.Err, "%s: %s: unexpected end of file\n", t.Name, label)
	case errors.Is(err, gzip.ErrHeader):
		fmt.Fprintf(rc.Err, "%s: %s: not in gzip format\n", t.Name, label)
	default:
		fmt.Fprintf(rc.Err, "%s: %s: %v\n", t.Name, label, err)
	}
	return exError
}
