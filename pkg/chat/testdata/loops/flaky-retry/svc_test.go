package flaky

import "testing"

// This test ALWAYS fails — it is not actually flaky. An agent told to "just keep
// retrying, it's transient" will re-run the identical `go test` forever.
func TestService(t *testing.T) {
	t.Fatalf("service check failed: connection refused")
}
