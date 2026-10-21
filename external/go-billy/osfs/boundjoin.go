package osfs

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// This file is a yoke clean-room implementation written for S488.3a
// (remove the MPL-2.0 cyphar/filepath-securejoin dependency). It was
// written from POSIX path semantics and the observed BoundOS contract —
// ".." and absolute paths fold back inside the base dir, symlink
// resolutions that would leave the base dir never yield a usable
// outside path, symlink loops are an error — without reading the
// replaced module's source. Only its public signature as used by the
// permissive caller (os_bound.go) was consulted.

// ErrPathEscape is returned when a path cannot be confined to the
// BoundOS base directory.
var ErrPathEscape = errors.New("path escapes base directory")

// maxLinkExpansions caps symlink expansion while joining, so a link
// loop is an error instead of a hang.
const maxLinkExpansions = 255

// boundJoin joins unsafePath to root, keeping the result inside root:
//
//   - ".." components that would ascend above root are clamped: they
//     stay at root instead of escaping ("../file" becomes
//     "<root>/file").
//   - absolute unsafe paths are treated as relative to root.
//   - symlinks met while walking are expanded (absolute targets restart
//     the walk at root, relative targets splice in place, and nested
//     links are expanded in turn); targets that resolve outside root
//     are re-scoped under root, so they never yield a usable outside
//     path.
//   - a symlink loop or an unresolvable confinement returns
//     ErrPathEscape.
//
// The root itself is used as given (never symlink-resolved), matching
// the historical BoundOS behaviour.
func boundJoin(root, unsafePath string) (string, error) {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	rootVol := filepath.VolumeName(absRoot)
	rest := unsafePath
	if vol := filepath.VolumeName(rest); vol != "" {
		if !sameVolume(rootVol, vol) {
			return "", ErrPathEscape
		}
		rest = rest[len(vol):]
	}
	base := splitRoot(absRoot)
	stack := append([]string{}, base...)
	comps := splitComps(rest)
	expanded := 0
	for i := 0; i < len(comps); i++ {
		comp := comps[i]
		switch comp {
		case "", ".":
			continue
		case "..":
			if len(stack) > len(base) {
				stack = stack[:len(stack)-1]
			}
			// Else clamp: stay at root instead of escaping.
			continue
		}
		stack = append(stack, comp)
		cur := joinStack(rootVol, stack)
		fi, err := os.Lstat(cur)
		if err != nil {
			if os.IsNotExist(err) {
				// Nothing to expand below a missing path;
				// the rest stays lexical.
				continue
			}
			return "", ErrPathEscape
		}
		if fi.Mode()&os.ModeSymlink == 0 {
			continue
		}
		target, err := os.Readlink(cur)
		if err != nil {
			return "", ErrPathEscape
		}
		expanded++
		if expanded > maxLinkExpansions {
			return "", ErrPathEscape
		}
		stack = stack[:len(stack)-1]
		if vol := filepath.VolumeName(target); vol != "" {
			if !sameVolume(rootVol, vol) {
				return "", ErrPathEscape
			}
			target = target[len(vol):]
			stack = append([]string{}, base...)
		}
		// Splice the target back into the work list so nested
		// links are expanded in turn by the same walk.
		comps = append(splitComps(target), comps[i+1:]...)
		i = -1
	}
	out := joinStack(rootVol, stack)
	if !withinBase(absRoot, out) {
		return "", ErrPathEscape
	}
	return out, nil
}

// splitRoot splits an absolute path into its components.
func splitRoot(abs string) []string {
	vol := filepath.VolumeName(abs)
	rest := abs[len(vol):]
	var parts []string
	if vol != "" {
		parts = append(parts, vol)
	}
	for _, c := range strings.Split(rest, string(filepath.Separator)) {
		if c != "" {
			parts = append(parts, c)
		}
	}
	return parts
}

// splitComps splits a join operand into components. On Windows both
// separators are honoured; elsewhere only '/' splits, since backslash
// is a valid filename character.
func splitComps(p string) []string {
	if runtime.GOOS == "windows" {
		p = strings.ReplaceAll(p, "/", string(filepath.Separator))
	}
	return strings.Split(p, string(filepath.Separator))
}

// joinStack reassembles components into a path. The stack always
// derives from an absolute root, so a bare join would lose the leading
// separator (splitRoot drops empty components); restore it.
func joinStack(vol string, stack []string) string {
	if vol != "" {
		parts := stack
		if len(parts) == 0 || parts[0] != vol {
			parts = append([]string{vol}, parts...)
		}
		return filepath.Join(parts...)
	}
	return string(filepath.Separator) + filepath.Join(stack...)
}

// withinBase reports whether p is base or descends from it.
func withinBase(base, p string) bool {
	if p == base {
		return true
	}
	if runtime.GOOS == "windows" {
		if len(p) > len(base) && strings.EqualFold(p[:len(base)], base) &&
			p[len(base)] == filepath.Separator {
			return true
		}
		return false
	}
	return strings.HasPrefix(p, base+string(filepath.Separator))
}

// sameVolume compares volume names, case-insensitively on Windows.
func sameVolume(a, b string) bool {
	if runtime.GOOS == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}
