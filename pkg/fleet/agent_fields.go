package fleet

import "strings"

// ValidPermission reports whether p is an agent permission mode.
func ValidPermission(p string) bool {
	switch p {
	case PermissionAuto, PermissionAsk, PermissionReview, PermissionDeny:
		return true
	default:
		return false
	}
}

// BindingFor selects the target for an inbound channel and peer. Peer-specific
// bindings win over channel bindings, which win over the global default. This
// is deliberately pure: OpenClaw and later channel adapters can use the same
// matching rule without importing transport code.
func BindingFor(bindings []AgentBinding, channel, sender string) (string, bool) {
	channel, sender = strings.TrimSpace(channel), strings.TrimSpace(sender)
	best, bestScore := "", -1
	for _, b := range bindings {
		if strings.TrimSpace(b.Agent) == "" {
			continue
		}
		bc, bs := strings.TrimSpace(b.Channel), strings.TrimSpace(b.Sender)
		score := -1
		switch {
		case bc == "" && bs == "":
			score = 0
		case bc == channel && bs == "":
			score = 1
		case bc == channel && bs == sender && sender != "":
			score = 2
		}
		if score > bestScore {
			best, bestScore = b.Agent, score
		}
	}
	return best, bestScore >= 0
}

// BindingFor returns the selected inbound route declared by this agent.
func (a Agent) BindingFor(channel, sender string) (string, bool) {
	return BindingFor(a.Bindings, channel, sender)
}
