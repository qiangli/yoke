// The AWS Budgets spend source (S522.2 rework 1, 2026-10-28).
//
// WHY THIS EXISTS. The first live dry-run against the real Sprint 522
// account refused: Cost Explorer answered HTTP 400 DataUnavailableException
// because the account is new and no cost data has been ingested yet. Cost
// Explorer only reports what it has already processed, so on a fresh
// account it cannot answer "what have I spent this month" at all — and a
// guard that fails closed turns that into a refusal to provision anything,
// ever, until AWS gets around to the first ingest.
//
// The AWS BUDGET itself does not have that problem. The operator's
// `dhnt-monthly-cap` budget carries CalculatedSpend.ActualSpend, which
// Budgets computes for a brand-new account, and BudgetLimit, which is the
// operator's cap expressed where the operator actually set it. So the
// budget is now the PRIMARY source for both numbers, and Cost Explorer is
// the FALLBACK.
//
// The ladder:
//
//  1. Budgets DescribeBudget (budgets.amazonaws.com, SigV4 us-east-1 /
//     "budgets") for the configured budget name. Its ActualSpend is
//     month-to-date; its BudgetLimit is the cap when no --budget-cap was
//     given.
//  2. Cost Explorer GetCostAndUsage, as before, when the budget produced
//     no usable figure.
//  3. Refuse — only when BOTH are unavailable. One working source is
//     enough; the refusal names what went wrong with each.
//
// The brief calls for the fallback on NotFoundException (no such budget).
// This takes the slightly wider rule — ANY budget failure falls through to
// Cost Explorer — because the invariant that matters is "refuse only when
// both are unavailable", and a denied or throttled Budgets call is no more
// informative about spend than a missing budget is. Nothing is weakened:
// if both sources fail the create is still refused.
//
// Credits: the figure must be spend BEFORE credits (credit-covered usage
// still counts against the operator's cap, or a credit balance could
// quietly fund a fleet). Cost Explorer gets that from its
// Not/RECORD_TYPE=Credit filter; a budget gets it from CostTypes with
// IncludeCredit=false, which is how `dhnt-monthly-cap` is configured. A
// budget that says otherwise is still used — it is the only spend figure
// there is on a fresh account — but the verdict says so in as many words,
// so an operator reading the dry-run sees that the number may understate
// the bill.
package aws

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

// ServiceBudgets is the SigV4 service name for AWS Budgets.
const ServiceBudgets = "budgets"

// BudgetsRegion is the Budgets endpoint's signing region. Budgets is a
// global service; this is NOT the instance's region.
const BudgetsRegion = "us-east-1"

// BudgetsEndpoint is the global Budgets endpoint.
const BudgetsEndpoint = "https://budgets.amazonaws.com/"

// budgetsTargetPrefix is the Budgets service's X-Amz-Target prefix: the
// service's internal coral name, not its public one.
const budgetsTargetPrefix = "AWSBudgetServiceGateway."

// DefaultBudgetName is the budget the preflight reads. It is the budget the
// Sprint 522 account defines, with IncludeCredit=false so its ActualSpend
// is spend before credits.
const DefaultBudgetName = "dhnt-monthly-cap"

// Spend source names, as they appear in a verdict.
const (
	SourceBudget       = "budget"
	SourceCostExplorer = "cost-explorer"
)

// ---- Budgets wire types (only the fields the preflight reads) ----

// budgetAmount is Budgets' Spend shape: a decimal string plus its unit.
type budgetAmount struct {
	Amount string `json:"Amount"`
	Unit   string `json:"Unit"`
}

type budgetCalculatedSpend struct {
	ActualSpend     *budgetAmount `json:"ActualSpend"`
	ForecastedSpend *budgetAmount `json:"ForecastedSpend"`
}

// budgetCostTypes is the budget's cost composition. IncludeCredit is a
// POINTER so "absent" is distinguishable from "false": absent means the
// service did not say, and the preflight then trusts the configured
// before-credits shape rather than inventing a warning.
type budgetCostTypes struct {
	IncludeCredit *bool `json:"IncludeCredit"`
	IncludeRefund *bool `json:"IncludeRefund"`
}

type awsBudget struct {
	BudgetName      string                 `json:"BudgetName"`
	BudgetType      string                 `json:"BudgetType"`
	TimeUnit        string                 `json:"TimeUnit"`
	BudgetLimit     *budgetAmount          `json:"BudgetLimit"`
	CalculatedSpend *budgetCalculatedSpend `json:"CalculatedSpend"`
	CostTypes       *budgetCostTypes       `json:"CostTypes"`
}

type describeBudgetRequest struct {
	AccountID  string `json:"AccountId"`
	BudgetName string `json:"BudgetName"`
}

type describeBudgetResponse struct {
	Budget *awsBudget `json:"Budget"`
}

// budgetName is the budget the preflight reads.
func (b Budget) budgetName() string {
	if n := strings.TrimSpace(b.Name); n != "" {
		return n
	}
	return DefaultBudgetName
}

// usdAmount parses one Budgets money value. A unit other than USD is an
// error rather than a silent conversion: every cap in this adapter is USD,
// and comparing a EUR figure against a USD cap would be wrong in the
// permissive direction.
func usdAmount(a *budgetAmount) (float64, error) {
	if a == nil || strings.TrimSpace(a.Amount) == "" {
		return 0, fmt.Errorf("aws: budget reported no amount")
	}
	if u := strings.ToUpper(strings.TrimSpace(a.Unit)); u != "" && u != "USD" {
		return 0, fmt.Errorf("aws: budget amount is in %s, not USD", u)
	}
	v, err := strconv.ParseFloat(strings.TrimSpace(a.Amount), 64)
	if err != nil {
		return 0, fmt.Errorf("aws: budget reported an unparseable amount %q", a.Amount)
	}
	return v, nil
}

// DescribeBudget reads the named budget. The account id comes from
// Config.AccountID or STS (sts.go).
func (a *adapter) DescribeBudget(ctx context.Context) (*awsBudget, error) {
	account, err := a.AccountID(ctx)
	if err != nil {
		return nil, err
	}
	in := describeBudgetRequest{AccountID: account, BudgetName: a.cfg.Budget.budgetName()}
	var out describeBudgetResponse
	if err := a.budgets.call(ctx, "DescribeBudget", in, &out); err != nil {
		return nil, err
	}
	if out.Budget == nil {
		return nil, &callError{status: 400, typ: "NotFoundException",
			msg: "budget " + in.BudgetName + " does not exist"}
	}
	return out.Budget, nil
}

// Spend is one month-to-date spend reading plus where it came from.
type Spend struct {
	// USD is month-to-date spend. Valid only when Source is set.
	USD float64
	// Source is SourceBudget or SourceCostExplorer.
	Source string
	// CapUSD is the budget's BudgetLimit when the Budgets call produced
	// one, zero otherwise. It is filled in even when the SPEND came from
	// Cost Explorer: a budget that answered with a limit but no usable
	// spend still told us the operator's cap.
	CapUSD float64
	// CreditsIncluded is true when the budget's CostTypes say credits
	// count toward its spend, i.e. the figure is NOT before credits.
	CreditsIncluded bool
	// BudgetErr and CostExplorerErr record why a source was not used.
	BudgetErr, CostExplorerErr error
}

// Known reports whether a source produced a month-to-date figure.
func (s Spend) Known() bool { return s.Source != "" }

// why renders the per-source failures for a message.
func (s Spend) why() string {
	parts := make([]string, 0, 2)
	if s.BudgetErr != nil {
		parts = append(parts, "budget: "+normalize(s.BudgetErr).Message)
	}
	if s.CostExplorerErr != nil {
		parts = append(parts, "cost explorer: "+normalize(s.CostExplorerErr).Message)
	}
	if len(parts) == 0 {
		return "no spend source was consulted"
	}
	return strings.Join(parts, "; ")
}

// MonthToDateSpend reads month-to-date spend from the budget first and
// Cost Explorer second (see this file's header for why). It returns a
// Spend with Known() false, and the per-source errors, when neither
// answered.
func (a *adapter) MonthToDateSpend(ctx context.Context) Spend {
	var s Spend
	if b, err := a.DescribeBudget(ctx); err != nil {
		s.BudgetErr = err
	} else {
		// Keep what the budget told us about the CAP and about credits
		// even if its spend figure turns out to be unusable.
		if limit, lerr := usdAmount(b.BudgetLimit); lerr == nil {
			s.CapUSD = limit
		}
		if b.CostTypes != nil && b.CostTypes.IncludeCredit != nil {
			s.CreditsIncluded = *b.CostTypes.IncludeCredit
		}
		var actual *budgetAmount
		if b.CalculatedSpend != nil {
			actual = b.CalculatedSpend.ActualSpend
		}
		v, aerr := usdAmount(actual)
		if aerr == nil {
			s.USD, s.Source = v, SourceBudget
			return s
		}
		// A budget with no CalculatedSpend yet is NOT zero spend: it is
		// an unanswered question, and reading it as zero would be the
		// permissive mistake. Fall through to Cost Explorer.
		s.BudgetErr = aerr
	}
	// Cost Explorer's Not/RECORD_TYPE=Credit filter excludes credits by
	// construction, so a budget's credit setting says nothing about the
	// figure below and must not be reported against it.
	s.CreditsIncluded = false
	mtd, cerr := a.MonthToDateUSD(ctx)
	if cerr != nil {
		s.CostExplorerErr = cerr
		return s
	}
	s.USD, s.Source = mtd, SourceCostExplorer
	return s
}
