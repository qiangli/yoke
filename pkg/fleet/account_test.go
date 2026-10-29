package fleet_test

// `--account` at the fleet seam (S524.7, slice D).
//
// These tests drive the real cobra verbs and a recording HTTP server, with a
// FAKE resolver standing in for the account service. The split is deliberate:
// the seam is where fleet's contract lives (which identity reached the wire,
// what refuses and when), while the store-to-credential half belongs to
// pkg/accounts and is tested there and in pkg/accounts/builtin.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/qiangli/yoke/pkg/accounts/accountstest"
	"github.com/qiangli/yoke/pkg/fleet"
)

// doRecorder is the fake DigitalOcean transport: it answers the one read
// `infra ip list` makes and records the bearer token each request carried, so
// a test can prove WHICH identity reached the wire.
type doRecorder struct {
	srv *httptest.Server

	mu     sync.Mutex
	tokens []string
}

func newDORecorder(t *testing.T) *doRecorder {
	t.Helper()
	r := &doRecorder{}
	r.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.mu.Lock()
		r.tokens = append(r.tokens, strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer "))
		r.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"reserved_ips": []any{}})
	}))
	t.Cleanup(r.srv.Close)
	return r
}

func (r *doRecorder) seen() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.tokens...)
}

// withResolver installs a fake account service for one test and removes it
// again, so a nil resolver (the unwired host) stays testable too.
func withResolver(t *testing.T, fn func(ctx context.Context, ref, operation string) (fleet.AccountSelection, error)) {
	t.Helper()
	prev := fleet.AccountResolver
	fleet.AccountResolver = fn
	t.Cleanup(func() { fleet.AccountResolver = prev })
}

// twoTeams is the fixture the sprint acceptance names: two DigitalOcean teams,
// each with its own token, reachable only by its own alias.
func twoTeams() map[string]fleet.AccountSelection {
	return map[string]fleet.AccountSelection{
		"owl": {AccountID: "acc_owl", Category: fleet.AccountDigitalOcean, Token: "token-owl"},
		"otter": {AccountID: "acc_otter", Category: fleet.AccountDigitalOcean, Token: "token-otter",
			Profile: "otter"},
	}
}

func teamResolver(t *testing.T, teams map[string]fleet.AccountSelection) {
	t.Helper()
	withResolver(t, func(_ context.Context, ref, operation string) (fleet.AccountSelection, error) {
		if operation != "infra" {
			return fleet.AccountSelection{}, fmt.Errorf("unexpected operation %q", operation)
		}
		sel, ok := teams[ref]
		if !ok {
			return fleet.AccountSelection{}, fmt.Errorf("no account %q", ref)
		}
		return sel, nil
	})
}

func runFleetInfra(t *testing.T, args ...string) (string, string, error) {
	t.Helper()
	cmd := fleet.NewInfraCmd()
	var out, errb bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errb)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), errb.String(), err
}

// Each alias must reach the wire as ITS OWN team. This is the whole point of
// the flag: two teams on one host, selected per invocation.
func TestInfraAccountSelectsItsOwnDigitalOceanTeam(t *testing.T) {
	rec := newDORecorder(t)
	t.Setenv("DIGITALOCEAN_API_URL", rec.srv.URL)
	t.Setenv("DIGITALOCEAN_ACCESS_TOKEN", "ambient-token-must-not-be-used")
	teamResolver(t, twoTeams())

	for _, tc := range []struct{ alias, want string }{
		{"owl", "token-owl"},
		{"otter", "token-otter"},
	} {
		if _, _, err := runFleetInfra(t, "ip", "list", "--account", tc.alias, "--json"); err != nil {
			t.Fatalf("infra ip list --account %s: %v", tc.alias, err)
		}
		seen := rec.seen()
		if got := seen[len(seen)-1]; got != tc.want {
			t.Fatalf("--account %s used token %q, want %q", tc.alias, got, tc.want)
		}
	}
	for _, tok := range rec.seen() {
		if tok == "ambient-token-must-not-be-used" {
			t.Fatal("an explicit --account fell back to the ambient environment token")
		}
	}
}

// Two differently selected operations running at once must never cross
// identities: the credential is per invocation, so it cannot live anywhere two
// goroutines can see it.
func TestInfraAccountConcurrentSelectionsNeverCross(t *testing.T) {
	type call struct{ token, account string }
	var mu sync.Mutex
	var calls []call

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		tok := strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer ")
		mu.Lock()
		calls = append(calls, call{token: tok})
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"reserved_ips": []any{}})
	}))
	defer srv.Close()

	t.Setenv("DIGITALOCEAN_API_URL", srv.URL)
	t.Setenv("DIGITALOCEAN_ACCESS_TOKEN", "")
	teams := twoTeams()
	teamResolver(t, teams)

	const rounds = 12
	var wg sync.WaitGroup
	errs := make(chan error, rounds*2)
	for i := 0; i < rounds; i++ {
		for _, alias := range []string{"owl", "otter"} {
			wg.Add(1)
			go func(alias string) {
				defer wg.Done()
				cmd := fleet.NewInfraCmd()
				var out, errb bytes.Buffer
				cmd.SetOut(&out)
				cmd.SetErr(&errb)
				cmd.SetArgs([]string{"ip", "list", "--account", alias, "--json"})
				if err := cmd.Execute(); err != nil {
					errs <- fmt.Errorf("%s: %w", alias, err)
				}
			}(alias)
		}
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent infra ip list: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(calls) != rounds*2 {
		t.Fatalf("got %d requests, want %d", len(calls), rounds*2)
	}
	counts := map[string]int{}
	for _, c := range calls {
		counts[c.token]++
	}
	if counts["token-owl"] != rounds || counts["token-otter"] != rounds {
		t.Fatalf("tokens crossed between accounts: %v", counts)
	}
}

// Two answers to "which credential" is a refusal, and it must land before the
// provider is ever contacted.
func TestInfraAccountConflictsWithCredentialRef(t *testing.T) {
	rec := newDORecorder(t)
	t.Setenv("DIGITALOCEAN_API_URL", rec.srv.URL)
	resolved := 0
	withResolver(t, func(context.Context, string, string) (fleet.AccountSelection, error) {
		resolved++
		return fleet.AccountSelection{Category: fleet.AccountDigitalOcean, Token: "token-owl"}, nil
	})

	_, _, err := runFleetInfra(t, "inspect", "digitalocean", "--name", "owl-1",
		"--account", "owl", "--credential-ref", "do-other")
	if err == nil {
		t.Fatal("--account with --credential-ref must fail")
	}
	if code := fleet.AccountErrorCode(err); code != "account_conflict" {
		t.Fatalf("error code = %q (%v), want account_conflict", code, err)
	}
	if resolved != 0 {
		t.Fatal("the conflict must be detected before the account is resolved")
	}
	if n := len(rec.seen()); n != 0 {
		t.Fatalf("%d provider requests made before the refusal", n)
	}
}

// A provider can only be driven by its own category of account.
func TestInfraAccountUnsupportedCategory(t *testing.T) {
	rec := newDORecorder(t)
	t.Setenv("DIGITALOCEAN_API_URL", rec.srv.URL)
	withResolver(t, func(context.Context, string, string) (fleet.AccountSelection, error) {
		return fleet.AccountSelection{AccountID: "acc_box", Category: fleet.AccountCloudbox, Token: "bearer"}, nil
	})

	_, _, err := runFleetInfra(t, "ip", "list", "--account", "box", "--json")
	if err == nil {
		t.Fatal("a cloudbox account must not drive a DigitalOcean verb")
	}
	if code := fleet.AccountErrorCode(err); code != "account_unsupported" {
		t.Fatalf("error code = %q (%v), want account_unsupported", code, err)
	}
	if n := len(rec.seen()); n != 0 {
		t.Fatalf("%d provider requests made before the refusal", n)
	}
}

// A binary with no account service refuses rather than quietly using whatever
// credential the environment carries.
func TestInfraAccountWithoutWiredService(t *testing.T) {
	rec := newDORecorder(t)
	t.Setenv("DIGITALOCEAN_API_URL", rec.srv.URL)
	t.Setenv("DIGITALOCEAN_ACCESS_TOKEN", "ambient")
	withResolver(t, nil)

	_, _, err := runFleetInfra(t, "ip", "list", "--account", "owl", "--json")
	if err == nil {
		t.Fatal("--account without a wired account service must fail")
	}
	if code := fleet.AccountErrorCode(err); code != "account_unresolved" {
		t.Fatalf("error code = %q (%v), want account_unresolved", code, err)
	}
	if n := len(rec.seen()); n != 0 {
		t.Fatalf("%d provider requests made before the refusal", n)
	}
}

// Without the flag nothing changed: the ambient environment token is used and
// the account service is never consulted.
func TestInfraWithoutAccountIsUnchanged(t *testing.T) {
	rec := newDORecorder(t)
	t.Setenv("DIGITALOCEAN_API_URL", rec.srv.URL)
	t.Setenv("DIGITALOCEAN_ACCESS_TOKEN", "ambient-token")
	consulted := 0
	withResolver(t, func(context.Context, string, string) (fleet.AccountSelection, error) {
		consulted++
		return fleet.AccountSelection{}, fmt.Errorf("must not be called")
	})

	out, _, err := runFleetInfra(t, "ip", "list", "--json")
	if err != nil {
		t.Fatalf("infra ip list: %v", err)
	}
	if consulted != 0 {
		t.Fatal("a run without --account consulted the account service")
	}
	if seen := rec.seen(); len(seen) != 1 || seen[0] != "ambient-token" {
		t.Fatalf("tokens = %v, want [ambient-token]", seen)
	}
	if strings.TrimSpace(out) != "[]" {
		t.Fatalf("output = %q, want the unchanged empty JSON list", strings.TrimSpace(out))
	}
}

// A refusal, and ordinary output, must never carry credential material.
func TestInfraAccountOutputCarriesNoSecret(t *testing.T) {
	rec := newDORecorder(t)
	t.Setenv("DIGITALOCEAN_API_URL", rec.srv.URL)
	withResolver(t, func(context.Context, string, string) (fleet.AccountSelection, error) {
		return fleet.AccountSelection{
			AccountID: "acc_owl", Category: fleet.AccountDigitalOcean,
			Token: accountstest.CanarySecret,
		}, nil
	})

	out, errOut, err := runFleetInfra(t, "ip", "list", "--account", "owl", "--json")
	accountstest.AssertNoSecret(t, out, errOut)
	if err != nil {
		accountstest.AssertNoSecret(t, err.Error())
	}
	// And a refusal path: a selection with no token at all.
	withResolver(t, func(context.Context, string, string) (fleet.AccountSelection, error) {
		return fleet.AccountSelection{AccountID: "acc_owl", Category: fleet.AccountDigitalOcean}, nil
	})
	out, errOut, err = runFleetInfra(t, "ip", "list", "--account", "owl", "--json")
	if err == nil {
		t.Fatal("a selection with no credential must fail")
	}
	if code := fleet.AccountErrorCode(err); code != "credential_unavailable" {
		t.Fatalf("error code = %q (%v), want credential_unavailable", code, err)
	}
	accountstest.AssertNoSecret(t, out, errOut, err.Error())
}

// ResolveCloudAccount is the cloudbox half: an account REPLACES the ladder, and
// an explicit --url/--token/--master alongside it is a conflict, not a
// precedence question.
func TestResolveCloudAccount(t *testing.T) {
	sel := &fleet.AccountSelection{
		Ref: "owl-box", AccountID: "acc_box", Category: fleet.AccountCloudbox,
		Endpoint: "https://owl.example.com/", Token: "bearer-owl",
	}
	base, token, err := fleet.ResolveCloudAccount(sel, "", "")
	if err != nil {
		t.Fatalf("ResolveCloudAccount: %v", err)
	}
	if base != "https://owl.example.com" || token != "bearer-owl" {
		t.Fatalf("got (%q, %q), want the account's endpoint and token", base, token)
	}

	for _, tc := range []struct{ url, tok string }{{"https://other.example.com", ""}, {"", "other"}} {
		if _, _, err := fleet.ResolveCloudAccount(sel, tc.url, tc.tok); fleet.AccountErrorCode(err) != "account_conflict" {
			t.Fatalf("overrides (%q,%q): code = %q, want account_conflict", tc.url, tc.tok, fleet.AccountErrorCode(err))
		}
	}

	wrong := *sel
	wrong.Category = fleet.AccountDigitalOcean
	if _, _, err := fleet.ResolveCloudAccount(&wrong, "", ""); fleet.AccountErrorCode(err) != "account_unsupported" {
		t.Fatalf("a DigitalOcean account must not supply a cloudbox session: %v", err)
	}

	noToken := *sel
	noToken.Token = ""
	if _, _, err := fleet.ResolveCloudAccount(&noToken, "", ""); fleet.AccountErrorCode(err) != "credential_unavailable" {
		t.Fatalf("a tokenless cloudbox account must refuse: %v", err)
	}

	// Without a selection it IS ResolveCloud, overrides and all.
	t.Setenv("BASHY_CLOUDBOX_URL", "")
	base, token, err = fleet.ResolveCloudAccount(nil, "https://plain.example.com", "plain-token")
	if err != nil || base != "https://plain.example.com" || token != "plain-token" {
		t.Fatalf("no selection: got (%q, %q, %v)", base, token, err)
	}
}

// Every verb this story touches must OFFER the flag: bashy's help and its MCP
// tool schema are reflected off these cobra flags, so registering the flag IS
// registering it in the schema.
func TestAccountFlagRegisteredOnTouchedVerbs(t *testing.T) {
	lookup := func(c *cobra.Command) *pflag.Flag {
		if f := c.Flags().Lookup("account"); f != nil {
			return f
		}
		return c.InheritedFlags().Lookup("account")
	}
	for _, tc := range []struct {
		label string
		root  *cobra.Command
		path  []string
	}{
		{"infra provision", fleet.NewInfraCmd(), []string{"provision"}},
		{"infra list", fleet.NewInfraCmd(), []string{"list"}},
		{"infra inspect", fleet.NewInfraCmd(), []string{"inspect"}},
		{"infra destroy", fleet.NewInfraCmd(), []string{"destroy"}},
		{"infra ip list", fleet.NewInfraCmd(), []string{"ip", "list"}},
		{"infra ip attach", fleet.NewInfraCmd(), []string{"ip", "attach"}},
		{"infra ip detach", fleet.NewInfraCmd(), []string{"ip", "detach"}},
		{"infra firewall list", fleet.NewInfraCmd(), []string{"firewall", "list"}},
		{"infra firewall allow", fleet.NewInfraCmd(), []string{"firewall", "allow"}},
		{"infra firewall deny", fleet.NewInfraCmd(), []string{"firewall", "deny"}},
		{"infra harden", fleet.NewInfraCmd(), []string{"harden"}},
		{"infra trash", fleet.NewInfraCmd(), []string{"trash"}},
		{"infra trash ls", fleet.NewInfraCmd(), []string{"trash", "ls"}},
		{"infra restore", fleet.NewInfraCmd(), []string{"restore"}},
		{"infra request ls", fleet.NewInfraCmd(), []string{"request", "ls"}},
		{"infra credential ls", fleet.NewInfraCmd(), []string{"credential", "ls"}},
		{"token create", fleet.NewTokenCmd(), []string{"create"}},
		{"host ls", fleet.NewPairedHostCmd(), []string{"ls"}},
	} {
		c, _, err := tc.root.Find(tc.path)
		if err != nil {
			t.Fatalf("%s not found: %v", tc.label, err)
		}
		if lookup(c) == nil {
			t.Fatalf("%s has no --account flag", tc.label)
		}
	}
}

// The AWS family's extra refusal: an explicit environment identity alongside
// --account is a conflict, because which one wins would otherwise depend on
// which code path read first.
func TestInfraAccountConflictsWithAWSEnvironment(t *testing.T) {
	withResolver(t, func(context.Context, string, string) (fleet.AccountSelection, error) {
		return fleet.AccountSelection{AccountID: "acc_aws", Category: fleet.AccountAWS, Profile: "alice"}, nil
	})
	for _, name := range []string{"AWS_ACCESS_KEY_ID", "AWS_PROFILE"} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("AWS_ACCESS_KEY_ID", "")
			t.Setenv("AWS_PROFILE", "")
			t.Setenv(name, "set-by-the-shell")
			_, _, err := runFleetInfra(t, "inspect", "aws", "--name", "owl-1", "--account", "alice")
			if code := fleet.AccountErrorCode(err); code != "account_conflict" {
				t.Fatalf("code = %q (%v), want account_conflict", code, err)
			}
		})
	}
}

// The cloudbox family end to end: `--account` supplies the endpoint AND the
// bearer for one cloudbox-facing call, and an explicit --master/--token
// alongside it refuses before the call.
func TestInfraCredentialAccountSelectsItsOwnCloudbox(t *testing.T) {
	type req struct{ host, auth string }
	var mu sync.Mutex
	var reqs []req
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		reqs = append(reqs, req{host: r.Host, auth: r.Header.Get("Authorization")})
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"credentials": []any{}})
	})
	owl := httptest.NewServer(handler)
	defer owl.Close()
	otter := httptest.NewServer(handler)
	defer otter.Close()

	// An ambient pairing that must never answer once --account is given.
	t.Setenv("BASHY_CLOUDBOX_URL", "https://ambient.example.com")
	t.Setenv("BASHY_FLEET_TOKEN", "ambient-token")

	boxes := map[string]fleet.AccountSelection{
		"owl":   {AccountID: "acc_owl", Category: fleet.AccountCloudbox, Endpoint: owl.URL, Token: "bearer-owl"},
		"otter": {AccountID: "acc_otter", Category: fleet.AccountSatellite, Endpoint: otter.URL, Token: "bearer-otter"},
	}
	withResolver(t, func(_ context.Context, ref, operation string) (fleet.AccountSelection, error) {
		if operation != "cloudbox" {
			return fleet.AccountSelection{}, fmt.Errorf("unexpected operation %q", operation)
		}
		sel, ok := boxes[ref]
		if !ok {
			return fleet.AccountSelection{}, fmt.Errorf("no account %q", ref)
		}
		return sel, nil
	})

	for alias, srv := range map[string]*httptest.Server{"owl": owl, "otter": otter} {
		if _, _, err := runFleetInfra(t, "credential", "ls", "--account", alias); err != nil {
			t.Fatalf("infra credential ls --account %s: %v", alias, err)
		}
		mu.Lock()
		last := reqs[len(reqs)-1]
		mu.Unlock()
		wantHost := strings.TrimPrefix(srv.URL, "http://")
		if last.host != wantHost {
			t.Fatalf("--account %s reached %q, want %q", alias, last.host, wantHost)
		}
		if last.auth != "Bearer bearer-"+alias {
			t.Fatalf("--account %s presented %q", alias, last.auth)
		}
	}

	before := len(reqs)
	_, _, err := runFleetInfra(t, "credential", "ls", "--account", "owl", "--token", "other")
	if code := fleet.AccountErrorCode(err); code != "account_conflict" {
		t.Fatalf("code = %q (%v), want account_conflict", code, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(reqs) != before {
		t.Fatal("a request was made despite the conflict")
	}
}

// A provider account cannot stand in for a cloudbox session.
func TestCloudboxAccountRejectsProviderCategory(t *testing.T) {
	withResolver(t, func(context.Context, string, string) (fleet.AccountSelection, error) {
		return fleet.AccountSelection{AccountID: "acc_owl", Category: fleet.AccountDigitalOcean, Token: "token-owl"}, nil
	})
	_, _, err := runFleetInfra(t, "credential", "ls", "--account", "owl")
	if code := fleet.AccountErrorCode(err); code != "account_unsupported" {
		t.Fatalf("code = %q (%v), want account_unsupported", code, err)
	}
}
