// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package dag

import (
	"context"
	"io"
	"sync"
	"time"

	"github.com/qiangli/coreutils/pkg/weavecli"
)

// TaskIO is the execution environment handed to an interpreter for one target.
type TaskIO struct {
	Dir    string
	Env    []string // os.Environ() shape
	Stdout io.Writer
	Stderr io.Writer
}

// TaskResult is the outcome of running one target. Stdout/Stderr are populated
// only when the engine runs in capture mode (e.g. --json), so a machine reader
// gets a single clean envelope on stdout instead of interleaved task output.
type TaskResult struct {
	Name        string
	Host        string
	Status      Status
	ExitCode    int
	Duration    time.Duration
	Err         error
	Stdout      string
	Stderr      string
	UpToDate    bool         // P1.5 fingerprint skip
	Attestation *Attestation // P2 contract verdict (nil when no Ensure/Effects)
	Artifacts   []string     // P1 #8 declared outputs that exist after success
}

// Interpreter runs a target's body. Implementations register themselves by
// language tag via RegisterInterpreter; the bash interpreter is the default.
type Interpreter interface {
	Run(ctx context.Context, t *Task, io TaskIO) TaskResult
}

// Executor is the placement seam above interpreters. The default executor runs
// locally through the registered in-process interpreter; a future outpost-mesh
// dispatcher can plug in here and honor Task.Host.
type Executor interface {
	Execute(ctx context.Context, t *Task, io TaskIO) TaskResult
}

var (
	interpMu sync.RWMutex
	interps  = map[string]Interpreter{}
)

// RegisterInterpreter associates an interpreter with a fenced-code lang tag.
// "" registers the default (used when a body has no info string). Shipped tags:
// ""/bash/sh/shell (Classic) and bsh/bashsharp — aliases bashpp/bash++ —
// (Bash#, which is also how a body reaches Python or TypeScript — as a `~~~py`/`~~~typescript` fence
// declared inside the Bash# body, not as a top-level body language).
func RegisterInterpreter(lang string, i Interpreter) {
	interpMu.Lock()
	defer interpMu.Unlock()
	interps[lang] = i
}

func interpFor(lang string) (Interpreter, error) {
	interpMu.RLock()
	defer interpMu.RUnlock()
	if i, ok := interps[lang]; ok {
		return i, nil
	}
	return nil, errf(weavecli.ExitInvalidArg, "no interpreter for language %q", lang)
}
