package capability

import "github.com/qiangli/yoke/pkg/ladder"

// LadderLines fits the duty thresholds from replayed ladder evidence.
func LadderLines(rep ladder.ReplayResult, season int) ladder.Lines {
	return dutyComputeLines(rep, season)
}
