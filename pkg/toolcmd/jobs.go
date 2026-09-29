package toolcmd

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/room"
)

// An async job is the detached runner process itself — no daemon. Its whole
// state is one directory, $BASHY_HOME/toolcmd/jobs/<id>/ (else
// ~/.bashy/toolcmd/jobs/<id>/; never os.UserConfigDir):
//
//	job.json     the spec and lifecycle (queued → running → done|failed|cancelled)
//	result.json  the Result envelope, written once when the run ends
//	output.log   the tool's stdout tee
//	cancel       marker written by `cancel`, read by the runner

// Job statuses.
const (
	JobQueued    = "queued"
	JobRunning   = "running"
	JobDone      = "done"
	JobFailed    = "failed"
	JobCancelled = "cancelled"
	JobLost      = "lost" // running on record, but its process is gone
)

// Job is the persisted record of one async command run.
type Job struct {
	ID      string    `json:"id"`
	Tool    string    `json:"tool"`
	Command string    `json:"command"`
	Args    string    `json:"args,omitempty"`
	Argv    []string  `json:"argv,omitempty"`
	Dir     string    `json:"dir,omitempty"`
	Model   string    `json:"model,omitempty"`
	Agent   string    `json:"agent,omitempty"`
	Timeout string    `json:"timeout,omitempty"`
	Status  string    `json:"status"`
	PID     int       `json:"pid,omitempty"`
	Outcome string    `json:"outcome,omitempty"`
	Error   string    `json:"error,omitempty"`
	Created time.Time `json:"created"`
	Started time.Time `json:"started,omitzero"`
	Ended   time.Time `json:"ended,omitzero"`
}

// Terminal reports whether the job will not change again.
func (j Job) Terminal() bool {
	switch j.Status {
	case JobDone, JobFailed, JobCancelled, JobLost:
		return true
	}
	return false
}

// Seams: tests replace these so the lifecycle runs in-process with a fake
// runner. Production spawns bashy's own binary detached.
var (
	// spawnJob starts the detached runner for job id; it returns the pid.
	spawnJob = spawnSelf
	// jobSelfArgv is the argv prefix that reaches the hidden runner verb
	// (set by NewCmd from its mount point: [exe, "tool", "cmd"]).
	jobSelfArgv func() ([]string, error)
	// lookupCommand resolves TOOL:NAME against the fleet catalog.
	lookupCommand = LookupCommand
	// runCommand executes a resolved command (Run in production).
	runCommand = Run
	// signalJob asks a running job's process to stop.
	signalJob = terminatePID
	// pidAlive reports whether a job's runner process still exists.
	pidAlive = room.PidAlive
)

// JobsDir is $BASHY_HOME/toolcmd/jobs, or ~/.bashy/toolcmd/jobs.
func JobsDir() (string, error) {
	if home := strings.TrimSpace(os.Getenv("BASHY_HOME")); home != "" {
		return filepath.Join(home, "toolcmd", "jobs"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("toolcmd: resolve bashy home: %w", err)
	}
	return filepath.Join(home, ".bashy", "toolcmd", "jobs"), nil
}

func jobDir(id string) (string, error) {
	if id == "" || strings.ContainsAny(id, `/\`) || strings.HasPrefix(id, ".") {
		return "", fmt.Errorf("toolcmd: invalid job id %q", id)
	}
	root, err := JobsDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, id), nil
}

func newJobID() string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return time.Now().UTC().Format("20060102T150405") + "-" + hex.EncodeToString(b[:])
}

// LookupCommand resolves "TOOL:NAME" against the fleet catalog.
func LookupCommand(ref string) (fleet.Tool, fleet.ToolCommand, error) {
	toolName, name, ok := strings.Cut(ref, ":")
	if !ok || toolName == "" || name == "" {
		return fleet.Tool{}, fleet.ToolCommand{}, fmt.Errorf("toolcmd: want TOOL:NAME, got %q", ref)
	}
	tool, found := fleet.New().Tool(toolName)
	if !found {
		return fleet.Tool{}, fleet.ToolCommand{}, fmt.Errorf("toolcmd: unknown tool %q (see `bashy tool list`)", toolName)
	}
	cmd, found := tool.Command(name)
	if !found {
		var names []string
		for _, c := range tool.Commands {
			names = append(names, c.Name)
		}
		have := "none declared"
		if len(names) > 0 {
			have = strings.Join(names, ", ")
		}
		return tool, fleet.ToolCommand{}, fmt.Errorf("toolcmd: tool %s declares no command %q (%s)", tool.Name, name, have)
	}
	return tool, cmd, nil
}

func writeJSONAtomic(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	if _, err := f.Write(append(b, '\n')); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}

// ReadJob loads a job record. A running job whose process is gone is
// reported as lost (and not rewritten: the record stays what the runner
// last said).
func ReadJob(id string) (Job, error) {
	dir, err := jobDir(id)
	if err != nil {
		return Job{}, err
	}
	b, err := os.ReadFile(filepath.Join(dir, "job.json"))
	if err != nil {
		if os.IsNotExist(err) {
			return Job{}, fmt.Errorf("toolcmd: no job %q", id)
		}
		return Job{}, err
	}
	var j Job
	if err := json.Unmarshal(b, &j); err != nil {
		return Job{}, fmt.Errorf("toolcmd: job %s: %w", id, err)
	}
	if j.Status == JobQueued {
		if b, err := os.ReadFile(filepath.Join(dir, "spawn.pid")); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil && pid > 0 {
				j.PID = pid
				if !pidAlive(pid) {
					if b2, err := os.ReadFile(filepath.Join(dir, "job.json")); err == nil {
						var j2 Job
						if json.Unmarshal(b2, &j2) == nil && j2.Status != JobQueued {
							return j2, nil
						}
					}
					j.Status = JobLost
				}
			}
		}
		return j, nil
	}
	if j.Status == JobRunning && j.PID > 0 && !pidAlive(j.PID) {
		// Re-read once: the runner may have finished between the two reads.
		if b2, err := os.ReadFile(filepath.Join(dir, "job.json")); err == nil {
			var j2 Job
			if json.Unmarshal(b2, &j2) == nil && j2.Terminal() {
				return j2, nil
			}
		}
		j.Status = JobLost
	}
	return j, nil
}

func saveJob(j Job) error {
	dir, err := jobDir(j.ID)
	if err != nil {
		return err
	}
	return writeJSONAtomic(filepath.Join(dir, "job.json"), j)
}

// StartJob records a new job and spawns its detached runner.
func StartJob(ref, args string, opts Options) (Job, error) {
	tool, cmd, err := lookupCommand(ref)
	if err != nil {
		return Job{}, err
	}
	j := Job{
		ID: newJobID(), Tool: tool.Name, Command: cmd.Name, Args: args, Argv: opts.Argv,
		Dir: opts.Dir, Model: opts.Model, Agent: opts.Agent,
		Status: JobQueued, Created: time.Now().UTC(),
	}
	if opts.Timeout > 0 {
		j.Timeout = opts.Timeout.String()
	}
	if j.Dir != "" {
		if abs, err := filepath.Abs(j.Dir); err == nil {
			j.Dir = abs
		}
	}
	dir, err := jobDir(j.ID)
	if err != nil {
		return Job{}, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return Job{}, fmt.Errorf("toolcmd: job dir: %w", err)
	}
	if err := saveJob(j); err != nil {
		return Job{}, err
	}
	pid, err := spawnJob(j.ID, dir)
	if err != nil {
		j.Status, j.Error, j.Ended = JobFailed, "spawn: "+err.Error(), time.Now().UTC()
		_ = saveJob(j)
		return j, fmt.Errorf("toolcmd: start job: %w", err)
	}
	// The runner records its own pid in job.json when it starts. The spawn
	// pid goes in a side file (job.json has one writer at a time: this
	// process before the spawn, the runner after), so a runner that dies
	// before recording itself still reads as lost.
	_ = os.WriteFile(filepath.Join(dir, "spawn.pid"), []byte(strconv.Itoa(pid)+"\n"), 0o600)
	j.PID = pid
	return j, nil
}

// RunJob is the body of the detached runner process: it executes job id to
// completion and records the result. ctx is cancelled by `cancel` (a signal).
func RunJob(ctx context.Context, id string) error {
	j, err := ReadJob(id)
	if err != nil {
		return err
	}
	dir, _ := jobDir(id)
	if j.Status != JobQueued {
		return fmt.Errorf("toolcmd: job %s is %s, not queued", id, j.Status)
	}
	if _, err := os.Stat(filepath.Join(dir, "cancel")); err == nil {
		j.Status, j.Outcome, j.Ended = JobCancelled, OutcomeCancelled, time.Now().UTC()
		return saveJob(j)
	}
	j.Status, j.PID, j.Started = JobRunning, os.Getpid(), time.Now().UTC()
	if err := saveJob(j); err != nil {
		return err
	}

	opts := Options{Dir: j.Dir, Model: j.Model, Agent: j.Agent, Argv: j.Argv}
	if j.Timeout != "" {
		if d, err := time.ParseDuration(j.Timeout); err == nil {
			opts.Timeout = d
		}
	}
	var res Result
	var runErr error
	logf, lerr := os.OpenFile(filepath.Join(dir, "output.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if lerr == nil {
		opts.Stdout = logf
		defer logf.Close()
	}
	tool, cmd, lookErr := lookupCommand(j.Tool + ":" + j.Command)
	if lookErr != nil {
		runErr = lookErr
		res = Result{Tool: j.Tool, Command: j.Command, Outcome: OutcomeError, Error: lookErr.Error()}
	} else {
		res, runErr = runCommand(ctx, tool, cmd, j.Args, opts)
	}
	if _, err := os.Stat(filepath.Join(dir, "cancel")); err == nil || ctx.Err() != nil {
		res.Outcome = OutcomeCancelled
	}
	if err := writeJSONAtomic(filepath.Join(dir, "result.json"), res); err != nil {
		return err
	}
	j.Outcome, j.Ended = res.Outcome, time.Now().UTC()
	switch {
	case res.Outcome == OutcomeCancelled:
		j.Status = JobCancelled
	case runErr != nil || res.Outcome != OutcomeSuccess:
		j.Status = JobFailed
		if runErr != nil {
			j.Error = runErr.Error()
		} else {
			j.Error = res.Error
		}
	default:
		j.Status = JobDone
	}
	return saveJob(j)
}

// WaitJob polls until the job is terminal or ctx ends.
func WaitJob(ctx context.Context, id string, poll time.Duration) (Job, error) {
	if poll <= 0 {
		poll = 500 * time.Millisecond
	}
	for {
		j, err := ReadJob(id)
		if err != nil {
			return j, err
		}
		if j.Terminal() {
			return j, nil
		}
		select {
		case <-ctx.Done():
			return j, fmt.Errorf("toolcmd: job %s still %s: %w", id, j.Status, ctx.Err())
		case <-time.After(poll):
		}
	}
}

// ReadResult returns the Result a finished job recorded.
func ReadResult(id string) (Result, error) {
	j, err := ReadJob(id)
	if err != nil {
		return Result{}, err
	}
	dir, _ := jobDir(id)
	b, err := os.ReadFile(filepath.Join(dir, "result.json"))
	if err != nil {
		if os.IsNotExist(err) {
			return Result{}, fmt.Errorf("toolcmd: job %s is %s; no result yet", id, j.Status)
		}
		return Result{}, err
	}
	var r Result
	if err := json.Unmarshal(b, &r); err != nil {
		return Result{}, fmt.Errorf("toolcmd: job %s result: %w", id, err)
	}
	return r, nil
}

// CancelJob asks a job to stop. A queued job is cancelled on the record; a
// running one gets the cancel marker plus a signal, and is marked cancelled
// if its process does not record that itself within grace.
func CancelJob(ctx context.Context, id string, grace time.Duration) (Job, error) {
	j, err := ReadJob(id)
	if err != nil {
		return j, err
	}
	if j.Terminal() {
		return j, nil
	}
	dir, _ := jobDir(id)
	if err := os.WriteFile(filepath.Join(dir, "cancel"), []byte(time.Now().UTC().Format(time.RFC3339)+"\n"), 0o600); err != nil {
		return j, err
	}
	if j.PID > 0 {
		if err := signalJob(j.PID); err != nil && !errors.Is(err, os.ErrProcessDone) {
			return j, fmt.Errorf("toolcmd: signal job %s (pid %d): %w", id, j.PID, err)
		}
	}
	wctx, cancel := context.WithTimeout(ctx, grace)
	defer cancel()
	if done, err := WaitJob(wctx, id, 100*time.Millisecond); err == nil {
		return done, nil
	}
	j, err = ReadJob(id)
	if err != nil || j.Terminal() {
		return j, err
	}
	j.Status, j.Outcome, j.Ended = JobCancelled, OutcomeCancelled, time.Now().UTC()
	return j, saveJob(j)
}

// ListJobs returns every job record, newest first.
func ListJobs() ([]Job, error) {
	root, err := JobsDir()
	if err != nil {
		return nil, err
	}
	ents, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []Job
	for i := len(ents) - 1; i >= 0; i-- {
		if !ents[i].IsDir() {
			continue
		}
		if j, err := ReadJob(ents[i].Name()); err == nil {
			out = append(out, j)
		}
	}
	return out, nil
}
