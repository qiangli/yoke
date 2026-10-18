package lens

import (
	"path/filepath"
)

// PipelineOptions configures the compile/validate pipeline (Sprint 446 S22).
type PipelineOptions struct {
	// StoreDir is the destination directory for content-addressed script artifacts.
	StoreDir string

	// Validator is the static script validation function run before artifact
	// storage or execution. Defaults to ValidatePureOutputScript if nil.
	Validator func(script []byte) error
}

// CompiledScriptArtifact holds the result of running the bounded compile/validate pipeline.
type CompiledScriptArtifact struct {
	Name   string // Artifact filename (<hex>.sh)
	Digest string // Canonical sha256 digest string ("sha256:<hex>")
	Path   string // Full path to the artifact on disk
	Script []byte // Extracted script bytes
}

// CompilePipeline executes the bounded compile/validate pipeline (Sprint 446 S22):
//  1. Deterministically extracts exactly one script fence from skill text using
//     the same extraction rules as lens -f skill -t script (rejecting ambiguous,
//     missing, unterminated, or multiple shell fences and empty bodies).
//  2. Statically validates the extracted script using ValidatePureOutputScript
//     (or opts.Validator if supplied) before any execution.
//  3. Stores the validated script bytes atomically using StoreScriptArtifact.
//
// Compile must not call a model or execute a script. It produces no side effects
// on failure: no artifact file is written and no command is run.
func CompilePipeline(skillText []byte, opts PipelineOptions) (*CompiledScriptArtifact, error) {
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

	name, digest, err := StoreScriptArtifact(opts.StoreDir, script)
	if err != nil {
		return nil, err
	}

	return &CompiledScriptArtifact{
		Name:   name,
		Digest: digest,
		Path:   filepath.Join(opts.StoreDir, name),
		Script: script,
	}, nil
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
