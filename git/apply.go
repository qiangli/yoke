package git

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// This file implements `apply [--check] <patch>...`: unified-diff
// application to the worktree (never the index, never a commit — host
// parity for plain apply). Hunks apply anchored at their @@ positions
// with exact context matching (no fuzz); any mismatch fails the whole
// run with the tree untouched, like host git.
//
// Supported shapes: modified / new / deleted files, renames via
// rename-from/to lines, exec-bit flips, multiple files and multiple
// patch files (all-or-nothing across all of them), --check.
// Loud ErrUnsupported: binary patches, mode-only changes beyond the
// exec bit, -p<n>, -R, --cached/--index/--3way/--reject/whitespace and
// every other flag, stdin ("-") — Exec carries no stdin channel.

type applyOp struct {
	kind byte // ' ', '-', '+'
	line string
}

type applyHunk struct {
	oldStart int
	oldCount int
	ops      []applyOp
	// noNewlineNew records a "\ No newline" marker after a '+' line:
	// the result file must not end with a newline.
	noNewlineNew bool
}

type applyFile struct {
	oldPath string // "" = create
	newPath string // "" = delete
	hunks   []applyHunk
	execBit *bool // non-nil = chmod to 0755 (true) / 0644 (false)
}

func stripP1(p string) string {
	if i := strings.Index(p, "/"); i >= 0 {
		return p[i+1:]
	}
	return p
}

func parseApplyPatch(text string) ([]applyFile, error) {
	var files []applyFile
	var cur *applyFile
	var hunk *applyHunk
	var renameFrom, renameTo string
	var oldMode, newMode string
	var isNew, isDel, binary bool
	flushHunk := func() {
		if cur != nil && hunk != nil {
			// Drop terminator empties: strings.Split leaves a final
			// "" that is not a hunk line. Only trim while the
			// old-side op count still exceeds the @@ declaration,
			// so genuine empty context lines survive.
			for len(hunk.ops) > 0 {
				last := hunk.ops[len(hunk.ops)-1]
				if last.kind != ' ' || last.line != "" {
					break
				}
				n := 0
				for _, op := range hunk.ops {
					if op.kind != '+' {
						n++
					}
				}
				if n <= hunk.oldCount {
					break
				}
				hunk.ops = hunk.ops[:len(hunk.ops)-1]
			}
			cur.hunks = append(cur.hunks, *hunk)
			hunk = nil
		}
	}
	// flushFile closes the current file section. A non-nil return is
	// ErrUnsupported (a mode change beyond the exec bit); anything else
	// is a parse failure.
	flushFile := func() error {
		flushHunk()
		if cur == nil {
			return nil
		}
		if binary {
			return ErrUnsupported
		}
		// Rename lines precede ---/+++ in git output; they win.
		if renameFrom != "" {
			cur.oldPath = renameFrom
		}
		if renameTo != "" {
			cur.newPath = renameTo
		}
		if isNew {
			cur.oldPath = ""
		}
		if isDel {
			cur.newPath = ""
		}
		if oldMode != "" || newMode != "" {
			bit, ok := execBitFlip(oldMode, newMode)
			if !ok {
				return ErrUnsupported
			}
			cur.execBit = &bit
		}
		files = append(files, *cur)
		cur = nil
		return nil
	}
	resetSection := func() {
		isNew, isDel = false, false
		renameFrom, renameTo = "", ""
		oldMode, newMode = "", ""
		binary = false
	}
	lines := strings.Split(text, "\n")
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		switch {
		case strings.HasPrefix(line, "diff --git "):
			if err := flushFile(); err != nil {
				return nil, err
			}
			cur = &applyFile{}
			resetSection()
		case strings.HasPrefix(line, "--- "):
			if cur == nil {
				cur = &applyFile{}
				resetSection()
			}
			flushHunk()
			raw := line[4:]
			if raw == "/dev/null" {
				isNew = true
			} else {
				cur.oldPath = stripP1(raw)
			}
		case strings.HasPrefix(line, "+++ "):
			if cur == nil {
				return nil, fmt.Errorf("apply: +++ without ---")
			}
			raw := line[4:]
			if raw == "/dev/null" {
				isDel = true
			} else {
				cur.newPath = stripP1(raw)
			}
		case strings.HasPrefix(line, "rename from "):
			renameFrom = stripP1(line[len("rename from "):])
		case strings.HasPrefix(line, "rename to "):
			renameTo = stripP1(line[len("rename to "):])
		case strings.HasPrefix(line, "old mode"):
			oldMode = strings.TrimSpace(strings.TrimPrefix(line, "old mode"))
		case strings.HasPrefix(line, "new mode"):
			newMode = strings.TrimSpace(strings.TrimPrefix(line, "new mode"))
		case strings.HasPrefix(line, "new file mode") || strings.HasPrefix(line, "deleted file mode") || strings.HasPrefix(line, "index ") || strings.HasPrefix(line, "similarity ") || strings.HasPrefix(line, "dissimilarity "):
			// Informational only.
		case strings.HasPrefix(line, "Binary files ") || strings.HasPrefix(line, "GIT binary patch"):
			binary = true
		case strings.HasPrefix(line, "@@ "):
			if cur == nil {
				return nil, fmt.Errorf("apply: hunk outside file")
			}
			if binary {
				return nil, fmt.Errorf("apply: binary patch")
			}
			flushHunk()
			rest := strings.TrimPrefix(line, "@@ ")
			end := strings.Index(rest, " @@")
			if end < 0 {
				return nil, fmt.Errorf("apply: bad hunk header")
			}
			parts := strings.Fields(rest[:end])
			if len(parts) < 2 {
				return nil, fmt.Errorf("apply: bad hunk header")
			}
			os, oc := parseHunkRange(parts[0])
			_, _ = parseHunkRange(parts[1])
			hunk = &applyHunk{oldStart: os, oldCount: oc}
		case hunk != nil && len(line) > 0 && (line[0] == ' ' || line[0] == '-' || line[0] == '+'):
			hunk.ops = append(hunk.ops, applyOp{kind: line[0], line: line[1:]})
		case hunk != nil && strings.HasPrefix(line, "\\ "):
			// "\ No newline at end of file": the previous '+' line
			// decides the result's trailing newline.
			if n := len(hunk.ops); n > 0 && hunk.ops[n-1].kind == '+' {
				hunk.noNewlineNew = true
			}
		case line == "" && hunk != nil:
			// Blank line inside a hunk is a context line with empty
			// content (unified diff drops the leading space).
			hunk.ops = append(hunk.ops, applyOp{kind: ' ', line: ""})
		case line == "":
			// Between sections.
		default:
			if hunk != nil {
				return nil, fmt.Errorf("apply: unrecognized input")
			}
			// Stray text between sections (e.g. mail headers): ignore.
		}
	}
	if err := flushFile(); err != nil {
		return nil, err
	}
	return files, nil
}

func parseHunkRange(s string) (start, count int) {
	s = strings.TrimPrefix(s, "-")
	s = strings.TrimPrefix(s, "+")
	if i := strings.Index(s, ","); i >= 0 {
		start, _ = strconv.Atoi(s[:i])
		count, _ = strconv.Atoi(s[i+1:])
		return start, count
	}
	start, _ = strconv.Atoi(s)
	return start, 1
}

// execBitFlip maps an old/new mode pair to a chmod decision. Only the
// executable bit is meaningful here; anything else stays loud.
func execBitFlip(oldMode, newMode string) (bool, bool) {
	norm := func(m string) string {
		m = strings.TrimSpace(m)
		if len(m) > 6 {
			m = m[len(m)-6:]
		}
		return m
	}
	o, n := norm(oldMode), norm(newMode)
	if o == n {
		return false, true // no-op marker, not a decision
	}
	if o == "100644" && n == "100755" {
		return true, true
	}
	if o == "100755" && n == "100644" {
		return false, true
	}
	return false, false
}

// applyOneFile verifies hunk against current lines and returns the new
// lines plus trailing-newline state. Pure: no filesystem writes.
func applyOneFile(current []string, endsNewline bool, hunks []applyHunk) ([]string, bool, error) {
	out := []string{}
	pos := 0 // consumed from current (0-based)
	newline := endsNewline
	for hi, h := range hunks {
		start := h.oldStart - 1 // to 0-based; 0 for insertions/new files
		if start < 0 {
			start = 0
		}
		if start < pos {
			return nil, false, fmt.Errorf("apply: overlapping hunks")
		}
		out = append(out, current[pos:start]...)
		pos = start
		for _, op := range h.ops {
			switch op.kind {
			case ' ':
				if pos >= len(current) || current[pos] != op.line {
					return nil, false, fmt.Errorf("apply: hunk %d context mismatch", hi+1)
				}
				out = append(out, current[pos])
				pos++
			case '-':
				if pos >= len(current) || current[pos] != op.line {
					return nil, false, fmt.Errorf("apply: hunk %d context mismatch", hi+1)
				}
				pos++
			case '+':
				out = append(out, op.line)
			}
		}
		if h.noNewlineNew {
			newline = false
		} else if len(h.ops) > 0 && h.ops[len(h.ops)-1].kind == '+' {
			// A trailing added line without a marker ends the file
			// with a newline (standard patch text).
			newline = true
		}
		_ = hi
	}
	out = append(out, current[pos:]...)
	return out, newline, nil
}

func splitTextLines(content string) ([]string, bool) {
	endsNewline := strings.HasSuffix(content, "\n")
	lines := strings.Split(content, "\n")
	if endsNewline {
		lines = lines[:len(lines)-1]
	}
	return lines, endsNewline
}

func joinTextLines(lines []string, endsNewline bool) string {
	s := strings.Join(lines, "\n")
	if endsNewline && len(lines) > 0 {
		s += "\n"
	}
	return s
}

func nativeApply(_ context.Context, dir string, args []string) (*ExecResult, error) {
	check := false
	var patches []string
	for _, a := range args {
		switch a {
		case "--check":
			check = true
		default:
			if strings.HasPrefix(a, "-") {
				return nil, ErrUnsupported
			}
			patches = append(patches, a)
		}
	}
	if len(patches) == 0 {
		return nil, ErrUnsupported
	}

	// Phase 0: read + parse every patch first (a missing file fails the
	// whole run before anything is verified or written).
	type plan struct {
		rel      string
		delete   bool
		create   bool
		lines    []string
		newline  bool
		execBit  *bool
		origPath string
	}
	var plans []plan
	for _, p := range patches {
		full := p
		if !filepath.IsAbs(full) {
			full = filepath.Join(dir, filepath.FromSlash(p))
		}
		raw, err := os.ReadFile(full)
		if err != nil {
			return &ExecResult{
				Stderr:   fmt.Sprintf("error: can't open patch '%s': %v\n", p, err),
				ExitCode: 128,
			}, nil
		}
		files, err := parseApplyPatch(string(raw))
		if err != nil {
			if err == ErrUnsupported {
				return nil, ErrUnsupported
			}
			return &ExecResult{Stderr: fmt.Sprintf("fatal: unrecognized input\n"), ExitCode: 128}, nil
		}
		for _, f := range files {
			oldRel, newRel := f.oldPath, f.newPath
			if oldRel == "" && newRel == "" {
				return &ExecResult{Stderr: "fatal: unrecognized input\n", ExitCode: 128}, nil
			}
			target := newRel
			if target == "" {
				target = oldRel
			}
			if target == "" || strings.HasPrefix(target, "/") || target == "/dev/null" {
				return &ExecResult{Stderr: "fatal: unrecognized input\n", ExitCode: 128}, nil
			}
			fullTarget := filepath.Join(dir, filepath.FromSlash(target))
			cur, rerr := os.ReadFile(fullTarget)
			exists := rerr == nil
			switch {
			case oldRel == "":
				// New file.
				if exists {
					return &ExecResult{Stderr: fmt.Sprintf("error: %s: already exists in working directory\n", target), ExitCode: 1}, nil
				}
				if len(f.hunks) == 0 && f.execBit == nil {
					return &ExecResult{Stderr: "fatal: unrecognized input\n", ExitCode: 128}, nil
				}
				var lines []string
				newline := true
				for _, h := range f.hunks {
					if h.oldCount != 0 {
						return &ExecResult{Stderr: fmt.Sprintf("error: patch failed: %s\n", target), ExitCode: 1}, nil
					}
					for _, op := range h.ops {
						if op.kind != '+' {
							return &ExecResult{Stderr: fmt.Sprintf("error: patch failed: %s\n", target), ExitCode: 1}, nil
						}
						lines = append(lines, op.line)
					}
					if h.noNewlineNew {
						newline = false
					}
				}
				plans = append(plans, plan{rel: target, create: true, lines: lines, newline: newline, execBit: f.execBit})
			case newRel == "":
				// Deleted file.
				if !exists {
					return &ExecResult{Stderr: fmt.Sprintf("error: %s: No such file or directory\n", target), ExitCode: 1}, nil
				}
				cls, _ := splitTextLines(string(cur))
				if len(f.hunks) == 0 {
					return &ExecResult{Stderr: "fatal: unrecognized input\n", ExitCode: 128}, nil
				}
				total := 0
				for _, h := range f.hunks {
					total += h.oldCount
				}
				if total != len(cls) {
					return &ExecResult{Stderr: fmt.Sprintf("error: patch failed: %s\n", target), ExitCode: 1}, nil
				}
				if _, _, err := applyOneFile(cls, true, f.hunks); err != nil {
					return &ExecResult{Stderr: fmt.Sprintf("error: patch failed: %s\n", target), ExitCode: 1}, nil
				}
				plans = append(plans, plan{rel: target, delete: true, execBit: f.execBit, origPath: oldRel})
			default:
				// Modified (possibly renamed) file.
				src := fullTarget
				if oldRel != newRel {
					src = filepath.Join(dir, filepath.FromSlash(oldRel))
				}
				rawCur, rerr := os.ReadFile(src)
				if rerr != nil {
					return &ExecResult{Stderr: fmt.Sprintf("error: %s: No such file or directory\n", oldRel), ExitCode: 1}, nil
				}
				cls, nl := splitTextLines(string(rawCur))
				newLines, newNl, err := applyOneFile(cls, nl, f.hunks)
				if err != nil {
					return &ExecResult{Stderr: fmt.Sprintf("error: patch failed: %s\n", target), ExitCode: 1}, nil
				}
				plans = append(plans, plan{rel: target, lines: newLines, newline: newNl, execBit: f.execBit, origPath: oldRel})
			}
		}
	}

	if check {
		return &ExecResult{Stdout: ""}, nil
	}
	for _, pl := range plans {
		full := filepath.Join(dir, filepath.FromSlash(pl.rel))
		switch {
		case pl.delete:
			if err := os.Remove(full); err != nil {
				return nil, ErrUnsupported
			}
		case pl.create:
			if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
				return nil, ErrUnsupported
			}
			if err := os.WriteFile(full, []byte(joinTextLines(pl.lines, pl.newline)), 0o644); err != nil {
				return nil, ErrUnsupported
			}
		default:
			if pl.origPath != "" && pl.origPath != pl.rel {
				if err := os.Remove(filepath.Join(dir, filepath.FromSlash(pl.origPath))); err != nil && !os.IsNotExist(err) {
					return nil, ErrUnsupported
				}
			}
			if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
				return nil, ErrUnsupported
			}
			if err := os.WriteFile(full, []byte(joinTextLines(pl.lines, pl.newline)), 0o644); err != nil {
				return nil, ErrUnsupported
			}
		}
		if pl.execBit != nil {
			mode := os.FileMode(0o644)
			if *pl.execBit {
				mode = os.FileMode(0o755)
			}
			if err := os.Chmod(full, mode); err != nil {
				return nil, ErrUnsupported
			}
		}
	}
	return &ExecResult{Stdout: ""}, nil
}
