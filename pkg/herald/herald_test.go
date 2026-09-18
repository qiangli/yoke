package herald

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"
)

// fakePeer serves an A2A agent card advertising cardVersion over JSON-RPC, and
// answers SendMessage. It records the A2A-Version header of every non-card
// request, which is the only place herald's declared protocol version becomes
// observable: a2a-go exposes no accessor for the version a client negotiated,
// so the wire is the assertion surface.
func fakePeer(t *testing.T, cardVersion string) (base string, versions func() []string) {
	t.Helper()

	var mu sync.Mutex
	var seen []string
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	mux.HandleFunc(CardPath, func(w http.ResponseWriter, r *http.Request) {
		card := a2a.AgentCard{
			Name:        "fake-peer",
			Description: "a peer that only records what it was told",
			Version:     "0.0.1",
			SupportedInterfaces: []*a2a.AgentInterface{{
				URL:             srv.URL + "/a2a",
				ProtocolBinding: a2a.TransportProtocolJSONRPC,
				ProtocolVersion: a2a.ProtocolVersion(cardVersion),
			}},
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(card); err != nil {
			t.Errorf("encoding card: %v", err)
		}
	})

	mux.HandleFunc("/a2a", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Header.Get(a2a.SvcParamVersion))
		mu.Unlock()

		msg := a2a.NewMessage(a2a.MessageRoleAgent, a2a.NewTextPart("ok"))
		result, err := json.Marshal(a2a.StreamResponse{Event: msg})
		if err != nil {
			t.Errorf("marshalling result: %v", err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(map[string]any{
			"jsonrpc": "2.0",
			"id":      "1",
			"result":  json.RawMessage(result),
		}); err != nil {
			t.Errorf("encoding response: %v", err)
		}
	})

	return srv.URL, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), seen...)
	}
}

// TestA2AVersionOnTheWire is the claim ProtocolVersion exists to make: every
// request herald sends carries A2A-Version: 1.0.
//
// It is asserted end to end, against the header a peer really receives, rather
// than against the constant — the constant agreeing with itself proves nothing,
// and the defect this replaces was precisely a constant with no call site while
// the SDK silently decided the wire.
func TestA2AVersionOnTheWire(t *testing.T) {
	base, versions := fakePeer(t, ProtocolVersion)

	res, err := Send(context.Background(), Peer{Name: "fake", URL: base}, "hello", SendOptions{})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if res.Text != "ok" {
		t.Errorf("Text = %q, want %q", res.Text, "ok")
	}

	got := versions()
	if len(got) == 0 {
		t.Fatal("peer received no A2A request")
	}
	for i, v := range got {
		if v != "1.0" {
			t.Errorf("request %d carried %s: %q, want %q", i, a2a.SvcParamVersion, v, "1.0")
		}
	}
	// And the constant is the thing being sent, not a coincidence.
	if ProtocolVersion != "1.0" {
		t.Errorf("ProtocolVersion = %q, want 1.0", ProtocolVersion)
	}
}

// TestSDKDefaultVersionAgrees is a canary, not a requirement.
//
// a2a-go's own transport defaults register at a2a.Version, so before herald
// stated a version the wire carried whatever that constant said — which today
// is also 1.0. The two agreeing is why the missing call site was invisible: the
// header was right by coincidence, not by declaration.
//
// If an SDK bump breaks this, nothing is wrong: herald keeps sending
// ProtocolVersion, which is the point of owning it. It means a decision is due
// — adopt the new version here, or record why herald stays behind.
func TestSDKDefaultVersionAgrees(t *testing.T) {
	if string(a2a.Version) != ProtocolVersion {
		t.Errorf("a2a-go now defaults to %q, herald declares %q — decide which herald speaks "+
			"and update ProtocolVersion (herald's wire is unchanged either way)",
			a2a.Version, ProtocolVersion)
	}
}

// TestIncompatiblePeerIsRefused pins the other half of owning the version: a
// peer advertising only v0.3 — the wire format the package doc calls out as
// incompatible — must fail at client construction, loudly, rather than be
// negotiated down or talked to in the wrong dialect.
func TestIncompatiblePeerIsRefused(t *testing.T) {
	base, versions := fakePeer(t, "0.3")

	_, err := Send(context.Background(), Peer{Name: "legacy", URL: base}, "hello", SendOptions{})
	if err == nil {
		t.Fatal("a v0.3-only peer must not be dialled")
	}
	if !strings.Contains(err.Error(), ProtocolVersion) {
		t.Errorf("error should name the version herald speaks, got: %v", err)
	}
	if n := len(versions()); n != 0 {
		t.Errorf("peer received %d requests, want 0 — nothing may go on an unnegotiated wire", n)
	}
}

func TestPeerValidate(t *testing.T) {
	cases := []struct {
		name string
		p    Peer
		ok   bool
	}{
		{"good", Peer{Name: "reviewer", URL: "https://a.example.com"}, true},
		{"good http", Peer{Name: "reviewer", URL: "http://127.0.0.1:8080"}, true},
		{"no name", Peer{URL: "https://a.example.com"}, false},
		{"no url", Peer{Name: "reviewer"}, false},
		{"bad scheme", Peer{Name: "reviewer", URL: "ftp://a.example.com"}, false},
		// A colon would collide with the tool:model binding syntax, and a
		// slash would break the on-disk entry name.
		{"colon in name", Peer{Name: "herald:x", URL: "https://a.example.com"}, false},
		{"slash in name", Peer{Name: "a/b", URL: "https://a.example.com"}, false},
		{"space in name", Peer{Name: "a b", URL: "https://a.example.com"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.p.Validate()
			if c.ok && err != nil {
				t.Fatalf("want valid, got %v", err)
			}
			if !c.ok && err == nil {
				t.Fatal("want invalid, got nil")
			}
		})
	}
}

func TestPeerBindingAndCardURL(t *testing.T) {
	p := Peer{Name: "reviewer", URL: "https://a.example.com/"}
	if got, want := p.Binding(), "herald:reviewer"; got != want {
		t.Errorf("Binding() = %q, want %q", got, want)
	}
	// Trailing slash must not produce a doubled slash, and the path must be
	// the v1.0 spelling — /.well-known/agent.json is the pre-v0.2 location
	// and reaching it silently talks the wrong protocol version.
	if got, want := p.CardURL(), "https://a.example.com/.well-known/agent-card.json"; got != want {
		t.Errorf("CardURL() = %q, want %q", got, want)
	}
}

// TestUnrunGateIsNotSuccess is the central invariant of this package.
//
// A2A's COMPLETED is self-reported. If an absent gate could read as success,
// herald would reproduce the exact defect it exists to prevent — the one the
// three-harness A/B measured, where all three harnesses exited 0 on failure.
func TestUnrunGateIsNotSuccess(t *testing.T) {
	o := GateOutcome{}
	if o.Trusted() {
		t.Fatal("an unrun gate must never be trusted")
	}
	// Even explicitly "passed" is meaningless without having run.
	o = GateOutcome{Passed: true}
	if o.Trusted() {
		t.Fatal("Passed without Ran must not be trusted")
	}
	if !strings.Contains(o.Summary(), "UNVERIFIED") {
		t.Errorf("Summary() = %q, want it to say UNVERIFIED", o.Summary())
	}
}

func TestResultSucceededAcceptsUngatedCompletionAsUnverified(t *testing.T) {
	// The peer says it finished. No gate ran: it composes successfully while
	// the Gate outcome continues to identify it as unverified.
	r := Result{State: "completed"}
	if !r.Succeeded() {
		t.Fatal("completed peer work should succeed when no gate was requested")
	}
	if got, want := r.ExitCode(), 0; got != want {
		t.Errorf("ExitCode() = %d, want %d (completed but unverified)", got, want)
	}
	if !strings.Contains(r.Gate.Summary(), "UNVERIFIED") {
		t.Errorf("Gate.Summary() = %q, want UNVERIFIED", r.Gate.Summary())
	}

	// Gate ran and passed: success.
	r = Result{State: "completed", Gate: GateOutcome{Ran: true, Passed: true}}
	if !r.Succeeded() {
		t.Fatal("gate pass must count as success")
	}
	if got := r.ExitCode(); got != 0 {
		t.Errorf("ExitCode() = %d, want 0", got)
	}

	// Peer claims completed, gate disagrees. The gate wins.
	r = Result{State: "completed", Gate: GateOutcome{Ran: true, Passed: false}}
	if r.Succeeded() {
		t.Fatal("gate failure must override a peer's completed claim")
	}
	if got := r.ExitCode(); got == 0 {
		t.Error("ExitCode() must be non-zero when the gate failed")
	}
}

func TestRunLocalGate(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	t.Run("empty gate does not run", func(t *testing.T) {
		o := RunLocalGate(ctx, dir, "  ", "completed")
		if o.Ran {
			t.Fatal("blank gate must not report as run")
		}
		if o.Trusted() {
			t.Fatal("blank gate must not be trusted")
		}
	})

	t.Run("passing gate", func(t *testing.T) {
		o := RunLocalGate(ctx, dir, "exit 0", "completed")
		if !o.Ran || !o.Passed || !o.Trusted() {
			t.Fatalf("want trusted pass, got %+v", o)
		}
		if o.Where != "local" {
			t.Errorf("Where = %q, want local", o.Where)
		}
		if o.PeerClaimed != "completed" {
			t.Errorf("PeerClaimed = %q, want completed", o.PeerClaimed)
		}
	})

	t.Run("failing gate records exit code", func(t *testing.T) {
		o := RunLocalGate(ctx, dir, "exit 7", "completed")
		if !o.Ran {
			t.Fatal("gate should report as run")
		}
		if o.Passed || o.Trusted() {
			t.Fatal("failing gate must not be trusted")
		}
		if o.ExitCode != 7 {
			t.Errorf("ExitCode = %d, want 7", o.ExitCode)
		}
	})

	t.Run("gate runs in the given dir", func(t *testing.T) {
		marker := filepath.Join(dir, "marker")
		if err := os.WriteFile(marker, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		o := RunLocalGate(ctx, dir, "test -f marker", "completed")
		if !o.Trusted() {
			t.Fatalf("gate should see files in dir, got %+v", o)
		}
	})

	t.Run("gate output is captured", func(t *testing.T) {
		o := RunLocalGate(ctx, dir, "echo hello-from-gate; exit 3", "completed")
		if !strings.Contains(o.Output, "hello-from-gate") {
			t.Errorf("Output = %q, want it to contain the gate's output", o.Output)
		}
	})

	t.Run("unrunnable gate is a failure, never a pass", func(t *testing.T) {
		o := RunLocalGate(ctx, dir, "this-command-does-not-exist-anywhere", "completed")
		if o.Passed || o.Trusted() {
			t.Fatal("a gate that cannot run must not pass")
		}
	})
}

func TestCardSupportsGate(t *testing.T) {
	if (Card{}).SupportsGate() {
		t.Error("empty card must not claim gate support")
	}
	c := Card{Extensions: []string{"https://example.com/other", GateExtensionURI}}
	if !c.SupportsGate() {
		t.Error("card declaring the extension URI should report support")
	}
}

func TestBookRoundTrip(t *testing.T) {
	root := t.TempDir()
	b := NewBook(root)

	if peers, err := b.List(); err == nil && len(peers) != 0 {
		t.Fatalf("fresh book should be empty, got %d", len(peers))
	}

	p := Peer{Name: "reviewer", URL: "https://a.example.com", APIKeyRef: "ACME_TOKEN"}
	if err := b.Add(p); err != nil {
		t.Fatalf("Add: %v", err)
	}

	got, err := b.Get("reviewer")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.URL != p.URL {
		t.Errorf("URL = %q, want %q", got.URL, p.URL)
	}
	if got.APIKeyRef != "ACME_TOKEN" {
		t.Errorf("APIKeyRef = %q, want ACME_TOKEN", got.APIKeyRef)
	}
	// A freshly added peer is UNPEGGED. Its card's skills[] are a claim, and
	// recording a self-declared capability is exactly what leveling forbids.
	if got.Band != 0 {
		t.Errorf("Band = %d, want 0 (unpegged until host-measured)", got.Band)
	}

	if err := b.Add(p); err == nil {
		t.Error("adding a duplicate peer should fail")
	}

	peers, err := b.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(peers) != 1 {
		t.Fatalf("List returned %d peers, want 1", len(peers))
	}

	if err := b.Remove("reviewer"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := b.Get("reviewer"); err == nil {
		t.Error("Get should fail after Remove")
	}
}

// TestBookIgnoresNonPeerModels pins the discriminator: the same catalog holds
// LLM models, and offering an inference endpoint as an agent would be wrong.
func TestBookIgnoresNonPeerModels(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "models")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	llm := "name: some-llm\nkind: api\nprovider: openai\nbase_url: https://api.example.com/v1\n"
	if err := os.WriteFile(filepath.Join(dir, "some-llm.yaml"), []byte(llm), 0o644); err != nil {
		t.Fatal(err)
	}

	b := NewBook(root)
	peers, err := b.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, p := range peers {
		if p.Name == "some-llm" {
			t.Fatal("an LLM model must not be listed as an A2A peer")
		}
	}

	// And the error should say WHY, not merely "unknown".
	_, err = b.Get("some-llm")
	if err == nil {
		t.Fatal("Get on a non-peer model should fail")
	}
	if !strings.Contains(err.Error(), "is a model") {
		t.Errorf("error should explain it is a model, got: %v", err)
	}
}

func TestCredential(t *testing.T) {
	if _, ok := Credential(Peer{Name: "x"}); ok {
		t.Error("peer without APIKeyRef should report no credential")
	}
	t.Setenv("HERALD_TEST_TOKEN", "tok")
	v, ok := Credential(Peer{Name: "x", APIKeyRef: "HERALD_TEST_TOKEN"})
	if !ok || v != "tok" {
		t.Errorf("Credential = (%q,%v), want (tok,true)", v, ok)
	}
	if _, ok := Credential(Peer{Name: "x", APIKeyRef: "HERALD_TEST_MISSING"}); ok {
		t.Error("missing env var should report no credential")
	}
}

// TestCLIAddListRemove exercises the verb tree end to end against a temp
// fleet root, so the cobra wiring is covered and not merely the library.
func TestCLIAddListRemove(t *testing.T) {
	root := t.TempDir()
	ctx := context.Background()

	// add: the peer URL is unreachable, which must NOT prevent recording it —
	// a peer that is merely asleep should still be addressable.
	if code := Run(ctx, []string{"add", "reviewer", "https://127.0.0.1:1/", "--fleet-root", root}); code != 0 {
		t.Fatalf("add exited %d, want 0", code)
	}
	if code := Run(ctx, []string{"list", "--fleet-root", root}); code != 0 {
		t.Fatalf("list exited %d, want 0", code)
	}
	// Duplicate must fail loudly rather than silently overwrite.
	if code := Run(ctx, []string{"add", "reviewer", "https://other.example.com", "--fleet-root", root}); code == 0 {
		t.Error("adding a duplicate peer should exit non-zero")
	}
	if code := Run(ctx, []string{"rm", "reviewer", "--fleet-root", root}); code != 0 {
		t.Fatalf("rm exited %d, want 0", code)
	}
	if code := Run(ctx, []string{"rm", "reviewer", "--fleet-root", root}); code == 0 {
		t.Error("removing an unknown peer should exit non-zero")
	}
	// An unknown peer must be a clear error, not a crash.
	if code := Run(ctx, []string{"send", "nobody", "do a thing", "--fleet-root", root}); code == 0 {
		t.Error("sending to an unknown peer should exit non-zero")
	}
}
