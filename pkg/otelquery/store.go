package otelquery

import (
	"context"
	"time"
)

// The exported query surface.
//
// `bashy otel query` is one front door onto the telemetry store. It is not the only one that
// should exist: an agent debugging its own run wants to ask the same questions from inside a
// tool call, not by shelling out and parsing a table meant for a human.
//
// So the client is exported, and both front doors share it. One query path, two callers.

// Backend paths on the Victoria stores, fronted by the `bashy otel` proxy.
const (
	backendLogs   = "/logs/select/logsql/query"
	backendTraces = "/traces/select/logsql/query"
)

// Reachable reports whether the store is actually up and answering.
//
// This exists so a CALLER CAN TELL THE DIFFERENCE between "the store says nothing happened"
// and "I could not reach the store." Those are opposite facts, and a query layer that returns
// an empty slice for both is the absence-of-evidence bug wearing a helpful face: the caller
// reads "no results" and concludes nothing happened, when the truth is that nobody looked.
func (c *Client) Reachable(ctx context.Context) bool {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	_, err := c.get(ctx, c.BaseURL+"/metrics/api/v1/query?query=up")
	return err == nil
}

// Logs runs a LogsQL query against VictoriaLogs and returns the raw rows.
func (c *Client) Logs(ctx context.Context, query string, since time.Duration, limit int) ([]map[string]any, string, error) {
	return c.logRows(ctx, backendLogs, query, since, limit)
}

// Spans runs a LogsQL query against VictoriaTraces. Spans are stored as structured rows, so
// the same query language reaches them.
func (c *Client) Spans(ctx context.Context, query string, since time.Duration, limit int) ([]map[string]any, string, error) {
	return c.logRows(ctx, backendTraces, query, since, limit)
}

// Trace fetches one complete trace by ID, across every service that touched it.
//
// This is the query the file-based tools structurally CANNOT answer. A JSONL file holds one
// process's spans; a trace crosses processes. Asking "what happened in this request" of a
// local file gets you the part of the answer that happened to be written by the process you
// asked — which is not a smaller answer, it is a misleading one.
func (c *Client) Trace(ctx context.Context, traceID string) ([]map[string]any, string, error) {
	return c.jaegerTrace(ctx, traceID)
}

// Metrics runs a PromQL query against VictoriaMetrics.
func (c *Client) Metrics(ctx context.Context, promql string) ([]MetricSeries, string, error) {
	return c.metricQuery(ctx, promql)
}
