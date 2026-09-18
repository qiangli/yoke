package resources

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/otelquery"
	"github.com/qiangli/yoke/pkg/weave"
)

// Canonical provider names in requested order.
var CanonicalProviders = []string{
	"Anthropic",
	"OpenAI",
	"Google",
	"Zhipu",
	"Moonshot",
	"DeepSeek",
}

// FleetGroup represents resource utilization for one Provider and Band.
type FleetGroup struct {
	Provider     string   `json:"provider"`
	Band         string   `json:"band"`
	BandNum      int      `json:"band_num"`
	Total        int      `json:"total"`
	Busy         int      `json:"busy"`
	Idle         int      `json:"idle"`
	Cooling      int      `json:"cooling"`
	Unavailable  int      `json:"unavailable"`
	Subscription int      `json:"subscription"`
	APIKey       int      `json:"api_key"`
	Tokens       *int64   `json:"tokens,omitempty"`
	CostUSD      *float64 `json:"cost_usd,omitempty"`
	MeterPresent bool     `json:"meter_present"`
}

// FleetTotals holds aggregate utilization stats across all groups.
type FleetTotals struct {
	Total        int      `json:"total"`
	Busy         int      `json:"busy"`
	Idle         int      `json:"idle"`
	Cooling      int      `json:"cooling"`
	Unavailable  int      `json:"unavailable"`
	Subscription int      `json:"subscription"`
	APIKey       int      `json:"api_key"`
	Tokens       *int64   `json:"tokens,omitempty"`
	CostUSD      *float64 `json:"cost_usd,omitempty"`
	MeterPresent bool     `json:"meter_present"`
	// Unattributed is the number of live runs whose agent, model, and tool do
	// not resolve to any catalog agent. It is separate from Busy because those
	// runs consume no identifiable catalog slot, but must remain visible.
	Unattributed int `json:"unattributed"`
}

// FleetResources represents the complete `bashy resources fleet` envelope.
type FleetResources struct {
	SchemaVersion string       `json:"schema_version"`
	GeneratedAt   time.Time    `json:"generated_at"`
	Groups        []FleetGroup `json:"groups"`
	Totals        FleetTotals  `json:"totals"`
	MeterPresent  bool         `json:"meter_present"`
	// IdleAgents names the agents counted in Totals.Idle. The counts alone
	// cannot answer "which agent could take this issue" — the utilization
	// verdict needs the names, so the collector records them here.
	IdleAgents []IdleAgent `json:"idle_agents,omitempty"`
}

type BoardAgent struct {
	Name         string
	Tool         string
	Model        string
	Band         int
	Available    bool
	Found        bool
	Availability string
	State        string
}

type BoardRun struct {
	State string
	Tool  string
	Agent string
	Model string
}

type busyRun struct {
	Tool  string
	Agent string
	Model string
}

type wireEnvelope struct {
	Status string          `json:"status"`
	Result json.RawMessage `json:"result"`
}

// CanonicalProvider maps model/provider names to the six canonical providers.
func CanonicalProvider(modelName, providerStr string) string {
	prov := strings.ToLower(providerStr)
	mod := strings.ToLower(modelName)

	switch {
	case strings.Contains(prov, "anthropic") || strings.Contains(mod, "claude") || strings.Contains(mod, "fable") || strings.Contains(mod, "haiku") || strings.Contains(mod, "opus") || strings.Contains(mod, "sonnet"):
		return "Anthropic"
	case (strings.Contains(prov, "openai") && !strings.Contains(prov, "openai-compat")) || strings.Contains(mod, "gpt") || strings.Contains(mod, "codex"):
		return "OpenAI"
	case strings.Contains(prov, "google") || strings.Contains(prov, "gemini") || strings.Contains(mod, "gemini") || strings.Contains(mod, "agy"):
		return "Google"
	case strings.Contains(prov, "zhipu") || strings.Contains(prov, "glm") || strings.Contains(mod, "glm") || strings.Contains(prov, "z.ai"):
		return "Zhipu"
	case strings.Contains(prov, "moonshot") || strings.Contains(prov, "kimi") || strings.Contains(mod, "kimi") || strings.Contains(mod, "moonshot"):
		return "Moonshot"
	case strings.Contains(prov, "deepseek") || strings.Contains(mod, "deepseek"):
		return "DeepSeek"
	default:
		if providerStr != "" && providerStr != "openai-compat" {
			return strings.Title(providerStr)
		}
		return "Other"
	}
}

// CollectFleetResources gathers live weave availability, active run counts,
// fleet catalog metadata, and OTel cost/token metrics.
func CollectFleetResources(ctx context.Context) (*FleetResources, error) {
	return CollectFleetResourcesFromBoard(ctx, time.Time{}, nil, nil)
}

type liveAvailInfo struct {
	found        bool
	available    bool
	reason       string
	coolingUntil string
}

func normStr(s string) string {
	s = strings.ToLower(s)
	s = strings.ReplaceAll(s, "-", "")
	s = strings.ReplaceAll(s, "_", "")
	s = strings.ReplaceAll(s, ".", "")
	return s
}

type agentRecord struct {
	Name        string
	Tool        string
	Model       string
	Provider    string
	Band        int
	Kind        string
	BillingMode string
	Aliases     []string
}

func attributeBusyRuns(records []agentRecord, runs []busyRun) (map[string]bool, int) {
	busy := make(map[string]bool)
	unattributed := 0

	matching := func(run busyRun, mode string) []int {
		var matches []int
		for i, rec := range records {
			matched := false
			switch mode {
			case "agent":
				if run.Agent != "" {
					for _, name := range append([]string{rec.Name}, rec.Aliases...) {
						if normStr(name) == normStr(run.Agent) {
							matched = true
							break
						}
					}
				}
			case "model":
				matched = run.Model != "" && normStr(rec.Model) == normStr(run.Model)
			case "tool":
				matched = run.Tool != "" && normStr(rec.Tool) == normStr(run.Tool)
			}
			if matched {
				matches = append(matches, i)
			}
		}
		sort.Slice(matches, func(i, j int) bool {
			return records[matches[i]].Name < records[matches[j]].Name
		})
		return matches
	}

	for _, run := range runs {
		var candidates []int
		for _, mode := range []string{"agent", "model", "tool"} {
			if candidates = matching(run, mode); len(candidates) > 0 {
				break
			}
		}
		if len(candidates) == 0 {
			unattributed++
			continue
		}
		chosen := candidates[0]
		for _, candidate := range candidates {
			if !busy[records[candidate].Name] {
				chosen = candidate
				break
			}
		}
		busy[records[chosen].Name] = true
	}
	return busy, unattributed
}

// CollectFleetResourcesFromBoard builds FleetResources from provided board agents/runs,
// or queries the live catalog and weave state if nil.
func CollectFleetResourcesFromBoard(ctx context.Context, at time.Time, bAgents []BoardAgent, bRuns []BoardRun) (*FleetResources, error) {
	if at.IsZero() {
		at = time.Now().UTC()
	}
	cat := fleet.New()

	availMap := map[string]liveAvailInfo{}
	var busyRuns []busyRun

	var records []agentRecord

	if bAgents != nil {
		for _, a := range bAgents {
			availMap[a.Name] = liveAvailInfo{
				found:     a.Found,
				available: a.Available,
				reason:    a.Availability,
				coolingUntil: func() string {
					if strings.HasPrefix(a.Availability, "cooling") {
						return a.Availability
					}
					return ""
				}(),
			}
			// Try resolving from catalog for richer metadata
			rec := agentRecord{
				Name:  a.Name,
				Tool:  a.Tool,
				Model: a.Model,
				Band:  a.Band,
			}
			if resolved, _, m, err := cat.Binding(a.Name); err == nil {
				rec.Tool = resolved.Tool
				rec.Model = m.Name
				rec.Provider = m.Provider
				if rec.Band <= 0 {
					rec.Band = m.Band
				}
				rec.Kind = m.Kind
				rec.BillingMode = m.BillingMode()
				rec.Aliases = resolved.Names()
			}
			records = append(records, rec)
		}
		for _, r := range bRuns {
			if r.State == "working" || r.State == "allocated" {
				busyRuns = append(busyRuns, busyRun{Agent: r.Agent, Tool: r.Tool, Model: r.Model})
			}
		}
		// Runs are authoritative when supplied. Agent state is retained as a
		// fallback for callers that have an agent snapshot but no run records.
		if bRuns == nil {
			for _, a := range bAgents {
				if a.State == "working" {
					busyRuns = append(busyRuns, busyRun{Agent: a.Name, Tool: a.Tool, Model: a.Model})
				}
			}
		}
	} else {
		// Live availability from `weave fleet --agents --json`
		type availability struct {
			Agent        string `json:"agent"`
			Tool         string `json:"tool"`
			Model        string `json:"model"`
			Reason       string `json:"reason"`
			CoolingUntil string `json:"cooling_until"`
			Available    bool   `json:"available"`
			Found        bool   `json:"found"`
		}
		rawAvail, err := runCobraJSON("fleet", "--agents", "--json")
		if err == nil {
			var env wireEnvelope
			var res struct {
				Tools []availability `json:"tools"`
			}
			if json.Unmarshal(rawAvail, &env) == nil && json.Unmarshal(env.Result, &res) == nil {
				for _, row := range res.Tools {
					availMap[row.Agent] = liveAvailInfo{
						found:        row.Found,
						available:    row.Available,
						reason:       row.Reason,
						coolingUntil: row.CoolingUntil,
					}
				}
			}
		}

		// Active runs from `weave list --all --json`
		rawRuns, err := runCobraJSON("list", "--all", "--json")
		if err == nil {
			var env wireEnvelope
			var res struct {
				Queues []struct {
					Items []struct {
						State  string `json:"state"`
						Tool   string `json:"tool"`
						Model  string `json:"model"`
						Launch *struct {
							Agent string `json:"agent"`
							Model string `json:"model"`
						} `json:"launch_spec"`
					} `json:"items"`
				} `json:"queues"`
			}
			if json.Unmarshal(rawRuns, &env) == nil && json.Unmarshal(env.Result, &res) == nil {
				for _, q := range res.Queues {
					for _, x := range q.Items {
						if x.State == "working" || x.State == "allocated" {
							// Owner is the conductor principal, not the launched
							// agent. Only launch_spec carries agent identity.
							agentName := ""
							modelName := x.Model
							if x.Launch != nil {
								if x.Launch.Agent != "" {
									agentName = x.Launch.Agent
								}
								if x.Launch.Model != "" {
									modelName = x.Launch.Model
								}
							}
							busyRuns = append(busyRuns, busyRun{Agent: agentName, Tool: x.Tool, Model: modelName})
						}
					}
				}
			}
		}

		agents, errs := cat.Agents()
		if len(errs) > 0 {
			return nil, errs[0]
		}
		for _, a := range agents {
			_, t, m, _ := cat.Binding(a.Name)
			bNum := a.Band
			if bNum <= 0 {
				bNum = m.Band
			}
			records = append(records, agentRecord{
				Name:        a.Name,
				Tool:        t.Name,
				Model:       m.Name,
				Provider:    m.Provider,
				Band:        bNum,
				Kind:        m.Kind,
				BillingMode: m.BillingMode(),
				Aliases:     a.Names(),
			})
		}
	}

	// Attribute each live run to one catalog agent before grouping. Explicit
	// agent/model identity wins. A tool-only record is assigned to the
	// lexicographically first not-yet-busy agent bound to that tool (the same
	// merged fleet catalog used by `weave fleet --agents`); if all are busy,
	// the first binding is reused. This stable rule makes ambiguous bare tools
	// such as claude observable without pretending that weave recorded a model.
	// A tool with no catalog binding is counted separately as unattributed.
	busyRecords, unattributed := attributeBusyRuns(records, busyRuns)

	// OTel metric store
	otelClient := otelquery.NewClient("")
	meterPresent := otelClient.Reachable(ctx)
	modelTokens := map[string]int64{}
	modelCosts := map[string]float64{}

	if meterPresent {
		if series, _, err := otelClient.Metrics(ctx, `sum(ycode.llm.tokens.total) by (model)`); err == nil {
			for _, s := range series {
				modelTokens[s.Labels["model"]] = int64(s.Value)
			}
		}
		if series, _, err := otelClient.Metrics(ctx, `sum(agent.turn.tokens) by (model)`); err == nil {
			for _, s := range series {
				modelTokens[s.Labels["model"]] += int64(s.Value)
			}
		}
		if series, _, err := otelClient.Metrics(ctx, `sum(ycode.llm.cost.dollars) by (model)`); err == nil {
			for _, s := range series {
				modelCosts[s.Labels["model"]] = s.Value
			}
		}
		if series, _, err := otelClient.Metrics(ctx, `sum(fleet.cost) by (model)`); err == nil {
			for _, s := range series {
				modelCosts[s.Labels["model"]] += s.Value
			}
		}
	}

	type groupKey struct {
		provider string
		bandNum  int
	}
	groupsMap := map[groupKey]*FleetGroup{}
	var idleAgents []IdleAgent

	for _, a := range records {
		provName := CanonicalProvider(a.Model, a.Provider)

		bNum := a.Band
		if bNum <= 0 {
			bNum = 1
		}

		key := groupKey{provider: provName, bandNum: bNum}
		grp, ok := groupsMap[key]
		if !ok {
			grp = &FleetGroup{
				Provider:     provName,
				Band:         fmt.Sprintf("L%d", bNum),
				BandNum:      bNum,
				MeterPresent: meterPresent,
			}
			groupsMap[key] = grp
		}

		billing := a.BillingMode
		isSub := a.Kind == fleet.ModelKindSubscription || billing == fleet.BillingFlat || billing == fleet.BillingFlatThenMetered
		if isSub {
			grp.Subscription++
		} else {
			grp.APIKey++
		}

		// Alias / name resolution for live availability
		names := append([]string{a.Name}, a.Aliases...)
		live, hasLive := availMap[a.Name]
		if !hasLive {
			for _, name := range names {
				if l, ok := availMap[name]; ok {
					live, hasLive = l, true
					break
				}
			}
		}

		isFound := true
		isAvail := true
		isCooling := false

		if hasLive {
			isFound = live.found
			isAvail = live.available
			if live.coolingUntil != "" || strings.Contains(live.reason, "cooling") {
				isCooling = true
			}
		} else if bAgents == nil {
			binary := a.Tool
			_, lookErr := exec.LookPath(binary)
			isFound = lookErr == nil
			isAvail = isFound
		}

		isBusy := busyRecords[a.Name]

		grp.Total++
		switch {
		case isBusy:
			grp.Busy++
		case isCooling:
			grp.Cooling++
		case !isAvail || !isFound:
			grp.Unavailable++
		default:
			grp.Idle++
			idleAgents = append(idleAgents, IdleAgent{
				Name: a.Name, Tool: a.Tool, Model: a.Model, Provider: provName, Band: bNum,
			})
		}

		if meterPresent {
			if tok, hasTok := modelTokens[a.Model]; hasTok {
				if grp.Tokens == nil {
					var zero int64
					grp.Tokens = &zero
				}
				*grp.Tokens += tok
			}
			if cost, hasCost := modelCosts[a.Model]; hasCost {
				if grp.CostUSD == nil {
					var zero float64
					grp.CostUSD = &zero
				}
				*grp.CostUSD += cost
			}
		}
	}

	provOrder := map[string]int{}
	for idx, p := range CanonicalProviders {
		provOrder[p] = idx
	}

	var groupKeys []groupKey
	for k := range groupsMap {
		groupKeys = append(groupKeys, k)
	}

	sort.Slice(groupKeys, func(i, j int) bool {
		pi, okI := provOrder[groupKeys[i].provider]
		if !okI {
			pi = 99
		}
		pj, okJ := provOrder[groupKeys[j].provider]
		if !okJ {
			pj = 99
		}
		if pi != pj {
			return pi < pj
		}
		if groupKeys[i].provider != groupKeys[j].provider {
			return groupKeys[i].provider < groupKeys[j].provider
		}
		return groupKeys[i].bandNum < groupKeys[j].bandNum
	})

	var resultGroups []FleetGroup
	var totals FleetTotals
	totals.MeterPresent = meterPresent
	totals.Unattributed = unattributed

	for _, k := range groupKeys {
		g := *groupsMap[k]
		resultGroups = append(resultGroups, g)

		totals.Total += g.Total
		totals.Busy += g.Busy
		totals.Idle += g.Idle
		totals.Cooling += g.Cooling
		totals.Unavailable += g.Unavailable
		totals.Subscription += g.Subscription
		totals.APIKey += g.APIKey

		if meterPresent {
			if g.Tokens != nil {
				if totals.Tokens == nil {
					var zero int64
					totals.Tokens = &zero
				}
				*totals.Tokens += *g.Tokens
			}
			if g.CostUSD != nil {
				if totals.CostUSD == nil {
					var zero float64
					totals.CostUSD = &zero
				}
				*totals.CostUSD += *g.CostUSD
			}
		}
	}

	sort.Slice(idleAgents, func(i, j int) bool {
		if idleAgents[i].Band != idleAgents[j].Band {
			return idleAgents[i].Band < idleAgents[j].Band
		}
		return idleAgents[i].Name < idleAgents[j].Name
	})

	return &FleetResources{
		SchemaVersion: SchemaVersion,
		GeneratedAt:   at,
		Groups:        resultGroups,
		Totals:        totals,
		MeterPresent:  meterPresent,
		IdleAgents:    idleAgents,
	}, nil
}

func runCobraJSON(args ...string) ([]byte, error) {
	cmd := weave.NewWeaveCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(args)
	if err := cmd.Execute(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// FormatTable renders the text table output for `bashy resources fleet`.
func FormatTable(fr *FleetResources) string {
	var out bytes.Buffer
	w := tabwriter.NewWriter(&out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "PROVIDER\tBAND\tTOTAL\tBUSY\tIDLE\tCOOLING\tUNAVAIL\tUNATTRIBUTED\tSUB\tAPI\tTOKENS\tCOST")

	for _, g := range fr.Groups {
		tokStr, costStr := "N/A", "N/A"
		if g.MeterPresent && g.Tokens != nil && g.CostUSD != nil {
			tokStr = formatTokens(*g.Tokens)
			costStr = fmt.Sprintf("$%.4f", *g.CostUSD)
		}
		fmt.Fprintf(w, "%s\t%s\t%d\t%d\t%d\t%d\t%d\t0\t%d\t%d\t%s\t%s\n",
			g.Provider, g.Band, g.Total, g.Busy, g.Idle, g.Cooling, g.Unavailable,
			g.Subscription, g.APIKey, tokStr, costStr)
	}

	tokTot, costTot := "N/A", "N/A"
	if fr.MeterPresent && fr.Totals.Tokens != nil && fr.Totals.CostUSD != nil {
		tokTot = formatTokens(*fr.Totals.Tokens)
		costTot = fmt.Sprintf("$%.4f", *fr.Totals.CostUSD)
	}
	fmt.Fprintf(w, "Totals\t\t%d\t%d\t%d\t%d\t%d\t%d\t%d\t%d\t%s\t%s\n",
		fr.Totals.Total, fr.Totals.Busy, fr.Totals.Idle, fr.Totals.Cooling, fr.Totals.Unavailable,
		fr.Totals.Unattributed, fr.Totals.Subscription, fr.Totals.APIKey, tokTot, costTot)

	_ = w.Flush()
	return out.String()
}

func formatTokens(n int64) string {
	if n >= 1_000_000 {
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000.0)
	}
	if n >= 1_000 {
		return fmt.Sprintf("%.1fk", float64(n)/1_000.0)
	}
	return strconv.FormatInt(n, 10)
}
