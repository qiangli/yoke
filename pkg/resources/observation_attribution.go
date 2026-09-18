package resources

import (
	"strconv"
	"strings"
	"time"
)

// AttributeProcesses joins a complete host observation to workload roots. Each
// process belongs to at most one nearest verified root. PID alone is never proof.
func AttributeProcesses(processes []ProcessObservation, refs []WorkloadRef) ([]ProcessObservation, []WorkloadObservation) {
	return attributeProcessesInPlace(append([]ProcessObservation(nil), processes...), refs)
}

// The host projection owns this fresh slice. Public callers keep copy semantics;
// projection avoids allocating another full host process observation array.
func attributeProcessesInPlace(out []ProcessObservation, refs []WorkloadRef) ([]ProcessObservation, []WorkloadObservation) {
	if len(refs) == 0 {
		for i := range out {
			out[i].WorkloadID = ""
			out[i].Attribution = "unattributed"
			out[i].Reason = "no verified workload ancestor"
		}
		return out, []WorkloadObservation{}
	}
	byPID := make(map[int]int, len(out))
	roots := map[int][]int{}
	for i, p := range out {
		byPID[p.Identity.PID] = i
	}
	workloads := make([]WorkloadObservation, len(refs))
	for i, ref := range refs {
		roots[ref.Root.PID] = append(roots[ref.Root.PID], i)
		workloads[i] = WorkloadObservation{WorkloadRef: ref, Coverage: ObservationCoverage{Complete: true}, CPU: unknownValue[float64]("process-tree", "no verified processes", time.Time{}), RSS: unknownValue[uint64]("process-tree", "no verified processes", time.Time{})}
	}
	cpu := make([]float64, len(refs))
	rss := make([]uint64, len(refs))
	cpuKnown := make([]int, len(refs))
	rssKnown := make([]int, len(refs))
	for i := range out {
		p := &out[i]
		p.WorkloadID = ""
		p.Attribution = "unattributed"
		p.Reason = "no verified workload ancestor"
		seen := map[int]bool{}
		cur := i
		assigned := -1
		for depth := 0; depth < 64; depth++ {
			ancestor := out[cur]
			pid := ancestor.Identity.PID
			if seen[pid] {
				p.Reason = "process ancestry cycle"
				break
			}
			seen[pid] = true
			matches := []int{}
			for _, w := range roots[pid] {
				if refs[w].Root.StartID != "" && refs[w].Root.StartID == ancestor.Identity.StartID {
					matches = append(matches, w)
				} else {
					p.Attribution = "unverified"
					p.Reason = "workload root birth identity missing or changed"
				}
			}
			if len(matches) > 1 {
				p.Attribution = "ambiguous"
				p.Reason = "competing workload roots"
				break
			}
			if len(matches) == 1 {
				assigned = matches[0]
				break
			}
			parent, ok := byPID[ancestor.PPID]
			if !ok {
				break
			}
			if childStart, childRealm, childKnown := observationBirthOrder(ancestor.Identity.StartID); childKnown {
				if parentStart, parentRealm, parentKnown := observationBirthOrder(out[parent].Identity.StartID); parentKnown && (parentRealm != childRealm || parentStart > childStart) {
					p.Attribution = "unverified"
					p.Reason = "parent PID birth identity changed"
					break
				}
			}
			cur = parent
			if depth == 63 {
				p.Reason = "ancestry depth limit"
			}
		}
		if assigned < 0 {
			continue
		}
		w := &workloads[assigned]
		p.WorkloadID = w.ID
		p.Attribution = "verified"
		p.Reason = ""
		w.ProcessCount++
		w.Coverage.Seen++
		w.Coverage.Included++
		if p.CPU.Value != nil && !p.CPU.Status.Stale {
			cpu[assigned] += *p.CPU.Value
			cpuKnown[assigned]++
			w.CPU.Status = mergeObservationAggregateStatus(w.CPU.Status, p.CPU.Status, cpuKnown[assigned])
		}
		if p.RSS.Value != nil && !p.RSS.Status.Stale {
			rss[assigned] += *p.RSS.Value
			rssKnown[assigned]++
			w.RSS.Status = mergeObservationAggregateStatus(w.RSS.Status, p.RSS.Status, rssKnown[assigned])
		}
	}
	for i := range workloads {
		w := &workloads[i]
		if w.ProcessCount == 0 {
			w.Coverage.Complete = false
			w.Coverage.Reason = "root absent or unverified"
			continue
		}
		if cpuKnown[i] > 0 {
			v := cpu[i]
			w.CPU.Value = &v
			w.CPU.Status.Source = "process-tree"
		}
		if rssKnown[i] > 0 {
			v := rss[i]
			w.RSS.Value = &v
			w.RSS.Status.Kind = "estimated"
			w.RSS.Status.Source = "process-tree"
			w.RSS.Status.Reason = "sum of resident pages may include shared pages"
		}
		if cpuKnown[i] != w.ProcessCount || rssKnown[i] != w.ProcessCount {
			w.Coverage.Complete = false
			w.Coverage.Reason = "one or more process metrics unavailable"
			if cpuKnown[i] > 0 && cpuKnown[i] != w.ProcessCount {
				w.CPU.Status.Kind = "estimated"
				w.CPU.Status.Reason = "partial process CPU sum"
			}
		}
	}
	return out, workloads
}

func mergeObservationAggregateStatus(aggregate, contributor ObservationStatus, count int) ObservationStatus {
	if count == 1 {
		return contributor
	}
	// A sum is only as fresh as its oldest contributor, and its rate window is
	// the union of contributing windows; never take metadata from an unknown row.
	if contributor.At.Before(aggregate.At) {
		aggregate.At = contributor.At
	}
	if contributor.ExpiresAt.Before(aggregate.ExpiresAt) {
		aggregate.ExpiresAt = contributor.ExpiresAt
	}
	if contributor.WindowStart.Before(aggregate.WindowStart) {
		aggregate.WindowStart = contributor.WindowStart
	}
	if contributor.WindowEnd.After(aggregate.WindowEnd) {
		aggregate.WindowEnd = contributor.WindowEnd
	}
	aggregate.Stale = aggregate.Stale || contributor.Stale
	return aggregate
}

func observationBirthOrder(identity string) (uint64, string, bool) {
	fields := strings.Split(identity, ":")
	if len(fields) == 3 && fields[0] == "darwin" {
		sec, e1 := strconv.ParseUint(fields[1], 10, 64)
		micros, e2 := strconv.ParseUint(fields[2], 10, 64)
		if e1 == nil && e2 == nil && micros < 1000000 && sec < (^uint64(0)-micros)/1000000 {
			return sec*1000000 + micros, "darwin", true
		}
	}
	if len(fields) == 3 && fields[0] == "linux" {
		ticks, err := strconv.ParseUint(fields[2], 10, 64)
		return ticks, "linux:" + fields[1], err == nil
	}
	if len(fields) == 2 && fields[0] == "windows" {
		ticks, err := strconv.ParseUint(fields[1], 10, 64)
		return ticks, "windows", err == nil
	}
	return 0, "", false
}
