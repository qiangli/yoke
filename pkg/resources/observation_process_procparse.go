package resources

import (
	"bytes"
	"fmt"
	"strconv"
	"strings"
)

func parseObservationProcStat(pid int, b []byte, boot string, hz float64, pageSize uint64) (processSample, error) {
	first, last := bytes.IndexByte(b, '('), bytes.LastIndexByte(b, ')')
	if first < 0 || last < first {
		return processSample{}, fmt.Errorf("invalid proc stat")
	}
	fields := strings.Fields(string(b[last+1:]))
	if len(fields) < 22 {
		return processSample{}, fmt.Errorf("short proc stat")
	}
	ppid, err := strconv.Atoi(fields[1])
	if err != nil {
		return processSample{}, err
	}
	start, err := strconv.ParseUint(fields[19], 10, 64)
	if err != nil {
		return processSample{}, err
	}
	p := processSample{Identity: ProcessIdentity{PID: pid}, PPID: ppid, Name: string(b[first+1 : last]), Source: "procfs:stat", Reason: "CPU clock or memory unavailable"}
	if boot != "" {
		p.Identity.StartID = fmt.Sprintf("linux:%s:%d", boot, start)
	}
	user, e1 := strconv.ParseUint(fields[11], 10, 64)
	system, e2 := strconv.ParseUint(fields[12], 10, 64)
	if e1 == nil && e2 == nil && hz > 0 {
		v := (float64(user) + float64(system)) / hz
		p.CPUSeconds = &v
	}
	resident, err := strconv.ParseUint(fields[21], 10, 64)
	if err == nil && resident <= ^uint64(0)/pageSize {
		v := resident * pageSize
		p.RSS = &v
	}
	return p, nil
}
