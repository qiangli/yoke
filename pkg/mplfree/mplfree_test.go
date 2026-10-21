package mplfree

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hashicorp/go-multierror"
)

// enginePkg is the engine entry point whose import graph the sprint
// gate polices.
const enginePkg = "github.com/qiangli/yoke/external/podman/engine"

// engineDeps lists the engine package's dependencies with the effective
// source directory of each package's module.
func engineDeps(t *testing.T) map[string]string {
	t.Helper()
	cmd := exec.Command("go", "list", "-deps", "-f", "{{.ImportPath}}\t{{if .Module}}{{.Module.Dir}}{{end}}", enginePkg)
	cmd.Env = append(os.Environ(), "GOWORK=off")
	cmd.Dir = "../.."
	out, err := cmd.Output()
	if err != nil {
		t.Skipf("go list unavailable: %v", err)
	}
	deps := make(map[string]string)
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.SplitN(line, "\t", 2)
		if len(fields) == 2 {
			deps[fields[0]] = fields[1]
		}
	}
	return deps
}

// TestEngineMultierrorChainIsPermissive pins the S488.3c contract: the
// hashicorp error-aggregation chain in the engine import graph must
// resolve to yoke's clean-room external/ replacements, never to the
// MPL-licensed upstream module cache. A module that dropped out of the
// graph entirely also satisfies the contract.
func TestEngineMultierrorChainIsPermissive(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	deps := engineDeps(t)
	for importPath, wantDir := range map[string]string{
		"github.com/hashicorp/go-multierror": filepath.Join(root, "external", "multierror"),
		"github.com/hashicorp/errwrap":       filepath.Join(root, "external", "errwrap"),
	} {
		dir, inGraph := deps[importPath]
		if !inGraph {
			t.Logf("%s dropped out of the engine graph: contract satisfied", importPath)
			continue
		}
		if dir != wantDir {
			t.Errorf("%s resolves to %s, want the clean-room replacement at %s", importPath, dir, wantDir)
		}
	}
}

// TestErrorAggregationContract pins the aggregation behavior every
// engine caller of multierror relies on: podman pkg/errorhandling and
// pkg/machine/shim (typed-nil start, ErrorOrNil), containers/common
// retry (type switch plus Errors iteration), libimage (assigning the
// aggregate to an error interface) and pkg/supplemented (repeated
// Append into one aggregate).
func TestErrorAggregationContract(t *testing.T) {
	one, two, three := errors.New("one"), errors.New("two"), errors.New("three")

	// The errorhandling/shim idiom: start from a typed-nil *Error.
	var agg *multierror.Error
	if got := multierror.Append(agg); got != nil {
		t.Errorf("Append of nothing = %v, want nil", got)
	}
	if got := multierror.Append(agg, nil, nil); got != nil {
		t.Errorf("Append of nil errors = %v, want nil", got)
	}

	// The libimage idiom: every Append is guarded by a non-nil check
	// and the error interface stays untouched (nil) when nothing
	// failed. A typed-nil *Error assigned to an interface would wrap
	// non-nil — inherent Go semantics the callers avoid by guarding —
	// so the contract only requires the populated aggregate to flow
	// through the interface whole.
	var iface error
	if one != nil { // mirrors "if d.err != nil"
		iface = multierror.Append(iface, one, two)
	}
	if iface == nil || !errors.Is(iface, two) {
		t.Errorf("populated aggregate as error = %v, want one carrying both errors", iface)
	}

	// Aggregation keeps order and exposes the Errors field.
	agg = multierror.Append(agg, one)
	agg = multierror.Append(agg, two, nil, three)
	if len(agg.Errors) != 3 {
		t.Fatalf("len(Errors) = %d, want 3", len(agg.Errors))
	}
	for i, want := range []error{one, two, three} {
		if agg.Errors[i] != want {
			t.Errorf("Errors[%d] = %v, want %v", i, agg.Errors[i], want)
		}
	}

	// ErrorOrNil distinguishes empty from non-empty aggregates.
	var empty *multierror.Error
	if got := empty.ErrorOrNil(); got != nil {
		t.Errorf("ErrorOrNil on typed-nil = %v, want nil", got)
	}
	if got := agg.ErrorOrNil(); got == nil || got != error(agg) {
		t.Errorf("ErrorOrNil = %v, want the aggregate itself", got)
	}

	// The containers/common retry pattern: a type switch on
	// *multierror.Error iterating e.Errors must see every element.
	seen := 0
	var asErr error = agg
	switch e := asErr.(type) {
	case *multierror.Error:
		for range e.Errors {
			seen++
		}
	default:
		t.Fatalf("type switch missed *multierror.Error (got %T)", asErr)
	}
	if seen != 3 {
		t.Errorf("retry-style iteration saw %d errors, want 3", seen)
	}

	// Error renders the header-plus-list shape the podman
	// errorhandling package documents and TrimSpaces.
	msg := agg.Error()
	if !strings.HasPrefix(msg, "3 errors occurred:") {
		t.Errorf("Error() header = %q, want %q prefix", msg, "3 errors occurred:")
	}
	for _, want := range []string{"one", "two", "three"} {
		if !strings.Contains(msg, "\t* "+want) {
			t.Errorf("Error() missing bullet %q in:\n%s", want, msg)
		}
	}
	if trimmed := strings.TrimSpace(msg); trimmed != strings.TrimRight(msg, "\n") || !strings.HasSuffix(msg, "\n\n") {
		t.Errorf("Error() should end with a blank line for TrimSpace callers: %q", msg)
	}

	// Appending an existing aggregate flattens it instead of nesting.
	nested := multierror.Append(nil, one, two)
	flat := multierror.Append(nested, three)
	if len(flat.Errors) != 3 || flat.Errors[0] != one || flat.Errors[2] != three {
		t.Errorf("flattened Errors = %v, want [one two three]", flat.Errors)
	}

	// A non-aggregate first argument seeds the list.
	seeded := multierror.Append(errEOF, one)
	if len(seeded.Errors) != 2 || seeded.Errors[0] != errEOF {
		t.Errorf("seeded Errors = %v, want [errEOF one]", seeded.Errors)
	}

	// errors.Is traverses the aggregate through Unwrap.
	if !errors.Is(agg, two) {
		t.Error("errors.Is(aggregate, wrapped error) = false, want true")
	}
	if errors.Is(agg, errors.New("missing")) {
		t.Error("errors.Is matched an error that was never appended")
	}
}

var errEOF = errors.New("simulated EOF")
