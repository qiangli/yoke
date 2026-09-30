package weave

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/qiangli/coreutils/pkg/lockfile"
	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/room"
)

// Reconcile after a durable queue/board transition, including lease release.
// Submitted/failed workers remain resumable; only merged or abandoned runs end
// ownership. Unknown ownership or unreadable safety evidence leaves it alone.
func weaveReconcileEphemeralAgents(dir string, q *weaveQueue) {
	cat := fleetCatalog()
	agents, errs := cat.Agents()
	if len(errs) != 0 {
		return
	}
	for _, a := range agents {
		if !weaveAgentWorkEnded(a, dir, q) {
			continue
		}
		_ = weaveGuardAgentLifecycle(dir, a, func() error {
			_, err := cat.ArchiveAgent(a.Name, func(current fleet.Agent) error {
				if !weaveAgentWorkEnded(current, dir, q) {
					return fmt.Errorf("agent ownership changed")
				}
				return weaveAgentIdle(current, dir, q)
			})
			return err
		})
	}
}

func weaveAgentWorkEnded(a fleet.Agent, dir string, q *weaveQueue) bool {
	p := a.Lifecycle
	if !a.Ephemeral || p == nil {
		return false
	}
	ownerDir := p.WeaveQueue
	if ownerDir == "" && p.SprintID != "" {
		var err error
		ownerDir, err = sprintStoreDir()
		if err != nil {
			return false
		}
	}
	if ownerDir == "" {
		return false
	}
	owner := q
	if ownerDir != dir {
		var err error
		owner, err = readWeaveQueue(ownerDir)
		if err != nil {
			return false
		}
	}
	if p.RunID > 0 && p.WeaveQueue != "" {
		it := findWeaveItem(owner, p.RunID)
		return it != nil && (it.State == "done" || it.State == "abandoned") &&
			!(it.WrapperPid > 0 && pidAlive(it.WrapperPid)) &&
			!(it.FinalizerPID > 0 && pidAlive(it.FinalizerPID)) &&
			(it.ResourceReservationID == "" || it.ResourceTerminated)
	}
	for _, s := range owner.Stories {
		if s != nil && s.UUID == p.SprintID && s.Column == "done" {
			return true
		}
	}
	return false
}

func weaveAgentIdle(a fleet.Agent, dir string, q *weaveQueue) error {
	matches := func(name string) bool {
		for _, n := range append([]string{a.Name, a.Nick, a.AutoNick}, a.Aliases...) {
			if n != "" && strings.EqualFold(strings.TrimSpace(name), n) {
				return true
			}
		}
		return false
	}
	cards, err := room.Members()
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	for _, c := range cards {
		if matches(c.ID) || matches(c.Nick) {
			return fmt.Errorf("agent %s is live", a.Name)
		}
	}
	queues := []*weaveQueue{q}
	board, err := sprintStoreDir()
	if err != nil {
		return err
	}
	dirs := append(weaveAllQueueDirs(), board)
	if a.Lifecycle.WeaveQueue != "" {
		dirs = append(dirs, a.Lifecycle.WeaveQueue)
	}
	for _, leaseDir := range append(dirs, dir) {
		lease, held, err := loadWeaveAutopilotLease(leaseDir)
		if err != nil {
			return err
		}
		if held && (matches(lease.Agent) || matches(lease.Holder)) {
			return fmt.Errorf("agent %s holds an orchestrator lease", a.Name)
		}
	}
	for _, otherDir := range dirs {
		if otherDir == dir {
			continue
		}
		other, err := readWeaveQueue(otherDir)
		if err != nil {
			return err
		}
		queues = append(queues, other)
	}
	for _, queue := range queues {
		if lease := queue.PausedOrchestratorLease; lease != nil && (matches(lease.Agent) || matches(lease.Holder)) {
			return fmt.Errorf("agent %s holds a paused orchestrator lease", a.Name)
		}
		for _, s := range queue.Stories {
			if s != nil && s.Lease != nil && matches(s.Lease.Holder) {
				return fmt.Errorf("agent %s holds a sprint lease", a.Name)
			}
		}
		for _, it := range queue.Items {
			if it == nil || it.LaunchSpec == nil || !matches(it.LaunchSpec.Agent) {
				continue
			}
			if (it.WrapperPid > 0 && pidAlive(it.WrapperPid)) ||
				(it.FinalizerPID > 0 && pidAlive(it.FinalizerPID)) ||
				(it.ResourceReservationID != "" && !it.ResourceTerminated) ||
				(it.State != "done" && it.State != "abandoned") {
				return fmt.Errorf("agent %s has an active run", a.Name)
			}
		}
	}
	return nil
}

// Queue writers serialize both run admission and lease acquisition. The caller
// holds dir's queue lock; try-lock every other known queue and the board so a
// concurrent claim makes maintenance defer instead of racing it or deadlocking.
func weaveGuardAgentLifecycle(dir string, a fleet.Agent, fn func() error) error {
	board, err := sprintStoreDir()
	if err != nil {
		return err
	}
	dirs := append(weaveAllQueueDirs(), board)
	if a.Lifecycle.WeaveQueue != "" {
		dirs = append(dirs, a.Lifecycle.WeaveQueue)
	}
	seen := map[string]bool{dir: true}
	var release []func() error
	defer func() {
		for i := len(release) - 1; i >= 0; i-- {
			_ = release[i]()
		}
	}()
	for _, d := range dirs {
		if seen[d] {
			continue
		}
		seen[d] = true
		held, err := lockfile.TryAcquire(filepath.Join(d, "queue.lock"), lockfile.Holder{Name: "agent-retirement", PID: os.Getpid(), Intent: "guard ephemeral lifecycle"})
		if err != nil {
			return err
		}
		release = append(release, held.Release)
	}
	return room.WithMemberClaimsGuard(fn)
}
