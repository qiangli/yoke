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
// Month-to-date spend comes from Cost Explorer GetCostAndUsage at
// ce.us-east-1.amazonaws.com (Cost Explorer is a global service with a
// us-east-1 endpoint and signing region, regardless of where the instance
// lands), with a Not/RECORD_TYPE=Credit filter so credits are excluded
// from the total rather than netted against it.
//
// It FAILS CLOSED. A Cost Explorer call that is denied or unreachable
// leaves spend unknown, and unknown spend refuses the create: a budget
// guard that passes when it cannot see the bill is not a guard. An
// operator who accepts that risk says so explicitly with
// Budget.AllowUnknownSpend.
package aws

import (
	"context"
	"fmt"
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
	// MonthToDateUSD is unblended cost before credits; Known is false
	// when Cost Explorer could not be read.
	MonthToDateUSD      float64 `json:"month_to_date_usd"`
	MonthToDateKnown    bool    `json:"month_to_date_known"`
	ProjectedMonthlyUSD float64 `json:"projected_monthly_usd"`
	MonthlyCapUSD       float64 `json:"monthly_cap_usd"`
	PerInstanceCapUSD   float64 `json:"per_instance_cap_usd"`
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
	for _, r := range out.ResultsByTime {
		m, ok := r.Total["UnblendedCost"]
		if !ok {
			continue
		}
		v, err := strconv.ParseFloat(strings.TrimSpace(m.Amount), 64)
		if err != nil {
			return 0, fmt.Errorf("aws: cost explorer returned an unparseable amount %q", m.Amount)
		}
		total += v
	}
	return total, nil
}

// BudgetPreflight decides whether bundleID may be created. It returns the
// verdict always, and a normalized provider error when the verdict refuses
// (so a caller may use either channel, as the rest of the contract does).
func (a *adapter) BudgetPreflight(ctx context.Context, b bundle) (Verdict, *provider.Error) {
	v := Verdict{
		BundleID:          b.BundleID,
		BundleMonthlyUSD:  b.Price,
		MonthlyCapUSD:     a.cfg.Budget.cap(),
		PerInstanceCapUSD: a.cfg.Budget.perInstance(),
	}
	if a.cfg.Budget.Disabled {
		v.Reason = "budget preflight disabled by configuration"
		return v, nil
	}
	v.Checked = true

	// No GPU, ever (plan §Budget guard 4). Lightsail's GPU plans are the
	// "Lightsail for Research" bundles; they are refused by shape, before
	// any price arithmetic, so a cheap GPU bundle is still refused.
	if isGPUBundle(b) {
		v.Reason = fmt.Sprintf("bundle %s is a GPU plan; GPU instances are never provisioned by this adapter", b.BundleID)
		return v, &provider.Error{Code: provider.ErrInvalidRequest, Message: "aws: budget preflight refused create: " + v.Reason}
	}
	if b.Price <= 0 {
		v.Reason = fmt.Sprintf("bundle %s reports no price; refusing to create against an unknown cost", b.BundleID)
		return v, &provider.Error{Code: provider.ErrInvalidRequest, Message: "aws: budget preflight refused create: " + v.Reason}
	}
	if b.Price > v.PerInstanceCapUSD {
		v.Reason = fmt.Sprintf("bundle %s costs $%.2f/mo, over the $%.2f per-instance ceiling",
			b.BundleID, b.Price, v.PerInstanceCapUSD)
		return v, &provider.Error{Code: provider.ErrInvalidRequest, Message: "aws: budget preflight refused create: " + v.Reason}
	}

	mtd, err := a.MonthToDateUSD(ctx)
	if err != nil {
		if !a.cfg.Budget.AllowUnknownSpend {
			v.Reason = fmt.Sprintf("month-to-date spend is unknown (cost explorer: %v); refusing to create against an unknown bill", normalize(err))
			return v, &provider.Error{Code: provider.ErrInvalidRequest, Message: "aws: budget preflight refused create: " + v.Reason}
		}
		v.ProjectedMonthlyUSD = b.Price
		v.OK = true
		v.Reason = fmt.Sprintf("month-to-date spend unknown (cost explorer: %v) and allow_unknown_spend is set; bundle %s at $%.2f/mo is within the $%.2f per-instance ceiling",
			normalize(err), b.BundleID, b.Price, v.PerInstanceCapUSD)
		return v, nil
	}
	v.MonthToDateUSD, v.MonthToDateKnown = mtd, true
	v.ProjectedMonthlyUSD = mtd + b.Price
	if v.ProjectedMonthlyUSD > v.MonthlyCapUSD {
		v.Reason = fmt.Sprintf("month-to-date spend $%.2f plus bundle %s at $%.2f/mo is $%.2f, over the $%.2f monthly cap",
			mtd, b.BundleID, b.Price, v.ProjectedMonthlyUSD, v.MonthlyCapUSD)
		return v, &provider.Error{Code: provider.ErrInvalidRequest, Message: "aws: budget preflight refused create: " + v.Reason}
	}
	v.OK = true
	v.Reason = fmt.Sprintf("month-to-date spend $%.2f plus bundle %s at $%.2f/mo is $%.2f, within the $%.2f monthly cap and the $%.2f per-instance ceiling",
		mtd, b.BundleID, b.Price, v.ProjectedMonthlyUSD, v.MonthlyCapUSD, v.PerInstanceCapUSD)
	return v, nil
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
