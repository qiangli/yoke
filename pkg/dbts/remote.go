package dbts

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/qiangli/yoke/pkg/dblifecycle"
	"github.com/qiangli/yoke/pkg/dbstore"
)

// Remote VictoriaMetrics tools (contract §3.1 remote targets, T11
// cooperative limits). The endpoint is explicit in every request: there is
// no default, no environment fallback and nothing is provisioned or
// started. Queries use GET /api/v1/query_range, ingestion uses POST
// /api/v1/import in Prometheus text exposition format. Every call is
// bounded by the request context plus the client's own timeout, and every
// endpoint failure is a structured backend_error, never a silent empty
// result. Exclusion beyond cooperative is refused: the caps row for these
// ops reports cooperative scope only.
const (
	DefaultRemoteTimeout = 10 * time.Second
	MaxRemoteTimeout     = 5 * time.Minute
	DefaultRemoteLimit   = 1000
	MaxRemoteLimit       = 100000
	MaxRemoteIngest      = 100000
	maxRemoteBody        = 8 << 20
	vmQueryPath          = "/api/v1/query_range"
	vmImportPath         = "/api/v1/import"
)

// Client is one explicitly configured VictoriaMetrics endpoint.
type Client struct {
	endpoint string
	http     *http.Client
}

// NewClient validates an explicit endpoint and returns a bounded client.
// Credentials in the URL are refused: endpoints carry no authority.
func NewClient(endpoint string, timeout time.Duration) (*Client, error) {
	endpoint, err := checkEndpoint(endpoint)
	if err != nil {
		return nil, err
	}
	if timeout == 0 {
		timeout = DefaultRemoteTimeout
	}
	if timeout <= 0 || timeout > MaxRemoteTimeout {
		return nil, fmt.Errorf("%w: timeout 1ns..%s", dbstore.ErrInvalidInput, MaxRemoteTimeout)
	}
	return &Client{endpoint: endpoint, http: &http.Client{Timeout: timeout}}, nil
}

// checkEndpoint requires an explicit http(s) URL without credentials.
func checkEndpoint(endpoint string) (string, error) {
	if endpoint == "" {
		return "", fmt.Errorf("%w: remote ops need an explicit endpoint; nothing is provisioned or assumed", dbstore.ErrInvalidInput)
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		return "", fmt.Errorf("%w: endpoint %q: %v", dbstore.ErrInvalidInput, endpoint, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("%w: endpoint scheme %q (http or https)", dbstore.ErrInvalidInput, u.Scheme)
	}
	if u.Host == "" {
		return "", fmt.Errorf("%w: endpoint %q has no host", dbstore.ErrInvalidInput, endpoint)
	}
	if u.User != nil {
		return "", fmt.Errorf("%w: endpoint carries credentials; pass auth outside the URL", dbstore.ErrInvalidInput)
	}
	return strings.TrimRight(endpoint, "/"), nil
}

// EndpointID is the canonical remote identity for cursor bindings and
// results: db:vm:remote:<endpoint>, with no credentials by construction.
func EndpointID(endpoint string) string { return "db:vm:remote:" + endpoint }

// RemotePoint is one sample of a remote series in §8.3 tagged form.
type RemotePoint struct {
	Ts    map[string]any `json:"ts"`
	Value map[string]any `json:"value"`
}

// RemoteSeries is one queried series: its name, labels and samples.
type RemoteSeries struct {
	Name   string            `json:"name"`
	Labels map[string]string `json:"labels"`
	Points []RemotePoint     `json:"points"`
}

// vmRange is the decoded /api/v1/query_range envelope.
type vmRange struct {
	Status string `json:"status"`
	Data   struct {
		ResultType string `json:"resultType"`
		Result     []struct {
			Metric map[string]string `json:"metric"`
			Values [][2]any          `json:"values"`
		} `json:"result"`
	} `json:"data"`
	ErrorType string `json:"errorType"`
	Error     string `json:"error"`
}

// Query runs an instant or range query against the endpoint. Start and end
// are Unix nanoseconds; step is the query resolution.
func (c *Client) Query(ctx context.Context, expr string, start, end int64, step time.Duration, limit int) ([]RemoteSeries, error) {
	if expr == "" {
		return nil, fmt.Errorf("%w: query needs an expression", dbstore.ErrInvalidInput)
	}
	if step <= 0 {
		return nil, fmt.Errorf("%w: step must be positive", dbstore.ErrInvalidInput)
	}
	if start >= end {
		return nil, fmt.Errorf("%w: start must precede end", dbstore.ErrInvalidInput)
	}
	if limit <= 0 || limit > MaxRemoteLimit {
		return nil, fmt.Errorf("%w: limit 1..%d", dbstore.ErrInvalidInput, MaxRemoteLimit)
	}
	u, err := url.Parse(c.endpoint + vmQueryPath)
	if err != nil {
		return nil, fmt.Errorf("%w: endpoint: %v", dbstore.ErrInvalidInput, err)
	}
	q := u.Query()
	q.Set("query", expr)
	// Unix seconds; fractional when the bound carries sub-second precision.
	q.Set("start", strconv.FormatFloat(float64(start)/1e9, 'f', -1, 64))
	q.Set("end", strconv.FormatFloat(float64(end)/1e9, 'f', -1, 64))
	q.Set("step", step.String())
	u.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", dbstore.ErrInvalidInput, err)
	}
	body, err := c.do(req)
	if err != nil {
		return nil, err
	}
	var vr vmRange
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	if err := dec.Decode(&vr); err != nil {
		return nil, fmt.Errorf("%w: endpoint returned undecodable JSON: %v", dbstore.ErrBackend, err)
	}
	if vr.Status != "success" {
		msg := vr.Error
		if msg == "" {
			msg = "unknown remote error"
		}
		if vr.ErrorType != "" {
			msg = vr.ErrorType + ": " + msg
		}
		return nil, fmt.Errorf("%w: vm endpoint: %s", dbstore.ErrBackend, msg)
	}
	if vr.Data.ResultType != "matrix" {
		return nil, fmt.Errorf("%w: result type %q (want matrix)", dbstore.ErrBackend, vr.Data.ResultType)
	}
	var out []RemoteSeries
	total := 0
	for _, r := range vr.Data.Result {
		name := r.Metric["__name__"]
		labels := map[string]string{}
		for k, v := range r.Metric {
			if k != "__name__" {
				labels[k] = v
			}
		}
		s := RemoteSeries{Name: name, Labels: labels}
		for _, p := range r.Values {
			ts, err := vmTS(p[0])
			if err != nil {
				return nil, err
			}
			v, err := vmValue(p[1])
			if err != nil {
				return nil, err
			}
			s.Points = append(s.Points, RemotePoint{Ts: encodeTS(ts), Value: encodeValue(v)})
			total++
			if total > limit {
				return nil, fmt.Errorf("%w: remote result holds over --limit %d points", dbstore.ErrLimitExceeded, limit)
			}
		}
		out = append(out, s)
	}
	if out == nil {
		out = []RemoteSeries{}
	}
	return out, nil
}

// Ingest writes series to the endpoint in Prometheus text format. NaN and
// Inf have no text rendering and are refused before any byte is sent.
func (c *Client) Ingest(ctx context.Context, series []RemoteSeries) (int, error) {
	if len(series) == 0 {
		return 0, fmt.Errorf("%w: ingest needs series", dbstore.ErrInvalidInput)
	}
	var b strings.Builder
	n := 0
	for _, s := range series {
		if err := checkSeriesName(s.Name); err != nil {
			return 0, err
		}
		if _, err := canonicalLabels(s.Labels); err != nil {
			return 0, err
		}
		for i, p := range s.Points {
			ts, err := taggedTS(p.Ts)
			if err != nil {
				return 0, fmt.Errorf("series %q point %d: %w", s.Name, i, err)
			}
			v, err := taggedValue(p.Value)
			if err != nil {
				return 0, fmt.Errorf("series %q point %d: %w", s.Name, i, err)
			}
			if math.IsNaN(v) || math.IsInf(v, 0) {
				return 0, fmt.Errorf("%w: series %q point %d is not finite; remote text has no NaN/Inf rendering", dbstore.ErrInvalidInput, s.Name, i)
			}
			fmt.Fprintf(&b, "%s{%s} %s %d\n", s.Name, promLabels(s.Labels),
				strconv.FormatFloat(v, 'g', -1, 64), ts/1e6)
			n++
			if n > MaxRemoteIngest {
				return 0, fmt.Errorf("%w: over %d remote points in one request", dbstore.ErrLimitExceeded, MaxRemoteIngest)
			}
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint+vmImportPath, strings.NewReader(b.String()))
	if err != nil {
		return 0, fmt.Errorf("%w: %v", dbstore.ErrInvalidInput, err)
	}
	req.Header.Set("Content-Type", "text/plain")
	if _, err := c.do(req); err != nil {
		return 0, err
	}
	return n, nil
}

// do sends one bounded request and maps every failure to a contract error.
// The context stays authoritative: a dead context reports timeout or
// cancelled even when the transport also failed.
func (c *Client) do(req *http.Request) ([]byte, error) {
	resp, err := c.http.Do(req)
	if err != nil {
		if cerr := req.Context().Err(); cerr != nil {
			return nil, dbstore.FromContext(cerr)
		}
		return nil, fmt.Errorf("%w: vm endpoint unreachable: %v", dbstore.ErrBackend, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxRemoteBody))
	if err != nil {
		if cerr := req.Context().Err(); cerr != nil {
			return nil, dbstore.FromContext(cerr)
		}
		return nil, fmt.Errorf("%w: reading endpoint response: %v", dbstore.ErrBackend, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg := strings.TrimSpace(string(b))
		if msg == "" {
			msg = resp.Status
		}
		if len(msg) > 500 {
			msg = msg[:500]
		}
		return nil, fmt.Errorf("%w: vm endpoint returned %s: %s", dbstore.ErrBackend, resp.Status, msg)
	}
	return b, nil
}

// vmTS decodes a query_range stamp (JSON seconds, number or string).
func vmTS(v any) (int64, error) {
	var f float64
	switch x := v.(type) {
	case json.Number:
		var err error
		if f, err = x.Float64(); err != nil {
			return 0, fmt.Errorf("%w: remote stamp %q", dbstore.ErrBackend, x)
		}
	case string:
		var err error
		if f, err = strconv.ParseFloat(x, 64); err != nil {
			return 0, fmt.Errorf("%w: remote stamp %q", dbstore.ErrBackend, x)
		}
	default:
		return 0, fmt.Errorf("%w: remote stamp of type %T", dbstore.ErrBackend, v)
	}
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, fmt.Errorf("%w: remote stamp %v", dbstore.ErrBackend, f)
	}
	ns := f * 1e9
	if ns > float64(math.MaxInt64) || ns < float64(math.MinInt64) {
		return 0, fmt.Errorf("%w: remote stamp %v overflows nanoseconds", dbstore.ErrBackend, f)
	}
	return int64(ns), nil
}

// vmValue decodes a query_range value (a JSON string).
func vmValue(v any) (float64, error) {
	s, ok := v.(string)
	if !ok {
		return 0, fmt.Errorf("%w: remote value of type %T", dbstore.ErrBackend, v)
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, fmt.Errorf("%w: remote value %q", dbstore.ErrBackend, s)
	}
	return f, nil
}

// promLabels renders labels in Prometheus text form.
func promLabels(labels map[string]string) string {
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	var b strings.Builder
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(',')
		}
		v := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(labels[k])
		fmt.Fprintf(&b, `%s="%s"`, k, v)
	}
	return b.String()
}

// taggedTS/taggedValue decode §8.3 tagged values from a remote series.
func taggedTS(m map[string]any) (int64, error) {
	b, _ := json.Marshal(m)
	return parseTS("ts", b)
}

func taggedValue(m map[string]any) (float64, error) {
	b, _ := json.Marshal(m)
	return parseValue("value", b)
}

func validateRemoteQuery(in *Input) error {
	if _, err := checkEndpoint(in.Endpoint); err != nil {
		return err
	}
	if in.Expr == "" {
		return fmt.Errorf("%w: remote query needs an expression", dbstore.ErrInvalidInput)
	}
	var start, end int64
	if len(in.Start) != 0 {
		n, err := parseTS("start", in.Start)
		if err != nil {
			return err
		}
		start = n
	} else {
		return fmt.Errorf("%w: remote query needs start and end", dbstore.ErrInvalidInput)
	}
	if len(in.End) != 0 {
		n, err := parseTS("end", in.End)
		if err != nil {
			return err
		}
		end = n
	} else {
		return fmt.Errorf("%w: remote query needs start and end", dbstore.ErrInvalidInput)
	}
	if start >= end {
		return fmt.Errorf("%w: start must precede end", dbstore.ErrInvalidInput)
	}
	if _, err := parseDuration("step", in.Step); err != nil {
		return err
	}
	limit := in.Limit
	if limit == 0 {
		limit = DefaultRemoteLimit
	}
	if limit < 0 || limit > MaxRemoteLimit {
		return fmt.Errorf("%w: limit 1..%d", dbstore.ErrInvalidInput, MaxRemoteLimit)
	}
	if in.Timeout != "" {
		if _, err := parseDuration("timeout", in.Timeout); err != nil {
			return err
		}
	}
	return nil
}

func validateRemoteIngest(in *Input) error {
	if _, err := checkEndpoint(in.Endpoint); err != nil {
		return err
	}
	if len(in.SeriesList) == 0 {
		return fmt.Errorf("%w: remote ingest needs series_list", dbstore.ErrInvalidInput)
	}
	n := 0
	for i, s := range in.SeriesList {
		if err := checkSeriesName(s.Name); err != nil {
			return fmt.Errorf("series_list[%d]: %w", i, err)
		}
		if _, err := canonicalLabels(s.Labels); err != nil {
			return fmt.Errorf("series_list[%d]: %w", i, err)
		}
		for j, p := range s.Points {
			if _, err := parseTS("ts", p.Ts); err != nil {
				return fmt.Errorf("series_list[%d] point %d: %w", i, j, err)
			}
			if _, err := parseValue("value", p.Value); err != nil {
				return fmt.Errorf("series_list[%d] point %d: %w", i, j, err)
			}
			n++
			if n > MaxRemoteIngest {
				return fmt.Errorf("%w: over %d remote points in one request", dbstore.ErrLimitExceeded, MaxRemoteIngest)
			}
		}
	}
	if in.Timeout != "" {
		if _, err := parseDuration("timeout", in.Timeout); err != nil {
			return err
		}
	}
	return nil
}

func remoteTimeout(in Input) (time.Duration, error) {
	if in.Timeout == "" {
		return DefaultRemoteTimeout, nil
	}
	ns, err := parseDuration("timeout", in.Timeout)
	if err != nil {
		return 0, err
	}
	if ns > int64(MaxRemoteTimeout) {
		return 0, fmt.Errorf("%w: timeout over %s", dbstore.ErrInvalidInput, MaxRemoteTimeout)
	}
	return time.Duration(ns), nil
}

func runRemoteQuery(ctx context.Context, h dbstore.Handle, in Input) (dblifecycle.Outcome, error) {
	start, _ := parseTS("start", in.Start)
	end, _ := parseTS("end", in.End)
	step, _ := parseDuration("step", in.Step)
	limit := in.Limit
	if limit == 0 {
		limit = DefaultRemoteLimit
	}
	timeout, err := remoteTimeout(in)
	if err != nil {
		return dblifecycle.Outcome{}, err
	}
	ep, err := checkEndpoint(in.Endpoint)
	if err != nil {
		return dblifecycle.Outcome{}, err
	}
	c, err := NewClient(ep, timeout)
	if err != nil {
		return dblifecycle.Outcome{}, err
	}
	// The lifecycle target the op was admitted through is only the
	// claim-guard anchor; the endpoint below is the data-plane target the
	// page binds to. Remote reads are one bounded page, never a cursor:
	// over-limit results fail explicitly so the caller narrows the range.
	binding := requestHash(OpRemoteQuery, map[string]any{"endpoint": ep, "expr": in.Expr,
		"start": start, "end": end, "step": step})
	series, err := c.Query(ctx, in.Expr, start, end, time.Duration(step), limit)
	if err != nil {
		return dblifecycle.Outcome{}, err
	}
	return dblifecycle.Outcome{Result: map[string]any{"endpoint": EndpointID(ep), "series": series},
		Page: newPage(EndpointID(ep), binding, 0, len(series), limit, 0, false)}, nil
}

func runRemoteIngest(ctx context.Context, h dbstore.Handle, in Input) (dblifecycle.Outcome, error) {
	timeout, err := remoteTimeout(in)
	if err != nil {
		return dblifecycle.Outcome{}, err
	}
	ep, err := checkEndpoint(in.Endpoint)
	if err != nil {
		return dblifecycle.Outcome{}, err
	}
	c, err := NewClient(ep, timeout)
	if err != nil {
		return dblifecycle.Outcome{}, err
	}
	series := make([]RemoteSeries, len(in.SeriesList))
	for i, s := range in.SeriesList {
		points := make([]RemotePoint, len(s.Points))
		for j, p := range s.Points {
			ts, err := parseTS("ts", p.Ts)
			if err != nil {
				return dblifecycle.Outcome{}, err
			}
			v, err := parseValue("value", p.Value)
			if err != nil {
				return dblifecycle.Outcome{}, err
			}
			points[j] = RemotePoint{Ts: encodeTS(ts), Value: encodeValue(v)}
		}
		series[i] = RemoteSeries{Name: s.Name, Labels: s.Labels, Points: points}
	}
	n, err := c.Ingest(ctx, series)
	if err != nil {
		return dblifecycle.Outcome{}, err
	}
	return dblifecycle.Outcome{Result: map[string]any{"endpoint": EndpointID(ep), "points": n}}, nil
}
