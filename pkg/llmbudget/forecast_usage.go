package llmbudget

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/otelquery"
)

// UsageSample is one observed request. Cached tokens are part of Input, not extra spend.
type UsageSample struct {
	At      time.Time `json:"at"`
	Agent   string    `json:"agent,omitempty"`
	Tool    string    `json:"tool,omitempty"`
	Model   string    `json:"model,omitempty"`
	Account string    `json:"account,omitempty"`
	Input   int64     `json:"input"`
	Output  int64     `json:"output"`
	Cached  int64     `json:"cached,omitempty"`
	Source  string    `json:"source,omitempty"`
	Key     string    `json:"key,omitempty"`
}

func usageHome() string {
	if p := os.Getenv("BASHY_HOME"); p != "" {
		return p
	}
	h, _ := os.UserHomeDir()
	return filepath.Join(h, ".bashy")
}
func usageNumber(v any) int64 {
	switch n := v.(type) {
	case float64:
		return int64(n)
	case string:
		x, _ := strconv.ParseFloat(n, 64)
		return int64(x)
	}
	return 0
}
func usageString(v any) string { s, _ := v.(string); return s }
func usageTime(v any) time.Time {
	if s := usageString(v); s != "" {
		if t, e := time.Parse(time.RFC3339Nano, s); e == nil {
			return t
		}
		if n, e := strconv.ParseFloat(s, 64); e == nil {
			return time.Unix(0, int64(n*1e9))
		}
	}
	if n, ok := v.(float64); ok {
		return time.Unix(0, int64(n*1e9))
	}
	return time.Time{}
}
func usageKey(s UsageSample) string {
	// Event timestamps may differ slightly between a CLI log and a span.
	b, _ := json.Marshal([]any{s.At.UTC().Truncate(time.Minute).Unix(), s.Agent, s.Model, s.Input, s.Output, s.Cached})
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:12])
}
func accountMaps() (map[string]string, map[string]string, map[string]string) {
	c := fleet.New()
	models, _ := c.Models()
	agents, _ := c.Agents()
	modelAccount := map[string]string{}
	modelName := map[string]string{}
	agentModel := map[string]string{}
	for _, m := range models {
		modelAccount[m.Name] = quotaAccount(m)
		modelName[m.Name] = m.Name
		if m.UpstreamID != "" {
			modelName[m.UpstreamID] = m.Name
		}
		for _, alias := range m.Aliases {
			modelName[alias] = m.Name
		}
		for _, id := range m.ToolIDs {
			modelName[id] = m.Name
		}
	}
	for _, a := range agents {
		agentModel[a.Name] = a.Model
	}
	return modelAccount, modelName, agentModel
}
func normalizeUsage(s *UsageSample, accounts, names, agents map[string]string) bool {
	if s.At.IsZero() || s.Input < 0 || s.Output < 0 || s.Input+s.Output == 0 {
		return false
	}
	if m := agents[s.Agent]; m != "" && s.Model == "" {
		s.Model = m
	}
	if m := agents[s.Model]; m != "" {
		s.Model = m
	}
	if m := names[s.Model]; m != "" {
		s.Model = m
	}
	if s.Account == "" {
		s.Account = accounts[s.Model]
	}
	if s.Account == "" {
		return false
	}
	if s.Tool == "" {
		if i := strings.IndexByte(s.Agent, '-'); i > 0 {
			s.Tool = s.Agent[:i]
		} else {
			s.Tool = s.Agent
		}
	}
	if s.Cached > s.Input {
		s.Cached = s.Input
	}
	if s.Key == "" {
		s.Key = usageKey(*s)
	}
	return true
}
func readUsageLines(path string, fn func(map[string]any)) {
	f, e := os.Open(path)
	if e != nil {
		return
	}
	defer f.Close()
	st, e := f.Stat()
	if e == nil && st.Size() > 16<<20 {
		_, _ = f.Seek(st.Size()-(16<<20), 0)
		sc := bufio.NewScanner(f)
		sc.Scan()
		for sc.Scan() {
			var v map[string]any
			if json.Unmarshal(sc.Bytes(), &v) == nil {
				fn(v)
			}
		}
		return
	}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 4096), 1<<20)
	for sc.Scan() {
		var v map[string]any
		if json.Unmarshal(sc.Bytes(), &v) == nil {
			fn(v)
		}
	}
}
func samplesFromFiles(since, until time.Time) []UsageSample {
	home := usageHome()
	var out []UsageSample
	readUsageLines(filepath.Join(home, "cligw", "usage.jsonl"), func(v map[string]any) {
		if usageString(v["schema_version"]) != "bashy-cligw-usage-v1" {
			return
		}
		u, _ := v["usage"].(map[string]any)
		s := UsageSample{At: usageTime(v["at"]), Agent: usageString(v["agent"]), Tool: usageString(v["command"]), Model: usageString(v["model"]), Input: usageNumber(u["prompt_tokens"]), Output: usageNumber(u["completion_tokens"]), Source: "cligw"}
		if !s.At.Before(since) && s.At.Before(until) {
			out = append(out, s)
		}
	})
	readUsageLines(filepath.Join(home, "broker", "audit.jsonl"), func(v map[string]any) {
		s := UsageSample{At: usageTime(v["time"]), Agent: usageString(v["principal"]), Tool: usageString(v["backend"]), Model: usageString(v["model"]), Input: usageNumber(v["prompt_tokens"]), Output: usageNumber(v["completion_tokens"]), Source: "broker"}
		if !s.At.Before(since) && s.At.Before(until) {
			out = append(out, s)
		}
	})
	// Queue files carry log_path, agent and model without requiring a weave import.
	root := filepath.Join(home, "weave")
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, e error) error {
		if e != nil || d == nil {
			return nil
		}
		if d.IsDir() {
			rel, _ := filepath.Rel(root, path)
			if strings.Count(rel, string(filepath.Separator)) > 0 {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, "queue.json") && !strings.HasSuffix(path, "queue.jsonl") {
			return nil
		}
		queueBytes, err := os.ReadFile(path)
		if err != nil || len(queueBytes) > 8<<20 {
			return nil
		}
		var queue map[string]any
		if json.Unmarshal(queueBytes, &queue) != nil {
			return nil
		}
		func(v map[string]any) {
			items, _ := v["items"].([]any)
			if items == nil {
				items = []any{v}
			}
			for _, raw := range items {
				item, ok := raw.(map[string]any)
				if !ok {
					continue
				}
				log := usageString(item["log_path"])
				if log == "" {
					continue
				}
				st, x := os.Stat(log)
				if x != nil || st.ModTime().Before(until.Add(-7*24*time.Hour)) {
					continue
				}
				readUsageLines(log, func(ev map[string]any) {
					typ := usageString(ev["type"])
					if typ != "result" && typ != "turn.completed" {
						return
					}
					u, _ := ev["usage"].(map[string]any)
					if typ == "turn.completed" {
						u, _ = ev["usage"].(map[string]any)
					}
					s := UsageSample{At: usageTime(ev["timestamp"]), Agent: usageString(item["owner"]), Tool: usageString(item["tool"]), Model: usageString(item["coding_agent"]), Input: usageNumber(u["input_tokens"]), Output: usageNumber(u["output_tokens"]), Cached: usageNumber(u["cached_input_tokens"]), Source: "weave"}
					if s.At.IsZero() {
						s.At = st.ModTime()
					}
					if !s.At.Before(since) && s.At.Before(until) {
						out = append(out, s)
					}
				})
				// Weave's normal stream-json writer distills Codex usage into a short line.
				f, err := os.Open(log)
				if err == nil {
					scan := bufio.NewScanner(f)
					scan.Buffer(make([]byte, 4096), 1<<20)
					for scan.Scan() {
						line := scan.Text()
						if !strings.HasPrefix(line, "[turn completed") && !strings.HasPrefix(line, "[result") {
							continue
						}
						field := func(name string) int64 {
							i := strings.Index(line, name+"=")
							if i < 0 {
								return 0
							}
							part := line[i+len(name)+1:]
							j := strings.IndexAny(part, " ]")
							if j >= 0 {
								part = part[:j]
							}
							n, _ := strconv.ParseInt(part, 10, 64)
							return n
						}
						s := UsageSample{At: st.ModTime(), Agent: usageString(item["owner"]), Tool: usageString(item["tool"]), Model: usageString(item["coding_agent"]), Input: field("input"), Output: field("output"), Cached: field("cached"), Source: "weave"}
						if !s.At.Before(since) && s.At.Before(until) && s.Input+s.Output > 0 {
							out = append(out, s)
						}
					}
					f.Close()
				}
			}
		}(queue)
		return nil
	})
	return out
}
func samplesFromOTel(ctx context.Context, since, until time.Time) []UsageSample {
	c := otelquery.NewClient("")
	ctx, cancel := context.WithTimeout(ctx, 1500*time.Millisecond)
	defer cancel()
	duration := time.Since(since)
	if duration < 0 {
		return nil
	}
	if duration > 7*24*time.Hour {
		duration = 7 * 24 * time.Hour
	}
	rows, _, e := c.Spans(ctx, `name:chat* OR name:gen_ai.turn`, duration, 5000)
	if e != nil {
		return nil
	} // telemetry is optional
	var out []UsageSample
	for _, v := range rows {
		get := func(k string) any {
			if x := v[k]; x != nil {
				return x
			}
			return v["span_attr:"+k]
		}
		s := UsageSample{At: usageTime(v["_time"]), Agent: usageString(get("agent.name")), Tool: usageString(get("agent.tool")), Model: usageString(get("gen_ai.request.model")), Input: usageNumber(get("gen_ai.usage.input_tokens")), Output: usageNumber(get("gen_ai.usage.output_tokens")), Source: "otel"}
		if s.Agent == "" {
			s.Agent = usageString(get("agent.principal"))
		}
		if s.At.IsZero() {
			s.At = usageTime(v["start_time"])
		}
		if !s.At.Before(since) && s.At.Before(until) {
			out = append(out, s)
		}
	}
	// Counter metrics are a fallback when the span store has no request-level usage.
	if len(out) == 0 {
		for _, query := range []string{`sum(increase(ycode.llm.tokens.total[1h])) by (model,agent,tool,type)`, `sum(increase(ycode.fleet.tokens[1h])) by (model,agent,tool,type)`} {
			series, _, e := c.Metrics(ctx, query)
			if e == nil {
				for _, m := range series {
					s := UsageSample{At: until.Add(-time.Minute), Agent: m.Labels["agent"], Tool: m.Labels["tool"], Model: m.Labels["model"], Source: "otel-metric"}
					if m.Labels["type"] == "output" {
						s.Output = int64(m.Value)
					} else {
						s.Input = int64(m.Value)
					}
					out = append(out, s)
				}
			}
			if len(out) > 0 {
				break
			}
		}
	}
	return out
}

// TokenUsage merges local request logs and optional telemetry, retaining one copy of each request.
func TokenUsage(ctx context.Context, since, until time.Time) []UsageSample {
	accounts, names, agents := accountMaps()
	all := append(samplesFromFiles(since, until), samplesFromOTel(ctx, since, until)...)
	seen := map[string]bool{}
	out := make([]UsageSample, 0, len(all))
	for _, s := range all {
		if !normalizeUsage(&s, accounts, names, agents) || seen[s.Key] {
			continue
		}
		if s.Source == "otel-metric" {
			covered := false
			for _, old := range out {
				if old.Account == s.Account && old.Model == s.Model && old.Source != "otel-metric" && old.At.Truncate(time.Hour).Equal(s.At.Truncate(time.Hour)) {
					covered = true
					break
				}
			}
			if covered {
				continue
			}
		}
		seen[s.Key] = true
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].At.Before(out[j].At) })
	return out
}
