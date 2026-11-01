// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

package webconsole

import (
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/qiangli/yoke/pkg/shape/appserver"
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

// repairLimitForRev resolves the maximum repair attempts for one incident:
// the served revision's shaped.json repair.max_attempts first, else the
// host default (ShapeOptions.RepairMaxAttempts), else
// DefaultMaxRepairAttempts. A present but invalid app value warns and uses
// the default — never unlimited by accident.
func (a *shapedApp) repairLimitForRev(rev string) int {
	if n, ok := a.repairLimitFromApp(rev); ok {
		return n
	}
	if a.opts.RepairMaxAttempts != nil {
		return *a.opts.RepairMaxAttempts
	}
	return DefaultMaxRepairAttempts
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
