//go:build linux

package bus

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

func parentProcessID(pid int) (int, bool) {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0, false
	}
	// comm is parenthesized and may contain spaces or ')' characters. The
	// fields following its final ')' begin with state, then ppid.
	i := strings.LastIndexByte(string(b), ')')
	if i < 0 {
		return 0, false
	}
	fields := strings.Fields(string(b[i+1:]))
	if len(fields) < 2 {
		return 0, false
	}
	ppid, err := strconv.Atoi(fields[1])
	return ppid, err == nil
}
