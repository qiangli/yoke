package llmbudget

import (
	"context"
	"sort"
	"time"

	"github.com/qiangli/yoke/pkg/quotaobs"
)

type LogFilter struct {
	Account string
	Since   time.Time
	Limit   int
}

type QuotaLogFilter = LogFilter

type LogEntry struct {
	At             time.Time              `json:"at"`
	Source         string                 `json:"source"`
	Account        string                 `json:"account"`
	Tool           string                 `json:"tool,omitempty"`
	Model          string                 `json:"model,omitempty"`
	Excerpt        string                 `json:"excerpt"`
	Parsed         *quotaobs.ParsedSignal `json:"parsed,omitempty"`
	Analysis       string                 `json:"analysis,omitempty"`
	Classification string                 `json:"classification,omitempty"`
}

// QuotaLog returns newest journal entries with their cached interpretations.
// It never invokes the model.
func QuotaLog(ctx context.Context, filter LogFilter) []LogEntry {
	obs, err := quotaobs.Read(filter.Since, func(o quotaobs.Observation) bool {
		return filter.Account == "" || o.Account == filter.Account || o.Plan == filter.Account
	})
	if err != nil {
		return nil
	}
	cache, _ := readAnnotations()
	entries := make([]LogEntry, 0, len(obs))
	for _, o := range obs {
		if ctx.Err() != nil {
			break
		}
		account := o.Account
		if account == "" {
			account = o.Plan
		}
		e := LogEntry{At: o.At, Source: o.Source, Account: account, Tool: o.Tool, Model: o.Model, Excerpt: o.Excerpt, Parsed: o.Parsed}
		if e.Parsed == nil {
			e.Parsed = cache[o.ExcerptSHA+"\x00"].Parsed
		}
		analysis := o.Analysis
		for _, a := range cache {
			if a.ExcerptSHA == o.ExcerptSHA && a.Analysis != nil {
				analysis = a.Analysis
				e.Analysis = a.AccountSummary
				break
			}
		}
		if e.Parsed != nil {
			e.Classification = "observed-unconfirmed"
		}
		if analysis != nil {
			if e.Analysis == "" {
				e.Analysis = analysis.Summary
			}
			e.Classification = "inferred"
			if e.Parsed != nil {
				e.Classification = "conflict"
				if e.Parsed.Kind == analysis.Kind && resetsAgree(e.Parsed.ResetAt, analysis.ResetAt) {
					e.Classification = "observed"
				}
			}
		}
		entries = append(entries, e)
	}
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].At.After(entries[j].At) })
	if filter.Limit > 0 && len(entries) > filter.Limit {
		entries = entries[:filter.Limit]
	}
	return entries
}
