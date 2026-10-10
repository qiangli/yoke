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

// A custom tool reads its key under a protocol name (key_env: OPENAI_API_KEY)
// while the bound model's credential lives under its ref (ZAI_API_KEY). The
// door's worker must project it exactly as delegate, chat and weave do, or a
// custom tool:model agent cannot be served as a model (Sprint 406 live check).
func TestWorkerEnvProjectsKeyEnvAliases(t *testing.T) {
	t.Setenv("BASHY_ALLOW_AGENT_SECRETS", "")
	parent := []string{"PATH=/usr/bin", "HOME=/home/u", "ZAI_API_KEY=zk-1", "OPENAI_API_KEY=sk-door"}
	l := agentlaunch.Launch{
		Nick: "qwen-glm", Tool: "qwen-code",
		CredentialEnvAliases: map[string][]string{"OPENAI_API_KEY": {"ZAI_API_KEY"}},
	}
	joined := "\n" + strings.Join(workerEnv(parent, l), "\n") + "\n"
	if !strings.Contains(joined, "\nOPENAI_API_KEY=zk-1\n") {
		t.Errorf("worker env did not project the bound credential under OPENAI_API_KEY:\n%s", joined)
	}
	if strings.Contains(joined, "sk-door") {
		t.Errorf("worker env leaked the door's own key:\n%s", joined)
	}
}
