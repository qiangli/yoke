package otelquery

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

type Client struct {
	BaseURL    string
	HTTPClient *http.Client
}

func NewClient(baseURL string) *Client {
	if baseURL == "" {
		baseURL = os.Getenv("BASHY_OTEL_QUERY_URL")
	}
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	return &Client{
		BaseURL:    strings.TrimRight(baseURL, "/"),
		HTTPClient: &http.Client{Timeout: 10 * time.Second},
	}
}

func (c *Client) logRows(ctx context.Context, backendPath, query string, since time.Duration, limit int) ([]map[string]any, string, error) {
	u, err := url.Parse(c.BaseURL + backendPath)
	if err != nil {
		return nil, "", err
	}
	q := u.Query()
	q.Set("query", query)
	q.Set("limit", strconv.Itoa(limit))
	if since > 0 {
		end := time.Now()
		q.Set("start", end.Add(-since).UTC().Format(time.RFC3339Nano))
		q.Set("end", end.UTC().Format(time.RFC3339Nano))
	}
	u.RawQuery = q.Encode()
	body, err := c.get(ctx, u.String())
	if err != nil {
		return nil, u.String(), err
	}
	rows, err := decodeLogRows(body)
	return rows, u.String(), err
}

func (c *Client) metricQuery(ctx context.Context, promql string) ([]MetricSeries, string, error) {
	u, err := url.Parse(c.BaseURL + "/metrics/api/v1/query")
	if err != nil {
		return nil, "", err
	}
	q := u.Query()
	q.Set("query", promql)
	u.RawQuery = q.Encode()
	body, err := c.get(ctx, u.String())
	if err != nil {
		return nil, u.String(), err
	}
	series, err := decodeMetricSeries(body)
	return series, u.String(), err
}

func (c *Client) jaegerTrace(ctx context.Context, traceID string) ([]map[string]any, string, error) {
	u := c.BaseURL + "/traces/select/jaeger/api/traces/" + url.PathEscape(traceID)
	body, err := c.get(ctx, u)
	if err != nil {
		return nil, u, err
	}
	rows, err := decodeJaegerTrace(body)
	return rows, u, err
}

func (c *Client) get(ctx context.Context, u string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("backend unreachable: %w", err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg := strings.TrimSpace(string(b))
		if msg == "" {
			msg = resp.Status
		}
		return nil, fmt.Errorf("backend returned %s: %s", resp.Status, msg)
	}
	return b, nil
}

func decodeLogRows(b []byte) ([]map[string]any, error) {
	var wrapped struct {
		Data []map[string]any `json:"data"`
		Hits []map[string]any `json:"hits"`
	}
	if json.Unmarshal(b, &wrapped) == nil {
		if wrapped.Data != nil {
			return wrapped.Data, nil
		}
		if wrapped.Hits != nil {
			return wrapped.Hits, nil
		}
	}
	var arr []map[string]any
	if json.Unmarshal(b, &arr) == nil {
		return arr, nil
	}
	var rows []map[string]any
	sc := bufio.NewScanner(strings.NewReader(string(b)))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var row map[string]any
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			return nil, err
		}
		rows = append(rows, row)
	}
	return rows, sc.Err()
}

func decodeMetricSeries(b []byte) ([]MetricSeries, error) {
	var resp struct {
		Status string `json:"status"`
		Data   struct {
			Result []struct {
				Metric map[string]string `json:"metric"`
				Value  []any             `json:"value"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(b, &resp); err != nil {
		return nil, err
	}
	out := make([]MetricSeries, 0, len(resp.Data.Result))
	for _, r := range resp.Data.Result {
		v := 0.0
		if len(r.Value) > 1 {
			v = number(r.Value[1])
		}
		out = append(out, MetricSeries{Name: r.Metric["__name__"], Labels: r.Metric, Value: v})
	}
	return out, nil
}

func decodeJaegerTrace(b []byte) ([]map[string]any, error) {
	var resp struct {
		Data []struct {
			Spans []map[string]any `json:"spans"`
		} `json:"data"`
	}
	if err := json.Unmarshal(b, &resp); err != nil {
		return nil, err
	}
	if len(resp.Data) == 0 {
		return nil, nil
	}
	return resp.Data[0].Spans, nil
}

// field looks up a logical attribute name in a VictoriaTraces row.
//
// VictoriaTraces does NOT store attributes under their bare OTel names. It prefixes them by
// origin, and buries span-event attributes under an indexed key:
//
//	cmd.exit_code    ->  span_attr:cmd.exit_code
//	service.name     ->  resource_attr:service.name
//	value.source     ->  event:event_attr:value.source:0   (the :0 is the event index)
//
// The first cut of this package queried and read the bare names, so every trace verb returned
// EMPTY against a real store — a plausible "0 matches" that meant "I looked in a schema that
// does not exist," not "nothing happened." It was never caught because the tests mock the HTTP
// server with hand-authored flat rows. This resolver is what a live VictoriaTraces actually
// returns; see schema_live_test.go, whose fixtures are copied from a real store.
func field(row map[string]any, names ...string) string {
	for _, n := range names {
		if s := lookupSchemaAware(row, n); s != "" {
			return s
		}
	}
	return ""
}

// lookupSchemaAware tries the bare key, then each origin prefix, then a scan for the indexed
// event-attribute form (event:event_attr:<name>:<N>).
func lookupSchemaAware(row map[string]any, n string) string {
	for _, k := range []string{n, "span_attr:" + n, "resource_attr:" + n} {
		if s := coerce(row[k]); s != "" {
			return s
		}
	}
	// Event attributes carry a trailing :<index> that we cannot predict.
	prefix := "event:event_attr:" + n + ":"
	for k, v := range row {
		if strings.HasPrefix(k, prefix) {
			if s := coerce(v); s != "" {
				return s
			}
		}
	}
	return ""
}

func coerce(v any) string {
	if v == nil {
		return ""
	}
	switch x := v.(type) {
	case string:
		return x
	case float64:
		if math.Trunc(x) == x {
			return strconv.FormatInt(int64(x), 10)
		}
		return strconv.FormatFloat(x, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(x)
	default:
		return fmt.Sprint(x)
	}
}

func number(v any) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case string:
		f, _ := strconv.ParseFloat(x, 64)
		return f
	default:
		f, _ := strconv.ParseFloat(fmt.Sprint(v), 64)
		return f
	}
}
