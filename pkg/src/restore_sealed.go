package src

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	shapegate "github.com/qiangli/yoke/pkg/shape/gate"
	"github.com/qiangli/yoke/pkg/shape/state"
	"github.com/qiangli/yoke/pkg/shape/verify"
)

// ErrUnknownVersion refuses a restore naming anything outside the
// principal's own version line: another principal's versions are simply
// unknown, never a cross-principal read. Callers map it to 404.
var ErrUnknownVersion = errors.New("unknown version: restore a version from your own history")

// RestoreSealedOptions tunes RestoreSealed. Runner is the check runner the
// verification seals through; nil takes the production runner, so the CLI
// and the web console judge a restore identically on every OS. Undo records
// the request line "undo"; Request overrides the recorded request line and
// empty derives "restore to <name or short sha>" from the rev argument.
// When overrides the commit time; zero means the server clock now (UTC).
type RestoreSealedOptions struct {
	Runner  verify.Runner
	Request string
	Undo    bool
	When    time.Time
}

// resolveHistoryTarget maps anything the principal may name — a version-line
// activation commit, the version's revision, one of their own named or
// review labels, or the base release — to the content revision. Anything
// else, including another principal's versions, is ErrUnknownVersion.
func resolveHistoryTarget(h VersionHistory, dir, rev string) (string, error) {
	rev = strings.TrimSpace(rev)
	if rev == "" {
		return "", ErrUnknownVersion
	}
	for _, e := range h.Entries {
		if rev == e.Activation || rev == e.Revision {
			return e.Revision, nil
		}
	}
	for _, n := range h.Named {
		if rev == n.Name {
			return n.Revision, nil
		}
	}
	for _, r := range h.Reviews {
		if rev == r.ID {
			return r.Revision, nil
		}
	}
	if rev == "base" || rev == pristineBranch {
		if h.Base != "" {
			return h.Base, nil
		}
		return "", ErrUnknownVersion
	}
	// The base release travels as a raw SHA in the history view's oldest
	// row, so accept that SHA exactly like the "base" label: it names the
	// shared release every principal started from, never another
	// principal's version.
	if strings.EqualFold(rev, h.Base) && h.Base != "" {
		return h.Base, nil
	}
	return "", ErrUnknownVersion
}

// prepareRestoreWorkspace clones the app checkout into a throwaway
// workspace with the restore pair on branches of its own: the verifier
// clones this workspace whole, so both the candidate and the base must be
// reachable there. The canonical checkout is never touched.
func prepareRestoreWorkspace(ctx context.Context, srcDir, target, base string) (string, error) {
	work, err := os.MkdirTemp("", "src-restore-*")
	if err != nil {
		return "", err
	}
	dir := filepath.Join(work, "work")
	r, err := gogit.PlainInit(dir, false)
	if err != nil {
		_ = os.RemoveAll(work)
		return "", err
	}
	if _, err := r.CreateRemote(&config.RemoteConfig{Name: "origin", URLs: []string{srcDir}}); err != nil {
		_ = os.RemoveAll(work)
		return "", err
	}
	err = r.FetchContext(ctx, &gogit.FetchOptions{RemoteName: "origin", RefSpecs: []config.RefSpec{
		"+refs/heads/*:refs/remotes/origin/*", "+refs/shaping/*:refs/shaping/*",
		"+refs/tags/*:refs/tags/*",
	}})
	if err != nil && err != gogit.NoErrAlreadyUpToDate {
		_ = os.RemoveAll(work)
		return "", fmt.Errorf("src: prepare restore workspace: %w", err)
	}
	if err := r.Storer.SetReference(plumbing.NewHashReference("refs/heads/restore-base", plumbing.NewHash(base))); err != nil {
		_ = os.RemoveAll(work)
		return "", fmt.Errorf("src: prepare restore workspace: %w", err)
	}
	if err := r.Storer.SetReference(plumbing.NewHashReference("refs/heads/restore-candidate", plumbing.NewHash(target))); err != nil {
		_ = os.RemoveAll(work)
		return "", fmt.Errorf("src: prepare restore workspace: %w", err)
	}
	wt, err := r.Worktree()
	if err != nil {
		_ = os.RemoveAll(work)
		return "", fmt.Errorf("src: prepare restore workspace: %w", err)
	}
	if err := wt.Checkout(&gogit.CheckoutOptions{Branch: "refs/heads/restore-candidate"}); err != nil {
		_ = os.RemoveAll(work)
		return "", fmt.Errorf("src: prepare restore workspace: %w", err)
	}
	return dir, nil
}

// RestoreSealed returns the principal's active revision to rev the way the
// web history route does: resolve rev against the principal's own version
// line, refuse a target that cannot read the current data schema, verify
// the target against the current line tip through the trusted verifier —
// sealing fresh evidence bound to exactly that (candidate, base,
// principal, verifier) binding — and only then commit forward through the
// production gate. Every refusal leaves the active revision unchanged.
// Schema-incompatible, verifier-failing and gate-failing restores refuse
// with the reason, identically for the web route and the src CLI on every
// OS.
func RestoreSealed(ctx context.Context, home, app, rev, principal string, opts RestoreSealedOptions) (ActivateResult, error) {
	var out ActivateResult
	principal = strings.TrimSpace(principal)
	if principal == "" {
		return out, fmt.Errorf("src: restore needs a principal")
	}
	if principalSlug(principal) == "" {
		return out, fmt.Errorf("src: principal %q has no usable identity", principal)
	}
	if home == "" {
		home = Home()
	}
	dir, err := repoDir(home, app)
	if err != nil {
		return out, err
	}
	h, err := History(home, app, principal)
	if err != nil {
		return out, err
	}
	targetSHA, err := resolveHistoryTarget(h, dir, rev)
	if err != nil {
		return out, err
	}
	if h.Tip == "" {
		return out, errors.New("src: restore refused: no versions are recorded on your line yet")
	}
	store, err := state.OpenAt(home, app, principal)
	if err != nil {
		return out, err
	}
	st, err := store.Read()
	if err != nil {
		return out, err
	}
	if err := checkReadable(dir, targetSHA, st.DataSchema); err != nil {
		return out, err
	}
	work, err := prepareRestoreWorkspace(ctx, dir, targetSHA, h.Tip)
	if err != nil {
		return out, err
	}
	defer os.RemoveAll(filepath.Dir(work))
	report, err := verify.Run(ctx, verify.Options{
		Home: home, RepoDir: work, BaseSHA: h.Tip,
		CandidateSHA: targetSHA, Principal: principal, Runner: opts.Runner,
	})
	if err != nil {
		return out, err
	}
	if !report.Passed() {
		reason := strings.TrimSpace(report.Record.Reason)
		if reason == "" {
			reason = "the verifier did not pass this version"
		}
		return out, fmt.Errorf("src: restore refused: %s", reason)
	}
	request := oneLineRequest(opts.Request)
	if opts.Undo || request == "undo" {
		request = "undo"
	}
	if request == "" {
		request = "restore to " + restoreDisplay(rev, targetSHA)
	}
	return Restore(home, app, targetSHA, principal, RestoreOptions{
		Gate: shapegate.New(home, report.Path), Request: request, When: opts.When,
	})
}

// UndoSealed returns the principal to the previous active revision through
// RestoreSealed: the undo of a restore is a restore, verified, sealed and
// gated like one. With nothing to undo it refuses and the active revision
// stays where it is.
func UndoSealed(ctx context.Context, home, app, principal string, opts RestoreSealedOptions) (ActivateResult, error) {
	var out ActivateResult
	principal = strings.TrimSpace(principal)
	if principal == "" {
		return out, fmt.Errorf("src: undo needs a principal")
	}
	if home == "" {
		home = Home()
	}
	store, err := state.OpenAt(home, app, principal)
	if err != nil {
		return out, err
	}
	st, err := store.Read()
	if err != nil {
		return out, err
	}
	if strings.TrimSpace(st.LastKnownGood) == "" {
		return out, fmt.Errorf("src: nothing to undo: no previous version is recorded")
	}
	opts.Undo = true
	return RestoreSealed(ctx, home, app, st.LastKnownGood, principal, opts)
}
