package aws_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/provider"
	"github.com/qiangli/yoke/pkg/provider/aws"
	"github.com/qiangli/yoke/pkg/provider/aws/fakelightsail"
	"github.com/qiangli/yoke/pkg/provider/conformance"
)

// The reusable conformance suite (pkg/provider/conformance) against the
// AWS adapter over fakelightsail: states and transitions, idempotency and
// reconciliation, normalized errors, unsupported operations,
// stop-versus-destroy billing and data semantics, and the A6 destroy
// authorization. Fixture-only first (it must allocate nothing), then with
// the allocating lifecycle enabled.
const confOwner = "conformance"

type seeds struct{ running, stopped, temp string }

// setup opens the adapter through the dispatcher the way a host does, and
// seeds the fixtures directly in the fake (no provider call, so the
// fixture-only create budget stays at zero).
func setup(t *testing.T, fake *fakelightsail.Server, s seeds) conformance.Setup {
	t.Helper()
	d, err := provider.Open(context.Background(), aws.Manifest(), provider.Options{
		Owner:       confOwner,
		Exec:        provider.ExecDisabled,
		Journal:     provider.NewMemoryJournal(),
		Credentials: provider.StaticCredential(fakelightsail.TestSecretAccessKey),
		Builtins: map[string]provider.BuiltinFactory{
			aws.Name: func(_ context.Context, _ provider.Manifest, credential string) (provider.Adapter, error) {
				return aws.New(aws.Config{
					Credentials: aws.Credentials{
						AccessKeyID:     fakelightsail.TestAccessKeyID,
						SecretAccessKey: credential,
					},
					Region:          fakelightsail.TestRegion,
					Owner:           confOwner,
					BaseURL:         fake.URL,
					CostExplorerURL: fake.URL,
					PollInterval:    time.Millisecond,
					WaitTimeout:     5 * time.Second,
					Sprint:          "522",
					Story:           "2816",
				})
			},
		},
		Timeout: 10 * time.Second,
		Retries: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Park the stopped fixture off through the adapter; a stop is not an
	// allocation, so the fixture-only budget is unaffected.
	if _, err := d.Do(context.Background(), provider.Request{
		Op: provider.OpStop, IdempotencyKey: "seed-park-stopped", ResourceID: s.stopped,
	}); err != nil {
		t.Fatal(err)
	}
	return conformance.Setup{
		Dispatcher: d,
		Counter:    counter{fake},
		RunningID:  s.running,
		StoppedID:  s.stopped,
		TempID:     s.temp,
		InjectCreateLoss: func() {
			fake.Inject("CreateInstances", fakelightsail.FaultLoseResponse, 1)
		},
	}
}

// seedFake provisions deterministic fixtures: a running instance, one to
// park stopped, and a temporary one carrying TTL tags. Each carries what a
// real create writes — the lease tags AND dhnt:managed=true.
func seedFake(fake *fakelightsail.Server) seeds {
	mk := func(name, key string, auth *provider.Authorization) string {
		tags := append(provider.LeaseTags(confOwner, key, auth), aws.ManagedTag, "dhnt:sprint=522")
		fake.AddInstance(name, "running", tags...)
		return name
	}
	return seeds{
		running: mk("fx-running", "seed-running", nil),
		stopped: mk("fx-stopped", "seed-stopped", nil),
		temp: mk("fx-temp", "seed-temp", &provider.Authorization{
			Kind: provider.AuthTemporary, ExpiresAt: time.Now().Add(time.Hour),
		}),
	}
}

type counter struct{ s *fakelightsail.Server }

func (c counter) CreateCount() int      { return c.s.CreateCount() }
func (c counter) DeleteCount() int      { return c.s.DeleteCount() }
func (c counter) RequestCount() int     { return c.s.RequestCount() }
func (c counter) Exists(id string) bool { return c.s.Exists(id) }

func TestConformanceSuite(t *testing.T) {
	fake := fakelightsail.New(t)
	s := seedFake(fake)
	conformance.Run(t, func(t *testing.T, _ *conformance.FakeAPI) conformance.Setup {
		return setup(t, fake, s)
	}, nil)
	if n := fake.CreateCount(); n != 0 {
		t.Fatalf("CreateCount after the fixture-only suite = %d, want 0", n)
	}
	if n := fake.BadSignatures(); n != 0 {
		t.Fatalf("%d request(s) failed SigV4 verification", n)
	}
}

func TestConformanceSuiteLiveLifecycle(t *testing.T) {
	fake := fakelightsail.New(t)
	s := seedFake(fake)
	conformance.Run(t, func(t *testing.T, _ *conformance.FakeAPI) conformance.Setup {
		return setup(t, fake, s)
	}, nil, conformance.WithLive("aws-fixture-live"))
	if n := fake.CreateCount(); n == 0 {
		t.Fatal("the live suite allocated nothing")
	}
	if n := fake.Losses(); n != 1 {
		t.Fatalf("injected lost responses consumed = %d, want 1 (the ambiguous-create check must fire)", n)
	}
	// No ORPHANED static IP: every address still allocated belongs to an
	// instance that still exists. (The suite's ambiguous-create check
	// deliberately leaves its instance standing; a destroy that left the
	// address behind would be the real leak, because an unattached
	// Lightsail static IP is billed.)
	for _, ip := range fake.StaticIPNames() {
		owner := strings.TrimSuffix(ip, "-ip")
		if !fake.Exists(owner) {
			t.Fatalf("static IP %s outlived instance %s and is still billing", ip, owner)
		}
	}
	if n := fake.BadSignatures(); n != 0 {
		t.Fatalf("%d request(s) failed SigV4 verification", n)
	}
}
