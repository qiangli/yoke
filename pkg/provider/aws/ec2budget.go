// The GPU money preflights (S546.2): the hourly session model the
// Lightsail monthly preflight cannot express.
//
// The Lightsail preflight compares a MONTHLY bundle price to a $24/mo
// per-instance ceiling, which every GPU type fails. A GPU worker is
// priced by the HOUR and lives for a SESSION, so the preflight prices
// (rate x session hours) plus the gp3 month and checks that against
// two budgets and one quota:
//
//  1. dhnt-monthly-cap: month-to-date spend before credits (the
//     account's own budget first, Cost Explorer second — the same
//     ladder as budgets.go) plus the session cost plus the EBS month
//     must stay within the monthly cap ($400 default).
//  2. dhnt-gpu-daily: today's spend plus the session cost must stay
//     within the daily budget's limit. EBS is monthly-billed and is
//     not in the daily figure.
//  3. The Running On-Demand G quota (L-DB2E81BA): the type's vCPUs
//     must fit the quota value. The quota was 0 on 2026-10-30 with an
//     8-vCPU increase pending, so a worker past it is refused here
//     rather than at RunInstances time.
//
// All three FAIL CLOSED, in that order. When the monthly spend is
// unknown the create is refused unless Budget.AllowUnknownSpend says
// otherwise; the daily and quota reads have no second source, so an
// unreadable one refuses the same way. Budget.Disabled skips all
// three (the fixture-only conformance run).
package aws

import (
	"context"
	"fmt"
	"strings"

	"github.com/qiangli/yoke/pkg/provider"
)

// GPUPreflight is the GPU preflight's answer. It is reported in the
// plan payload so --dry-run shows the operator exactly what the live
// create would decide, including the hourly price.
type GPUPreflight struct {
	Checked bool `json:"checked"`
	OK      bool `json:"ok"`
	// Reason is always set: why it passed, or why it refused.
	Reason string `json:"reason"`

	InstanceType   string  `json:"instance_type"`
	HourlyUSD      float64 `json:"hourly_usd"`
	SessionHours   float64 `json:"session_hours"`
	SessionCostUSD float64 `json:"session_cost_usd"`
	VolumeGB       int     `json:"volume_gb"`
	ThroughputMBs  int     `json:"throughput_mbs"`
	EBSMonthlyUSD  float64 `json:"ebs_monthly_usd"`

	// Monthly: the dhnt-monthly-cap check. Fields mirror Verdict's
	// so a reader of either preflight sees the same shape.
	MonthToDateUSD      float64 `json:"month_to_date_usd"`
	MonthToDateKnown    bool    `json:"month_to_date_known"`
	SpendSource         string  `json:"spend_source,omitempty"`
	BudgetName          string  `json:"budget_name,omitempty"`
	CreditsIncluded     bool    `json:"credits_included,omitempty"`
	ProjectedMonthlyUSD float64 `json:"projected_monthly_usd"`
	MonthlyCapUSD       float64 `json:"monthly_cap_usd"`
	CapSource           string  `json:"cap_source,omitempty"`

	// Daily: the dhnt-gpu-daily check.
	DailyBudget   string  `json:"daily_budget,omitempty"`
	DailySpendUSD float64 `json:"daily_spend_usd"`
	DailyKnown    bool    `json:"daily_known"`
	DailyLimitUSD float64 `json:"daily_limit_usd"`

	// Quota: the Service Quotas check.
	QuotaCode     string  `json:"quota_code,omitempty"`
	QuotaValue    float64 `json:"quota_value"`
	QuotaNeedVCPU int     `json:"quota_need_vcpu"`
}

// gpuPreflight decides whether a worker of this type may launch. It
// returns the verdict always, and a normalized provider error when
// the verdict refuses.
func (e *ec2Adapter) gpuPreflight(ctx context.Context, t gpuType, typeName string) (GPUPreflight, *provider.Error) {
	v := GPUPreflight{
		InstanceType:   typeName,
		HourlyUSD:      t.hourlyUSD,
		SessionHours:   e.gpu.hours,
		SessionCostUSD: t.hourlyUSD * e.gpu.hours,
		VolumeGB:       e.gpu.volumeGB,
		ThroughputMBs:  e.gpu.throughput,
		EBSMonthlyUSD:  ebsMonthly(e.gpu.volumeGB, e.gpu.throughput),
		BudgetName:     e.cfg.Budget.budgetName(),
		DailyBudget:    e.gpu.dailyBudget,
		QuotaCode:      e.gpu.quotaCode,
		QuotaNeedVCPU:  t.vcpu,
	}
	if e.cfg.Budget.Disabled {
		v.Reason = "gpu preflights disabled by configuration"
		return v, nil
	}
	v.Checked = true
	refuse := func() (GPUPreflight, *provider.Error) {
		return v, &provider.Error{Code: provider.ErrInvalidRequest,
			Message: "aws-ec2-gpu: preflight refused create: " + v.Reason}
	}

	// 1. Monthly: session compute plus the EBS month on top of what
	// the account already spent. The cap is the caller's --budget-cap
	// first, the dhnt-monthly-cap budget's own limit second, $400
	// last: the Lightsail $50 default is the wrong number for GPU.
	s := e.MonthToDateSpend(ctx)
	v.MonthlyCapUSD = e.cfg.Budget.MonthlyCapUSD
	v.CapSource = CapSourceConfig
	if v.MonthlyCapUSD <= 0 {
		if s.CapUSD > 0 {
			v.MonthlyCapUSD, v.CapSource = s.CapUSD, SourceBudget
		} else {
			v.MonthlyCapUSD, v.CapSource = DefaultGPUMonthlyCapUSD, CapSourceDefault
		}
	}
	if !s.Known() {
		if !e.cfg.Budget.AllowUnknownSpend {
			v.Reason = fmt.Sprintf("month-to-date spend is unknown (%s); refusing to launch against an unknown bill", s.why())
			return refuse()
		}
		v.ProjectedMonthlyUSD = v.SessionCostUSD + v.EBSMonthlyUSD
		v.Reason = fmt.Sprintf("month-to-date spend is unknown (%s) and allow_unknown_spend is set", s.why())
	} else {
		v.MonthToDateUSD, v.MonthToDateKnown = s.USD, true
		v.SpendSource, v.CreditsIncluded = s.Source, s.CreditsIncluded
		v.ProjectedMonthlyUSD = s.USD + v.SessionCostUSD + v.EBSMonthlyUSD
		if v.ProjectedMonthlyUSD > v.MonthlyCapUSD {
			v.Reason = fmt.Sprintf("month-to-date spend $%.2f plus a %.1fh %s session at $%.3f/h ($%.2f) and $%.2f EBS is $%.2f, over the $%.2f monthly cap",
				s.USD, v.SessionHours, typeName, v.HourlyUSD, v.SessionCostUSD, v.EBSMonthlyUSD, v.ProjectedMonthlyUSD, v.MonthlyCapUSD)
			return refuse()
		}
		if v.CreditsIncluded {
			v.Reason = fmt.Sprintf("month-to-date spend $%.2f (from the %s budget, which counts credits, so the figure may understate the bill) plus $%.2f session and $%.2f EBS is $%.2f, within the $%.2f monthly cap",
				s.USD, v.BudgetName, v.SessionCostUSD, v.EBSMonthlyUSD, v.ProjectedMonthlyUSD, v.MonthlyCapUSD)
		} else {
			v.Reason = fmt.Sprintf("month-to-date spend $%.2f plus $%.2f session and $%.2f EBS is $%.2f, within the $%.2f monthly cap",
				s.USD, v.SessionCostUSD, v.EBSMonthlyUSD, v.ProjectedMonthlyUSD, v.MonthlyCapUSD)
		}
	}

	// 2. Daily: today's spend plus this session against the daily
	// budget's limit.
	dSpend, dLimit, derr := e.describeDailyBudget(ctx, e.gpu.dailyBudget)
	if derr != nil {
		if !e.cfg.Budget.AllowUnknownSpend {
			v.Reason = fmt.Sprintf("daily spend is unknown (budget %s: %s); refusing to launch against an unknown bill",
				e.gpu.dailyBudget, normalize(derr).Message)
			return refuse()
		}
		v.Reason += "; daily spend is unknown and allow_unknown_spend is set"
	} else {
		v.DailySpendUSD, v.DailyKnown = dSpend, true
		v.DailyLimitUSD = dLimit
		if dSpend+v.SessionCostUSD > dLimit {
			v.Reason = fmt.Sprintf("today's spend $%.2f plus a %.1fh %s session ($%.2f) is $%.2f, over the %s daily limit of $%.2f",
				dSpend, v.SessionHours, typeName, v.SessionCostUSD, dSpend+v.SessionCostUSD, e.gpu.dailyBudget, dLimit)
			return refuse()
		}
	}

	// 3. Quota: the type's vCPUs against the On-Demand G quota.
	q, qerr := e.getServiceQuota(ctx, e.gpu.quotaCode)
	if qerr != nil {
		v.Reason = fmt.Sprintf("quota %s is unreadable (%s); refusing to launch against unknown headroom",
			e.gpu.quotaCode, normalize(qerr).Message)
		return refuse()
	}
	v.QuotaValue = q
	if q < float64(t.vcpu) {
		v.Reason = fmt.Sprintf("quota %s allows %.0f vCPUs and a %s worker needs %d; request an increase before launching",
			e.gpu.quotaCode, q, typeName, t.vcpu)
		return refuse()
	}

	v.OK = true
	if v.Reason == "" {
		v.Reason = fmt.Sprintf("%.1fh %s session at $%.3f/h ($%.2f) plus $%.2f EBS fits the $%.2f monthly cap and the %s daily limit of $%.2f, and quota %s allows %.0f vCPUs",
			v.SessionHours, typeName, v.HourlyUSD, v.SessionCostUSD, v.EBSMonthlyUSD,
			v.MonthlyCapUSD, e.gpu.dailyBudget, v.DailyLimitUSD, e.gpu.quotaCode, q)
	} else {
		v.Reason += fmt.Sprintf("; quota %s allows %.0f vCPUs, the worker needs %d", e.gpu.quotaCode, q, t.vcpu)
	}
	return v, nil
}

// describeDailyBudget reads the DAILY budget's today-to-date spend and
// limit. Unlike DescribeBudget (which requires a MONTHLY cost budget),
// this requires a DAILY one: pointing the daily preflight at a
// monthly budget would silently compare a month of spend against a
// day's limit, so the mismatch fails loudly instead.
func (e *ec2Adapter) describeDailyBudget(ctx context.Context, name string) (spend, limit float64, err error) {
	account, err := e.AccountID(ctx)
	if err != nil {
		return 0, 0, err
	}
	in := describeBudgetRequest{AccountID: account, BudgetName: name}
	var out describeBudgetResponse
	if err := e.budgets.call(ctx, "DescribeBudget", in, &out); err != nil {
		return 0, 0, err
	}
	b := out.Budget
	if b == nil {
		return 0, 0, &callError{status: 400, typ: "NotFoundException", msg: "budget " + name + " does not exist"}
	}
	if b.BudgetType != "COST" || b.TimeUnit != "DAILY" {
		return 0, 0, fmt.Errorf("aws-ec2-gpu: budget %s is a %s %s budget, not a DAILY cost budget",
			name, b.TimeUnit, b.BudgetType)
	}
	limit, lerr := usdAmount(b.BudgetLimit)
	if lerr != nil {
		return 0, 0, fmt.Errorf("aws-ec2-gpu: daily budget %s: %w", name, lerr)
	}
	var actual *budgetAmount
	if b.CalculatedSpend != nil {
		actual = b.CalculatedSpend.ActualSpend
	}
	spend, serr := usdAmount(actual)
	if serr != nil {
		// A daily budget AWS has not evaluated yet is an unanswered
		// question, not zero spend — same rule as the monthly path.
		return 0, 0, fmt.Errorf("aws-ec2-gpu: daily budget %s: %w", name, serr)
	}
	return spend, limit, nil
}

// ---- Service Quotas wire types (only the fields the preflight reads) ----

type serviceQuota struct {
	QuotaCode string  `json:"QuotaCode"`
	QuotaName string  `json:"QuotaName"`
	Value     float64 `json:"Value"`
	Unit      string  `json:"Unit"`
}

type getServiceQuotaResponse struct {
	Quota *serviceQuota `json:"Quota"`
}

// getServiceQuota reads one quota value for the EC2 service. The
// Service Quotas API is JSON 1.1 like Budgets, but REGIONAL: the
// quota is drawn in the worker's region, so the endpoint and the
// signing scope are the worker's region too.
func (e *ec2Adapter) getServiceQuota(ctx context.Context, quotaCode string) (float64, error) {
	code := strings.TrimSpace(quotaCode)
	if code == "" {
		return 0, &provider.Error{Code: provider.ErrInvalidRequest, Message: "aws-ec2-gpu: no quota code to check"}
	}
	var out getServiceQuotaResponse
	if err := e.squota.call(ctx, "GetServiceQuota",
		map[string]any{"ServiceCode": "ec2", "QuotaCode": code}, &out); err != nil {
		return 0, err
	}
	if out.Quota == nil || !strings.EqualFold(strings.TrimSpace(out.Quota.QuotaCode), code) {
		return 0, fmt.Errorf("aws-ec2-gpu: GetServiceQuota answered without quota %s", code)
	}
	if out.Quota.Value < 0 {
		return 0, fmt.Errorf("aws-ec2-gpu: quota %s reports a negative value", code)
	}
	return out.Quota.Value, nil
}
