package llmbudget

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/qiangli/yoke/pkg/broker/door"
	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/quotaobs"
)

// AnalyzerBackend is the paid model boundary. A fake backend can be supplied
// by tests; production uses the local door or the fleet launch contract.
type AnalyzerBackend interface {
	Analyze(context.Context, string, string, string) ([]byte, error)
}

type AnalyzeOptions struct {
	Now     time.Time
	Backend AnalyzerBackend
}

type AnalyzeResult struct {
	Matched  int    `json:"matched"`
	Analyzed int    `json:"analyzed"`
	Cached   int    `json:"cached"`
	Invalid  int    `json:"invalid"`
	Hint     string `json:"hint,omitempty"`
}

type ObservedState struct {
	Account        string                 `json:"account"`
	Plan           string                 `json:"plan,omitempty"`
	Tool           string                 `json:"tool,omitempty"`
	Kind           string                 `json:"kind"`
	Window         string                 `json:"window,omitempty"`
	UsedPercent    *float64               `json:"used_percent,omitempty"`
	ResetAt        *time.Time             `json:"reset_at,omitempty"`
	Classification string                 `json:"classification"`
	Signal         string                 `json:"signal,omitempty"`
	At             time.Time              `json:"at"`
	Summary        string                 `json:"summary,omitempty"`
	Matcher        *quotaobs.ParsedSignal `json:"matcher,omitempty"`
	Model          *quotaobs.Analysis     `json:"model,omitempty"`
}

type annotation struct {
	ExcerptSHA     string                 `json:"excerpt_sha"`
	Model          string                 `json:"model,omitempty"`
	Parsed         *quotaobs.ParsedSignal `json:"parsed,omitempty"`
	Analysis       *quotaobs.Analysis     `json:"analysis,omitempty"`
	AccountSummary string                 `json:"account_summary,omitempty"`
}

func annotationPath() (string, error) {
	home := os.Getenv("BASHY_HOME")
	if home == "" {
		h, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		home = filepath.Join(h, ".bashy")
	}
	return filepath.Join(home, "quota", "analysis.jsonl"), nil
}

func readAnnotations() (map[string]annotation, error) {
	p, err := annotationPath()
	if err != nil {
		return nil, err
	}
	f, err := os.Open(p)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]annotation{}, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	out := map[string]annotation{}
	r := bufio.NewReader(f)
	for {
		line, err := r.ReadBytes('\n')
		if err == io.EOF {
			break
		} // incomplete append is retried next read
		if err != nil {
			return nil, err
		}
		var a annotation
		if json.Unmarshal(line, &a) != nil || a.ExcerptSHA == "" {
			continue
		}
		key := a.ExcerptSHA + "\x00" + a.Model
		out[key] = a
	}
	return out, nil
}

func appendAnnotations(rows []annotation) error {
	if len(rows) == 0 {
		return nil
	}
	p, err := annotationPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
		return err
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	for _, row := range rows {
		b, err := json.Marshal(row)
		if err != nil {
			return err
		}
		if _, err := f.Write(append(b, '\n')); err != nil {
			return err
		}
	}
	return f.Sync()
}

func matchObservation(cat *fleet.Catalog, o quotaobs.Observation) *quotaobs.ParsedSignal {
	var signals []fleet.UsageSignal
	if p, ok := cat.Plan(o.Plan); ok {
		signals = append(signals, p.UsageSignals...)
	}
	if t, ok := cat.Tool(o.Tool); ok {
		signals = append(signals, t.UsageSignals...)
	}
	got := fleet.MatchSignals(signals, o.Excerpt, o.Headers, o.At)
	if len(got) == 0 {
		return nil
	}
	x := got[0]
	return &quotaobs.ParsedSignal{Signal: x.Signal, Kind: x.Kind, Window: x.Window, UsedPercent: x.UsedPercent, ResetAt: x.ResetAt}
}

// MatchObservations runs the free deterministic pass and persists annotations.
// The vendor journal remains append-only; sidecar entries are keyed by excerpt_sha.
func MatchObservations(ctx context.Context) (int, error) {
	obs, err := quotaobs.Read(time.Time{}, nil)
	if err != nil {
		return 0, err
	}
	cache, err := readAnnotations()
	if err != nil {
		return 0, err
	}
	cat := fleet.New()
	var rows []annotation
	for _, o := range obs {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		if o.Parsed != nil || cache[o.ExcerptSHA+"\x00"].Parsed != nil {
			continue
		}
		if p := matchObservation(cat, o); p != nil {
			a := annotation{ExcerptSHA: o.ExcerptSHA, Parsed: p}
			rows = append(rows, a)
			cache[o.ExcerptSHA+"\x00"] = a
		}
	}
	return len(rows), appendAnnotations(rows)
}

func Analyze(ctx context.Context, opt AnalyzeOptions) (AnalyzeResult, error) {
	return defaultGate.Analyze(ctx, opt)
}

// Analyze is the only entry point that may call a model. Quota and seat
// selection never call it; callers opt in through quota refresh/analyze.
func (g *Gate) Analyze(ctx context.Context, opt AnalyzeOptions) (AnalyzeResult, error) {
	var result AnalyzeResult
	n, err := MatchObservations(ctx)
	result.Matched = n
	if err != nil {
		return result, err
	}
	p, err := g.policy()
	if err != nil {
		return result, err
	}
	if !p.Analyzer.Enabled {
		result.Hint = "enable the analyzer with quota enable analyzer"
		return result, nil
	}
	obs, err := quotaobs.Read(time.Time{}, nil)
	if err != nil {
		return result, err
	}
	cache, err := readAnnotations()
	if err != nil {
		return result, err
	}
	seen := map[string]bool{}
	var pending []quotaobs.Observation
	for _, o := range obs {
		if o.ExcerptSHA == "" || seen[o.ExcerptSHA] {
			continue
		}
		seen[o.ExcerptSHA] = true
		pending = append(pending, o)
	}
	if len(pending) == 0 {
		return result, nil
	}
	max := p.Analyzer.MaxBatch
	if max <= 0 {
		max = 20
	}
	for len(pending) > 0 {
		batch := pending
		if len(batch) > max {
			batch = batch[:max]
		}
		pending = pending[len(batch):]
		model, agent, hint := g.chooseAnalyzer(ctx, p.Analyzer, batch, opt.Backend == nil)
		if model == "" {
			result.Hint = hint
			return result, nil
		}
		var todo []quotaobs.Observation
		for _, o := range batch {
			if cache[o.ExcerptSHA+"\x00"+model].Analysis != nil {
				result.Cached++
				continue
			}
			todo = append(todo, o)
		}
		if len(todo) == 0 {
			continue
		}
		backend := opt.Backend
		if backend == nil {
			backend = liveAnalyzer{}
		}
		prompt, _ := json.Marshal(todo)
		instruction := "Analyze each vendor quota excerpt. Return strict JSON object with analyses array and account_summaries object (account to one rolling summary sentence). Each analysis must contain excerpt_sha, kind (exhausted|rate-limited|warning|unrelated), window, used_percent (number or null), reset_at (RFC3339 or null; resolve relative times against observation At), account_hint, confidence (0..1), summary (<=120 chars). No other fields. Observations: " + string(prompt)
		raw, err := backend.Analyze(ctx, model, agent, instruction)
		if err != nil {
			return result, err
		}
		var response struct {
			Analyses         []json.RawMessage `json:"analyses"`
			AccountSummaries map[string]string `json:"account_summaries"`
		}
		if err := strictJSON(raw, &response); err != nil {
			result.Invalid += len(todo)
			continue
		}
		bySHA := map[string]quotaobs.Observation{}
		for _, o := range todo {
			bySHA[o.ExcerptSHA] = o
		}
		var rows []annotation
		for _, rawItem := range response.Analyses {
			var a quotaobs.Analysis
			if strictJSON(rawItem, &a) != nil || !hasAnalysisFields(rawItem) {
				continue
			}
			o, ok := bySHA[a.ExcerptSHA]
			if !ok || !validAnalysis(a, o.At) {
				continue
			}
			delete(bySHA, a.ExcerptSHA)
			row := annotation{ExcerptSHA: a.ExcerptSHA, Model: model, Analysis: &a, AccountSummary: response.AccountSummaries[o.Account]}
			rows = append(rows, row)
			cache[a.ExcerptSHA+"\x00"+model] = row
			result.Analyzed++
		}
		result.Invalid += len(bySHA)
		if err := appendAnnotations(rows); err != nil {
			return result, err
		}
	}
	return result, nil
}

func hasAnalysisFields(raw []byte) bool {
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil {
		return false
	}
	for _, name := range []string{"excerpt_sha", "kind", "window", "used_percent", "reset_at", "account_hint", "confidence", "summary"} {
		if _, ok := fields[name]; !ok {
			return false
		}
	}
	return len(fields) == 8
}

func strictJSON(b []byte, dst any) error {
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err := d.Decode(dst); err != nil {
		return err
	}
	if d.Decode(new(any)) != io.EOF {
		return errors.New("trailing JSON")
	}
	return nil
}

func validAnalysis(a quotaobs.Analysis, at time.Time) bool {
	switch a.Kind {
	case "exhausted", "rate-limited", "warning", "unrelated":
	default:
		return false
	}
	if len(a.Summary) > 120 || a.Confidence < 0 || a.Confidence > 1 || a.Confidence != a.Confidence {
		return false
	}
	if a.UsedPercent != nil && (*a.UsedPercent < 0 || *a.UsedPercent > 100) {
		return false
	}
	if a.ResetAt != nil && (!a.ResetAt.After(at) || a.ResetAt.After(at.Add(31*24*time.Hour))) {
		return false
	}
	return true
}

// EnableAnalyzer records explicit consent before any model pass can spend quota.
func EnableAnalyzer() error            { return defaultGate.setAnalyzerEnabled(context.Background(), true) }
func DisableAnalyzer() error           { return defaultGate.setAnalyzerEnabled(context.Background(), false) }
func (g *Gate) EnableAnalyzer() error  { return g.setAnalyzerEnabled(context.Background(), true) }
func (g *Gate) DisableAnalyzer() error { return g.setAnalyzerEnabled(context.Background(), false) }

func (g *Gate) setAnalyzerEnabled(ctx context.Context, enabled bool) error {
	path := g.cfg.PolicyPath
	if path == "" {
		path = os.Getenv("BASHY_LLM_BUDGET_POLICY")
	}
	if path == "" && g.cfg.StatePath != "" {
		path = filepath.Join(filepath.Dir(g.cfg.StatePath), "llm-budget-policy.json")
	}
	if path == "" || g.cfg.Policy != nil {
		return errors.New("llmbudget: no writable file policy authority")
	}
	lock, err := acquireMeter(ctx, path+".lock")
	if err != nil {
		return err
	}
	defer lock.Release()
	p, err := g.policy()
	if err != nil {
		return err
	}
	p.missing = false
	p.Analyzer.Enabled = enabled
	return atomicJSON(path, p)
}

func Observed(ctx context.Context, now time.Time) []ObservedState {
	if now.IsZero() {
		now = time.Now()
	}
	_, _ = MatchObservations(ctx)
	obs, err := quotaobs.Read(time.Time{}, nil)
	if err != nil {
		return nil
	}
	cache, err := readAnnotations()
	if err != nil {
		return nil
	}
	cat := fleet.New()
	var out []ObservedState
	for _, o := range obs {
		if o.At.After(now) {
			continue
		}
		parsed := o.Parsed
		if parsed == nil {
			parsed = cache[o.ExcerptSHA+"\x00"].Parsed
		}
		if parsed == nil {
			parsed = matchObservation(cat, o)
		}
		var model *quotaobs.Analysis
		summary := ""
		for _, a := range cache {
			if a.ExcerptSHA == o.ExcerptSHA && a.Analysis != nil {
				model = a.Analysis
				summary = a.AccountSummary
				break
			}
		}
		if o.Analysis != nil {
			model = o.Analysis
		}
		if model != nil && model.Kind == "unrelated" && parsed == nil {
			continue
		}
		if parsed == nil && model == nil {
			continue
		}
		s := ObservedState{Account: o.Account, Plan: o.Plan, Tool: o.Tool, At: o.At, Matcher: parsed, Model: model, Summary: summary}
		if parsed != nil {
			s.Kind, s.Window, s.UsedPercent, s.ResetAt, s.Signal = parsed.Kind, parsed.Window, parsed.UsedPercent, parsed.ResetAt, parsed.Signal
			s.Classification = "observed-unconfirmed"
		}
		if model != nil {
			s.Kind, s.Window, s.UsedPercent, s.ResetAt = model.Kind, model.Window, model.UsedPercent, model.ResetAt
			if parsed == nil {
				s.Classification = "inferred"
			}
			if parsed != nil {
				s.Classification = "conflict"
				if parsed.Kind == model.Kind && resetsAgree(parsed.ResetAt, model.ResetAt) {
					s.Classification = "observed"
				}
			}
			if s.Summary == "" {
				s.Summary = model.Summary
			}
		}
		if s.ResetAt != nil {
			if !now.Before(*s.ResetAt) {
				continue
			}
		} else if now.Sub(o.At) > 6*time.Hour {
			continue
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].At.After(out[j].At) })
	return out
}

func resetsAgree(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	d := a.Sub(*b)
	return d >= -10*time.Minute && d <= 10*time.Minute
}

func (g *Gate) chooseAnalyzer(ctx context.Context, policy AnalyzerPolicy, batch []quotaobs.Observation, allowDoor bool) (string, string, string) {
	cat := fleet.New()
	excluded := map[string]bool{}
	for _, o := range batch {
		excluded[o.Account] = true
		excluded[o.Plan] = true
	}
	if policy.Model != "" {
		if m, ok := cat.Model(policy.Model); ok && excluded[quotaAccount(m)] {
			return "", "", "analyzer would measure its own account"
		}
		if allowDoor && doorListsModel(ctx, policy.Model) {
			return policy.Model, "", ""
		}
		agents, _ := cat.Agents()
		for _, a := range agents {
			if a.Model == policy.Model {
				return policy.Model, a.Name, ""
			}
		}
		// A fake backend can evaluate an explicit model without a live binding.
		return policy.Model, "", ""
	}
	if policy.Agent != "" {
		a, ok := cat.Agent(policy.Agent)
		if !ok {
			return "", "", "analyzer agent is unavailable"
		}
		m, ok := cat.Model(a.Model)
		if !ok || excluded[quotaAccount(m)] {
			return "", "", "analyzer would measure its own account"
		}
		if allowDoor && doorListsModel(ctx, m.Name) {
			return m.Name, "", ""
		}
		return m.Name, a.Name, ""
	}
	models, _ := cat.Models()
	for _, m := range models {
		if m.Kind != fleet.ModelKindLocal {
			continue
		}
		if allowDoor && doorListsModel(ctx, m.Name) {
			return m.Name, "", ""
		}
	}
	agents, _ := cat.Agents()
	var candidates []analyzerCandidate
	for _, a := range agents {
		m, ok := cat.Model(a.Model)
		if !ok {
			continue
		}
		h, _, known, err := g.CachedMeasuredHeadroom(ctx, m.Name, a.Name)
		if err == nil && known {
			candidates = append(candidates, analyzerCandidate{Model: m.Name, Agent: a.Name, Account: quotaAccount(m), Headroom: h})
		}
	}
	model, agent := bestAnalyzerCandidate(candidates, excluded)
	if model == "" {
		return "", "", "no local model or independent measured fleet account; set quota analyzer --model"
	}
	if allowDoor && doorListsModel(ctx, model) {
		return model, "", ""
	}
	return model, agent, ""
}

type analyzerCandidate struct {
	Model, Agent, Account string
	Headroom              float64
}

func bestAnalyzerCandidate(candidates []analyzerCandidate, excluded map[string]bool) (string, string) {
	best := -1.0
	var model, agent string
	for _, c := range candidates {
		if c.Account == "" || excluded[c.Account] || c.Headroom <= best {
			continue
		}
		best, model, agent = c.Headroom, c.Model, c.Agent
	}
	return model, agent
}

func doorListsModel(ctx context.Context, model string) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, door.BaseURL()+"/v1/models", nil)
	if err != nil {
		return false
	}
	if token, err := door.Token(); err == nil {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false
	}
	var v struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&v) != nil {
		return false
	}
	for _, item := range v.Data {
		if item.ID == model {
			return true
		}
	}
	return false
}

type liveAnalyzer struct{}

func (liveAnalyzer) Analyze(ctx context.Context, model, agent, prompt string) ([]byte, error) {
	cat := fleet.New()
	if agent == "" {
		payload, _ := json.Marshal(map[string]any{"model": model, "messages": []map[string]string{{"role": "user", "content": prompt}}, "temperature": 0})
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, door.BaseURL()+"/v1/chat/completions", bytes.NewReader(payload))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		if token, err := door.Token(); err == nil {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := (&http.Client{Timeout: 2 * time.Minute}).Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("analyzer door status %d", resp.StatusCode)
		}
		var completion struct {
			Choices []struct {
				Message struct {
					Content string `json:"content"`
				} `json:"message"`
			} `json:"choices"`
		}
		if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&completion); err != nil {
			return nil, err
		}
		if len(completion.Choices) == 0 {
			return nil, errors.New("analyzer door returned no choice")
		}
		return []byte(completion.Choices[0].Message.Content), nil
	}
	a, ok := cat.Agent(agent)
	if !ok {
		return nil, errors.New("analyzer agent unavailable")
	}
	t, ok := cat.Tool(a.Tool)
	if !ok {
		return nil, errors.New("analyzer tool unavailable")
	}
	m, ok := cat.Model(model)
	if !ok {
		return nil, errors.New("analyzer model unavailable")
	}
	argv := t.Argv(m.TargetFor(t.Name), prompt)
	if len(argv) == 0 {
		return nil, errors.New("analyzer launch unavailable")
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Stdin = strings.NewReader(prompt)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("analyzer launch: %w", err)
	}
	return out, nil
}
