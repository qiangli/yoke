package bus

import (
	"os"
	"path/filepath"
	"strings"
)

// ProcessHasAncestor reports whether anchor is this process or one of its
// parents. The bound and cycle check fail closed on corrupt process metadata.
func ProcessHasAncestor(anchor int) bool {
	return processHasAncestor(anchor)
}

func processHasAncestor(anchor int) bool {
	if anchor <= 0 {
		return false
	}
	pid := os.Getpid()
	seen := make(map[int]bool)
	for depth := 0; pid > 0 && depth < 128; depth++ {
		if pid == anchor {
			return true
		}
		if seen[pid] {
			return false
		}
		seen[pid] = true
		parent, ok := parentProcessID(pid)
		if !ok || parent == pid {
			return false
		}
		pid = parent
	}
	return false
}

// ancestorNamed returns the nearest ancestor of this process whose executable
// is one of names, which is how a command run by an agent harness finds the
// harness itself: the harness is not this command's parent (a shell sits in
// between) but it is always above it. Names compare without directory, case or
// a Windows .exe suffix. The walk is bounded like processHasAncestor.
func ancestorNamed(names ...string) (int, bool) {
	want := make(map[string]bool, len(names))
	for _, n := range names {
		if n = processBaseName(n); n != "" {
			want[n] = true
		}
	}
	if len(want) == 0 {
		return 0, false
	}
	pid := os.Getpid()
	seen := make(map[int]bool)
	for depth := 0; pid > 1 && depth < 128; depth++ {
		if seen[pid] {
			return 0, false
		}
		seen[pid] = true
		parent, ok := parentProcessID(pid)
		if !ok || parent == pid || parent <= 1 {
			return 0, false
		}
		if name, ok := processName(parent); ok && want[processBaseName(name)] {
			return parent, true
		}
		pid = parent
	}
	return 0, false
}

func processBaseName(name string) string {
	name = strings.TrimSpace(name)
	if i := strings.LastIndexAny(name, `/\`); i >= 0 {
		name = name[i+1:]
	}
	if name == "" {
		return ""
	}
	name = strings.ToLower(filepath.Base(name))
	return strings.TrimSuffix(name, ".exe")
}
