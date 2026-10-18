package foreman

import (
	"os"
	"strconv"
	"strings"
)

// ProcessStart is pid's start time in clock ticks since boot (/proc stat field
// 22), which a recycled pid does not share.
func ProcessStart(pid int) (string, bool) {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return "", false
	}
	// comm (field 2) may contain spaces and parens; fields resume after the last ')'.
	i := strings.LastIndexByte(string(data), ')')
	if i < 0 {
		return "", false
	}
	f := strings.Fields(string(data[i+1:]))
	if len(f) < 20 {
		return "", false
	}
	return f[19], true
}
