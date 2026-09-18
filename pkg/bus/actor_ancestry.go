package bus

import "os"

// processHasAncestor reports whether anchor is this process or one of its
// parents. The bound and cycle check fail closed on corrupt process metadata.
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
