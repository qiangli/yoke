package fleet

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/qiangli/yoke/pkg/assetring"
)

// A registered resource is the operator's name for something agents must not
// use at the same time: a directory, a device, a seat, a license. It is the
// file to the fleet's binary — the thing itself lives elsewhere, and this
// record only says what it covers and under which claim kind it is held.
//
// A resource carries no claim logic of its own. Its Kind names the coord
// kind its claims are taken under (a builtin like "path", or a user kind
// declared by a resourcekind record), and its Members are what those claims
// cover. The fleetkinds provider turns the record into that kind's members,
// so entries are claimed through the same registry-derived kind path every
// other fleet noun uses.
//
// Removing a resource never destroys the underlying thing: it only drops the
// record, and a live claim on it refuses the removal first.
type Resource struct {
	RecordLifecycle `yaml:",inline" schema:"-"`
	Name            string   `yaml:"name" json:"name" doc:"canonical resource name"`
	Kind            string   `yaml:"kind" json:"kind" doc:"claim kind the resource is held under: a builtin (name, path, repo) or a resourcekind name"`
	Members         []string `yaml:"members,omitempty" json:"members,omitempty" doc:"what claims on this resource cover: paths, seats, or member strings, depending on the kind"`
	Title           string   `yaml:"title,omitempty" json:"title,omitempty" doc:"human-facing label"`
	Notes           string   `yaml:"notes,omitempty" json:"notes,omitempty" doc:"free-form notes"`
	Aliases         []string `yaml:"aliases,omitempty" json:"aliases,omitempty" doc:"alternate accepted names"`
	TTL             string   `yaml:"ttl,omitempty" json:"ttl,omitempty" doc:"Go duration overriding the kind's claim TTL; empty means the kind's own"`
	Mode            string   `yaml:"mode,omitempty" json:"mode,omitempty" doc:"narrows the kind's permitted claim modes to this one; empty means the kind's own"`
	Guard           []string `yaml:"guard,omitempty" json:"guard,omitempty" doc:"argv prefixes another lane enforces; stored here, never interpreted"`

	Ring assetring.Ring `yaml:"-" json:"ring"`
}

// Names returns the resource's canonical name and every alias.
func (r Resource) Names() []string { return names(r.Name, r.Aliases) }

// ParseResource reads a resource entry. name is the fallback identity.
func ParseResource(name string, body []byte, src assetring.Source) (Resource, error) {
	var r Resource
	dec := yaml.NewDecoder(strings.NewReader(string(body)))
	dec.KnownFields(true)
	if err := dec.Decode(&r); err != nil && !strings.Contains(err.Error(), "EOF") {
		return Resource{}, fmt.Errorf("fleet: resource %q: %w", name, err)
	}
	if r.Name == "" {
		r.Name = name
	}
	if src != nil {
		r.Ring = src.Ring()
	}
	return r, nil
}

// Validate is the structural check a resource write answers to. The kind
// itself is not resolved here: a resourcekind may be registered after the
// resource that names it, so an unknown kind is a claim-time question, not a
// write-time one.
func (r Resource) Validate() error {
	if err := validName(r.Name); err != nil {
		return err
	}
	if strings.ContainsAny(r.Name, " \t\r\n") {
		return fmt.Errorf("fleet: resource name %q: no whitespace", r.Name)
	}
	if strings.TrimSpace(r.Kind) == "" {
		return fmt.Errorf("fleet: resource %q: kind is required (a builtin claim kind or a resourcekind name)", r.Name)
	}
	if r.TTL != "" {
		if _, err := time.ParseDuration(r.TTL); err != nil {
			return fmt.Errorf("fleet: resource %q: ttl %q is not a Go duration: %w", r.Name, r.TTL, err)
		}
	}
	if r.Mode != "" && !slices.Contains(claimModes, r.Mode) {
		return fmt.Errorf("fleet: resource %q: mode %q is not one of %s", r.Name, r.Mode, strings.Join(claimModes, "|"))
	}
	return nil
}

// A registered resourcekind declares a user claim kind: the match rule its
// claims collide under, the domain they collide in, and the hook commands
// that resolve a name to members and probe whether it exists. Resourcekind
// records register coord kinds at load, so user kinds work wherever builtin
// kinds do.
type ResourceKind struct {
	RecordLifecycle `yaml:",inline" schema:"-"`
	Name            string   `yaml:"name" json:"name" doc:"claim kind name this record declares"`
	Match           string   `yaml:"match,omitempty" json:"match,omitempty" doc:"collision rule: name (same name), member (shared member), or path (equal or containing paths)"`
	Domain          string   `yaml:"domain,omitempty" json:"domain,omitempty" doc:"collision scope; kinds in different domains never conflict; empty means the kind's own name"`
	TTL             string   `yaml:"ttl,omitempty" json:"ttl,omitempty" doc:"Go duration overriding the package claim TTL for lease-mode claims of this kind"`
	Modes           []string `yaml:"modes,omitempty" json:"modes,omitempty" doc:"claim modes the kind permits; empty permits all three"`
	Resolve         string   `yaml:"resolve,omitempty" json:"resolve,omitempty" doc:"registered command turning a name into members; absent claims just the name"`
	Probe           string   `yaml:"probe,omitempty" json:"probe,omitempty" doc:"registered command testing a name: exit 0 free, 1 busy, anything else unknown"`

	Ring assetring.Ring `yaml:"-" json:"ring"`
}

// ParseResourceKind reads a resourcekind entry. name is the fallback identity.
func ParseResourceKind(name string, body []byte, src assetring.Source) (ResourceKind, error) {
	var r ResourceKind
	dec := yaml.NewDecoder(strings.NewReader(string(body)))
	dec.KnownFields(true)
	if err := dec.Decode(&r); err != nil && !strings.Contains(err.Error(), "EOF") {
		return ResourceKind{}, fmt.Errorf("fleet: resourcekind %q: %w", name, err)
	}
	if r.Name == "" {
		r.Name = name
	}
	if src != nil {
		r.Ring = src.Ring()
	}
	return r, nil
}

// Validate is the structural check a resourcekind write answers to. The
// hooks are not resolved here: a command may be registered after the kind
// that names it, and a hook that names nothing registered is refused when it
// runs, never when it is written.
func (r ResourceKind) Validate() error {
	if err := validName(r.Name); err != nil {
		return err
	}
	if strings.ContainsAny(r.Name, " \t\r\n") {
		return fmt.Errorf("fleet: resourcekind name %q: no whitespace", r.Name)
	}
	if r.Match != "" && !slices.Contains(matchRules, r.Match) {
		return fmt.Errorf("fleet: resourcekind %q: match %q is not one of %s", r.Name, r.Match, strings.Join(matchRules, "|"))
	}
	if r.TTL != "" {
		if _, err := time.ParseDuration(r.TTL); err != nil {
			return fmt.Errorf("fleet: resourcekind %q: ttl %q is not a Go duration: %w", r.Name, r.TTL, err)
		}
	}
	for _, m := range r.Modes {
		if !slices.Contains(claimModes, m) {
			return fmt.Errorf("fleet: resourcekind %q: mode %q is not one of %s", r.Name, m, strings.Join(claimModes, "|"))
		}
	}
	return nil
}

// claimModes is the closed vocabulary a resource or resourcekind mode answers
// to. It mirrors coord's modes without importing the claim engine — this
// package must stay importable from it.
const (
	claimModeLease    = "lease"
	claimModeAttached = "attached"
	claimModeAnnounce = "announce"
)

var claimModes = []string{claimModeLease, claimModeAttached, claimModeAnnounce}

// matchRules is the closed vocabulary a resourcekind match answers to,
// mirroring coord's match rules for the same import-graph reason.
var matchRules = []string{"name", "member", "path"}

// --- catalog ---------------------------------------------------------------

// Resources returns every registered resource, name-sorted, across the
// shared and local rings. Entries that fail to parse are reported, never
// hidden.
func (c *Catalog) Resources() ([]Resource, []error) {
	var errs []error
	cat := &assetring.Catalog[Resource]{
		Sources: c.sources(dirResources),
		Parse: func(n string, b []byte, s assetring.Source) Resource {
			r, err := ParseResource(n, b, s)
			if err != nil {
				errs = append(errs, parseErr{n, err})
				return Resource{Name: n, Ring: s.Ring()}
			}
			return r
		},
	}
	rows, err := cat.Rows()
	if err != nil {
		return nil, append(errs, err)
	}
	out := make([]Resource, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Entry)
	}
	return out, errs
}

// Resource resolves a registered resource by canonical name or alias.
func (c *Catalog) Resource(name string) (Resource, bool) {
	all, _ := c.Resources()
	for _, r := range all {
		if slices.Contains(r.Names(), name) {
			return r, true
		}
	}
	return Resource{}, false
}

// SaveResource validates and writes a resource into the local store.
func (c *Catalog) SaveResource(r Resource) error {
	if err := r.Validate(); err != nil {
		return err
	}
	data, err := Marshal(r)
	if err != nil {
		return err
	}
	return writeEntry(c.nounDir(dirResources), r.Name, data)
}

// RemoveResource deletes a resource from the local store. It drops only the
// record: the underlying thing is never touched.
func (c *Catalog) RemoveResource(name string) error {
	return removeEntry(c.nounDir(dirResources), dirResources, name)
}

// ResourceKinds returns every registered resourcekind, name-sorted, across
// the shared and local rings. Entries that fail to parse are reported, never
// hidden.
func (c *Catalog) ResourceKinds() ([]ResourceKind, []error) {
	var errs []error
	cat := &assetring.Catalog[ResourceKind]{
		Sources: c.sources(dirResourceKinds),
		Parse: func(n string, b []byte, s assetring.Source) ResourceKind {
			r, err := ParseResourceKind(n, b, s)
			if err != nil {
				errs = append(errs, parseErr{n, err})
				return ResourceKind{Name: n, Ring: s.Ring()}
			}
			return r
		},
	}
	rows, err := cat.Rows()
	if err != nil {
		return nil, append(errs, err)
	}
	out := make([]ResourceKind, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Entry)
	}
	return out, errs
}

// ResourceKind resolves a registered resourcekind by name. Kind names are
// bare identifiers, so aliases would collide with the claim syntax; kinds
// resolve by canonical name only.
func (c *Catalog) ResourceKind(name string) (ResourceKind, bool) {
	all, _ := c.ResourceKinds()
	for _, r := range all {
		if r.Name == name {
			return r, true
		}
	}
	return ResourceKind{}, false
}

// SaveResourceKind validates and writes a resourcekind into the local store.
func (c *Catalog) SaveResourceKind(r ResourceKind) error {
	if err := r.Validate(); err != nil {
		return err
	}
	data, err := Marshal(r)
	if err != nil {
		return err
	}
	return writeEntry(c.nounDir(dirResourceKinds), r.Name, data)
}

// RemoveResourceKind deletes a resourcekind from the local store.
func (c *Catalog) RemoveResourceKind(name string) error {
	return removeEntry(c.nounDir(dirResourceKinds), dirResourceKinds, name)
}
