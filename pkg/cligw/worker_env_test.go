package cligw

// Sprint: #290; Story: G0.13 follow-up (subscription seats never bill an inherited key)

import (
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/agentlaunch"
)

func TestWorkerEnvDropsInheritedVendorKeys(t *testing.T) {
	t.Setenv("BASHY_ALLOW_AGENT_SECRETS", "")
	parent := []string{
		"PATH=/usr/bin", "HOME=/home/u",
		"OPENAI_API_KEY=sk-door", "ANTHROPIC_API_KEY=sk-ant-door", "CODEX_API_KEY=x",
		"DEEPSEEK_API_KEY=sk-ds",
	}
	seat := workerEnv(parent, agentlaunch.Launch{Nick: "codex-gpt-5.5", Tool: "codex"})
	joined := "\n" + strings.Join(seat, "\n") + "\n"
	for _, gone := range []string{"OPENAI_API_KEY=", "ANTHROPIC_API_KEY=", "CODEX_API_KEY=", "DEEPSEEK_API_KEY="} {
		if strings.Contains(joined, "\n"+gone) {
			t.Errorf("a subscription seat inherited %s", gone)
		}
	}
	for _, kept := range []string{"PATH=/usr/bin", "HOME=/home/u", "BASHY_AGENT_ID=codex-gpt-5.5"} {
		if !strings.Contains(joined, "\n"+kept+"\n") {
			t.Errorf("worker env lost %s", kept)
		}
	}
	// A key-billed binding keeps exactly the credential its contract names.
	keyed := workerEnv(parent, agentlaunch.Launch{Nick: "ycode-deepseek", Tool: "ycode", PreserveEnv: []string{"DEEPSEEK_API_KEY"}})
	j2 := "\n" + strings.Join(keyed, "\n") + "\n"
	if !strings.Contains(j2, "\nDEEPSEEK_API_KEY=sk-ds\n") || strings.Contains(j2, "\nOPENAI_API_KEY=") {
		t.Errorf("keyed binding env wrong:\n%s", j2)
	}
}
