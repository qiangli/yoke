package cligw

import (
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	PolicyQuotaFirst   = "quota-first"
	PolicyLatencyFirst = "latency-first"
	PolicyRoundRobin   = "round-robin"

	EscalateNone = "none"
	EscalateUp   = "up"
)

// Policy is the operator's routing policy. Weights accepts provider (vendor)
// names; ProviderWeights is an explicit spelling of the same setting. When a
// name occurs in both maps, ProviderWeights wins.
type Policy struct {
	Default               string             `json:"default" yaml:"default"`
	DefaultPolicy         string             `json:"default_policy,omitempty" yaml:"default_policy,omitempty"`
	ReserveFloor          float64            `json:"reserve_floor" yaml:"reserve_floor"`
	Weights               map[string]float64 `json:"weights,omitempty" yaml:"weights,omitempty"`
	VendorWeights         map[string]float64 `json:"vendor_weights,omitempty" yaml:"vendor_weights,omitempty"`
	ProviderWeights       map[string]float64 `json:"provider_weights,omitempty" yaml:"provider_weights,omitempty"`
	Escalate              string             `json:"escalate" yaml:"escalate"`
	Filter                Filter             `json:"filter,omitempty" yaml:"filter,omitempty"`
	VendorConcurrencyCaps map[string]int     `json:"vendor_concurrency_caps,omitempty" yaml:"vendor_concurrency_caps,omitempty"`
	VendorConcurrency     map[string]int     `json:"vendor_concurrency,omitempty" yaml:"vendor_concurrency,omitempty"`
	ConcurrencyCaps       map[string]int     `json:"concurrency_caps,omitempty" yaml:"concurrency_caps,omitempty"`
}

// DefaultPolicy returns the routing defaults used when policy.yaml is absent.
func DefaultPolicy() Policy {
	return Policy{
		Default:               PolicyQuotaFirst,
		ReserveFloor:          0.15,
		Weights:               map[string]float64{},
		VendorWeights:         map[string]float64{},
		ProviderWeights:       map[string]float64{},
		Escalate:              EscalateNone,
		VendorConcurrencyCaps: map[string]int{},
		VendorConcurrency:     map[string]int{},
		ConcurrencyCaps:       map[string]int{},
	}
}

// PolicyPath resolves $BASHY_HOME/cligw/policy.yaml, or the normal
// ~/.bashy/cligw/policy.yaml when the bashy home has not been relocated.
func PolicyPath() (string, error) {
	if home := strings.TrimSpace(os.Getenv("BASHY_HOME")); home != "" {
		return filepath.Join(home, "cligw", "policy.yaml"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cligw: resolve policy home: %w", err)
	}
	return filepath.Join(home, ".bashy", "cligw", "policy.yaml"), nil
}

// LoadPolicy loads the host policy when present and otherwise returns the
// documented defaults.
func LoadPolicy() (Policy, error) {
	path, err := PolicyPath()
	if err != nil {
		return Policy{}, err
	}
	return LoadPolicyFile(path)
}

// LoadPolicyFile loads one policy file. A missing file is not an error.
func LoadPolicyFile(path string) (Policy, error) {
	p := DefaultPolicy()
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return p, nil
	}
	if err != nil {
		return Policy{}, fmt.Errorf("cligw: open policy: %w", err)
	}
	defer f.Close()
	dec := yaml.NewDecoder(f)
	dec.KnownFields(true)
	if err := dec.Decode(&p); err != nil {
		return Policy{}, fmt.Errorf("cligw: decode policy: %w", err)
	}
	if err := p.Validate(); err != nil {
		return Policy{}, err
	}
	return p, nil
}

// Validate rejects policy values whose meaning would otherwise be ambiguous.
func (p *Policy) Validate() error {
	if strings.TrimSpace(p.DefaultPolicy) != "" {
		if strings.TrimSpace(p.Default) != "" && p.Default != PolicyQuotaFirst && p.Default != p.DefaultPolicy {
			return fmt.Errorf("cligw: default and default_policy disagree")
		}
		p.Default = p.DefaultPolicy
	}
	p.Default = strings.TrimSpace(p.Default)
	if p.Default == "" {
		p.Default = PolicyQuotaFirst
	}
	if !validPolicyName(p.Default) {
		return fmt.Errorf("cligw: unknown default policy %q", p.Default)
	}
	if math.IsNaN(p.ReserveFloor) || math.IsInf(p.ReserveFloor, 0) || p.ReserveFloor < 0 || p.ReserveFloor > 1 {
		return fmt.Errorf("cligw: reserve_floor must be between 0 and 1")
	}
	p.Escalate = strings.TrimSpace(p.Escalate)
	if p.Escalate == "" {
		p.Escalate = EscalateNone
	}
	if p.Escalate != EscalateNone && p.Escalate != EscalateUp {
		return fmt.Errorf("cligw: escalate must be %q or %q", EscalateNone, EscalateUp)
	}
	if p.Weights == nil {
		p.Weights = map[string]float64{}
	}
	if p.ProviderWeights == nil {
		p.ProviderWeights = map[string]float64{}
	}
	if p.VendorWeights == nil {
		p.VendorWeights = map[string]float64{}
	}
	for _, weights := range []map[string]float64{p.Weights, p.VendorWeights, p.ProviderWeights} {
		for provider, weight := range weights {
			if strings.TrimSpace(provider) == "" || math.IsNaN(weight) || math.IsInf(weight, 0) || weight < 0 {
				return fmt.Errorf("cligw: invalid provider weight for %q", provider)
			}
		}
	}
	if p.VendorConcurrencyCaps == nil {
		p.VendorConcurrencyCaps = map[string]int{}
	}
	for vendor, cap := range p.ConcurrencyCaps {
		if _, exists := p.VendorConcurrencyCaps[vendor]; !exists {
			p.VendorConcurrencyCaps[vendor] = cap
		}
	}
	for vendor, cap := range p.VendorConcurrency {
		p.VendorConcurrencyCaps[vendor] = cap
	}
	for vendor, cap := range p.VendorConcurrencyCaps {
		if strings.TrimSpace(vendor) == "" || cap < 1 {
			return fmt.Errorf("cligw: invalid vendor concurrency cap for %q", vendor)
		}
	}
	return nil
}

func validPolicyName(name string) bool {
	if name == PolicyQuotaFirst || name == PolicyLatencyFirst || name == PolicyRoundRobin {
		return true
	}
	value, ok := strings.CutPrefix(name, "prefer:")
	return ok && strings.TrimSpace(value) != ""
}

func (p Policy) weight(provider string) float64 {
	if weight, ok := p.ProviderWeights[provider]; ok {
		return weight
	}
	if weight, ok := p.VendorWeights[provider]; ok {
		return weight
	}
	return p.Weights[provider]
}
