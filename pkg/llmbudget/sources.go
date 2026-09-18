package llmbudget

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/qiangli/coreutils/pkg/lockfile"
)

type cachedSource struct {
	Version  int          `json:"version"`
	At       time.Time    `json:"at"`
	Next     time.Time    `json:"next"`
	Failures int          `json:"failures"`
	Result   SourceResult `json:"result"`
}

func (g *Gate) source(ctx context.Context, c SourceConfig, now time.Time, refresh bool) (SourceResult, error) {
	if !c.Enabled {
		return SourceResult{Status: "unavailable", Limitations: []string{"Source is not explicitly enabled."}}, nil
	}
	var adapter Adapter
	for _, a := range g.cfg.Adapters {
		if a.Kind() == c.Kind {
			adapter = a
			break
		}
	}
	if adapter == nil {
		switch c.Kind {
		case "claude-statusline":
			adapter = ClaudeStatuslineAdapter{}
		case "openai-organization":
			adapter = OpenAIOrganizationAdapter{Resolve: g.cfg.ResolveCredential}
		}
	}
	if adapter == nil {
		return SourceResult{Status: "unsupported", Limitations: []string{"No supported adapter for this source kind."}}, nil
	}
	cadence := time.Duration(c.RefreshSeconds) * time.Second
	if cadence < time.Minute {
		cadence = 5 * time.Minute
	}
	timeout := time.Duration(c.TimeoutSeconds) * time.Second
	if timeout <= 0 || timeout > 30*time.Second {
		timeout = 5 * time.Second
	}
	var cached cachedSource
	path := ""
	if g.cfg.StatePath != "" {
		raw, _ := json.Marshal(c)
		hash := sha256.Sum256(raw)
		path = filepath.Join(filepath.Dir(g.cfg.StatePath), "llm-budget-sources", hex.EncodeToString(hash[:])+".json")
		read := func() {
			if b, err := readBounded(path, 1<<20); err == nil {
				_ = json.Unmarshal(b, &cached)
			}
		}
		read()
		// now is the report's captured observation time. A concurrent report
		// can publish a newer observation before this report reaches its second
		// source; that newer cache entry is reusable, not a clock rollback.
		usable := func() bool {
			authorityNow := g.now()
			return cached.Version == 1 && !cached.At.After(authorityNow) && authorityNow.Before(cached.Next)
		}
		// Explicit refresh respects cadence too: otherwise each manager bypasses
		// the shared throttle independently. It requests refresh at next eligibility.
		_ = refresh
		if usable() {
			return cached.Result, nil
		}
		l, err := lockfile.TryAcquire(path+".lock", lockfile.Holder{Name: "llmbudget-source", Intent: "refresh observation"})
		if errors.Is(err, lockfile.ErrHeld) {
			if cached.Version == 1 {
				return staleResult(cached.Result, "Another process is refreshing this source."), nil
			}
			return SourceResult{Status: "unavailable", Limitations: []string{"Initial source refresh is in progress."}}, nil
		}
		if err != nil {
			return SourceResult{Status: "unavailable"}, err
		}
		defer l.Release()
		read()
		if usable() {
			return cached.Result, nil
		}
	}
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	result, err := adapter.Collect(callCtx, c, now)
	if err != nil {
		cached.Failures++
		backoff := time.Minute * time.Duration(1<<min(cached.Failures, 5))
		if backoff > cadence {
			cadence = backoff
		}
		if cached.Version == 1 && len(cached.Result.Metrics) > 0 {
			old := staleResult(cached.Result, "Refresh failed; values are from an earlier successful observation.")
			old.RetryAt = result.RetryAt
			result = old
		}
		if result.Status == "" {
			result.Status = "unavailable"
		}
	} else {
		cached.Failures = 0
	}
	next := now.Add(cadence)
	if result.RetryAt != nil && result.RetryAt.After(next) {
		next = *result.RetryAt
	}
	cached.Version = 1
	cached.At = now
	cached.Next = next
	cached.Result = result
	if path != "" {
		if e := atomicJSON(path, cached); e != nil && err == nil {
			err = e
		}
	}
	return result, err
}
func staleResult(s SourceResult, why string) SourceResult {
	s.Status = "stale"
	s.Metrics = append([]Metric(nil), s.Metrics...)
	for i := range s.Metrics {
		if s.Metrics[i].Value != nil {
			s.Metrics[i].Classification = "stale"
		}
	}
	s.Limitations = append(append([]string(nil), s.Limitations...), why)
	return s
}
func explicitCredential(ctx context.Context, ref string, resolve CredentialResolver) (string, error) {
	if len(ref) > 4 && ref[:4] == "env:" {
		value := os.Getenv(ref[4:])
		if value != "" {
			return value, nil
		}
		return "", errors.New("credential unavailable")
	}
	if ref != "" && resolve != nil {
		return resolve(ctx, ref)
	}
	return "", errors.New("explicit credential reference required")
}
