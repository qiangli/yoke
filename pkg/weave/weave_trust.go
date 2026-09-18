package weave

import (
	"strings"

	"github.com/qiangli/yoke/pkg/agentctl"
)

// Trust handling moved to pkg/agentctl — getting an agent CLI past its first-run
// prompt is a problem for anything that launches one unattended, not just for a
// weave worker. What is left here is weave's name for it.

type weaveTrustLaunch struct {
	Preseed string
	Clear   string
}

func weaveTrustLaunchFor(toolName string) weaveTrustLaunch {
	p, ok := agentctl.ProfileFor(strings.TrimSpace(toolName))
	if !ok {
		return weaveTrustLaunch{}
	}
	return weaveTrustLaunch{Preseed: p.Preseed, Clear: p.Clear}
}

func weaveTrustClearPayload(spec string) (string, bool) { return agentctl.ClearPayload(spec) }

func weaveApplyTrustPreseed(workspace, preseed string) error {
	return agentctl.ApplyTrustPreseed(workspace, preseed)
}
