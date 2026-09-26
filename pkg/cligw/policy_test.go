package cligw

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadPolicyDefaultsAndYAML(t *testing.T) {
	home := t.TempDir()
	t.Setenv("BASHY_HOME", home)
	p, err := LoadPolicy()
	if err != nil {
		t.Fatal(err)
	}
	if p.Default != PolicyQuotaFirst || p.ReserveFloor != 0.15 || p.Escalate != EscalateNone {
		t.Fatalf("defaults = %+v", p)
	}

	path, err := PolicyPath()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(home, "cligw", "policy.yaml"); path != want {
		t.Fatalf("PolicyPath() = %q, want %q", path, want)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	body := []byte(`default: latency-first
reserve_floor: 0.25
provider_weights:
  openai: 3
weights:
  anthropic: 2
escalate: up
filter:
  kind: subscription
vendor_concurrency_caps:
  openai: 4
`)
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	p, err = LoadPolicy()
	if err != nil {
		t.Fatal(err)
	}
	if p.Default != PolicyLatencyFirst || p.ReserveFloor != 0.25 || p.Escalate != EscalateUp {
		t.Fatalf("loaded scalar policy = %+v", p)
	}
	if p.weight("openai") != 3 || p.weight("anthropic") != 2 || p.Filter.Kind != "subscription" || p.VendorConcurrencyCaps["openai"] != 4 {
		t.Fatalf("loaded policy maps = %+v", p)
	}
}

func TestLoadPolicyRejectsInvalidValuesAndUnknownFields(t *testing.T) {
	for name, body := range map[string]string{
		"floor":    "reserve_floor: 1.1\n",
		"policy":   "default: cheapest\n",
		"escalate": "escalate: down\n",
		"unknown":  "mystery: true\n",
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "policy.yaml")
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadPolicyFile(path); err == nil {
				t.Fatalf("LoadPolicyFile accepted %q", body)
			}
		})
	}
}
