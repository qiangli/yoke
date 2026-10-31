package aws_test

// The spend-source ladder (S522.2 rework 1, 2026-10-28).
//
// The live dry-run against the real account refused to provision anything:
// Cost Explorer answered HTTP 400 DataUnavailableException because the
// account is brand new and no cost data had been ingested. These tests pin
// the fix — the account's own AWS Budget is the PRIMARY source for both
// month-to-date spend and the cap, Cost Explorer is the fallback, and only
// losing BOTH refuses a create.

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/provider"
	"github.com/qiangli/yoke/pkg/provider/aws"
	"github.com/qiangli/yoke/pkg/provider/aws/fakelightsail"
)

// monthlyCap is the budget the real account carries: a $50 limit with
// IncludeCredit=false, so ActualSpend is spend BEFORE credits.
func monthlyCap(spend float64) fakelightsail.Budget {
	return fakelightsail.Budget{
		Name: aws.DefaultBudgetName, LimitUSD: 50, ActualSpendUSD: spend,
	}
}

// verdict runs a plan (which allocates nothing) and returns its budget
// verdict.
func verdict(t *testing.T, a provider.Adapter, name string) aws.Verdict {
	t.Helper()
	res, err := do(t, a, provider.Request{Op: provider.OpPlan, IdempotencyKey: "plan-" + name, Spec: spec(name)})
	if err != nil {
		t.Fatalf("plan: %v (%s)", err, res.Error)
	}
	var p aws.Plan
	if err := json.Unmarshal(res.Data, &p); err != nil {
		t.Fatal(err)
	}
	return p.Budget
}

// The budget is the PRIMARY source: when it answers, Cost Explorer is not
// called at all. That is the whole point — on a new account Cost Explorer
// cannot answer, and the preflight must not depend on it.
func TestBudgetIsThePrimarySpendSource(t *testing.T) {
	fake := fakelightsail.New(t)
	fake.SetBudget(monthlyCap(7.25))
	a := open(t, fake, nil)

	v := verdict(t, a, "budget-primary")
	if !v.Checked || !v.OK {
		t.Fatalf("verdict = %+v, want a passing checked verdict", v)
	}
	if v.SpendSource != aws.SourceBudget {
		t.Fatalf("spend_source = %q, want %q", v.SpendSource, aws.SourceBudget)
	}
	if !v.MonthToDateKnown || v.MonthToDateUSD != 7.25 {
		t.Fatalf("month to date = %v (known %t), want 7.25 from CalculatedSpend.ActualSpend",
			v.MonthToDateUSD, v.MonthToDateKnown)
	}
	if v.ProjectedMonthlyUSD != 12.25 { // 7.25 + the $5 bundle
		t.Fatalf("projected = %v, want 12.25", v.ProjectedMonthlyUSD)
	}
	if v.BudgetName != aws.DefaultBudgetName {
		t.Fatalf("budget_name = %q, want %q", v.BudgetName, aws.DefaultBudgetName)
	}
	if n := fake.Calls("GetCostAndUsage"); n != 0 {
		t.Fatalf("cost explorer was called %d time(s); the budget answered, so it must not be", n)
	}
	if !strings.Contains(v.Reason, "spend from the "+aws.DefaultBudgetName+" budget") {
		t.Fatalf("reason %q does not say where the figure came from", v.Reason)
	}
	mustEnd(t, fake)
}

// Budgets and STS are GLOBAL services, signed for us-east-1 — not for the
// instance's region. Getting that wrong would only show up as a live
// SignatureDoesNotMatch.
func TestBudgetsAndSTSAreSignedForUSEast1(t *testing.T) {
	fake := fakelightsail.New(t)
	fake.SetBudget(monthlyCap(1))
	a := open(t, fake, nil)
	verdict(t, a, "scopes")
	for op, want := range map[string]string{
		"DescribeBudget":    "budgets/us-east-1",
		"GetCallerIdentity": "sts/us-east-1",
	} {
		if got := fake.Scope(op); got != want {
			t.Fatalf("%s signed for %q, want %q", op, got, want)
		}
	}
	mustEnd(t, fake)
}

// The live failure, reproduced: Cost Explorer has no data for this account
// yet, the budget does. The create PROCEEDS.
func TestCostExplorerDataUnavailableWithABudgetProceeds(t *testing.T) {
	fake := fakelightsail.New(t)
	fake.SetBudget(monthlyCap(3))
	// Armed generously: the point is that nothing in the create path can
	// get a usable answer out of Cost Explorer.
	fake.Inject("GetCostAndUsage", fakelightsail.FaultDataUnavailable, 100)
	a := open(t, fake, nil)

	if _, err := do(t, a, provider.Request{
		Op: provider.OpCreate, IdempotencyKey: "ce-blind", Spec: spec("ce-blind"),
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if fake.CreateCount() != 1 {
		t.Fatalf("CreateCount = %d, want 1", fake.CreateCount())
	}
	mustEnd(t, fake)
}

// With both sources unavailable, create now reports the missing prerequisite
// before consulting spend sources, including the one-time setup fix.
func TestMissingBudgetRefusesBeforeSpendLookup(t *testing.T) {
	fake := fakelightsail.New(t)
	fake.ClearBudget() // no budget on this account
	fake.Inject("GetCostAndUsage", fakelightsail.FaultDataUnavailable, 100)
	a := open(t, fake, nil)

	res, err := do(t, a, provider.Request{
		Op: provider.OpCreate, IdempotencyKey: "blind-both", Spec: spec("blind-both"),
	})
	if codeOf(err) != provider.ErrInvalidRequest {
		t.Fatalf("err = %v, want %s", err, provider.ErrInvalidRequest)
	}
	for _, want := range []string{
		"account prerequisites refused create", "budget dhnt-monthly-cap is missing",
		"aws-budget-guard.sh",
	} {
		if res.Error == nil || !strings.Contains(res.Error.Message, want) {
			t.Fatalf("refusal %q does not name %q", res.Error, want)
		}
	}
	if fake.CreateCount() != 0 {
		t.Fatalf("a refused create allocated %d instances", fake.CreateCount())
	}
	mustEnd(t, fake)
}

// A denied Budgets call is not the end of it either: Cost Explorer still
// gets asked, and one working source is enough.
func TestBudgetDeniedFallsBackToCostExplorer(t *testing.T) {
	fake := fakelightsail.New(t)
	fake.SetBudget(monthlyCap(9))
	fake.Inject("DescribeBudget", fakelightsail.FaultAccessDenied, 100)
	a := open(t, fake, nil)

	v := verdict(t, a, "budget-denied")
	if !v.OK || v.SpendSource != aws.SourceCostExplorer {
		t.Fatalf("verdict = %+v, want a passing verdict sourced from cost explorer", v)
	}
	if v.MonthToDateUSD != 3.5 { // the fake's Cost Explorer figure
		t.Fatalf("month to date = %v, want the cost explorer figure 3.5", v.MonthToDateUSD)
	}
	mustEnd(t, fake)
}

// The cap comes from the BUDGET'S OWN LIMIT when the operator did not pass
// --budget-cap: the number lives where the operator set it, so the two
// cannot drift.
func TestCapComesFromTheBudgetLimit(t *testing.T) {
	fake := fakelightsail.New(t)
	fake.SetBudget(fakelightsail.Budget{
		Name: aws.DefaultBudgetName, LimitUSD: 10, ActualSpendUSD: 7,
	})
	a := open(t, fake, nil)

	// 7 + 5 = 12, over the budget's own $10 limit — and well under the
	// compiled-in $50 default, so only the budget's limit can refuse this.
	res, err := do(t, a, provider.Request{
		Op: provider.OpCreate, IdempotencyKey: "over-budget-limit", Spec: spec("over-budget-limit"),
	})
	if codeOf(err) != provider.ErrInvalidRequest {
		t.Fatalf("err = %v, want %s", err, provider.ErrInvalidRequest)
	}
	for _, want := range []string{"$7.00", "$12.00", "over the $10.00 monthly cap", "cap from its $10.00 limit"} {
		if res.Error == nil || !strings.Contains(res.Error.Message, want) {
			t.Fatalf("refusal %q does not name %q", res.Error, want)
		}
	}
	if fake.CreateCount() != 0 {
		t.Fatal("a refused create allocated an instance")
	}
	mustEnd(t, fake)
}

// ...and an explicit --budget-cap always wins over the budget's limit.
func TestExplicitCapOverridesTheBudgetLimit(t *testing.T) {
	fake := fakelightsail.New(t)
	fake.SetBudget(fakelightsail.Budget{
		Name: aws.DefaultBudgetName, LimitUSD: 10, ActualSpendUSD: 7,
	})
	a := open(t, fake, func(c *aws.Config) { c.Budget.MonthlyCapUSD = 30 })

	v := verdict(t, a, "explicit-cap")
	if !v.OK {
		t.Fatalf("verdict = %+v, want a pass under the explicit $30 cap", v)
	}
	if v.MonthlyCapUSD != 30 || v.CapSource != "config" {
		t.Fatalf("cap = $%.2f from %q, want $30.00 from config", v.MonthlyCapUSD, v.CapSource)
	}
	mustEnd(t, fake)
}

// A budget AWS has not evaluated yet reports no CalculatedSpend. That is
// an unanswered question, NOT zero spend: the spend figure falls back to
// Cost Explorer while the CAP still comes from the budget's limit.
func TestBudgetWithNoCalculatedSpendFallsBackForSpendOnly(t *testing.T) {
	fake := fakelightsail.New(t)
	fake.SetBudget(fakelightsail.Budget{
		Name: aws.DefaultBudgetName, LimitUSD: 20, NoSpend: true,
	})
	a := open(t, fake, nil)

	v := verdict(t, a, "no-spend-yet")
	if v.SpendSource != aws.SourceCostExplorer || v.MonthToDateUSD != 3.5 {
		t.Fatalf("spend = %v from %q, want 3.5 from cost explorer", v.MonthToDateUSD, v.SpendSource)
	}
	if v.MonthlyCapUSD != 20 || v.CapSource != aws.SourceBudget {
		t.Fatalf("cap = $%.2f from %q, want $20.00 from the budget", v.MonthlyCapUSD, v.CapSource)
	}
	// Two sources in one verdict, so the reason has to name the budget
	// rather than say "its" and leave the reader guessing.
	want := "spend from cost explorer, cap from the " + aws.DefaultBudgetName + " budget's $20.00 limit"
	if !strings.Contains(v.Reason, want) {
		t.Fatalf("reason %q does not read %q", v.Reason, want)
	}
	mustEnd(t, fake)
}

// A budget whose spend counts credits does not report spend before
// credits, so the verdict says so: the figure is a LOWER bound on the
// bill, and an operator reading a dry-run has to see that.
func TestBudgetThatCountsCreditsIsReportedAsSuch(t *testing.T) {
	fake := fakelightsail.New(t)
	b := monthlyCap(4)
	b.IncludeCredit = true
	fake.SetBudget(b)
	a := open(t, fake, nil)

	v := verdict(t, a, "credit-funded")
	if !v.OK || !v.CreditsIncluded {
		t.Fatalf("verdict = %+v, want a pass flagged credits_included", v)
	}
	if !strings.Contains(v.Reason, "may understate the bill") {
		t.Fatalf("reason %q does not warn that the figure counts credits", v.Reason)
	}
	mustEnd(t, fake)
}

// A budget denominated in another currency cannot be compared with a USD
// cap, so it is not used: the spend falls back rather than silently
// treating EUR as USD.
func TestNonUSDBudgetIsNotUsed(t *testing.T) {
	fake := fakelightsail.New(t)
	fake.SetBudget(fakelightsail.Budget{
		Name: aws.DefaultBudgetName, LimitUSD: 10, ActualSpendUSD: 7, Unit: "EUR",
	})
	a := open(t, fake, nil)

	v := verdict(t, a, "eur-budget")
	if v.SpendSource != aws.SourceCostExplorer {
		t.Fatalf("spend_source = %q, want the cost explorer fallback", v.SpendSource)
	}
	if v.CreditsIncluded {
		t.Fatal("a cost-explorer figure excludes credits by construction; credits_included must not carry over from the budget")
	}
	if v.MonthlyCapUSD != aws.DefaultMonthlyCapUSD || v.CapSource != "default" {
		t.Fatalf("cap = $%.2f from %q, want the $%.0f default", v.MonthlyCapUSD, v.CapSource, aws.DefaultMonthlyCapUSD)
	}
	mustEnd(t, fake)
}

// --budget-name reaches the Budgets call, and only the named budget
// answers.
func TestBudgetNameIsConfigurable(t *testing.T) {
	fake := fakelightsail.New(t)
	fake.SetBudget(fakelightsail.Budget{Name: "sprint-522-cap", LimitUSD: 40, ActualSpendUSD: 6})
	a := open(t, fake, func(c *aws.Config) { c.Budget.Name = "sprint-522-cap" })

	v := verdict(t, a, "named-budget")
	if v.SpendSource != aws.SourceBudget || v.MonthToDateUSD != 6 {
		t.Fatalf("spend = %v from %q, want 6 from the named budget", v.MonthToDateUSD, v.SpendSource)
	}
	if v.BudgetName != "sprint-522-cap" || v.MonthlyCapUSD != 40 {
		t.Fatalf("verdict = %+v, want the sprint-522-cap budget's $40 limit", v)
	}
	// The default name does not answer on this account.
	b := open(t, fake, nil)
	if got := verdict(t, b, "default-name").SpendSource; got != aws.SourceCostExplorer {
		t.Fatalf("spend_source for the default budget name = %q, want the fallback", got)
	}
	mustEnd(t, fake)
}

// The account id is asked for ONCE and reused: it cannot change, and a
// create already makes enough calls. A caller that knows it can skip STS
// altogether.
func TestAccountIDIsCachedAndOverridable(t *testing.T) {
	fake := fakelightsail.New(t)
	fake.SetBudget(monthlyCap(2))
	a := open(t, fake, nil)
	verdict(t, a, "acct-1")
	verdict(t, a, "acct-2")
	if n := fake.Calls("GetCallerIdentity"); n != 1 {
		t.Fatalf("GetCallerIdentity called %d times across two preflights, want 1", n)
	}

	b := open(t, fake, func(c *aws.Config) { c.AccountID = fakelightsail.TestAccountID })
	if v := verdict(t, b, "acct-given"); v.SpendSource != aws.SourceBudget {
		t.Fatalf("spend_source = %q with a configured account id, want the budget", v.SpendSource)
	}
	if n := fake.Calls("GetCallerIdentity"); n != 1 {
		t.Fatalf("GetCallerIdentity called %d times, want still 1: a configured account id asks STS nothing", n)
	}
	mustEnd(t, fake)
}

// STS down is just another unavailable budget: the preflight falls back to
// Cost Explorer instead of refusing.
func TestSTSFailureFallsBackToCostExplorer(t *testing.T) {
	fake := fakelightsail.New(t)
	fake.SetBudget(monthlyCap(2))
	fake.Inject("GetCallerIdentity", fakelightsail.FaultServiceError, 100)
	a := open(t, fake, nil)

	v := verdict(t, a, "sts-down")
	if !v.OK || v.SpendSource != aws.SourceCostExplorer {
		t.Fatalf("verdict = %+v, want a pass sourced from cost explorer", v)
	}
	if n := fake.Calls("DescribeBudget"); n != 0 {
		t.Fatalf("DescribeBudget was called %d time(s) without an account id", n)
	}
	mustEnd(t, fake)
}
