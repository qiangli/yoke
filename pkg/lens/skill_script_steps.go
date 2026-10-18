package lens

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"mvdan.cc/sh/v3/interp"
)

// ScriptStep has an explicit, bounded recovery policy. ReplacementSkill is
// supplied by the caller (including any explicit model planning outside this
// package); execution never calls a model. Retries replay identical bytes.
type ScriptStep struct {
	Name             string
	Skill            []byte
	Retries          int
	ReplacementSkill []byte
}

type ScriptAttempt struct {
	Step       string
	Recompiled bool
	Result     *DelegationResult
}

// ExecuteScriptSteps stops at the first unrecovered failure. Only that step is
// retried/recompiled; successful predecessors are never replayed. A replacement
// gets one attempt after the original's retries. Policy/compile errors stop
// immediately, as do cancellation and workers with no execution outcome.
func ExecuteScriptSteps(ctx context.Context, steps []ScriptStep, opts PipelineOptions, seat WorkerSeat) ([]ScriptAttempt, error) {
	if opts.DryRun || seat.Runner == nil {
		return nil, errorf(ExitInvalid, "step execution requires a runner and non-dry-run policy")
	}
	seen := map[string]bool{}
	for _, step := range steps {
		if step.Name == "" || seen[step.Name] || step.Retries < 0 || step.Retries > 3 {
			return nil, errorf(ExitInvalid, "invalid step name or retry bound (0..3)")
		}
		seen[step.Name] = true
	}
	var attempts []ScriptAttempt
	for _, step := range steps {
		opts.Seat = &seat
		plan, err := PlanPipeline(step.Skill, opts)
		if err != nil {
			return attempts, err
		}
		var result *DelegationResult
		for n := 0; n <= step.Retries; n++ {
			if err := ctx.Err(); err != nil {
				return attempts, err
			}
			result, err = plan.Delegate(ctx, seat)
			attempts = append(attempts, ScriptAttempt{Step: step.Name, Result: result})
			if result.CanRunNext() {
				break
			}
			_, processExit := interp.IsExitStatus(resultError(result))
			if result == nil || !result.Executed || (result.Error != nil && !processExit) {
				return attempts, err
			}
		}
		if !result.CanRunNext() && len(step.ReplacementSkill) > 0 {
			if err := ctx.Err(); err != nil {
				return attempts, err
			}
			plan, err = PlanPipeline(step.ReplacementSkill, opts)
			if err != nil {
				return attempts, err
			}
			result, err = plan.Delegate(ctx, seat)
			attempts = append(attempts, ScriptAttempt{Step: step.Name, Recompiled: true, Result: result})
		}
		if !result.CanRunNext() {
			if err == nil {
				err = errorf(ExitInvalid, "step %q has no successful outcome", step.Name)
			}
			return attempts, err
		}
	}
	return attempts, nil
}

var dagScriptTarget = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_-]*$`)

// PublishDAG publishes a new standalone DAG containing the exact successfully
// executed bytes. It never overwrites a file. The private execution snapshot
// prevents mutated or deserialized result objects from claiming verification.
// The runner remains the trusted execution boundary; this is not an attestation
// of arbitrary caller-provided runners. Existing Promote only describes a ref.
func (r *DelegationResult) PublishDAG(path, target string) (*PromotedArtifact, error) {
	if !r.CanRunNext() || len(r.verifiedScript) == 0 || r.Artifact == nil ||
		ScriptArtifactDigest(r.verifiedScript) != r.Artifact.Digest ||
		ScriptArtifactDigest(r.Artifact.Script) != r.Artifact.Digest ||
		ScriptArtifactName(r.Artifact.Script) != r.Artifact.Name {
		return nil, errorf(ExitInvalid, "promotion requires an unchanged verified execution")
	}
	if !dagScriptTarget.MatchString(target) {
		return nil, errorf(ExitInvalid, "invalid DAG target name")
	}
	if err := ValidatePureOutputScript(r.verifiedScript); err != nil {
		return nil, err
	}
	// The DAG parser accepts three-character fences only. Refuse embedded
	// fence lines (possible inside multiline literals) instead of changing bytes.
	for _, line := range strings.Split(string(r.verifiedScript), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "```") || strings.HasPrefix(line, "~~~") {
			return nil, errorf(ExitInvalid, "script contains a DAG fence line")
		}
	}
	fence := "```"
	body := fmt.Sprintf("# Verified script\n\nArtifact: %s\nDigest: %s\n\n## Tasks\n\n### %s\nEffects: pure\n%s bash\n%s\n%s\n", r.Artifact.Name, r.Artifact.Digest, target, fence, r.verifiedScript, fence)
	tmp, err := os.CreateTemp(filepath.Dir(path), ".dag-script-*")
	if err != nil {
		return nil, err
	}
	defer os.Remove(tmp.Name())
	if _, err = tmp.WriteString(body); err != nil {
		tmp.Close()
		return nil, err
	}
	if err = tmp.Sync(); err != nil {
		tmp.Close()
		return nil, err
	}
	if err = tmp.Close(); err != nil {
		return nil, err
	}
	if err = os.Link(tmp.Name(), path); err != nil {
		return nil, err
	}
	r.Promoted, r.TargetRef = true, "dag:"+path+"#"+target
	return &PromotedArtifact{Type: "dag", Ref: r.TargetRef, Artifact: r.Artifact.Name, Digest: r.Artifact.Digest}, nil
}

func resultError(r *DelegationResult) error {
	if r == nil {
		return nil
	}
	return r.Error
}
