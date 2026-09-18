package autoretry

import (
	"testing"
	"time"
)

func TestClassify(t *testing.T) {
	transient := []string{
		"curl: (7) Failed to connect to example.com port 443: Connection refused",
		"fatal: unable to access 'https://x/': Could not resolve host: x",
		"read tcp: i/o timeout",
		"HTTP/1.1 503 Service Unavailable",
		"ssh: connect to host x port 22: Operation timed out",
		"error: RPC failed; The remote end hung up unexpectedly",
	}
	for _, s := range transient {
		if _, ok := Classify(s); !ok {
			t.Errorf("expected transient: %q", s)
		}
	}
	terminal := []string{
		"fatal: Authentication failed for 'https://x/'",
		"curl: (22) The requested URL returned error: 404",
		"No such file or directory",
		"permission denied",
		"",
	}
	for _, s := range terminal {
		if _, ok := Classify(s); ok {
			t.Errorf("expected NON-transient: %q", s)
		}
	}
}

func TestEligible(t *testing.T) {
	for _, ok := range []string{"curl", "git", "wget", "dig", "ssh"} {
		if !Eligible(ok) {
			t.Errorf("%s should be eligible", ok)
		}
	}
	for _, no := range []string{"pip", "npm", "apt-get", "rm", "grep", "cp"} {
		if Eligible(no) {
			t.Errorf("%s must NOT be eligible (non-idempotent or not transient-prone)", no)
		}
	}
}

func TestBackoffCapped(t *testing.T) {
	if Backoff(1) != 300*time.Millisecond {
		t.Errorf("backoff(1)=%v want 300ms", Backoff(1))
	}
	if Backoff(2) != 900*time.Millisecond {
		t.Errorf("backoff(2)=%v want 900ms", Backoff(2))
	}
	if Backoff(10) != 3*time.Second {
		t.Errorf("backoff(10)=%v want cap 3s", Backoff(10))
	}
}

func TestNoteMentionsAttemptsAndClass(t *testing.T) {
	n := Note("timeout", 3, false)
	if n == "" {
		t.Fatal("empty note")
	}
	// 3 attempts = 2 retries; the note must name the retries and the class and
	// tell the agent not to redo it.
	for _, want := range []string{"2x", "timeout", "do not re-run"} {
		if !contains(n, want) {
			t.Errorf("note %q missing %q", n, want)
		}
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}
