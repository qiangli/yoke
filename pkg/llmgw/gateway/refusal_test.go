package gateway

import (
	"context"
	"net/http"
	"sync"
	"testing"

	"github.com/qiangli/yoke/pkg/llmgw/resolve"
	"github.com/qiangli/yoke/pkg/llmgw/sched"
)

// principalEnv wires a gateway whose principal comes from the
// X-Test-Principal header (default alice) and whose per-principal cap
// comes from limits (default DefaultMaxInFlight). Refusals are collected
// for assertion.
type refusalSink struct {
	mu  sync.Mutex
	got []Refusal
}

func (s *refusalSink) record(r Refusal) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.got = append(s.got, r)
}

func (s *refusalSink) all() []Refusal {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Refusal(nil), s.got...)
}

func newPrincipalEnv(t *testing.T, rows []resolve.ModelRow, ups []*upstream, limits map[string]int, sink *refusalSink) *testEnv {
	t.Helper()
	return newEnv(t, rows, ups, func(c *Config) {
		c.Authorize = func(r *http.Request) (string, int, error) {
			if p := r.Header.Get("X-Test-Principal"); p != "" {
				return p, 100, nil
			}
			return "alice", 100, nil
		}
		c.AdmissionLimit = func(principal string) int {
			if n, ok := limits[principal]; ok {
				return n
			}
			return DefaultMaxInFlight
		}
		if sink != nil {
			c.OnRefusal = sink.record
		}
	})
}

func principalHdr(principal, job string) map[string]string {
	return map[string]string{"X-Test-Principal": principal, sched.JobIDHeader: job}
}

// TestAdmissionLimit_PerPrincipalHonoured pins the story's first half: the
// cap is per principal, so a saturated alice 429s while bob still serves.
func TestAdmissionLimit_PerPrincipalHonoured(t *testing.T) {
	release := make(chan struct{})
	blocked := newUpstream(t, "alpha", func(w http.ResponseWriter, _ *http.Request) {
		<-release
		echoJSON(w, nil)
	})
	open := newUpstream(t, "beta", echoJSON)
	env := newPrincipalEnv(t,
		[]resolve.ModelRow{row("model-a", "alpha"), row("model-b", "beta")},
		[]*upstream{blocked, open},
		map[string]int{"alice": 1, "bob": 8}, nil)
	defer close(release)

	go func() {
		env.do(t, http.MethodPost, ChatCompletionsPath, chatPayload("model-a"), principalHdr("alice", "job-a"))
	}()
	waitFor(t, func() bool { return env.cfg.Admitter.InFlight("alice") == 1 })

	// Bob has his own cap and his own backend: unaffected by alice.
	if w := env.do(t, http.MethodPost, ChatCompletionsPath, chatPayload("model-b"), principalHdr("bob", "job-b")); w.Code != http.StatusOK {
		t.Fatalf("bob status=%d body=%s, want 200", w.Code, w.Body.String())
	}

	// Alice's second concurrent request is past her cap of 1.
	w := env.do(t, http.MethodPost, ChatCompletionsPath, chatPayload("model-a"), principalHdr("alice", "job-c"))
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("alice status=%d body=%s, want 429", w.Code, w.Body.String())
	}
	// The first request is released by the deferred close; it must not
	// be awaited here or the test deadlocks against that close.
}

// TestAdmissionRefusal_ReportedToHook pins the story's second half for the
// in-flight cap: the 429 carries the principal, model, limit and live
// in-flight count to the host hook.
func TestAdmissionRefusal_ReportedToHook(t *testing.T) {
	release := make(chan struct{})
	up := newUpstream(t, "alpha", func(w http.ResponseWriter, _ *http.Request) {
		<-release
		echoJSON(w, nil)
	})
	sink := &refusalSink{}
	env := newPrincipalEnv(t,
		[]resolve.ModelRow{row("llama3.2:1b", "alpha")},
		[]*upstream{up},
		map[string]int{"alice": 1}, sink)
	defer close(release)

	go func() {
		env.do(t, http.MethodPost, ChatCompletionsPath, chatPayload("llama3.2:1b"), principalHdr("alice", "job-a"))
	}()
	waitFor(t, func() bool { return env.cfg.Admitter.InFlight("alice") == 1 })

	w := env.do(t, http.MethodPost, ChatCompletionsPath, chatPayload("llama3.2:1b"), principalHdr("alice", "job-b"))
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("status=%d body=%s, want 429", w.Code, w.Body.String())
	}

	got := sink.all()
	if len(got) != 1 {
		t.Fatalf("refusals=%+v, want exactly one", got)
	}
	r := got[0]
	if r.Principal != "alice" || r.Model != "llama3.2:1b" || r.Status != http.StatusTooManyRequests {
		t.Errorf("refusal=%+v, want alice/llama3.2:1b/429", r)
	}
	if r.Limit != 1 || r.InFlight != 1 {
		t.Errorf("refusal limit=%d in_flight=%d, want 1/1", r.Limit, r.InFlight)
	}
	if r.Reason == "" {
		t.Error("refusal carries no reason")
	}
}

// TestQuotaRefusal_ReportedToHook: a host-policy wipe (every backend
// excluded) is a quota 429 and reaches the same hook the admission cap
// uses, so usage.jsonl sees throttling either way.
func TestQuotaRefusal_ReportedToHook(t *testing.T) {
	up := newUpstream(t, "alpha", echoJSON)
	sink := &refusalSink{}
	env := newEnv(t,
		[]resolve.ModelRow{row("llama3.2:1b", "alpha")},
		[]*upstream{up}, func(c *Config) {
			c.AdmissionLimit = func(string) int { return 4 }
			c.OnRefusal = sink.record
			c.FilterCandidates = func(context.Context, string, string, []string) []string {
				return nil
			}
		})

	w := env.do(t, http.MethodPost, ChatCompletionsPath, chatPayload("llama3.2:1b"), principalHdr("alice", "job-q"))
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("status=%d body=%s, want 429", w.Code, w.Body.String())
	}

	got := sink.all()
	if len(got) != 1 {
		t.Fatalf("refusals=%+v, want exactly one", got)
	}
	r := got[0]
	if r.Principal != "alice" || r.Model != "llama3.2:1b" || r.Status != http.StatusTooManyRequests {
		t.Errorf("refusal=%+v, want alice/llama3.2:1b/429", r)
	}
	if r.Limit != 4 {
		t.Errorf("refusal limit=%d, want the configured cap 4", r.Limit)
	}
	if r.Reason == "" {
		t.Error("refusal carries no reason")
	}
}
