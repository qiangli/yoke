package craft

import "testing"

// THE CASE THIS EXISTS FOR. `ssh host make test` returning 1 means the BUILD
// failed — ssh connected fine, so the port and login it used are PROVEN. Read
// as a plain failure it cost twice: the evidence was discarded, and a hint
// fired about a host that was never the problem.
func TestClassify_SSHPassesThroughTheRemoteStatus(t *testing.T) {
	if got := Classify("ssh", 1); got != VerdictPayload {
		t.Errorf("ssh exit 1 = %v, want payload — that is the remote command's status", got)
	}
	if got := Classify("ssh", 2); got != VerdictPayload {
		t.Errorf("ssh exit 2 = %v, want payload", got)
	}
	// 255 is ssh's own.
	if got := Classify("ssh", 255); got != VerdictTransport {
		t.Errorf("ssh exit 255 = %v, want transport", got)
	}
	if got := Classify("ssh", 0); got != VerdictSuccess {
		t.Errorf("ssh exit 0 = %v", got)
	}
}

// A payload verdict is positive evidence: the session provably worked. That is
// what lets the store learn from the common case, where the thing being run can
// fail on its own merits.
func TestVerdict_Confirms(t *testing.T) {
	for _, v := range []Verdict{VerdictSuccess, VerdictPayload} {
		if !v.Confirms() {
			t.Errorf("%v must confirm the connection arguments", v)
		}
	}
	for _, v := range []Verdict{VerdictTransport, VerdictUnknown} {
		if v.Confirms() {
			t.Errorf("%v must NOT be read as proof the session worked", v)
		}
	}
}

// scp and friends run nothing on the far side, so there is no remote status to
// pass through — every failure is the tool's own.
func TestClassify_ToolOnlyCommands(t *testing.T) {
	for _, bin := range []string{"scp", "sftp", "sshfs", "ssh-copy-id", "ssh-keyscan"} {
		if got := Classify(bin, 1); got != VerdictTransport {
			t.Errorf("%s exit 1 = %v, want transport — it runs nothing remotely", bin, got)
		}
		if got := Classify(bin, 0); got != VerdictSuccess {
			t.Errorf("%s exit 0 = %v", bin, got)
		}
	}
}

// psql documents 2 as a connection failure; 1 and 3 are SQL and script errors,
// both of which mean it got there.
func TestClassify_Psql(t *testing.T) {
	if got := Classify("psql", 2); got != VerdictTransport {
		t.Errorf("psql exit 2 = %v, want transport", got)
	}
	for _, st := range []int{1, 3} {
		if got := Classify("psql", st); got != VerdictPayload {
			t.Errorf("psql exit %d = %v, want payload — it connected", st, got)
		}
	}
}

// rsync's connection-layer codes are transport; its file-level codes mean the
// link worked and the transfer did not.
func TestClassify_Rsync(t *testing.T) {
	for _, st := range []int{10, 12, 30, 35, 255} {
		if got := Classify("rsync", st); got != VerdictTransport {
			t.Errorf("rsync exit %d = %v, want transport", st, got)
		}
	}
	for _, st := range []int{11, 23, 24} {
		if got := Classify("rsync", st); got != VerdictPayload {
			t.Errorf("rsync exit %d = %v, want payload — file-level, link was fine", st, got)
		}
	}
}

// A convention this package has not declared stays UNKNOWN rather than being
// assumed into either bucket. The caller may still hint on it; it must not
// record a fact from a status it cannot interpret.
func TestClassify_UndeclaredStaysUnknown(t *testing.T) {
	for _, bin := range []string{"git", "mysql", "redis-cli", "kubectl", "podman"} {
		if got := Classify(bin, 1); got != VerdictUnknown {
			t.Errorf("%s exit 1 = %v, want unknown — its convention is not declared", bin, got)
		}
		// Zero is unambiguous for everyone.
		if got := Classify(bin, 0); got != VerdictSuccess {
			t.Errorf("%s exit 0 = %v", bin, got)
		}
	}
	if got := Classify("some-unknown-binary", 1); got != VerdictUnknown {
		t.Errorf("unknown binary = %v", got)
	}
	if got := Classify("some-unknown-binary", 0); got != VerdictSuccess {
		t.Errorf("exit 0 is success even for an unknown binary, got %v", got)
	}
}

// RATCHET. A pass-through tool with no transport statuses declared would
// classify EVERY failure as payload — silently deciding that the connection
// always worked, which is the one reading that can never be checked.
func TestSpecs_ExitModelIsCoherent(t *testing.T) {
	for name, spec := range commandSpecs {
		if spec.ExitModel == ExitPassThrough && len(spec.TransportExits) == 0 {
			t.Errorf("%s is pass-through with no transport statuses: every failure "+
				"would read as 'the connection was fine', which is unfalsifiable", name)
		}
		if spec.ExitModel == ExitUnclassified && len(spec.TransportExits) > 0 {
			t.Errorf("%s declares transport statuses but no exit model, so they are "+
				"reachable but the rest of its statuses are not interpreted", name)
		}
		for st := range spec.TransportExits {
			if st == 0 {
				t.Errorf("%s lists 0 as a transport failure; 0 is success", name)
			}
		}
	}
}
