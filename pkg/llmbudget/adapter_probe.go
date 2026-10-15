package llmbudget

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/itchyny/gojq"
	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/secrets"
	"github.com/qiangli/yoke/pkg/snippet"
)

func init() {
	snippet.SetModelCredentialResolver(func(_ context.Context, ref snippet.ModelRef) (string, error) {
		cat := fleet.New()
		resolve := func(m fleet.Model) (string, bool) {
			credentialRef := (fleet.Tool{}).CredentialRefFor(m)
			if credentialRef == "" {
				return "", false
			}
			kv, ok := secrets.GrantAgentKey(os.Environ(), credentialRef)
			if !ok {
				return "", false
			}
			_, value, ok := strings.Cut(kv, "=")
			return value, ok && value != ""
		}
		if ref.Model != "" {
			if m, ok := cat.Model(ref.Model); ok && (ref.Provider == "" || strings.EqualFold(m.Provider, ref.Provider)) && (ref.Plan == "" || m.Plan == ref.Plan) {
				if v, ok := resolve(m); ok {
					return v, nil
				}
			}
			return "", snippet.ErrNotConfigured
		}
		if ref.Plan == "" {
			return "", snippet.ErrNotConfigured
		}
		models, _ := cat.Models()
		for _, m := range models {
			if m.Plan == ref.Plan && m.Active() {
				if v, ok := resolve(m); ok {
					return v, nil
				}
			}
		}
		return "", snippet.ErrNotConfigured
	})
}

func synthesizedProbes(p *Policy) []SourceConfig {
	cat := fleet.New()
	models, _ := cat.Models()
	plans, _ := cat.Plans()
	byPlan := map[string]fleet.Plan{}
	for _, plan := range plans {
		byPlan[plan.Name] = plan
	}
	seen := map[string]bool{}
	allowed := map[string]bool{}
	for _, id := range p.ProbeConsent {
		allowed[id] = true
	}
	for _, s := range p.Sources {
		seen[s.ID] = true
	}
	var out []SourceConfig
	for _, m := range models {
		if !m.Active() {
			continue
		}
		add := func(kind, name string, probe fleet.UsageProbe, provider, account string, lane Lane) {
			id := "probe:" + kind + ":" + name + "/" + probe.ID
			if seen[id] {
				return
			}
			seen[id] = true
			if provider == "" {
				provider = m.Provider
			}
			if account == "" {
				account = "unresolved"
			}
			if m.Kind == fleet.ModelKindLocal {
				lane = LaneLocal
			}
			d := probe.RefreshDuration()
			timeout := 5
			if strings.Contains(probe.Run.Body, "agentic") {
				timeout = 30
			}
			c := SourceConfig{ID: id, Kind: "probe", Provider: provider, Account: account, Pool: account, Lane: lane, Enabled: !snippet.NeedsConsent(probe.Run) || allowed[id], RefreshSeconds: int(d.Seconds()), probe: &probe, probeModel: m.Name, probePlan: m.Plan}
			c.TimeoutSeconds = timeout
			out = append(out, c)
		}
		for _, probe := range m.Usage {
			add("model", m.Name, probe, m.Provider, m.APIKeyRef, LaneAPIKey)
		}
		if plan, ok := byPlan[m.Plan]; ok {
			for _, probe := range plan.Usage {
				provider := m.Provider
				if provider == "" {
					provider = strings.ToLower(plan.Vendor)
				}
				add("plan", plan.Name, probe, provider, plan.Name, LaneSubscription)
			}
		}
	}
	return out
}

type ProbeAdapter struct{}

func (ProbeAdapter) Kind() string { return "probe" }
func (ProbeAdapter) Collect(ctx context.Context, c SourceConfig, now time.Time) (SourceResult, error) {
	if c.probe == nil {
		return SourceResult{Status: "not-configured"}, nil
	}
	provider := c.Provider
	model := c.probeModel
	if strings.HasPrefix(c.ID, "probe:plan:") {
		provider = ""
		model = ""
	}
	ctx = snippet.WithModel(ctx, provider, model, c.probePlan)
	result, err := snippet.Run(ctx, c.probe.Run, snippet.IO{})
	if err != nil {
		if errors.Is(err, snippet.ErrNotConfigured) {
			return SourceResult{Status: "not-configured"}, nil
		}
		if result.Exit == 6 {
			return SourceResult{Status: "needs-input", Limitations: []string{err.Error()}}, nil
		}
		return SourceResult{Status: "unavailable", Limitations: []string{err.Error()}}, err
	}
	if result.Exit == 6 {
		return SourceResult{Status: "needs-input", Limitations: []string{"Probe needs input."}}, nil
	}
	if result.Exit != 0 {
		return SourceResult{Status: "unavailable", Limitations: []string{fmt.Sprintf("probe exited %d", result.Exit)}}, nil
	}
	var doc any
	if err := json.Unmarshal(result.Stdout, &doc); err != nil {
		return SourceResult{Status: "unavailable"}, fmt.Errorf("probe JSON: %w", err)
	}
	classification := "measured"
	if strings.Contains(c.probe.Run.Body, "agentic") {
		classification = "inferred"
	}
	if field := reflect.ValueOf(result).FieldByName("Classification"); field.IsValid() && field.Kind() == reflect.String && field.String() != "" {
		classification = field.String()
	}
	out := SourceResult{Status: "ok"}
	appendMetric := func(name string, value any, unit, window string, reset any) error {
		f, ok := number(value)
		if !ok || math.IsNaN(f) || math.IsInf(f, 0) {
			return fmt.Errorf("probe metric %s is not numeric", name)
		}
		metricName := "usage." + name
		if name == "used_percent" {
			metricName = "quota.used_percent"
		}
		metric := Metric{Name: metricName, Value: &f, Unit: unit, Window: window, Classification: classification, Source: c.ID, ObservedAt: now}
		if t := resetTime(reset); t != nil {
			metric.ResetAt = t
			metric.WindowEnd = t
		}
		out.Metrics = append(out.Metrics, metric)
		return nil
	}
	if len(c.probe.Metrics) > 0 {
		for _, m := range c.probe.Metrics {
			v, e := jqValue(m.Value, doc)
			if e != nil {
				return SourceResult{Status: "unavailable"}, e
			}
			unit := m.Unit
			if strings.HasPrefix(unit, ".") {
				u, e := jqValue(unit, doc)
				if e != nil {
					return SourceResult{Status: "unavailable"}, e
				}
				unit = fmt.Sprint(u)
			}
			var reset any
			if m.Reset != "" {
				reset, e = jqValue(m.Reset, doc)
				if e != nil {
					return SourceResult{Status: "unavailable"}, e
				}
			}
			if e := appendMetric(m.Name, v, unit, m.Window, reset); e != nil {
				return SourceResult{Status: "unavailable"}, e
			}
		}
	} else {
		obj, ok := doc.(map[string]any)
		if !ok || obj["schema_version"] != "bashy-usage-v1" {
			return SourceResult{Status: "unavailable"}, errors.New("probe: expected bashy-usage-v1")
		}
		rows, ok := obj["metrics"].([]any)
		if !ok {
			return SourceResult{Status: "unavailable"}, errors.New("probe: metrics missing")
		}
		for _, raw := range rows {
			m, ok := raw.(map[string]any)
			if !ok {
				return SourceResult{Status: "unavailable"}, errors.New("probe: invalid metric")
			}
			name, _ := m["name"].(string)
			if e := appendMetric(name, m["value"], stringValue(m["unit"]), stringValue(m["window"]), m["reset_at"]); e != nil {
				return SourceResult{Status: "unavailable"}, e
			}
		}
	}
	return out, nil
}
func jqValue(expr string, doc any) (any, error) {
	q, e := gojq.Parse(expr)
	if e != nil {
		return nil, e
	}
	code, e := gojq.Compile(q)
	if e != nil {
		return nil, e
	}
	iter := code.Run(doc)
	v, ok := iter.Next()
	if !ok {
		return nil, nil
	}
	if e, ok := v.(error); ok {
		return nil, e
	}
	return v, nil
}
func number(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	case json.Number:
		f, e := n.Float64()
		return f, e == nil
	case string:
		f, e := strconv.ParseFloat(n, 64)
		return f, e == nil
	}
	return 0, false
}
func stringValue(v any) string { s, _ := v.(string); return s }
func resetTime(v any) *time.Time {
	if s, ok := v.(string); ok {
		if t, e := time.Parse(time.RFC3339, s); e == nil {
			return &t
		}
		v = s
	}
	f, ok := number(v)
	if !ok {
		return nil
	}
	if f > 1e12 {
		f /= 1000
	}
	t := time.Unix(int64(f), 0).UTC()
	return &t
}
