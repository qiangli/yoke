// Package tarcmd implements tar(1) — the GNU tar common surface:
// -c create, -x extract, -t list, -f FILE ('-' = stdin/stdout),
// -z gzip, -v verbose, -C DIR, --exclude=PATTERN (create), and
// --strip-components=N (extract).
//
// Portions adapted from https://github.com/u-root/u-root
// pkg/tarutil/tar.go and cmds/core/tar/tar.go (BSD-3-Clause).
// Changes: rewired to tool framework; GNU flag surface incl. old-style
// first-word options ("tar xzf a.tgz"); GNU -t/-tv listing; mode+mtime
// preservation; symlink/hardlink entries; path-traversal refusal on
// extract; --strip-components; member-operand selection; gzip
// auto-detection on read.
package tarcmd

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/qiangli/coreutils/tool"
)

var cmd = &tool.Tool{
	Name:     "tar",
	Synopsis: "Create, list, or extract tar archives (optionally gzip-compressed).",
	Usage: "tar -c [-zv] -f ARCHIVE [-C DIR] [--exclude=PATTERN] FILE...\n" +
		"   or: tar -t [-zv] -f ARCHIVE [MEMBER...]\n" +
		"   or: tar -x [-zv] -f ARCHIVE [-C DIR] [--strip-components=N] [MEMBER...]",
}

// Run is wired in init: a literal would create an initialization
// cycle (run's flag-error paths reference cmd).
func init() { cmd.Run = run; tool.Register(cmd) }

func run(rc *tool.RunContext, args []string) int {
	args = expandOldStyle(args)
	if code := validateTarLongOptions(rc, args); code >= 0 {
		return code
	}
	orderedOperands := orderedCreateOperands(args)

	fs := tool.NewFlags(cmd.Name)
	create := fs.BoolP("create", "c", false, "create a new archive")
	extract := fs.BoolP("extract", "x", false, "extract files from an archive")
	list := fs.BoolP("list", "t", false, "list the contents of an archive")
	file := fs.StringP("file", "f", "", "use archive FILE ('-' means stdin/stdout)")
	gz := fs.BoolP("gzip", "z", false, "filter the archive through gzip")
	verbose := fs.BoolP("verbose", "v", false, "verbosely list files processed")
	chdir := fs.StringP("directory", "C", "", "change to DIR before performing any operations")
	fs.StringArray("exclude", nil, "exclude files matching PATTERN when creating an archive")
	strip := fs.Uint("strip-components", 0, "strip N leading components from file names on extraction")

	operands, code := tool.Parse(rc, cmd, fs, args)
	if code >= 0 {
		return code
	}

	modes := 0
	for _, m := range []bool{*create, *extract, *list} {
		if m {
			modes++
		}
	}
	if modes == 0 {
		return tool.UsageError(rc, cmd, "You must specify one of the '-ctx' options")
	}
	if modes > 1 {
		return tool.UsageError(rc, cmd, "You may not specify more than one '-ctx' option")
	}
	if *file == "" {
		return tool.UsageError(rc, cmd, "no archive file specified; use -f FILE ('-' means stdin/stdout)")
	}
	if fs.Changed("strip-components") && !*extract {
		return tool.UsageError(rc, cmd, "--strip-components is only supported with -x")
	}
	if fs.Changed("exclude") && !*create {
		return tool.UsageError(rc, cmd, "--exclude is only supported with -c")
	}

	// Base directory for member resolution (-C), resolved against the
	// invocation working directory.
	base := rc.Dir
	if *chdir != "" {
		base = rc.Path(*chdir)
		if fi, err := os.Stat(base); err != nil || !fi.IsDir() {
			fmt.Fprintf(rc.Err, "tar: %s: Cannot chdir: No such file or directory\n", *chdir)
			return 1
		}
	}
	if base == "" {
		base = "."
	}

	if *create {
		if len(operands) == 0 {
			return tool.UsageError(rc, cmd, "Cowardly refusing to create an empty archive")
		}
		return doCreate(rc, *file, base, orderedOperands, *gz, *verbose)
	}
	return doRead(rc, *file, base, operands, *gz, *verbose, *extract, int(*strip))
}

// expandOldStyle handles GNU tar's "old option style": a first operand
// of bundled option letters without a leading dash ("tar xzf a.tgz").
// Letters that take an argument (f, C) consume subsequent operands in
// order, exactly as GNU documents.
func expandOldStyle(args []string) []string {
	if len(args) == 0 {
		return args
	}
	first := args[0]
	if first == "" || strings.HasPrefix(first, "-") {
		return args
	}
	for _, r := range first {
		if !strings.ContainsRune("cxtzvfC", r) {
			return args // not old-style; leave as an operand
		}
	}
	rest := args[1:]
	var out []string
	for _, r := range first {
		out = append(out, "-"+string(r))
		if (r == 'f' || r == 'C') && len(rest) > 0 {
			out = append(out, rest[0])
			rest = rest[1:]
		}
	}
	return append(out, rest...)
}

// ---------------------------------------------------------------- create

type createOperand struct {
	name     string
	excludes []string
}

// tarLongOptions is the canonical long-option grammar used by both the
// ordered scanner and validation. Tar's option order is meaningful for
// --exclude, so accepting pflag's general unique-prefix extension here would
// let an abbreviation bypass the scanner.
var tarLongOptions = map[string]bool{
	"create": true, "extract": true, "list": true, "file": true,
	"gzip": true, "verbose": true, "directory": true, "exclude": true,
	"strip-components": true, "help": true, "version": true,
}

var tarLongOptionsWithValue = map[string]bool{
	"file": true, "directory": true, "exclude": true, "strip-components": true,
}

func validateTarLongOptions(rc *tool.RunContext, args []string) int {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			return -1
		}
		if !strings.HasPrefix(arg, "--") || len(arg) == 2 {
			continue
		}
		name, _, hasValue := strings.Cut(arg[2:], "=")
		if !tarLongOptions[name] {
			return tool.NotSupported(rc, cmd, arg)
		}
		if tarLongOptionsWithValue[name] && !hasValue {
			i++
		}
	}
	return -1
}

// orderedCreateOperands preserves the position-sensitive nature of GNU tar
// exclusions. Each file operand is paired with the exclusions that have been
// seen at that point; a later --exclude therefore cannot affect an earlier
// operand. The normal flag parser still owns validation and diagnostics.
func orderedCreateOperands(args []string) []createOperand {
	var operands []createOperand
	var excludes []string
	operand := func(name string) {
		operands = append(operands, createOperand{name: name, excludes: append([]string(nil), excludes...)})
	}

	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			for _, name := range args[i+1:] {
				operand(name)
			}
			break
		}
		if strings.HasPrefix(arg, "--") && len(arg) > 2 {
			name, value, equal := strings.Cut(arg[2:], "=")
			if tarLongOptions[name] {
				switch name {
				case "exclude":
					if !equal && i+1 < len(args) {
						i++
						value = args[i]
					}
					excludes = append(excludes, value)
				case "file", "directory", "strip-components":
					if !equal && i+1 < len(args) {
						i++
					}
				}
			}
			continue
		}
		if strings.HasPrefix(arg, "-") && arg != "-" {
			short := arg[1:]
			for pos := 0; pos < len(short); pos++ {
				switch short[pos] {
				case 'f', 'C':
					if pos+1 == len(short) && i+1 < len(args) {
						i++
					}
					pos = len(short)
				}
			}
			continue
		}
		operand(arg)
	}
	return operands
}

func doCreate(rc *tool.RunContext, archive, base string, operands []createOperand, gz, verbose bool) int {
	var w io.Writer
	var closers []io.Closer
	vout := rc.Out
	if archive == "-" {
		w = rc.Out
		vout = rc.Err // archive occupies stdout; GNU moves -v there
	} else {
		f, err := os.Create(rc.Path(archive))
		if err != nil {
			fmt.Fprintf(rc.Err, "tar: %s: Cannot create: %v\n", archive, err)
			return 1
		}
		w = f
		closers = append(closers, f)
	}
	if gz {
		zw := gzip.NewWriter(w)
		w = zw
		closers = append([]io.Closer{zw}, closers...)
	}
	tw := tar.NewWriter(w)

	failed := false
	warned := map[string]bool{}
	for _, op := range operands {
		if err := addToArchive(rc, tw, base, op.name, verbose, vout, warned, op.excludes); err != nil {
			fmt.Fprintf(rc.Err, "tar: %v\n", err)
			failed = true
		}
	}
	if err := tw.Close(); err != nil {
		fmt.Fprintf(rc.Err, "tar: %v\n", err)
		failed = true
	}
	for _, c := range closers {
		if err := c.Close(); err != nil {
			fmt.Fprintf(rc.Err, "tar: %v\n", err)
			failed = true
		}
	}
	if failed {
		return 1
	}
	return 0
}

// memberName converts an operand path (as the user typed it) into the
// name stored in the archive: forward slashes, no leading '/' or
// '../' (warned once each, GNU-style).
func memberName(rc *tool.RunContext, op string, warned map[string]bool) string {
	n := filepath.ToSlash(op)
	if strings.HasPrefix(n, "/") {
		if !warned["abs"] {
			fmt.Fprintln(rc.Err, "tar: Removing leading '/' from member names")
			warned["abs"] = true
		}
		n = strings.TrimLeft(n, "/")
	}
	for strings.HasPrefix(n, "../") {
		if !warned["dotdot"] {
			fmt.Fprintln(rc.Err, "tar: Removing leading '../' from member names")
			warned["dotdot"] = true
		}
		n = n[3:]
	}
	if n == "" {
		n = "."
	}
	return n
}

func addToArchive(rc *tool.RunContext, tw *tar.Writer, base, op string, verbose bool, vout io.Writer, warned map[string]bool, excludes []string) error {
	root := op
	if !filepath.IsAbs(op) {
		root = filepath.Join(base, op)
	}
	memberBase := memberName(rc, op, warned)

	return filepath.Walk(root, func(p string, info fs.FileInfo, err error) error {
		if err != nil {
			return fmt.Errorf("%s: Cannot stat: %w", p, err)
		}
		name := memberBase
		if rel, rerr := filepath.Rel(root, p); rerr == nil && rel != "." {
			rel = filepath.ToSlash(rel)
			if memberBase == "." {
				name = "./" + rel
			} else {
				name = path.Join(memberBase, rel)
			}
		}
		if excluded(name, excludes) {
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		var linkname string
		if info.Mode()&os.ModeSymlink != 0 {
			if linkname, err = os.Readlink(p); err != nil {
				return fmt.Errorf("%s: Cannot readlink: %w", p, err)
			}
		}
		hdr, err := tar.FileInfoHeader(info, linkname)
		if err != nil {
			return fmt.Errorf("%s: %w", p, err)
		}
		hdr.Name = name
		if info.IsDir() && !strings.HasSuffix(hdr.Name, "/") {
			hdr.Name += "/"
		}
		// Second-precision mtime, no atime/ctime: keeps the writer on
		// plain ustar for typical entries and the output deterministic.
		hdr.ModTime = hdr.ModTime.Truncate(time.Second)
		hdr.AccessTime = time.Time{}
		hdr.ChangeTime = time.Time{}

		switch {
		case info.Mode().IsRegular():
			f, err := os.Open(p)
			if err != nil {
				return fmt.Errorf("%s: Cannot open: %w", p, err)
			}
			if err := tw.WriteHeader(hdr); err != nil {
				f.Close()
				return err
			}
			if _, err := io.Copy(tw, f); err != nil {
				f.Close()
				return fmt.Errorf("%s: %w", p, err)
			}
			f.Close()
		case info.IsDir(), info.Mode()&os.ModeSymlink != 0:
			if err := tw.WriteHeader(hdr); err != nil {
				return err
			}
		default:
			// Sockets, devices, fifos: GNU warns and skips sockets;
			// devices need privileged probing we don't do.
			fmt.Fprintf(rc.Err, "tar: %s: file type not supported by pure-Go tar; skipped\n", p)
			return nil
		}
		if verbose {
			fmt.Fprintln(vout, hdr.Name)
		}
		return nil
	})
}

// excluded applies GNU tar --exclude patterns to archive member names, rather
// than host paths. Default patterns are unanchored: each component-boundary
// suffix is considered, so "foo" matches both "top/foo" and "top/sub/foo".
// Wildcards may span slashes. A directory is also tested without its tar
// trailing slash so a match can prune the whole subtree.
func excluded(name string, patterns []string) bool {
	plain := strings.TrimSuffix(name, "/")
	for _, pattern := range patterns {
		if tarGlobSuffixMatch(pattern, name) || (plain != name && tarGlobSuffixMatch(pattern, plain)) {
			return true
		}
	}
	return false
}

func tarGlobSuffixMatch(pattern, name string) bool {
	if tarGlobMatch(pattern, name) {
		return true
	}
	for i := 0; i < len(name); i++ {
		if name[i] == '/' && tarGlobMatch(pattern, name[i+1:]) {
			return true
		}
	}
	return false
}

// tarGlobMatch is the small wildcard dialect needed by GNU --exclude: '*'
// matches any sequence (including '/'), '?' one byte, and bracket expressions
// accept GNU shell-style ! (as well as ^) negation.
func tarGlobMatch(pattern, name string) bool {
	for {
		if pattern == "" {
			return name == ""
		}
		switch pattern[0] {
		case '*':
			for len(pattern) > 0 && pattern[0] == '*' {
				pattern = pattern[1:]
			}
			if pattern == "" {
				return true
			}
			for i := 0; ; i++ {
				if tarGlobMatch(pattern, name[i:]) {
					return true
				}
				if i == len(name) {
					return false
				}
			}
		case '?':
			if name == "" {
				return false
			}
			pattern, name = pattern[1:], name[1:]
		case '[':
			matched, consumed, ok := tarBracketMatch(pattern, name)
			if !ok || !matched {
				return false
			}
			pattern, name = pattern[consumed:], name[1:]
		case '\\':
			if len(pattern) == 1 || name == "" || pattern[1] != name[0] {
				return false
			}
			pattern, name = pattern[2:], name[1:]
		default:
			if name == "" || pattern[0] != name[0] {
				return false
			}
			pattern, name = pattern[1:], name[1:]
		}
	}
}

func tarBracketMatch(pattern, name string) (matched bool, consumed int, ok bool) {
	if len(pattern) < 2 || pattern[0] != '[' || name == "" {
		return false, 0, false
	}
	i := 1
	negated := false
	if i < len(pattern) && (pattern[i] == '!' || pattern[i] == '^') {
		negated = true
		i++
	}
	haveMember := false
	for i < len(pattern) {
		if pattern[i] == ']' && haveMember {
			if negated {
				matched = !matched
			}
			return matched, i + 1, true
		}
		lo := pattern[i]
		if pattern[i] == '[' && i+1 < len(pattern) && pattern[i+1] == ':' {
			end := strings.Index(pattern[i+2:], ":]")
			if end >= 0 {
				end += i + 2
				class := pattern[i+2 : end]
				if !tarNamedClass(class) {
					return false, 0, false
				}
				haveMember = true
				if tarNamedClassContains(class, name[0]) {
					matched = true
				}
				i = end + 2
				continue
			}
		}
		if lo == '\\' && i+1 < len(pattern) {
			i++
			lo = pattern[i]
		}
		i++
		haveMember = true
		hi := lo
		if i+1 < len(pattern) && pattern[i] == '-' && pattern[i+1] != ']' {
			i++
			hi = pattern[i]
			if hi == '\\' && i+1 < len(pattern) {
				i++
				hi = pattern[i]
			}
			i++
		}
		if lo <= name[0] && name[0] <= hi {
			matched = true
		}
	}
	return false, 0, false
}

func tarNamedClass(class string) bool {
	switch class {
	case "alnum", "alpha", "ascii", "blank", "cntrl", "digit", "graph", "lower", "print", "punct", "space", "upper", "word", "xdigit":
		return true
	default:
		return false
	}
}

func tarNamedClassContains(class string, c byte) bool {
	return (class == "ascii" && c <= 0x7f) ||
		(class == "digit" && c >= '0' && c <= '9') ||
		(class == "xdigit" && ((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F'))) ||
		(class == "alpha" && ((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z'))) ||
		(class == "alnum" && ((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9'))) ||
		(class == "lower" && c >= 'a' && c <= 'z') ||
		(class == "upper" && c >= 'A' && c <= 'Z') ||
		(class == "blank" && (c == ' ' || c == '\t')) ||
		(class == "space" && (c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\v' || c == '\f')) ||
		(class == "cntrl" && (c < 0x20 || c == 0x7f)) ||
		(class == "print" && c >= 0x20 && c <= 0x7e) ||
		(class == "graph" && c >= 0x21 && c <= 0x7e) ||
		(class == "punct" && ((c >= 0x21 && c <= 0x2f) || (c >= 0x3a && c <= 0x40) || (c >= 0x5b && c <= 0x60) || (c >= 0x7b && c <= 0x7e))) ||
		(class == "word" && ((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_'))
}

// ----------------------------------------------------------- list/extract

func doRead(rc *tool.RunContext, archive, base string, operands []string, gz, verbose, extract bool, strip int) int {
	var r io.Reader
	if archive == "-" {
		if rc.In == nil {
			fmt.Fprintln(rc.Err, "tar: no stdin available for '-f -'")
			return 1
		}
		r = rc.In
	} else {
		f, err := os.Open(rc.Path(archive))
		if err != nil {
			fmt.Fprintf(rc.Err, "tar: %s: Cannot open: %v\n", archive, err)
			return 1
		}
		defer f.Close()
		r = f
	}

	// gzip: explicit via -z, or auto-detected by magic (GNU tar
	// recognizes compressed archives automatically when reading).
	br := bufio.NewReader(r)
	magic, _ := br.Peek(2)
	if gz || (len(magic) == 2 && magic[0] == 0x1f && magic[1] == 0x8b) {
		zr, err := gzip.NewReader(br)
		if err != nil {
			fmt.Fprintf(rc.Err, "tar: %s: not in gzip format\n", archive)
			return 1
		}
		defer zr.Close()
		r = zr
	} else {
		r = br
	}

	tr := tar.NewReader(r)
	matched := make([]bool, len(operands))
	failed := false
	lister := &verboseLister{out: rc.Out}
	var dirs []dirFix

	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			fmt.Fprintf(rc.Err, "tar: %v\n", err)
			return 1
		}
		if hdr.Typeflag == tar.TypeXGlobalHeader {
			continue
		}
		if len(operands) > 0 && !matchMember(hdr.Name, operands, matched) {
			continue
		}

		if !extract {
			if verbose {
				lister.print(hdr)
			} else {
				fmt.Fprintln(rc.Out, hdr.Name)
			}
			continue
		}

		name, ok := stripComponents(hdr.Name, strip)
		if !ok {
			continue // fewer components than --strip-components: GNU skips silently
		}
		if verbose {
			fmt.Fprintln(rc.Out, hdr.Name)
		}
		if err := extractEntry(rc, tr, hdr, name, base, strip, &dirs); err != nil {
			fmt.Fprintf(rc.Err, "tar: %v\n", err)
			failed = true
		}
	}

	// Directory modes/mtimes are applied after their contents exist;
	// reverse order fixes children before parents.
	for i := len(dirs) - 1; i >= 0; i-- {
		d := dirs[i]
		os.Chmod(d.path, d.mode)
		os.Chtimes(d.path, d.mtime, d.mtime)
	}

	for i, op := range operands {
		if !matched[i] {
			fmt.Fprintf(rc.Err, "tar: %s: Not found in archive\n", op)
			failed = true
		}
	}
	if failed {
		fmt.Fprintln(rc.Err, "tar: Exiting with failure status due to previous errors")
		return 1
	}
	return 0
}

// matchMember implements GNU member-operand selection: an operand
// names a member exactly, or (as a directory) everything beneath it.
func matchMember(name string, operands []string, matched []bool) bool {
	hit := false
	for i, op := range operands {
		opc := strings.TrimSuffix(filepath.ToSlash(op), "/")
		nc := strings.TrimSuffix(name, "/")
		if nc == opc || strings.HasPrefix(name, opc+"/") {
			matched[i] = true
			hit = true
		}
	}
	return hit
}

func stripComponents(name string, n int) (string, bool) {
	if n <= 0 {
		return name, true
	}
	trailing := strings.HasSuffix(name, "/")
	parts := strings.Split(strings.TrimSuffix(name, "/"), "/")
	if len(parts) <= n {
		return "", false
	}
	out := strings.Join(parts[n:], "/")
	if trailing {
		out += "/"
	}
	return out, true
}

type dirFix struct {
	path  string
	mode  fs.FileMode
	mtime time.Time
}

// securePath resolves member against dest and refuses anything that
// escapes it (absolute names, '..', Windows volume tricks). Security
// non-negotiable: traversal entries are an error, never extracted.
func securePath(dest, member string) (string, error) {
	n := strings.TrimSuffix(member, "/")
	if n == "" || n == "." {
		return "", fmt.Errorf("empty")
	}
	if strings.HasPrefix(n, "/") || filepath.IsAbs(filepath.FromSlash(n)) || filepath.VolumeName(filepath.FromSlash(n)) != "" {
		return "", fmt.Errorf("member name %q is absolute: refusing to extract outside the target directory", member)
	}
	target := filepath.Join(dest, filepath.FromSlash(n))
	rel, err := filepath.Rel(dest, target)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("member name %q escapes the target directory via '..': refusing to extract", member)
	}
	return target, nil
}

func extractEntry(rc *tool.RunContext, tr *tar.Reader, hdr *tar.Header, name, dest string, strip int, dirs *[]dirFix) error {
	target, err := securePath(dest, name)
	if err != nil {
		if err.Error() == "empty" {
			return nil // "./" and similar no-op entries
		}
		return err
	}
	mode := hdr.FileInfo().Mode()
	mtime := hdr.ModTime

	switch hdr.Typeflag {
	case tar.TypeDir:
		if err := os.MkdirAll(target, 0o755); err != nil {
			return fmt.Errorf("%s: Cannot mkdir: %w", name, err)
		}
		*dirs = append(*dirs, dirFix{target, mode.Perm(), mtime})
		return nil

	case tar.TypeSymlink:
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		if fi, err := os.Lstat(target); err == nil && !fi.IsDir() {
			os.Remove(target)
		}
		if err := os.Symlink(filepath.FromSlash(hdr.Linkname), target); err != nil {
			if runtime.GOOS == "windows" {
				// Windows needs a privilege/developer mode for
				// symlinks: clear per-entry warning, keep going.
				fmt.Fprintf(rc.Err, "tar: %s: cannot create symlink to %q on windows (requires privilege); skipped\n", name, hdr.Linkname)
				return nil
			}
			return fmt.Errorf("%s: Cannot create symlink to %q: %w", name, hdr.Linkname, err)
		}
		return nil

	case tar.TypeLink:
		linkSrc, ok := stripComponents(hdr.Linkname, strip)
		if !ok {
			return fmt.Errorf("%s: hard link target %q was removed by --strip-components", name, hdr.Linkname)
		}
		src, err := securePath(dest, linkSrc)
		if err != nil {
			return fmt.Errorf("%s: hard link target %v", name, err)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		if fi, err := os.Lstat(target); err == nil && !fi.IsDir() {
			os.Remove(target)
		}
		if err := os.Link(src, target); err != nil {
			return fmt.Errorf("%s: Cannot hard link to %q: %w", name, hdr.Linkname, err)
		}
		return nil

	case tar.TypeChar, tar.TypeBlock, tar.TypeFifo:
		return fmt.Errorf("%s: special file type not supported by pure-Go tar", name)

	case tar.TypeReg:
		// fall through below
	default:
		fmt.Fprintf(rc.Err, "tar: %s: Unknown file type '%c', extracted as normal file\n", name, hdr.Typeflag)
	}

	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	if fi, err := os.Lstat(target); err == nil && !fi.IsDir() {
		os.Remove(target) // never write through a pre-existing symlink
	}
	f, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode.Perm())
	if err != nil {
		return fmt.Errorf("%s: Cannot open: %w", name, err)
	}
	if _, err := io.Copy(f, tr); err != nil {
		f.Close()
		return fmt.Errorf("%s: %w", name, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	os.Chmod(target, mode.Perm()) // exact mode, not umask-filtered
	os.Chtimes(target, mtime, mtime)
	return nil
}

// ------------------------------------------------------------- -tv lines

// verboseLister prints GNU 'tar -tv' lines: permissions owner/group
// size date name. Like GNU, the owner/group+size column starts at
// width 18 and grows when an entry exceeds it.
type verboseLister struct {
	out      io.Writer
	ugswidth int
}

func (l *verboseLister) print(hdr *tar.Header) {
	if l.ugswidth == 0 {
		l.ugswidth = 18
	}
	owner := hdr.Uname
	if owner == "" {
		owner = strconv.Itoa(hdr.Uid)
	}
	group := hdr.Gname
	if group == "" {
		group = strconv.Itoa(hdr.Gid)
	}
	ug := owner + "/" + group

	var sizeStr string
	switch hdr.Typeflag {
	case tar.TypeChar, tar.TypeBlock:
		sizeStr = fmt.Sprintf("%d,%d", hdr.Devmajor, hdr.Devminor)
	default:
		sizeStr = strconv.FormatInt(hdr.Size, 10)
	}

	pad := l.ugswidth - len(ug) - len(sizeStr)
	if pad < 1 {
		pad = 1
		l.ugswidth = len(ug) + len(sizeStr) + 1
	}

	name := hdr.Name
	switch hdr.Typeflag {
	case tar.TypeSymlink:
		name += " -> " + hdr.Linkname
	case tar.TypeLink:
		name += " link to " + hdr.Linkname
	}

	fmt.Fprintf(l.out, "%s %s%s%s %s %s\n",
		permString(hdr), ug, strings.Repeat(" ", pad), sizeStr,
		hdr.ModTime.Local().Format("2006-01-02 15:04"), name)
}

func permString(hdr *tar.Header) string {
	var t byte
	switch hdr.Typeflag {
	case tar.TypeDir:
		t = 'd'
	case tar.TypeSymlink:
		t = 'l'
	case tar.TypeLink:
		t = 'h'
	case tar.TypeChar:
		t = 'c'
	case tar.TypeBlock:
		t = 'b'
	case tar.TypeFifo:
		t = 'p'
	default:
		t = '-'
	}
	m := hdr.Mode
	b := []byte{t, '-', '-', '-', '-', '-', '-', '-', '-', '-'}
	rwx := "rwxrwxrwx"
	for i := 0; i < 9; i++ {
		if m&(1<<uint(8-i)) != 0 {
			b[i+1] = rwx[i]
		}
	}
	// setuid/setgid/sticky, GNU/ls style
	if m&0o4000 != 0 {
		if b[3] == 'x' {
			b[3] = 's'
		} else {
			b[3] = 'S'
		}
	}
	if m&0o2000 != 0 {
		if b[6] == 'x' {
			b[6] = 's'
		} else {
			b[6] = 'S'
		}
	}
	if m&0o1000 != 0 {
		if b[9] == 'x' {
			b[9] = 't'
		} else {
			b[9] = 'T'
		}
	}
	return string(b)
}
