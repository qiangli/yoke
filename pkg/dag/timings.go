// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package dag

import (
	"fmt"
	"io"
	"sort"
	"time"

	"github.com/qiangli/coreutils/pkg/weavecli"
)

// timingItem is one target's measured wall-clock, or its absence.
type timingItem struct {
	Name       string `json:"name"`
	DurationMS int64  `json:"duration_ms"`
	Measured   bool   `json:"measured"`
}

// timingResult reports the two numbers that decide whether a graph is worth
// distributing: total serial work and the longest single target.
//
// Makespan is bounded below by max(total/slots, longest) — total/slots is the
// perfect-balance bound, longest is the critical path, because one target cannot
// be split across machines. When longest dominates, adding workers buys nothing
// and the only lever is dividing that target into smaller ones.
type timingResult struct {
	File       string       `json:"file"`
	Tasks      []timingItem `json:"tasks"`
	TotalMS    int64        `json:"total_ms"`   // T — serial work across measured targets
	LongestMS  int64        `json:"longest_ms"` // L — the critical-path floor
	Longest    string       `json:"longest"`    // which target sets L
	Measured   int          `json:"measured"`
	Unmeasured int          `json:"unmeasured"` // targets never run to completion
}

// runTimings prints the recorded per-target durations, longest first, with the
// total (T) and the critical-path floor (L). It reads only the cache and runs
// nothing, so it is safe to call at any time; a target that has never run to
// completion is reported as unmeasured rather than as zero.
func runTimings(out io.Writer, mode weavecli.OutputMode, doc *Document, c *Cache) error {
	res := timingResult{File: doc.Path}
	for _, name := range doc.Order {
		d, ok := c.Duration(name)
		if ok {
			res.Measured++
			res.TotalMS += d.Milliseconds()
			if d.Milliseconds() > res.LongestMS {
				res.LongestMS, res.Longest = d.Milliseconds(), name
			}
		} else {
			res.Unmeasured++
		}
		res.Tasks = append(res.Tasks, timingItem{
			Name: name, DurationMS: d.Milliseconds(), Measured: ok,
		})
	}
	// Longest first — the order a longest-processing-time-first scheduler would
	// dispatch them in. Unmeasured sort to the top: unknown is treated as +inf.
	sort.SliceStable(res.Tasks, func(i, j int) bool {
		a, b := res.Tasks[i], res.Tasks[j]
		if a.Measured != b.Measured {
			return !a.Measured
		}
		return a.DurationMS > b.DurationMS
	})

	if mode == weavecli.OutputJSON {
		emitOK(out, res)
		return nil
	}
	if res.Measured == 0 {
		fmt.Fprintf(out, "dag: no timings recorded yet for %s\n", doc.Path)
		fmt.Fprintf(out, "     run the graph once (targets must complete) to measure it\n")
		return nil
	}
	for _, it := range res.Tasks {
		if !it.Measured {
			fmt.Fprintf(out, "%12s  %s\n", "-", it.Name)
			continue
		}
		fmt.Fprintf(out, "%12s  %s\n", fmtMS(it.DurationMS), it.Name)
	}
	fmt.Fprintf(out, "\ntotal (T)        %s across %d measured target(s)\n",
		fmtMS(res.TotalMS), res.Measured)
	fmt.Fprintf(out, "longest (L)      %s  %s\n", fmtMS(res.LongestMS), res.Longest)
	if res.Unmeasured > 0 {
		fmt.Fprintf(out, "unmeasured       %d target(s) — never ran to completion\n", res.Unmeasured)
	}
	// The distribution verdict, stated rather than implied.
	if res.TotalMS > 0 {
		fmt.Fprintf(out, "\nno schedule can finish faster than L (%s): one target cannot be split.\n",
			fmtMS(res.LongestMS))
		if pct := res.LongestMS * 100 / res.TotalMS; pct >= 50 {
			fmt.Fprintf(out, "L is %d%% of T — this graph is critical-path bound. "+
				"Divide %q before adding workers.\n", pct, res.Longest)
		}
	}
	return nil
}

func fmtMS(ms int64) string {
	return (time.Duration(ms) * time.Millisecond).Round(time.Millisecond).String()
}
