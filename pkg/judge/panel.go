// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package judge

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/qiangli/yoke/pkg/chat"
	"github.com/qiangli/yoke/pkg/fleet"
)

// Panel runs N reviewers INDEPENDENTLY and in parallel, then combines their verdicts.
//
// In parallel because they never see each other — the whole design (see the package
// doc) is that no reviewer is anchored by another's opinion, so there is nothing to
// serialize. Reviewing is the one place where a slow sequential "discussion" buys
// nothing and costs the wall-clock of the whole fleet.
func Panel(ctx context.Context, agents []string, stage, subject, content string, timeout time.Duration) Report {
	var (
		mu  sync.Mutex
		ops = make([]Opinion, len(agents))
		wg  sync.WaitGroup
	)
	instruction := Rubric(stage, subject, content)

	for i, agent := range agents {
		wg.Add(1)
		go func(i int, agent string) {
			defer wg.Done()
			started := time.Now()
			op := review(ctx, agent, instruction, timeout)
			op.Took = time.Since(started).Round(time.Millisecond).String()
			mu.Lock()
			ops[i] = op
			mu.Unlock()
		}(i, agent)
	}
	wg.Wait()

	r := Combine(ops)
	r.Stage = stage
	r.Subject = subject
	return r
}

func review(ctx context.Context, agent, instruction string, timeout time.Duration) Opinion {
	res, err := chat.Invoke(ctx, chat.Options{
		Agent:       agent,
		Role:        "reviewer",
		Instruction: instruction,
		Timeout:     timeout,
		// A REVIEWER MUST NOT BE ABLE TO MODIFY WHAT IT REVIEWS. An agent with write
		// authority could "fix" the code and then approve its own fix, and the verdict
		// would be worth nothing. It also does not need the access: the diff is passed
		// INLINE in the instruction, so the reviewer touches no files to do its job.
		ReadOnly: true,
	}, nil)
	if err != nil {
		// A reviewer that could not run has no opinion. It is NOT an approval.
		return Opinion{Agent: agent, Verdict: Errored, Error: err.Error()}
	}
	if res.ExitCode != 0 && strings.TrimSpace(res.Output) == "" {
		return Opinion{Agent: agent, Verdict: Errored,
			Error: fmt.Sprintf("reviewer exited %d with no output", res.ExitCode)}
	}
	return ParseOpinion(agent, res.Output)
}

// SelectPanel picks n DISTINCT agents to review with.
//
// Distinct is the point. A "panel" of three identical models is one opinion billed three
// times: they share the same blind spots, so their agreement carries no information. If
// the host cannot field n distinct agents, the panel is smaller and SAYS so — an honest
// panel of one beats a fake panel of three.
func SelectPanel(n int, pinned []string) ([]string, string, error) {
	if len(pinned) > 0 {
		return pinned, "", nil
	}
	installed := availableAgents()
	if len(installed) == 0 {
		return nil, "", fmt.Errorf("no agentic CLI is installed and signed in — `bashy tool` to see what this host can drive")
	}
	if n > len(installed) {
		note := fmt.Sprintf("panel of %d, not %d: this host can field only %d distinct agent(s) — a panel of clones is one opinion billed %d times",
			len(installed), n, len(installed), n)
		return installed, note, nil
	}
	return installed[:n], "", nil
}

// availableAgents lists the agents this host can actually drive.
//
// ONE agent per TOOL. Two models on the same harness are not two reviewers: they share
// the harness's blind spots (how it reads a repo, what it truncates, what it never
// sees), so their agreement is not evidence. Diversity of HARNESS is what makes a
// second opinion a second opinion.
func availableAgents() []string {
	list, err := fleet.New().Agents()
	if err != nil {
		return nil
	}
	var out []string
	seenTool := map[string]bool{}
	for _, a := range list {
		if a.Tool == "" || seenTool[a.Tool] {
			continue
		}
		if _, err := exec.LookPath(a.Tool); err != nil {
			continue // not installed on this host
		}
		seenTool[a.Tool] = true
		out = append(out, a.Name)
	}
	return out
}
