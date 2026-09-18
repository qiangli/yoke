package llmbudget

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"time"
)

// ClaudeStatuslineAdapter reads an explicitly installed bridge snapshot. The
// source is the documented statusline stdin schema, not a private transcript:
// https://code.claude.com/docs/en/statusline
// Current-context totals must never be accumulated as lifetime token usage.
type ClaudeStatuslineAdapter struct{}

func (ClaudeStatuslineAdapter) Kind() string { return "claude-statusline" }
func (ClaudeStatuslineAdapter) Collect(ctx context.Context, c SourceConfig, now time.Time) (SourceResult, error) {
	result := SourceResult{Status: "partial", Metrics: []Metric{}, Limitations: []string{"Local Claude statusline observation; account association is explicitly configured. Session cost is estimated, not account billing."}}
	fail := func() (SourceResult, error) {
		result.Status = "unavailable"
		result.Metrics = nil
		return result, errors.New("invalid or unavailable Claude bridge snapshot")
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if c.Path == "" {
		return fail()
	}
	raw, err := readBounded(c.Path, 256<<10)
	if err != nil {
		return fail()
	}
	var envelope struct {
		SchemaVersion string    `json:"schema_version"`
		ObservedAt    time.Time `json:"observed_at"`
		Account       string    `json:"account"`
		Payload       struct {
			Version string `json:"version"`
			Cost    struct {
				Total *float64 `json:"total_cost_usd"`
			} `json:"cost"`
			Context struct {
				Remaining *float64 `json:"remaining_percentage"`
				Current   *struct {
					Input  *float64 `json:"input_tokens"`
					Output *float64 `json:"output_tokens"`
					Read   *float64 `json:"cache_read_input_tokens"`
					Write  *float64 `json:"cache_creation_input_tokens"`
				} `json:"current_usage"`
			} `json:"context_window"`
			Rate map[string]struct {
				Used  *float64 `json:"used_percentage"`
				Reset *int64   `json:"resets_at"`
			} `json:"rate_limits"`
		} `json:"payload"`
	}
	if json.Unmarshal(raw, &envelope) != nil || envelope.SchemaVersion != "bashy-claude-statusline-v1" || envelope.Account != c.Account || envelope.ObservedAt.IsZero() || envelope.ObservedAt.After(now.Add(time.Minute)) {
		return fail()
	}
	at := envelope.ObservedAt
	add := func(name, unit, class string, value *float64) bool {
		if value == nil {
			return true
		}
		if math.IsNaN(*value) || math.IsInf(*value, 0) || *value < 0 || unit == "tokens" && (*value != math.Trunc(*value) || *value > 1<<53) {
			return false
		}
		result.Metrics = append(result.Metrics, measured(name, *value, unit, class, "claude-statusline", at))
		return true
	}
	if !add("billing.spend", "usd", "estimated", envelope.Payload.Cost.Total) {
		return fail()
	}
	if v := envelope.Payload.Context.Remaining; v != nil {
		if *v > 100 || !add("usage.context_remaining_percent", "percent", "actual", v) {
			return fail()
		}
	}
	if current := envelope.Payload.Context.Current; current != nil {
		for _, v := range []struct {
			name string
			p    *float64
		}{{"context.input_tokens", current.Input}, {"context.output_tokens", current.Output}, {"context.cached_input_tokens", current.Read}, {"context.cache_write_tokens", current.Write}} {
			if !add(v.name, "tokens", "actual", v.p) {
				return fail()
			}
		}
	}
	for _, window := range []string{"five_hour", "seven_day", "spend_limit"} {
		v, ok := envelope.Payload.Rate[window]
		if !ok || v.Used == nil {
			continue
		}
		if *v.Used < 0 || math.IsNaN(*v.Used) || math.IsInf(*v.Used, 0) {
			return fail()
		}
		m := measured("quota.used_percent", *v.Used, "percent", "actual", "claude-statusline:"+window, at)
		if v.Reset != nil {
			if *v.Reset < 0 {
				return fail()
			}
			t := time.Unix(*v.Reset, 0).UTC()
			m.ResetAt = &t
		}
		result.Metrics = append(result.Metrics, m)
	}
	if len(result.Metrics) == 0 {
		result.Status = "unavailable"
		result.Limitations = append(result.Limitations, "Harness has not emitted any supported metrics.")
	}
	maxAge := 10 * time.Minute
	if c.RefreshSeconds > 0 {
		maxAge = max(2*time.Duration(c.RefreshSeconds)*time.Second, time.Minute)
	}
	if now.Sub(at) > maxAge {
		return staleResult(result, "Bridge source timestamp is stale."), nil
	}
	return result, nil
}
