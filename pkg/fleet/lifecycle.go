package fleet

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// AgentLifecycle identifies the work that owns a disposable definition. Run
// numbers are queue-local; a sprint UUID is host-independent. Ledger records
// continue to name the agent after its definition has been archived.
type AgentLifecycle struct {
	WeaveQueue string `yaml:"weave_queue,omitempty" json:"weave_queue,omitempty" doc:"absolute owning weave queue directory"`
	RunID      int64  `yaml:"run_id,omitempty" json:"run_id,omitempty" doc:"run number within the owning queue"`
	SprintID   string `yaml:"sprint_id,omitempty" json:"sprint_id,omitempty" doc:"owning sprint UUID"`
}

// MintAgentLifecycle applies only at creation, never during ordinary edits.
// A manager's child definitions belong to its sprint unless explicitly scoped.
func MintAgentLifecycle(a *Agent, sprint string) {
	if sprint == "" {
		sprint = os.Getenv("BASHY_MINT_SPRINT")
	}
	if sprint = strings.TrimSpace(sprint); sprint != "" {
		a.Ephemeral = true
		a.Lifecycle = &AgentLifecycle{SprintID: sprint}
	}
}

// ArchiveAgent preserves the resolved definition before removing its local
// entry. The caller supplies the lifecycle/liveness guard; no guard means no
// deletion. Lower-ring identities are never automatically retired.
func (c *Catalog) ArchiveAgent(name string, idle func(Agent) error) (string, error) {
	a, ok := c.Agent(name)
	if !ok {
		return "", nil
	}
	if !a.Ephemeral || a.Lifecycle == nil || a.Ring != ringLocal() {
		return "", fmt.Errorf("fleet: %s is not an owned local ephemeral agent", name)
	}
	if _, _, lower := c.lowerEntry(dirAgents, a.Name); lower {
		return "", fmt.Errorf("fleet: %s shadows a lower-ring identity", name)
	}
	if idle == nil {
		return "", fmt.Errorf("fleet: archive requires an idle guard")
	}
	if err := idle(a); err != nil {
		return "", err
	}
	body, err := yaml.Marshal(struct {
		Agents []Agent `yaml:"agents"`
	}{[]Agent{a}})
	if err != nil {
		return "", err
	}
	dir := filepath.Join(c.nounDir(dirAgents), "archive")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", err
	}
	f, err := os.CreateTemp(dir, a.Name+"-*.yaml")
	if err != nil {
		return "", err
	}
	path := f.Name()
	_, err = f.Write(body)
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return path, err
	}
	// Recheck after durable archival. Failed removal leaves recoverable evidence.
	if err := idle(a); err != nil {
		return path, err
	}
	if err := c.RemoveAgent(a.Name); err != nil {
		return path, err
	}
	return path, nil
}
