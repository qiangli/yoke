package chat

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/qiangli/yoke/pkg/llmbudget"
)

// A harness turn is opaque: stdout and rune counts do not reveal all model calls,
// context or tool-loop output. These claims never assert bounded token usage or
// turn average catalog pricing into a hard spend ceiling.
type budgetWork struct {
	gate        *llmbudget.Gate
	owner       *llmbudget.OwnerLease
	reservation *llmbudget.Reservation
	launch      Launch
	prompt      string
	stop        chan struct{}
	once        sync.Once
}

func opaqueBudgetRequest(l Launch, prompt string, launch bool) llmbudget.Request {
	host, _ := os.Hostname()
	r := llmbudget.Request{Model: l.ModelName, Agent: l.Nick, Host: host, Tokens: estimateTokens(prompt), UnknownTokens: true, TTL: 2 * time.Minute}
	if launch {
		r.Concurrency = 1
		r.HostSlots = 1
		r.UnknownMemory = true
	}
	return r
}
func reserveBudgetWork(ctx context.Context, l Launch, prompt, run string, launch bool, allowPremium bool) (*budgetWork, error) {
	gate := llmbudget.DefaultGate()
	owner, e := gate.NewOwner(ctx, "chat "+l.Binding())
	if e != nil {
		return nil, e
	}
	r := opaqueBudgetRequest(l, prompt, launch)
	r.ID = "chat-" + owner.ID()
	r.Owner = owner.ID()
	r.Run = run
	r.AllowPremium = allowPremium
	a, e := gate.Reserve(ctx, r)
	if e != nil || a.Decision.Action != llmbudget.Allow || a.Reservation == nil {
		owner.Close()
		if e != nil {
			return nil, e
		}
		return nil, fmt.Errorf("chat: budget %s: %s", a.Decision.Action, a.Decision.Reason)
	}
	b := &budgetWork{gate: gate, owner: owner, reservation: a.Reservation, launch: l, prompt: prompt, stop: make(chan struct{})}
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-b.stop:
				return
			case <-ticker.C:
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				_ = gate.Renew(ctx, r.ID, r.Owner, 2*time.Minute)
				cancel()
			}
		}
	}()
	return b, nil
}

// finish releases capacity only on a confirmed ordinary completion. Errors and
// cancellation can leave external work alive; their claims remain available to
// the lifecycle authority's verified ReconcileTerminated path.
func (b *budgetWork) finish(output string, runErr error) error {
	if b == nil {
		return nil
	}
	var result error
	b.once.Do(func() {
		close(b.stop)
		defer b.owner.Close()
		if runErr != nil {
			return
		}
		input, out := estimateTokens(b.prompt), estimateTokens(output)
		actual := llmbudget.Actual{InputTokens: input, OutputTokens: out, TokensEstimated: true, Source: "chat-text-estimate", ObservedAt: time.Now().UTC()}
		if cost, known := llmbudget.EstimatedCostUSD(b.launch.ModelName, input+out); known && cost >= 0 && cost < float64(math.MaxInt64)/1e6 {
			micro := int64(math.Ceil(cost * 1e6))
			actual.SpendMicroUSD = &micro
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		result = b.gate.Settle(ctx, b.reservation.ID, b.owner.ID(), actual)
	})
	return result
}
func (b *budgetWork) abort() error {
	if b == nil {
		return nil
	}
	var result error
	b.once.Do(func() {
		close(b.stop)
		defer b.owner.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		result = b.gate.Release(ctx, b.reservation.ID, b.owner.ID())
	})
	return result
}
func (s *Session) reserveTurnBudget(text string) error {
	s.budgetMu.Lock()
	defer s.budgetMu.Unlock()
	if s.budgetClosed {
		return errors.New("chat: session budget lifetime ended")
	}
	b, e := reserveBudgetWork(context.Background(), s.launch, text, s.Agent, false, s.allowPremium)
	if e != nil {
		return e
	}
	s.budgetWorks = append(s.budgetWorks, b)
	return nil
}
func (s *Session) finishBudgetWorks(output string, runErr error) error {
	s.budgetMu.Lock()
	defer s.budgetMu.Unlock()
	if s.budgetClosed {
		return nil
	}
	s.budgetClosed = true
	var result error
	for i, b := range s.budgetWorks {
		reply := ""
		if i == 0 {
			reply = output
		}
		if e := b.finish(reply, runErr); e != nil {
			result = errors.Join(result, e)
		}
	}
	return result
}

// Concrete subprocess runners populate this proof. Injected Runner implementations
// retain their existing synchronous contract: nil Run error means bounded work ended.
type budgetProcessProof struct{ Observed, Started, Terminated bool }
type budgetProcessKey struct{}

func observeBudgetProcess(ctx context.Context, cmd *exec.Cmd) {
	if p, ok := ctx.Value(budgetProcessKey{}).(*budgetProcessProof); ok {
		p.Observed = true
		p.Started = cmd != nil && cmd.Process != nil
		p.Terminated = budgetOwnedGroupGone(cmd)
	}
}
func budgetCompletionError(cmd *exec.Cmd) error {
	if !budgetOwnedGroupGone(cmd) {
		return errors.New("inherited child lifetime unverified; reservation retained")
	}
	return nil
}
