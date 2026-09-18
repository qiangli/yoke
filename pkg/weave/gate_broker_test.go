package weave

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/agentpty"
)

func TestClassifyGate(t *testing.T) {
	tests := []struct {
		name string
		tail string
		kind GateKind
		url  string
	}{
		{
			name: "claude trust",
			tail: "Do you trust the contents of this folder?\n1. Yes, proceed\n2. No, exit",
			kind: GateTrust,
		},
		{
			name: "codex trust",
			tail: "Do you trust this directory?\n1) Yes\n2) No",
			kind: GateTrust,
		},
		{
			name: "agy browser oauth",
			tail: "You are currently not signed in.\nOpen https://accounts.google.com/o/oauth2/v2/auth?client_id=abc to sign in.",
			kind: GateBrowserOAuth,
			url:  "https://accounts.google.com/o/oauth2/v2/auth?client_id=abc",
		},
		{
			name: "codex browser login",
			tail: "Login required. Visit https://auth.openai.com/login?state=xyz and complete authentication.",
			kind: GateBrowserOAuth,
			url:  "https://auth.openai.com/login?state=xyz",
		},
		{
			name: "gh device code",
			tail: "First copy your one-time code: 1A2B-3C4D\nThen press Enter to open github.com in your browser...\nEnter the code at https://github.com/login/device",
			kind: GateDeviceCode,
			url:  "https://github.com/login/device",
		},
		{
			name: "gcloud device code",
			tail: "Go to the following link in your browser:\nhttps://www.google.com/device\nEnter the code: AB12-CD34",
			kind: GateDeviceCode,
			url:  "https://www.google.com/device",
		},
		{
			name: "claude api key",
			tail: "Authentication failed: API key not set. Set ANTHROPIC_API_KEY and retry.",
			kind: GateAPIKey,
		},
		{
			name: "opencode api key",
			tail: "Error: no api key found for provider.",
			kind: GateAPIKey,
		},
		{
			name: "human login no url",
			tail: "Please log in to continue. Run `login` from an interactive terminal.",
			kind: GateHuman,
		},
		{
			name: "unauthorized no route",
			tail: "Authentication required before continuing.",
			kind: GateHuman,
		},
		{
			name: "plain question is none",
			tail: "Do you want me to update the tests as well, or keep the change scoped?",
			kind: GateNone,
		},
		{
			name: "working output is none",
			tail: "go test ./pkg/weave/...\nok  github.com/qiangli/yoke/pkg/weave  0.243s\nWriting implementation notes.",
			kind: GateNone,
		},
		{
			name: "ordinary url is none",
			tail: "Fetched https://example.com/callbacks.md while researching webhook callback behavior.",
			kind: GateNone,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := classifyGate(tt.tail)
			if got.Kind != tt.kind {
				t.Fatalf("kind = %q, want %q (signature %q)", got.Kind, tt.kind, got.Signature)
			}
			if got.URL != tt.url {
				t.Fatalf("url = %q, want %q", got.URL, tt.url)
			}
			if tt.kind != GateNone && got.Signature == "" {
				t.Fatalf("signature is empty for %q", tt.kind)
			}
		})
	}
}

func TestRouteGateRoutesByKind(t *testing.T) {
	tests := []struct {
		name       string
		verdict    GateVerdict
		browserErr error
		action     string
		wantSay    string
		wantURL    string
		wantEsc    string
	}{
		{
			name:    "none",
			verdict: GateVerdict{Kind: GateNone},
			action:  "none",
		},
		{
			name:    "trust says clear payload",
			verdict: GateVerdict{Kind: GateTrust, Signature: "do you trust"},
			action:  "say_trust",
			wantSay: agentpty.GateTrustClearPayload,
		},
		{
			name:    "browser oauth opens browser login",
			verdict: GateVerdict{Kind: GateBrowserOAuth, URL: "https://auth.example.com/oauth/authorize", Signature: "login required"},
			action:  "browser_login",
			wantURL: "https://auth.example.com/oauth/authorize",
		},
		{
			name:       "browser oauth escalates on browser failure",
			verdict:    GateVerdict{Kind: GateBrowserOAuth, URL: "https://auth.example.com/login", Signature: "login required"},
			browserErr: errors.New("no browser"),
			action:     "escalate",
			wantURL:    "https://auth.example.com/login",
			wantEsc:    "browser OAuth login failed",
		},
		{
			name:    "device code escalates",
			verdict: GateVerdict{Kind: GateDeviceCode, URL: "https://github.com/login/device", Signature: "enter the code"},
			action:  "escalate",
			wantEsc: "device-code gate",
		},
		{
			name:    "api key escalates",
			verdict: GateVerdict{Kind: GateAPIKey, Signature: "api key not set"},
			action:  "escalate",
			wantEsc: "API key gate",
		},
		{
			name:    "human escalates",
			verdict: GateVerdict{Kind: GateHuman, Signature: "authentication required"},
			action:  "escalate",
			wantEsc: "interactive auth gate",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var said, browserURL, escalated string
			action, err := routeGate(tt.verdict, routeDeps{
				State: &gateRouteState{},
				Say: func(payload string) error {
					said = payload
					return nil
				},
				BrowserLogin: func(url string) error {
					browserURL = url
					return tt.browserErr
				},
				Escalate: func(msg string) error {
					escalated = msg
					return nil
				},
			})
			if err != nil {
				t.Fatalf("routeGate returned error: %v", err)
			}
			if action != tt.action {
				t.Fatalf("action = %q, want %q", action, tt.action)
			}
			if said != tt.wantSay {
				t.Fatalf("said = %q, want %q", said, tt.wantSay)
			}
			if browserURL != tt.wantURL {
				t.Fatalf("browserURL = %q, want %q", browserURL, tt.wantURL)
			}
			if tt.wantEsc != "" && !strings.Contains(escalated, tt.wantEsc) {
				t.Fatalf("escalation = %q, want substring %q", escalated, tt.wantEsc)
			}
			if tt.wantEsc == "" && escalated != "" {
				t.Fatalf("unexpected escalation: %q", escalated)
			}
		})
	}
}

func TestRouteGateDedupe(t *testing.T) {
	state := &gateRouteState{}
	calls := 0
	deps := routeDeps{
		State: state,
		Say: func(payload string) error {
			calls++
			return nil
		},
		Escalate: func(msg string) error { return nil },
	}
	verdict := GateVerdict{Kind: GateTrust, Signature: "do you trust"}

	action, err := routeGate(verdict, deps)
	if err != nil {
		t.Fatalf("first route error: %v", err)
	}
	if action != "say_trust" || calls != 1 {
		t.Fatalf("first route action=%q calls=%d, want say_trust and 1", action, calls)
	}
	action, err = routeGate(verdict, deps)
	if err != nil {
		t.Fatalf("second route error: %v", err)
	}
	if action != "dedupe" || calls != 1 {
		t.Fatalf("second route action=%q calls=%d, want dedupe and 1", action, calls)
	}
}

func TestRouteGateAutoRouteCapEscalates(t *testing.T) {
	state := &gateRouteState{}
	var said, escalated int
	deps := routeDeps{
		State:        state,
		AutoRouteCap: 1,
		Say: func(payload string) error {
			said++
			return nil
		},
		Escalate: func(msg string) error {
			escalated++
			if !strings.Contains(msg, "cap reached") {
				t.Fatalf("escalation message = %q, want cap reached", msg)
			}
			return nil
		},
	}

	action, err := routeGate(GateVerdict{Kind: GateTrust, Signature: "do you trust issue 1"}, deps)
	if err != nil {
		t.Fatalf("first route error: %v", err)
	}
	if action != "say_trust" || said != 1 || escalated != 0 {
		t.Fatalf("first route action=%q said=%d escalated=%d", action, said, escalated)
	}
	action, err = routeGate(GateVerdict{Kind: GateTrust, Signature: "do you trust issue 2"}, deps)
	if err != nil {
		t.Fatalf("second route error: %v", err)
	}
	if action != "escalate" || said != 1 || escalated != 1 {
		t.Fatalf("second route action=%q said=%d escalated=%d", action, said, escalated)
	}
}

func TestGateBrokerObserveTailDebouncesAndRoutesOnce(t *testing.T) {
	now := time.Unix(100, 0)
	var said []string
	b := newGateBroker(routeDeps{
		State: &gateRouteState{},
		Say: func(payload string) error {
			said = append(said, payload)
			return nil
		},
		Escalate: func(msg string) error { return nil },
	}, time.Second)
	tail := "Do you trust the contents of this folder?\n1. Yes, proceed\n2. No"

	_, action, err := b.ObserveTail(tail, now)
	if err != nil {
		t.Fatalf("first observe error: %v", err)
	}
	if action != "debounce" || len(said) != 0 {
		t.Fatalf("first observe action=%q said=%d, want debounce and 0", action, len(said))
	}
	_, action, err = b.ObserveTail(tail, now.Add(500*time.Millisecond))
	if err != nil {
		t.Fatalf("second observe error: %v", err)
	}
	if action != "debounce" || len(said) != 0 {
		t.Fatalf("second observe action=%q said=%d, want debounce and 0", action, len(said))
	}
	_, action, err = b.ObserveTail(tail, now.Add(time.Second))
	if err != nil {
		t.Fatalf("third observe error: %v", err)
	}
	if action != "say_trust" || len(said) != 1 {
		t.Fatalf("third observe action=%q said=%d, want say_trust and 1", action, len(said))
	}
	_, action, err = b.ObserveTail(tail, now.Add(2*time.Second))
	if err != nil {
		t.Fatalf("fourth observe error: %v", err)
	}
	if action != "dedupe" || len(said) != 1 {
		t.Fatalf("fourth observe action=%q said=%d, want dedupe and 1", action, len(said))
	}
}

func TestGateBrokerObserveTailCapEscalates(t *testing.T) {
	now := time.Unix(200, 0)
	var said, escalated int
	b := newGateBroker(routeDeps{
		State:        &gateRouteState{},
		AutoRouteCap: 1,
		Say: func(payload string) error {
			said++
			return nil
		},
		BrowserLogin: func(url string) error { return nil },
		Escalate: func(msg string) error {
			escalated++
			if !strings.Contains(msg, "cap reached") {
				t.Fatalf("escalation = %q, want cap reached", msg)
			}
			return nil
		},
	}, time.Second)

	first := "Do you trust this directory?\n1) Yes\n2) No"
	second := "Login required. Visit https://auth.example.com/oauth/authorize?client_id=abc to continue."
	if _, _, err := b.ObserveTail(first, now); err != nil {
		t.Fatalf("first observe error: %v", err)
	}
	actionVerdict, action, err := b.ObserveTail(first, now.Add(time.Second))
	if err != nil {
		t.Fatalf("first route error: %v", err)
	}
	if actionVerdict.Kind != GateTrust || action != "say_trust" || said != 1 || escalated != 0 {
		t.Fatalf("first route kind=%q action=%q said=%d escalated=%d", actionVerdict.Kind, action, said, escalated)
	}
	if _, _, err := b.ObserveTail(second, now.Add(2*time.Second)); err != nil {
		t.Fatalf("second observe error: %v", err)
	}
	actionVerdict, action, err = b.ObserveTail(second, now.Add(3*time.Second))
	if err != nil {
		t.Fatalf("second route error: %v", err)
	}
	if actionVerdict.Kind != GateBrowserOAuth || action != "escalate" || said != 1 || escalated != 1 {
		t.Fatalf("second route kind=%q action=%q said=%d escalated=%d", actionVerdict.Kind, action, said, escalated)
	}
}

func TestWeaveWaitBrokerFlagExists(t *testing.T) {
	cmd := NewWeaveCmd()
	wait, _, err := cmd.Find([]string{"wait"})
	if err != nil {
		t.Fatalf("find wait: %v", err)
	}
	if wait == nil {
		t.Fatal("wait command not found")
	}
	if wait.Flags().Lookup("broker") == nil {
		t.Fatal("wait --broker flag not found")
	}
}
