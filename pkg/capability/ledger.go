package capability

// THE RUN LEDGER — the raw evidence the matrix throws away.
//
// pkg/capability's Matrix holds an EXPONENTIAL MOVING AVERAGE per
// (agent, capability). That is the right shape for routing — one number,
// cheap to read, biased toward recent behaviour — and it is the wrong shape
// for a leaderboard, for a reason worth stating plainly because it is not
// recoverable after the fact:
//
//	An EMA cannot be un-averaged. Given quality 0.72 over 13 samples there is
//	no way back to "9 passed, 4 failed", so there is no way to compute a
//	confidence interval, and no way to tell a genuine 0.72 from one agent's
//	single lucky run followed by a decay.
//
// Ranking on a point estimate with no n is exactly how a fleet comes to trust
// an agent that ran twice. So the ledger records the OUTCOMES, append-only,
// and the leaderboard computes from those. The matrix stays what it is.
//
// # Append-only, and it holds no identity
//
// One JSONL line per finalized run, O_APPEND so several agents on one host
// write without a lock — the same discipline as the graph contribution log and
// the attest ledger.
//
// The record is SANITIZED BY CONSTRUCTION rather than by a scrubber: there is
// no field for a hostname, a username, a path, an issue title, or a branch
// name. Repo is a basename. That is deliberate — this file is designed to be
// summarized into a doc that ships in an OSS repo, and a scrubber you have to
// remember to run is a scrubber that eventually does not run. The publishing
// script greps as a second line of defence (belt and braces), but the first
// line is that the identity-bearing fields do not exist.
//
// # Nil is not zero, here as everywhere
//
// GatePass, VerifyExit and Recovered are POINTERS. A run whose verify never
// ran is not a failed run — it is a run with no evidence, and the difference
// is the whole reason this repo has an evidence invariant. A leaderboard that
// counted absent gates as failures would rank an agent down for the harness's
// crash.

import (
	"bufio"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// LedgerSchema is the envelope version. Fields may be ADDED within a major
// version; they may never be repurposed, because a published leaderboard is
// regenerated from records written by older binaries.
const LedgerSchema = "bashy-run-ledger-v1"

// RunSource is where a record came from. Kept coarse on purpose: the
// leaderboard reports per-source counts so a reader can tell a gated weave run
// from a meeting turn, and those are not comparable evidence.
type RunSource string

const (
	SourceWeave RunSource = "weave" // a gated run in an isolated workspace
	SourceMeet  RunSource = "meet"  // a deliberation turn — operability only
	SourceEval  RunSource = "eval"  // a benchmark harness run
)

// RunRecord is one finalized run.
//
// Every field is either a measurement or a coarse label. If you find yourself
// adding a field that names a machine, a person, or a path, it belongs in the
// host-local fact store and not here.
type RunRecord struct {
	At     string    `json:"at"`             // RFC3339 UTC
	Agent  string    `json:"agent"`          // tool:model — the canonical matrix identity, never a band
	Role   string    `json:"role,omitempty"` // seat the agent held, when known
	Source RunSource `json:"source"`         // weave | meet | eval
	Repo   string    `json:"repo,omitempty"` // BASENAME only

	// GatePass is the leaderboard's primary outcome. Nil means the gate did
	// not run — no evidence, not a failure.
	GatePass   *bool  `json:"gate_pass,omitempty"`
	VerifyExit *int   `json:"verify_exit,omitempty"`
	Review     string `json:"review_verdict,omitempty"`

	WallMS    int64 `json:"wall_ms,omitempty"`
	CostMicro int64 `json:"cost_micro,omitempty"`
	TokensIn  int64 `json:"tokens_in,omitempty"`
	TokensOut int64 `json:"tokens_out,omitempty"`

	// CoachMode is events | pty. A pty scrape is a NOVELTY PROXY, not a call
	// count, so records from the two modes are never pooled into one repeat
	// statistic — the leaderboard filters to events-mode for loop discipline.
	CoachMode   string  `json:"coach_mode,omitempty"`
	RepeatRatio float64 `json:"repeat_ratio,omitempty"`
	Steers      int     `json:"steers,omitempty"`
	Recovered   *bool   `json:"recovered,omitempty"` // steered >=1 and still converged
}

// LedgerPath is the append-only run log. Honours BASHY_CAPABILITY_DIR, so a
// test or an eval harness can point the whole store somewhere disposable.
func LedgerPath() string {
	d := Dir()
	if d == "" {
		return ""
	}
	return filepath.Join(d, "ledger.jsonl")
}

// Append writes one record.
//
// Callers treat this as best-effort (`_ =`): a leaderboard that cannot be
// written must never fail the run it is describing. That is why the weave and
// meet call sites discard the error — the evidence is a by-product of the
// work, and the work is what matters.
func Append(r RunRecord) error {
	path := LedgerPath()
	if path == "" {
		return errors.New("capability: no store directory")
	}
	if strings.TrimSpace(r.Agent) == "" {
		// No canonical identity, no row. Recording an outcome against a
		// guessed agent is worse than recording nothing: it is evidence
		// pointing at the wrong actor, and nothing downstream can detect it.
		return errors.New("capability: a ledger record needs a tool:model agent")
	}
	if r.At == "" {
		r.At = NowRFC()
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	line, err := json.Marshal(r)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(line, '\n'))
	return err
}

// ReadLedger replays the log.
//
// A malformed line is SKIPPED, never fatal. This is an append-only log several
// processes write concurrently; one torn line must not make the whole history
// unreadable, and a leaderboard that refuses to render because of one bad
// record is a leaderboard nobody can use.
//
// An absent log is an empty history and no error — a host that has run nothing
// has a real, reportable state.
func ReadLedger() ([]RunRecord, error) {
	path := LedgerPath()
	if path == "" {
		return nil, nil
	}
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()

	var out []RunRecord
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var r RunRecord
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			continue
		}
		if r.Agent == "" {
			continue
		}
		out = append(out, r)
	}
	if err := sc.Err(); err != nil {
		return out, err
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].At < out[j].At })
	return out, nil
}

// Since filters to records newer than d. Zero returns everything.
//
// A record whose timestamp will not parse is KEPT: an unreadable clock is not
// grounds to drop an outcome that happened, and dropping it would silently
// shrink an agent's sample count.
func Since(records []RunRecord, d time.Duration, now time.Time) []RunRecord {
	if d <= 0 {
		return records
	}
	cutoff := now.Add(-d)
	out := make([]RunRecord, 0, len(records))
	for _, r := range records {
		t, err := time.Parse(time.RFC3339, r.At)
		if err != nil || !t.Before(cutoff) {
			out = append(out, r)
		}
	}
	return out
}
