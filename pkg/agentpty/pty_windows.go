//go:build windows

package agentpty

import (
	"fmt"
	"io"
	"os/exec"
)

// Supported reports whether this host can run an agent under a PTY.
//
// Callers branch on this rather than on runtime.GOOS, and they must FALL BACK
// to a plain exec rather than failing. A Windows user loses the trust-prompt
// clearing and the steer channel, which is a degraded run — not a broken one.
// The agent still speaks; nobody can interrupt it.
func Supported() bool { return false }

// Run is Unix-only. Console-host APIs differ enough — and the agentic CLIs this
// drives are macOS/Linux-first — that a Windows PTY is not load-bearing. Callers
// check Supported() and exec normally instead.
func Run(cmd *exec.Cmd, logSink io.Writer, opts Options) (int, string, error) {
	return 127, "", fmt.Errorf("agentpty: PTY is not supported on Windows (run under WSL for trust-clearing and steering)")
}
