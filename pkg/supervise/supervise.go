// Package supervise implements `bashy supervise` — the conductor-as-a-verb.
//
// A FLEET of worker agents (the spokes) works against a GOAL decomposed into
// CONTRACTS, with optional deterministic gates, and files a report. An optional
// SUPERVISOR agent may add a final summary, but it never determines success.
// It is the turnkey form of the conductor pattern
// (docs/conductor-team-model.md) and the in-place, shared-tree counterpart to
// `bashy weave`: weave isolates each agent in its own git worktree, which is the
// wrong tool when the work spans a sibling repo or depends on gitignored assets
// (a submodule, an `external/` symlink). supervise runs the fleet IN the current
// working tree so it sees everything the operator does.
//
// The load-bearing idea is the GATE. Each contract carries a shell command that
// the ORCHESTRATOR runs itself after the worker finishes — the verdict is that
// command's exit code, never the agent's own claim of success. This is the
// "orchestrator MUST re-run the real gate" rule from
// docs/agentic-fleet-orchestration.md, and it is the direct defense against the
// observed failure mode where `codex exec` returns green-but-uncommitted: a
// worker that says "done" but did not actually make the tree pass the gate is
// recorded as FAIL and retried.
//
// It is a flexible primitive, not a policy: fleet size, optional supervisor,
// attempts, and whether to keep going past a failure are all the caller's choice. It
// launches every agent through pkg/chat (the shared agentic-CLI layer), never
// directly.
package supervise

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const schemaVersion = "bashy-supervise-v1"

// nowFn is indirected so tests get deterministic timestamps.
var nowFn = time.Now

// Contract is one unit of work: a goal a worker must achieve, and the objective
// GATE that decides whether it did. A contract is the spoke's whole world —
// input goal in, artifact + verdict out (docs/conductor-team-model.md).
type Contract struct {
	ID   string `json:"id"`
	Goal string `json:"goal"`
	// Gate is a shell command run by the ORCHESTRATOR after the worker's turn.
	// Exit 0 => the contract is met. Empty gate => advisory only (the worker's
	// exit code stands in), which the report marks as UNVERIFIED so nobody
	// mistakes an ungated task for independently verified work.
	Gate   string `json:"gate,omitempty"`
	Worker string `json:"worker,omitempty"` // pinned spoke, or "" to rotate the fleet
}

func (c *Contract) gated() bool { return strings.TrimSpace(c.Gate) != "" }

// Verdict is a contract's sealed outcome. A supplied gate determines Passed;
// otherwise the worker exit does and Unverified records the weaker evidence.
type Verdict struct {
	Contract   string    `json:"contract"`
	Worker     string    `json:"worker"`
	Attempts   int       `json:"attempts"`
	Passed     bool      `json:"passed"`
	Unverified bool      `json:"unverified,omitempty"` // no gate — worker exit only
	GateExit   int       `json:"gate_exit"`
	Detail     string    `json:"detail,omitempty"` // gate output tail on failure
	At         time.Time `json:"at"`
}

// Plan is the durable supervision header.
type Plan struct {
	Schema      string      `json:"schema"`
	ID          string      `json:"id"`
	Goal        string      `json:"goal"`
	Brief       []string    `json:"brief,omitempty"`      // context files handed to every worker
	Supervisor  string      `json:"supervisor,omitempty"` // optional agent that adds a final summary
	Fleet       []string    `json:"fleet"`                // worker spokes
	Contracts   []*Contract `json:"contracts"`
	MaxAttempts int         `json:"max_attempts,omitempty"`
	Sandbox     string      `json:"sandbox,omitempty"` // e.g. danger-full-access
	TurnTimeout string      `json:"turn_timeout,omitempty"`
	KeepGoing   bool        `json:"keep_going,omitempty"` // continue past a failed contract
	Cwd         string      `json:"cwd"`
	Out         string      `json:"out,omitempty"`
	Created     time.Time   `json:"created"`
}

func (p *Plan) maxAttempts() int {
	if p.MaxAttempts > 0 {
		return p.MaxAttempts
	}
	return 3
}

func (p *Plan) turnTimeout() time.Duration {
	if d, err := time.ParseDuration(strings.TrimSpace(p.TurnTimeout)); err == nil && d > 0 {
		return d
	}
	return 30 * time.Minute
}

// wor..pick returns the worker for a contract on a given attempt: a pinned
// Contract.Worker always wins; otherwise the fleet is rotated so a retry lands
// on a DIFFERENT agent (diversity beats hammering one tool that already failed).
func (p *Plan) pick(c *Contract, attempt int) string {
	if w := strings.TrimSpace(c.Worker); w != "" {
		return w
	}
	if len(p.Fleet) == 0 {
		return ""
	}
	return p.Fleet[attempt%len(p.Fleet)]
}

// Validate enforces the invariants a supervision needs to be meaningful.
func (p *Plan) Validate() error {
	if strings.TrimSpace(p.Goal) == "" {
		return fmt.Errorf("supervise: --goal is required")
	}
	if len(p.Contracts) == 0 {
		return fmt.Errorf("supervise: at least one --task is required")
	}
	for _, c := range p.Contracts {
		if strings.TrimSpace(c.Goal) == "" {
			return fmt.Errorf("supervise: a task has no goal")
		}
		if w := strings.TrimSpace(c.Worker); w != "" && !contains(p.Fleet, w) {
			return fmt.Errorf("supervise: task %q pins worker %q, which is not in --worker fleet %v", c.ID, w, p.Fleet)
		}
	}
	if len(p.Fleet) == 0 {
		return fmt.Errorf("supervise: at least one --worker is required")
	}
	return nil
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if strings.EqualFold(x, s) {
			return true
		}
	}
	return false
}

// Event is one append-only record in a supervision transcript.
type Event struct {
	Kind     string    `json:"kind"` // dispatch|turn|gate|verdict|summary|note
	Contract string    `json:"contract,omitempty"`
	Worker   string    `json:"worker,omitempty"`
	Attempt  int       `json:"attempt,omitempty"`
	Text     string    `json:"text,omitempty"`
	File     string    `json:"file,omitempty"`
	TS       time.Time `json:"ts"`
}

func newID(goal string, now time.Time) string {
	sum := sha256.Sum256([]byte(goal + now.Format(time.RFC3339Nano)))
	return fmt.Sprintf("%s-%s", now.Format("2006-01-02"), hex.EncodeToString(sum[:])[:8])
}

// baseDir is the supervision store root, overridable for tests.
func baseDir() (string, error) {
	if d := strings.TrimSpace(os.Getenv("BASHY_SUPERVISE_DIR")); d != "" {
		return d, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".bashy", "supervise"), nil
}

func storeDir(id string) (string, error) {
	base, err := baseDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, id), nil
}

func (p *Plan) appendEvent(e Event) {
	dir, err := storeDir(p.ID)
	if err != nil {
		return
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	if e.TS.IsZero() {
		e.TS = nowFn()
	}
	f, err := os.OpenFile(filepath.Join(dir, "transcript.jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	if b, err := json.Marshal(e); err == nil {
		_, _ = f.Write(append(b, '\n'))
	}
}

// writeTurnFile offloads a worker's full output for read-on-demand.
func (p *Plan) writeTurnFile(c *Contract, attempt int, text string) string {
	dir, err := storeDir(p.ID)
	if err != nil {
		return ""
	}
	turns := filepath.Join(dir, "turns")
	if err := os.MkdirAll(turns, 0o755); err != nil {
		return ""
	}
	name := fmt.Sprintf("%s-%d.txt", slug(c.ID), attempt)
	path := filepath.Join(turns, name)
	if os.WriteFile(path, []byte(text), 0o644) != nil {
		return ""
	}
	return path
}

func slug(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		out = "task"
	}
	return out
}
