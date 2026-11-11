package fleet

import "testing"

// TestSuiteAppArgs: APP is a logical entry (any host the owner serves it
// from); HOST/APP pins one host.
func TestSuiteAppArgs(t *testing.T) {
	got, err := suiteAppArgs([]string{"brainstorm", "elephant/whiteboard"})
	if err != nil {
		t.Fatal(err)
	}
	if got[0] != (suiteAppArg{suiteAnyHost, "brainstorm"}) || got[1] != (suiteAppArg{"elephant", "whiteboard"}) {
		t.Fatalf("got %+v", got)
	}
	for _, bad := range []string{"", "/app", "host/", "a/b/c"} {
		if _, err := suiteAppArgs([]string{bad}); err == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
}
