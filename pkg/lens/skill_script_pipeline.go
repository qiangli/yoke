package lens

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"mvdan.cc/sh/v3/syntax"

	"github.com/qiangli/yoke/pkg/atlas"
	"github.com/qiangli/yoke/pkg/policy/advice"
)

// PipelineOptions configures the compile/validate/plan pipeline (Sprint 446 S22).
type PipelineOptions struct {
	// StoreDir is the destination directory for content-addressed script artifacts.
	StoreDir string

	// Validator is the static script validation function run before artifact
	// storage or execution. Defaults to ValidatePureOutputScript if nil.
	Validator func(script []byte) error

	// AllowedCaps defines the capabilities permitted by the caller or guard.
	// If set (non-nil), any required capability not in AllowedCaps is denied fail-closed.
	AllowedCaps []string

	// DeniedCaps defines explicitly forbidden capabilities.
	// If any required capability is in DeniedCaps, it is denied fail-closed.
	DeniedCaps []string

	// AllowedEffects defines the permitted security effects (e.g. from atlas.Effects()).
	// If set (non-nil), any required effect not in AllowedEffects is denied fail-closed.
	AllowedEffects []string

	// Guard specifies an effect cap guard from pkg/policy/advice.
	// If set, required effects are checked against Guard.Exceeded().
	Guard *advice.Cap

	// RequireCaps, if true, requires that AllowedCaps be explicitly provided.
	// If true and AllowedCaps is nil or empty, fails closed with missing capability error.
	RequireCaps bool

	// DryRun, if true, executes zero worker execution and creates no artifact files on disk.
	DryRun bool
}

// CompiledScriptArtifact holds the result of running the bounded compile/validate pipeline.
type CompiledScriptArtifact struct {
	Name   string `json:"name"`   // Artifact filename (<hex>.sh)
	Digest string `json:"digest"` // Canonical sha256 digest string ("sha256:<hex>")
	Path   string `json:"path"`   // Full path to the artifact on disk
	Script []byte `json:"script"` // Extracted script bytes
}

// ScriptPlan is the typed declaration of a compiled and validated script's
// content-addressed artifact, required effects, and capabilities (Sprint 446 S22).
type ScriptPlan struct {
	// Artifact contains the content-addressed script artifact.
	Artifact *CompiledScriptArtifact `json:"artifact"`

	// Effects is the required security effects from the closed atlas vocabulary
	// (e.g., atlas.EffPure, atlas.EffRead).
	Effects []string `json:"effects"`

	// Capabilities is the required capabilities from the closed atlas vocabulary
	// (e.g., atlas.CapReadOnly).
	Capabilities []string `json:"capabilities"`

	// Commands is the list of unique commands invoked by the script.
	Commands []string `json:"commands"`

	// Reversibility is the atlas reversibility classification (e.g., atlas.RevReadonly).
	Reversibility string `json:"reversibility,omitempty"`

	// DryRun reports whether this plan was generated under zero-execution dry-run mode.
	DryRun bool `json:"dry_run,omitempty"`
}

// Caps returns the declared/required capabilities.
func (p *ScriptPlan) Caps() []string {
	return append([]string(nil), p.Capabilities...)
}

// VerifyCaps verifies that all required capabilities are permitted by allowedCaps.
// If allowedCaps is nil, no check is performed.
func (p *ScriptPlan) VerifyCaps(allowedCaps []string) error {
	if allowedCaps == nil {
		return nil
	}
	for _, reqCap := range p.Capabilities {
		if !slices.Contains(allowedCaps, reqCap) {
			return errorf(ExitInvalid, "script requires capability %q which is denied or missing from allowed capabilities: %v", reqCap, allowedCaps)
		}
	}
	return nil
}

// VerifyDeniedCaps verifies that no required capability is in deniedCaps.
func (p *ScriptPlan) VerifyDeniedCaps(deniedCaps []string) error {
	for _, reqCap := range p.Capabilities {
		if slices.Contains(deniedCaps, reqCap) {
			return errorf(ExitInvalid, "script requires denied capability %q", reqCap)
		}
	}
	return nil
}

// VerifyEffects verifies that all required effects are permitted by allowedEffects.
// If allowedEffects is nil, no check is performed.
func (p *ScriptPlan) VerifyEffects(allowedEffects []string) error {
	if allowedEffects == nil {
		return nil
	}
	for _, reqEff := range p.Effects {
		if !slices.Contains(allowedEffects, reqEff) {
			return errorf(ExitInvalid, "script requires effect %q which is denied (allowed: %v)", reqEff, allowedEffects)
		}
	}
	return nil
}

// VerifyGuard verifies that the required effects do not exceed the advice.Cap guard.
func (p *ScriptPlan) VerifyGuard(guard *advice.Cap) error {
	if guard == nil {
		return nil
	}
	if denied := guard.Exceeded(p.Effects); len(denied) > 0 {
		return errorf(ExitInvalid, "guard denied effects %v (allowed: %s)", denied, guard.String())
	}
	return nil
}

// Explain formats the plan into a zero-execution diagnostic view showing
// the content-addressed artifact (name, digest, path) and required caps/effects.
func (p *ScriptPlan) Explain() string {
	var b strings.Builder
	name := ""
	digest := ""
	path := ""
	if p.Artifact != nil {
		name = p.Artifact.Name
		digest = p.Artifact.Digest
		path = p.Artifact.Path
	}
	fmt.Fprintf(&b, "artifact: %s\n", name)
	fmt.Fprintf(&b, "digest: %s\n", digest)
	if path != "" {
		fmt.Fprintf(&b, "path: %s\n", path)
	}
	fmt.Fprintf(&b, "required_caps: %s\n", strings.Join(p.Capabilities, ", "))
	fmt.Fprintf(&b, "effects: %s\n", strings.Join(p.Effects, ", "))
	if len(p.Commands) > 0 {
		fmt.Fprintf(&b, "commands: %s\n", strings.Join(p.Commands, ", "))
	}
	if p.Reversibility != "" {
		fmt.Fprintf(&b, "reversibility: %s\n", p.Reversibility)
	}
	fmt.Fprintf(&b, "execution: zero-execution dry-run (no worker executed)\n")
	return strings.TrimRight(b.String(), "\n")
}

// ExplainJSON serializes the plan into indented JSON.
func (p *ScriptPlan) ExplainJSON() ([]byte, error) {
	return json.MarshalIndent(p, "", "  ")
}

// String returns the plan's explain representation.
func (p *ScriptPlan) String() string {
	return p.Explain()
}

// AnalyzeScript parses script AST and derives its invoked commands, required security
// effects from the atlas vocabulary, required capabilities, and reversibility class.
func AnalyzeScript(script []byte) (commands []string, effects []string, caps []string, reversibility string, err error) {
	p := syntax.NewParser(syntax.Variant(syntax.LangBash))
	f, err := p.Parse(bytes.NewReader(script), "")
	if err != nil {
		return nil, nil, nil, "", errorf(ExitInvalid, "invalid script: syntax error: %v", err)
	}

	cmdSet := make(map[string]bool)
	for _, st := range f.Stmts {
		collectStmtCmds(st, cmdSet)
	}

	for cmd := range cmdSet {
		commands = append(commands, cmd)
	}
	sort.Strings(commands)

	effSet := make(map[string]bool)
	capSet := make(map[string]bool)
	worstRev := atlas.RevReadonly

	for _, cmd := range commands {
		e, ok := atlas.Lookup(cmd)
		if !ok {
			effSet["unknown"] = true
			continue
		}
		for _, ef := range e.Effects {
			effSet[ef] = true
		}
		for _, cp := range e.Caps {
			capSet[cp] = true
		}
		if pureOutputCmds[cmd] {
			capSet[atlas.CapReadOnly] = true
		}
		if e.Reversibility != "" {
			worstRev = maxReversibility(worstRev, e.Reversibility)
		}
	}

	if len(commands) == 0 {
		effSet[atlas.EffPure] = true
		capSet[atlas.CapReadOnly] = true
	} else if len(effSet) == 0 {
		effSet[atlas.EffPure] = true
		capSet[atlas.CapReadOnly] = true
	} else if len(effSet) > 1 && effSet[atlas.EffPure] {
		// Atlas invariant: EffPure cannot be combined with governed side effects.
		delete(effSet, atlas.EffPure)
	}

	for ef := range effSet {
		effects = append(effects, ef)
	}
	sort.Strings(effects)

	for cp := range capSet {
		caps = append(caps, cp)
	}
	sort.Strings(caps)

	return commands, effects, caps, worstRev, nil
}

func collectStmtCmds(st *syntax.Stmt, cmdSet map[string]bool) {
	if st == nil {
		return
	}
	switch cmd := st.Cmd.(type) {
	case *syntax.CallExpr:
		if len(cmd.Args) > 0 {
			if name, err := literalWord(cmd.Args[0], syntax.LangBash); err == nil && name != "" {
				cmdSet[name] = true
			}
		}
	case *syntax.BinaryCmd:
		collectStmtCmds(cmd.X, cmdSet)
		collectStmtCmds(cmd.Y, cmdSet)
	case *syntax.Block:
		for _, s := range cmd.Stmts {
			collectStmtCmds(s, cmdSet)
		}
	case *syntax.Subshell:
		for _, s := range cmd.Stmts {
			collectStmtCmds(s, cmdSet)
		}
	case *syntax.IfClause:
		for _, s := range cmd.Then {
			collectStmtCmds(s, cmdSet)
		}
		if cmd.Else != nil {
			collectStmtCmds(&syntax.Stmt{Cmd: cmd.Else}, cmdSet)
		}
	case *syntax.ForClause:
		for _, s := range cmd.Do {
			collectStmtCmds(s, cmdSet)
		}
	case *syntax.WhileClause:
		for _, s := range cmd.Do {
			collectStmtCmds(s, cmdSet)
		}
	}
}

func reversibilityRank(rev string) int {
	for i, r := range atlas.Reversibilities() {
		if r == rev {
			return i
		}
	}
	return 0
}

func maxReversibility(a, b string) string {
	if reversibilityRank(b) > reversibilityRank(a) {
		return b
	}
	return a
}

// PlanPipeline executes the bounded compile/validate/plan pipeline (Sprint 446 S22):
//  1. Deterministically extracts exactly one script fence from skill text using
//     the same extraction rules as lens -f skill -t script (rejecting ambiguous,
//     missing, unterminated, or multiple shell fences and empty bodies).
//  2. Statically validates the extracted script using ValidatePureOutputScript
//     (or opts.Validator if supplied) before any execution or storage.
//  3. Analyzes the validated script AST to build a typed ScriptPlan declaring its
//     content-addressed artifact, required atlas effects, capabilities, and reversibility.
//  4. Fails closed before any worker execution if required capabilities or effects
//     are denied or missing under opts.AllowedCaps, opts.DeniedCaps, opts.AllowedEffects,
//     or opts.Guard.
//  5. In DryRun mode: executes zero execution and produces no side effects on disk,
//     performing digest verification if a preexisting artifact is found in StoreDir.
//     In non-DryRun mode: stores the artifact atomically using StoreScriptArtifact.
func PlanPipeline(skillText []byte, opts PipelineOptions) (*ScriptPlan, error) {
	script, err := ExtractSkillScript(skillText)
	if err != nil {
		return nil, err
	}

	validator := opts.Validator
	if validator == nil {
		validator = ValidatePureOutputScript
	}
	if err := validator(script); err != nil {
		return nil, err
	}

	commands, effects, caps, rev, err := AnalyzeScript(script)
	if err != nil {
		return nil, err
	}

	name := ScriptArtifactName(script)
	digest := ScriptArtifactDigest(script)
	path := ""
	if opts.StoreDir != "" {
		path = filepath.Join(opts.StoreDir, name)
	}

	plan := &ScriptPlan{
		Artifact: &CompiledScriptArtifact{
			Name:   name,
			Digest: digest,
			Path:   path,
			Script: script,
		},
		Effects:       effects,
		Capabilities:  caps,
		Commands:      commands,
		Reversibility: rev,
		DryRun:        opts.DryRun,
	}

	// Fail closed on missing/denied capabilities and effects BEFORE any storage or worker execution.
	if opts.RequireCaps && len(opts.AllowedCaps) == 0 {
		return nil, errorf(ExitInvalid, "missing required capability declaration")
	}
	if len(opts.DeniedCaps) > 0 {
		if err := plan.VerifyDeniedCaps(opts.DeniedCaps); err != nil {
			return nil, err
		}
	}
	if opts.AllowedCaps != nil {
		if err := plan.VerifyCaps(opts.AllowedCaps); err != nil {
			return nil, err
		}
	}
	if opts.AllowedEffects != nil {
		if err := plan.VerifyEffects(opts.AllowedEffects); err != nil {
			return nil, err
		}
	}
	if opts.Guard != nil {
		if err := plan.VerifyGuard(opts.Guard); err != nil {
			return nil, err
		}
	}

	// Zero-execution dry-run mode: do not write to store dir, but verify existing artifact digest if present.
	if opts.DryRun {
		if opts.StoreDir != "" {
			target := filepath.Join(opts.StoreDir, name)
			if fi, statErr := os.Lstat(target); statErr == nil {
				if fi.Mode()&os.ModeSymlink != 0 {
					return nil, errorf(ExitInvalid, "script artifact %s is a symlink", name)
				}
				if !fi.Mode().IsRegular() {
					return nil, errorf(ExitInvalid, "script artifact %s is not a regular file", name)
				}
				existing, readErr := os.ReadFile(target)
				if readErr != nil {
					return nil, readErr
				}
				if !bytes.Equal(existing, script) {
					return nil, errorf(ExitInvalid, "script artifact %s exists with differing content", name)
				}
				existingDigest := ScriptArtifactDigest(existing)
				if existingDigest != digest {
					return nil, errorf(ExitInvalid, "script artifact %s digest mismatch: %s != %s", name, existingDigest, digest)
				}
			} else if !errors.Is(statErr, os.ErrNotExist) && !os.IsNotExist(statErr) {
				return nil, statErr
			}
		}
		return plan, nil
	}

	// Non-dry-run mode: store artifact atomically.
	if opts.StoreDir != "" {
		storedName, storedDigest, err := StoreScriptArtifact(opts.StoreDir, script)
		if err != nil {
			return nil, err
		}
		plan.Artifact.Name = storedName
		plan.Artifact.Digest = storedDigest
		plan.Artifact.Path = filepath.Join(opts.StoreDir, storedName)
	}

	return plan, nil
}

// CompilePipeline executes the bounded compile/validate pipeline (Sprint 446 S22):
//  1. Deterministically extracts exactly one script fence from skill text using
//     the same extraction rules as lens -f skill -t script (rejecting ambiguous,
//     missing, unterminated, or multiple shell fences and empty bodies).
//  2. Statically validates the extracted script using ValidatePureOutputScript
//     (or opts.Validator if supplied) before any execution.
//  3. Analyzes effects and caps, failing closed before storage if denied.
//  4. Stores the validated script bytes atomically using StoreScriptArtifact.
func CompilePipeline(skillText []byte, opts PipelineOptions) (*CompiledScriptArtifact, error) {
	plan, err := PlanPipeline(skillText, opts)
	if err != nil {
		return nil, err
	}
	return plan.Artifact, nil
}

// DryRunPipeline compiles, validates, and plans skill text in zero-execution
// dry-run mode without creating any artifact files on disk.
func DryRunPipeline(skillText []byte, opts PipelineOptions) (*ScriptPlan, error) {
	opts.DryRun = true
	return PlanPipeline(skillText, opts)
}

// ExplainPipeline compiles, validates, and plans skill text in zero-execution
// dry-run mode and returns its human-readable explanation.
func ExplainPipeline(skillText []byte, opts PipelineOptions) (string, error) {
	plan, err := DryRunPipeline(skillText, opts)
	if err != nil {
		return "", err
	}
	return plan.Explain(), nil
}

// CompileSkillScriptPipeline compiles skill text to an extracted script, validates
// it against the pure-output gate, and stores it atomically as a content-addressed artifact.
func CompileSkillScriptPipeline(dir string, skillText []byte) (*CompiledScriptArtifact, error) {
	return CompilePipeline(skillText, PipelineOptions{StoreDir: dir})
}

// CompileSkillScript runs the bounded compile/validate pipeline and stores the artifact
// in dir. It returns the artifact filename and its sha256 digest ("sha256:<hex>").
func CompileSkillScript(dir string, skillText []byte) (name string, digest string, err error) {
	art, err := CompileSkillScriptPipeline(dir, skillText)
	if err != nil {
		return "", "", err
	}
	return art.Name, art.Digest, nil
}

// CompileScriptPipeline is an alias for CompileSkillScript.
func CompileScriptPipeline(dir string, skillText []byte) (name string, digest string, err error) {
	return CompileSkillScript(dir, skillText)
}

// CompileScript is an alias for CompileSkillScript.
func CompileScript(dir string, skillText []byte) (name string, digest string, err error) {
	return CompileSkillScript(dir, skillText)
}

// CompileAndValidateSkill extracts and validates the script bytes without storing.
func CompileAndValidateSkill(skillText []byte) ([]byte, error) {
	script, err := ExtractSkillScript(skillText)
	if err != nil {
		return nil, err
	}
	if err := ValidatePureOutputScript(script); err != nil {
		return nil, err
	}
	return script, nil
}

// PlanSkillScript is a helper to plan skill text with a destination store dir.
func PlanSkillScript(dir string, skillText []byte) (*ScriptPlan, error) {
	return PlanPipeline(skillText, PipelineOptions{StoreDir: dir})
}

// DryRunSkillScript is a helper to dry-run skill text with a store dir without writing files.
func DryRunSkillScript(dir string, skillText []byte) (*ScriptPlan, error) {
	return DryRunPipeline(skillText, PipelineOptions{StoreDir: dir})
}

// ExplainSkillScript is a helper returning the explanation of a skill script plan.
func ExplainSkillScript(dir string, skillText []byte) (string, error) {
	return ExplainPipeline(skillText, PipelineOptions{StoreDir: dir})
}
