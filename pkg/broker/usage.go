package broker

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"time"
)

// UsageRollupOptions selects the records included by RollupUsage. Since is
// inclusive. Principal empty includes every principal.
type UsageRollupOptions struct {
	Since     time.Time
	Principal string
}

// PrincipalUsage is the aggregate usage of one authenticated door principal.
// It is the shared representation used by the CLI and by enforcement code.
type PrincipalUsage struct {
	Principal        string  `json:"principal"`
	Requests         uint64  `json:"requests"`
	PromptTokens     uint64  `json:"prompt_tokens"`
	CompletionTokens uint64  `json:"completion_tokens"`
	CachedTokens     uint64  `json:"cached_tokens"`
	CacheWriteTokens uint64  `json:"cache_write_tokens"`
	CostUSD          float64 `json:"cost_usd"`
	Unpriced         bool    `json:"unpriced"`
	// PricedTokens is the prompt+completion total of the records the price
	// table priced (unpriced false). UnpricedTokens is the same total for
	// the records with no price (unpriced true). The two always add up to
	// PromptTokens+CompletionTokens, and they split a principal that mixes
	// priced and unpriced models, which the OR-ed Unpriced flag alone
	// cannot show.
	PricedTokens   uint64 `json:"priced_tokens"`
	UnpricedTokens uint64 `json:"unpriced_tokens"`
}

// RollupUsage reads door audit JSONL and returns one row per principal, sorted
// by principal. Unknown record fields are ignored so older and newer audit
// schemas can be rolled up by the same report.
func RollupUsage(r io.Reader, opts UsageRollupOptions) ([]PrincipalUsage, error) {
	type usageRecord struct {
		Time       time.Time `json:"time"`
		Principal  string    `json:"principal"`
		Prompt     int64     `json:"prompt_tokens"`
		Completion int64     `json:"completion_tokens"`
		Cached     int64     `json:"cached_tokens"`
		CacheWrite int64     `json:"cache_write_tokens"`
		CostUSD    float64   `json:"cost_usd"`
		Unpriced   bool      `json:"unpriced"`
	}

	byPrincipal := make(map[string]*PrincipalUsage)
	s := bufio.NewScanner(r)
	// Options and identities can make an audit row larger than Scanner's 64K
	// default. Keep reporting useful without imposing a small artificial cap.
	s.Buffer(make([]byte, 64<<10), 4<<20)
	line := 0
	for s.Scan() {
		line++
		data := bytes.TrimSpace(s.Bytes())
		if len(data) == 0 {
			continue
		}
		var rec usageRecord
		if err := json.Unmarshal(data, &rec); err != nil {
			return nil, fmt.Errorf("broker: audit line %d: %w", line, err)
		}
		if rec.Principal == "" || (opts.Principal != "" && rec.Principal != opts.Principal) || (!opts.Since.IsZero() && rec.Time.Before(opts.Since)) {
			continue
		}
		if rec.Prompt < 0 || rec.Completion < 0 || rec.Cached < 0 || rec.CacheWrite < 0 || rec.CostUSD < 0 {
			return nil, fmt.Errorf("broker: audit line %d: usage values must not be negative", line)
		}
		row := byPrincipal[rec.Principal]
		if row == nil {
			row = &PrincipalUsage{Principal: rec.Principal}
			byPrincipal[rec.Principal] = row
		}
		tokens := uint64(rec.Prompt + rec.Completion)
		row.Requests++
		row.PromptTokens += uint64(rec.Prompt)
		row.CompletionTokens += uint64(rec.Completion)
		row.CachedTokens += uint64(rec.Cached)
		row.CacheWriteTokens += uint64(rec.CacheWrite)
		row.CostUSD += rec.CostUSD
		row.Unpriced = row.Unpriced || rec.Unpriced
		if rec.Unpriced {
			row.UnpricedTokens += tokens
		} else {
			row.PricedTokens += tokens
		}
	}
	if err := s.Err(); err != nil {
		return nil, fmt.Errorf("broker: read audit: %w", err)
	}

	rows := make([]PrincipalUsage, 0, len(byPrincipal))
	for _, row := range byPrincipal {
		rows = append(rows, *row)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Principal < rows[j].Principal })
	return rows, nil
}
