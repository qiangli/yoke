// Package meshagent resolves and execs the outpost mesh agent WITHOUT linking it,
// so bashy stays the standalone userland keystone. It is the shared plumbing
// behind the front-door verbs that drive the mesh — `bashy sphere` (tier 4) and
// `bashy tessaro`/`bashy login` (account) — each of which prints its own
// context-specific guidance when the agent is absent.
//
// The mesh/pairing data plane is owned by outpost (github.com/qiangli/outpost);
// this package only finds its binary and passes commands through, the same
// exec-never-link discipline as `bashy podman`/`kubectl`.
package meshagent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// ErrNotFound means the outpost mesh agent binary could not be located — the
// caller should print its own invite/guidance.
var ErrNotFound = errors.New("meshagent: outpost mesh agent not found")

// Resolve finds the outpost binary: $OUTPOST_BIN, then $PATH, then the usual
// install spots. Returns ("", false) when none is usable.
func Resolve() (string, bool) {
	if p := strings.TrimSpace(os.Getenv("OUTPOST_BIN")); p != "" {
		return p, isExec(p)
	}
	if p, err := exec.LookPath("outpost"); err == nil && p != "" {
		return p, true
	}
	if home, err := os.UserHomeDir(); err == nil {
		for _, rel := range []string{"bin/outpost", ".local/bin/outpost"} {
			if cand := filepath.Join(home, rel); isExec(cand) {
				return cand, true
			}
		}
	}
	return "", false
}

// Installed reports whether the mesh agent is resolvable.
func Installed() bool {
	_, ok := Resolve()
	return ok
}

// Exec runs `outpost <args…>` with inherited stdio. Returns ErrNotFound when the
// agent is absent (so the caller can print its own guidance), or a clear error if
// $OUTPOST_BIN is set but not executable.
func Exec(ctx context.Context, args ...string) error {
	bin, ok := Resolve()
	if !ok {
		if p := strings.TrimSpace(os.Getenv("OUTPOST_BIN")); p != "" {
			return fmt.Errorf("meshagent: $OUTPOST_BIN=%q is not an executable", p)
		}
		return ErrNotFound
	}
	c := exec.CommandContext(ctx, bin, args...)
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
	return c.Run()
}

func isExec(p string) bool {
	fi, err := os.Stat(p)
	if err != nil || fi.IsDir() {
		return false
	}
	return fi.Mode()&0o111 != 0
}
