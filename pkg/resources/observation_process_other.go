//go:build !linux && !darwin && !windows

package resources

import (
	"context"
	"fmt"
)

func collectProcessSamples(context.Context) ([]processSample, ObservationCoverage, error) {
	return nil, ObservationCoverage{Reason: "native process source unsupported"}, fmt.Errorf("native process observations unavailable on this OS")
}

func lookupNativeProcessIdentity(int) (ProcessIdentity, error) {
	return ProcessIdentity{}, fmt.Errorf("native process birth identity unavailable on this OS")
}
