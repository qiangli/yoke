//go:build darwin

package ctty

import "testing"

// injectDarwinSession pins the two world observations guiAttended makes on
// macOS: the launchd domain and who owns /dev/console. Root (0) owns the
// console device exactly when nobody is logged in at the machine.
func injectDarwinSession(t *testing.T, manager string, consoleOwner int) {
	t.Helper()
	for _, k := range []string{"SSH_CONNECTION", "SSH_CLIENT", "SSH_TTY"} {
		t.Setenv(k, "")
	}
	prevManager, prevOwner := launchctlManagerName, consoleDeviceOwnerUID
	launchctlManagerName = func() string { return manager }
	consoleDeviceOwnerUID = func() int { return consoleOwner }
	t.Cleanup(func() { launchctlManagerName, consoleDeviceOwnerUID = prevManager, prevOwner })
}

// A headless box must not take the GUI rung, even when the launchd domain says
// Aqua.
//
// That combination is real, not contrived: a managed, launchd-detached process
// can carry an Aqua managername from a desktop session that has since gone
// away, or whose window server the process cannot reach. Story f2c250c5f038 is
// exactly this shape — the GUI channel was chosen, the popup was dead within
// seconds without operator action, and the failure read as a cancel. Nobody
// pressed anything: root owns /dev/console because nobody is logged in at the
// machine, so the dialog had no screen to appear on and no human to cancel it.
func TestHeadlessConsoleNeverTakesTheGuiRung(t *testing.T) {
	t.Run("aqua domain but no console user", func(t *testing.T) {
		injectDarwinSession(t, "Aqua", 0)
		if guiAttended() {
			t.Error("guiAttended accepted a headless session (Aqua domain, root-owned console) — " +
				"a dialog there reaches nobody and its failure reads as an operator cancel")
		}
	})
	t.Run("unreadable console state fails closed", func(t *testing.T) {
		injectDarwinSession(t, "Aqua", -1)
		if guiAttended() {
			t.Error("guiAttended accepted a session whose console state is unknown — " +
				"uncertainty must fall back to the rendezvous, which always works")
		}
	})
}

// The gate must not over-reach: a logged-in console user in an Aqua domain is
// the case that has to keep working — an agent harness's setsid'd, tty-less
// child prompting on the desktop the operator is sitting at.
func TestConsoleUserSessionKeepsTheGuiRung(t *testing.T) {
	injectDarwinSession(t, "Aqua", 1000)
	if !guiAttended() {
		t.Error("guiAttended rejected a logged-in desktop session — the harness-child-on-a-desktop case must keep working")
	}
}

// A non-GUI launchd domain was already rejected and must stay rejected.
func TestNonAquaDomainIsStillRejected(t *testing.T) {
	injectDarwinSession(t, "Background", 1000)
	if guiAttended() {
		t.Error("guiAttended accepted a Background domain session")
	}
}
