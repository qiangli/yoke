// Package providerselector resolves open service roles across registered commands
// and vendor tool commands. It never runs a provider.
package providerselector

import (
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/spacetime"
)

type Candidate struct {
	Name     string   `json:"name"`
	Role     string   `json:"role"`
	Route    string   `json:"route"`
	Kind     string   `json:"kind"`
	Effects  []string `json:"effects,omitempty"`
	Rejected string   `json:"rejected,omitempty"`
}

type Options struct {
	Role     string
	Provider string
	Route    string
	Request  any
	Yes      bool
	Probes   *spacetime.ProbeSet
}

func Project(records []fleet.Command, tools []fleet.Tool, opt Options) []Candidate {
	probes := opt.Probes
	if probes == nil {
		probes = spacetime.DefaultProbes(spacetime.NopCache())
	}
	out := make([]Candidate, 0)
	for _, rec := range records {
		if !rec.ProvidesRole(opt.Role) {
			continue
		}
		route := rec.Route
		if route == "" {
			route = "oss"
		}
		c := Candidate{Name: rec.Name, Role: opt.Role, Route: route, Kind: "command", Effects: rec.Effects}
		if opt.Route != "" && opt.Provider == "" && opt.Route != route {
			c.Rejected = "route is " + route
		}
		if c.Rejected == "" {
			if verdict := rec.Applicability(probes); !verdict.Applicable {
				c.Rejected = verdict.Failing
			}
		}
		if c.Rejected == "" {
			if ok, reason := rec.AcceptsJSON(opt.Request); !ok {
				c.Rejected = reason
			}
		}
		if c.Rejected == "" && !opt.Yes && needsYes(c.Effects) {
			c.Rejected = "requires --yes for spend/send effect"
		}
		out = append(out, c)
	}
	for _, tool := range tools {
		for _, command := range tool.Commands {
			head, _, _ := strings.Cut(opt.Role, ".")
			if command.Name != opt.Role && command.Name != head {
				continue
			}
			c := Candidate{Name: tool.Name + ":" + command.Name, Role: opt.Role, Route: "vendor", Kind: "tool-command", Effects: command.Effects}
			if opt.Route != "" && opt.Provider == "" && opt.Route != "vendor" {
				c.Rejected = "route is vendor"
			}
			if c.Rejected == "" && !tool.Active() {
				c.Rejected = "vendor CLI is not installed"
			}
			if c.Rejected == "" && !opt.Yes {
				if needsYes(command.Effects) {
					c.Rejected = "requires --yes for spend/send effect"
				} else if need, reason := command.NeedsConfirmation(); need {
					c.Rejected = "requires --yes: " + reason
				}
			}
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if rank(out[i].Route) != rank(out[j].Route) {
			return rank(out[i].Route) < rank(out[j].Route)
		}
		return out[i].Name < out[j].Name
	})
	return out
}

func Select(candidates []Candidate, opt Options) (Candidate, error) {
	if opt.Provider != "" {
		for _, c := range candidates {
			if c.Name == opt.Provider || strings.HasPrefix(c.Name, opt.Provider+":") {
				if c.Rejected != "" {
					return Candidate{}, fmt.Errorf("provider %q is unavailable: %s", c.Name, c.Rejected)
				}
				return c, nil
			}
		}
		return Candidate{}, fmt.Errorf("provider %q does not provide %q", opt.Provider, opt.Role)
	}
	if len(candidates) == 0 {
		return Candidate{}, fmt.Errorf("no provider declares %q", opt.Role)
	}
	rankNow := 99
	var matches []Candidate
	var rejected []string
	for _, c := range candidates {
		if c.Rejected != "" {
			rejected = append(rejected, c.Name+": "+c.Rejected)
			continue
		}
		r := rank(c.Route)
		if r < rankNow {
			rankNow = r
			matches = nil
		}
		if r == rankNow {
			matches = append(matches, c)
		}
	}
	if len(matches) == 1 {
		return matches[0], nil
	}
	if len(matches) > 1 {
		names := make([]string, 0, len(matches))
		for _, c := range matches {
			names = append(names, c.Name)
		}
		slices.Sort(names)
		return Candidate{}, fmt.Errorf("ambiguous %s providers: %s; pass --provider NAME", opt.Role, strings.Join(names, ", "))
	}
	return Candidate{}, fmt.Errorf("no provider supports %q (%s)", opt.Role, strings.Join(rejected, "; "))
}

func rank(route string) int {
	switch route {
	case "oss":
		return 0
	case "vendor":
		return 1
	case "browser":
		return 2
	case "api":
		return 3
	}
	return 4
}
func needsYes(effects []string) bool {
	for _, e := range effects {
		if e == "spend" || e == "send" {
			return true
		}
	}
	return false
}
