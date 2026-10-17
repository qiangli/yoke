package fleet

import (
	"context"
	"testing"
)

// Sprint 455 P7 (Story #2281, Story-ID 7ad909c17d91): the llm unit is a
// managed service bashy supervisord can autostart and restart, not just
// something `bashy llm up` starts ad hoc.

// Acceptance: the seeded llm unit parses and validates, and its restart
// policy is bounded (a backoff ceiling and a start-limit burst), so a
// broken config does not spin forever once bashy supervisord owns it.
func TestLLMServiceUnitHealthAndRestartPolicyAreBounded(t *testing.T) {
	t.Setenv(SeedsEnv, "off")
	s, ok := New(serviceOpts(t.TempDir())...).Service("llm")
	if !ok {
		t.Fatal("no llm service unit")
	}
	if err := s.Validate(); err != nil {
		t.Fatalf("llm unit: %v", err)
	}
	if s.ProviderName() != ServiceProviderProcess {
		t.Fatalf("llm provider = %q, want process", s.ProviderName())
	}
	if !s.Health.TCP || s.Health.Failures <= 0 {
		t.Fatalf("llm health = %+v, want a bounded tcp probe", s.Health)
	}
	r := s.Restart
	if r.Policy != RestartOnFailure {
		t.Fatalf("llm restart.policy = %q, want %q", r.Policy, RestartOnFailure)
	}
	if r.Backoff.Min == "" || r.Backoff.Max == "" {
		t.Fatalf("llm restart.backoff = %+v, want a bounded min/max", r.Backoff)
	}
	if r.StartLimit.Burst <= 0 || r.StartLimit.Interval == "" {
		t.Fatalf("llm restart.start_limit = %+v, want a positive burst within an interval", r.StartLimit)
	}
}

// Acceptance: SetServiceEnabled is the one write `bashy llm up/down
// --service` composes with its own immediate start/stop — same effect as
// `bashy service enable/disable llm`, callable without going through the
// service command tree.
func TestCatalogSetServiceEnabledPersistsDesiredState(t *testing.T) {
	root := t.TempDir()
	opts := serviceOpts(root)
	ctx := context.Background()

	cat := New(opts...)
	if s, _ := cat.Service("llm"); s.Enabled {
		t.Fatal("llm seed ships enabled; expected false")
	}

	if err := cat.SetServiceEnabled(ctx, "llm", true); err != nil {
		t.Fatalf("enable: %v", err)
	}
	if s, _ := New(opts...).Service("llm"); !s.Enabled {
		t.Fatal("llm not enabled after SetServiceEnabled(true)")
	}

	// Idempotent: enabling an already-enabled unit is a no-op, not an error.
	if err := cat.SetServiceEnabled(ctx, "llm", true); err != nil {
		t.Fatalf("re-enable: %v", err)
	}

	if err := cat.SetServiceEnabled(ctx, "llm", false); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if s, _ := New(opts...).Service("llm"); s.Enabled {
		t.Fatal("llm still enabled after SetServiceEnabled(false)")
	}

	if err := cat.SetServiceEnabled(ctx, "no-such-unit", true); err == nil {
		t.Fatal("enabling an unknown unit: want an error")
	}
}
