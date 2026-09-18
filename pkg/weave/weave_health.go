package weave

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

// weaveHealth is the runtime condition of a queue item, independent of its
// lifecycle state.  A terminal item can be healthy (the wrapper recorded a
// coherent outcome), while a working item can be inconsistent (the durable
// record cannot describe who or what is running it).
type weaveHealth string

const (
	weaveHealthHealthy      weaveHealth = "healthy"
	weaveHealthIdle         weaveHealth = "idle"
	weaveHealthStale        weaveHealth = "stale"
	weaveHealthWedged       weaveHealth = "wedged"
	weaveHealthInconsistent weaveHealth = "inconsistent"
)

// weaveHealthProbe contains the only observations a read needs. Keeping the
// probes here makes classification deterministic and makes it impossible for
// a status read to mutate queue state as a side effect of deciding health.
type weaveHealthProbe struct {
	Now             time.Time
	PIDAlive        func(int) bool
	WorkspaceExists func(string) bool
	LogModifiedAt   func(string) (time.Time, bool)
}

func defaultWeaveHealthProbe(now time.Time) weaveHealthProbe {
	return weaveHealthProbe{
		Now:      now.UTC(),
		PIDAlive: pidAlive,
		WorkspaceExists: func(path string) bool {
			st, err := os.Stat(path)
			return err == nil && st.IsDir()
		},
		LogModifiedAt: func(path string) (time.Time, bool) {
			if path == "" {
				return time.Time{}, false
			}
			st, err := os.Stat(path)
			if err != nil {
				return time.Time{}, false
			}
			return st.ModTime().UTC(), true
		},
	}
}

// weaveHealthSnapshot is a stable, JSON-ready view of the evidence used by
// weaveClassifyHealth. It intentionally contains no derived verdict so the
// same snapshot can be rendered in human and machine output.
type weaveHealthSnapshot struct {
	Issue             int64     `json:"issue"`
	State             string    `json:"state"`
	Owner             string    `json:"owner,omitempty"`
	Tool              string    `json:"tool,omitempty"`
	WrapperPID        int       `json:"wrapper_pid,omitempty"`
	WrapperAlive      bool      `json:"wrapper_alive"`
	StartedAt         time.Time `json:"started_at,omitempty"`
	LastProgressAt    time.Time `json:"last_progress_at,omitempty"`
	DeadlineAt        time.Time `json:"deadline_at,omitempty"`
	Workspace         string    `json:"workspace,omitempty"`
	WorkspaceExists   bool      `json:"workspace_exists"`
	CommitsAhead      int       `json:"commits_ahead"`
	Head              string    `json:"head,omitempty"`
	VerifyConfigured  bool      `json:"verify_configured"`
	VerifyRecorded    bool      `json:"verify_recorded"`
	VerifyPassed      bool      `json:"verify_passed"`
	Dirty             bool      `json:"dirty"`
	FinishedAt        time.Time `json:"finished_at,omitempty"`
	ExitRecorded      bool      `json:"exit_recorded"`
	Completion        string    `json:"completion,omitempty"`
	KilledBy          string    `json:"killed_by,omitempty"`
	MaxRuntimeSeconds int64     `json:"max_runtime_seconds,omitempty"`
}

// time.Time is a struct, so encoding/json's omitempty does not omit a zero
// timestamp. Keep the internal representation convenient for comparisons, but
// use pointers at the wire boundary so a missing observation is absent rather
// than misleadingly rendered as year 1.
func (s weaveHealthSnapshot) MarshalJSON() ([]byte, error) {
	optionalTime := func(t time.Time) *time.Time {
		if t.IsZero() {
			return nil
		}
		t = t.UTC()
		return &t
	}
	type snapshotJSON struct {
		Issue             int64      `json:"issue"`
		State             string     `json:"state"`
		Owner             string     `json:"owner,omitempty"`
		Tool              string     `json:"tool,omitempty"`
		WrapperPID        int        `json:"wrapper_pid,omitempty"`
		WrapperAlive      bool       `json:"wrapper_alive"`
		StartedAt         *time.Time `json:"started_at,omitempty"`
		LastProgressAt    *time.Time `json:"last_progress_at,omitempty"`
		DeadlineAt        *time.Time `json:"deadline_at,omitempty"`
		Workspace         string     `json:"workspace,omitempty"`
		WorkspaceExists   bool       `json:"workspace_exists"`
		CommitsAhead      int        `json:"commits_ahead"`
		Head              string     `json:"head,omitempty"`
		VerifyConfigured  bool       `json:"verify_configured"`
		VerifyRecorded    bool       `json:"verify_recorded"`
		VerifyPassed      bool       `json:"verify_passed"`
		Dirty             bool       `json:"dirty"`
		FinishedAt        *time.Time `json:"finished_at,omitempty"`
		ExitRecorded      bool       `json:"exit_recorded"`
		Completion        string     `json:"completion,omitempty"`
		KilledBy          string     `json:"killed_by,omitempty"`
		MaxRuntimeSeconds int64      `json:"max_runtime_seconds,omitempty"`
	}
	return json.Marshal(snapshotJSON{
		Issue:             s.Issue,
		State:             s.State,
		Owner:             s.Owner,
		Tool:              s.Tool,
		WrapperPID:        s.WrapperPID,
		WrapperAlive:      s.WrapperAlive,
		StartedAt:         optionalTime(s.StartedAt),
		LastProgressAt:    optionalTime(s.LastProgressAt),
		DeadlineAt:        optionalTime(s.DeadlineAt),
		Workspace:         s.Workspace,
		WorkspaceExists:   s.WorkspaceExists,
		CommitsAhead:      s.CommitsAhead,
		Head:              s.Head,
		VerifyConfigured:  s.VerifyConfigured,
		VerifyRecorded:    s.VerifyRecorded,
		VerifyPassed:      s.VerifyPassed,
		Dirty:             s.Dirty,
		FinishedAt:        optionalTime(s.FinishedAt),
		ExitRecorded:      s.ExitRecorded,
		Completion:        s.Completion,
		KilledBy:          s.KilledBy,
		MaxRuntimeSeconds: s.MaxRuntimeSeconds,
	})
}

type weaveHealthReport struct {
	Health   weaveHealth         `json:"health"`
	Reason   string              `json:"health_reason"`
	Next     string              `json:"health_next_action"`
	Snapshot weaveHealthSnapshot `json:"health_snapshot"`
}

// weaveHealthDefaultIdleAfter is deliberately finite even when a launcher did
// not declare --idle-timeout. Without a bound, a live but wedged process would
// remain "healthy" forever merely because its PID can be signalled.
var weaveHealthDefaultIdleAfter = 5 * time.Minute

func weaveHealthSnapshotFor(it *weaveItem, probe weaveHealthProbe) weaveHealthSnapshot {
	if probe.Now.IsZero() {
		probe.Now = time.Now().UTC()
	}
	if probe.PIDAlive == nil {
		probe.PIDAlive = pidAlive
	}
	if probe.WorkspaceExists == nil {
		probe.WorkspaceExists = func(path string) bool {
			st, err := os.Stat(path)
			return err == nil && st.IsDir()
		}
	}
	s := weaveHealthSnapshot{}
	if it == nil {
		return s
	}
	s.Issue, s.State, s.Owner, s.Tool = it.ID, it.State, it.Owner, it.Tool
	s.WrapperPID, s.StartedAt, s.Workspace = it.WrapperPid, it.StartedAt, it.Workspace
	s.CommitsAhead, s.Head, s.Dirty, s.FinishedAt = it.CommitsAhead, it.Head, it.Dirty, it.FinishedAt
	s.ExitRecorded, s.Completion, s.KilledBy = it.ExitCode != nil, it.Completion, it.KilledBy
	s.VerifyConfigured, s.VerifyRecorded = it.VerifyCommand != "", it.VerifyExit != nil
	s.VerifyPassed = it.VerifyExit != nil && *it.VerifyExit == 0
	if s.Workspace != "" {
		s.WorkspaceExists = probe.WorkspaceExists(s.Workspace)
	}
	if s.WrapperPID > 0 {
		s.WrapperAlive = probe.PIDAlive(s.WrapperPID)
	}
	if !it.StartedAt.IsZero() && it.LaunchSpec != nil && it.LaunchSpec.MaxRuntime > 0 {
		s.MaxRuntimeSeconds = int64(it.LaunchSpec.MaxRuntime / time.Second)
		s.DeadlineAt = it.StartedAt.Add(it.LaunchSpec.MaxRuntime)
	}
	for _, c := range it.Comments {
		if c.At.IsZero() || (c.Kind != "progress" && c.Kind != "decision" && c.Kind != "review") {
			continue
		}
		if c.At.After(s.LastProgressAt) {
			s.LastProgressAt = c.At.UTC()
		}
	}
	if probe.LogModifiedAt != nil && it.LogPath != "" {
		if at, ok := probe.LogModifiedAt(it.LogPath); ok && at.After(s.LastProgressAt) {
			s.LastProgressAt = at.UTC()
		}
	}
	return s
}

func weaveHealthIdleAfter(s weaveHealthSnapshot, it *weaveItem) time.Duration {
	if it != nil && it.LaunchSpec != nil && it.LaunchSpec.IdleTimeout > 0 {
		return it.LaunchSpec.IdleTimeout
	}
	if s.MaxRuntimeSeconds > 0 {
		d := time.Duration(s.MaxRuntimeSeconds) * time.Second / 3
		if d > weaveHealthDefaultIdleAfter {
			return weaveHealthDefaultIdleAfter
		}
		if d > 0 {
			return d
		}
	}
	return weaveHealthDefaultIdleAfter
}

func weaveHealthConsistencyIssue(s weaveHealthSnapshot, it *weaveItem) (string, string, bool) {
	active := s.State == "working" || s.State == "finalizing" || s.State == "allocated"
	if s.State == "" {
		return "missing lifecycle state", "inspect queue record; do not reassign", true
	}
	if s.State == "working" || s.State == "finalizing" {
		if s.Owner == "" {
			return "active run has no owner", "repair the assignment before resuming", true
		}
		if it == nil || it.LaunchSpec == nil {
			return "active run has no launch specification", "inspect the queue record before resuming", true
		}
		if s.Tool != "" && it.LaunchSpec.Tool != "" && s.Tool != it.LaunchSpec.Tool {
			return fmt.Sprintf("owner launch tool %q contradicts recorded tool %q", it.LaunchSpec.Tool, s.Tool), "repair the launch record before resuming", true
		}
		if s.WrapperPID <= 0 {
			return "active run has no wrapper pid", "inspect the queue record; do not claim a second worker", true
		}
		if !s.WorkspaceExists {
			return "active run workspace is missing", "start a fresh run only after preserving any reachable branch", true
		}
	}
	if s.State == "allocated" && s.Workspace != "" && !s.WorkspaceExists {
		return "allocated run workspace is missing", "re-provision the run after inspecting its queue record", true
	}
	if active && it != nil && it.LaunchSpec != nil && it.LaunchSpec.MaxRuntime < 0 {
		return "launch specification has a negative max runtime", "repair the launch specification before resuming", true
	}
	if s.State == "submitted" {
		if s.WrapperPID > 0 {
			return "submitted run still claims a live wrapper", "inspect the wrapper before pulling or reassigning", true
		}
		if s.CommitsAhead <= 0 && strings.TrimSpace(s.Head) == "" {
			return "submitted run has no commit evidence", "reverify or inspect the workspace; do not treat it as complete", true
		}
		if s.VerifyConfigured && !s.VerifyRecorded {
			return "submitted run has no recorded test evidence", "run `weave reverify` before pulling", true
		}
	}
	if s.State == "done" && s.CommitsAhead <= 0 && strings.TrimSpace(s.Head) == "" {
		return "done run has no commit evidence", "inspect the base and queue record before declaring success", true
	}
	if s.State == "no-op" && (s.FinishedAt.IsZero() || s.Dirty || s.CommitsAhead != 0) {
		return "no-op terminal evidence is incomplete", "inspect the workspace and terminal record", true
	}
	if (s.State == "failed" || s.State == "killed" || s.State == "abandoned") && s.FinishedAt.IsZero() && !s.ExitRecorded && s.Completion == "" && s.KilledBy == "" {
		return "terminal state has no terminal evidence", "inspect the queue record before resuming or abandoning", true
	}
	return "", "", false
}

// weaveClassifyHealth is pure: it only reads the supplied snapshot and item;
// it never writes queue state, kills a process, or changes ownership.
func weaveClassifyHealth(s weaveHealthSnapshot, it *weaveItem, now time.Time) weaveHealthReport {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	if reason, next, bad := weaveHealthConsistencyIssue(s, it); bad {
		return weaveHealthReport{Health: weaveHealthInconsistent, Reason: reason, Next: next, Snapshot: s}
	}
	terminal := isTerminalState(s.State)
	if terminal {
		if s.State == "failed" || s.State == "killed" || s.State == "abandoned" {
			return weaveHealthReport{Health: weaveHealthHealthy, Reason: "terminal outcome is durably recorded", Next: fmt.Sprintf("inspect or resume with `weave start --resume --issue %d`", s.Issue), Snapshot: s}
		}
		next := "inspect the recorded terminal evidence"
		if s.State == "submitted" {
			next = fmt.Sprintf("pull or steward with `weave pull %d`", s.Issue)
		}
		return weaveHealthReport{Health: weaveHealthHealthy, Reason: "terminal evidence is coherent", Next: next, Snapshot: s}
	}
	if s.WrapperPID > 0 && !s.WrapperAlive {
		return weaveHealthReport{Health: weaveHealthStale, Reason: "wrapper process is no longer alive", Next: fmt.Sprintf("inspect, then `weave start --resume --issue %d` or `weave abandon %d`", s.Issue, s.Issue), Snapshot: s}
	}
	if !s.DeadlineAt.IsZero() && !now.Before(s.DeadlineAt) {
		return weaveHealthReport{Health: weaveHealthWedged, Reason: fmt.Sprintf("run exceeded its %s runtime deadline", s.DeadlineAt.Sub(s.StartedAt)), Next: fmt.Sprintf("inspect and `weave kill %d` if it is not making progress", s.Issue), Snapshot: s}
	}
	if s.WrapperPID <= 0 || !s.WrapperAlive {
		return weaveHealthReport{Health: weaveHealthIdle, Reason: "run has no live worker evidence yet", Next: fmt.Sprintf("inspect launch evidence before assigning another worker (#%d)", s.Issue), Snapshot: s}
	}
	last := s.LastProgressAt
	if last.IsZero() {
		return weaveHealthReport{Health: weaveHealthIdle, Reason: "worker is live but has recorded no progress", Next: fmt.Sprintf("attach or inspect run #%d; reassign only after confirming it is idle", s.Issue), Snapshot: s}
	}
	if now.Sub(last) > weaveHealthIdleAfter(s, it) {
		return weaveHealthReport{Health: weaveHealthWedged, Reason: fmt.Sprintf("no worker progress for %s", now.Sub(last).Round(time.Second)), Next: fmt.Sprintf("inspect and `weave kill %d` if it is not making progress", s.Issue), Snapshot: s}
	}
	return weaveHealthReport{Health: weaveHealthHealthy, Reason: "live worker and recent progress evidence agree", Next: fmt.Sprintf("continue monitoring run #%d", s.Issue), Snapshot: s}
}
