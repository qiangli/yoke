// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package reduce

import (
	"bytes"
	"path/filepath"
)

// CanonicalizeHome replaces occurrences of the current absolute home directory
// with the literal $HOME token. A match must name the home itself or continue
// through a path separator; lookalike path components are left untouched.
// Unsafe homes (empty, relative, or a filesystem root) disable canonicalization.
func CanonicalizeHome(in []byte, home string) []byte {
	if len(in) == 0 || !safeHome(home) {
		return in
	}

	home = filepath.Clean(home)
	needle := []byte(home)
	var out []byte
	scan, last := 0, 0
	for scan < len(in) {
		rel := bytes.Index(in[scan:], needle)
		if rel < 0 {
			break
		}
		at := scan + rel
		end := at + len(needle)
		if homeLeftBoundary(in, at) && homeRightBoundary(in, end) {
			if out == nil {
				out = make([]byte, 0, len(in)-len(needle)+len("$HOME"))
			}
			out = append(out, in[last:at]...)
			out = append(out, "$HOME"...)
			last = end
			scan = end
			continue
		}
		scan = at + 1
	}
	if out == nil {
		return in
	}
	return append(out, in[last:]...)
}

func safeHome(home string) bool {
	if home == "" || !filepath.IsAbs(home) {
		return false
	}
	home = filepath.Clean(home)
	volume := filepath.VolumeName(home)
	rest := home[len(volume):]
	return rest != string(filepath.Separator)
}

func homeLeftBoundary(in []byte, at int) bool {
	if at == 0 {
		return true
	}
	return !homePathByte(in[at-1])
}

func homeRightBoundary(in []byte, end int) bool {
	if end == len(in) {
		return true
	}
	if in[end] == byte(filepath.Separator) {
		return true
	}
	switch in[end] {
	case ' ', '\t', '\r', '\n', '\'', '"', '`', ':', ',', ';', '!', '?', ')', ']', '}':
		return true
	default:
		return false
	}
}

func homePathByte(b byte) bool {
	return b == byte(filepath.Separator) || b == '/' || b == '\\' || b == '.' || b == '_' || b == '-' ||
		b >= '0' && b <= '9' || b >= 'A' && b <= 'Z' || b >= 'a' && b <= 'z'
}
