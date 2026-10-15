package fleet

import (
	"fmt"
	"strings"
	"time"

	"github.com/itchyny/gojq"
	"github.com/qiangli/yoke/pkg/snippet"
)

type UsageProbe struct {
	ID      string           `yaml:"id" json:"id" doc:"unique probe id on this record"`
	Refresh string           `yaml:"refresh,omitempty" json:"refresh,omitempty" doc:"refresh interval, default 5m"`
	Run     snippet.Snippet  `yaml:"run" json:"run" doc:"Bash# usage probe"`
	Metrics []UsageMetricMap `yaml:"metrics,omitempty" json:"metrics,omitempty" doc:"JSON output mapping"`
}

type UsageMetricMap struct {
	Name   string `yaml:"name" json:"name" doc:"metric name from the usage vocabulary"`
	Value  string `yaml:"value" json:"value" doc:"jq expression for the numeric value"`
	Unit   string `yaml:"unit,omitempty" json:"unit,omitempty" doc:"literal unit or jq expression"`
	Window string `yaml:"window,omitempty" json:"window,omitempty" doc:"usage window"`
	Reset  string `yaml:"reset,omitempty" json:"reset,omitempty" doc:"jq expression for reset time"`
}

func (p UsageProbe) RefreshDuration() time.Duration {
	if p.Refresh != "" {
		if d, err := time.ParseDuration(p.Refresh); err == nil {
			return d
		}
	}
	if strings.Contains(p.Run.Body, "agentic") {
		return time.Hour
	}
	return 5 * time.Minute
}

func validateUsage(probes []UsageProbe) error {
	seen := map[string]bool{}
	for _, p := range probes {
		if p.ID == "" || seen[p.ID] || strings.ContainsAny(p.ID, "/\\:\x00") {
			return fmt.Errorf("invalid or duplicate usage probe id %q", p.ID)
		}
		seen[p.ID] = true
		if p.Refresh != "" {
			d, err := time.ParseDuration(p.Refresh)
			if err != nil || d <= 0 {
				return fmt.Errorf("probe %q: invalid refresh", p.ID)
			}
		}
		if p.Run.Body == "" {
			return fmt.Errorf("probe %q: empty run", p.ID)
		}
		for _, m := range p.Metrics {
			switch m.Name {
			case "used_percent", "used", "limit", "remaining", "balance", "tokens_in", "tokens_out":
			default:
				return fmt.Errorf("probe %q: unknown metric %q", p.ID, m.Name)
			}
			switch m.Window {
			case "", "5h", "day", "week", "month":
			default:
				return fmt.Errorf("probe %q: unknown window %q", p.ID, m.Window)
			}
			for _, expr := range []string{m.Value, m.Reset} {
				if expr == "" {
					continue
				}
				q, err := gojq.Parse(expr)
				if err != nil {
					return fmt.Errorf("probe %q: invalid jq: %w", p.ID, err)
				}
				if _, err = gojq.Compile(q); err != nil {
					return fmt.Errorf("probe %q: invalid jq: %w", p.ID, err)
				}
			}
			if m.Value == "" {
				return fmt.Errorf("probe %q: empty value", p.ID)
			}
			if strings.HasPrefix(m.Unit, ".") {
				q, err := gojq.Parse(m.Unit)
				if err != nil {
					return fmt.Errorf("probe %q: invalid unit jq: %w", p.ID, err)
				}
				if _, err = gojq.Compile(q); err != nil {
					return fmt.Errorf("probe %q: invalid unit jq: %w", p.ID, err)
				}
			}
		}
	}
	return nil
}
