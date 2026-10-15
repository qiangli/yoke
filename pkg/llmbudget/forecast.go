package llmbudget

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/qiangli/yoke/pkg/fleet"
)

type AccountForecast struct {
	Account                string             `json:"account"`
	Window                 string             `json:"window"`
	Classification         string             `json:"classification"`
	Verdict                string             `json:"verdict"`
	UsedPercent            float64            `json:"used_percent"`
	ResetAt                time.Time          `json:"reset_at"`
	TokensPerPercent       float64            `json:"tokens_per_percent,omitempty"`
	CoefficientOfVariation float64            `json:"coefficient_of_variation,omitempty"`
	Burn3H                 float64            `json:"burn_3h,omitempty"`
	Burn24H                float64            `json:"burn_24h,omitempty"`
	BurnRate               float64            `json:"burn_rate,omitempty"`
	P50                    *time.Time         `json:"p50,omitempty"`
	P90                    *time.Time         `json:"p90,omitempty"`
	ExhaustsAt             *time.Time         `json:"exhausts_at,omitempty"`
	Confidence             float64            `json:"confidence"`
	CompletedCycles        int                `json:"completed_cycles"`
	AgentShare             map[string]float64 `json:"agent_share,omitempty"`
	ToolShare              map[string]float64 `json:"tool_share,omitempty"`
	SuggestedWorkers       *int               `json:"suggested_workers,omitempty"`
	Regime                 string             `json:"regime,omitempty"`
}
type forecastRecord struct {
	Kind             string    `json:"kind"`
	Key              string    `json:"key"`
	At               time.Time `json:"at"`
	Account          string    `json:"account"`
	Window           string    `json:"window,omitempty"`
	Agent            string    `json:"agent,omitempty"`
	Tool             string    `json:"tool,omitempty"`
	Input            int64     `json:"input,omitempty"`
	Output           int64     `json:"output,omitempty"`
	Used             float64   `json:"used_percent,omitempty"`
	ResetAt          time.Time `json:"reset_at,omitempty"`
	Source           string    `json:"source,omitempty"`
	Classification   string    `json:"classification,omitempty"`
	PlanDigest       string    `json:"plan_digest,omitempty"`
	CycleStart       time.Time `json:"cycle_start,omitempty"`
	WindowStart      time.Time `json:"window_start,omitempty"`
	Peak             float64   `json:"peak_used_percent,omitempty"`
	Tokens           int64     `json:"tokens_in_cycle,omitempty"`
	TokensPerPercent float64   `json:"tokens_per_percent,omitempty"`
	Reason           string    `json:"reason,omitempty"`
	Verdict          string    `json:"verdict,omitempty"`
}

var forecastMu sync.Mutex

func forecastPath() string { return filepath.Join(usageHome(), "quota", "forecast-history.jsonl") }
func recordKey(parts ...any) string {
	b, _ := json.Marshal(parts)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:12])
}
func loadForecast(path string) []forecastRecord {
	f, e := os.Open(path)
	if e != nil {
		return nil
	}
	defer f.Close()
	var out []forecastRecord
	s := bufio.NewScanner(f)
	s.Buffer(make([]byte, 4096), 1<<20)
	for s.Scan() {
		var r forecastRecord
		if json.Unmarshal(s.Bytes(), &r) == nil && r.Kind != "" {
			out = append(out, r)
		}
	}
	return out
}
func appendForecast(path string, rows []forecastRecord) error {
	if len(rows) == 0 {
		return nil
	}
	if e := os.MkdirAll(filepath.Dir(path), 0700); e != nil {
		return e
	}
	f, e := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if e != nil {
		return e
	}
	defer f.Close()
	if e := f.Chmod(0600); e != nil {
		return e
	}
	enc := json.NewEncoder(f)
	for _, r := range rows {
		if e := enc.Encode(r); e != nil {
			return e
		}
	}
	return f.Sync()
}
func compactForecast(path string, rows []forecastRecord, now time.Time) error {
	cutoff := now.Add(-90 * 24 * time.Hour)
	hourCutoff := now.Add(-7 * 24 * time.Hour)
	tmp := path + ".tmp"
	f, e := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if e != nil {
		return e
	}
	enc := json.NewEncoder(f)
	seen := map[string]bool{}
	rolled := map[string]forecastRecord{}
	for _, r := range rows {
		if r.At.Before(cutoff) || seen[r.Key] {
			continue
		}
		seen[r.Key] = true
		if r.Kind == "usage" && r.At.Before(hourCutoff) {
			hour := r.At.UTC().Truncate(time.Hour)
			key := "hour:" + recordKey(r.Account, r.Agent, r.Tool, hour)
			a := rolled[key]
			a.Kind, a.Key, a.At, a.Account, a.Agent, a.Tool = "usage_hour", key, hour, r.Account, r.Agent, r.Tool
			a.Input += r.Input
			a.Output += r.Output
			rolled[key] = a
			continue
		}
		if e = enc.Encode(r); e != nil {
			break
		}
	}
	if e == nil {
		for _, r := range rolled {
			if !seen[r.Key] {
				if e = enc.Encode(r); e != nil {
					break
				}
			}
		}
	}
	if x := f.Close(); e == nil {
		e = x
	}
	if e != nil {
		os.Remove(tmp)
		return e
	}
	return os.Rename(tmp, path)
}
func planDigests() map[string]string {
	c := fleet.New()
	plans, _ := c.Plans()
	out := map[string]string{}
	for _, p := range plans {
		out[p.Name] = recordKey(p.Limits, p.AsOf)
	}
	return out
}

// Ingest stores new usage and authoritative vendor observations. It is safe to repeat.
func Ingest(ctx context.Context, now time.Time) error {
	if now.IsZero() {
		now = time.Now()
	}
	r, e := defaultGate.CollectReport(ctx, ReportOptions{Now: now})
	if e != nil {
		return e
	}
	return ingestReport(ctx, now, r)
}
func ingestReport(ctx context.Context, now time.Time, report *Report) error {
	forecastMu.Lock()
	defer forecastMu.Unlock()
	path := forecastPath()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	lock, err := acquireMeter(ctx, path+".lock")
	if err != nil {
		return err
	}
	defer lock.Release()
	prior := loadForecast(path)
	seen := map[string]bool{}
	for _, r := range prior {
		seen[r.Key] = true
	}
	add := func(r forecastRecord) {
		if r.Key != "" && !seen[r.Key] {
			seen[r.Key] = true
			prior = append(prior, r)
		}
	}
	since := now.Add(-7 * 24 * time.Hour)
	for _, s := range TokenUsage(ctx, since, now.Add(time.Second)) {
		add(forecastRecord{Kind: "usage", Key: "usage:" + s.Key, At: s.At, Account: s.Account, Agent: s.Agent, Tool: s.Tool, Input: s.Input, Output: s.Output, Source: s.Source})
	}
	digests := planDigests()
	if report != nil {
		for _, a := range report.Accounts {
			for _, m := range a.Metrics {
				if m.Name != "quota.used_percent" || m.Value == nil || m.ResetAt == nil || m.ObservedAt.IsZero() {
					continue
				}
				if m.Classification != "measured" && m.Classification != "observed" && m.Classification != "observed-unconfirmed" {
					continue
				}
				key := recordKey(a.Account, m.Window, m.ObservedAt, m.Source, *m.Value, *m.ResetAt)
				rec := forecastRecord{Kind: "snapshot", Key: "snapshot:" + key, At: m.ObservedAt, Account: a.Account, Window: m.Window, Used: *m.Value, ResetAt: *m.ResetAt, Source: m.Source, Classification: m.Classification, PlanDigest: digests[a.Account]}
				if m.WindowStart != nil {
					rec.WindowStart = *m.WindowStart
				}
				add(rec)
			}
		}
	}
	derived := deriveCycles(prior)
	for _, r := range derived {
		add(r)
	}
	derived = deriveRegimes(prior)
	for _, r := range derived {
		add(r)
	}
	for _, f := range calculateForecast(prior, now) {
		add(forecastRecord{Kind: "prediction", Key: "prediction:" + recordKey(f.Account, f.Window, now.UTC().Truncate(time.Hour)), At: now, Account: f.Account, Window: f.Window, ResetAt: f.ResetAt, Verdict: f.Verdict})
	}
	old := loadForecast(path)
	newRows := make([]forecastRecord, 0)
	oldKeys := map[string]bool{}
	for _, r := range old {
		oldKeys[r.Key] = true
	}
	for _, r := range prior {
		if !oldKeys[r.Key] {
			newRows = append(newRows, r)
		}
	}
	if e := appendForecast(path, newRows); e != nil {
		return e
	}
	// Compaction is bounded to once per UTC day and retains raw observations for audit.
	marker := path + ".compact"
	if st, e := os.Stat(marker); e != nil || !sameDay(st.ModTime(), now) {
		if e := compactForecast(path, prior, now); e != nil {
			return e
		}
		_ = os.WriteFile(marker, []byte(now.UTC().Format(time.RFC3339)), 0600)
	}
	return nil
}
func sameDay(a, b time.Time) bool {
	y, m, d := a.UTC().Date()
	yy, mm, dd := b.UTC().Date()
	return y == yy && m == mm && d == dd
}
func isUsage(r forecastRecord) bool { return r.Kind == "usage" || r.Kind == "usage_hour" }
func forecastWindowDuration(window string) time.Duration {
	if d, e := time.ParseDuration(window); e == nil {
		return d
	}
	switch window {
	case "day", "daily", "24h":
		return 24 * time.Hour
	case "week", "weekly", "7d":
		return 7 * 24 * time.Hour
	case "month", "monthly":
		return 30 * 24 * time.Hour
	}
	return 0
}
func deriveCycles(rows []forecastRecord) []forecastRecord {
	groups := map[string][]forecastRecord{}
	for _, r := range rows {
		if r.Kind == "snapshot" {
			groups[r.Account+"\x00"+r.Window] = append(groups[r.Account+"\x00"+r.Window], r)
		}
	}
	var out []forecastRecord
	for key, snaps := range groups {
		sort.Slice(snaps, func(i, j int) bool { return snaps[i].At.Before(snaps[j].At) })
		_ = key
		for i := 1; i < len(snaps); i++ {
			prev, cur := snaps[i-1], snaps[i]
			if !cur.ResetAt.After(prev.ResetAt) || cur.At.Before(prev.ResetAt) {
				continue
			}
			end := prev.ResetAt
			start := prev.WindowStart
			if start.IsZero() {
				if d := forecastWindowDuration(prev.Window); d > 0 {
					start = end.Add(-d)
				}
			}
			if start.IsZero() {
				continue
			}
			var tokens int64
			for _, u := range rows {
				if isUsage(u) && u.Account == cur.Account && !u.At.Before(start) && u.At.Before(end) {
					tokens += u.Input + u.Output
				}
			}
			peak := prev.Used
			for _, s := range snaps {
				if s.ResetAt.Equal(prev.ResetAt) && s.Used > peak {
					peak = s.Used
				}
			}
			if peak <= 0 || tokens <= 0 {
				continue
			}
			tpp := float64(tokens) / peak
			k := "cycle:" + recordKey(cur.Account, cur.Window, start, end)
			out = append(out, forecastRecord{Kind: "cycle", Key: k, At: end, Account: cur.Account, Window: cur.Window, CycleStart: start, ResetAt: end, Peak: peak, Tokens: tokens, TokensPerPercent: tpp, PlanDigest: cur.PlanDigest})
		}
	}
	return out
}
func deriveRegimes(rows []forecastRecord) []forecastRecord {
	var cycles []forecastRecord
	var snaps []forecastRecord
	for _, r := range rows {
		if r.Kind == "cycle" {
			cycles = append(cycles, r)
		}
		if r.Kind == "snapshot" {
			snaps = append(snaps, r)
		}
	}
	sort.Slice(cycles, func(i, j int) bool { return cycles[i].At.Before(cycles[j].At) })
	var out []forecastRecord
	for i, c := range cycles {
		var prior []float64
		var last forecastRecord
		for j := 0; j < i; j++ {
			p := cycles[j]
			if p.Account == c.Account && p.Window == c.Window {
				prior = append(prior, p.TokensPerPercent)
				last = p
			}
		}
		reason := ""
		if len(prior) > 0 {
			mean, cv := meanCV(prior)
			if (len(prior) < 4 && (c.TokensPerPercent > 2*mean || c.TokensPerPercent < mean/2)) || (len(prior) >= 4 && math.Abs(c.TokensPerPercent-mean) > 3*cv*mean) {
				reason = "token calibration changed"
			}
			if last.PlanDigest != "" && c.PlanDigest != "" && last.PlanDigest != c.PlanDigest {
				reason = "plan digest changed"
			}
			if !last.CycleStart.IsZero() && math.Abs(float64(c.ResetAt.Sub(c.CycleStart)-last.ResetAt.Sub(last.CycleStart))) > float64(time.Hour) {
				reason = "reset cadence changed"
			}
		}
		if reason != "" {
			out = append(out, forecastRecord{Kind: "regime", Key: "regime:" + recordKey(c.Account, c.Window, c.At, reason), At: c.At, Account: c.Account, Window: c.Window, Reason: reason})
		}
	}
	sort.Slice(snaps, func(i, j int) bool { return snaps[i].At.Before(snaps[j].At) })
	seenKind := map[string]string{}
	seenPlan := map[string]string{}
	for _, s := range snaps {
		k := s.Account + "\x00" + s.Window
		reason := ""
		if p := seenKind[k]; p != "" && p != s.Classification {
			reason = "signal kind changed"
		}
		if p := seenPlan[k]; p != "" && s.PlanDigest != "" && p != s.PlanDigest {
			reason = "plan digest changed"
		}
		seenKind[k] = s.Classification
		if s.PlanDigest != "" {
			seenPlan[k] = s.PlanDigest
		}
		if reason != "" {
			out = append(out, forecastRecord{Kind: "regime", Key: "regime:" + recordKey(k, s.At, reason), At: s.At, Account: s.Account, Window: s.Window, Reason: reason})
		}
		if s.Used >= 100 {
			for _, p := range rows {
				if p.Kind == "prediction" && p.Account == s.Account && p.Window == s.Window && p.At.Before(s.At) && p.ResetAt.Equal(s.ResetAt) && p.Verdict == "ok-until-reset" {
					out = append(out, forecastRecord{Kind: "regime", Key: "regime:" + recordKey(k, s.At, "forecast contradicted"), At: s.At, Account: s.Account, Window: s.Window, Reason: "forecast contradicted by vendor"})
					break
				}
			}
		}
	}
	return out
}
func meanCV(a []float64) (float64, float64) {
	if len(a) == 0 {
		return 0, 0
	}
	var sum float64
	for _, x := range a {
		sum += x
	}
	m := sum / float64(len(a))
	if m == 0 {
		return 0, 0
	}
	var v float64
	for _, x := range a {
		v += (x - m) * (x - m)
	}
	return m, math.Sqrt(v/float64(len(a))) / m
}
func median(a []float64) float64 {
	if len(a) == 0 {
		return 0
	}
	b := append([]float64(nil), a...)
	sort.Float64s(b)
	n := len(b)
	if n%2 == 1 {
		return b[n/2]
	}
	return (b[n/2-1] + b[n/2]) / 2
}

func forecastHistory(now time.Time) []AccountForecast {
	forecastMu.Lock()
	rows := loadForecast(forecastPath())
	forecastMu.Unlock()
	return calculateForecast(rows, now)
}

// Forecast ingests available observations before calculating an advisory forecast.
func Forecast(ctx context.Context, now time.Time) []AccountForecast {
	if now.IsZero() {
		now = time.Now()
	}
	_ = Ingest(ctx, now)
	return forecastHistory(now)
}
func calculateForecast(rows []forecastRecord, now time.Time) []AccountForecast {
	latest := map[string]forecastRecord{}
	for _, r := range rows {
		if r.Kind == "snapshot" && r.ResetAt.After(now) {
			k := r.Account + "\x00" + r.Window
			if r.At.After(latest[k].At) {
				latest[k] = r
			}
		}
	}
	var out []AccountForecast
	for k, s := range latest {
		_ = k
		f := AccountForecast{Account: s.Account, Window: s.Window, Classification: "predicted", UsedPercent: s.Used, ResetAt: s.ResetAt, Verdict: "unknown", AgentShare: map[string]float64{}, ToolShare: map[string]float64{}}
		var cycles []forecastRecord
		var usage []forecastRecord
		var boundary time.Time
		for _, r := range rows {
			if r.Account != s.Account {
				continue
			}
			if r.Kind == "cycle" && r.Window == s.Window && r.At.Before(now) {
				cycles = append(cycles, r)
			}
			if r.Kind == "regime" && r.Window == s.Window && r.At.After(boundary) {
				boundary = r.At
			}
			if isUsage(r) && r.At.Before(now) {
				usage = append(usage, r)
			}
		}
		sort.Slice(cycles, func(i, j int) bool { return cycles[i].At.Before(cycles[j].At) })
		f.CompletedCycles = len(cycles)
		var all []float64
		for _, c := range cycles {
			all = append(all, c.TokensPerPercent)
		}
		if len(cycles) > 0 {
			ewma := cycles[0].TokensPerPercent
			for _, c := range cycles[1:] {
				alpha := .3
				if c.At.Before(boundary) {
					alpha = .03
				}
				ewma = alpha*c.TokensPerPercent + (1-alpha)*ewma
			}
			f.TokensPerPercent = ewma
			if !boundary.IsZero() {
				var recent []float64
				for _, c := range cycles {
					if !c.At.Before(boundary) {
						recent = append(recent, c.TokensPerPercent)
					}
				}
				if len(recent) > 0 {
					current := recent[0]
					for _, v := range recent[1:] {
						current = .3*v + .7*current
					}
					f.TokensPerPercent = .1*ewma + .9*current
				}
			}
			_, f.CoefficientOfVariation = meanCV(all)
		}
		// Interval median cross-check guards against one anomalous completed cycle.
		var intervals []float64
		var snapshots []forecastRecord
		for _, r := range rows {
			if r.Kind == "snapshot" && r.Account == s.Account && r.Window == s.Window {
				snapshots = append(snapshots, r)
			}
		}
		sort.Slice(snapshots, func(i, j int) bool { return snapshots[i].At.Before(snapshots[j].At) })
		for i := 1; i < len(snapshots); i++ {
			a, b := snapshots[i-1], snapshots[i]
			if !a.ResetAt.Equal(b.ResetAt) || b.Used <= a.Used || (!boundary.IsZero() && a.At.Before(boundary)) {
				continue
			}
			var tok int64
			for _, u := range usage {
				if u.At.After(a.At) && !u.At.After(b.At) {
					tok += u.Input + u.Output
				}
			}
			if tok > 0 {
				intervals = append(intervals, float64(tok)/(b.Used-a.Used))
			}
		}
		if len(intervals) >= 2 {
			m := median(intervals)
			if f.TokensPerPercent == 0 {
				f.TokensPerPercent = m
			} else {
				f.TokensPerPercent = .7*f.TokensPerPercent + .3*m
			}
		}
		var t3, t24 float64
		var byAgent, byTool = map[string]float64{}, map[string]float64{}
		var profile [168]float64
		var bucketN [168]int
		hourly := map[time.Time]float64{}
		for _, u := range usage {
			v := float64(u.Input + u.Output)
			if u.At.After(now.Add(-3 * time.Hour)) {
				t3 += v
			}
			if u.At.After(now.Add(-24 * time.Hour)) {
				t24 += v
				byAgent[u.Agent] += v
				byTool[u.Tool] += v
			}
			if u.At.After(now.Add(-90 * 24 * time.Hour)) {
				hourly[u.At.Truncate(time.Hour)] += v
			}
		}
		var hours []time.Time
		for hour := range hourly {
			hours = append(hours, hour)
		}
		sort.Slice(hours, func(i, j int) bool { return hours[i].Before(hours[j]) })
		for _, hour := range hours {
			h := int(hour.Weekday())*24 + hour.Hour()
			if bucketN[h] == 0 {
				profile[h] = hourly[hour]
			} else {
				profile[h] = .3*hourly[hour] + .7*profile[h]
			}
			bucketN[h]++
		}
		f.Burn3H = t3 / 3
		f.Burn24H = t24 / 24
		f.BurnRate = f.Burn3H
		h := int(now.Weekday())*24 + now.Hour()
		if bucketN[h] > 0 {
			weight := math.Min(.65, float64(bucketN[h])/(float64(bucketN[h])+8))
			f.BurnRate = (1-weight)*f.Burn3H + weight*profile[h]
		}
		if f.BurnRate == 0 {
			f.BurnRate = f.Burn24H
		}
		for agent, v := range byAgent {
			if t24 > 0 {
				f.AgentShare[agent] = v / t24
			}
		}
		for tool, v := range byTool {
			if t24 > 0 {
				f.ToolShare[tool] = v / t24
			}
		}
		f.Confidence = math.Min(1, float64(len(cycles))/6) * math.Min(1, float64(len(usage))/24)
		f.Confidence *= 1 / (1 + f.CoefficientOfVariation)
		if !boundary.IsZero() {
			var recent int
			for _, c := range cycles {
				if !c.At.Before(boundary) {
					recent++
				}
			}
			f.Confidence *= math.Min(1, float64(recent)/3)
			if recent < 2 {
				f.Regime = "relearning"
			}
		}
		if f.TokensPerPercent > 0 && f.BurnRate > 0 {
			hours := math.Max(0, 100-s.Used) * f.TokensPerPercent / f.BurnRate
			p50 := now.Add(time.Duration(hours * float64(time.Hour)))
			spread := math.Max(.1, f.CoefficientOfVariation+1-f.Confidence)
			p90 := now.Add(time.Duration(hours * (1 + 1.28*spread) * float64(time.Hour)))
			f.P50 = &p50
			f.P90 = &p90
			f.ExhaustsAt = &p50
			if len(cycles) < 2 {
				f.Verdict = "learning"
			} else if f.Regime != "" {
				f.Verdict = "relearning"
			} else if p50.Before(s.ResetAt) {
				f.Verdict = "exhausts-before-reset at " + p50.UTC().Format(time.RFC3339)
			} else {
				f.Verdict = "ok-until-reset"
			}
		} else if len(cycles) < 2 {
			f.Verdict = "learning"
		} else if f.Regime != "" {
			f.Verdict = "relearning"
		}
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Account == out[j].Account {
			return out[i].Window < out[j].Window
		}
		return out[i].Account < out[j].Account
	})
	return out
}
func applyForecast(rows []QuotaRow, forecasts []AccountForecast, now time.Time) {
	byKey := map[string]AccountForecast{}
	for _, f := range forecasts {
		byKey[f.Account+"\x00"+f.Window] = f
	}
	for i := range rows {
		r := &rows[i]
		f, ok := byKey[r.Account+"\x00"+r.Window]
		if !ok {
			continue
		}
		r.Predict = f.Verdict
		r.ExhaustsAt = f.P50
		r.Confidence = f.Confidence
		if strings.HasPrefix(f.Verdict, "exhausts-before-reset") && f.P50 != nil {
			r.Predict = "exhausts ~" + f.P50.Local().Format("Mon 15:04")
			if f.P90 != nil {
				r.Predict += " (P90 " + f.P90.Local().Format("15:04") + ")"
			}
		}
		if r.Classification == "" && (r.Status == "unmeasured" || r.Status == "not-configured") {
			r.Classification = "predicted"
		}
		if f.P50 != nil && f.P50.Before(f.ResetAt) && f.Verdict != "learning" && f.Verdict != "relearning" && r.SuggestedWorkers != nil {
			toExhaust := f.P50.Sub(now).Hours()
			toReset := f.ResetAt.Sub(now).Hours()
			if toReset > 0 {
				n := int(math.Floor(float64(*r.SuggestedWorkers) * toReset / math.Max(toExhaust, 1.0/3600)))
				if n < 0 {
					n = 0
				}
				r.SuggestedWorkers = &n
			}
		}
	}
}
