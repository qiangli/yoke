package principal

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/fleet/fleettest"
	"github.com/qiangli/yoke/pkg/room"
)

// testEnv is a fully hermetic host: no DNS, no ssh_config, no pairing.
// Tests opt facts back in one at a time.
func testEnv(t *testing.T) Env {
	t.Helper()
	return Env{
		LookupHost: func(context.Context, string) ([]string, error) {
			return nil, errors.New("no resolver in tests")
		},
		LocalUser: "localguy",
		Hostname:  "this-box",
	}
}

func testResolver(t *testing.T, env Env, opts ...fleet.Option) (*Resolver, *fleet.Catalog) {
	t.Helper()
	opts = append([]fleet.Option{fleet.WithRoot(t.TempDir())}, opts...)
	cat := fleet.New(opts...)
	return NewResolver(cat, env), cat
}

// --- identifier grammar ---------------------------------------------------

func TestURNRoundTrip(t *testing.T) {
	for _, tt := range []struct{ kind, name, owner, want string }{
		{"agent", "007", "", "dhnt:agent/007"},
		{"agent", "007", LocalOwner, "dhnt:agent/007"},
		{"person", "alice", "alice@example.com", "dhnt:person/alice@alice@example.com"},
		{"host", "host-a", "alice@example.com", "dhnt:host/host-a@alice@example.com"},
	} {
		got := URN(Kind(tt.kind), tt.name, tt.owner)
		if got != tt.want {
			t.Errorf("URN(%s,%s,%s) = %q, want %q", tt.kind, tt.name, tt.owner, got, tt.want)
		}
		k, n, _, err := ParseURN(got)
		if err != nil || string(k) != tt.kind || n != tt.name {
			t.Errorf("ParseURN(%q) = %v,%v,%v", got, k, n, err)
		}
	}
}

func TestParseURNRejectsJunk(t *testing.T) {
	for _, s := range []string{"", "007", "dhnt:", "dhnt:wat/x", "dhnt:agent/"} {
		if _, _, _, err := ParseURN(s); err == nil {
			t.Errorf("ParseURN(%q) = nil error", s)
		}
	}
}

// The colon trap: `agent:007` is a typed principal, `codex:deepseek-v4` is a
// tool:model binding. Handing the former to a tool:model helper would report
// the tool as "agent".
func TestSplitQueryDoesNotMistakeABindingForAKind(t *testing.T) {
	if k, n := SplitQuery("agent:007"); k != KindAgent || n != "007" {
		t.Fatalf("SplitQuery(agent:007) = %q,%q", k, n)
	}
	// "codex" is not a kind keyword, so the whole string stays the name and
	// the agent resolver gets to interpret it as a binding.
	if k, n := SplitQuery("codex:deepseek-v4"); k != "" || n != "codex:deepseek-v4" {
		t.Fatalf("SplitQuery(codex:deepseek-v4) = %q,%q — a binding must not be read as a typed kind", k, n)
	}
	if k, n := SplitQuery("007"); k != "" || n != "007" {
		t.Fatalf("SplitQuery(007) = %q,%q", k, n)
	}
}

// --- resolution -----------------------------------------------------------

func TestResolveAgentByNicknameAndAlias(t *testing.T) {
	fleettest.Ring(t) // `fable` is the family alias of the ring's fable5
	r, cat := testResolver(t, testEnv(t))
	if err := cat.SaveAgent(fleet.Agent{
		Name: "007", Aliases: []string{"smarty", "bond"}, Tool: "claude", Model: "fable",
	}); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{"007", "smarty", "bond", "agent:007"} {
		ans := r.Resolve(q)
		if !ans.Resolved || ans.Matches[0].Name != "007" {
			t.Fatalf("Resolve(%q) did not land on 007: %+v", q, ans)
		}
		if ans.Matches[0].Summary != "claude:fable5" {
			t.Fatalf("Resolve(%q) summary = %q", q, ans.Matches[0].Summary)
		}
	}
}

func TestResolveAgentProjectsLiveTakenClaim(t *testing.T) {
	t.Setenv("BASHY_ROOM_DIR", t.TempDir())
	r, cat := testResolver(t, testEnv(t))
	if err := cat.SaveAgent(fleet.Agent{Name: "claimed", Tool: "claude", Model: "fable"}); err != nil {
		t.Fatal(err)
	}
	if err := room.Join(room.Card{ID: "claimed", Nick: "claimed", Mode: "inbox", PID: os.Getpid(), Binding: "claude:fable"}); err != nil {
		t.Fatal(err)
	}
	ans := r.Resolve("agent:claimed")
	if !ans.Resolved || len(ans.Matches) != 1 {
		t.Fatalf("resolution = %+v", ans)
	}
	claim := ans.Matches[0].Claim
	if claim == nil || claim.State != "TAKEN" || claim.ID != "claimed" || claim.Mode != "inbox" || claim.PID != os.Getpid() {
		t.Fatalf("claim = %+v", claim)
	}
	found := false
	for _, fact := range ans.Matches[0].Facts {
		if fact[0] == "claim" && strings.Contains(fact[1], "TAKEN id=claimed mode=inbox") {
			found = true
		}
	}
	if !found {
		t.Fatalf("human claim fact missing: %+v", ans.Matches[0].Facts)
	}
}

func TestResolveDottedAgentProjectsCanonicalAndLegacyTakenClaims(t *testing.T) {
	for _, tc := range []struct {
		name, cardID string
	}{
		{name: "canonical", cardID: room.AgentClaimID("claimed.agent")},
		{name: "legacy raw", cardID: "claimed.agent"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("BASHY_ROOM_DIR", t.TempDir())
			r, cat := testResolver(t, testEnv(t))
			if err := cat.SaveAgent(fleet.Agent{Name: "claimed.agent", Tool: "claude", Model: "fable"}); err != nil {
				t.Fatal(err)
			}
			if err := room.Join(room.Card{ID: tc.cardID, Nick: "claimed.agent", Mode: "inbox", PID: os.Getpid(), Binding: "claude:fable"}); err != nil {
				t.Fatal(err)
			}
			ans := r.Resolve("agent:claimed.agent")
			if !ans.Resolved || len(ans.Matches) != 1 || ans.Matches[0].Claim == nil || ans.Matches[0].Claim.State != "TAKEN" || ans.Matches[0].Claim.ID != tc.cardID {
				t.Fatalf("dotted claim resolution = %+v", ans)
			}
		})
	}
}

func TestResolveUnknownName(t *testing.T) {
	r, _ := testResolver(t, testEnv(t))
	if ans := r.Resolve("nobody-here"); ans.Resolved {
		t.Fatalf("resolved a name that names nothing: %+v", ans)
	}
}

// A bare name matching two kinds is ambiguity to surface, not to guess at.
func TestAmbiguousNameAcrossKinds(t *testing.T) {
	r, cat := testResolver(t, testEnv(t))
	if err := cat.SaveAgent(fleet.Agent{Name: "atlas", Tool: "claude", Model: "opus"}); err != nil {
		t.Fatal(err)
	}
	if err := cat.SavePerson(fleet.Person{Handle: "atlas"}); err != nil {
		t.Fatal(err)
	}
	ans := r.Resolve("atlas")
	if !ans.Ambiguous() {
		t.Fatalf("expected ambiguity, got %d match(es)", len(ans.Matches))
	}
	// Qualifying breaks the tie.
	if got := r.Resolve("person:atlas"); got.Ambiguous() || got.Matches[0].Kind != KindPerson {
		t.Fatalf("person:atlas = %+v", got)
	}
	if got := r.Resolve("agent:atlas"); got.Ambiguous() || got.Matches[0].Kind != KindAgent {
		t.Fatalf("agent:atlas = %+v", got)
	}
}

// --- Self ------------------------------------------------------------------

func TestSelfPrefersMintedPrincipal(t *testing.T) {
	t.Setenv("BASHY_PRINCIPAL", "dhnt:agent/007")
	t.Setenv("BASHY_AGENT_ID", "ignored")
	r, _ := testResolver(t, testEnv(t))
	ref, ok := r.Self()
	if !ok || ref.Kind != KindAgent || ref.Name != "007" {
		t.Fatalf("Self = %+v", ref)
	}
}

func TestSelfFallsBackToLauncherNickname(t *testing.T) {
	t.Setenv("BASHY_PRINCIPAL", "")
	t.Setenv("BASHY_AGENT_ID", "007")
	r, _ := testResolver(t, testEnv(t))
	ref, ok := r.Self()
	if !ok || ref.Kind != KindAgent || ref.Name != "007" || ref.URN != "dhnt:agent/007" {
		t.Fatalf("Self = %+v", ref)
	}
}

// Detection yields a TOOL identity, never a fabricated nickname: a bare tool
// is not an agent.
func TestSelfDetectionYieldsToolNotAgent(t *testing.T) {
	t.Setenv("BASHY_PRINCIPAL", "")
	t.Setenv("BASHY_AGENT_ID", "")
	t.Setenv("BASHY_AGENT", "")
	t.Setenv("WEAVE_AGENT", "")
	t.Setenv("AGENT", "")
	t.Setenv("AI_AGENT", "")
	t.Setenv("CLAUDECODE", "1")
	r, _ := testResolver(t, testEnv(t))
	ref, ok := r.Self()
	if !ok || ref.Kind != KindTool || ref.Name != "claude" {
		t.Fatalf("Self = %+v, want the claude TOOL identity", ref)
	}
}

// The name-valued conventions (AGENT=amp, Vercel's AI_AGENT) carry a harness
// name directly, and detection honors them — but they still yield a TOOL, not
// a fabricated agent nickname.
func TestSelfHonorsTheNameValuedConvention(t *testing.T) {
	// Clear all higher-priority ambient detector variables to isolate the
	// test, matching TestSelfUnattributed's exhaustive list.
	for _, k := range []string{"BASHY_PRINCIPAL", "BASHY_AGENT_ID", "BASHY_AGENT", "WEAVE_AGENT",
		"CLAUDECODE", "CLAUDE_CODE_ENTRYPOINT", "CODEX_SANDBOX", "CODEX_THREAD_ID",
		"OPENCODE_CLIENT", "GEMINI_CLI", "CURSOR_AGENT", "CURSOR_TRACE_ID", "GOOSE_TERMINAL", "CLINE_ACTIVE",
		"AGENT"} {
		t.Setenv(k, "")
	}
	t.Setenv("AI_AGENT", "amp")
	r, _ := testResolver(t, testEnv(t))
	ref, ok := r.Self()
	if !ok || ref.Kind != KindTool || ref.Name != "amp" {
		t.Fatalf("Self = %+v, want the amp TOOL identity", ref)
	}
}

func TestSelfUnattributed(t *testing.T) {
	// AGENT / AI_AGENT are the name-valued detection conventions: they carry a
	// harness name directly. Detection honors them, so an "unattributed"
	// process must have them unset too.
	for _, k := range []string{"BASHY_PRINCIPAL", "BASHY_AGENT_ID", "BASHY_AGENT", "WEAVE_AGENT",
		"CLAUDECODE", "CLAUDE_CODE_ENTRYPOINT", "CODEX_SANDBOX", "CODEX_THREAD_ID",
		"OPENCODE_CLIENT", "GEMINI_CLI", "CURSOR_AGENT", "CURSOR_TRACE_ID", "GOOSE_TERMINAL", "CLINE_ACTIVE",
		"AGENT", "AI_AGENT"} {
		t.Setenv(k, "")
	}
	r, _ := testResolver(t, testEnv(t))
	if _, ok := r.Self(); ok {
		t.Fatal("an unattributed process must not claim an identity")
	}
}

// --- host reach ------------------------------------------------------------

// lanEnv resolves only <name>.local — the shape of an mDNS answer.
func lanEnv(t *testing.T, onLAN bool) Env {
	e := testEnv(t)
	e.LookupHost = func(_ context.Context, name string) ([]string, error) {
		if onLAN && strings.HasSuffix(name, ".local") {
			return []string{"192.0.2.10"}, nil
		}
		return nil, errors.New("no such host")
	}
	return e
}

func TestHostOnLANIsObservedAndSSHIsLive(t *testing.T) {
	r, _ := testResolver(t, lanEnv(t, true))
	ans := r.Resolve("host-a")
	if !ans.Resolved {
		t.Fatal("a host answering mdns must resolve")
	}
	res := ans.Matches[0]
	best, ok := res.Best()
	if !ok {
		t.Fatal("no live contact")
	}
	if best.Method != "mdns" && best.Method != "ssh" {
		t.Fatalf("best contact = %+v, want a direct method", best)
	}
	if best.Confidence != Observed {
		t.Fatalf("a same-network answer is observed, not %q", best.Confidence)
	}
}

// The roaming case: the same host, off the LAN, with no other evidence. The
// ladder must not vanish, and the relay must not claim to be live on an
// unpaired machine.
func TestHostOffLANHasNoLiveDirectContact(t *testing.T) {
	r, cat := testResolver(t, lanEnv(t, false))
	if err := cat.SaveHost(fleet.Host{Name: "host-a", Address: "host-a.example.com"}); err != nil {
		t.Fatal(err)
	}
	res := r.Resolve("host-a").Matches[0]

	var sawRelay bool
	for _, c := range res.Contacts {
		if c.Method == "relay" {
			sawRelay = true
			if c.Live {
				t.Error("an unpaired machine has no relay")
			}
		}
		if c.Method == "mdns" {
			t.Error("mdns contact offered for a host that is not on the network")
		}
	}
	if !sawRelay {
		t.Error("the ladder must still list the relay, unavailable, so a caller can see why")
	}
	// The declared address keeps ssh live even off the LAN.
	if best, ok := res.Best(); !ok || best.Method != "ssh" {
		t.Fatalf("best = %+v, want the declared ssh path", best)
	}
}

// A roam flips which contacts are live. The ladder is recomputed, never
// served from a memo of the previous coordinate.
func TestRoamingReranksTheLadder(t *testing.T) {
	on, _ := testResolver(t, lanEnv(t, true))
	off, _ := testResolver(t, lanEnv(t, false))

	lan := on.Resolve("host-a").Matches[0]
	if _, ok := lan.Best(); !ok {
		t.Fatal("on-LAN host has no live contact")
	}
	var lanHasMDNS bool
	for _, c := range lan.Contacts {
		if c.Method == "mdns" && c.Live {
			lanHasMDNS = true
		}
	}
	if !lanHasMDNS {
		t.Fatal("on-LAN host should offer a live mdns contact")
	}

	remote := off.Resolve("host-a")
	if remote.Resolved {
		for _, c := range remote.Matches[0].Contacts {
			if c.Method == "mdns" && c.Live {
				t.Fatal("mdns still live after the host left the network — the ladder was memoized")
			}
		}
	}
}

// A paired machine can reach a host it cannot see directly.
func TestPairedMachineOffersARelay(t *testing.T) {
	env := lanEnv(t, false)
	env.Paired, env.PairedName = true, "this-box"
	r, cat := testResolver(t, env)
	if err := cat.SaveHost(fleet.Host{Name: "host-a"}); err != nil {
		t.Fatal(err)
	}
	res := r.Resolve("host-a").Matches[0]
	best, ok := res.Best()
	if !ok {
		t.Fatal("a paired machine always has at least the relay")
	}
	if best.Method != "relay" {
		// ssh may also be live; the relay must at minimum exist and be live.
		var relayLive bool
		for _, c := range res.Contacts {
			if c.Method == "relay" && c.Live {
				relayLive = true
			}
		}
		if !relayLive {
			t.Fatalf("paired machine has no live relay: %+v", res.Contacts)
		}
	}
}

// --- the remote username ----------------------------------------------------

// The account name is per-host. The local $USER is the LAST rung, and using
// it must be visible as a guess.
func TestSSHUserLadder(t *testing.T) {
	env := lanEnv(t, true)
	r, cat := testResolver(t, env)

	// Rung 4: nothing known → the local user, marked as a guess.
	res := r.Resolve("host-z").Matches[0]
	ssh := contactOf(t, res, "ssh")
	if !strings.Contains(ssh.Address, "localguy@") {
		t.Fatalf("expected the local user as the last resort: %q", ssh.Address)
	}
	if ssh.Confidence != Assumed || !strings.Contains(ssh.Why, "may not exist there") {
		t.Fatalf("a guessed account must say so: %+v", ssh)
	}

	// Rung 1: an explicit alias binding wins and is declared.
	if err := cat.SaveHost(fleet.Host{Name: "host-a", SSHUser: "al", SSHPort: 2222}); err != nil {
		t.Fatal(err)
	}
	res = r.Resolve("host-a").Matches[0]
	ssh = contactOf(t, res, "ssh")
	if !strings.Contains(ssh.Address, "al@") || !strings.Contains(ssh.Address, ":2222") {
		t.Fatalf("alias ssh_user/port ignored: %q", ssh.Address)
	}
	if strings.Contains(ssh.Why, "may not exist there") {
		t.Fatalf("a declared account must not be reported as a guess: %+v", ssh)
	}
}

// Rung 2: ~/.ssh/config is the convention users already maintain, and it is
// authoritative when present.
func TestSSHConfigSuppliesUserAndHostName(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config")
	if err := os.WriteFile(cfg, []byte(strings.Join([]string{
		"Host bastion",
		"  User root",
		"",
		"Host host-a host-a.internal",
		"  HostName 10.0.0.5",
		"  User al",
		"  Port 2200",
		"",
		"Host *",
		"  User catchall",
	}, "\n")), 0o600); err != nil {
		t.Fatal(err)
	}
	env := lanEnv(t, false)
	env.SSHConfig = cfg
	r, _ := testResolver(t, env)

	res := r.Resolve("host-a").Matches[0]
	ssh := contactOf(t, res, "ssh")
	if ssh.Address != "ssh://al@10.0.0.5:2200" {
		t.Fatalf("ssh address = %q, want ssh://al@10.0.0.5:2200", ssh.Address)
	}
	if !ssh.Live || ssh.Source != "ssh_config" || ssh.Confidence != Declared {
		t.Fatalf("ssh_config contact = %+v", ssh)
	}
}

// Rung 3: a person's per-host binding, when ssh_config is silent.
func TestPersonPerHostAccountUsed(t *testing.T) {
	env := lanEnv(t, true)
	r, cat := testResolver(t, env)
	if err := cat.SavePerson(fleet.Person{
		Handle: "localguy", OSUsers: map[string]string{"host-a": "al"},
	}); err != nil {
		t.Fatal(err)
	}
	res := r.Resolve("host-a").Matches[0]
	ssh := contactOf(t, res, "ssh")
	if !strings.Contains(ssh.Address, "al@") {
		t.Fatalf("per-host account binding ignored: %q", ssh.Address)
	}
	if ssh.Confidence == Assumed {
		t.Fatalf("a declared per-host account is not a guess: %+v", ssh)
	}
}

func TestSSHConfigWildcardMatch(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config")
	if err := os.WriteFile(cfg, []byte("Host *.example.com\n  User wild\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got := readSSHConfig(cfg, "box.example.com")
	if !got.Found || got.User != "wild" {
		t.Fatalf("wildcard Host pattern not matched: %+v", got)
	}
}

func TestSSHConfigEqualsForm(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config")
	if err := os.WriteFile(cfg, []byte("Host=host-a\nUser=al\nPort=2022\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got := readSSHConfig(cfg, "host-a")
	if !got.Found || got.User != "al" || got.Port != 2022 {
		t.Fatalf("Key=value form not parsed: %+v", got)
	}
}

func contactOf(t *testing.T, r Resolution, method string) Contact {
	t.Helper()
	for _, c := range r.Contacts {
		if c.Method == method {
			return c
		}
	}
	t.Fatalf("no %q contact in %+v", method, r.Contacts)
	return Contact{}
}

// --- ranking ---------------------------------------------------------------

// Rank, never collapse: an on-LAN host is reachable both directly and by
// relay, and both must survive in the ladder.
func TestRankOrdersLiveObservedFirstWithoutDroppingOthers(t *testing.T) {
	cs := []Contact{
		{Method: "relay", Live: true, Confidence: Inferred, Cost: 50},
		{Method: "ssh", Live: false, Confidence: Observed, Cost: 10},
		{Method: "mdns", Live: true, Confidence: Observed, Cost: 5},
		{Method: "lan", Live: true, Confidence: Declared, Cost: 1},
	}
	rankContacts(cs)
	if cs[0].Method != "mdns" {
		t.Fatalf("observed live contact must rank first, got %q", cs[0].Method)
	}
	if cs[1].Method != "lan" {
		t.Fatalf("declared live beats inferred live, got %q", cs[1].Method)
	}
	if cs[2].Method != "relay" {
		t.Fatalf("inferred live beats any dead contact, got %q", cs[2].Method)
	}
	if len(cs) != 4 || cs[3].Method != "ssh" {
		t.Fatalf("a dead contact is demoted, never dropped: %+v", cs)
	}
}

// --- mentions ---------------------------------------------------------------

func TestMentionsGrammar(t *testing.T) {
	got := Mentions("cc @007 and @person:alice, mail user@example.com, escape @@literal")
	if len(got) != 2 {
		t.Fatalf("got %d mentions, want 2: %+v", len(got), got)
	}
	if got[0].Name != "007" || got[0].Kind != "" {
		t.Errorf("first = %+v", got[0])
	}
	if got[1].Name != "alice" || got[1].Kind != KindPerson {
		t.Errorf("second = %+v", got[1])
	}
}

// An email address must not be read as a mention of "example".
func TestMentionsIgnoreEmailAddresses(t *testing.T) {
	for _, m := range Mentions("write to alice@example.com") {
		if m.Name == "example.com" || m.Name == "example" {
			t.Fatalf("an email local-part boundary was treated as a mention: %+v", m)
		}
	}
}

func TestCheckMentionsWarnsOnUnknownAndAmbiguous(t *testing.T) {
	r, cat := testResolver(t, testEnv(t))
	if err := cat.SaveAgent(fleet.Agent{Name: "atlas", Tool: "claude", Model: "opus"}); err != nil {
		t.Fatal(err)
	}
	if err := cat.SavePerson(fleet.Person{Handle: "atlas"}); err != nil {
		t.Fatal(err)
	}
	if err := cat.SaveAgent(fleet.Agent{Name: "007", Tool: "claude", Model: "fable"}); err != nil {
		t.Fatal(err)
	}

	bad := r.CheckMentions("@007 is fine, @atlas is ambiguous, @ghost is unknown")
	if len(bad) != 2 {
		t.Fatalf("got %d unresolved, want 2: %+v", len(bad), bad)
	}
	byName := map[string]string{}
	for _, u := range bad {
		byName[u.Name] = u.Why
	}
	if !strings.Contains(byName["atlas"], "2 kinds") {
		t.Errorf("atlas: %q", byName["atlas"])
	}
	if !strings.Contains(byName["ghost"], "names nothing") {
		t.Errorf("ghost: %q", byName["ghost"])
	}
}

// Expanding a mention is what makes "@007 commented" legible to the next
// agent that reads the page.
func TestExpandResolvesMentionsInline(t *testing.T) {
	fleettest.Ring(t)
	r, cat := testResolver(t, testEnv(t))
	if err := cat.SaveAgent(fleet.Agent{Name: "007", Tool: "claude", Model: "fable"}); err != nil {
		t.Fatal(err)
	}
	got := r.Expand("@007 commented on the gate")
	if !strings.Contains(got, "@007 (agent, claude:fable5)") {
		t.Fatalf("Expand = %q", got)
	}
	// An unresolvable mention is left exactly as written.
	if got := r.Expand("@ghost said hi"); got != "@ghost said hi" {
		t.Fatalf("Expand rewrote an unresolvable mention: %q", got)
	}
}

// --- regressions --------------------------------------------------------

// A `Host *` catch-all in a real ssh_config matches every string. Treating a
// wildcard match as evidence of existence made every typo — and every agent
// nickname — resolve as a host.
func TestWildcardSSHConfigStanzaIsNotExistenceEvidence(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config")
	if err := os.WriteFile(cfg, []byte("Host *\n  User catchall\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	env := lanEnv(t, false)
	env.SSHConfig = cfg
	r, _ := testResolver(t, env)

	if ans := r.Resolve("007"); ans.Resolved {
		t.Fatalf("a Host * stanza made %q resolve as a host: %+v", "007", ans.Matches)
	}
	// But a real host still picks up the wildcard's User. (Close the handle
	// immediately — a leaked *os.File blocks t.TempDir's RemoveAll cleanup on
	// Windows, where an open file "is being used by another process.")
	fh, err := os.OpenFile(cfg, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	fh.Close()
	_, cat := testResolver(t, env)
	if err := cat.SaveHost(fleet.Host{Name: "host-a"}); err != nil {
		t.Fatal(err)
	}
	r2 := NewResolver(cat, env)
	res := r2.Resolve("host-a")
	if !res.Resolved {
		t.Fatal("an aliased host must still resolve")
	}
	ssh := contactOf(t, res.Matches[0], "ssh")
	if !strings.Contains(ssh.Address, "catchall@") {
		t.Fatalf("wildcard stanza must still supply User: %q", ssh.Address)
	}
}

// A nickname is not a host just because ssh would happily try to dial it.
func TestAgentNicknameDoesNotResolveAsAHost(t *testing.T) {
	r, cat := testResolver(t, lanEnv(t, false))
	if err := cat.SaveAgent(fleet.Agent{Name: "007", Tool: "claude", Model: "fable"}); err != nil {
		t.Fatal(err)
	}
	ans := r.Resolve("007")
	if ans.Ambiguous() {
		t.Fatalf("007 must name only the agent, got %v", ans.Kinds())
	}
	if ans.Matches[0].Kind != KindAgent {
		t.Fatalf("kind = %q", ans.Matches[0].Kind)
	}
}

// A trailing period ends a sentence, not a name.
func TestMentionNameStopsBeforeTrailingPunctuation(t *testing.T) {
	got := Mentions("ping @smarty. also @gpt-5.5 and @kimi-k2.7-code, done")
	if len(got) != 3 {
		t.Fatalf("got %d mentions: %+v", len(got), got)
	}
	if got[0].Name != "smarty" {
		t.Errorf("trailing period swallowed: %q", got[0].Name)
	}
	// Interior dots are part of the name and must survive.
	if got[1].Name != "gpt-5.5" {
		t.Errorf("interior dot lost: %q", got[1].Name)
	}
	if got[2].Name != "kimi-k2.7-code" {
		t.Errorf("interior dot lost: %q", got[2].Name)
	}
	// Raw must shrink with Name so Expand splices at the right offset.
	if got[0].Raw != "@smarty" {
		t.Errorf("Raw = %q, want @smarty", got[0].Raw)
	}
}

// The mdns contact must carry a dialable name, not a URL: `ssh $(… --reach)`
// has to work on it verbatim.
func TestMDNSContactAddressIsDialable(t *testing.T) {
	r, _ := testResolver(t, lanEnv(t, true))
	res := r.Resolve("host-a").Matches[0]
	c := contactOf(t, res, "mdns")
	if strings.Contains(c.Address, "://") {
		t.Fatalf("mdns address %q is a URL, not something a client can dial", c.Address)
	}
	if c.Address != "host-a.local" {
		t.Fatalf("mdns address = %q", c.Address)
	}
}

// --reach --method picks the ssh target even when mdns ranks higher.
func TestPickContactByMethod(t *testing.T) {
	r, _ := testResolver(t, lanEnv(t, true))
	res := r.Resolve("host-a").Matches[0]

	best, ok := pickContact(res, "")
	if !ok || best.Method != "mdns" {
		t.Fatalf("unrestricted best = %+v, want mdns", best)
	}
	ssh, ok := pickContact(res, "ssh")
	if !ok || ssh.Method != "ssh" {
		t.Fatalf("method-restricted pick = %+v", ssh)
	}
	if got := reachArg(ssh); strings.Contains(got, "://") {
		t.Fatalf("reachArg(ssh) = %q; ssh takes user@host, not a URL", got)
	}
	if _, ok := pickContact(res, "carrier-pigeon"); ok {
		t.Fatal("an unknown method must not match")
	}
}

// getaddrinfo accepts `007` as the legacy octal address 0.0.0.7, so a plain
// lookup "succeeds" for a nickname. A hostname contains a letter.
func TestNumericNameIsNotAHostname(t *testing.T) {
	for _, n := range []string{"007", "42", "1.2.3"} {
		if looksLikeHostname(n) {
			t.Errorf("looksLikeHostname(%q) = true", n)
		}
	}
	for _, n := range []string{"host-a", "host-a.local", "a1"} {
		if !looksLikeHostname(n) {
			t.Errorf("looksLikeHostname(%q) = false", n)
		}
	}

	// A resolver that answers EVERYTHING (an NXDOMAIN-hijacking ISP, or
	// getaddrinfo's numeric parsing) must still not turn 007 into a host.
	env := testEnv(t)
	env.LookupHost = func(context.Context, string) ([]string, error) {
		return []string{"0.0.0.7"}, nil
	}
	r, cat := testResolver(t, env)
	if err := cat.SaveAgent(fleet.Agent{Name: "007", Tool: "claude", Model: "fable"}); err != nil {
		t.Fatal(err)
	}
	ans := r.Resolve("007")
	if ans.Ambiguous() {
		t.Fatalf("007 resolved as %v; a nickname is not a machine", ans.Kinds())
	}
	if ans.Matches[0].Kind != KindAgent {
		t.Fatalf("kind = %q", ans.Matches[0].Kind)
	}
	// A real hostname still resolves through the same permissive resolver.
	if got := r.Resolve("host-z"); !got.Resolved || got.Matches[0].Kind != KindHost {
		t.Fatalf("host-z = %+v", got)
	}
}
