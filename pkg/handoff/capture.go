// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package handoff

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// maxUntrackedBytes caps a single carried file. A handoff record is meant to
// travel (over a mesh, in an issue comment, through a scheduler payload), so it
// must not silently swallow a 2 GB core dump an agent left lying around. Files
// over the cap are NAMED but not carried — a successor is told what it is
// missing, which is strictly better than a truncated file that looks complete.
const maxUntrackedBytes = 1 << 20 // 1 MiB

// CaptureWork records the in-flight working tree of one repo as a self-contained
// bundle: the diff against HEAD (staged and unstaged together), plus untracked
// files carried BY CONTENT.
//
// This is the piece nothing else had. `sprint handoff`, `weave baton write` and
// the cloudbox session lease all record PROSE — so a successor inherited a
// narrative, not a working tree. In this project that is not hypothetical: one
// session found an unexplained edit in the tree and had to guess whose it was,
// and another swept a third session's staged submodule pins into a commit,
// landing an untested engine regression that took the release gate from 86/86 to
// 85/86.
//
// Three deliberate choices, each of which is a lesson from that failure:
//
//  1. Staged and unstaged are captured TOGETHER (`git diff HEAD`). The index is
//     not preserved, on purpose. The index is precisely the shared mutable state
//     that let one session commit another's staged work. A handoff should carry
//     what was CHANGED, not what someone had half-decided to commit.
//
//  2. Untracked files are carried by CONTENT, not by path. A patch does not
//     contain them, and they are routinely the entire point — the new file the
//     agent just wrote is the work.
//
//  3. Nothing is destroyed. Capture is a READ. It does not stash, does not
//     reset, does not clean. An agent being killed mid-edit must not have its
//     work moved out from under it by the very command meant to preserve it.
func CaptureWork(repo string) (WorkingState, error) {
	ws := WorkingState{Repo: repo}

	branch, err := gitOut(repo, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return ws, fmt.Errorf("not a git repo (or no commits): %s: %w", repo, err)
	}
	ws.Branch = branch
	if sha, err := gitOut(repo, "rev-parse", "HEAD"); err == nil {
		ws.BaseSHA = sha
	}

	// Staged + unstaged, as one patch against the commit a successor can find.
	//
	// gitOutRaw, NOT gitOut: a git patch MUST end with a newline, and gitOut
	// trims trailing newlines. Stripping it produces a patch that git rejects as
	// corrupt — a one-byte bug that would have made every non-empty handoff fail
	// to apply, while looking perfectly fine in the record.
	diff, err := gitOutRaw(repo, "diff", "HEAD")
	if err != nil {
		return ws, fmt.Errorf("git diff HEAD: %w", err)
	}
	ws.Diff = diff

	// Untracked, excluding anything gitignored: an ignored file is ignored for a
	// reason (build output, caches, a 259 MB binary), and carrying it would make
	// the record unusable as a travelling artifact.
	out, err := gitOut(repo, "ls-files", "--others", "--exclude-standard")
	if err != nil {
		return ws, fmt.Errorf("git ls-files --others: %w", err)
	}
	for _, rel := range strings.Split(out, "\n") {
		rel = strings.TrimSpace(rel)
		if rel == "" {
			continue
		}
		abs := filepath.Join(repo, rel)
		fi, err := os.Lstat(abs)
		if err != nil || !fi.Mode().IsRegular() {
			continue
		}
		if fi.Size() > maxUntrackedBytes {
			// Name it, do not carry it. A successor told "this file exists and is
			// too big to travel" can go get it; a successor handed a truncated
			// file cannot tell that anything is wrong.
			ws.Untracked = append(ws.Untracked, UntrackedFile{
				Path: rel,
				Content: fmt.Sprintf("<<< NOT CARRIED: %d bytes exceeds the %d-byte handoff cap. "+
					"Fetch it from the origin host (%s). >>>", fi.Size(), maxUntrackedBytes, repo),
				Mode: uint32(fi.Mode().Perm()),
			})
			continue
		}
		b, err := os.ReadFile(abs)
		if err != nil {
			continue
		}
		ws.Untracked = append(ws.Untracked, UntrackedFile{
			Path: rel, Content: string(b), Mode: uint32(fi.Mode().Perm()),
		})
	}

	ws.Clean = ws.Diff == "" && len(ws.Untracked) == 0
	return ws, nil
}

// Apply reconstitutes a captured working tree into a target repo. It is the
// mirror of CaptureWork and the half that makes the record portable in practice
// rather than in principle.
//
// It REFUSES to apply onto a dirty tree. A handoff lands in a fresh weave
// workspace (or a clean checkout); applying a patch on top of someone else's
// uncommitted edits is how you manufacture a conflict that neither agent
// understands and neither can attribute.
func Apply(ws WorkingState, target string) error {
	if ws.Clean {
		return nil
	}
	dirty, err := gitOut(target, "status", "--porcelain")
	if err != nil {
		return fmt.Errorf("git status: %w", err)
	}
	if strings.TrimSpace(dirty) != "" {
		return fmt.Errorf("refusing to apply a handoff onto a dirty tree (%s): "+
			"commit, stash, or use a fresh workspace first", target)
	}

	// Warn loudly on a base mismatch rather than applying to the wrong commit and
	// producing a plausible-looking mess.
	if ws.BaseSHA != "" {
		if head, err := gitOut(target, "rev-parse", "HEAD"); err == nil && head != ws.BaseSHA {
			fmt.Fprintf(os.Stderr,
				"handoff: WARNING base mismatch — captured at %s, applying onto %s. "+
					"The patch may not apply cleanly; that is a real signal, not a glitch.\n",
				short(ws.BaseSHA), short(head))
		}
	}

	if strings.TrimSpace(ws.Diff) != "" {
		if err := applyPatch(target, ws.Diff); err != nil {
			return err
		}
	}

	for _, f := range ws.Untracked {
		if strings.HasPrefix(f.Content, "<<< NOT CARRIED:") {
			fmt.Fprintf(os.Stderr, "handoff: %s was NOT carried (too large); fetch it from the origin host\n", f.Path)
			continue
		}
		abs := filepath.Join(target, f.Path)
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			return err
		}
		mode := os.FileMode(f.Mode)
		if mode == 0 {
			mode = 0o644
		}
		if err := os.WriteFile(abs, []byte(f.Content), mode); err != nil {
			return err
		}
	}
	return nil
}

// applyPatch lands the captured diff, trying the strongest strategy first and
// degrading to the most portable one.
//
// This ordering is load-bearing, and getting it wrong silently breaks the ONLY
// case this feature exists for. `git apply --3way` is the better strategy — it
// can reconstruct and merge when context has drifted — but it needs the blob
// SHAs named in the patch's `index` lines to be present in the TARGET's object
// database. That holds when the successor is a clone of the same repo (a weave
// workspace, a fetched branch), and it fails outright — exit 128 — when the
// successor is a genuinely foreign repo: another machine, a fresh checkout, a
// tree reconstructed from a bundle.
//
// A foreign repo is not an exotic case here. It is the headline: "resume this
// session in a different tool on a different machine". So --3way is an
// optimisation, and PLAIN apply is the guarantee. Try the optimisation, keep the
// guarantee.
func applyPatch(target, diff string) error {
	try := func(args ...string) error {
		cmd := exec.Command("git", append(gitArgs(target, "apply"), args...)...)
		cmd.Stdin = strings.NewReader(diff)
		var stderr strings.Builder
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
		}
		return nil
	}

	// Best effort: 3-way can merge around drift when the objects are reachable.
	if err := try("--3way", "-"); err == nil {
		return nil
	}
	// The portable path: a plain context patch needs nothing but the working tree.
	if err := try("-"); err == nil {
		return nil
	}
	// Last resort before giving up: tolerate whitespace noise, which is the most
	// common cause of a patch failing on an otherwise identical tree.
	if err := try("--whitespace=nowarn", "-"); err == nil {
		return nil
	}
	return fmt.Errorf("the captured diff did not apply to %s.\n"+
		"The base has genuinely moved — that is a real signal, not a glitch. "+
		"Reconcile by hand rather than guessing: the handoff record still holds the "+
		"full diff and the base SHA it was taken at", target)
}

func gitOut(repo string, args ...string) (string, error) {
	out, err := gitOutRaw(repo, args...)
	if err != nil {
		return "", err
	}
	return strings.TrimRight(out, "\n"), nil
}

// gitOutRaw preserves output byte-for-byte. Use it for anything whose trailing
// newline is semantically load-bearing — a patch, above all.
func gitOutRaw(repo string, args ...string) (string, error) {
	out, err := exec.Command("git", gitArgs(repo, args...)...).Output()
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// gitArgs builds the argv for a handoff git call, pinning the line-ending
// config OFF. Handoff's contract is a byte-exact working tree: it must carry
// the literal LF/CRLF bytes an agent left, not what a host's git happens to
// normalize them to. A Windows git defaults to core.autocrlf=true, which would
// rewrite LF→CRLF on `git apply` (and skew the captured diff) — silently
// corrupting the reconstruction the whole feature exists to guarantee. Forcing
// autocrlf=false + eol=lf on handoff's own invocations makes capture/apply
// deterministic on every platform, independent of the host's git config.
func gitArgs(repo string, args ...string) []string {
	base := []string{"-C", repo, "-c", "core.autocrlf=false", "-c", "core.eol=lf"}
	return append(base, args...)
}

func short(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}
