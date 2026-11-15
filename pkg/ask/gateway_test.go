package ask

import "testing"

func TestGatewayDigestBindsArgvHostAndTask(t *testing.T) {
	base := GatewayDigest([]string{"shape", "wish", "hello"}, "hostA", "task-1")
	if len(base) != 64 {
		t.Fatalf("digest %q is not 64 hex chars", base)
	}
	if got := GatewayDigest([]string{"shape", "wish", "hello"}, "hostA", "task-1"); got != base {
		t.Fatal("digest must be stable")
	}
	// No two distinct candidates share a digest.
	cases := map[string]struct {
		argv []string
		host string
		task string
	}{
		"argv boundary": {[]string{"shape", "wishhello"}, "hostA", "task-1"},
		"argv element":  {[]string{"shape", "wish", "hello!"}, "hostA", "task-1"},
		"other host":    {[]string{"shape", "wish", "hello"}, "hostB", "task-1"},
		"other task":    {[]string{"shape", "wish", "hello"}, "hostA", "task-2"},
		"empty argv":    {nil, "hostA", "task-1"},
	}
	for name, tc := range cases {
		if got := GatewayDigest(tc.argv, tc.host, tc.task); got == base {
			t.Fatalf("%s: digest did not change", name)
		}
	}
	// A NUL inside an element must not alias an element boundary.
	if joined, split := GatewayDigest([]string{"a\x00b"}, "h", "t"), GatewayDigest([]string{"a", "b"}, "h", "t"); joined == split {
		t.Fatal("embedded NUL aliases an element boundary")
	}
}
