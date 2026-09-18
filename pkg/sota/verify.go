package sota

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// urlRe pulls citation URLs out of a report.
var urlRe = regexp.MustCompile(`https?://[^\s)\]}>"']+`)

// verifyCitations extracts the URLs a report cites and checks that a sample of
// them ACTUALLY RESOLVE — the anti-hallucination pass. A citation that does not
// resolve is the exact failure a research tool must never hide: a plausible
// source that is not real. Returns a summary line + counts.
func verifyCitations(ctx context.Context, report string, sample int) (summary string, ok, total int, dead []string) {
	urls := dedupURLs(urlRe.FindAllString(report, -1))
	total = len(urls)
	if total == 0 {
		return "\n\n---\n[sota] citations: none found to verify — treat findings as UNSOURCED.", 0, 0, nil
	}
	if sample <= 0 || sample > total {
		sample = total
	}
	client := &http.Client{Timeout: 10 * time.Second}
	for _, u := range urls[:sample] {
		if resolves(ctx, client, u) {
			ok++
		} else {
			dead = append(dead, u)
		}
	}
	summary = fmt.Sprintf("\n\n---\n[sota] citations: %d/%d checked resolve", ok, sample)
	if len(dead) > 0 {
		summary += fmt.Sprintf(" — %d UNRESOLVED (treat as unsupported): %s", len(dead), strings.Join(dead, ", "))
	}
	return summary, ok, total, dead
}

// resolves reports whether a URL answers 2xx/3xx. HEAD first (cheap); some hosts
// reject HEAD, so fall back to a ranged GET.
func resolves(ctx context.Context, client *http.Client, u string) bool {
	u = strings.TrimRight(u, ".,;)")
	for _, method := range []string{http.MethodHead, http.MethodGet} {
		req, err := http.NewRequestWithContext(ctx, method, u, nil)
		if err != nil {
			return false
		}
		req.Header.Set("User-Agent", "bashy-sota/1")
		resp, err := client.Do(req)
		if err != nil {
			continue
		}
		resp.Body.Close()
		if resp.StatusCode >= 200 && resp.StatusCode < 400 {
			return true
		}
		if resp.StatusCode == http.StatusMethodNotAllowed || resp.StatusCode == http.StatusForbidden {
			continue // try GET
		}
		return false
	}
	return false
}

func dedupURLs(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, u := range in {
		u = strings.TrimRight(u, ".,;)")
		if !seen[u] {
			seen[u] = true
			out = append(out, u)
		}
	}
	return out
}
