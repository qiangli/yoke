package coord

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/bus"
	"github.com/qiangli/yoke/pkg/principal"
	"github.com/qiangli/yoke/pkg/room"
)

func TestClaimNamedResourceCLI(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BASHY_COORD_DIR", dir)
	t.Setenv("BASHY_AGENT_ID", "codex-b")
	t.Setenv(EpisodeEnv, "ep-bbb")

	run := func(args ...string) string {
		t.Helper()
		cmd := NewClaimCmd(func() []string { return []string{"/w/bashy"} })
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetErr(&out)
		cmd.SetArgs(args)
		if err := cmd.Execute(); err != nil {
			t.Fatalf("claim %v: %v\n%s", args, err, out.String())
		}
		return out.String()
	}
	if out := run("do1", "--intent", "leaf replay"); !strings.Contains(out, "do1 held on") {
		t.Fatalf("claim output = %q", out)
	}
	if out := run("list"); !strings.Contains(out, "do1") || !strings.Contains(out, "lease") {
		t.Fatalf("list output = %q", out)
	}
	run("release", "do1")
	claims, err := List(dir)
	if err != nil || len(claims) != 0 {
		t.Fatalf("after release: claims=%#v err=%v", claims, err)
	}
}

func TestClaimChildExitStatusPropagatesAndReleases(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BASHY_COORD_DIR", dir)
	t.Setenv("BASHY_AGENT_ID", "codex-b")
	t.Setenv(EpisodeEnv, "ep-bbb")
	cmd := NewClaimCmd(func() []string { return []string{"/w/bashy"} })
	cmd.SetArgs([]string{"do1", "--", "sh", "-c", "exit 23"})
	err := cmd.Execute()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 23 {
		t.Fatalf("exit = %T %v, want child status 23", err, err)
	}
	c, l, err := AcquireAttached(dir, "do1", agentA(), "after child", 0)
	if err != nil {
		t.Fatalf("child-scoped hold was not released: %v", err)
	}
	if err := ReleaseAttached(dir, c, l); err != nil {
		t.Fatal(err)
	}
}

func TestClaimRequestNotifiesConflictingOwnerWithoutStealing(t *testing.T) {
	coordDir := t.TempDir()
	t.Setenv("BASHY_COORD_DIR", coordDir)
	t.Setenv("BASHY_ROOM_DIR", t.TempDir())
	t.Setenv("BASHY_PRINCIPAL", "")
	t.Setenv("BASHY_AGENT_ID", "codex-b")
	t.Setenv(EpisodeEnv, "ep-bbb")

	if _, err := Acquire(coordDir, []string{"/w/bashy"}, agentA(), "integrating", false); err != nil {
		t.Fatal(err)
	}
	cmd := NewClaimCmd(func() []string { return []string{"/w/bashy"} })
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"request", "-m", "please merge my reviewed commit"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "sent to claude-a") || !strings.Contains(out.String(), "lock remains enforced") {
		t.Fatalf("request output = %q", out.String())
	}
	events, err := room.Timeline(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].To != "claude-a" || events[0].Principal != "codex-b" || events[0].Body != "please merge my reviewed commit" || events[0].Priority != "interrupt" {
		t.Fatalf("request event = %#v", events)
	}
	if _, err := Acquire(coordDir, []string{"/w/bashy"}, agentB(), "write anyway", false); err == nil {
		t.Fatal("request silently stole or released the owner's claim")
	}
}

// cliProvider is a registry whose entries claim under another kind: "appdir"
// is a path claim over /w/app, reachable bare, as the fleet resource noun is.
type cliProvider struct{}

func (cliProvider) Kind() Kind           { return Kind{Name: "clires", Match: MatchName, Domain: "clires"} }
func (cliProvider) Exists(n string) bool { return n == "appdir" }
func (cliProvider) Members(string) ([]string, error) {
	return []string{"/w/app"}, nil
}
func (cliProvider) EffectiveKind(n string) (Kind, bool) {
	if n != "appdir" {
		return Kind{}, false
	}
	return LookupKind("path")
}
func (cliProvider) MapBare(n string) (Ref, bool) {
	if n != "appdir" {
		return Ref{}, false
	}
	return Ref{Kind: "clires", Name: "appdir"}, true
}

// cliEnv points the CLI at a fresh ledger as codex-b.
func cliEnv(t *testing.T) string {
	t.Helper()
	dir := ledger(t)
	t.Setenv("BASHY_ROOM_DIR", t.TempDir())
	t.Setenv("BASHY_PRINCIPAL", "")
	t.Setenv("BASHY_AGENT_ID", "codex-b")
	t.Setenv(EpisodeEnv, "ep-bbb")
	RegisterProvider(cliProvider{})
	return dir
}

// runClaim executes the claim CLI and returns stdout, stderr and the error.
func runClaim(ctx context.Context, args ...string) (string, string, error) {
	cmd := NewClaimCmd(func() []string { return []string{"/w/bashy"} })
	var out, errb bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errb)
	cmd.SetArgs(args)
	err := cmd.ExecuteContext(ctx)
	return out.String(), errb.String(), err
}

func TestClaimTargetForms(t *testing.T) {
	dir := cliEnv(t)
	ctx := context.Background()
	hold := func(args ...string) *Claim {
		t.Helper()
		if _, _, err := runClaim(ctx, args...); err != nil {
			t.Fatalf("claim %v: %v", args, err)
		}
		claims, err := List(dir)
		if err != nil {
			t.Fatal(err)
		}
		if len(claims) != 1 {
			t.Fatalf("claim %v: %d claims", args, len(claims))
		}
		c := claims[0]
		if err := ReleaseRef(ctx, c.Address(), Self(), 0); err != nil {
			t.Fatal(err)
		}
		return c
	}

	// A bare name nothing registers is the ad-hoc name claim, on the same file
	// as before the registry existed.
	if _, _, err := runClaim(ctx, "do1"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(resourceClaimPath(dir, "do1")); err != nil {
		t.Fatalf("bare name is not on the legacy claim file: %v", err)
	}
	if _, _, err := runClaim(ctx, "release", "do1"); err != nil {
		t.Fatal(err)
	}

	if c := hold("name:do2"); c.Kind != KindName || c.Resource != "do2" {
		t.Fatalf("name:do2 = %+v", c)
	}
	if c := hold("path:/w/app"); c.Kind != "path" || len(c.Members) != 1 || c.Members[0] != "/w/app" {
		t.Fatalf("path:/w/app = %+v", c)
	}
	// --kind claims ad hoc, taking the name literally (colons and all).
	if c := hold("--kind", "path", "/w/app"); c.Kind != "path" || c.Resource != "/w/app" {
		t.Fatalf("--kind path /w/app = %+v", c)
	}
	if c := hold("--kind", "thing", "a:b"); c.Kind != "thing" || c.Resource != "a:b" {
		t.Fatalf("--kind thing a:b = %+v", c)
	}
	// A registered resource resolves bare, by its registry kind and members.
	c := hold("appdir")
	if c.Kind != "path" || c.Address().String() != "clires:appdir" || c.Members[0] != "/w/app" {
		t.Fatalf("bare registered resource = %+v", c)
	}
	// Registry-derived kinds take kind:NAME directly.
	if c := hold("clires:appdir"); c.Kind != "path" {
		t.Fatalf("clires:appdir = %+v", c)
	}
	// And no argument is the project: a repo claim over the roots closure.
	if _, _, err := runClaim(ctx); err != nil {
		t.Fatal(err)
	}
	claims, _ := List(dir)
	if len(claims) != 1 || claims[0].Kind != kindRepo || claims[0].Resource != "" || claims[0].Roots[0] != "/w/bashy" {
		t.Fatalf("project claim = %+v", claims)
	}
}

func TestClaimReleaseRefreshEpoch(t *testing.T) {
	cliEnv(t)
	ctx := context.Background()
	if _, _, err := runClaim(ctx, "model:gpt-x"); err != nil {
		t.Fatal(err)
	}
	out, _, err := runClaim(ctx, "refresh", "model:gpt-x", "--epoch", "1")
	if err != nil || !strings.Contains(out, "refreshed model:gpt-x (epoch 1)") {
		t.Fatalf("refresh = %q %v", out, err)
	}
	if _, _, err := runClaim(ctx, "refresh", "model:gpt-x"); err != nil {
		t.Fatalf("epoch 0 is my current claim: %v", err)
	}
	if _, _, err := runClaim(ctx, "refresh", "model:gpt-x", "--epoch", "7"); !errors.Is(err, ErrFenced) {
		t.Fatalf("stale refresh = %v, want ErrFenced", err)
	}
	if _, _, err := runClaim(ctx, "release", "model:gpt-x", "--epoch", "7"); !errors.Is(err, ErrFenced) {
		t.Fatalf("stale release = %v, want ErrFenced", err)
	}
	if _, _, err := runClaim(ctx, "refresh", "nothing-here"); !errors.Is(err, ErrNotHeld) {
		t.Fatalf("refresh of an unheld claim = %v, want ErrNotHeld", err)
	}
	if _, _, err := runClaim(ctx, "release", "model:gpt-x", "--epoch", "1"); err != nil {
		t.Fatal(err)
	}
	// Re-acquiring after a release is a new epoch.
	if out, _, err := runClaim(ctx, "model:gpt-x"); err != nil || !strings.Contains(out, "epoch 2") {
		t.Fatalf("reacquire = %q %v", out, err)
	}
	// Epoch 0 release drops my current claim.
	if _, _, err := runClaim(ctx, "release", "model:gpt-x"); err != nil {
		t.Fatal(err)
	}
	if claims, _ := List(DefaultDir()); len(claims) != 0 {
		t.Fatalf("claims after release: %+v", claims)
	}
}

func TestClaimConflictExitsNineAndPrintsExplanation(t *testing.T) {
	cliEnv(t)
	ctx := context.Background()
	if _, err := AcquireRef(ctx, Request{Ref: ParseRef("do1"), Holder: agentA(), Intent: "leaf replay"}); err != nil {
		t.Fatal(err)
	}
	out, errb, err := runClaim(ctx, "do1")
	var ec interface{ ExitCode() int }
	if !errors.As(err, &ec) || ec.ExitCode() != ExitConflict {
		t.Fatalf("err = %v, want exit %d", err, ExitConflict)
	}
	if out != "" || !strings.Contains(errb, "claude-a already holds name:do1") || !strings.Contains(errb, "bashy claim request name:do1") {
		t.Fatalf("stdout=%q stderr=%q", out, errb)
	}
	out, errb, err = runClaim(ctx, "do1", "--json")
	if !errors.As(err, &ec) || ec.ExitCode() != ExitConflict || errb != "" {
		t.Fatalf("json: err=%v stderr=%q", err, errb)
	}
	var doc struct {
		Schema   string `json:"schema_version"`
		Holder   string
		Resource string
		Kind     string
		Contacts []string
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("not json: %v\n%s", err, out)
	}
	if doc.Schema != "bashy-claim-conflict-v1" || doc.Holder != "claude-a" || doc.Resource != "name:do1" || len(doc.Contacts) == 0 {
		t.Fatalf("doc = %+v", doc)
	}
	// A child-scoped hold refuses the same way.
	if _, err := AcquireRef(ctx, Request{Ref: ParseRef("path:/w/app"), Holder: agentA()}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := runClaim(ctx, "path:/w/app/x", "--", "true"); !errors.As(err, &ec) || ec.ExitCode() != ExitConflict {
		t.Fatalf("attached conflict err = %v", err)
	}
}

func TestClaimRequestRoutesToResourceHolder(t *testing.T) {
	cliEnv(t)
	ctx := context.Background()
	var sent []bus.Notification
	old := claimPublish
	claimPublish = func(n bus.Notification) error { sent = append(sent, n); return nil }
	t.Cleanup(func() { claimPublish = old })

	holder := principal.Ref{Name: "claude-a", Episode: "ep-aaa", Host: "h"}
	if _, err := AcquireRef(ctx, Request{Ref: ParseRef("appdir"), Holder: holder}); err != nil {
		t.Fatal(err)
	}
	// By bare registered name, by kind:NAME, and through a member the claim covers.
	for _, args := range [][]string{
		{"request", "appdir", "-m", "need the app dir"},
		{"request", "clires:appdir", "-m", "need the app dir"},
		{"request", "--kind", "path", "/w/app/sub", "-m", "need the app dir"},
	} {
		sent = nil
		out, _, err := runClaim(ctx, args...)
		if err != nil || !strings.Contains(out, "sent to claude-a") {
			t.Fatalf("%v: out=%q err=%v", args, out, err)
		}
		if len(sent) != 1 || sent[0].To != "claude-a" || sent[0].Principal != "codex-b" ||
			sent[0].Body != "need the app dir" || sent[0].Priority != bus.DeliveryInterrupt {
			t.Fatalf("%v: sent = %+v", args, sent)
		}
	}
	sent = nil
	if _, _, err := runClaim(ctx, "request", "appdir"); err != nil || len(sent) != 1 ||
		!strings.Contains(sent[0].Body, "clires:appdir") {
		t.Fatalf("default message: %+v %v", sent, err)
	}
	// Nobody holds it: nothing is sent.
	sent = nil
	if _, _, err := runClaim(ctx, "request", "do-free"); err == nil || len(sent) != 0 {
		t.Fatalf("request for an unheld target: err=%v sent=%+v", err, sent)
	}
	if _, _, err := runClaim(ctx, "request", "appdir"); err != nil {
		t.Fatal(err)
	}
	// The request never steals.
	if _, err := AcquireRef(ctx, Request{Ref: ParseRef("appdir"), Holder: agentB()}); !isConflict(err) {
		t.Fatalf("request released the holder's claim: %v", err)
	}
}

// shellFields splits a printed command the way a shell would, enough for the
// quoting Contacts emits: single quotes, double quotes, backslash-quote.
func shellFields(s string) []string {
	var out []string
	var cur strings.Builder
	var quote rune
	in := false
	for _, r := range s {
		switch {
		case quote != 0 && r == quote:
			quote = 0
		case quote != 0:
			cur.WriteRune(r)
		case r == '\'' || r == '"':
			quote, in = r, true
		case r == ' ':
			if in {
				out = append(out, cur.String())
				cur.Reset()
				in = false
			}
		default:
			cur.WriteRune(r)
			in = true
		}
	}
	if in {
		out = append(out, cur.String())
	}
	return out
}

// Every `bashy claim ...` line a Conflict prints must parse with the CLI and
// act on the claim it was printed for.
func TestConflictContactsParseWithClaimCmd(t *testing.T) {
	dir := cliEnv(t)
	var sent []bus.Notification
	old := claimPublish
	claimPublish = func(n bus.Notification) error { sent = append(sent, n); return nil }
	t.Cleanup(func() { claimPublish = old })
	ctx := context.Background()

	if _, err := Acquire(dir, []string{"/w/bashy"}, agentA(), "project", false); err != nil {
		t.Fatal(err)
	}
	for _, ref := range []string{"do1", "path:/w/my app", "appdir", "model:gpt-x"} {
		if _, err := AcquireRef(ctx, Request{Ref: ParseRef(ref), Holder: agentA()}); err != nil {
			t.Fatal(err)
		}
	}
	claims, err := List(dir)
	if err != nil || len(claims) != 5 {
		t.Fatalf("claims = %d %v", len(claims), err)
	}
	for _, c := range claims {
		conflict := &Conflict{Claim: c}
		lines := 0
		for _, line := range conflict.Contacts() {
			f := shellFields(line)
			if len(f) < 2 || f[0] != "bashy" || f[1] != "claim" {
				continue
			}
			lines++
			sent = nil
			wctx, cancel := context.WithTimeout(ctx, 250*time.Millisecond)
			_, _, err := runClaim(wctx, f[2:]...)
			cancel()
			switch f[2] {
			case "request":
				if err != nil || len(sent) != 1 || sent[0].To != "claude-a" {
					t.Fatalf("%q: err=%v sent=%+v", line, err, sent)
				}
			default:
				// A wait on a held claim runs until its deadline; anything
				// else is a parse error or a claim on the wrong thing.
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("%q: err=%v, want to wait on the holder", line, err)
				}
			}
		}
		if lines != 2 {
			t.Fatalf("%s: %d claim lines in %q", shown(c), lines, conflict.Contacts())
		}
	}
	// The explanation's own release hint parses too.
	for _, c := range claims {
		if c.Resource == "" {
			continue
		}
		for _, line := range strings.Split((&Conflict{Claim: c}).Error(), "\n") {
			if f := shellFields(strings.TrimSpace(line)); len(f) >= 4 && f[1] == "claim" && f[2] == "release" {
				if want := c.Address().String(); f[3] != want {
					t.Fatalf("release hint %q targets %q, want %q", line, f[3], want)
				}
			}
		}
	}
}

type enumBackend struct {
	*memBackend
	claims []*Claim
}

func (b enumBackend) Claims() ([]*Claim, error) { return b.claims, nil }

func TestClaimListKindEpochAndBackends(t *testing.T) {
	dir := cliEnv(t)
	ctx := context.Background()
	RegisterKind(Kind{Name: "test-clilist", Match: MatchName})
	RegisterBackend("test-clilist", enumBackend{memBackend: &memBackend{m: map[string]*Claim{}}, claims: []*Claim{{
		SchemaVersion: SchemaVersion, Kind: "test-clilist", Resource: "408", Mode: ModeLease, Epoch: 3,
		Holder: agentA(), Intent: "sprint seat", AcquiredAt: time.Now(), Heartbeat: time.Now(),
	}}})
	t.Cleanup(func() { RegisterBackend("test-clilist", nil) })
	if _, _, err := runClaim(ctx, "do1", "--intent", "leaf replay"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := runClaim(ctx, "path:/w/app"); err != nil {
		t.Fatal(err)
	}

	out, _, err := runClaim(ctx, "list", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var rows []struct {
		Kind     string
		Epoch    uint64
		Resource string
	}
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	got := map[string]uint64{}
	for _, r := range rows {
		if r.Kind == "" {
			t.Fatalf("row without kind: %s", out)
		}
		got[r.Kind+":"+r.Resource] = r.Epoch
	}
	if len(rows) != 3 || got["name:do1"] != 1 || got["path:/w/app"] != 1 || got["test-clilist:408"] != 3 {
		t.Fatalf("rows = %v\n%s", got, out)
	}

	out, _, err = runClaim(ctx, "list")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 4 || strings.Fields(lines[0])[1] != "KIND" || !strings.Contains(lines[0], "EPOCH") {
		t.Fatalf("list = %q", out)
	}
	for _, want := range []string{"test-clilist:408", "sprint seat", "do1", "leaf replay"} {
		if !strings.Contains(out, want) {
			t.Fatalf("list lacks %q:\n%s", want, out)
		}
	}
	_ = dir
}
