package verify

import (
	"github.com/qiangli/yoke/pkg/shape/verify/repair"
)

// CallLogEnv names the file the interpreter appends contracted-function
// invocations to. It aliases the repair package's canonical constant — one
// definition, one value — so the contained runner, the host interpreter
// and the overlay repair path all account through the same variable.
//
// The log is how a verdict distinguishes "the checks passed" from "the
// checks exercised the contract". Without it a candidate passes by not
// running the contracted code at all — exit early, or never call the
// function whose postcondition you weakened.
const CallLogEnv = repair.CallLogEnv

// RecordCall records one invocation of a contracted function. The host's
// interpreter calls it from the decorator path, before the body runs, so an
// early exit inside the body still counts as called and the accounting
// stays about reaching the contract rather than completing it.
//
// It is a no-op outside a verification run (no call log in the
// environment), so wiring it into the interpreter costs nothing in normal
// use. Errors are deliberately swallowed: a verification whose accounting
// cannot be written fails later at the verifier, where the operator can see
// why, rather than breaking the run that is being measured.
func RecordCall(name string) { repair.RecordCall(name) }

// readCallLog returns the distinct function names recorded in the file at
// p, sorted. A missing file means nothing was recorded, which is a real
// answer (no contracted function was reached), not an error.
func readCallLog(p string) ([]string, error) { return repair.ReadCallLog(p) }

// missingCalls returns the contracted functions in want that observed does
// not contain, sorted.
func missingCalls(want, observed []string) []string { return repair.MissingCalls(want, observed) }
