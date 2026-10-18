package lens

import (
	"bytes"
	"context"
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
	"github.com/qiangli/yoke/pkg/fleet"
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

	// Seat specifies the receiving worker seat for delegation.
	Seat *WorkerSeat
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

	// Seat is the target worker seat name when planned for delegation.
	Seat string `json:"seat,omitempty"`

	// SeatHelp is the target worker seat help mode (must be HelpScript).
	SeatHelp string `json:"seat_help,omitempty"`

	// SeatNoInput is the target worker seat no_input mode (must be true).
	SeatNoInput bool `json:"seat_no_input,omitempty"`
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
	if p.Seat != "" {
		fmt.Fprintf(&b, "delegation_seat: %s (help:%s, no_input:%t)\n", p.Seat, p.SeatHelp, p.SeatNoInput)
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

	if opts.Seat != nil {
		opts.Seat.Normalize()
		if opts.Seat.Help != fleet.HelpScript {
			return nil, errorf(ExitInvalid, "seat %q has help mode %q, want %q (help:script required for delegation)", opts.Seat.Name, opts.Seat.Help, fleet.HelpScript)
		}
		if !opts.Seat.NoInput {
			return nil, errorf(ExitInvalid, "seat %q has no_input=false; no_input:true required for script delegation", opts.Seat.Name)
		}
		if len(opts.Seat.DeniedCaps) > 0 {
			if err := plan.VerifyDeniedCaps(opts.Seat.DeniedCaps); err != nil {
				return nil, err
			}
		}
		if opts.Seat.AllowedCaps != nil {
			if err := plan.VerifyCaps(opts.Seat.AllowedCaps); err != nil {
				return nil, err
			}
		}
		if opts.Seat.AllowedEffects != nil {
			if err := plan.VerifyEffects(opts.Seat.AllowedEffects); err != nil {
				return nil, err
			}
		}
		if opts.Seat.Guard != nil {
			if err := plan.VerifyGuard(opts.Seat.Guard); err != nil {
				return nil, err
			}
		}
		plan.Seat = opts.Seat.Name
		plan.SeatHelp = opts.Seat.Help
		plan.SeatNoInput = opts.Seat.NoInput
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

// WorkerSeat defines a receiving worker seat for script delegation (Sprint 446 S22).
// In accordance with the S22 contract, delegated worker seats must have
// help: script (fleet.HelpScript) and no_input: true.
type WorkerSeat struct {
	Name           string            `json:"name"`                      // Seat or worker identifier (e.g. "worker-local", "ci-seat")
	Role           string            `json:"role,omitempty"`            // Seat role (e.g. "worker", "ci")
	Help           string            `json:"help"`                      // Instruction form; must be "script" (fleet.HelpScript)
	NoInput        bool              `json:"no_input"`                  // Must be true for unattended script execution
	AllowedCaps    []string          `json:"allowed_caps,omitempty"`    // Permitted capabilities
	DeniedCaps     []string          `json:"denied_caps,omitempty"`     // Forbidden capabilities
	AllowedEffects []string          `json:"allowed_effects,omitempty"` // Permitted effects
	Guard          *advice.Cap       `json:"-"`                         // Optional receiving seat @guard cap
	Prefs          fleet.ReaderPrefs `json:"prefs,omitempty"`           // Optional reader preferences
	Runner         WorkerRunner      `json:"-"`                         // Execution interface for the seat
}

// Normalize ensures the seat profile defaults are resolved from its Prefs if unset.
func (s *WorkerSeat) Normalize() {
	if s.Help == "" && s.Prefs.Help != "" {
		s.Help = s.Prefs.Help
	}
	if !s.NoInput {
		if s.Prefs.Defaults != nil {
			if star, ok := s.Prefs.Defaults["*"]; ok && star != nil {
				if noInput, ok := star["no_input"].(bool); ok && noInput {
					s.NoInput = true
				}
			}
		}
	}
}

// WorkerRunner executes a validated script artifact.
type WorkerRunner interface {
	RunScript(ctx context.Context, artifact *CompiledScriptArtifact) (*ExecutionResult, error)
}

// WorkerRunnerFunc adapts a plain function to the WorkerRunner interface.
type WorkerRunnerFunc func(ctx context.Context, artifact *CompiledScriptArtifact) (*ExecutionResult, error)

// RunScript calls f(ctx, artifact).
func (f WorkerRunnerFunc) RunScript(ctx context.Context, artifact *CompiledScriptArtifact) (*ExecutionResult, error) {
	return f(ctx, artifact)
}

// ExecutionResult holds the output from a worker executing a script artifact.
type ExecutionResult struct {
	Recording *ScriptRecording `json:"recording,omitempty"` // Nil unless durable recording was acknowledged.
	ExitCode  int              `json:"exit_code"`
	Stdout    []byte           `json:"stdout,omitempty"`
	Stderr    []byte           `json:"stderr,omitempty"`
	Error     error            `json:"error,omitempty"`
}

// DelegationResult contains the outcome of delegating a validated script to a worker seat.
type DelegationResult struct {
	verifiedScript []byte

	Recording *ScriptRecording        `json:"recording,omitempty"`
	Plan      *ScriptPlan             `json:"plan"`
	Seat      string                  `json:"seat"`
	Artifact  *CompiledScriptArtifact `json:"artifact"`
	Body      []byte                  `json:"body"`                 // The script body passed to the worker seat
	Executed  bool                    `json:"executed"`             // True if worker runner was invoked
	Success   bool                    `json:"success"`              // True if worker ran successfully with exit code 0
	ExitCode  int                     `json:"exit_code"`            // Process exit status
	Stdout    []byte                  `json:"stdout,omitempty"`     // Captured stdout
	Stderr    []byte                  `json:"stderr,omitempty"`     // Captured stderr
	Error     error                   `json:"error,omitempty"`      // Execution error if any
	Promoted  bool                    `json:"promoted"`             // True if promoted to dag target or kb runbook
	TargetRef string                  `json:"target_ref,omitempty"` // Promoted reference
}

// PromotedArtifact is a compiled script promoted into a dag target or kb runbook.
type PromotedArtifact struct {
	Type     string `json:"type"`     // "dag" or "runbook"
	Ref      string `json:"ref"`      // "dag:<target>" or "kb:runbook/<slug>"
	Artifact string `json:"artifact"` // Artifact name (<digest>.sh)
	Digest   string `json:"digest"`   // sha256:<hex>
}

// CanRunNext reports whether a subsequent dependent task can run.
// S22 invariant: target failure cannot promote or run subsequent steps.
func (r *DelegationResult) CanRunNext() bool {
	return r != nil && r.Executed && r.Success && r.ExitCode == 0 && r.Error == nil
}

// Promote promotes a successfully delegated script artifact into a dag target or kb runbook.
// In accordance with S22 safety invariants, if the target failed (ExitCode != 0, Error != nil,
// or Success == false), Promote refuses fail-closed with ExitInvalid and returns an error.
func (r *DelegationResult) Promote(targetType, name string) (*PromotedArtifact, error) {
	if r == nil {
		return nil, errorf(ExitInvalid, "cannot promote nil delegation result")
	}
	if !r.Executed || !r.Success || r.ExitCode != 0 || r.Error != nil {
		return nil, errorf(ExitInvalid, "target failure cannot promote: script execution failed (exit %d: %v)", r.ExitCode, r.Error)
	}
	if r.Artifact == nil || r.Artifact.Digest == "" {
		return nil, errorf(ExitInvalid, "target cannot promote: missing compiled artifact")
	}
	targetType = strings.ToLower(strings.TrimSpace(targetType))
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, errorf(ExitInvalid, "promotion target name cannot be empty")
	}
	var ref string
	switch targetType {
	case "dag":
		ref = "dag:" + name
	case "runbook", "kb":
		ref = "kb:runbook/" + name
	default:
		return nil, errorf(ExitInvalid, "unsupported promotion target type %q (allowed: dag, runbook)", targetType)
	}
	r.Promoted = true
	r.TargetRef = ref
	return &PromotedArtifact{
		Type:     targetType,
		Ref:      ref,
		Artifact: r.Artifact.Name,
		Digest:   r.Artifact.Digest,
	}, nil
}

// Delegate delegates the validated content-addressed script as body to a receiving
// worker seat (Sprint 446 S22). It enforces the following production invariants:
//  1. The receiving seat MUST declare help:script (fleet.HelpScript) and no_input:true.
//     Any other help mode or interactive requirement fails closed.
//  2. Fails closed before any worker execution if required capabilities or effects
//     are denied by the receiving seat's AllowedCaps, DeniedCaps, AllowedEffects, or Guard.
//  3. Script bytes are handed off directly as body (no implicit model calls).
//  4. On worker failure, the result records failure and refuses promotion.
func (p *ScriptPlan) Delegate(ctx context.Context, seat WorkerSeat) (*DelegationResult, error) {
	if p == nil || p.Artifact == nil || len(p.Artifact.Script) == 0 {
		return nil, errorf(ExitInvalid, "cannot delegate empty or uncompiled script plan")
	}

	seat.Normalize()

	if seat.Help != fleet.HelpScript {
		return nil, errorf(ExitInvalid, "seat %q has help mode %q, want %q (help:script required for delegation)", seat.Name, seat.Help, fleet.HelpScript)
	}
	if !seat.NoInput {
		return nil, errorf(ExitInvalid, "seat %q has no_input=false; no_input:true required for script delegation", seat.Name)
	}

	if !p.DryRun && seat.Runner != nil && (len(seat.AllowedCaps) == 0 || len(seat.AllowedEffects) == 0) {
		return nil, errorf(ExitInvalid, "script execution requires nonempty declared capabilities and effects")
	}

	// Verify seat security caps/effects BEFORE any worker execution (fail closed).
	if len(seat.DeniedCaps) > 0 {
		if err := p.VerifyDeniedCaps(seat.DeniedCaps); err != nil {
			return nil, err
		}
	}
	if seat.AllowedCaps != nil {
		if err := p.VerifyCaps(seat.AllowedCaps); err != nil {
			return nil, err
		}
	}
	if seat.AllowedEffects != nil {
		if err := p.VerifyEffects(seat.AllowedEffects); err != nil {
			return nil, err
		}
	}
	if seat.Guard != nil {
		if err := p.VerifyGuard(seat.Guard); err != nil {
			return nil, err
		}
	}

	body := append([]byte(nil), p.Artifact.Script...)
	p.Seat = seat.Name
	p.SeatHelp = seat.Help
	p.SeatNoInput = seat.NoInput

	// If DryRun or no Runner supplied (handoff across repository boundary):
	if p.DryRun || seat.Runner == nil {
		return &DelegationResult{
			Plan:     p,
			Seat:     seat.Name,
			Artifact: p.Artifact,
			Body:     body,
			Executed: false,
			Success:  false,
			ExitCode: 0,
		}, nil
	}

	// Execute via worker runner. Preserve the process outcome even when a
	// subsequent operation (such as durable recording) fails.
	execRes, err := seat.Runner.RunScript(ctx, p.Artifact)
	if err != nil {
		res := &DelegationResult{Plan: p, Seat: seat.Name, Artifact: p.Artifact, Body: body, Executed: true, ExitCode: ExitInvalid, Error: err}
		if execRes != nil {
			res.ExitCode, res.Stdout, res.Stderr = execRes.ExitCode, execRes.Stdout, execRes.Stderr
			res.Recording = execRes.Recording
		}
		return res, err
	}

	if execRes == nil {
		err := errorf(ExitInvalid, "worker returned no execution outcome")
		return &DelegationResult{
			Plan:     p,
			Seat:     seat.Name,
			Artifact: p.Artifact,
			Body:     body,
			Executed: true,
			Success:  false,
			ExitCode: ExitInvalid,
			Error:    err,
		}, err
	}

	success := execRes.ExitCode == 0 && execRes.Error == nil
	res := &DelegationResult{
		Plan:      p,
		Seat:      seat.Name,
		Artifact:  p.Artifact,
		Body:      body,
		Executed:  true,
		Success:   success,
		ExitCode:  execRes.ExitCode,
		Recording: execRes.Recording,
		Stdout:    execRes.Stdout,
		Stderr:    execRes.Stderr,
		Error:     execRes.Error,
	}
	if !success && execRes.Error != nil {
		return res, execRes.Error
	}
	if !success {
		return res, errorf(ExitInvalid, "delegated script worker failed with exit code %d", execRes.ExitCode)
	}
	res.verifiedScript = append([]byte(nil), body...)
	return res, nil
}

// DelegatePipeline compiles, validates, plans, and delegates skill text to a worker seat.
func DelegatePipeline(ctx context.Context, skillText []byte, opts PipelineOptions, seat WorkerSeat) (*DelegationResult, error) {
	opts.Seat = &seat
	plan, err := PlanPipeline(skillText, opts)
	if err != nil {
		return nil, err
	}
	return plan.Delegate(ctx, seat)
}

// DelegateSkillScript compiles skill text and delegates it to seat using storeDir.
func DelegateSkillScript(ctx context.Context, skillText []byte, storeDir string, seat WorkerSeat) (*DelegationResult, error) {
	return DelegatePipeline(ctx, skillText, PipelineOptions{StoreDir: storeDir}, seat)
}
