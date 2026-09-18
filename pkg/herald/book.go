package herald

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/qiangli/yoke/pkg/fleet"
)

// Book is the peer address book.
//
// It is NOT a new registry. A2A standardized discovery only as far as a
// well-known URI — there is no registry API — so the local address book is
// the missing layer, and the fleet catalog already is one: YAML per entry,
// ring precedence embedded → shared → cloud → local, an org overlay for free.
// A peer is therefore a Model whose provider is ProviderA2A, and Book is a
// thin projection over the catalog.
type Book struct {
	root string
}

// NewBook opens the address book under root. An empty root means the default
// fleet root (~/.config/bashy unless $BASHY_FLEET_DIR says otherwise).
func NewBook(root string) *Book {
	if root == "" {
		root = fleet.DefaultRoot()
	}
	return &Book{root: root}
}

// catalog builds a catalog rooted at the book's root.
func (b *Book) catalog() *fleet.Catalog {
	return fleet.New(fleet.WithRoot(b.root))
}

// modelsDir is where peer entries are written.
func (b *Book) modelsDir() string { return fleet.NounDir(b.root, "models") }

// List returns every peer, name-sorted.
//
// Only entries carrying ProviderA2A are peers; the same catalog holds LLM
// models, and confusing the two would offer an inference endpoint as an agent.
func (b *Book) List() ([]Peer, error) {
	models, errs := b.catalog().Models()
	var out []Peer
	for _, m := range models {
		if !strings.EqualFold(m.Provider, ProviderA2A) {
			continue
		}
		out = append(out, Peer{
			Name:      m.Name,
			URL:       m.BaseURL,
			APIKeyRef: m.APIKeyRef,
			Display:   m.Display,
			Band:      m.Band,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	if len(out) == 0 && len(errs) > 0 {
		return nil, fmt.Errorf("herald: reading peers: %w", errs[0])
	}
	return out, nil
}

// Get resolves one peer by name.
func (b *Book) Get(name string) (Peer, error) {
	peers, err := b.List()
	if err != nil {
		return Peer{}, err
	}
	for _, p := range peers {
		if p.Name == name {
			return p, nil
		}
	}
	// Distinguish "not a peer" from "not known at all" — an operator who
	// typed an LLM model name deserves to be told that, not "unknown".
	if m, ok := b.catalog().Model(name); ok {
		return Peer{}, fmt.Errorf("herald: %q is a model (provider %q), not an A2A peer; add one with `herald add %s <url>`",
			name, m.Provider, name)
	}
	return Peer{}, fmt.Errorf("herald: no peer named %q — `herald list` shows the address book", name)
}

// Add writes a peer to the host-local ring.
//
// Band is deliberately not a parameter. An AgentCard's skills[] is a
// self-asserted claim, and recording a self-declared capability is exactly
// what the leveling rules forbid: a peer enters unpegged (band 0) and earns a
// band only from a host-measured gate.
func (b *Book) Add(p Peer) error {
	p.Band = 0
	if err := p.Validate(); err != nil {
		return err
	}
	if existing, err := b.Get(p.Name); err == nil {
		return fmt.Errorf("herald: peer %q already points at %s — remove it first", p.Name, existing.URL)
	}
	dir := b.modelsDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("herald: %w", err)
	}
	path := filepath.Join(dir, p.Name+".yaml")
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("herald: %s already exists", path)
	}
	if err := os.WriteFile(path, []byte(p.yaml()), 0o644); err != nil {
		return fmt.Errorf("herald: %w", err)
	}
	return nil
}

// Remove deletes a peer from the host-local ring.
func (b *Book) Remove(name string) error {
	path := filepath.Join(b.modelsDir(), name+".yaml")
	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("herald: no local peer file for %q (a peer from a shared or org ring cannot be removed here)", name)
	}
	return os.Remove(path)
}

// yaml renders the peer as a fleet Model document.
//
// It is written as a Model, not a bespoke format, so every existing consumer —
// the capability matrix, `meet --participant`, weave, foreman — resolves
// `herald:<name>` with no special case.
func (p Peer) yaml() string {
	var sb strings.Builder
	sb.WriteString("# An A2A peer: an agent on someone else's infrastructure.\n")
	sb.WriteString("# Reached as the binding `" + p.Binding() + "`.\n")
	sb.WriteString("name: " + p.Name + "\n")
	if p.Display != "" {
		sb.WriteString("display: " + p.Display + "\n")
	}
	sb.WriteString("kind: api\n")
	sb.WriteString("provider: " + ProviderA2A + "\n")
	sb.WriteString("base_url: " + p.URL + "\n")
	if p.APIKeyRef != "" {
		sb.WriteString("api_key_ref: " + p.APIKeyRef + "\n")
	}
	sb.WriteString("# Unpegged. A card's skills[] is a claim, not a measurement:\n")
	sb.WriteString("# a band is earned from a gate this host ran, never from what the peer says.\n")
	sb.WriteString("band: 0\n")
	sb.WriteString("band_source: declared\n")
	return sb.String()
}
