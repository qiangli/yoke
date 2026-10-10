//go:build linux

package room

import (
	"os"
	"strconv"
	"strings"
)

// procStatFields reads /proc/<pid>/stat and returns the fields after the
// comm field — everything from state (field 3) onward. comm is a parenthesised
// process name that may itself contain spaces and parens, so the split is
// taken after its LAST closing paren, the same way ps and procps parse it.
// Field N of the stat line (N >= 3) is then element N-3.
func procStatFields(pid int) ([]string, bool) {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return nil, false
	}
	i := strings.LastIndexByte(string(b), ')')
	if i < 0 || i+2 > len(b) {
		return nil, false
	}
	return strings.Fields(string(b)[i+2:]), true
}

// processZombie reads the state the kernel keeps for the pid, including a
// zombie's — which is exactly what signal 0 cannot separate.
func processZombie(pid int) bool {
	f, ok := procStatFields(pid)
	if !ok || len(f) == 0 {
		// No stat, no verdict: keep the signal-0 answer already made.
		return false
	}
	return f[0] == "Z"
}

// pidStart is the process start time (field 22, clock ticks since boot) — a
// value set once at exec and never rewritten — as the same-process half of a
// holder's identity. Only ever compared, never interpreted.
func pidStart(pid int) (string, bool) {
	f, ok := procStatFields(pid)
	if !ok || len(f) < 20 {
		return "", false
	}
	return f[19], true
}
