package repair

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/qiangli/yoke/pkg/unit"
)

// BashSharpRunner executes one Bash# body in dir with env. It is the skills
// runner's shape, restated here so the repair verdict does not import the
// skills package.
type BashSharpRunner func(ctx context.Context, dir string, env []string, body string, stdout, stderr io.Writer) (int, error)

// CheckEval evaluates one frozen contract clause in dir: a nil error means
// the clause holds. The caller supplies the evaluator so a host without a
// shell (or one whose checks must run in-process) still gets a verdict.
type CheckEval func(ctx context.Context, dir, expr string) error

// VerifyRepair runs a repaired Bash# candidate in dir and checks it against
// the contract frozen from the original body. The candidate is promoted only
// on a nil error; anything else — a changed contract, a failing frozen
// check, an uncalled contracted function — refuses it.
//
// Frozen inputs. The require/ensure clauses and the capability key come from
// the ORIGINAL body. A repair that weakens a check fails the key comparison
// before anything runs; a repair that exits early fails the frozen
// postconditions after it runs.
//
// Accounted execution. The candidate runs with CallLogEnv set, and every
// contracted function the original or the candidate declares must be
// recorded by the run: a repair that passes its checks by never calling the
// contracted code is refused.
//
// The returned string is the candidate run's combined output, for the repair
// loop's next prompt as well as for the failure at hand.
func VerifyRepair(ctx context.Context, original, candidate, dir string, env []string, run BashSharpRunner, stdout, stderr io.Writer, eval CheckEval) (string, error) {
	orig, err := unit.ReadContract(original)
	if err != nil {
		return "", fmt.Errorf("verify: the original unit does not parse: %w", err)
	}
	if !orig.Contracted() {
		return "", fmt.Errorf("verify: the original unit states no contract, so no repair of it can be verified")
	}
	if _, err := unit.ReadContract(candidate); err != nil {
		return "", fmt.Errorf("verify: the repair does not parse as Bash#: %w", err)
	}
	want, err := unit.CapabilityKey(orig)
	if err != nil {
		return "", fmt.Errorf("verify: the original unit has no capability key: %w", err)
	}
	cand, _ := unit.ReadContract(candidate)
	got, cerr := unit.CapabilityKey(cand)
	if cerr != nil || got != want {
		return "", fmt.Errorf("verify: the repair keeps a different contract than the original — the how may change, never the what")
	}
	if run == nil {
		return "", fmt.Errorf("verify: no runner for the repair")
	}
	if eval == nil {
		return "", fmt.Errorf("verify: no evaluator for the frozen checks")
	}
	// The accounting set is original AND candidate contracted functions: a
	// repair that stops exercising the contract it inherited is unverified,
	// and contracted code a repair adds without reaching it is too.
	names := union(functionNames(original), functionNames(candidate))
	if len(names) == 0 {
		return "", fmt.Errorf("verify: no contracted function to account for")
	}
	for _, req := range orig.Require {
		if err := eval(ctx, dir, req); err != nil {
			return "", fmt.Errorf("verify: frozen precondition %q failed: %v", req, err)
		}
	}
	log, err := os.CreateTemp("", "shape-calls-*")
	if err != nil {
		return "", err
	}
	callLog := log.Name()
	log.Close()
	defer os.Remove(callLog)
	runEnv := make([]string, 0, len(env)+1)
	runEnv = append(runEnv, env...)
	runEnv = append(runEnv, CallLogEnv+"="+callLog)
	var buf bytes.Buffer
	out, errOut := stdout, stderr
	if out == nil {
		out = io.Discard
	}
	if errOut == nil {
		errOut = io.Discard
	}
	code, rerr := run(ctx, dir, runEnv, candidate, io.MultiWriter(out, &buf), io.MultiWriter(errOut, &buf))
	output := buf.String()
	if rerr != nil {
		if code == 0 {
			code = 1
		}
		output = strings.TrimSpace(output + "\n" + rerr.Error())
	}
	if code != 0 {
		return output, fmt.Errorf("verify: the repair exited %d", code)
	}
	observed, err := ReadCallLog(callLog)
	if err != nil {
		return output, err
	}
	if missing := MissingCalls(names, observed); len(missing) > 0 {
		return output, fmt.Errorf("verify: the repair never called %s — passing by not running the contract proves nothing", strings.Join(missing, ", "))
	}
	for _, ens := range orig.Ensure {
		if err := eval(ctx, dir, ens); err != nil {
			return output, fmt.Errorf("verify: frozen postcondition %q failed: %v", ens, err)
		}
	}
	return output, nil
}

// functionNames returns the contracted function names one Bash# body
// declares, through the unit grammar. A body the grammar cannot read names
// no functions; the capability comparison above already refused the repair
// by then, so this stays a name list, never a second verdict.
func functionNames(body string) []string {
	funcs, _, ok := BodyFunctions(body, "")
	if !ok {
		return nil
	}
	var out []string
	for _, f := range funcs {
		out = append(out, f.Name)
	}
	return out
}

// union merges two name lists, deduplicated and sorted.
func union(a, b []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, v := range append(append([]string{}, a...), b...) {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	sort.Strings(out)
	return out
}
