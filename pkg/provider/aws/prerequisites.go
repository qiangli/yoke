package aws

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/qiangli/yoke/pkg/provider"
)

// Prerequisites describes the account setup checked before a create.
// Problems contains every missing or unverifiable prerequisite, with its fix.
// It never contains credentials or raw service error responses.
type Prerequisites struct {
	AccountID  string   `json:"account_id,omitempty"`
	Region     string   `json:"region"`
	BudgetName string   `json:"budget_name"`
	Bucket     string   `json:"bucket,omitempty"`
	Problems   []string `json:"problems,omitempty"`
}

// CheckPrerequisites checks account setup without allocating resources. It is
// available separately from New's guarded provider.Adapter for setup UIs.
func CheckPrerequisites(ctx context.Context, cfg Config) (Prerequisites, *provider.Error) {
	a, err := newAdapter(cfg)
	if err != nil {
		return Prerequisites{}, normalize(err)
	}
	return a.CheckPrerequisites(ctx)
}

// CheckPrerequisites is the create preflight. Read failures are distinguished
// from absence, and independent checks continue so the owner gets one fix list.
func (a *adapter) CheckPrerequisites(ctx context.Context) (Prerequisites, *provider.Error) {
	p := Prerequisites{Region: a.cfg.Region, BudgetName: a.cfg.Budget.budgetName()}
	add := func(problem string) { p.Problems = append(p.Problems, problem) }
	const guardFix = "run docs/sprint-522/aws-budget-guard.sh in AWS CloudShell as root to configure the budget and scoped IAM user"
	const bucketFix = "run docs/sprint-522/aws-userdata-bucket.sh in AWS CloudShell as root"
	if a.cfg.Region != DefaultRegion {
		add("region must be us-west-2; configure the adapter for us-west-2")
	}
	if regionOf(a.cfg.AvailabilityZone) != DefaultRegion {
		add("availability zone must be in us-west-2; configure us-west-2a")
	}
	id, arn, err := a.callerIdentity(ctx)
	if err != nil {
		add("account identity could not be verified with STS GetCallerIdentity; provide valid scoped IAM user credentials")
	} else {
		p.AccountID = id
		if strings.HasSuffix(arn, ":root") {
			add("account root credentials are refused; use a scoped IAM user for the master; " + guardFix)
		} else if arn == "" {
			add("IAM principal could not be verified; provide scoped IAM user credentials; " + guardFix)
		}
		if configured := strings.TrimSpace(a.cfg.AccountID); configured != "" && configured != id {
			add("configured account does not match STS identity; configure the owner's STS account")
		}
	}
	// Without an identity, absence cannot be established. Do not mislabel
	// dependent checks as missing or query a caller-supplied foreign account.
	if err != nil || (strings.TrimSpace(a.cfg.AccountID) != "" && strings.TrimSpace(a.cfg.AccountID) != id) {
		add("budget " + p.BudgetName + " could not be verified until the account identity is corrected; " + guardFix)
		add("staging bucket could not be verified until the account identity is corrected; " + bucketFix)
	} else {
		if _, err := a.DescribeBudget(ctx); err != nil {
			state := "could not be verified (check budgets:ViewBudget permission and service availability)"
			if normalize(err).Code == provider.ErrNotFound {
				state = "is missing"
			}
			add("budget " + p.BudgetName + " " + state + "; " + guardFix)
		}
		c, err := a.stagingClient(ctx)
		if err != nil {
			add("staging bucket could not be resolved; " + bucketFix)
		} else {
			p.Bucket = c.bucket
			if err := c.do(ctx, http.MethodHead, "", nil, nil); err != nil {
				state := "could not be verified (check S3 access, bucket region and service availability)"
				if ce, ok := err.(*callError); ok {
					switch ce.status {
					case http.StatusNotFound:
						state = "is missing"
					case http.StatusForbidden:
						state = "is present but unreadable (HeadBucket denied; grant s3:ListBucket on this bucket)"
					}
				}
				add("staging bucket " + p.Bucket + " " + state + "; " + bucketFix)
			}
		}
	}
	if len(p.Problems) != 0 {
		return p, &provider.Error{Code: provider.ErrInvalidRequest, Message: fmt.Sprintf(
			"aws: account prerequisites refused create: %s. See docs/satellite-on-aws.md prerequisites; perform one-time setup in AWS CloudShell as root, then give the master scoped IAM user credentials.", strings.Join(p.Problems, "; "))}
	}
	return p, nil
}
