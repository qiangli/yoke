// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

package webconsole

import (
	"fmt"
	shapecore "github.com/qiangli/yoke/pkg/shape"
	"log/slog"
	"strconv"
	"time"

	"github.com/qiangli/yoke/pkg/shape/appserver"
	shape "github.com/qiangli/yoke/pkg/shape/state"
)

// RepairUnlimited is the incidentStore limit meaning no cap on repair
// attempts: keep trying until a repair activates, is refused, or the
// incident leaves the repairing state. One repair stays in flight per
// (app, principal) either way (the serial in startRepairAsync).
const RepairUnlimited = -1

// repairAttemptTimeout caps one repair attempt: the clone, delegate,
// verification, replay and activation of a single candidate. It used to
// bound the whole incident; with a configurable (and possibly unlimited)
// attempt count each attempt gets its own window instead.
const repairAttemptTimeout = 20 * time.Minute

// DefaultRepairWallTime bounds one incident's whole repair, every attempt
// included, when the host sets no ShapeOptions.RepairMaxWallTime.
const DefaultRepairWallTime = 60 * time.Minute

// repairLimitForRev resolves the maximum repair attempts for one incident.
// The host bound wins: it is ShapeOptions.RepairMaxAttempts, else
// DefaultMaxRepairAttempts, and the served revision's shaped.json
// repair.max_attempts may only lower it — never raise it, never make it
// unlimited. A present but invalid app value warns and uses the default,
// still under the host bound. With unattended shaping switched off on this
// host (`bashy shape auto off`) the limit is 0: record and report only.
func (a *shapedApp) repairLimitForRev(rev string) int {
	if !a.autoEnabled() {
		return 0
	}
	return a.repairLimit(rev, false)
}

// repairLimit is repairLimitForRev without the off switch. override is one
// run's logged permission to exceed the host bound: the app's value then
// applies as written.
func (a *shapedApp) repairLimit(rev string, override bool) int {
	host := DefaultMaxRepairAttempts
	if a.opts.RepairMaxAttempts != nil {
		host = *a.opts.RepairMaxAttempts
	}
	n, ok := a.repairLimitFromApp(rev)
	if !ok {
		return host
	}
	if override || host < 0 {
		return n
	}
	if n < 0 || n > host {
		return host
	}
	return n
}

func (a *shapedApp) repairWallTime() time.Duration {
	if a.opts.RepairMaxWallTime > 0 {
		return a.opts.RepairMaxWallTime
	}
	return DefaultRepairWallTime
}

// autoEnabled reads the host off switch for unattended shaping.
func (a *shapedApp) autoEnabled() bool {
	home, err := a.opts.home()
	return err != nil || shape.AutoEnabled(home)
}

// logAuto appends one line to the host's decision log for incident in.
// The log is a record, not a gate: a write error only warns.
func (a *shapedApp) logAuto(principal string, in Incident, event, result, detail string) {
	a.candidateService().LogDecision(shapecore.Candidate{
		Principal: principal, ActivationPrincipal: principal, BaseRev: in.BaseRev,
		Source: shapecore.Source{Kind: "incident", Ref: in.ID},
	}, event, result, detail)
}

// logTrigger records what an observed incident triggered: a repair, or a
// refusal because unattended shaping is off on this host.
func (a *shapedApp) logTrigger(principal string, in Incident, start bool) {
	switch {
	case start:
		a.logAuto(principal, in, "trigger", "repair", string(in.Kind)+" "+in.Path)
	case in.Count == 1 && !a.autoEnabled():
		a.logAuto(principal, in, "refusal", "off", "unattended shaping is off on this host")
	}
}

func (a *shapedApp) repairLimitFromApp(rev string) (int, bool) {
	repo, err := a.open()
	if err != nil {
		return 0, false
	}
	commit, err := resolveCommit(repo, rev)
	if err != nil {
		commit, err = base(repo)
		if err != nil {
			return 0, false
		}
	}
	tree, err := commit.Tree()
	if err != nil {
		return 0, false
	}
	f, err := tree.File(appserver.File)
	if err != nil {
		return 0, false // no manifest: not this app's decision
	}
	body, err := f.Contents()
	if err != nil {
		return 0, false
	}
	m, err := appserver.Parse([]byte(body))
	if err != nil {
		slog.Warn("shaped app: unreadable shaped.json, using default repair attempts", "app", a.name, "err", err)
		return 0, false
	}
	if m.Repair == nil || !m.Repair.MaxAttempts.Set {
		return 0, false
	}
	lim := m.Repair.MaxAttempts
	if !lim.Valid {
		slog.Warn("shaped app: invalid repair.max_attempts, using default", "app", a.name)
		return DefaultMaxRepairAttempts, true
	}
	if lim.Unlimited {
		return RepairUnlimited, true
	}
	return lim.N, true
}

// parseRepairMaxAttempts parses the --repair-max-attempts serve flag: a
// non-negative number, "unlimited", or empty for the app default. Anything
// else fails loudly at startup.
func parseRepairMaxAttempts(s string) (*int, error) {
	if s == "" {
		return nil, nil
	}
	if s == "unlimited" {
		n := RepairUnlimited
		return &n, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 {
		return nil, fmt.Errorf("invalid --repair-max-attempts %q: a non-negative number or %q", s, "unlimited")
	}
	return &n, nil
}
