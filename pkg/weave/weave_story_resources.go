package weave

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"time"

	"github.com/qiangli/coreutils/pkg/lockfile"
	"github.com/spf13/cobra"
)

// Resource composition is injected by the host: resources already imports weave.
type SprintResourceSummary struct {
	At       time.Time `json:"at"`
	Status   string    `json:"status"`
	Host     string    `json:"host"`
	CPU      *float64  `json:"cpu_percent"`
	CPUKind  string    `json:"cpu_kind"`
	Memory   *float64  `json:"memory_percent"`
	Accounts int       `json:"accounts"`
	Alerts   int       `json:"alerts"`
	Unknown  int       `json:"unknown"`
	Read     string    `json:"read"`
	Warnings []string  `json:"warnings,omitempty"`
}
type SprintResourceProvider func(context.Context, int64) (SprintResourceSummary, error)
type SprintOption func(*cobra.Command)
type sprintResourceContextKey struct{}

func WithSprintResources(provider SprintResourceProvider) SprintOption {
	return func(cmd *cobra.Command) {
		ctx := cmd.Context()
		if ctx == nil {
			ctx = context.Background()
		}
		cmd.SetContext(context.WithValue(ctx, sprintResourceContextKey{}, provider))
		var install func(*cobra.Command)
		install = func(c *cobra.Command) {
			if original := c.RunE; original != nil {
				c.RunE = func(c *cobra.Command, args []string) error {
					c.SetContext(context.WithValue(c.Context(), sprintResourceContextKey{}, provider))
					return original(c, args)
				}
			}
			for _, child := range c.Commands() {
				install(child)
			}
		}
		install(cmd)
	}
}
func sprintResources(cmd *cobra.Command, id int64) SprintResourceSummary {
	summary := SprintResourceSummary{Status: "unavailable", Read: "bashy sprint monitor"}
	if id > 0 {
		summary.Read += " " + strconv.FormatInt(id, 10)
	}
	provider, _ := cmd.Context().Value(sprintResourceContextKey{}).(SprintResourceProvider)
	if provider == nil {
		return summary
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), 5*time.Second)
	defer cancel()
	got, err := provider(ctx, id)
	if err != nil {
		summary.Warnings = []string{err.Error()}
		return summary
	}
	return got
}
func renderSprintResources(w io.Writer, s SprintResourceSummary) {
	fmt.Fprintf(w, "resources: %s; %d accounts, %d alerts, %d unknown — %s\n", s.Status, s.Accounts, s.Alerts, s.Unknown, s.Read)
}

// SprintInventory is a bounded, derived projection. Reading it never reconciles
// queues, refreshes leases, prunes work, or invokes a worker.
type SprintInventory struct {
	At        time.Time                 `json:"at"`
	ExpiresAt time.Time                 `json:"expires_at"`
	Sprints   []SprintInventorySeat     `json:"sprints"`
	Workloads []SprintInventoryWorkload `json:"workloads"`
	Complete  bool                      `json:"complete"`
	Warnings  []string                  `json:"warnings"`
}
type SprintInventorySeat struct {
	ID          int64     `json:"id"`
	Owner       string    `json:"owner"`
	Active      bool      `json:"active"`
	AttachedPID int       `json:"attached_pid"`
	UpdatedAt   time.Time `json:"updated_at"`
}
type SprintInventoryWorkload struct {
	ID        string    `json:"id"`
	Sprint    int64     `json:"sprint"`
	Todo      string    `json:"todo,omitempty"`
	Run       int64     `json:"run"`
	Repo      string    `json:"repo"`
	Agent     string    `json:"agent"`
	Model     string    `json:"model"`
	Tool      string    `json:"tool"`
	Workspace string    `json:"workspace"`
	PID       int       `json:"pid"`
	StartID   string    `json:"start_id,omitempty"`
	StartedAt time.Time `json:"started_at"`
	State     string    `json:"state"`
}

func ReadSprintInventory(ctx context.Context, cacheDir string) (*SprintInventory, error) {
	if cacheDir == "" {
		return nil, fmt.Errorf("resource inventory cache directory unavailable")
	}
	path := filepath.Join(cacheDir, "sprint-inventory.json")
	var cached SprintInventory
	if err := readInventoryJSON(path, &cached); err == nil && !time.Now().Before(cached.At) && time.Now().Before(cached.ExpiresAt) {
		return &cached, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	lock, err := lockfile.TryAcquire(filepath.Join(cacheDir, "sprint-inventory.lock"), lockfile.Holder{Name: "sprint-inventory", Intent: "refresh"})
	if err != nil {
		if !cached.At.IsZero() {
			cached.Complete = false
			cached.Warnings = append(cached.Warnings, "inventory refresh busy; cached attribution is stale")
			return &cached, nil
		}
		return nil, err
	}
	defer lock.Release()
	if err := readInventoryJSON(path, &cached); err == nil && !time.Now().Before(cached.At) && time.Now().Before(cached.ExpiresAt) {
		return &cached, nil
	}
	result := &SprintInventory{At: time.Now().UTC(), Complete: true}
	result.ExpiresAt = result.At.Add(5 * time.Second)
	deadline := time.Now().Add(250 * time.Millisecond)
	store, err := sprintStoreDir()
	if err != nil {
		return nil, err
	}
	var board weaveQueue
	if err := readInventoryJSON(filepath.Join(store, "queue.json"), &board); err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	sort.SliceStable(board.Stories, func(i, j int) bool {
		a, b := board.Stories[i], board.Stories[j]
		if a == nil {
			return false
		}
		if b == nil {
			return true
		}
		if a.currentBox().Running() != b.currentBox().Running() {
			return a.currentBox().Running()
		}
		return a.UpdatedAt.After(b.UpdatedAt)
	})
	linked := map[string]int64{}
	activeQueues := map[string]bool{}
	for _, s := range board.Stories {
		if s == nil {
			result.Complete = false
			continue
		}
		if len(result.Sprints) >= 256 {
			result.Complete = false
			result.Warnings = append(result.Warnings, "sprint inventory row limit reached")
			break
		}
		seat := SprintInventorySeat{ID: s.ID, Owner: s.Owner, Active: s.currentBox().Running(), UpdatedAt: s.UpdatedAt}
		if s.Lease != nil {
			seat.AttachedPID = s.Lease.AttachedPID
		}
		result.Sprints = append(result.Sprints, seat)
		for _, run := range s.Runs {
			if run.Queue != "" && !run.Born.IsZero() {
				linked[monitorRunKey(run)] = s.ID
				if seat.Active {
					activeQueues[run.Queue] = true
				}
			}
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	queueCount := 0
	for _, base := range append([]string{weaveStateRoot(home)}, weaveLegacyStateRoots(home)...) {
		f, err := os.Open(base)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			result.Complete = false
			result.Warnings = append(result.Warnings, "queue root unavailable")
			continue
		}
		entries, readErr := f.ReadDir(65)
		if len(entries) >= 65 {
			result.Complete = false
		}
		f.Close()
		if readErr != nil && readErr != io.EOF {
			result.Complete = false
		}
		queueNames := make([]string, 0, len(activeQueues)+len(entries))
		for queue := range activeQueues {
			if filepath.Base(queue) == queue && queue != "." && queue != ".." {
				queueNames = append(queueNames, queue)
			}
		}
		sort.Strings(queueNames)
		seenQueues := map[string]bool{}
		for _, queue := range queueNames {
			seenQueues[queue] = true
		}
		for _, entry := range entries {
			if entry.IsDir() && !seenQueues[entry.Name()] {
				queueNames = append(queueNames, entry.Name())
				seenQueues[entry.Name()] = true
			}
		}
		for _, queueName := range queueNames {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if queueCount >= 64 || len(result.Workloads) >= 512 || time.Now().After(deadline) {
				result.Complete = false
				break
			}
			var q weaveQueue
			if err := readInventoryJSON(filepath.Join(base, queueName, "queue.json"), &q); err != nil {
				if !os.IsNotExist(err) {
					result.Complete = false
				}
				continue
			}
			queueCount++
			sort.SliceStable(q.Items, func(i, j int) bool {
				a, b := q.Items[i], q.Items[j]
				if a == nil {
					return false
				}
				if b == nil {
					return true
				}
				activeA, activeB := a.WrapperPid > 0 || a.State == "working", b.WrapperPid > 0 || b.State == "working"
				if activeA != activeB {
					return activeA
				}
				return a.StartedAt.After(b.StartedAt)
			})
			for _, it := range q.Items {
				if it == nil {
					result.Complete = false
					continue
				}
				if isTerminalState(it.State) || (it.State == "todo" && it.WrapperPid == 0) {
					continue
				}
				if len(result.Workloads) >= 512 {
					result.Complete = false
					break
				}
				run := sprintRun{Repo: filepath.Base(q.Root), Queue: queueName, ID: it.ID, Born: it.Created}
				id := monitorRunKey(run)
				row := SprintInventoryWorkload{ID: id, Sprint: linked[id], Todo: it.Register, Run: it.ID, Repo: q.Root, Agent: it.Owner, Tool: it.Tool, Workspace: it.Workspace, PID: it.WrapperPid, StartID: it.WrapperStartID, StartedAt: it.StartedAt, State: it.State}
				if it.LaunchSpec != nil {
					row.Model = it.LaunchSpec.Model
					if row.Agent == "" {
						row.Agent = it.LaunchSpec.Agent
					}
				}
				result.Workloads = append(result.Workloads, row)
			}
		}
	}
	if !result.Complete {
		result.Warnings = append(result.Warnings, "bounded inventory incomplete; omitted work may compete for resources")
	}
	sort.Slice(result.Workloads, func(i, j int) bool { return result.Workloads[i].ID < result.Workloads[j].ID })
	data, err := json.Marshal(result)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(cacheDir, 0700); err != nil {
		return nil, err
	}
	tmp, err := os.CreateTemp(cacheDir, ".inventory-*")
	if err != nil {
		return nil, err
	}
	defer os.Remove(tmp.Name())
	if _, err = tmp.Write(data); err != nil {
		tmp.Close()
		return nil, err
	}
	if err = tmp.Close(); err != nil {
		return nil, err
	}
	if err = os.Rename(tmp.Name(), path); err != nil {
		return nil, err
	}
	return result, nil
}
func readInventoryJSON(path string, out any) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if info.Size() > 4<<20 {
		return fmt.Errorf("inventory input exceeds 4MiB limit")
	}
	return json.NewDecoder(io.LimitReader(f, 4<<20)).Decode(out)
}
func monitorRunKey(run sprintRun) string {
	return sprintRunKey(run) + "#" + strconv.FormatInt(run.ID, 10) + "@" + run.Born.UTC().Format(time.RFC3339Nano)
}

// WithSprintObservationOwner fences a notice against the same lifecycle lock
// used by ownership transfer. It neither refreshes a lease nor changes a queue.
// A busy transfer leaves the notice pending rather than sending to its old owner.
func WithSprintObservationOwner(ctx context.Context, id int64, owner string, send func() error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	dir, err := sprintStoreDir()
	if err != nil {
		return err
	}
	lock, err := lockfile.TryAcquire(filepath.Join(dir, "owner-lifecycle", fmt.Sprintf("%d.lock", id)), lockfile.Holder{Name: "resource-notice", Intent: "owner-check"})
	if err != nil {
		return err
	}
	defer lock.Release()
	var q weaveQueue
	if err := readInventoryJSON(filepath.Join(dir, "queue.json"), &q); err != nil {
		return err
	}
	s := findWeaveStory(&q, id)
	if s == nil || s.Owner != owner || !s.currentBox().Running() {
		return fmt.Errorf("sprint owner changed or stopped; resource notice remains pending")
	}
	return send()
}
