package multierror

import (
	"errors"
	"testing"
)

// TestGroupContract pins the buildah imagebuildah idiom: a zero-value
// Group fans out with Go, Wait returns nil when every task succeeded,
// otherwise an aggregate whose Len and Errors expose each failure.
func TestGroupContract(t *testing.T) {
	var ok Group
	for i := 0; i < 8; i++ {
		ok.Go(func() error { return nil })
	}
	if merr := ok.Wait(); merr != nil {
		t.Fatalf("Wait with no failures = %v, want nil", merr)
	}

	boom := errors.New("boom")
	var g Group
	for i := 0; i < 16; i++ {
		fail := i%4 == 0
		g.Go(func() error {
			if fail {
				return boom
			}
			return nil
		})
	}
	merr := g.Wait()
	if merr == nil || merr.Len() != 4 || len(merr.Errors) != 4 {
		t.Fatalf("Wait = %v, want 4 aggregated errors", merr)
	}
	if !errors.Is(merr.ErrorOrNil(), boom) {
		t.Errorf("aggregate does not carry the task error")
	}
	var nilErr *Error
	if nilErr.Len() != 0 {
		t.Error("Len on typed-nil *Error should be 0")
	}
}
