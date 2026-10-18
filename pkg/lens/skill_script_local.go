package lens

// Derived from AGY run 500, principally 8632d64d (see the S22 delivery evidence).
// These are local library seams, not a production weave receiving seat.
import (
	"bytes"
	"context"
	"io"
	"os"

	"mvdan.cc/sh/v3/expand"
	"mvdan.cc/sh/v3/interp"
	"mvdan.cc/sh/v3/syntax"
)

// LocalScriptRunner executes only literal pure-output scripts locally under declared effects,
// ensuring zero implicit model calls and strict fail-closed enforcement of security caps.
type LocalScriptRunner struct {
	AllowedCaps    []string
	DeniedCaps     []string
	AllowedEffects []string
	WorkDir        string
	Env            []string
}

// RunScript statically verifies declared capabilities and effects, then executes the script locally.
func (r *LocalScriptRunner) RunScript(ctx context.Context, artifact *CompiledScriptArtifact) (*ExecutionResult, error) {
	if r == nil || artifact == nil || len(artifact.Script) == 0 {
		return nil, errorf(ExitInvalid, "cannot run empty or uncompiled script artifact")
	}

	// Revalidate at the execution boundary: exported artifacts are mutable and
	// the plan's custom validator or effect analysis is not an execution guard.
	if artifact.Digest != ScriptArtifactDigest(artifact.Script) || artifact.Name != ScriptArtifactName(artifact.Script) {
		return nil, errorf(ExitInvalid, "script artifact identity mismatch")
	}
	if err := ValidatePureOutputScript(artifact.Script); err != nil {
		return nil, err
	}
	// Statically analyze only the bounded, literal pure-output language.
	_, effects, caps, _, err := AnalyzeScript(artifact.Script)
	if err != nil {
		return nil, err
	}

	// Execution requires the caller to explicitly authorize capabilities and
	// effects, even within the bounded pure-output language.
	if len(r.AllowedCaps) == 0 {
		return nil, errorf(ExitInvalid, "local script execution denied: missing explicit nonempty allowed capability policy (fail-closed; arbitrary capabilities forbidden)")
	}
	if len(r.AllowedEffects) == 0 {
		return nil, errorf(ExitInvalid, "local script execution denied: missing explicit nonempty allowed effect policy (fail-closed; arbitrary effects forbidden)")
	}

	// Verify denied capabilities (fail closed).
	for _, denied := range r.DeniedCaps {
		for _, cp := range caps {
			if cp == denied {
				return nil, errorf(ExitInvalid, "script execution denied: capability %q is explicitly forbidden", denied)
			}
		}
	}

	// Verify required capabilities against allowed policy (fail closed).
	allowedCapMap := make(map[string]bool, len(r.AllowedCaps))
	for _, ac := range r.AllowedCaps {
		allowedCapMap[ac] = true
	}
	for _, cp := range caps {
		if !allowedCapMap[cp] {
			return nil, errorf(ExitInvalid, "script execution denied: capability %q is not in allowed capabilities %v", cp, r.AllowedCaps)
		}
	}

	// Verify required effects against allowed policy (fail closed).
	allowedEffMap := make(map[string]bool, len(r.AllowedEffects))
	for _, ae := range r.AllowedEffects {
		allowedEffMap[ae] = true
	}
	for _, ef := range effects {
		if !allowedEffMap[ef] {
			return nil, errorf(ExitInvalid, "script execution denied: effect %q is not in allowed effects %v", ef, r.AllowedEffects)
		}
	}

	// Interpret only builtins in process, with no inherited environment, shell
	// startup files, external commands, model hooks, or file-opening handler.
	prog, err := syntax.NewParser().Parse(bytes.NewReader(artifact.Script), artifact.Name)
	if err != nil {
		return nil, err
	}
	var stdout, stderr bytes.Buffer
	runner, err := interp.New(
		interp.Dir(r.WorkDir),
		interp.Env(expand.ListEnviron(r.Env...)),
		interp.StdIO(nil, &stdout, &stderr),
		interp.ExecHandler(func(context.Context, []string) error {
			return errorf(ExitInvalid, "external commands forbidden in local script execution")
		}),
		interp.OpenHandler(func(context.Context, string, int, os.FileMode) (io.ReadWriteCloser, error) {
			return nil, errorf(ExitInvalid, "file access forbidden in local script execution")
		}),
	)
	if err != nil {
		return nil, err
	}
	runErr := runner.Run(ctx, prog)
	exitCode := 0
	if runErr != nil {
		if code, ok := interp.IsExitStatus(runErr); ok {
			exitCode = int(code)
		} else {
			exitCode = ExitInvalid
		}
	}

	return &ExecutionResult{
		ExitCode: exitCode,
		Stdout:   stdout.Bytes(),
		Stderr:   stderr.Bytes(),
		Error:    runErr,
	}, nil
}

// ExecuteTask executes a WeaveReceivingSeatTask locally under declared effects.
func (r *LocalScriptRunner) ExecuteTask(ctx context.Context, task WeaveReceivingSeatTask) (*ExecutionResult, error) {
	art := &CompiledScriptArtifact{
		Name:   task.ArtifactName,
		Digest: task.Digest,
		Script: task.Script,
	}
	return r.RunScript(ctx, art)
}

// WeaveReceivingSeatTask represents the task payload delivered to a weave receiving seat.
// It carries the exact content-addressed script body and mandatory help:script,no_input:true metadata.
type WeaveReceivingSeatTask struct {
	Digest       string            `json:"digest"`             // sha256:<hex> content digest
	ArtifactName string            `json:"artifact_name"`      // <hex>.sh filename
	Script       []byte            `json:"script"`             // Exact content-addressed script body
	Help         string            `json:"help"`               // Must be "script" (fleet.HelpScript)
	NoInput      bool              `json:"no_input"`           // Must be true
	Seat         string            `json:"seat"`               // Seat name
	Sprint       int               `json:"sprint,omitempty"`   // Sprint number (e.g. 446)
	StoryID      string            `json:"story_id,omitempty"` // Story ID (e.g. "70b3848b08a3" or "#2141")
	Title        string            `json:"title,omitempty"`    // Task title
	RunID        int64             `json:"run_id,omitempty"`   // Assigned weave run ID
	Caps         []string          `json:"caps,omitempty"`     // Declared capabilities
	Metadata     map[string]string `json:"metadata,omitempty"` // Receiving seat metadata (e.g. help: script, no_input: true)
}

// WeaveSeatWorker defines an execution worker attached to a weave receiving seat that processes WeaveReceivingSeatTask.
type WeaveSeatWorker interface {
	ExecuteTask(ctx context.Context, task WeaveReceivingSeatTask) (*ExecutionResult, error)
}

// WeaveResultRecordingWorker is a WeaveSeatWorker that explicitly records the completion
// and execution outcome of a weave run into the receiving seat's queue or result store.
type WeaveResultRecordingWorker interface {
	WeaveSeatWorker
	RecordResult(ctx context.Context, runID int64, res *ExecutionResult) error
}

// ScriptRecording is a receipt returned only after a recorder durably stores
// the exact ExecutionResult. Ref identifies that record for later readback.
// A receipt from an injected recorder is not evidence of a production weave run.
type ScriptRecording struct {
	RunID int64  `json:"run_id"`
	Ref   string `json:"ref"`
}

// LocalWeaveWorker is an experimental local worker with an injected durable
// recorder. No production weave recorder is supplied by this package.
// ExecuteTask executes and records exactly once; RecordResult is also usable
// separately for callers that already have an execution outcome.
type LocalWeaveWorker struct {
	AllowedCaps    []string
	DeniedCaps     []string
	AllowedEffects []string
	WorkDir        string
	Env            []string
	RecordFunc     func(context.Context, int64, *ExecutionResult) (*ScriptRecording, error)
}

func (w *LocalWeaveWorker) ExecuteTask(ctx context.Context, task WeaveReceivingSeatTask) (*ExecutionResult, error) {
	if w == nil || w.RecordFunc == nil || task.RunID <= 0 {
		return nil, errorf(ExitInvalid, "local worker requires a durable recorder and positive run ID before execution")
	}
	if task.Help != "script" || !task.NoInput {
		return nil, errorf(ExitInvalid, "local worker requires help:script and no_input:true")
	}
	runner := &LocalScriptRunner{AllowedCaps: w.AllowedCaps, DeniedCaps: w.DeniedCaps, AllowedEffects: w.AllowedEffects, WorkDir: w.WorkDir, Env: w.Env}
	res, err := runner.ExecuteTask(ctx, task)
	if err != nil {
		return res, err
	}
	// Nonzero exits and cancellation are outcomes too, and must be recorded.
	return res, w.RecordResult(ctx, task.RunID, res)
}

// RecordResult fails closed without a recorder, on storage failure, or without
// a valid durable receipt. It preserves the process outcome on recording errors.
func (w *LocalWeaveWorker) RecordResult(ctx context.Context, runID int64, res *ExecutionResult) error {
	if res == nil {
		return errorf(ExitInvalid, "cannot record a nil execution result")
	}
	res.Recording = nil
	if w == nil || w.RecordFunc == nil {
		return errorf(ExitInvalid, "durable result recorder is unavailable")
	}
	if runID <= 0 {
		return errorf(ExitInvalid, "cannot record an invalid run ID")
	}
	receipt, err := w.RecordFunc(ctx, runID, res)
	if err != nil {
		return err
	}
	if receipt == nil || receipt.RunID != runID || receipt.Ref == "" {
		return errorf(ExitInvalid, "recorder did not return a durable receipt for run %d", runID)
	}
	res.Recording = receipt
	return nil
}
