package aws_test

import (
	"context"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/provider"
	"github.com/qiangli/yoke/pkg/provider/aws"
	"github.com/qiangli/yoke/pkg/provider/aws/fakelightsail"
)

func TestCreatePrerequisites(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(*fakelightsail.Server)
		tweak func(*aws.Config)
		want  []string
	}{
		{"empty", func(f *fakelightsail.Server) { f.ClearBudget(); f.SetBucketStatus(404) }, nil,
			[]string{"budget dhnt-monthly-cap is missing", "staging bucket dhnt-userdata-" + fakelightsail.TestAccountID + "-us-west-2 is missing", "aws-budget-guard.sh", "aws-userdata-bucket.sh", "AWS CloudShell", "docs/satellite-on-aws.md"}},
		{"root", func(f *fakelightsail.Server) { f.SetRootIdentity(true) }, func(c *aws.Config) { c.AccountID = fakelightsail.TestAccountID },
			[]string{"account root", "scoped IAM user"}},
		{"region", nil, func(c *aws.Config) { c.Region = "us-east-1" }, []string{"region must be us-west-2"}},
		{"zone", nil, func(c *aws.Config) { c.AvailabilityZone = "us-east-1a" }, []string{"availability zone must be in us-west-2"}},
		{"unreadable", func(f *fakelightsail.Server) { f.SetBucketStatus(403) }, nil, []string{"present but unreadable", "s3:ListBucket"}},
		{"service failure", func(f *fakelightsail.Server) { f.SetBucketStatus(500) }, nil, []string{"could not be verified"}},
		{"custom budget missing", nil, func(c *aws.Config) { c.Budget.Name = "owner-cap" }, []string{"budget owner-cap is missing"}},
		{"all missing", func(f *fakelightsail.Server) { f.ClearBudget(); f.SetBucketStatus(404); f.SetRootIdentity(true) }, nil,
			[]string{"budget dhnt-monthly-cap is missing", "staging bucket", "is missing", "account root"}},
		{"identity unavailable", func(f *fakelightsail.Server) { f.Inject("GetCallerIdentity", fakelightsail.FaultServiceError, 1) }, nil,
			[]string{"account identity could not be verified", "budget dhnt-monthly-cap could not be verified", "staging bucket could not be verified"}},
		{"account mismatch", nil, func(c *aws.Config) { c.AccountID = "999999999999" }, []string{"configured account does not match STS identity"}},
		{"ready", nil, nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := fakelightsail.New(t)
			if tc.setup != nil {
				tc.setup(f)
			}
			a := open(t, f, tc.tweak)
			_, err := do(t, a, provider.Request{Op: provider.OpCreate, IdempotencyKey: "prereq", Spec: spec("prereq-box")})
			if len(tc.want) == 0 {
				if err != nil {
					t.Fatal(err)
				}
				if f.CreateCount() != 1 {
					t.Fatal("did not create")
				}
			} else {
				if codeOf(err) != provider.ErrInvalidRequest {
					t.Fatalf("error = %v", err)
				}
				for _, want := range tc.want {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("%q lacks %q", err, want)
					}
				}
				for _, op := range []string{"CreateInstances", "PutObject", "AllocateStaticIp", "AttachStaticIp"} {
					if f.Calls(op) != 0 {
						t.Errorf("refusal called %s", op)
					}
				}
				for _, secret := range []string{fakelightsail.TestAccessKeyID, fakelightsail.TestSecretAccessKey} {
					if strings.Contains(err.Error(), secret) {
						t.Fatal("credential leaked")
					}
				}
			}
		})
	}
}

func TestDestroyStaticIPOwnershipBoundary(t *testing.T) {
	for _, gone := range []bool{false, true} {
		for _, tc := range []struct {
			name, ip, attached string
			release            bool
		}{
			{"own", "satellite-ip", "satellite", true},
			{"foreign name", "zoo-aws-ip", "satellite", false},
			{"foreign attachment", "satellite-ip", "another-satellite", false},
		} {
			t.Run(tc.name+map[bool]string{true: "/gone", false: "/present"}[gone], func(t *testing.T) {
				f := fakelightsail.New(t)
				if !gone {
					seed(f, "satellite", "running", "seed", nil)
				}
				f.AddStaticIP(tc.ip, tc.attached)
				f.Observe(func(op string, body map[string]any) {
					if op == "ReleaseStaticIp" || op == "DetachStaticIp" {
						if body["staticIpName"] != "satellite-ip" || !tc.release {
							t.Errorf("unsafe %s: %v", op, body)
						}
					}
				})
				a := open(t, f, nil)
				_, err := do(t, a, provider.Request{Op: provider.OpDestroy, ResourceID: "satellite", IdempotencyKey: "destroy", Authorization: confirm("satellite", "destroy")})
				if err != nil {
					t.Fatal(err)
				}
				_, remains := f.StaticIP(tc.ip)
				if remains == tc.release {
					t.Fatalf("IP remains=%v, want release=%v", remains, tc.release)
				}
				mustEnd(t, f)
			})
		}
	}
}

// The exported setup check is read-only and rechecks bucket/budget readiness
// after a failure, so an owner can complete setup and retry.
func TestExportedPrerequisitesCheck(t *testing.T) {
	f := fakelightsail.New(t)
	var cfg aws.Config
	open(t, f, func(c *aws.Config) { cfg = *c; cfg.Budget.Name = "owner-cap"; cfg.UserDataBucket = "owner-staging" })
	f.ClearBudget()
	f.SetBucketStatus(404)
	p, err := aws.CheckPrerequisites(context.Background(), cfg)
	if err == nil || len(p.Problems) != 2 || p.AccountID != fakelightsail.TestAccountID || p.Bucket != "owner-staging" || p.BudgetName != "owner-cap" {
		t.Fatalf("empty setup = %+v, %v", p, err)
	}
	f.SetBudget(fakelightsail.Budget{Name: "owner-cap", LimitUSD: 50})
	f.SetBucketStatus(200)
	p, err = aws.CheckPrerequisites(context.Background(), cfg)
	if err != nil || len(p.Problems) != 0 {
		t.Fatalf("ready setup = %+v, %v", p, err)
	}
	if f.CreateCount() != 0 || f.Calls("PutObject") != 0 || f.StaticIPCount() != 0 {
		t.Fatal("check mutated account")
	}
	if f.Scope("HeadBucket") != "s3/us-west-2" {
		t.Fatal("HeadBucket signing scope")
	}
	mustEnd(t, f)
}
