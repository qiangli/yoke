// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

// Package gate owns the production activation gate: the evidence-bound
// check every activation and restore passes before anything is recorded
// or switched. It lives here, below pkg/src and pkg/webconsole, so both
// the src CLI wiring and the web console use the same constructor without
// an import cycle: sealed passing evidence bound to exactly this
// candidate, this base, this principal and the running verifier build,
// verified against the pair the caller names. Anything else refuses with
// the evidence reason.
package gate

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/qiangli/yoke/pkg/shape/evidence"
	"github.com/qiangli/yoke/pkg/shape/state"
	"github.com/qiangli/yoke/pkg/shape/verify"
)

// home resolves the bashy home the way the CLI does: $BASHY_HOME, else
// ~/.bashy. The gate resolves it per check, so one process-wide gate
// serves whatever home the process runs with.
func home(dir string) string {
	if h := strings.TrimSpace(dir); h != "" {
		return h
	}
	if h := strings.TrimSpace(os.Getenv("BASHY_HOME")); h != "" {
		return h
	}
	h, err := os.UserHomeDir()
	if err != nil || strings.TrimSpace(h) == "" {
		return ".bashy"
	}
	return filepath.Join(h, ".bashy")
}

func shortSHA(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

// Require returns the sealed passing record authorizing candidate for
// principal against base, or the reason it does not. Only fields the
// caller names are checked; the verifier build, candidate, base and
// principal are always named, so a record minted for any other input is
// stale, a failing verdict fails, and a missing or forged record refuses.
func Require(dir, candidate, base, principal string) (evidence.Record, error) {
	if strings.TrimSpace(principal) == "" {
		return evidence.Record{}, errors.New("gate: no principal (a verdict is always for someone)")
	}
	return evidence.Require(home(dir), evidence.Binding{
		Candidate: candidate,
		Base:      base,
		Verifier:  verify.BuildID(),
		Principal: principal,
	})
}

// Check reports whether candidate may be activated for principal against
// base: it requires the sealed passing record for exactly that binding.
// Errors wrap the evidence sentinels (ErrMissing, ErrForged, ErrStale,
// ErrFailed) so a caller can tell "verify again" from "fix the
// candidate" from "something minted a verdict it had not earned".
func Check(dir, candidate, base, principal string) error {
	if _, err := Require(dir, candidate, base, principal); err != nil {
		return fmt.Errorf("gate: refusing to activate %s: %w", shortSHA(candidate), err)
	}
	return nil
}

// New returns the production ActivationGate over dir: the sealed-evidence
// check above, reporting the verifier's verdict with evidenceID as the
// evidence reference. An empty dir resolves per check; an empty
// evidenceID resolves to the sealed record's own path for that
// candidate, which is what the activation commit records.
func New(dir, evidenceID string) state.ActivationGate {
	return state.GateFunc(func(candidate, base, principal string) (state.EvidenceRef, error) {
		h := home(dir)
		if err := Check(h, candidate, base, principal); err != nil {
			return state.EvidenceRef{}, err
		}
		id := evidenceID
		if id == "" {
			id = evidence.Path(h, candidate)
		}
		return state.EvidenceRef{Verdict: "pass", EvidenceID: id}, nil
	})
}
