package chat

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/qiangli/yoke/pkg/capability"
	"github.com/qiangli/yoke/pkg/fleet"
)

// P2b — the band-graduated agent coach.
//
// When the LLM-free reflex trips repeatedly and its generic "you are looping,
// stop" steer does not break the loop, escalate to an AGENT one band above the
// coachee for a content-full steer. The delegation chain IS the coaching chain:
// L4 steward -> L3 conductor -> L2 coder -> L1 tester. The agent-coach is always
// >= 1 band up, because to coach you must both RECOGNIZE the subordinate is stuck
// AND KNOW the fix — more capability than doing the task. Escalation is the
// EXPENSIVE layer: it spends an inference call, so it fires only after the cheap
// reflex has demonstrably failed (EscalateAfter generic steers), and only once.

// maxBand is the top of the capability ladder (frontier).
const maxBand = 4

// EscalationRequest is what the reflex hands an agent-coach when the generic
// steer has not worked.
type EscalationRequest struct {
	Coachee string   // the looping agent's binding/nick, e.g. codex-gpt-5.5
	Recent  []string // its recent significant output lines, for context
	Trip    int      // which intervention triggered the escalation
}

// EscalateFunc produces a content-full steer for a stuck agent and names the
// coach that produced it. ok=false means "no escalation available" — the caller
// falls back to the generic steer.
type EscalateFunc func(ctx context.Context, req EscalationRequest) (steer, coach string, ok bool)

// BandGraduatedEscalator is the default agent-coach: pick the minimum-sufficient
// OPERABLE agent one band above the coachee and ask it, in one sentence, what the
// stuck agent should do differently.
func BandGraduatedEscalator(ctx context.Context, req EscalationRequest) (string, string, bool) {
	cat := newCatalog()
	target := coacheeBand(cat, req.Coachee) + 1
	if target < 2 {
		target = 2 // a coach is judgment work; never agent-coach at L1
	}
	coach := pickCoachAgent(cat, target, req.Coachee)
	if coach == "" {
		return "", "", false // no operable agent above the coachee — reflex only
	}
	// Self-bound: the coach must not run longer than the loop it is diagnosing.
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	res, err := Invoke(ctx, Options{
		Agent:       coach,
		Instruction: escalationPrompt(req),
		Timeout:     3 * time.Minute,
	}, execRunner{})
	if err != nil {
		return "", "", false
	}
	steer := collapseSteer(res.Output)
	if steer == "" {
		return "", "", false
	}
	return steer, coach, true
}

// LadderEscalator escalates through an EXPLICIT ladder of agents (e.g. an L3
// then an L4, possibly cross-provider) instead of auto-picking one band up. Each
// escalation trip advances one rung: the base gets stuck → cheap reflex steer →
// still stuck → the first ladder agent diagnoses and steers → still stuck → the
// next. This is the cascade agent's engine: a cheap base served up to a target
// band by calling in progressively senior help only when it has demonstrably
// failed to progress. Returns ok=false once the ladder is exhausted (fall back
// to the reflex).
func LadderEscalator(ladder []string) EscalateFunc {
	var (
		mu   sync.Mutex
		rung int
	)
	return func(ctx context.Context, req EscalationRequest) (string, string, bool) {
		mu.Lock()
		if rung >= len(ladder) {
			mu.Unlock()
			return "", "", false
		}
		coach := ladder[rung]
		rung++
		mu.Unlock()
		if strings.TrimSpace(coach) == "" {
			return "", "", false
		}
		// Self-bound: a coach must not run longer than the loop it is diagnosing.
		ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
		defer cancel()
		res, err := Invoke(ctx, Options{
			Agent:       coach,
			Instruction: escalationPrompt(req),
			Timeout:     3 * time.Minute,
		}, execRunner{})
		if err != nil {
			return "", "", false
		}
		steer := collapseSteer(res.Output)
		if steer == "" {
			return "", "", false
		}
		return steer, coach, true
	}
}

// cascadeLadder returns the base agent and escalation ladder for a cascade agent
// by name, or ok=false if the name is not a cascade agent. The coach launches
// the base and escalates through the ladder.
func cascadeLadder(name string) (base string, ladder []string, ok bool) {
	cat := newCatalog()
	agents, _ := cat.Agents()
	for i := range agents {
		if agents[i].Name == name && agents[i].IsCascade() {
			return agents[i].Base, agents[i].Escalation, true
		}
	}
	return "", nil, false
}

// coacheeBand resolves the coachee binding to its model's band (0 if unknown).
func coacheeBand(cat *fleet.Catalog, coachee string) int {
	if strings.TrimSpace(coachee) == "" {
		return 0
	}
	if _, _, m, err := cat.Binding(coachee); err == nil {
		return m.Band
	}
	return 0
}

// pickCoachAgent returns the MINIMUM-SUFFICIENT operable agent at >= target band
// (the cheapest that clears the bar — do not send an L4 when an L3 suffices),
// excluding the coachee itself. "" when none is available.
func pickCoachAgent(cat *fleet.Catalog, target int, coachee string) string {
	agents, _ := cat.Agents()
	type cand struct {
		name string
		band int
	}
	var cands []cand
	for _, a := range agents {
		if a.Name == coachee {
			continue // never coach yourself
		}
		_, _, m, err := cat.Binding(a.Name)
		if err != nil || m.Band < target {
			continue
		}
		if ok, _ := capability.Operable(capability.ResolveTool(a.Tool)); !ok {
			continue // an uninstalled tool can't coach anyone
		}
		cands = append(cands, cand{name: a.Name, band: m.Band})
	}
	if len(cands) == 0 {
		return ""
	}
	sort.Slice(cands, func(i, j int) bool {
		if cands[i].band != cands[j].band {
			return cands[i].band < cands[j].band // lowest sufficient band first
		}
		return cands[i].name < cands[j].name // deterministic
	})
	return cands[0].name
}

func escalationPrompt(req EscalationRequest) string {
	var b strings.Builder
	b.WriteString("An agent you are supervising is STUCK IN A LOOP — it keeps repeating work without making progress. ")
	b.WriteString("Here is its recent output:\n\n")
	for _, ln := range req.Recent {
		b.WriteString("  " + ln + "\n")
	}
	b.WriteString("\nIn ONE sentence, tell it specifically what to do differently to break the loop ")
	b.WriteString("(a wrong assumption, an unsatisfiable requirement, a step it is skipping). ")
	b.WriteString("Output only the one-sentence instruction — no preamble, no reasoning.")
	return b.String()
}

// collapseSteer flattens an agent's reply to a single steer line.
func collapseSteer(s string) string {
	s = strings.Join(strings.Fields(SanitizeTurn(s)), " ")
	if len(s) > 400 {
		s = s[:400]
	}
	return strings.TrimSpace(s)
}
