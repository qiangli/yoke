// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

package meet

import "testing"

// A seat may ACT by default, and the whole host can be put back.
//
// Both halves are asserted because each is a decision someone will want to
// re-examine: the default is a risk acceptance (an unattended agent with write
// authority on an uncontained host), and the escape hatch is the answer to it.
// A default with no way back would be a policy rather than a setting.
func TestASeatActsUnlessTheHostSaysOtherwise(t *testing.T) {
	readOnly, allowUnsafe := turnAuthority()
	if readOnly || !allowUnsafe {
		t.Fatalf("default authority = readOnly:%v allowUnsafe:%v, want a seat that can act", readOnly, allowUnsafe)
	}

	for _, on := range []string{"1", "true", "YES", "on"} {
		t.Setenv(ReadOnlyTurnsEnv, on)
		readOnly, allowUnsafe = turnAuthority()
		if !readOnly {
			t.Errorf("%s=%s did not restore read-only", ReadOnlyTurnsEnv, on)
		}
		// Not merely redundant with ReadOnly winning downstream: a restricted
		// host must not also be told to skip the uncontained-host guard.
		if allowUnsafe {
			t.Errorf("%s=%s left the unsafe-launch acceptance on", ReadOnlyTurnsEnv, on)
		}
	}

	// Anything else is not an affirmative. A setting that quietly accepts a
	// typo as "off" is fine; one that accepts a typo as "on" would silently
	// disarm the fleet.
	for _, off := range []string{"", "0", "false", "no", "maybe"} {
		t.Setenv(ReadOnlyTurnsEnv, off)
		if readOnly, _ := turnAuthority(); readOnly {
			t.Errorf("%s=%q was read as a restriction", ReadOnlyTurnsEnv, off)
		}
	}
}
