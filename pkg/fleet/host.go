package fleet

import (
	"fmt"

	"gopkg.in/yaml.v3"

	"github.com/qiangli/yoke/pkg/assetring"
)

// Host is a static reach alias for a machine.
//
// Hosts are NOT a registry the way tools, models, and agents are. A machine
// is discovered (mDNS), owned (an account), or aliased (ssh_config) — three
// sources of truth a fourth writable store would immediately drift from.
// This type exists only for reach hints that cannot be discovered: an
// address on a network with no mDNS, a non-default ssh port, the account
// name to use there.
//
// Resolution merges these entries with live discovery; see pkg/principal.
type Host struct {
	Name    string   `yaml:"name" json:"name"`
	Aliases []string `yaml:"aliases,omitempty" json:"aliases,omitempty"`
	Display string   `yaml:"display,omitempty" json:"display,omitempty"`

	// Address is where the machine actually answers — a DNS name or an
	// address literal. Empty means "resolve the name itself".
	Address string `yaml:"address,omitempty" json:"address,omitempty"`
	// SSHUser is the account name on THIS host. It is per-host on purpose:
	// the local $USER usually does not exist on the remote box.
	SSHUser string `yaml:"ssh_user,omitempty" json:"ssh_user,omitempty"`
	SSHPort int    `yaml:"ssh_port,omitempty" json:"ssh_port,omitempty"`
	// LANEndpoint is a service URL reachable only from the same network.
	LANEndpoint string `yaml:"lan_endpoint,omitempty" json:"lan_endpoint,omitempty"`
	Notes       string `yaml:"notes,omitempty" json:"notes,omitempty"`

	Ring assetring.Ring `yaml:"-" json:"ring"`
}

// Names returns the host's canonical name and every alias.
func (h Host) Names() []string { return names(h.Name, h.Aliases) }

// Target is the address to dial: the explicit one, else the name itself.
func (h Host) Target() string {
	if h.Address != "" {
		return h.Address
	}
	return h.Name
}

// ParseHost reads a host alias entry.
func ParseHost(name string, body []byte, src assetring.Source) (Host, error) {
	var h Host
	if err := yaml.Unmarshal(body, &h); err != nil {
		return Host{}, fmt.Errorf("fleet: host %q: %w", name, err)
	}
	if h.Name == "" {
		h.Name = name
	}
	if src != nil {
		h.Ring = src.Ring()
	}
	return h, nil
}

// Hosts returns every static host alias, name-sorted.
func (c *Catalog) Hosts() ([]Host, []error) {
	var errs []error
	cat := &assetring.Catalog[Host]{
		Sources: c.sources(dirHosts),
		Parse: func(n string, b []byte, s assetring.Source) Host {
			h, err := ParseHost(n, b, s)
			if err != nil {
				errs = append(errs, parseErr{n, err})
				return Host{Name: n, Ring: s.Ring()}
			}
			return h
		},
	}
	rows, err := cat.Rows()
	if err != nil {
		return nil, append(errs, err)
	}
	out := make([]Host, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Entry)
	}
	return out, errs
}

// Host resolves a static host alias by name or alias.
func (c *Catalog) Host(name string) (Host, bool) {
	hosts, _ := c.Hosts()
	for _, h := range hosts {
		for _, n := range h.Names() {
			if n == name {
				return h, true
			}
		}
	}
	return Host{}, false
}

// SaveHost writes a static host alias into the local store.
func (c *Catalog) SaveHost(h Host) error {
	if err := validName(h.Name); err != nil {
		return err
	}
	data, err := Marshal(h)
	if err != nil {
		return err
	}
	return writeEntry(c.nounDir(dirHosts), h.Name, data)
}

// RemoveHost deletes a static host alias from the local store.
func (c *Catalog) RemoveHost(name string) error {
	return removeEntry(c.nounDir(dirHosts), dirHosts, name)
}
