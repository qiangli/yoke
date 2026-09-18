package meet

import (
	"strings"
	"testing"
)

func TestShellRouting(t *testing.T) {
	t.Setenv("BASHY_FORCE_AGENT_SHELL", "")
	if got := shellRouting("claude"); !strings.Contains(got, "bashy") {
		t.Fatalf("claude should be env-forced to bashy: %q", got)
	}
	if got := shellRouting("codex"); !strings.Contains(got, "install-agent codex") {
		t.Fatalf("codex should advise install-agent: %q", got)
	}
	t.Setenv("BASHY_FORCE_AGENT_SHELL", "0")
	if got := shellRouting("claude"); !strings.Contains(got, "system") {
		t.Fatalf("kill-switch should report system shell: %q", got)
	}
}
