package fleet

import (
	"fmt"
	"sort"

	"github.com/qiangli/yoke/pkg/assetring"
	"gopkg.in/yaml.v3"
)

// Normalized subscription plan tiers, lowest to highest.
//
// A tier is the plan's POSITION ON ITS OWN VENDOR'S LADDER, never the vendor's
// word for it: OpenAI's top individual plan is called "Pro" and is PlanTierMax,
// while z.ai's "Pro" sits below its "Max" and is PlanTierPro. The vendor's
// spelling lives in Plan.VendorPlan; only the normalized tier is routable.
const (
	PlanTierFree  = "free"  // no paid seat
	PlanTierEntry = "entry" // the vendor's lowest paid plan
	PlanTierPro   = "pro"   // a paid plan below the vendor's highest
	PlanTierMax   = "max"   // the vendor's highest individual plan
)

// PlanTiers lists the tier vocabulary, lowest first.
func PlanTiers() []string { return []string{PlanTierFree, PlanTierEntry, PlanTierPro, PlanTierMax} }

// PlanTierRank orders a tier: 1 (free) to 4 (max). 0 means unknown — an empty
// or unrecognized tier — and callers must treat 0 as "no preference", never as
// "worst": an unrecorded plan is fail-open, not demoted.
func PlanTierRank(tier string) int {
	for i, t := range PlanTiers() {
		if t == tier {
			return i + 1
		}
	}
	return 0
}

// Plan limit windows and units. A limit is recorded ONLY when a vendor page or
// a bashy observation states it; nothing here is estimated. Consumers use the
// windows and units they meter and ignore the rest (fail-open) — a 5-hour
// credit budget is real information even for a meter that counts tokens per day.
const (
	PlanWindow5h    = "5h"
	PlanWindowDay   = "day"
	PlanWindowWeek  = "week"
	PlanWindowMonth = "month"
	PlanWindowNone  = "concurrent" // an instantaneous ceiling, not a window

	PlanUnitTokens   = "tokens"
	PlanUnitRequests = "requests"
	PlanUnitPrompts  = "prompts"
	PlanUnitMessages = "messages"
	PlanUnitCredits  = "credits"
)

// Plan evidence: what a limit rests on.
const (
	PlanEvidencePublished = "published" // stated on the vendor's own page
	PlanEvidenceMeasured  = "measured"  // observed by bashy on the wire
)

// Plan is a subscription seat: which vendor plan the fleet is billed through,
// where it sits on that vendor's ladder, and the limits KNOWN about it.
//
// It is data, like the model roster: the seeded records are the operator's
// plans as of AsOf, and a same-named record in the local store
// (~/.config/bashy/plans/<name>.yaml) or a shared dir on $BASHY_PLANS_PATH
// replaces one when the seat changes. Go never branches on a plan name.
type Plan struct {
	Name       string   `yaml:"name" json:"name" doc:"plan id models reference with plan:"`
	Display    string   `yaml:"display,omitempty" json:"display,omitempty" doc:"human-facing label"`
	Vendor     string   `yaml:"vendor,omitempty" json:"vendor,omitempty" doc:"company selling the seat"`
	VendorPlan string   `yaml:"vendor_plan,omitempty" json:"vendor_plan,omitempty" doc:"the vendor's own name for the plan"`
	Tier       string   `yaml:"tier,omitempty" json:"tier,omitempty" doc:"normalized position on the vendor's ladder: free, entry, pro, or max"`
	AsOf       string   `yaml:"as_of,omitempty" json:"as_of,omitempty" doc:"date (YYYY-MM-DD) the record was checked against its sources"`
	Sources    []string `yaml:"sources,omitempty" json:"sources,omitempty" doc:"vendor pages the record was checked against"`
	Notes      string   `yaml:"notes,omitempty" json:"notes,omitempty" doc:"what the sources say and do not say"`

	// Limits holds only KNOWN limits. An absent limit is unknown, and unknown
	// is fail-open: no consumer may invent a number for it.
	Limits []PlanLimit `yaml:"limits,omitempty" json:"limits,omitempty" doc:"known, sourced plan limits; absent means unknown"`

	Ring assetring.Ring `yaml:"-" json:"ring"`
}

// PlanLimit is one known ceiling on a plan.
type PlanLimit struct {
	Window   string `yaml:"window" json:"window" doc:"5h, day, week, month, or concurrent"`
	Unit     string `yaml:"unit" json:"unit" doc:"tokens, requests, prompts, messages, or credits"`
	Value    int64  `yaml:"value" json:"value" doc:"the ceiling in Unit per Window"`
	Model    string `yaml:"model,omitempty" json:"model,omitempty" doc:"model the limit applies to; empty = the whole plan"`
	Evidence string `yaml:"evidence,omitempty" json:"evidence,omitempty" doc:"published (vendor page) or measured (bashy observation)"`
	AsOf     string `yaml:"as_of,omitempty" json:"as_of,omitempty" doc:"date (YYYY-MM-DD) the limit was read or measured"`
	Source   string `yaml:"source,omitempty" json:"source,omitempty" doc:"URL or bashy record the limit comes from"`
}

// Rank is PlanTierRank of the plan's tier.
func (p Plan) Rank() int { return PlanTierRank(p.Tier) }

// Limit returns the known limit for a window and unit, scoped to model when a
// model-specific one exists, else the plan-wide one. !ok means unknown.
func (p Plan) Limit(window, unit, model string) (int64, bool) {
	var wide int64
	found := false
	for _, l := range p.Limits {
		if l.Window != window || l.Unit != unit || l.Value <= 0 {
			continue
		}
		if l.Model != "" && l.Model == model {
			return l.Value, true
		}
		if l.Model == "" && !found {
			wide, found = l.Value, true
		}
	}
	return wide, found
}

// Validate checks the vocabulary. Unknown tiers, windows, units and evidence
// are errors: a typo must not read as "unknown, fail-open" forever.
func (p Plan) Validate() error {
	if p.Tier != "" && PlanTierRank(p.Tier) == 0 {
		return fmt.Errorf("fleet: plan %q: tier %q is not one of %v", p.Name, p.Tier, PlanTiers())
	}
	for i, l := range p.Limits {
		switch l.Window {
		case PlanWindow5h, PlanWindowDay, PlanWindowWeek, PlanWindowMonth, PlanWindowNone:
		default:
			return fmt.Errorf("fleet: plan %q: limit %d: unknown window %q", p.Name, i, l.Window)
		}
		switch l.Unit {
		case PlanUnitTokens, PlanUnitRequests, PlanUnitPrompts, PlanUnitMessages, PlanUnitCredits:
		default:
			return fmt.Errorf("fleet: plan %q: limit %d: unknown unit %q", p.Name, i, l.Unit)
		}
		switch l.Evidence {
		case "", PlanEvidencePublished, PlanEvidenceMeasured:
		default:
			return fmt.Errorf("fleet: plan %q: limit %d: unknown evidence %q", p.Name, i, l.Evidence)
		}
		if l.Value <= 0 {
			return fmt.Errorf("fleet: plan %q: limit %d: value must be positive (omit an unknown limit)", p.Name, i)
		}
		if l.Source == "" {
			return fmt.Errorf("fleet: plan %q: limit %d: a limit needs its source", p.Name, i)
		}
	}
	return nil
}

// ParsePlan decodes one plan file.
func ParsePlan(name string, body []byte, src assetring.Source) (Plan, error) {
	var p Plan
	if err := yaml.Unmarshal(body, &p); err != nil {
		return Plan{}, fmt.Errorf("fleet: plan %q: %w", name, err)
	}
	if p.Name == "" {
		p.Name = name
	}
	if src != nil {
		p.Ring = src.Ring()
	}
	return p, p.Validate()
}

// Plans returns every subscription plan, name-sorted.
func (c *Catalog) Plans() ([]Plan, []error) {
	var errs []error
	cat := &assetring.Catalog[Plan]{
		Sources: c.sources(dirPlans),
		Parse: func(n string, b []byte, s assetring.Source) Plan {
			p, err := ParsePlan(n, b, s)
			if err != nil {
				errs = append(errs, parseErr{n, err})
				return Plan{Name: n, Ring: s.Ring()}
			}
			return p
		},
	}
	rows, err := cat.Rows()
	if err != nil {
		return nil, append(errs, err)
	}
	out := make([]Plan, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Entry)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, errs
}

// Plan resolves a plan by name.
func (c *Catalog) Plan(name string) (Plan, bool) {
	if name == "" {
		return Plan{}, false
	}
	plans, _ := c.Plans()
	for _, p := range plans {
		if p.Name == name {
			return p, true
		}
	}
	return Plan{}, false
}

// ModelPlan resolves the plan a model is billed through. !ok — no plan named,
// or a dangling name — means unknown.
func (c *Catalog) ModelPlan(m Model) (Plan, bool) { return c.Plan(m.Plan) }
