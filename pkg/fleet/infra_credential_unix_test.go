//go:build !windows

package fleet

import (
	"strings"
	"testing"

	"github.com/creack/pty/v2"
)

func TestInfraCredentialRefusesTerminal(t *testing.T) {
	isolatedFleetRoot(t)
	setupFakeCredentialMaster(t)

	ptmx, tty, err := pty.Open()
	if err != nil {
		t.Skipf("pty.Open: %v", err)
	}
	defer ptmx.Close()
	defer tty.Close()

	_, err = runCredentialWithReader(t, tty, "add", "--provider", "do", "--role", "purge")
	if err == nil || !strings.Contains(err.Error(), "refusing to read a credential from a terminal; pipe it instead") {
		t.Fatalf("expected terminal refusal, got: %v", err)
	}
	if !strings.Contains(err.Error(), "bashy ask --stdout | bashy infra credential add --provider P --role R") {
		t.Fatalf("expected hint in terminal refusal, got: %v", err)
	}
}
