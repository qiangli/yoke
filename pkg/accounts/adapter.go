package accounts

import (
	"context"
	"fmt"
	"sync"
)

type Capability uint32

const (
	Discover Capability = 1 << iota
	Verify
	Resources
	Usage
	Billing
	Select
)

type Operation string
type Selection struct {
	AccountID     string `json:"account_id"`
	ConnectionID  string `json:"connection_id,omitempty"`
	Endpoint      string `json:"endpoint,omitempty"`
	Profile       string `json:"profile,omitempty"`
	CredentialRef string `json:"credential_ref,omitempty"`
	AuthMethod    string `json:"auth_method,omitempty"`
	Username      string `json:"username,omitempty"`
}
type Adapter interface {
	Name() string
	Capabilities() Capability
	Discover(context.Context) ([]Observation, error)
	Verify(context.Context, Account) (Observation, error)
	Resources(context.Context, Account) ([]ResourceRef, error)
	Usage(context.Context, Account, string) ([]Metric, error)
	Billing(context.Context, Account, string) ([]Metric, error)
	Select(context.Context, Account, Operation) (Selection, error)
}
type Registry struct {
	mu       sync.RWMutex
	adapters map[string]Adapter
}

func NewRegistry() *Registry { return &Registry{adapters: make(map[string]Adapter)} }
func (r *Registry) Register(a Adapter) error {
	if a == nil || a.Name() == "" {
		return fmt.Errorf("accounts: invalid adapter")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.adapters[a.Name()]; ok {
		return fmt.Errorf("accounts: adapter already registered")
	}
	r.adapters[a.Name()] = a
	return nil
}
func (r *Registry) Adapter(name string, cap Capability) (Adapter, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	a, ok := r.adapters[name]
	if !ok || a.Capabilities()&cap == 0 {
		return nil, ErrUnsupported
	}
	return a, nil
}

// Resolve selects one account connection and returns safe metadata only.
func (s *Store) Resolve(ref string, _ Operation) (Selection, error) {
	a, err := s.Get(ref)
	if err != nil {
		return Selection{}, err
	}
	if a.Status != Resolved {
		return Selection{}, ErrUnsupported
	}
	if len(a.Connections) != 1 {
		return Selection{}, ErrAmbiguous
	}
	c := a.Connections[0]
	if c.CredentialRef != "" && !ValidCredentialRef(c.CredentialRef) {
		return Selection{}, ErrUnsupported
	}
	return Selection{AccountID: a.ID, ConnectionID: c.ID, Endpoint: c.Endpoint, Profile: c.Profile, CredentialRef: c.CredentialRef, AuthMethod: c.AuthMethod, Username: a.Username}, nil
}
