package accounts

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/accounts/accountstest"
	awsprovider "github.com/qiangli/yoke/pkg/provider/aws"
	do "github.com/qiangli/yoke/pkg/provider/digitalocean"
	"github.com/qiangli/yoke/pkg/provider/digitalocean/fakedo"
)

type cloudTransport func(*http.Request) (*http.Response, error)

func (f cloudTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func cloudReply(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}
}
func cloudJSON(t *testing.T, v any) string {
	t.Helper()
	b, e := json.Marshal(v)
	if e != nil {
		t.Fatal(e)
	}
	accountstest.AssertNoSecret(t, string(b))
	return string(b)
}

func TestDigitalOceanTeamsInventoryAndPartialBilling(t *testing.T) {
	ctx := context.Background()
	a := NewDigitalOceanAdapter()
	clients := map[string]*do.Client{}
	servers := map[string]*fakedo.Server{}
	for _, name := range []string{"owl", "otter"} {
		s := fakedo.New(t)
		s.SetMaxPerPage(1)
		servers[name] = s
		s.SetAccount(map[string]any{"email": "alice@example.com", "uuid": "same-user", "team": map[string]string{"uuid": name, "name": name}, "droplet_limit": 25, "status": "active"})
		s.SetAccountInventory([]map[string]any{{"id": 1, "name": "alice", "fingerprint": "aa:bb"}}, map[string]any{"account_balance": "-2.50", "month_to_date_balance": "3.25", "month_to_date_usage": "5.75", "generated_at": "2026-10-29T00:00:00Z"})
		id := s.AddDroplet(name)
		s.AddDroplet(name + "-two")
		s.AddProject(name)
		ref := "secret:" + name
		a.Connections = append(a.Connections, Connection{ID: name, CredentialRef: ref})
		if a.DropletKeys == nil {
			a.DropletKeys = map[string]map[int][]string{}
		}
		a.DropletKeys[ref] = map[int][]string{id: {"aa:bb", "unknown"}}
		httpClient := &http.Client{Transport: cloudTransport(func(r *http.Request) (*http.Response, error) {
			if r.Method != http.MethodGet {
				t.Fatal("mutation attempted")
			}
			if r.Header.Get("Authorization") != "Bearer "+accountstest.CanarySecret {
				t.Fatal("wrong token")
			}
			if name == "otter" && r.URL.Path == "/v2/customers/my/balance" {
				return cloudReply(403, `{"message":"`+accountstest.CanarySecret+`"}`), nil
			}
			clone := r.Clone(r.Context())
			clone.Header.Set("Authorization", "Bearer "+fakedo.TestToken)
			return s.Client().Transport.RoundTrip(clone)
		})}
		c, e := do.NewClient(do.ClientConfig{BaseURL: s.URL, Token: accountstest.CanarySecret, HTTPClient: httpClient})
		if e != nil {
			t.Fatal(e)
		}
		clients[ref] = c
	}
	a.Client = func(_ context.Context, c Connection) (*do.Client, error) { return clients[c.CredentialRef], nil }
	discovered, e := a.Discover(ctx)
	if e != nil {
		t.Fatal(e)
	}
	if len(discovered) != 2 {
		t.Fatal("missing configurations")
	}
	store := Open(filepath.Join(t.TempDir(), "accounts.json"))
	for _, o := range discovered {
		if o.Account.Status != Unresolved {
			t.Fatal("discovery invented identity")
		}
		verified, e := a.Verify(ctx, o.Account)
		if e != nil {
			t.Fatal(e)
		}
		cloudJSON(t, verified)
		if verified.Account.ProviderAccountID != strings.TrimPrefix(o.SourceKey, "secret:") {
			t.Fatal("team crossover")
		}
		counts := map[string]int{}
		for _, r := range verified.Account.Resources {
			counts[r.Kind]++
		}
		if counts["droplet"] != 2 || counts["droplet-ssh-key"] != 1 || counts["project"] != 1 || counts["region"] != 1 {
			t.Fatalf("inventory: %v", counts)
		}
		if e = store.Observe(a.Name(), []Observation{verified}); e != nil {
			t.Fatal(e)
		}
		metrics, e := a.Billing(ctx, verified.Account, "month")
		if e != nil {
			t.Fatal(e)
		}
		cloudJSON(t, metrics)
		for _, m := range metrics {
			if m.Unit != "usd" || m.Currency != "USD" || m.Classification != ProviderReported || m.Coverage != Partial {
				t.Fatal("wrong metric contract")
			}
			if verified.Account.Team == "otter" && m.Value != nil {
				t.Fatal("403 became zero")
			}
		}
		if verified.Account.Team == "owl" && (metrics[0].Value == nil || *metrics[0].Value != -2.5 || metrics[2].WindowStart == nil) {
			t.Fatal("wrong balance/window")
		}
		u, e := a.Usage(ctx, verified.Account, "month")
		if e != nil || u[0].Value == nil || *u[0].Value != 25 {
			t.Fatal("quota missing")
		}
		cloudJSON(t, u)
		wrong := verified.Account
		wrong.ProviderAccountID = "other"
		_, e = a.Resources(ctx, wrong)
		if !errors.Is(e, errCloudIdentity) {
			t.Fatal("mismatched team accepted")
		}
		accountstest.AssertNoSecret(t, e)
	}
	rows, e := store.List()
	if e != nil || len(rows) != 2 {
		t.Fatal("teams merged")
	}
	cloudJSON(t, rows)
	for _, s := range servers {
		if s.CreateCount() != 0 || s.DeleteCount() != 0 {
			t.Fatal("remote mutation")
		}
	}
}

func TestAWSProfilesIdentityResourcesAndMoney(t *testing.T) {
	ctx := context.Background()
	a := NewAWSAdapter()
	for _, p := range []string{"owl", "otter"} {
		a.Connections = append(a.Connections, Connection{ID: p, Profile: p, CredentialRef: "profile:" + p})
	}
	a.Client = func(_ context.Context, c Connection, _ string) (*awsprovider.InventoryClient, error) {
		h := &http.Client{Transport: cloudTransport(func(r *http.Request) (*http.Response, error) {
			if !strings.Contains(r.Header.Get("Authorization"), "Credential="+c.Profile+"/") {
				t.Fatal("profile crossover")
			}
			target := r.Header.Get("X-Amz-Target")
			switch {
			case target == "":
				body, _ := io.ReadAll(r.Body)
				if !strings.Contains(string(body), "Action=GetCallerIdentity") {
					t.Fatal("unexpected STS call")
				}
				return cloudReply(200, `<GetCallerIdentityResponse><GetCallerIdentityResult><Account>123456789012</Account><Arn>arn:aws:iam::123456789012:role/`+c.Profile+`</Arn></GetCallerIdentityResult></GetCallerIdentityResponse>`), nil
			case strings.HasSuffix(target, ".GetInstances"):
				return cloudReply(200, `{"instances":[{"name":"`+c.Profile+`","arn":"arn:aws:lightsail:us-east-1:123456789012:Instance/`+c.Profile+`","location":{"regionName":"us-east-1"}}]}`), nil
			case strings.HasSuffix(target, ".DescribeBudget"):
				if c.Profile == "otter" {
					return cloudReply(403, `{"message":"`+accountstest.CanarySecret+`"}`), nil
				}
				return cloudReply(200, `{"Budget":{"BudgetType":"COST","TimeUnit":"MONTHLY","BudgetLimit":{"Amount":"0","Unit":"USD"},"CalculatedSpend":{"ActualSpend":{"Amount":"12.50","Unit":"USD"}},"CostTypes":{"IncludeCredit":true}}}`), nil
			case strings.HasSuffix(target, ".GetCostAndUsage"):
				return cloudReply(200, `{"ResultsByTime":[{"Total":{"UnblendedCost":{"Amount":"7","Unit":"USD"}}}]}`), nil
			default:
				t.Fatalf("unexpected provider operation: %s", target)
				return nil, fmt.Errorf("unexpected call")
			}
		})}
		return awsprovider.NewInventoryClient(awsprovider.Config{Credentials: awsprovider.Credentials{AccessKeyID: c.Profile, SecretAccessKey: accountstest.CanarySecret}, Region: "us-east-1", HTTPClient: h})
	}
	discovered, e := a.Discover(ctx)
	if e != nil {
		t.Fatal(e)
	}
	store := Open(filepath.Join(t.TempDir(), "accounts.json"))
	for _, o := range discovered {
		v, e := a.Verify(ctx, o.Account)
		if e != nil {
			t.Fatal(e)
		}
		cloudJSON(t, v)
		p := o.Account.Connections[0].Profile
		wrong := v.Account
		wrong.Role = "arn:aws:iam::123456789012:role/other"
		_, roleErr := a.Resources(ctx, wrong)
		if !errors.Is(roleErr, errCloudIdentity) {
			t.Fatal("role crossover accepted")
		}
		accountstest.AssertNoSecret(t, roleErr)
		if v.Account.ProviderAccountID != "123456789012" || !strings.HasSuffix(v.Account.Role, "/"+p) || v.Account.Resources[0].Name != p {
			t.Fatal("incorrect attribution")
		}
		if e = store.Observe(a.Name(), []Observation{v}); e != nil {
			t.Fatal(e)
		}
		ms, e := a.Billing(ctx, v.Account, "month")
		if e != nil {
			t.Fatal(e)
		}
		cloudJSON(t, ms)
		if ms[0].Value == nil || ms[0].Unit != "usd" || ms[0].Currency != "USD" || ms[0].WindowStart == nil || ms[0].Classification != ProviderReported {
			t.Fatal("invalid spend")
		}
		if p == "owl" {
			if *ms[0].Value != 12.5 || ms[1].Value == nil || *ms[1].Value != 0 {
				t.Fatal("reported zero budget lost")
			}
		} else {
			if *ms[0].Value != 7 || ms[0].Source != "aws/cost-explorer" || ms[1].Value != nil {
				t.Fatal("fallback or unknown limit wrong")
			}
		}
	}
	rows, e := store.List()
	if e != nil || len(rows) != 2 {
		t.Fatal("profiles in same AWS account merged")
	}
	cloudJSON(t, rows)
}

func TestCloudFailuresAndUnsupported(t *testing.T) {
	ctx := context.Background()
	a := NewDigitalOceanAdapter()
	acct := Account{Provider: "digitalocean", Connections: []Connection{{CredentialRef: "secret:owl"}}}
	a.Client = func(context.Context, Connection) (*do.Client, error) {
		return nil, errors.New(accountstest.CanarySecret)
	}
	_, e := a.Verify(ctx, acct)
	if e == nil {
		t.Fatal("failure hidden")
	}
	accountstest.AssertNoSecret(t, e)
	acct.Connections = append(acct.Connections, acct.Connections[0])
	_, e = a.Verify(ctx, acct)
	if !errors.Is(e, ErrAmbiguous) {
		t.Fatal("ambiguous selection accepted")
	}
	accountstest.AssertNoSecret(t, e)
	hook := ManagedAdapter{ProviderName: "example"}
	_, e = hook.Verify(ctx, Account{})
	if !errors.Is(e, ErrUnsupported) {
		t.Fatal("unsupported enrichment accepted")
	}
	accountstest.AssertNoSecret(t, e)
	r := NewRegistry()
	if e = r.Register(hook); e != nil {
		t.Fatal(e)
	}
	if e = RegisterDefaults(r); e != nil {
		t.Fatal(e)
	}
	for _, p := range []string{"digitalocean", "aws"} {
		if _, e = r.Adapter(p, Billing); e != nil {
			t.Fatal(e)
		}
	}
	if _, e = r.Adapter("example", Billing); !errors.Is(e, ErrUnsupported) {
		t.Fatal("invented capability")
	}
	// Store contents as well as serialized return values must omit the sentinel.
	dir := t.TempDir()
	s := Open(filepath.Join(dir, "accounts.json"))
	if e = s.Observe("example", []Observation{{SourceKey: "owl", Account: Account{Category: Other, Provider: "example", Connections: []Connection{{ID: "owl", CredentialRef: "secret:owl"}}}}}); e != nil {
		t.Fatal(e)
	}
	filepath.WalkDir(dir, func(path string, d os.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if !d.IsDir() {
			b, e := os.ReadFile(path)
			if e != nil {
				t.Fatal(e)
			}
			accountstest.AssertNoSecret(t, string(b))
		}
		return nil
	})
}

func TestAWSUnavailableAndNonUSDStayNil(t *testing.T) {
	for _, body := range []string{`{}`, `{"ResultsByTime":[{"Total":{"UnblendedCost":{"Amount":"9","Unit":"EUR"}}}]}`, `{"ResultsByTime":[{"Total":{"UnblendedCost":{"Amount":"NaN","Unit":"USD"}}}]}`} {
		a := NewAWSAdapter()
		a.Client = func(context.Context, Connection, string) (*awsprovider.InventoryClient, error) {
			return awsprovider.NewInventoryClient(awsprovider.Config{Credentials: awsprovider.Credentials{AccessKeyID: "owl", SecretAccessKey: accountstest.CanarySecret}, Region: "us-east-1", HTTPClient: &http.Client{Transport: cloudTransport(func(r *http.Request) (*http.Response, error) {
				target := r.Header.Get("X-Amz-Target")
				if target == "" {
					return cloudReply(200, `<GetCallerIdentityResponse><GetCallerIdentityResult><Account>123456789012</Account></GetCallerIdentityResult></GetCallerIdentityResponse>`), nil
				}
				if strings.HasSuffix(target, ".DescribeBudget") {
					return cloudReply(403, `{"message":"`+accountstest.CanarySecret+`"}`), nil
				}
				if !strings.HasSuffix(target, ".GetCostAndUsage") {
					t.Fatal("unexpected operation")
				}
				return cloudReply(200, body), nil
			})}})
		}
		ms, e := a.Billing(context.Background(), Account{Provider: "aws", Connections: []Connection{{Profile: "owl", CredentialRef: "profile:owl"}}}, "month")
		if e != nil {
			t.Fatal(e)
		}
		for _, m := range ms {
			if m.Value != nil {
				t.Fatal("unavailable or non-USD became money")
			}
		}
		cloudJSON(t, ms)
	}
}

func TestDigitalOceanKeyPaginationAndMissingIdentity(t *testing.T) {
	calls := 0
	c, e := do.NewClient(do.ClientConfig{Token: accountstest.CanarySecret, HTTPClient: &http.Client{Transport: cloudTransport(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodGet {
			t.Fatal("mutation")
		}
		calls++
		if r.URL.Path == "/v2/account" {
			return cloudReply(200, `{"account":{"uuid":"user-only","email":"alice@example.com"}}`), nil
		}
		if r.URL.Query().Get("page") == "1" {
			return cloudReply(200, `{"ssh_keys":[{"fingerprint":"aa"}],"links":{"pages":{"next":"https://example.com/v2/account/keys?page=2"}}}`), nil
		}
		if r.URL.Host == "example.com" {
			t.Fatal("followed untrusted pagination host")
		}
		return cloudReply(200, `{"ssh_keys":[{"fingerprint":"bb"}]}`), nil
	})}})
	if e != nil {
		t.Fatal(e)
	}
	keys, e := c.AccountKeys(context.Background())
	if e != nil || len(keys) != 2 || calls != 2 {
		t.Fatal("key pagination failed")
	}
	cloudJSON(t, keys)
	a := NewDigitalOceanAdapter()
	a.Client = func(context.Context, Connection) (*do.Client, error) { return c, nil }
	_, e = a.Verify(context.Background(), Account{Provider: "digitalocean", Connections: []Connection{{CredentialRef: "secret:owl"}}})
	if !errors.Is(e, ErrUnsupported) {
		t.Fatal("user UUID used as team identity")
	}
	accountstest.AssertNoSecret(t, e)
}
