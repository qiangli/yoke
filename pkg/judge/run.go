// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package judge

import (
	"fmt"
)

// The weave seam.
//
// judge deliberately does NOT import weave. The dependency has to point this way, or it
// cannot point at all: the whole reason judge exists is so the CONDUCTOR can review a
// run's work before merging it — which means weave (and the autopilot inside it) must be
// free to call judge. Importing weave from here would make that a cycle, and the feature
// would have to be built somewhere worse.
//
// So judge declares what it needs and the front door supplies it. Unset, `judge --run N`
// says so plainly instead of failing with a nil dereference.

// RunReader returns a run's work as reviewable text: what to call it, the diff to read,
// and which SDLC stage the run declared (so the right rubric is chosen without the
// caller having to remember).
var RunReader func(id int64) (subject, content, stage string, err error)

// RunRecorder writes a verdict back onto the run.
//
// The fields it fills — ReviewVerdict, ReviewBlocking, ReviewNotes, ReviewBy, ReviewAt —
// have existed on a weave item since long before this package, and until now were
// written ONLY by a mechanical clean-room re-run of the verify command. A field called
// "ReviewVerdict" that could never hold a review is exactly the gap D3 closes.
var RunRecorder func(id int64, r Report) error

func gatherRun(id int64) (subject, content, stage string, err error) {
	if RunReader == nil {
		return "", "", "", fmt.Errorf("--run is unavailable in this build (no weave)")
	}
	return RunReader(id)
}

func recordOnRun(id int64, r Report) {
	if RunRecorder == nil {
		return
	}
	// A failure to record must not discard the verdict the user is about to read — the
	// report is printed either way; only the persistence is best-effort.
	_ = RunRecorder(id, r)
}
