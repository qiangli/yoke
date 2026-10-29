package accounts

import (
	"context"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/accounts/accountstest"
	awsprovider "github.com/qiangli/yoke/pkg/provider/aws"
	do "github.com/qiangli/yoke/pkg/provider/digitalocean"
)

func adoptRegistry(t *testing.T) (*Registry, *DigitalOceanAdapter, *AWSAdapter) {
	t.Helper()
	secretGet := func(name string) (string, error) { return accountstest.CanarySecret + ":" + name, nil }
	m := &Materializer{
		SecretGet: secretGet,
		AWSProfileKeys: func(p string) (awsprovider.Credentials, error) {
			return awsprovider.Credentials{AccessKeyID: p, SecretAccessKey: accountstest.CanarySecret}, nil
		},
	}
	d := NewDigitalOceanAdapter()
	d.Materializer = m
	d.configure = func(c *do.ClientConfig) {
		token := c.Token
		c.HTTPClient = &http.Client{Transport: cloudTransport(func(r *http.Request) (*http.Response, error) {
			if r.Header.Get("Authorization") != "Bearer "+token || !strings.HasPrefix(token, accountstest.CanarySecret+":") {
				t.Fatal("token not materialized from secret")
			}
			team := strings.TrimPrefix(token, accountstest.CanarySecret+":secret-")
			if r.URL.Path == "/v2/account" {
				return cloudReply(200, `{"account":{"email":"a@example.com","uuid":"u","team":{"uuid":"`+team+`","name":"`+team+`"},"status":"active"}}`), nil
			}
			return cloudReply(200, `{}`), nil
		})}
	}
	a := NewAWSAdapter()
	a.Materializer = m
	a.configure = func(c *awsprovider.Config) {
		profile := c.Credentials.AccessKeyID
		c.HTTPClient = &http.Client{Transport: cloudTransport(func(r *http.Request) (*http.Response, error) {
			if r.Header.Get("X-Amz-Target") == "" {
				io.Copy(io.Discard, r.Body)
				acct := "111111111111"
				if profile == "otter" {
					acct = "222222222222"
				}
				return cloudReply(200, `<GetCallerIdentityResponse><GetCallerIdentityResult><Account>`+acct+`</Account><Arn>arn:aws:iam::`+acct+`:user/x</Arn></GetCallerIdentityResult></GetCallerIdentityResponse>`), nil
			}
			return cloudReply(200, `{"instances":[]}`), nil
		})}
	}
	reg := NewRegistry()
	if err := reg.Register(d); err != nil {
		t.Fatal(err)
	}
	if err := reg.Register(a); err != nil {
		t.Fatal(err)
	}
	return reg, d, a
}

func TestRefreshAdoptsManualAccountsIntoOneResolvedRecord(t *testing.T) {
	ctx := context.Background()
	reg, _, _ := adoptRegistry(t)
	store := Open(filepath.Join(t.TempDir(), "accounts.json"))
	SetHookStore(store)
	t.Cleanup(func() { SetHookStore(nil) })

	manual := []Account{
		{Alias: "do-owl", Category: DigitalOcean, Provider: "digitalocean", Connections: []Connection{{ID: "c", CredentialRef: "secret:secret-owl"}}},
		{Alias: "do-otter", Category: DigitalOcean, Provider: "digitalocean", Connections: []Connection{{ID: "c", CredentialRef: "secret:secret-otter"}}},
		{Alias: "aws-owl", Category: AWS, Provider: "aws", Connections: []Connection{{ID: "owl", Profile: "owl", CredentialRef: "profile:owl"}}},
		{Alias: "aws-otter", Category: AWS, Provider: "aws", Connections: []Connection{{ID: "otter", Profile: "otter", CredentialRef: "profile:otter"}}},
	}
	for _, m := range manual {
		added, err := store.Add(m)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := RefreshOne(ctx, store, reg, added); err != nil {
			t.Fatalf("refresh %s: %v", m.Alias, err)
		}
	}
	rows, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 4 {
		t.Fatalf("want 4 records, got %d: %+v", len(rows), rows)
	}
	for _, r := range rows {
		if r.Status != Resolved || r.ProviderAccountID == "" || r.Alias == "" {
			t.Fatalf("not adopted: %+v", r)
		}
	}
	accountstest.AssertNoSecret(t, rows)
	for _, m := range manual {
		acct, sel, err := SelectFor(ctx, store, reg, m.Alias, OpInfra)
		if err != nil {
			t.Fatalf("select %s: %v", m.Alias, err)
		}
		if sel.CredentialRef != m.Connections[0].CredentialRef || acct.Alias != m.Alias {
			t.Fatalf("wrong selection for %s: %+v", m.Alias, sel)
		}
	}
	// Refreshing again stays at one record per identity.
	again, _ := store.Get("do-owl")
	if _, err := RefreshOne(ctx, store, reg, again); err != nil {
		t.Fatal(err)
	}
	if rows, _ = store.List(); len(rows) != 4 {
		t.Fatalf("repeat refresh duplicated records: %d", len(rows))
	}
}

func TestAdoptMergesIntoExistingObservedRecord(t *testing.T) {
	ctx := context.Background()
	reg, d, _ := adoptRegistry(t)
	store := Open(filepath.Join(t.TempDir(), "accounts.json"))
	SetHookStore(nil)
	conn := Connection{ID: "c", CredentialRef: "secret:secret-owl"}
	obs, err := d.Verify(ctx, Account{Category: DigitalOcean, Provider: "digitalocean", Connections: []Connection{conn}})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Observe("digitalocean", []Observation{obs}); err != nil {
		t.Fatal(err)
	}
	added, err := store.Add(Account{Alias: "mine", Name: "Mine", Category: DigitalOcean, Provider: "digitalocean", Connections: []Connection{conn}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := RefreshOne(ctx, store, reg, added); err != nil {
		t.Fatal(err)
	}
	rows, _ := store.List()
	if len(rows) != 1 || rows[0].Alias != "mine" || rows[0].Name != "Mine" || rows[0].Status != Resolved {
		t.Fatalf("merge lost data: %+v", rows)
	}
}
