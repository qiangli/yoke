package fleet

import (
	"fmt"
	"sort"
	"strings"

	"github.com/qiangli/yoke/pkg/assetring"
	"gopkg.in/yaml.v3"
)

// Roster is a named default selection of fleet entries: which tools or
// agents a verb uses when the caller names none, and which agent fills a
// role. It is seeded DATA (rod, not fish): the defaults a verb used to carry
// as a Go literal — weave's default fleet, chat's role defaults, the meet
// secretary — live here, so an operator reshapes them with a same-named
// shared, org or local record instead of waiting for a release.
//
// A roster names entries; it never defines them. A member that resolves to
// nothing is reported by the reader that uses it, exactly as a hand-typed
// --fleet would be.
type Roster struct {
	RecordLifecycle `yaml:",inline" schema:"-"`
	Name            string            `yaml:"name" json:"name" doc:"roster id the reading verb looks up"`
	Display         string            `yaml:"display,omitempty" json:"display,omitempty" doc:"human-facing label"`
	Members         []string          `yaml:"members,omitempty" json:"members,omitempty" doc:"ordered tool or agent names"`
	Roles           map[string]string `yaml:"roles,omitempty" json:"roles,omitempty" doc:"role name to tool or agent name"`
	Notes           string            `yaml:"notes,omitempty" json:"notes,omitempty" doc:"why this selection"`

	Ring assetring.Ring `yaml:"-" json:"ring"`
}

// Seeded roster names read by the core verbs.
const (
	RosterWeave = "weave" // weave fleet / interview / heartbeat default fleet
	RosterRoles = "roles" // chat role defaults, meet secretary, sdlc fallback
)

// Validate rejects an empty or duplicated member list entry.
func (r Roster) Validate() error {
	seen := map[string]bool{}
	for _, m := range r.Members {
		m = strings.TrimSpace(m)
		if m == "" {
			return fmt.Errorf("fleet: roster %q: empty member", r.Name)
		}
		if seen[m] {
			return fmt.Errorf("fleet: roster %q: duplicate member %q", r.Name, m)
		}
		seen[m] = true
	}
	for role, who := range r.Roles {
		if strings.TrimSpace(role) == "" || strings.TrimSpace(who) == "" {
			return fmt.Errorf("fleet: roster %q: role entries need a name and a value", r.Name)
		}
	}
	return nil
}

// ParseRoster decodes one roster file.
func ParseRoster(name string, body []byte, src assetring.Source) (Roster, error) {
	var r Roster
	if err := yaml.Unmarshal(body, &r); err != nil {
		return Roster{}, fmt.Errorf("fleet: roster %q: %w", name, err)
	}
	if r.Name == "" {
		r.Name = name
	}
	if src != nil {
		r.Ring = src.Ring()
	}
	return r, r.Validate()
}

// Rosters returns every roster, name-sorted.
func (c *Catalog) Rosters() ([]Roster, []error) {
	var errs []error
	cat := &assetring.Catalog[Roster]{
		Sources: c.sources(dirRosters),
		Parse: func(n string, b []byte, s assetring.Source) Roster {
			errs = append(errs, sourceWarnings(s)...)
			r, err := ParseRoster(n, b, s)
			if err != nil {
				errs = append(errs, parseErr{n, err})
				return Roster{Name: n, Ring: s.Ring()}
			}
			return r
		},
	}
	rows, err := cat.Rows()
	if err != nil {
		return nil, append(errs, err)
	}
	out := make([]Roster, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Entry)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, errs
}

// Roster resolves a roster by name, retired or not (lifecycle verbs need the
// retired record; the default readers below skip it).
func (c *Catalog) Roster(name string) (Roster, bool) {
	if name == "" {
		return Roster{}, false
	}
	rows, _ := c.Rosters()
	for _, r := range rows {
		if r.Name == name {
			return r, true
		}
	}
	return Roster{}, false
}

// RosterMembers returns a roster's members, or fallback when the roster is
// absent or empty (seeds off and nothing mounted). The fallback is the
// reader's last-resort default, not a second source of truth.
func (c *Catalog) RosterMembers(name string, fallback []string) []string {
	if r, ok := c.Roster(name); ok && !r.IsRetired() && len(r.Members) > 0 {
		return append([]string(nil), r.Members...)
	}
	return append([]string(nil), fallback...)
}

// RosterRole returns the entry filling role in roster name, or fallback.
func (c *Catalog) RosterRole(name, role, fallback string) string {
	if r, ok := c.Roster(name); ok && !r.IsRetired() {
		if v := strings.TrimSpace(r.Roles[role]); v != "" {
			return v
		}
	}
	return fallback
}
