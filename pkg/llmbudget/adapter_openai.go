package llmbudget

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// OpenAIOrganizationAdapter implements the documented admin GET endpoints:
// https://developers.openai.com/api/reference/resources/admin/subresources/organization/subresources/usage/methods/completions
// https://developers.openai.com/api/reference/resources/admin/subresources/organization/subresources/usage/methods/costs
// Client is injectable for fixture transports. The production URL is fixed;
// credentials cannot follow redirects to another origin.
type OpenAIOrganizationAdapter struct {
	Client  *http.Client
	Resolve CredentialResolver
}

func (OpenAIOrganizationAdapter) Kind() string { return "openai-organization" }
func (a OpenAIOrganizationAdapter) Collect(ctx context.Context, c SourceConfig, now time.Time) (SourceResult, error) {
	out := SourceResult{Status: "ok", Metrics: []Metric{}, Limitations: []string{"Organization aggregates include covered external usage; do not add these totals to their local component observations. Reporting windows are not vendor quota windows."}}
	if c.Lane != LaneAPIKey || c.Provider != "openai" {
		out.Status = "unavailable"
		return out, errors.New("OpenAI organization source requires the OpenAI API billing lane")
	}
	key, err := explicitCredential(ctx, c.CredentialRef, a.Resolve)
	if err != nil || key == "" {
		out.Status = "auth_required"
		return out, errors.New("configured OpenAI admin credential unavailable")
	}
	client := &http.Client{Timeout: 5 * time.Second}
	if a.Client != nil {
		*client = *a.Client
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	start := dayStart(now)
	failed := false
	for _, kind := range []string{"completions", "costs"} {
		endpoint := "https://api.openai.com/v1/organization/usage/completions"
		if kind == "costs" {
			endpoint = "https://api.openai.com/v1/organization/costs"
		}
		params := url.Values{"start_time": {strconv.FormatInt(start.Unix(), 10)}, "end_time": {strconv.FormatInt(now.Unix(), 10)}, "bucket_width": {"1d"}, "limit": {"1"}}
		var totals = map[string]float64{}
		var have = map[string]bool{}
		pages := map[string]bool{}
		complete := false
		for page := 0; page < 4; page++ {
			req, e := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"?"+params.Encode(), nil)
			if e != nil {
				return out, e
			}
			req.Header.Set("Authorization", "Bearer "+key)
			if c.Organization != "" {
				req.Header.Set("OpenAI-Organization", c.Organization)
			}
			response, e := client.Do(req)
			if e != nil {
				failed = true
				break
			}
			body, e := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
			response.Body.Close()
			if response.StatusCode != http.StatusOK {
				failed = true
				if response.StatusCode == 401 || response.StatusCode == 403 {
					out.Status = "auth_required"
				}
				if response.StatusCode == 429 {
					t := retryTime(response.Header.Get("Retry-After"), now)
					out.RetryAt = &t
				}
				break
			}
			if e != nil || len(body) > 1<<20 {
				failed = true
				break
			}
			var wire struct {
				Object string `json:"object"`
				Data   []struct {
					Object  string            `json:"object"`
					Start   *int64            `json:"start_time"`
					End     *int64            `json:"end_time"`
					Results []json.RawMessage `json:"results"`
				} `json:"data"`
				More *bool   `json:"has_more"`
				Next *string `json:"next_page"`
			}
			if json.Unmarshal(body, &wire) != nil || wire.Object != "page" || wire.More == nil || wire.Data == nil {
				failed = true
				break
			}
			valid := true
			for _, bucket := range wire.Data {
				if bucket.Object != "bucket" || bucket.Start == nil || bucket.End == nil || *bucket.End <= *bucket.Start {
					valid = false
					break
				}
				for _, raw := range bucket.Results {
					var row struct {
						Object   string   `json:"object"`
						Input    *float64 `json:"input_tokens"`
						Output   *float64 `json:"output_tokens"`
						Cached   *float64 `json:"input_cached_tokens"`
						Requests *float64 `json:"num_model_requests"`
						Amount   *struct {
							Value    *float64 `json:"value"`
							Currency string   `json:"currency"`
						} `json:"amount"`
					}
					if json.Unmarshal(raw, &row) != nil {
						valid = false
						break
					}
					if kind == "costs" {
						if row.Object != "organization.costs.result" || row.Amount == nil || row.Amount.Value == nil || row.Amount.Currency != "usd" || !finiteNonnegative(*row.Amount.Value) {
							valid = false
							break
						}
						totals["billing.spend"] += *row.Amount.Value
						if !finiteNonnegative(totals["billing.spend"]) {
							valid = false
							break
						}
						have["billing.spend"] = true
					} else {
						if row.Object != "organization.usage.completions.result" {
							valid = false
							break
						}
						for _, metric := range []struct {
							name string
							p    *float64
						}{{"usage.input_tokens", row.Input}, {"usage.output_tokens", row.Output}, {"usage.cached_input_tokens", row.Cached}, {"usage.requests", row.Requests}} {
							if metric.p == nil {
								continue
							}
							if !finiteNonnegative(*metric.p) || *metric.p != math.Trunc(*metric.p) || *metric.p > 1<<53 {
								valid = false
								break
							}
							totals[metric.name] += *metric.p
							if totals[metric.name] > 1<<53 {
								valid = false
								break
							}
							have[metric.name] = true
						}
					}
				}
			}
			if !valid {
				failed = true
				break
			}
			if !*wire.More {
				complete = true
				break
			}
			if wire.Next == nil || *wire.Next == "" || pages[*wire.Next] {
				failed = true
				break
			}
			pages[*wire.Next] = true
			params.Set("page", *wire.Next)
		}
		if !complete {
			failed = true
			out.Limitations = append(out.Limitations, kind+" report is incomplete/unavailable; no partial aggregate is presented as a total.")
			continue
		}
		for name, value := range totals {
			if !have[name] {
				continue
			}
			unit := "tokens"
			if name == "usage.requests" {
				unit = "requests"
			}
			if name == "billing.spend" {
				unit = "usd"
			}
			m := measured(name, value, unit, "actual", "openai-organization:"+kind, now)
			m.WindowStart = &start
			m.WindowEnd = &now
			out.Metrics = append(out.Metrics, m)
		}
	}
	if failed {
		if out.Status == "ok" {
			out.Status = "partial"
		}
		return out, errors.New("OpenAI organization source partially unavailable")
	}
	return out, nil
}
func finiteNonnegative(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) && v >= 0 }
func retryTime(value string, now time.Time) time.Time {
	if seconds, e := strconv.ParseInt(value, 10, 64); e == nil && seconds >= 0 && seconds <= 86400*30 {
		return now.Add(time.Duration(seconds) * time.Second)
	}
	if at, e := http.ParseTime(value); e == nil && at.After(now) {
		return at
	}
	return now.Add(time.Minute)
}
