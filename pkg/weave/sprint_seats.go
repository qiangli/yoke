package weave

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/qiangli/yoke/pkg/capability"
	"github.com/qiangli/yoke/pkg/ladder"
	"github.com/qiangli/yoke/pkg/ladder/blame"
)

type seatFallbackEvent struct {
	Seat        string `json:"seat"`
	Story       string `json:"story"`
	WantedBand  int    `json:"wanted_band"`
	ChosenAgent string `json:"chosen_agent"`
	Band        int    `json:"band"`
	WantedSize  int    `json:"wanted_size,omitempty"`
	Size        int    `json:"size,omitempty"`
}

func seatRecordFallback(s *weaveStory, event seatFallbackEvent) {
	raw, _ := json.Marshal(event)
	weaveStoryAppend(s, weaveConductorName(""), "fallback", string(raw))
}

func seatPool(root string, events []ladder.Event, now time.Time, exclusions ...sprintAssignExclusions) ([]ladder.Entrant, ladder.Lines, error) {
	cat := fleetCatalog()
	agents, errs := cat.Agents()
	if len(errs) > 0 {
		return nil, ladder.Lines{}, fmt.Errorf("fleet catalog: %v", errs)
	}
	sort.Slice(agents, func(i, j int) bool { return agents[i].Name < agents[j].Name })
	season := ladder.SeasonOf(now)
	rep := ladder.Replay(events, season)
	lines := capability.LadderLines(rep, season)
	dirs := weaveAllQueueDirs()
	if root != "" {
		dir, err := weaveQueueDir(root)
		if err != nil {
			return nil, lines, err
		}
		dirs = append(dirs, dir)
	}
	var queues []*weaveQueue
	unusable := map[string]bool{}
	seenDirs := map[string]bool{}
	for _, dir := range dirs {
		if seenDirs[dir] {
			continue
		}
		seenDirs[dir] = true
		q, err := loadWeaveQueue(dir)
		if err != nil {
			return nil, lines, err
		}
		queues = append(queues, q)
		for tool, p := range loadFleetProbeCache(dir) {
			if !p.Capable {
				unusable[tool] = true
			}
		}
	}
	toolReasons, bindingReasons := map[string]string{}, map[string]string{}
	for _, x := range exclusions {
		for _, name := range x.tools {
			key := strings.TrimSpace(name)
			if tool, ok := cat.Tool(key); ok {
				key = tool.Name
			}
			toolReasons[key] = "excluded tool:" + name
		}
		for _, name := range x.agents {
			key := strings.TrimSpace(name)
			if binding, _, _, err := cat.Binding(key); err == nil {
				key = binding.MatrixKey()
			}
			bindingReasons[key] = "excluded agent:" + name
		}
	}
	// Role.Scope is the fleet responsibility boundary. A shadow binding must
	// never affect real picks, even through another name for the same binding.
	for _, a := range agents {
		if a.Role != nil && strings.EqualFold(strings.TrimSpace(a.Role.Scope), "shadow") {
			if binding, _, _, err := cat.Binding(a.Name); err == nil && bindingReasons[binding.MatrixKey()] == "" {
				bindingReasons[binding.MatrixKey()] = "shadow role scope"
			}
		}
	}
	aliases := map[string][]string{}
	for _, a := range agents {
		binding, _, _, err := cat.Binding(a.Name)
		if err == nil {
			aliases[binding.MatrixKey()] = append(aliases[binding.MatrixKey()], a.Name)
		}
	}
	// Plan tier ranks the seat each model bills through; an unrecorded or
	// dangling plan is rank 0 (unknown), which only ever loses a heavy-work tie.
	planRank := map[string]int{}
	plans, _ := cat.Plans()
	for _, p := range plans {
		planRank[p.Name] = p.Rank()
	}
	var pool []ladder.Entrant
	for _, a := range agents {
		if a.Ephemeral || a.ClonedFrom != "" {
			continue
		}
		binding, tool, model, err := cat.Binding(a.Name)
		if err != nil {
			continue
		}
		key := binding.MatrixKey()
		reason := toolReasons[tool.Name]
		if reason == "" {
			reason = bindingReasons[key]
		}
		if reason == "" && unusable[tool.Name] {
			reason = "cached probe: unusable tool:" + tool.Name
		}
		if reason != "" {
			for _, x := range exclusions {
				if x.report != nil {
					x.report(a.Name, reason)
				}
			}
			continue
		}
		seed := model.Band
		if a.IsCascade() && a.Band > 0 {
			seed = a.Band
		}
		state := seatBand(seed, events, key)
		profile := ladder.Profile{}
		if rec := rep.Agents[key]; rec != nil {
			profile.Standings = rec.Standings
		}
		standings := map[ladder.Duty]ladder.DutyStanding{}
		for duty, s := range profile.Standings {
			standings[duty] = s
		}
		if _, ok := standings[ladder.DutyCode]; !ok {
			r := ladder.NewRating()
			standings[ladder.DutyCode] = ladder.DutyStanding{R: r.R, RD: r.RD}
		}
		count := 0
		for _, ev := range events {
			if ev.Agent == key && ev.Season == season && ev.Kind == ladder.EventKindDelivery && ev.Duty == ladder.DutyCode {
				count++
			}
		}
		names := append(aliases[key], key)
		// Fleet exposes a billing-adjusted relative cost, not a measured
		// dollars-per-point rate; use it only as the scheduler tie-breaker.
		successes, failures := seatRecord(events, key)
		pool = append(pool, ladder.Entrant{Agent: a.Name, Vendor: tool.Name, Band: state.Band, Standings: standings, Free: !sprintAssignBusy(queues, names), CostPerPoint: float64(model.MarginalCostMicro()), PlanRank: planRank[model.Plan], CodingStoriesThisSeason: count, Successes: successes, AgentFailures: failures})
	}
	return pool, lines, nil
}

// seatBand treats an unseeded catalog model as L1 while retaining that fact in Seed.
func seatBand(seed int, events []ladder.Event, agent string) ladder.BandState {
	if seed <= 0 {
		seed = 1
	}
	return ladder.CurrentBand(seed, events, agent)
}

func seatRecord(events []ladder.Event, agent string) (successes, failures int) {
	dropped := make(map[string]bool)
	for _, e := range events {
		if e.Kind == ladder.EventKindCorrection && e.Supersedes != "" {
			dropped[e.Supersedes] = true
		}
	}
	for _, e := range events {
		if e.Kind != ladder.EventKindDelivery || e.Agent != agent || dropped[e.ID] || strings.Contains(strings.ToLower(e.Note), "shadow") {
			continue
		}
		switch {
		case e.Outcome == 1 || e.Outcome == 0.5:
			successes++
		case e.Outcome == 0 && blame.Consequence(e.Blame) == blame.ActionRate:
			failures++
		}
	}
	return
}

// seatPanel applies the band cascade and keeps an odd panel. Vendor diversity
// is a preference among equally ranked candidates.
func seatPanel(pool []ladder.Entrant, wanted int, authorVendor string) []ladder.Entrant {
	eligible := make([]ladder.Entrant, 0, len(pool))
	for _, e := range pool {
		if e.Free && e.Band >= 1 {
			eligible = append(eligible, e)
		}
	}
	sort.SliceStable(eligible, func(i, j int) bool {
		a, b := eligible[i], eligible[j]
		if a.Band != b.Band {
			return a.Band > b.Band
		}
		aRecord, bRecord := a.Successes+a.AgentFailures, b.Successes+b.AgentFailures
		if aRecord > 0 || bRecord > 0 {
			aScore := a.SeedScore
			bScore := b.SeedScore
			if aRecord > 0 {
				aScore = float64(a.Successes+1) / float64(aRecord+2)
			}
			if bRecord > 0 {
				bScore = float64(b.Successes+1) / float64(bRecord+2)
			}
			if aScore != bScore {
				return aScore > bScore
			}
		}
		if a.SeedScore != b.SeedScore {
			return a.SeedScore > b.SeedScore
		}
		return a.Agent < b.Agent
	})
	if wanted > 1 && authorVendor != "" {
		without := make([]ladder.Entrant, 0, len(eligible))
		for _, e := range eligible {
			if e.Vendor != authorVendor {
				without = append(without, e)
			}
		}
		if len(without) >= wanted {
			eligible = without
		}
	}
	if wanted > len(eligible) {
		wanted = len(eligible)
	}
	if wanted%2 == 0 {
		wanted--
	}
	if wanted <= 0 {
		return nil
	}
	picked := append([]ladder.Entrant(nil), eligible[:wanted]...)
	if wanted > 1 {
		seen := map[string]bool{}
		for _, e := range picked {
			seen[e.Vendor] = true
		}
		if len(seen) == 1 {
			for _, e := range eligible[wanted:] {
				if e.Band == picked[wanted-1].Band && !seen[e.Vendor] {
					picked[wanted-1] = e
					break
				}
			}
		}
	} else if authorVendor != "" && picked[0].Vendor == authorVendor {
		for _, e := range eligible[1:] {
			if e.Band == picked[0].Band && e.Vendor != authorVendor {
				picked[0] = e
				break
			}
		}
	}
	return picked
}
