package src

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/qiangli/yoke/pkg/shape/evidence"
	shapegate "github.com/qiangli/yoke/pkg/shape/gate"
	"github.com/qiangli/yoke/pkg/shape/verify"
)

// Principal resolves who a gate decision is for. It is a package variable
// so the launcher's own principal resolution can supply it; the default
// reads $BASHY_PRINCIPAL and refuses to guess. An unresolvable principal
// fails closed: a verdict earned for someone else authorizes nobody.
var Principal = func() string { return strings.TrimSpace(os.Getenv("BASHY_PRINCIPAL")) }

// Gate reports whether candidate may be activated for principal against
// base: it requires a sealed, passing evidence record bound to exactly
// this candidate, this base, this principal and the running verifier
// build. Activation calls it; the merge gate goes through the same
// evidence with the contract and dependency digests recomputed as well.
//
// Errors wrap the evidence sentinels (evidence.ErrMissing, ErrForged,
// ErrStale, ErrFailed) so a caller can tell "verify again" from "fix the
// candidate" from "something minted a verdict it had not earned".
func Gate(home, candidate, base, principal string) error {
	if home == "" {
		home = Home()
	}
	if strings.TrimSpace(principal) == "" {
		return fmt.Errorf("src: refusing to activate %s: no principal (a verdict is always for someone)",
			shortSHA(candidate))
	}
	// The check itself lives in pkg/shape/gate, shared with the web
	// console; this wrapper keeps the src refusal wording the CLI and
	// the production activator report.
	if _, err := shapegate.Require(home, candidate, base, principal); err != nil {
		return fmt.Errorf("src: refusing to activate %s: %w", shortSHA(candidate), err)
	}
	return nil
}

// GateBinding is Gate with every part of the binding supplied, including
// the contract and dependency digests the caller recomputed itself. Use it
// wherever those digests can be derived independently; a record is only
// as good as the inputs the reader checked it against.
func GateBinding(home string, want evidence.Binding) error {
	if home == "" {
		home = Home()
	}
	if strings.TrimSpace(want.Principal) == "" {
		return fmt.Errorf("src: refusing %s: no principal (a verdict is always for someone)",
			shortSHA(want.Candidate))
	}
	_, err := evidence.Require(home, want)
	return err
}

// requireEvidence is the merge gate's evidence check: recompute the whole
// binding from the repository, then demand a sealed passing record for it.
// Every refusal names the approval override, because a blocked merge the
// operator cannot consciously unblock is a trap rather than a gate.
func requireEvidence(home, dir string, pr PR, baseSHA string) error {
	if home == "" {
		home = Home()
	}
	principal := Principal()
	if principal == "" {
		return fmt.Errorf("src: refusing PR %s: no principal is resolved, so no verification evidence applies "+
			"(set the principal, or record an approval covering the whole change)", pr.ID)
	}
	want, err := verify.Binding(dir, pr.HeadSHA, baseSHA, principal, nil)
	if err != nil {
		return fmt.Errorf("src: PR %s: %w", pr.ID, err)
	}
	if _, err := evidence.Require(home, want); err != nil {
		return fmt.Errorf("src: refusing PR %s at %s: %w "+
			"(verify the candidate, or record an approval covering the whole change)",
			pr.ID, shortSHA(pr.HeadSHA), err)
	}
	return nil
}

// approvalsCoverAll reports whether the stored approvals for repo cover
// every path in the change. This is the one override for the evidence
// requirement, and it is deliberately the existing approval flow: an
// operator who has reviewed the whole change may merge it unverified, and
// there is a record saying they did. Nothing waives verification silently.
func approvalsCoverAll(home, repo string, paths []string) (bool, error) {
	if len(paths) == 0 {
		return false, nil
	}
	recs, err := ListApprovals(home, repo)
	if err != nil {
		return false, err
	}
	for _, p := range paths {
		ok := false
		for _, r := range recs {
			for _, ap := range r.Paths {
				if approvalCovers(ap, p) {
					ok = true
					break
				}
			}
			if ok {
				break
			}
		}
		if !ok {
			return false, nil
		}
	}
	return true, nil
}

// EvidenceStale reports whether err is a gate refusal the operator fixes by
// verifying again rather than by changing the candidate.
func EvidenceStale(err error) bool {
	return errors.Is(err, evidence.ErrMissing) || errors.Is(err, evidence.ErrStale)
}

// VerifyPR runs the trusted verifier over an open PR: the candidate is the
// recorded head SHA, the base is its merge base with `local` — the version
// the principal is running — and the verdict is sealed for the resolved
// principal. It returns the record so a caller can report it, and a
// failing verdict is a result, not an error: the record is what the gate
// will read either way.
//
// runner is the host's check runner; nil takes the default, which hands
// each frozen check to the installed trusted interpreter inside the
// isolated clone.
func VerifyPR(ctx context.Context, home, id string, runner verify.Runner) (evidence.Record, error) {
	if home == "" {
		home = Home()
	}
	pr, err := GetPR(home, id)
	if err != nil {
		return evidence.Record{}, err
	}
	if pr.State != PRStateOpen {
		return evidence.Record{}, fmt.Errorf("src: PR %s is %s, not open", id, pr.State)
	}
	principal := Principal()
	if principal == "" {
		return evidence.Record{}, fmt.Errorf("src: refusing to verify PR %s: no principal is resolved "+
			"(a verdict is always for someone)", id)
	}
	dir, err := repoDir(home, pr.Repo)
	if err != nil {
		return evidence.Record{}, err
	}
	baseSHA, err := mergeBase(dir, localBranch, pr.HeadSHA)
	if err != nil {
		return evidence.Record{}, fmt.Errorf("src: PR %s head %s: %w", id, shortSHA(pr.HeadSHA), err)
	}
	rep, err := verify.Run(ctx, verify.Options{
		Home: home, RepoDir: dir, BaseSHA: baseSHA, CandidateSHA: pr.HeadSHA,
		Principal: principal, Runner: runner,
	})
	if err != nil {
		return evidence.Record{}, fmt.Errorf("src: verify PR %s: %w", id, err)
	}
	return rep.Record, nil
}
