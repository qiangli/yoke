package sdlc

import (
	"context"
	"strings"
)

// Executable deploy targets — the DIRECT/local deploy path, complementing the
// GitHub-Actions path (PromoteByLabel). This makes TargetConfig.{Command,
// Healthcheck,Rollback} actually run (previously they were only rendered into the
// conductor brief). The shape mirrors the CI/CD reference pipeline: run Command,
// verify with Healthcheck, and on failure run Rollback.

// DeployOutcome is the result of running a deploy target.
type DeployOutcome struct {
	SchemaVersion string `json:"schema_version"`
	Target        string `json:"target"`
	Environment   string `json:"environment"`
	Status        string `json:"status"` // deployed | failed | rolled-back | skipped
	Output        string `json:"output,omitempty"`
	RolledBack    bool   `json:"rolled_back,omitempty"`
}

// RunDeployTarget executes a TargetConfig in dir: Command → Healthcheck; on any
// failure it runs Rollback (when set) and reports rolled-back, else failed. A
// target with no Command is skipped (nothing to do). The returned error is the
// underlying command failure, if any, so callers can gate on it.
func RunDeployTarget(ctx context.Context, dir string, t TargetConfig) (DeployOutcome, error) {
	out := DeployOutcome{SchemaVersion: schemaVersion, Target: t.Name, Environment: t.Environment}
	if strings.TrimSpace(t.Command) == "" {
		out.Status = "skipped"
		return out, nil
	}

	var log strings.Builder
	o, err := runShellCommand(ctx, dir, t.Command)
	log.WriteString(o)
	if err == nil && strings.TrimSpace(t.Healthcheck) != "" {
		var herr error
		o, herr = runShellCommand(ctx, dir, t.Healthcheck)
		log.WriteString(o)
		err = herr
	}
	out.Output = log.String()

	if err != nil {
		if strings.TrimSpace(t.Rollback) != "" {
			ro, _ := runShellCommand(ctx, dir, t.Rollback)
			out.Output += ro
			out.RolledBack = true
			out.Status = "rolled-back"
		} else {
			out.Status = "failed"
		}
		return out, err
	}
	out.Status = "deployed"
	return out, nil
}
