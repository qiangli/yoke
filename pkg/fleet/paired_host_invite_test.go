package fleet

// `host invite` — the pair-once identity code of wire contract §3 (S606
// story 748f23cc0790), tested against the public fake in
// pkg/fleet/cloudboxfake: the account Bearer POSTs /api/v1/hosts/invites on
// the master (no token exchange, no satellite scope), the verb prints
// {code, expires_at}, and the minted code pairs exactly one host to the
// account that minted it. The help and output must keep that ownership
// explicit so an owner-scoped code is never mistaken for a member
// invitation.

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/fleet/cloudboxfake"
)

func runPairedHostInvite(t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmd := NewPairedHostCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(append([]string{"invite"}, args...))
	err := cmd.Execute()
	return out.String(), err
}

// The minted code is a pair-once identity code: redeeming it via
// POST /api/v1/hosts/pair registers the host to the MINTER's account.
func TestPairedHostInviteMintsPairOnceCode(t *testing.T) {
	f := cloudboxfake.New(t)
	owner := f.AddOwner("alice@example.com")

	out, err := runPairedHostInvite(t, "--master", f.URL(), "--token", owner)
	if err != nil {
		t.Fatalf("host invite: %v (%s)", err, out)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 2 {
		t.Fatalf("output = %q, want the code line and the ownership note", out)
	}
	codeLine := regexp.MustCompile(`\t`).Split(lines[0], 2)
	if len(codeLine) != 2 {
		t.Fatalf("code line = %q, want CODE<TAB>EXPIRES", lines[0])
	}
	code, expires := codeLine[0], codeLine[1]
	if !regexp.MustCompile(`^PAIR-\d{4}$`).MatchString(code) {
		t.Fatalf("code = %q, want the fake's PAIR-#### shape", code)
	}
	if !regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z$`).MatchString(expires) {
		t.Fatalf("expires = %q, want RFC 3339 UTC", expires)
	}
	if !strings.Contains(lines[1], "account that minted it") || !strings.Contains(lines[1], "member") {
		t.Fatalf("ownership note missing: %q", lines[1])
	}
	if strings.Contains(out, owner) {
		t.Fatalf("token leaked in output: %q", out)
	}

	// Redeeming the code pairs a host OWNED BY THE MINTER.
	c := OwnerClient{BaseURL: f.URL(), Token: owner}
	var pair struct {
		Host struct {
			OwnerEmail string `json:"owner_email"`
		} `json:"host"`
		Token string `json:"token"`
	}
	if err := c.request(context.Background(), "POST", "/api/v1/hosts/pair",
		map[string]string{"code": code, "host": "studio"}, &pair); err != nil {
		t.Fatalf("pair with minted code: %v", err)
	}
	if pair.Host.OwnerEmail != "alice@example.com" {
		t.Fatalf("paired host owner = %q, want the minting account", pair.Host.OwnerEmail)
	}

	// Pair-once: the code is spent, a second host cannot use it.
	if err := c.request(context.Background(), "POST", "/api/v1/hosts/pair",
		map[string]string{"code": code, "host": "other"}, &pair); err == nil ||
		!strings.Contains(err.Error(), "pair code already used") {
		t.Fatalf("reused code err = %v, want code_consumed", err)
	}
}

func TestPairedHostInviteJSONShape(t *testing.T) {
	f := cloudboxfake.New(t)
	owner := f.AddOwner("alice@example.com")

	out, err := runPairedHostInvite(t, "--master", f.URL(), "--token", owner, "--json")
	if err != nil {
		t.Fatalf("host invite --json: %v (%s)", err, out)
	}
	var res map[string]any
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("not JSON: %v (%s)", err, out)
	}
	if len(res) != 2 {
		t.Fatalf("fields = %v, want exactly {code, expires_at}", res)
	}
	if code, _ := res["code"].(string); !regexp.MustCompile(`^PAIR-\d{4}$`).MatchString(code) {
		t.Fatalf("json code = %v", res["code"])
	}
	if exp, _ := res["expires_at"].(string); !regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T`).MatchString(exp) {
		t.Fatalf("json expires_at = %v", res["expires_at"])
	}
}

func TestPairedHostInviteDryRunSendsNothing(t *testing.T) {
	// A closed server: any request would fail, so success proves none was
	// sent.
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	srv.Close()

	out, err := runPairedHostInvite(t, "--master", srv.URL, "--token", "tok", "--dry-run")
	if err != nil {
		t.Fatalf("dry-run: %v (%s)", err, out)
	}
	if strings.TrimSpace(out) != "POST /api/v1/hosts/invites" {
		t.Fatalf("dry-run output = %q", out)
	}

	jout, err := runPairedHostInvite(t, "--master", srv.URL, "--token", "tok", "--dry-run", "--json")
	if err != nil {
		t.Fatalf("dry-run --json: %v (%s)", err, jout)
	}
	var res map[string]any
	if err := json.Unmarshal([]byte(jout), &res); err != nil {
		t.Fatalf("dry-run --json is not JSON: %v (%s)", err, jout)
	}
	if res["dry_run"] != true || res["method"] != "POST" || res["path"] != "/api/v1/hosts/invites" {
		t.Fatalf("dry-run --json = %v", res)
	}
}

// The master invite needs the hosts:write owner scope on the account
// Bearer: a scoped token without it is refused, and the refusal names the
// scope — no token exchange happens (that is the satellite verb's shape).
func TestPairedHostInviteNeedsOwnerScope(t *testing.T) {
	f := cloudboxfake.New(t)
	limited := f.AddToken("limited@example.com", cloudboxfake.ScopeMembersRead)

	_, err := runPairedHostInvite(t, "--master", f.URL(), "--token", limited)
	if err == nil {
		t.Fatal("limited token minted a pair code")
	}
	if !strings.Contains(err.Error(), "insufficient_scope") || !strings.Contains(err.Error(), "hosts:write") {
		t.Fatalf("err = %v, want insufficient_scope naming hosts:write", err)
	}
}

// Help keeps the two invitation lanes apart: this is owner node
// provisioning for the minter's own hardware, never a member invitation.
func TestPairedHostInviteHelpSeparatesMemberLane(t *testing.T) {
	root := NewPairedHostCmd()
	invite, _, err := root.Find([]string{"invite"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"OWNED BY THE ACCOUNT THAT",
		"owner node provisioning",
		"member invite",
		"outpost satellite attach",
		"Never hand this code",
	} {
		if !strings.Contains(invite.Long, want) {
			t.Fatalf("help missing %q:\n%s", want, invite.Long)
		}
	}
}

// A paired host's token carries only the host scope and cannot mint; like
// the member verbs, an unselected credential prefers the saved master
// credential for this account ladder.
func TestPairedHostInvitePrefersSavedMasterCredential(t *testing.T) {
	isolatedFleetRoot(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	f := cloudboxfake.New(t)
	owner := f.AddOwner("alice@example.com")
	t.Setenv("BASHY_FLEET_TOKEN", "")
	t.Setenv("BASHY_API_KEY", "")

	if _, err := writeMasterTokenFile(owner, f.URL()); err != nil {
		t.Fatalf("seed master token: %v", err)
	}
	out, err := runPairedHostInvite(t, "--master", f.URL())
	if err != nil {
		t.Fatalf("host invite with saved credential: %v (%s)", err, out)
	}
	if !strings.Contains(out, "PAIR-") {
		t.Fatalf("no code minted: %q", out)
	}
}
