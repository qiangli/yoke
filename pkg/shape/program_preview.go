package shape

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/qiangli/yoke/pkg/shape/contain"
	"github.com/qiangli/yoke/pkg/shape/slot"
	shapeverify "github.com/qiangli/yoke/pkg/shape/verify"
)

const shapePreviewOutputLimit = 1 << 20

type ProgramPreviewResult struct {
	ProgramPreview
	Args      []string `json:"args"`
	ExitCode  int      `json:"exit_code"`
	Stdout    string   `json:"stdout"`
	Stderr    string   `json:"stderr"`
	Truncated bool     `json:"truncated,omitempty"`
	Limited   string   `json:"limited,omitempty"`
}
type shapePreviewBuffer struct {
	buf       bytes.Buffer
	truncated bool
}

func (b *shapePreviewBuffer) String() string { return b.buf.String() }

func (b *shapePreviewBuffer) Write(p []byte) (int, error) {
	n := len(p)
	remaining := shapePreviewOutputLimit - b.buf.Len()
	if remaining < len(p) {
		b.truncated = true
		if remaining > 0 {
			_, _ = b.buf.Write(p[:remaining])
		}
	} else {
		_, _ = b.buf.Write(p)
	}
	return n, nil
}

// runShapeCLIPreview runs only the verified artifact, with private cwd/home,
// scrubbed environment, process/resource/time bounds and capped output capture.
func runShapeCLIPreview(ctx context.Context, p ProgramPreview, args []string) (ProgramPreviewResult, error) {
	out := ProgramPreviewResult{ProgramPreview: p, Args: append([]string(nil), args...), ExitCode: -1}
	if len(out.Args) == 0 {
		out.Args = []string{"--help"}
	}
	digest, e := shapeverify.ProgramArtifactDigest("cli", p.Path)
	if e != nil {
		return out, e
	}
	if digest != p.Artifact {
		return out, errors.New("shape: preview artifact digest changed; verify again")
	}
	dir, e := os.MkdirTemp("", "bashy-shape-preview-*")
	if e != nil {
		return out, e
	}
	defer os.RemoveAll(dir)
	dir, e = filepath.EvalSymlinks(dir)
	if e != nil {
		return out, e
	}
	selected, e := filepath.Abs(p.Path)
	if e != nil {
		return out, e
	}
	stdoutR, stdoutW, e := os.Pipe()
	if e != nil {
		return out, e
	}
	defer stdoutR.Close()
	defer stdoutW.Close()
	stderrR, stderrW, e := os.Pipe()
	if e != nil {
		return out, e
	}
	defer stderrR.Close()
	defer stderrW.Close()
	var stdout, stderr shapePreviewBuffer
	var readers sync.WaitGroup
	readers.Add(2)
	go func() { defer readers.Done(); _, _ = io.Copy(&stdout, stdoutR) }()
	go func() { defer readers.Done(); _, _ = io.Copy(&stderr, stderrR) }()
	result, runErr := contain.Run(ctx, contain.Spec{Dir: dir, Argv: append([]string{selected}, out.Args...), Cap: "read,write,exec,net", Effects: []string{"read", "write", "exec", "net"}, EnvAllow: []string{"PATH"}, EnvSet: map[string]string{"BASHY_HOME": filepath.Join(dir, ".bashy"), slot.EntryEnvironmentKey: selected}, Timeout: 30 * time.Second, MaxProcs: 64, MaxRSSBytes: 1 << 30, Stdout: stdoutW, Stderr: stderrW})
	_ = stdoutW.Close()
	_ = stderrW.Close()
	done := make(chan struct{})
	go func() { readers.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		_ = stdoutR.Close()
		_ = stderrR.Close()
		<-done
		out.Truncated = true
	}
	out.Stdout, out.Stderr = stdout.String(), stderr.String()
	out.Truncated = out.Truncated || stdout.truncated || stderr.truncated
	out.ExitCode, out.Limited = result.ExitCode, result.Limited
	if runErr != nil && out.ExitCode == 0 {
		out.ExitCode = -1
	}
	if runErr != nil {
		return out, runErr
	}
	if result.ExitCode != 0 {
		return out, fmt.Errorf("shape: preview exited %d", result.ExitCode)
	}
	if result.Limited != "" {
		return out, fmt.Errorf("shape: preview reached %s limit", result.Limited)
	}
	return out, nil
}
