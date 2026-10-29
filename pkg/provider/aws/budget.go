// The budget preflight (S522.2 item 4; the plan's "Budget guard" section
// is binding).
//
// Nothing is created until the money is checked, and the check is the
// operator's cap, not AWS's: the bundle's MONTHLY price plus the account's
// month-to-date UNBLENDED cost BEFORE CREDITS must stay within the monthly
// cap, and the bundle price alone must stay within the per-instance
// ceiling. Before credits is the load-bearing part — credit-covered usage
// still counts against the cap, so a credit balance cannot quietly fund a
// fleet.
//
// Month-to-date spend comes from the account's own AWS BUDGET first and
// Cost Explorer second — budgets.go holds that ladder and the reason for
// it (Cost Explorer cannot answer at all on an account whose cost data has
// never been ingested, which is exactly the state a new account is in).
// This file keeps the POLICY: the caps, the arithmetic, and the refusal.
//
// The cap itself can also come from the budget: when no --budget-cap is
// given, the budget's BudgetLimit IS the operator's cap, read from where
// the operator set it so the two cannot drift.
//
// It FAILS CLOSED. When NEITHER source can be read, spend is unknown, and
// unknown spend refuses the create: a budget guard that passes when it
// cannot see the bill is not a guard. An operator who accepts that risk
// says so explicitly with Budget.AllowUnknownSpend.
package aws

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/qiangli/yoke/pkg/provider"
)

// ServiceCostExplorer is the SigV4 service name for Cost Explorer.
const ServiceCostExplorer = "ce"

// CostExplorerRegion is Cost Explorer's single endpoint region. It is NOT
// the instance's region.
const CostExplorerRegion = "us-east-1"

// CostExplorerEndpoint is the Cost Explorer endpoint.
const CostExplorerEndpoint = "https://ce." + CostExplorerRegion + ".amazonaws.com/"

// costExplorerTargetPrefix is Cost Explorer's X-Amz-Target prefix: the
// service's internal coral name, not its public one.
const costExplorerTargetPrefix = "AWSInsightsIndexService."

// Budget caps. These are the plan's live numbers: a $50/month account cap
// and a $24/month per-instance ceiling.
const (
	DefaultMonthlyCapUSD  = 50.0
	DefaultPerInstanceUSD = 24.0
)

// Budget is the spend policy for one adapter.
type Budget struct {
	// Name is the AWS Budget read for month-to-date spend and (when
	// MonthlyCapUSD is zero) for the cap itself. Empty means
	// DefaultBudgetName.
	Name string
	// MonthlyCapUSD caps month-to-date spend plus the new instance's
	// monthly price. Zero means DefaultMonthlyCapUSD.
	MonthlyCapUSD float64
	// PerInstanceUSD caps one bundle's monthly price. Zero means
	// DefaultPerInstanceUSD.
	PerInstanceUSD float64
	// Disabled skips the preflight entirely. It exists for the
	// fixture-only conformance run and for a caller that has its own
	// spend control; it is never the default.
	Disabled bool
	// AllowUnknownSpend permits a create when Cost Explorer could not be
	// read. Without it, unknown month-to-date spend refuses the create.
	AllowUnknownSpend bool
}

func (b Budget) cap() float64 {
	if b.MonthlyCapUSD > 0 {
		return b.MonthlyCapUSD
	}
	return DefaultMonthlyCapUSD
}

// Cap sources, as they appear in a verdict.
const (
	CapSourceConfig  = "config"
	CapSourceDefault = "default"
)

// capFrom resolves the monthly cap. An explicit MonthlyCapUSD (the
// operator's --budget-cap) always wins. Otherwise the AWS BUDGET's own
// BudgetLimit is the cap: the operator set the number there, and reading it
// from the account means the two cannot drift. The compiled-in default is
// the last resort, for an account with no budget at all.
func (b Budget) capFrom(budgetLimitUSD float64) (float64, string) {
	if b.MonthlyCapUSD > 0 {
		return b.MonthlyCapUSD, CapSourceConfig
	}
	if budgetLimitUSD > 0 {
		return budgetLimitUSD, SourceBudget
	}
	return DefaultMonthlyCapUSD, CapSourceDefault
}

func (b Budget) perInstance() float64 {
	if b.PerInstanceUSD > 0 {
		return b.PerInstanceUSD
	}
	return DefaultPerInstanceUSD
}

// Verdict is the preflight's answer. It is reported in the plan payload so
// `--dry-run` shows the operator exactly what the live create would decide.
type Verdict struct {
	Checked bool `json:"checked"`
	OK      bool `json:"ok"`
	// Reason is always set: why it passed, or why it refused.
	Reason string `json:"reason"`

	BundleID         string  `json:"bundle_id,omitempty"`
	BundleMonthlyUSD float64 `json:"bundle_monthly_usd"`
	// MonthToDateUSD is spend before credits; Known is false when NO
	// source could be read.
	MonthToDateUSD   float64 `json:"month_to_date_usd"`
	MonthToDateKnown bool    `json:"month_to_date_known"`
	// SpendSource is where MonthToDateUSD came from: SourceBudget or
	// SourceCostExplorer (budgets.go).
	SpendSource string `json:"spend_source,omitempty"`
	// BudgetName is the AWS Budget the preflight consulted.
	BudgetName string `json:"budget_name,omitempty"`
	// CreditsIncluded is true when the source counts credits toward
	// spend, i.e. the figure may understate the bill.
	CreditsIncluded     bool    `json:"credits_included,omitempty"`
	ProjectedMonthlyUSD float64 `json:"projected_monthly_usd"`
	MonthlyCapUSD       float64 `json:"monthly_cap_usd"`
	// CapSource is where MonthlyCapUSD came from: "config" (the caller's
	// --budget-cap), SourceBudget (the budget's own BudgetLimit) or
	// "default".
	CapSource         string  `json:"cap_source,omitempty"`
	PerInstanceCapUSD float64 `json:"per_instance_cap_usd"`
}

// ---- Cost Explorer wire types ----

type ceDateInterval struct {
	Start string `json:"Start"`
	End   string `json:"End"`
}

type ceDimensionValues struct {
	Key    string   `json:"Key"`
	Values []string `json:"Values"`
}

type ceExpression struct {
	Not        *ceExpression      `json:"Not,omitempty"`
	Dimensions *ceDimensionValues `json:"Dimensions,omitempty"`
}

type ceRequest struct {
	TimePeriod  ceDateInterval `json:"TimePeriod"`
	Granularity string         `json:"Granularity"`
	Metrics     []string       `json:"Metrics"`
	Filter      *ceExpression  `json:"Filter,omitempty"`
}

type ceMetricValue struct {
	Amount string `json:"Amount"`
	Unit   string `json:"Unit"`
}

type ceResponse struct {
	ResultsByTime []struct {
		TimePeriod ceDateInterval           `json:"TimePeriod"`
		Total      map[string]ceMetricValue `json:"Total"`
	} `json:"ResultsByTime"`
}

// monthToDateWindow is the Cost Explorer window for the current month. The
// End date is EXCLUSIVE, so it is tomorrow: an End of today would exclude
// today's usage, and on the 1st of a month would be an empty interval.
func monthToDateWindow(now time.Time) ceDateInterval {
	t := now.UTC()
	start := time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC).AddDate(0, 0, 1)
	return ceDateInterval{Start: start.Format("2006-01-02"), End: end.Format("2006-01-02")}
}

// excludeCredits is the filter that keeps credit records out of the total,
// so the figure is spend BEFORE credits.
func excludeCredits() *ceExpression {
	return &ceExpression{Not: &ceExpression{
		Dimensions: &ceDimensionValues{Key: "RECORD_TYPE", Values: []string{"Credit"}},
	}}
}

// MonthToDateUSD returns the account's month-to-date unblended cost before
// credits.
func (a *adapter) MonthToDateUSD(ctx context.Context) (float64, error) {
	in := ceRequest{
		TimePeriod:  monthToDateWindow(a.now()),
		Granularity: "MONTHLY",
		Metrics:     []string{"UnblendedCost"},
		Filter:      excludeCredits(),
	}
	var out ceResponse
	if err := a.ce.call(ctx, "GetCostAndUsage", in, &out); err != nil {
		return 0, err
	}
	var total float64
	if len(out.ResultsByTime) == 0 {
		return 0, fmt.Errorf("aws: cost explorer returned no spend")
	}
	for _, r := range out.ResultsByTime {
		m, ok := r.Total["UnblendedCost"]
		if !ok || strings.ToUpper(strings.TrimSpace(m.Unit)) != "USD" {
			return 0, fmt.Errorf("aws: cost explorer returned no USD amount")
		}
		v, err := strconv.ParseFloat(strings.TrimSpace(m.Amount), 64)
		if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
			return 0, fmt.Errorf("aws: cost explorer returned an invalid amount")
		}
		total += v
	}
	return total, nil
}

// BudgetPreflight decides whether bundleID may be created. It returns the
// verdict always, and a normalized provider error when the verdict refuses
// (so a caller may use either channel, as the rest of the contract does).
//
// Order matters: everything decidable WITHOUT asking AWS about money — the
// GPU shape, a missing price, the per-instance ceiling — is decided first,
// so an obviously-refused request costs no API calls. Only then is
// month-to-date spend read (budgets.go: the budget first, Cost Explorer
// second).
func (a *adapter) BudgetPreflight(ctx context.Context, b bundle) (Verdict, *provider.Error) {
	v := Verdict{
		BundleID:          b.BundleID,
		BundleMonthlyUSD:  b.Price,
		BudgetName:        a.cfg.Budget.budgetName(),
		MonthlyCapUSD:     a.cfg.Budget.cap(),
		PerInstanceCapUSD: a.cfg.Budget.perInstance(),
	}
	if a.cfg.Budget.Disabled {
		v.Reason = "budget preflight disabled by configuration"
		return v, nil
	}
	v.Checked = true
	refuse := func() (Verdict, *provider.Error) {
		return v, &provider.Error{Code: provider.ErrInvalidRequest,
			Message: "aws: budget preflight refused create: " + v.Reason}
	}

	// No GPU, ever (plan §Budget guard 4). Lightsail's GPU plans are the
	// "Lightsail for Research" bundles; they are refused by shape, before
	// any price arithmetic, so a cheap GPU bundle is still refused.
	if isGPUBundle(b) {
		v.Reason = fmt.Sprintf("bundle %s is a GPU plan; GPU instances are never provisioned by this adapter", b.BundleID)
		return refuse()
	}
	if b.Price <= 0 {
		v.Reason = fmt.Sprintf("bundle %s reports no price; refusing to create against an unknown cost", b.BundleID)
		return refuse()
	}
	if b.Price > v.PerInstanceCapUSD {
		v.Reason = fmt.Sprintf("bundle %s costs $%.2f/mo, over the $%.2f per-instance ceiling",
			b.BundleID, b.Price, v.PerInstanceCapUSD)
		return refuse()
	}

	s := a.MonthToDateSpend(ctx)
	// The cap can come from the budget itself, so it is resolved after
	// the lookup rather than before.
	v.MonthlyCapUSD, v.CapSource = a.cfg.Budget.capFrom(s.CapUSD)
	if !s.Known() {
		if !a.cfg.Budget.AllowUnknownSpend {
			v.Reason = fmt.Sprintf("month-to-date spend is unknown (%s); refusing to create against an unknown bill", s.why())
			return refuse()
		}
		v.ProjectedMonthlyUSD = b.Price
		v.OK = true
		v.Reason = fmt.Sprintf("month-to-date spend is unknown (%s) and allow_unknown_spend is set; bundle %s at $%.2f/mo is within the $%.2f per-instance ceiling",
			s.why(), b.BundleID, b.Price, v.PerInstanceCapUSD)
		return v, nil
	}
	v.MonthToDateUSD, v.MonthToDateKnown = s.USD, true
	v.SpendSource, v.CreditsIncluded = s.Source, s.CreditsIncluded
	v.ProjectedMonthlyUSD = s.USD + b.Price
	if v.ProjectedMonthlyUSD > v.MonthlyCapUSD {
		v.Reason = fmt.Sprintf("month-to-date spend $%.2f plus bundle %s at $%.2f/mo is $%.2f, over the $%.2f monthly cap%s",
			s.USD, b.BundleID, b.Price, v.ProjectedMonthlyUSD, v.MonthlyCapUSD, v.provenance())
		return refuse()
	}
	v.OK = true
	v.Reason = fmt.Sprintf("month-to-date spend $%.2f plus bundle %s at $%.2f/mo is $%.2f, within the $%.2f monthly cap and the $%.2f per-instance ceiling%s",
		s.USD, b.BundleID, b.Price, v.ProjectedMonthlyUSD, v.MonthlyCapUSD, v.PerInstanceCapUSD, v.provenance())
	return v, nil
}

// provenance is the trailing clause that says where the numbers came from,
// so a dry-run (and an incident review of a refusal) can tell a figure
// read from the account's own budget from one computed by Cost Explorer —
// and can see when the spend figure counts credits, which makes it a
// LOWER bound on the real bill.
func (v Verdict) provenance() string {
	var b strings.Builder
	switch v.SpendSource {
	case SourceBudget:
		fmt.Fprintf(&b, " (spend from the %s budget", v.BudgetName)
	case SourceCostExplorer:
		b.WriteString(" (spend from cost explorer")
	default:
		return ""
	}
	switch {
	case v.CapSource == SourceBudget && v.SpendSource == SourceBudget:
		// "its" is unambiguous here: the same budget supplied both.
		fmt.Fprintf(&b, ", cap from its $%.2f limit", v.MonthlyCapUSD)
	case v.CapSource == SourceBudget:
		fmt.Fprintf(&b, ", cap from the %s budget's $%.2f limit", v.BudgetName, v.MonthlyCapUSD)
	case v.CapSource == CapSourceConfig:
		b.WriteString(", cap from --budget-cap")
	}
	if v.CreditsIncluded {
		b.WriteString("; that budget counts credits toward its spend, so the figure may understate the bill")
	}
	b.WriteString(")")
	return b.String()
}

// isGPUBundle reports whether a bundle is a GPU plan. Lightsail does not
// expose an accelerator field, so the shape of the id/type/name is all
// there is; every one of them is checked rather than just the id.
func isGPUBundle(b bundle) bool {
	for _, s := range []string{b.BundleID, b.InstanceType, b.Name} {
		if strings.Contains(strings.ToLower(s), "gpu") {
			return true
		}
	}
	return false
}
