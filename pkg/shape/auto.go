// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

package shape

import (
	"errors"
	"fmt"
	"log/slog"

	"github.com/qiangli/yoke/pkg/shape/state"
)

// ErrAutoOff refuses an unattended wish while the host's off switch is set.
var ErrAutoOff = errors.New("shape: unattended shaping is off on this host (bashy shape auto on)")

// Unattended reports whether the wish came from a source that shapes with
// no person asking. Incident repair keeps its own guard in the console.
func (s Source) Unattended() bool { return s.Kind == "feed" || s.Kind == "schedule" }

// logAuto appends one line to the host's decision log for an unattended
// source; human and empty sources log nothing. The log is a record, not a
// gate: a write error only warns.
func (s Service) logAuto(src Source, event, result, detail string) {
	if !src.Unattended() {
		return
	}
	home, err := s.home()
	if err == nil {
		err = state.LogAuto(home, state.AutoDecision{Event: event, App: s.App.Name, Source: src.Kind, Ref: src.Ref, Result: result, Detail: detail})
	}
	if err != nil {
		slog.Warn("shape: decision log write failed", "app", s.App.Name, "err", err)
	}
}

// admitAuto refuses, and logs the refusal of, an unattended wish while
// unattended shaping is off on this host.
func (s Service) admitAuto(src Source) error {
	if !src.Unattended() {
		return nil
	}
	if home, err := s.home(); err == nil && !state.AutoEnabled(home) {
		s.logAuto(src, "refusal", "off", "unattended shaping is off on this host")
		return fmt.Errorf("%w: %s wish for %s refused", ErrAutoOff, src.Kind, s.App.Name)
	}
	return nil
}

// logAutoVerdict records the trusted verifier's verdict on c.
func (s Service) logAutoVerdict(c Candidate) {
	if c.Verdict == nil {
		return
	}
	result := "fail"
	if c.Verdict.Pass {
		result = "pass"
	}
	s.logAuto(c.Source, "verdict", result, joinDetail(c.ID, c.Verdict.Detail))
}

func joinDetail(id, detail string) string {
	if detail == "" {
		return id
	}
	return id + " " + detail
}
