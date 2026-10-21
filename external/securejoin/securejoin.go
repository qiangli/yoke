// Package securejoin joins an unsafe path to a root directory while
// keeping the result confined inside the root.
//
// This is a yoke clean-room implementation written for S488.3d
// (remove the MPL-2.0 cyphar/filepath-securejoin dependency from the
// engine build). It was written from POSIX path semantics, the call
// sites and expectations of its permissive callers (podman
// pkg/util, pkg/lookup, containers/common etchosts, storage,
// buildah parse, cgroups — Apache-2.0, all using only SecureJoin and
// checking only err != nil), the one-line public signature from
// `go doc`, and black-box behavior observed by executing the
// replaced module (lexical escapes clamp inside, outside symlinks
// re-scope under the root, loops are an error). The replaced
// module's source — including vendor copies — was never opened.
// Only the caller-used API is provided: SecureJoin.
package securejoin

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// errLoop is returned when symlink expansion does not converge.
var errLoop = errors.New("securejoin: too many symlink expansions")

// maxExpansions caps symlink expansion while joining, so a link loop
// is an error instead of a hang.
const maxExpansions = 255

// SecureJoin joins unsafePath to root, keeping the result inside
// root:
//
//   - Absolute unsafe paths are treated as relative to root, and
//     ".." components that would ascend above root are clamped: they
//     stay at root instead of escaping.
//   - Symlinks met while walking are expanded (absolute targets
//     restart the walk at root, relative targets splice in place,
//     and nested links are expanded in turn by the same walk);
//     targets that resolve outside root are re-scoped under root, so
//     they never yield a usable outside path.
//   - A symlink loop is an error.
//
// The root itself is used as given (never symlink-resolved). A
// missing path component ends expansion for that branch and the
// remainder stays lexical, still confined under root.
func SecureJoin(root, unsafePath string) (string, error) {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	absRoot = filepath.Clean(absRoot)
	rootVol := filepath.VolumeName(absRoot)
	rest := unsafePath
	if vol := filepath.VolumeName(rest); vol != "" {
		if !sameVolume(rootVol, vol) {
			return "", errLoop
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
			return "", err
		}
		if fi.Mode()&os.ModeSymlink == 0 {
			continue
		}
		target, err := os.Readlink(cur)
		if err != nil {
			return "", err
		}
		expanded++
		if expanded > maxExpansions {
			return "", errLoop
		}
		stack = stack[:len(stack)-1]
		if vol := filepath.VolumeName(target); vol != "" {
			if !sameVolume(rootVol, vol) {
				return "", errLoop
			}
			target = target[len(vol):]
			stack = append([]string{}, base...)
		} else if filepath.IsAbs(target) {
			stack = append([]string{}, base...)
		}
		// Splice the target back into the work list so nested
		// links are expanded in turn by the same walk.
		comps = append(splitComps(target), comps[i+1:]...)
		i = -1
	}
	out := joinStack(rootVol, stack)
	if !withinBase(absRoot, out) {
		return "", errLoop
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
// separators are honoured; elsewhere only '/' splits, since
// backslash is a valid filename character.
func splitComps(p string) []string {
	if runtime.GOOS == "windows" {
		p = strings.ReplaceAll(p, "/", string(filepath.Separator))
	}
	return strings.Split(p, string(filepath.Separator))
}

// joinStack reassembles components into a path. The stack always
// derives from an absolute root, so a bare join would lose the
// leading separator (splitRoot drops empty components); restore it.
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
