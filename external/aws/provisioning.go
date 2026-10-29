// The live provisioning-authorization check (Sprint 522 story ed3ef42edf96).
//
// Authentication proves identity, never authorization: a resolved
// credential says who you are, not that you may spend money. Sprint 457
// recorded that invariant with ProvisioningPermitted, which refused
// offline because the live check had no home yet. It has one now — the
// Lightsail adapter's budget preflight in pkg/provider/aws — and this file
// is the seam between `bashy aws` and that preflight.
//
// ProvisioningPermitted still refuses, for the original reason: it is
// handed nothing but credentials, and no amount of credential inspection
// can answer "may this create happen". What answers that is
// ProvisioningAllowed, which needs to know WHAT would be created (the
// region and the bundle, because the verdict is arithmetic about that
// bundle's monthly price) and then asks AWS: the bundle's price from
// Lightsail, the account's month-to-date unblended cost BEFORE CREDITS
// from Cost Explorer, both against the operator's caps.
//
// (S522.2 rework 1: "asks AWS" now means the account's own AWS BUDGET
// first — DescribeBudget's CalculatedSpend.ActualSpend, and its
// BudgetLimit as the cap when none is given — with Cost Explorer as the
// fallback. The live account's Cost Explorer answers
// DataUnavailableException because no cost data has been ingested yet;
// pkg/provider/aws/budgets.go holds the ladder.)
//
// It FAILS CLOSED, the same way the adapter does: when NEITHER spend
// source can be read, spend is unknown, and unknown spend refuses. Credential material never reaches a message here — every string
// that could carry provider text goes through RedactText with the live
// secret as a declared literal.
package aws

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/qiangli/yoke/pkg/provider"
	awsprovider "github.com/qiangli/yoke/pkg/provider/aws"
)

// ProvisionIntent is what a provisioning request would create. The budget
// verdict is arithmetic about a specific bundle in a specific region, so
// those two are required: there is no "may I provision in general".
type ProvisionIntent struct {
	// Region is the Lightsail region; empty means the adapter's default
	// (us-west-2, the only region the automation principal may act in).
	Region string
	// BundleID is the Lightsail plan whose monthly price is checked.
	BundleID string
	// BlueprintID is the machine image; empty means Ubuntu 24.04 LTS.
	BlueprintID string
	// Owner scopes the ownership tags; empty means "local".
	Owner string
	// MonthlyCapUSD and PerInstanceUSD override the operator's caps; zero
	// means the adapter's defaults (the account budget's own limit, else
	// $50, and $24).
	MonthlyCapUSD, PerInstanceUSD float64
	// BudgetName is the account budget read for month-to-date spend (and
	// for the cap, when MonthlyCapUSD is zero); empty means the adapter's
	// default, dhnt-monthly-cap.
	BudgetName string
	// Endpoints override the Lightsail, Cost Explorer, Budgets and STS
	// URLs. They exist for tests; no flag or environment variable sets
	// them, so production cannot be redirected by accident.
	LightsailURL, CostExplorerURL, BudgetsURL, STSURL string
	// Keys is an explicit key pair for THIS check only (S524.7): the pair
	// `bashy infra --account <alias>` resolved from that account's profile.
	//
	// It exists because the budget guard is the one authorization step that
	// must run as the SAME identity as the create it is guarding. Reading
	// the ambient environment instead would let a guard pass on one
	// account's budget while the create spends from another's. Zero value
	// keeps the historical behavior: the explicit environment key pair.
	Keys awsprovider.Credentials
}

// ProvisioningAllowed answers whether one create may happen, by running
// the Lightsail adapter's budget preflight against the live account. It
// returns the verdict always — including when it refuses, so the caller
// can show the operator the numbers — and a typed error when the answer
// is no.
//
// Nothing is created: the preflight runs through the provider contract's
// plan operation, which allocates nothing by construction.
func ProvisioningAllowed(ctx context.Context, c Credentials, intent ProvisionIntent) (awsprovider.Verdict, *Error) {
	if intent.BundleID == "" {
		return awsprovider.Verdict{}, NewError(CodeInvalidInput,
			"aws: provisioning authorization needs a bundle id — the budget verdict is arithmetic about one plan's monthly price", nil)
	}
	keys := intent.Keys
	if !keys.Valid() {
		fromEnv, perr := awsprovider.CredentialsFromEnv(os.Environ())
		if perr != nil {
			return awsprovider.Verdict{}, NewError(CodeMissingCredentials,
				fmt.Sprintf("aws: provisioning authorization for %s needs an explicit key pair: %s", c.RedactedScope(), perr.Message), nil)
		}
		keys = fromEnv
	}
	region := intent.Region
	if region == "" {
		region = c.Region
	}
	owner := intent.Owner
	if owner == "" {
		owner = "local"
	}
	blueprint := intent.BlueprintID
	if blueprint == "" {
		blueprint = awsprovider.DefaultBlueprintID
	}
	a, err := awsprovider.New(awsprovider.Config{
		Credentials:     keys,
		Region:          region,
		Owner:           owner,
		BaseURL:         intent.LightsailURL,
		CostExplorerURL: intent.CostExplorerURL,
		BudgetsURL:      intent.BudgetsURL,
		STSURL:          intent.STSURL,
		Budget: awsprovider.Budget{
			Name:           intent.BudgetName,
			MonthlyCapUSD:  intent.MonthlyCapUSD,
			PerInstanceUSD: intent.PerInstanceUSD,
		},
	})
	if err != nil {
		return awsprovider.Verdict{}, typedFromProvider(err, keys.SecretAccessKey)
	}
	res, planErr := a.Do(ctx, provider.Request{
		ProtocolVersion: provider.ProtocolVersion,
		Op:              provider.OpPlan,
		Spec: &provider.Spec{
			Name:   "preflight",
			Region: region,
			Size:   intent.BundleID,
			Image:  blueprint,
		},
	})
	verdict := verdictOf(res)
	if planErr != nil {
		return verdict, typedFromProvider(planErr, keys.SecretAccessKey)
	}
	if !verdict.OK {
		// A plan that succeeded with a non-OK verdict means the budget
		// check was skipped (disabled), which is not an authorization.
		return verdict, NewError(CodePermissionDenied,
			RedactText("aws: provisioning refused: "+verdict.Reason, keys.SecretAccessKey), nil)
	}
	return verdict, nil
}

// verdictOf pulls the budget verdict out of a plan result's payload. A
// payload that does not parse leaves a zero verdict rather than a guess.
func verdictOf(res provider.Result) awsprovider.Verdict {
	if len(res.Data) == 0 {
		return awsprovider.Verdict{}
	}
	var p struct {
		Budget awsprovider.Verdict `json:"budget"`
	}
	if json.Unmarshal(res.Data, &p) != nil {
		return awsprovider.Verdict{}
	}
	return p.Budget
}

// typedFromProvider maps a normalized provider error onto this package's
// typed codes, redacting the live secret from whatever text it carries.
func typedFromProvider(err error, secret string) *Error {
	msg := RedactText(err.Error(), secret)
	var pe *provider.Error
	if !errors.As(err, &pe) {
		return NewError(CodeProviderFailure, msg, nil)
	}
	code := CodeProviderFailure
	switch pe.Code {
	case provider.ErrInvalidCredentials:
		code = CodePermissionDenied
	case provider.ErrInvalidRequest:
		// The budget refusal travels as invalid_request (the request asks
		// to spend more than is authorized); report it as the denial it is.
		code = CodeInvalidInput
		if strings.Contains(pe.Message, "budget preflight refused create") {
			code = CodePermissionDenied
		}
	case provider.ErrNotFound:
		code = CodeNotFound
	case provider.ErrRateLimited:
		code = CodeRateLimit
	case provider.ErrTimeout:
		code = CodeTimeout
	}
	e := NewError(code, RedactText(pe.Message, secret), nil)
	e.ProviderCode = string(pe.Code)
	e.Retryable = pe.Retryable
	if e.Message == "" {
		e.Message = msg
	}
	return e
}
