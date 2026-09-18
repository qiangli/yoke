package llmbudget

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/qiangli/coreutils/pkg/lockfile"
)

const stateVersion = 1
const maxStateBytes = 16 << 20
const maxCompletions = 8192

// PoolCounters extend the original meter; legacy totals remain untouched.
// Split input/cache counters cover only records made through the new API.
type PoolCounters struct {
	EstimatedTokens   bool       `json:"estimated_tokens,omitempty"`
	UnknownTokens     bool       `json:"unknown_tokens,omitempty"`
	UnknownTokensAt   *time.Time `json:"unknown_tokens_at,omitempty"`
	UnknownSpendAt    time.Time  `json:"unknown_spend_at,omitempty"`
	Binding           Binding    `json:"binding"`
	Counters          Counters   `json:"counters"`
	InputTokens       int64      `json:"input_tokens"`
	OutputTokens      int64      `json:"output_tokens"`
	CachedInputTokens int64      `json:"cached_input_tokens"`
	ObservedAt        time.Time  `json:"observed_at"`
	UnknownSpend      bool       `json:"unknown_spend,omitempty"`
}
type Completion struct {
	Owner       string            `json:"owner"`
	Request     Request           `json:"request"`
	Actual      *Actual           `json:"actual,omitempty"`
	Termination *TerminationProof `json:"termination,omitempty"`
	At          time.Time         `json:"at"`
	Kind        string            `json:"kind"`
}

func normalizeState(s *State) {
	if s.Unattributed == nil {
		s.Unattributed = map[string]Counters{}
		if s.Version == 0 {
			for m, c := range s.Models {
				s.Unattributed[m] = c
			}
		}
	}
	if s.Models == nil {
		s.Models = map[string]Counters{}
	}
	if s.Plans == nil {
		s.Plans = map[string]Counters{}
	}
	if s.Providers == nil {
		s.Providers = map[string]Counters{}
	}
	if s.Buckets == nil {
		s.Buckets = map[string]Bucket{}
	}
	if s.Pools == nil {
		s.Pools = map[string]PoolCounters{}
	}
	if s.Reservations == nil {
		s.Reservations = map[string]Reservation{}
	}
	if s.Completed == nil {
		s.Completed = map[string]Completion{}
	}
	s.Version = stateVersion
}
func readBounded(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, errors.New("budget file exceeds size limit")
	}
	return b, nil
}

// reload never converts an unreadable, replaced-with-empty or future-version
// meter into an empty budget. Missing initial state is the only empty case.
func (g *Gate) reload() error {
	if g.cfg.StatePath == "" {
		normalizeState(&g.state)
		g.loaded = true
		return nil
	}
	b, err := readBounded(g.cfg.StatePath, maxStateBytes)
	if os.IsNotExist(err) && !g.seenDisk {
		if _, markerErr := os.Stat(g.cfg.StatePath + ".initialized"); markerErr == nil {
			return errors.New("llmbudget: initialized meter missing; refusing empty reset")
		} else if !os.IsNotExist(markerErr) {
			return errors.New("llmbudget: initialization marker unreadable")
		}
		normalizeState(&g.state)
		g.loaded = true
		return nil
	}
	if err != nil {
		return fmt.Errorf("llmbudget: cannot read meter: %w", err)
	}
	var s State
	if err := json.Unmarshal(b, &s); err != nil || string(b) == "null" {
		return errors.New("llmbudget: invalid meter; refusing empty reset")
	}
	if s.Version == stateVersion && (s.Integrity == "" || s.Integrity != stateDigest(s)) {
		return errors.New("llmbudget: meter integrity check failed")
	}
	if s.Version == 0 {
		if s.Models == nil && s.Plans == nil && s.Providers == nil && s.Buckets == nil {
			return errors.New("llmbudget: empty legacy meter; refusing empty reset")
		}
		if _, e := os.Stat(g.cfg.StatePath + ".initialized"); e == nil {
			return errors.New("llmbudget: initialized meter lost its version/integrity")
		}
	}
	if s.Version < 0 || s.Version > stateVersion {
		return errors.New("llmbudget: unsupported meter version")
	}
	normalizeState(&s)
	g.state = s
	g.loaded = true
	g.seenDisk = true
	g.stateErr = nil
	return nil
}
func atomicJSON(path string, value any) error {
	b, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if len(b) > maxStateBytes {
		return errors.New("llmbudget: state size limit reached")
	}
	if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".budget-*")
	if err != nil {
		return err
	}
	temp := f.Name()
	defer os.Remove(temp)
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(b)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err := os.Rename(temp, path); err != nil {
		return err
	}
	if runtime.GOOS == "windows" {
		return nil
	} // Windows does not expose directory fsync.
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
func (g *Gate) writeState() error {
	if g.cfg.StatePath == "" {
		return nil
	}
	if err := g.publishBoundary("before-marker"); err != nil {
		return err
	}
	g.state.Integrity = stateDigest(g.state)
	if err := atomicJSON(g.cfg.StatePath+".initialized", map[string]int{"version": 1}); err != nil {
		return err
	}
	if err := g.publishBoundary("after-marker"); err != nil {
		return err
	}
	if err := atomicJSON(g.cfg.StatePath, g.state); err != nil {
		return fmt.Errorf("llmbudget: cannot publish meter: %w", err)
	}
	g.seenDisk = true
	return g.publishBoundary("after-meter")
}
func (g *Gate) transaction(ctx context.Context, fn func() error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.cfg.StatePath != "" {
		l, err := acquireMeter(ctx, g.cfg.StatePath+".lock")
		if err != nil {
			return fmt.Errorf("llmbudget: meter busy: %w", err)
		}
		defer l.Release()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := g.reload(); err != nil {
		g.stateErr = err
		return err
	}
	before, _ := json.Marshal(g.state)
	var previous State
	_ = json.Unmarshal(before, &previous)
	g.inTransaction = true
	defer func() { g.inTransaction = false }()
	err := fn()
	if err == nil {
		err = ctx.Err()
	}
	if err == nil {
		err = g.archiveCompletions(previous.Completed)
	}
	if err == nil {
		err = g.writeState()
	}
	if err != nil {
		g.state = State{}
		_ = json.Unmarshal(before, &g.state)
		normalizeState(&g.state)
		g.stateErr = err
	}
	return err
}
func (g *Gate) stateSnapshot() (State, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if err := g.reload(); err != nil {
		return State{}, err
	}
	b, err := json.Marshal(g.state)
	if err != nil {
		return State{}, err
	}
	var out State
	err = json.Unmarshal(b, &out)
	return out, err
}

// Archived receipts preserve idempotency indefinitely without making every
// admission replay history. They are transaction receipts, not usage counters.
func (g *Gate) receiptPath(id string) string {
	h := sha256.Sum256([]byte(id))
	name := hex.EncodeToString(h[:])
	return filepath.Join(g.cfg.StatePath+".receipts", name[:2], name+".json")
}
func (g *Gate) completion(id string) (Completion, bool, error) {
	if c, ok := g.state.Completed[id]; ok {
		return c, true, nil
	}
	if g.cfg.StatePath == "" {
		return Completion{}, false, nil
	}
	b, err := readBounded(g.receiptPath(id), 65536)
	if os.IsNotExist(err) {
		return Completion{}, false, nil
	}
	if err != nil {
		return Completion{}, false, errors.New("llmbudget: archived receipt unreadable")
	}
	var c Completion
	if json.Unmarshal(b, &c) != nil || c.Request.ID != id {
		return Completion{}, false, errors.New("llmbudget: invalid archived receipt")
	}
	return c, true, nil
}
func (g *Gate) archiveCompletions(committed map[string]Completion) error {
	if g.cfg.StatePath == "" || len(g.state.Completed) <= 128 {
		return nil
	}
	for id, c := range g.state.Completed {
		if _, ok := committed[id]; !ok {
			continue
		}
		if err := atomicJSON(g.receiptPath(id), c); err != nil {
			return err
		}
		if err := g.publishBoundary("after-receipt"); err != nil {
			return err
		}
		delete(g.state.Completed, id)
		if len(g.state.Completed) <= 64 {
			break
		}
	}
	return nil
}

func stateDigest(s State) string {
	s.Integrity = ""
	b, _ := json.Marshal(s)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// Short acquisition attempts retain lockfile's kernel arbitration while honoring
// cancellation between attempts. Legacy Record has a five-second overall bound.
func acquireMeter(ctx context.Context, path string) (*lockfile.Lock, error) {
	deadline := time.Now().Add(5 * time.Second)
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		l, err := lockfile.AcquireWithin(path, 20*time.Millisecond, lockfile.Holder{Name: "llmbudget", Intent: "meter transaction"})
		if !errors.Is(err, lockfile.ErrHeld) || !time.Now().Before(deadline) {
			return l, err
		}
	}
}

func (g *Gate) publishBoundary(stage string) error {
	if g.publishFault != nil {
		return g.publishFault(stage)
	}
	return nil
}
