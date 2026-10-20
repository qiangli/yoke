package conformance_test

import (
	"testing"

	"github.com/qiangli/yoke/pkg/provider/conformance"
	"github.com/qiangli/yoke/pkg/provider/scaffold"
)

// First red test (S472.14): A scaffold generated in t.TempDir() and completed
// with the fixture implementation must pass validate and the full conformance
// suite against its fake API; any failing case fails the test.
func TestScaffoldToConformanceNoSourceChange(t *testing.T) {
	dir := t.TempDir()
	err := scaffold.Init(scaffold.Options{
		Dir:  dir,
		Form: "declarative",
		Name: "scaffold-fixture",
	})
	if err != nil {
		t.Fatalf("scaffold.Init failed: %v", err)
	}

	if err := scaffold.Validate(dir); err != nil {
		t.Fatalf("scaffold.Validate failed: %v", err)
	}

	// Now run the full conformance suite
	conformance.Run(t, nil, nil, conformance.WithLive(true))
}
