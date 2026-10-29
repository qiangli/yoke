// Package repair verifies a repaired Bash# body against the contract
// frozen from the original unit body: the check that must pass before the
// repair is promoted to a host-local overlay.
//
// It lives one level below pkg/shape/verify — which keeps the tree-level
// verifier, the evidence writer and the ContractReader — because the skills
// overlay path needs the verdict and package verify's containment chain
// cannot be imported from pkg/skills. The call log here IS the accounting
// verify runs on: CallLogEnv names the same file, with the same value, as
// verify.CallLogEnv, which aliases it.
package repair

import (
	"bufio"
	"os"
	"sort"
	"strings"
	"sync"
)

// CallLogEnv names the file the interpreter appends contracted-function
// invocations to. The verifier sets it for each check run; the
// interpreter's decorator path calls RecordCall, which appends one line per
// invocation.
//
// The log is how a verdict distinguishes "the checks passed" from "the
// checks exercised the contract". Without it a candidate passes by not
// running the contracted code at all — exit early, or never call the
// function whose postcondition you weakened.
const CallLogEnv = "BASHY_SHAPE_CALLLOG"

// recordMu serialises appends from one process; the open/append/close
// pattern keeps concurrent writers from different processes interleaving
// within a line.
var recordMu sync.Mutex

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
func RecordCall(name string) {
	p := strings.TrimSpace(os.Getenv(CallLogEnv))
	if p == "" || name == "" {
		return
	}
	recordMu.Lock()
	defer recordMu.Unlock()
	f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	_, _ = f.WriteString(strings.TrimSpace(name) + "\n")
	_ = f.Close()
}

// ReadCallLog returns the distinct function names recorded in the file at
// p, sorted. A missing file means nothing was recorded, which is a real
// answer (no contracted function was reached), not an error.
func ReadCallLog(p string) ([]string, error) {
	f, err := os.Open(p)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	seen := map[string]bool{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if name := strings.TrimSpace(sc.Text()); name != "" {
			seen[name] = true
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(seen))
	for n := range seen {
		out = append(out, n)
	}
	sort.Strings(out)
	return out, nil
}

// MissingCalls returns the contracted functions in want that observed does
// not contain, sorted.
func MissingCalls(want, observed []string) []string {
	seen := map[string]bool{}
	for _, c := range observed {
		seen[c] = true
	}
	var out []string
	for _, w := range want {
		if !seen[w] {
			out = append(out, w)
		}
	}
	sort.Strings(out)
	return out
}
