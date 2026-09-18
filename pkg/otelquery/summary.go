package otelquery

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

const (
	rowLimit     = 200
	displayLimit = 10
)

func (c *Client) Guessed(ctx context.Context, opts Options) (Envelope, error) {
	// Provenance emits a span NAMED value.<name> carrying an event whose value.source says
	// where the number came from. The store buries the event attr at
	// event:event_attr:value.source:0, so we cannot filter it in LogsQL by that logical name --
	// we select by span name (clean, top-level) and keep the guesses in code.
	query := `name:value.*`
	rows, queryURL, err := c.logRows(ctx, "/traces/select/logsql/query", query, opts.Since, rowLimit+1)
	if err != nil {
		return Envelope{}, err
	}
	items := make([]SummaryItem, 0, len(rows))
	for _, r := range capRows(rows, rowLimit) {
		src := field(r, "value.source")
		if src != "estimate" && !strings.HasPrefix(src, "GUESS") {
			continue // ran on a real value, not a guess -- not what this verb reports
		}
		items = append(items, SummaryItem{
			Key:       joinNonEmpty(field(r, "value.source"), field(r, "value.name")),
			TraceID:   field(r, "trace_id", "traceId"),
			SpanID:    field(r, "span_id", "spanId"),
			Service:   field(r, "service.name", "service"),
			Span:      field(r, "span.name", "name"),
			Event:     field(r, "event.name", "event"),
			Source:    field(r, "value.source"),
			ValueName: field(r, "value.name"),
			Amount:    field(r, "value.amount"),
			Count:     1,
		})
	}
	items = topItems(groupItems(items, func(i SummaryItem) string {
		return joinNonEmpty(i.Source, i.ValueName, i.Service)
	}), displayLimit)
	return env("guessed", "traces", queryURL, query, len(rows), items)
}

func (c *Client) Bounds(ctx context.Context, opts Options) (Envelope, error) {
	// BoundHit emits a span NAMED bound.<kind> with span_attr:bound.was_hit=true and an event
	// carrying the limit/actual. Select by span name; the details resolve via schema-aware field.
	query := `name:bound.*`
	rows, queryURL, err := c.logRows(ctx, "/traces/select/logsql/query", query, opts.Since, rowLimit+1)
	if err != nil {
		return Envelope{}, err
	}
	items := make([]SummaryItem, 0, len(rows))
	for _, r := range capRows(rows, rowLimit) {
		items = append(items, SummaryItem{
			Key:     joinNonEmpty(field(r, "bound.kind"), field(r, "bound.limit"), field(r, "bound.actual")),
			TraceID: field(r, "trace_id", "traceId"),
			SpanID:  field(r, "span_id", "spanId"),
			Service: field(r, "service.name", "service"),
			Span:    field(r, "span.name", "name"),
			Event:   field(r, "event.name", "event"),
			Kind:    field(r, "bound.kind"),
			Limit:   field(r, "bound.limit"),
			Actual:  field(r, "bound.actual"),
			Count:   1,
		})
	}
	items = topItems(groupItems(items, func(i SummaryItem) string {
		return joinNonEmpty(i.Kind, "limit="+i.Limit, "actual="+i.Actual)
	}), displayLimit)
	return env("bounds", "traces", queryURL, query, len(rows), items)
}

func (c *Client) Failed(ctx context.Context, opts Options) (Envelope, error) {
	// cmd.exit_code lives at span_attr:cmd.exit_code, which LogsQL will not match by the bare
	// name -- select all exec spans and keep the failures in code (exit != 0), below.
	query := `name:exec*`
	rows, queryURL, err := c.logRows(ctx, "/traces/select/logsql/query", query, opts.Since, rowLimit+1)
	if err != nil {
		return Envelope{}, err
	}
	items := make([]SummaryItem, 0, len(rows))
	for _, r := range capRows(rows, rowLimit) {
		exit := field(r, "cmd.exit_code")
		if exit == "" || exit == "0" {
			continue
		}
		items = append(items, SummaryItem{
			Key:        joinNonEmpty(field(r, "cmd.name"), "exit "+exit, field(r, "cwd")),
			TraceID:    field(r, "trace_id", "traceId"),
			SpanID:     field(r, "span_id", "spanId"),
			Service:    field(r, "service.name", "service"),
			Span:       field(r, "span.name", "name"),
			Command:    field(r, "cmd.name", "command"),
			ExitCode:   exit,
			CWD:        field(r, "cwd", "process.cwd"),
			Principal:  field(r, "agent.principal"),
			DurationMS: number(firstAny(r, "duration_ms")) + number(field(r, "duration"))/1000,
			Count:      1,
		})
	}
	items = topItems(groupItems(items, func(i SummaryItem) string {
		return joinNonEmpty(i.Command, "exit "+i.ExitCode, i.CWD)
	}), displayLimit)
	e := mustEnv("failed", "traces", queryURL, query, len(items), items)
	e.TotalMatches = len(rows)
	e.Truncated = len(rows) > rowLimit || len(items) > displayLimit
	return e, nil
}

func (c *Client) Cost(ctx context.Context, opts Options) (Envelope, error) {
	query := `sum(ycode.llm.cost.dollars) by (model,pricing_known)`
	if opts.Suspect {
		query = `sum(ycode.llm.cost.dollars{pricing_known="false"}) by (model,pricing_known)`
	}
	costs, queryURL, err := c.metricQuery(ctx, query)
	if err != nil {
		return Envelope{}, err
	}
	tokens, _, _ := c.metricQuery(ctx, `sum(ycode.llm.tokens.total) by (model)`)
	items := make([]SummaryItem, 0, len(costs))
	tokenByModel := map[string]float64{}
	for _, t := range tokens {
		tokenByModel[t.Labels["model"]] += t.Value
	}
	for _, s := range costs {
		model := s.Labels["model"]
		items = append(items, SummaryItem{
			Key:          joinNonEmpty(model, "pricing_known="+s.Labels["pricing_known"]),
			Model:        model,
			PricingKnown: s.Labels["pricing_known"],
			CostUSD:      s.Value,
			Tokens:       tokenByModel[model],
			Count:        1,
		})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].CostUSD > items[j].CostUSD })
	if len(items) > displayLimit {
		items = items[:displayLimit]
	}
	e := mustEnv("cost", "metrics", queryURL, query, len(costs), items)
	e.Metrics = costs
	return e, nil
}

func (c *Client) WhySlow(ctx context.Context, traceID string, opts Options) (Envelope, error) {
	rows, queryURL, err := c.jaegerTrace(ctx, traceID)
	if err != nil {
		return Envelope{}, err
	}
	items := make([]SummaryItem, 0, len(rows))
	total := 0.0
	bound := 0.0
	for _, r := range rows {
		name := field(r, "operationName", "name", "span.name")
		d := number(firstAny(r, "duration_ms", "duration"))
		if d > 1000000 {
			d /= 1000
		}
		total += d
		item := SummaryItem{
			Key:        name,
			TraceID:    traceID,
			SpanID:     field(r, "spanID", "span_id", "spanId"),
			Span:       name,
			DurationMS: d,
			Kind:       boundKindFromJaeger(r),
			Count:      1,
		}
		if item.Kind != "" {
			bound += d
		}
		items = append(items, item)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].DurationMS > items[j].DurationMS })
	top := items
	if len(top) > displayLimit {
		top = top[:displayLimit]
	}
	var bounds []SummaryItem
	for _, i := range items {
		if i.Kind != "" {
			bounds = append(bounds, i)
		}
	}
	if len(bounds) > displayLimit {
		bounds = bounds[:displayLimit]
	}
	e := mustEnv("why-slow", "traces", queryURL, "", len(rows), top)
	e.Trace = &TraceSummary{
		TraceID: traceID, DurationMS: total, BoundWaitMS: bound, WorkMS: total - bound,
		SpanCount: len(rows), TopSpans: top, BoundSpans: bounds,
	}
	e.Summary = fmt.Sprintf("%d spans; %.0fms total, %.0fms bound waits, %.0fms work", len(rows), total, bound, total-bound)
	return e, nil
}

func env(verb, backend, queryURL, query string, total int, items []SummaryItem) (Envelope, error) {
	return mustEnv(verb, backend, queryURL, query, total, items), nil
}

func mustEnv(verb, backend, queryURL, query string, total int, items []SummaryItem) Envelope {
	shown := len(items)
	e := Envelope{
		SchemaVersion: SchemaVersion,
		Verb:          verb, Backend: backend, QueryURL: queryURL, Query: query,
		TotalMatches: total, Shown: shown, Truncated: total > rowLimit || total > shown,
		Items: items,
	}
	if total == 0 {
		e.Summary = "0 matches"
	} else if e.Truncated {
		e.Summary = fmt.Sprintf("%d matches, showing the %d worst", total, shown)
	} else {
		e.Summary = fmt.Sprintf("%d matches", total)
	}
	return e
}

func capRows(rows []map[string]any, n int) []map[string]any {
	if len(rows) > n {
		return rows[:n]
	}
	return rows
}

func groupItems(items []SummaryItem, key func(SummaryItem) string) []SummaryItem {
	m := map[string]SummaryItem{}
	for _, item := range items {
		k := key(item)
		if k == "" {
			k = "(unknown)"
		}
		if prev, ok := m[k]; ok {
			prev.Count++
			if item.DurationMS > prev.DurationMS {
				prev.DurationMS = item.DurationMS
			}
			m[k] = prev
			continue
		}
		item.Key = k
		item.Count = 1
		m[k] = item
	}
	out := make([]SummaryItem, 0, len(m))
	for _, item := range m {
		out = append(out, item)
	}
	return out
}

func topItems(items []SummaryItem, n int) []SummaryItem {
	sort.Slice(items, func(i, j int) bool {
		if items[i].Count != items[j].Count {
			return items[i].Count > items[j].Count
		}
		return items[i].DurationMS > items[j].DurationMS
	})
	if len(items) > n {
		return items[:n]
	}
	return items
}

func firstAny(r map[string]any, names ...string) any {
	for _, n := range names {
		if v, ok := r[n]; ok {
			return v
		}
	}
	return nil
}

func joinNonEmpty(parts ...string) string {
	var out []string
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" && p != "limit=" && p != "actual=" && p != "pricing_known=" {
			out = append(out, p)
		}
	}
	return strings.Join(out, " ")
}

func boundKindFromJaeger(r map[string]any) string {
	for _, tagList := range []string{"tags", "process.tags"} {
		tags, _ := r[tagList].([]any)
		for _, raw := range tags {
			tag, _ := raw.(map[string]any)
			if field(tag, "key") == "bound.kind" {
				return field(tag, "value")
			}
		}
	}
	return field(r, "bound.kind")
}
