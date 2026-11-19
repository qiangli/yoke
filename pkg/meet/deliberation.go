package meet

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/qiangli/yoke/pkg/chat"
)

// DeliberationPlan selects a small panel explicitly; the room roster is never
// expanded into an all-agent round. Budgets count every attempted invocation,
// including failed responses and facilitator requests. No token/cost estimate
// is presented as an enforced provider limit.
type DeliberationPlan struct {
	Question    string        `json:"question"`
	Proposers   []string      `json:"proposers"`
	Experts     []string      `json:"experts,omitempty"`
	Critic      string        `json:"critic"`
	Facilitator string        `json:"facilitator"`
	MaxTurns    int           `json:"max_turns"`
	Duration    time.Duration `json:"duration"`
	TurnTimeout time.Duration `json:"turn_timeout"`
}

type DeliberationEvidence struct {
	Source string `json:"source"`
	Claim  string `json:"claim"`
}

type DeliberationEvidenceRequest struct {
	Expert   string `json:"expert"`
	Question string `json:"question"`
}

// DeliberationReply is the structured brainstorming response contract. Evidence
// is attributed testimony, not a verification verdict. Dissent is append-only.
type DeliberationReply struct {
	Text     string                       `json:"text"`
	Evidence []DeliberationEvidence       `json:"evidence,omitempty"`
	Dissent  []string                     `json:"dissent,omitempty"`
	Request  *DeliberationEvidenceRequest `json:"request,omitempty"`
}

type DeliberationContribution struct {
	Speaker string `json:"speaker"`
	Role    string `json:"role"`
	DeliberationReply
}

type DeliberationResult struct {
	Status        string                     `json:"status"` // always proposal, never adopted
	Stop          string                     `json:"stop"`
	Turns         int                        `json:"turns"`
	Contributions []DeliberationContribution `json:"contributions"`
	Conclusion    string                     `json:"conclusion,omitempty"`
}

// Deliberate composes Meet's room authority, run lease and transcript with the
// existing chat invocation primitive. Independent replies stay only in memory
// until every proposer has replied: no live stream, turn file, room event or
// peer context exposes a partial proposal. This is workflow isolation, not an
// OS security boundary against a malicious same-host participant.
func Deliberate(ctx context.Context, ref, actor string, p DeliberationPlan, runner chat.Runner) (DeliberationResult, error) {
	return deliberateWith(ctx, ref, actor, p, func(ctx context.Context, opt chat.Options) (chat.Result, error) { return chat.Invoke(ctx, opt, runner) })
}

func deliberateWith(ctx context.Context, ref, actor string, p DeliberationPlan, invoke func(context.Context, chat.Options) (chat.Result, error)) (result DeliberationResult, err error) {
	result.Status = "proposal"
	st, err := roomOf(ref)
	if err != nil {
		return result, err
	}
	lease, err := acquireRunLease(st.ID)
	if err != nil {
		return result, err
	}
	defer lease.Release()
	st, err = loadState(st.ID)
	if err != nil {
		return result, err
	}
	if err = requireOrganizer(st, actor); err != nil {
		return result, err
	}
	if st.board() || st.Status != "open" {
		return result, fmt.Errorf("meet: deliberation requires an open meeting")
	}
	if err = validateDeliberation(st, p); err != nil {
		return result, err
	}
	ctx, cancel := context.WithTimeout(ctx, p.Duration)
	defer cancel()
	record := func(kind, speaker string, v any) error {
		b, e := json.Marshal(v)
		if e != nil {
			return e
		}
		return appendEvent(st.ID, Event{Kind: kind, Speaker: speaker, Text: string(b), TS: time.Now(), Round: st.Round})
	}
	if err = record("deliberation-start", actor, p); err != nil {
		return result, err
	}
	// Every exit after start leaves a durable terminal reason, including partial
	// runs. Sealed proposals are deliberately omitted on pre-reveal failure.
	defer func() {
		if result.Stop == "" {
			result.Stop = "failed"
		}
		if e := record("deliberation-stop", actor, result); e != nil && err == nil {
			err = e
		}
	}()
	call := func(name, role, question string, history []DeliberationContribution) (DeliberationContribution, error) {
		c := DeliberationContribution{Speaker: name, Role: role}
		if ctx.Err() != nil {
			result.Stop = "deadline-or-cancelled"
			return c, ctx.Err()
		}
		if result.Turns >= p.MaxTurns {
			result.Stop = "turn-budget"
			return c, fmt.Errorf("meet: deliberation turn budget exhausted")
		}
		result.Turns++
		payload, _ := json.Marshal(history)
		turnCtx, stop := context.WithTimeout(ctx, p.TurnTimeout)
		defer stop()
		response, e := invoke(turnCtx, chat.Options{Agent: name, Role: role, Task: question,
			Instruction: `Brainstorm a proposal only. Do not adopt or execute actions. Return JSON: {"text":"...","evidence":[{"source":"reference","claim":"..."}],"dissent":["unresolved concern"]}. Only facilitator may additionally return "request":{"expert":"selected expert","question":"missing evidence"}. Critic must identify weaknesses and unresolved dissent. Facilitator may conclude only when missing evidence is addressed; preserve dissent.`,
			Context:     []string{string(payload)}, Files: st.Context, Cwd: st.Cwd, Timeout: p.TurnTimeout, ReadOnly: true, KillOnParentExit: true})
		if turnCtx.Err() != nil {
			result.Stop = "deadline-or-cancelled"
			return c, turnCtx.Err()
		}
		if e != nil {
			return c, e
		}
		if response.ExitCode != 0 {
			return c, fmt.Errorf("meet: %s failed", name)
		}
		if e = json.Unmarshal([]byte(normalizeTurnText(response.Output)), &c.DeliberationReply); e != nil {
			return c, fmt.Errorf("meet: invalid deliberation response from %s: %w", name, e)
		}
		if strings.TrimSpace(c.Text) == "" {
			return c, fmt.Errorf("meet: empty deliberation response")
		}
		for _, ev := range c.Evidence {
			if strings.TrimSpace(ev.Source) == "" || strings.TrimSpace(ev.Claim) == "" {
				return c, fmt.Errorf("meet: evidence requires source and claim")
			}
		}
		if c.Request != nil && role != "facilitator" {
			return c, fmt.Errorf("meet: only facilitator can request evidence")
		}
		return c, nil
	}
	var sealed []DeliberationContribution
	for _, name := range p.Proposers {
		c, e := call(name, "independent-proposer", p.Question, nil)
		if e != nil {
			return result, e
		}
		sealed = append(sealed, c)
	}
	if err = record("deliberation-reveal", actor, sealed); err != nil {
		return result, err
	}
	result.Contributions = sealed
	add := func(c DeliberationContribution) error {
		if e := record("deliberation-contribution", c.Speaker, c); e != nil {
			return e
		}
		result.Contributions = append(result.Contributions, c)
		return nil
	}
	critic, e := call(p.Critic, "critic", p.Question, result.Contributions)
	if e != nil {
		return result, e
	}
	if err = add(critic); err != nil {
		return result, err
	}
	for {
		c, e := call(p.Facilitator, "facilitator", p.Question, result.Contributions)
		if e != nil {
			return result, e
		}
		if err = add(c); err != nil {
			return result, err
		}
		if c.Request == nil {
			result.Conclusion = c.Text
			result.Stop = "concluded-proposal"
			return result, nil
		}
		r := c.Request
		if !deliberationContains(p.Experts, r.Expert) || strings.TrimSpace(r.Question) == "" {
			return result, fmt.Errorf("meet: evidence request must target a selected expert with a question")
		}
		expert, e := call(r.Expert, "expert", r.Question, result.Contributions)
		if e != nil {
			return result, e
		}
		if err = add(expert); err != nil {
			return result, err
		}
	}
}

func deliberationContains(names []string, name string) bool {
	for _, n := range names {
		if n == name {
			return true
		}
	}
	return false
}
func validateDeliberation(st *State, p DeliberationPlan) error {
	if strings.TrimSpace(p.Question) == "" || len(p.Proposers) < 2 || p.MaxTurns < len(p.Proposers)+2 || p.Duration <= 0 || p.TurnTimeout <= 0 || p.TurnTimeout > p.Duration {
		return fmt.Errorf("meet: require question, two proposers, sufficient positive turn/time budgets")
	}
	seen := map[string]bool{}
	names := append(append(append([]string{}, p.Proposers...), p.Experts...), p.Critic, p.Facilitator)
	for _, n := range names {
		key := canonAgent(n)
		if n == "" || seen[key] || !deliberationContains(st.Participants, n) {
			return fmt.Errorf("meet: distinct explicitly selected room participants required: %q", n)
		}
		seen[key] = true
	}
	return nil
}
