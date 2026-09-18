// Package treecmd implements a pure-Go tree(1): a recursive, indented listing of
// a directory using box-drawing connectors, ending with a directory/file count.
//
// Defaults match the classic tool: hidden entries (dot-files) are omitted unless
// -a is given. -L limits depth, -d lists directories only. The opt-in --agentic
// flag additionally skips .gitignore'd and noise paths (node_modules/.git/
// vendor/…) via coreutils/pkg/ignore — useful for orienting in a codebase
// without drowning in dependency trees — and reports how many paths it hid.
package treecmd

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/qiangli/coreutils/pkg/ignore"
	"github.com/qiangli/coreutils/tool"
)

var cmd = &tool.Tool{
	Name:     "tree",
	Synopsis: "List the contents of directories in a tree-like format.",
	Usage:    "tree [-ad] [-L level] [--agentic] [DIR...]",
}

func init() { cmd.Run = run; tool.Register(cmd) }

type walkOpts struct {
	all      bool
	dirsOnly bool
	level    int // max depth; <=0 = unlimited
	matcher  *ignore.Matcher
}

type counter struct{ dirs, files int }

func run(rc *tool.RunContext, args []string) int {
	fs := tool.NewFlags(cmd.Name)
	all := fs.BoolP("all", "a", false, "list hidden (dot) files too")
	dirsOnly := fs.BoolP("dirs-only", "d", false, "list directories only")
	level := fs.IntP("level", "L", -1, "descend only LEVEL directories deep")
	agentic := fs.Bool("agentic", false, "opt-in: also skip .gitignore'd and noise paths (node_modules, .git, …)")

	operands, code := tool.Parse(rc, cmd, fs, args)
	if code >= 0 {
		return code
	}
	roots := operands
	if len(roots) == 0 {
		roots = []string{"."}
	}

	status := 0
	var total counter
	for _, root := range roots {
		o := walkOpts{all: *all, dirsOnly: *dirsOnly, level: *level}
		if *agentic {
			o.matcher = ignore.New(rc.Path(root))
		}
		fmt.Fprintln(rc.Out, root)
		if err := walk(rc, rc.Path(root), "", 1, o, &total); err != nil {
			fmt.Fprintf(rc.Err, "tree: %s: %v\n", root, err)
			status = 1
		}
		if n := o.matcher.Hidden(); n > 0 {
			fmt.Fprintf(rc.Err, "tree: --agentic skipped %d ignored path(s)\n", n)
		}
	}

	dirWord, fileWord := "directories", "files"
	if total.dirs == 1 {
		dirWord = "directory"
	}
	if total.files == 1 {
		fileWord = "file"
	}
	fmt.Fprintf(rc.Out, "\n%d %s, %d %s\n", total.dirs, dirWord, total.files, fileWord)
	return status
}

func walk(rc *tool.RunContext, dir, prefix string, depth int, o walkOpts, c *counter) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	// Filter, then render.
	var shown []os.DirEntry
	for _, e := range entries {
		name := e.Name()
		if !o.all && len(name) > 0 && name[0] == '.' {
			continue
		}
		full := filepath.Join(dir, name)
		if o.matcher.Skip(full, e.IsDir()) {
			continue
		}
		if o.dirsOnly && !e.IsDir() {
			continue
		}
		shown = append(shown, e)
	}
	sort.Slice(shown, func(i, j int) bool { return shown[i].Name() < shown[j].Name() })

	for i, e := range shown {
		last := i == len(shown)-1
		connector, childPrefix := "├── ", "│   "
		if last {
			connector, childPrefix = "└── ", "    "
		}
		fmt.Fprintf(rc.Out, "%s%s%s\n", prefix, connector, e.Name())
		if e.IsDir() {
			c.dirs++
			if o.level <= 0 || depth < o.level {
				_ = walk(rc, filepath.Join(dir, e.Name()), prefix+childPrefix, depth+1, o, c)
			}
		} else {
			c.files++
		}
	}
	return nil
}
