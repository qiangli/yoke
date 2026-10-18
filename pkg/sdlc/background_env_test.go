package sdlc

import (
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/chat"
)

func TestBackgroundSupervisorEnvStampsNamedConductor(t *testing.T) {
	env := backgroundSupervisorEnv([]string{"PATH=/bin"}, chat.Result{Nick: "007", Agent: "claude"})
	joined := strings.Join(env, "\n")
	for _, want := range []string{"BASHY_PRINCIPAL=dhnt:agent/007", "BASHY_AGENT_ID=007", "PATH=/bin"} {
		if !strings.Contains(joined, want) {
			t.Errorf("env missing %q: %v", want, env)
		}
	}
}

func TestBackgroundSupervisorEnvLeavesBareToolAlone(t *testing.T) {
	env := backgroundSupervisorEnv([]string{"PATH=/bin"}, chat.Result{Nick: "claude", Agent: "claude"})
	if len(env) != 1 || env[0] != "PATH=/bin" {
		t.Errorf("bare tool env changed: %v", env)
	}
}
