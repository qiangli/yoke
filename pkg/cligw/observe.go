package cligw

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/qiangli/yoke/pkg/agentlaunch"
	"github.com/qiangli/yoke/pkg/quotaobs"
)

// Only inspect vendor output, never the prompt supplied to the worker.
func (w *Worker) observeLimit(ctx context.Context, data []byte) {
	var event struct {
		Type       string            `json:"type"`
		UsageLimit json.RawMessage   `json:"usage_limit"`
		Status     int               `json:"status"`
		StatusCode int               `json:"status_code"`
		Headers    map[string]string `json:"headers"`
	}
	_ = json.Unmarshal(data, &event)
	if event.Type == "user" || event.Type == "input" {
		return
	}
	matched := event.Type == "usage_limit" || (len(event.UsageLimit) > 0 && string(event.UsageLimit) != "null" && string(event.UsageLimit) != `""`)
	status := event.StatusCode
	if status == 0 {
		status = event.Status
	}
	matched = matched || status == 429 || status == 402 || status == 403
	lower := strings.ToLower(string(data))
	for _, signal := range []string{"usage limit", "rate limit", "rate_limit", "rate-limited", "quota", "too many requests", "weekly limit", "daily limit", "limit reached", "try again later", "resource_exhausted", "http 429", "http 402", "http 403"} {
		matched = matched || strings.Contains(lower, signal)
	}
	if !matched {
		return
	}
	// Headers have a separate allowlisted field; never persist an auth header
	// embedded in a CLI's HTTP error envelope.
	var object map[string]json.RawMessage
	if json.Unmarshal(data, &object) == nil && object != nil {
		delete(object, "headers")
		delete(object, "request")
		delete(object, "request_body")
		data, _ = json.Marshal(object)
	}
	o := quotaobs.Observation{Source: "cligw", Tool: w.tool.Name, Model: w.launch.ModelName, Agent: w.agent, Status: status, Headers: event.Headers, Excerpt: string(data)}
	if m, ok := agentlaunch.NewCatalog().Model(o.Model); ok {
		o.Plan = m.Plan
	}
	quotaobs.RecordBestEffort(ctx, o)
}
