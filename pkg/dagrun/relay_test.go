package dagrun

import (
	"encoding/json"
	"strings"
	"testing"
)

// containerOf digs the one container out of a rendered Job or Pod.
func containerOf(t *testing.T, obj Object) map[string]any {
	t.Helper()
	var podSpec any
	switch obj["kind"] {
	case "Job":
		podSpec = dig(t, obj, "spec", "template", "spec")
	case "Pod":
		podSpec = dig(t, obj, "spec")
	default:
		t.Fatalf("object kind %v carries no pod spec", obj["kind"])
	}
	containers, ok := dig(t, podSpec, "containers").([]any)
	if !ok || len(containers) == 0 {
		t.Fatalf("pod spec has no containers: %v", podSpec)
	}
	c, ok := containers[0].(map[string]any)
	if !ok {
		t.Fatalf("container is %T", containers[0])
	}
	return c
}

func commandOf(t *testing.T, c map[string]any) []string {
	t.Helper()
	raw, ok := c["command"].([]any)
	if !ok {
		t.Fatalf("container has no command: %v", c)
	}
	out := make([]string, 0, len(raw))
	for _, a := range raw {
		s, _ := a.(string)
		out = append(out, s)
	}
	return out
}

// envOf splits a container's env into literal values and secretKeyRef
// wirings, so a test can tell "the pod was handed the value" from "the pod
// was handed a reference to it".
func envOf(t *testing.T, c map[string]any) (map[string]string, map[string][2]string) {
	t.Helper()
	literals := map[string]string{}
	refs := map[string][2]string{}
	raw, _ := c["env"].([]any)
	for _, e := range raw {
		entry, ok := e.(map[string]any)
		if !ok {
			t.Fatalf("env entry is %T", e)
		}
		name, _ := entry["name"].(string)
		if vf, ok := entry["valueFrom"].(map[string]any); ok {
			skr, _ := vf["secretKeyRef"].(map[string]any)
			secret, _ := skr["name"].(string)
			key, _ := skr["key"].(string)
			refs[name] = [2]string{secret, key}
			continue
		}
		v, _ := entry["value"].(string)
		literals[name] = v
	}
	return literals, refs
}

func relayCases() []Relay {
	return []Relay{
		{Name: "code-db", Mode: RelayModeTCP, Upstream: "https://sat.example.com/matrix/h/host-a/app/code-db/", LocalPort: 5432},
		{Name: "web", Mode: RelayModeHTTP, Upstream: "https://sat.example.com/matrix/h/host-a/app/web/", LocalPort: 18080, Expires: "2026-10-21T00:00:00Z"},
	}
}

// RED (S487.2): with relays on the manifest, the rendered container's PID 1
// is the relay wrapper, the list rides $DKS_RELAYS as plain JSON, and each
// token rides a secretKeyRef into the expose's own Secret — never a literal
// value and never argv.
func TestRenderPrefixesRelayWrapperAndUsesSecretKeyRefs(t *testing.T) {
	for _, kind := range []string{"one-shot", "interactive"} {
		t.Run(kind, func(t *testing.T) {
			in := renderCases()[kind]
			in.Relays = relayCases()
			objs, err := Render(mustGenerate(t, in), linuxClass())
			if err != nil {
				t.Fatalf("Render: %v", err)
			}
			c := containerOf(t, objs[0])

			// PID 1 is the wrapper; what used to be PID 1 is now its child,
			// untouched (the plain `bashy -c` form and the session-server
			// form both survive the prefix).
			cmd := commandOf(t, c)
			want := []string{"bashy", "dks", "relay", "--all", "--"}
			if len(cmd) <= len(want) {
				t.Fatalf("command %q is not a wrapped command", cmd)
			}
			for i, w := range want {
				if cmd[i] != w {
					t.Fatalf("command %q is not prefixed with %q", cmd, want)
				}
			}
			bare, err := Render(mustGenerate(t, renderCases()[kind]), linuxClass())
			if err != nil {
				t.Fatal(err)
			}
			if got, wantChild := strings.Join(cmd[len(want):], "\x00"), strings.Join(commandOf(t, containerOf(t, bare[0])), "\x00"); got != wantChild {
				t.Errorf("wrapped child = %q, want the unwrapped command %q", got, wantChild)
			}

			literals, refs := envOf(t, c)
			var gotRelays []Relay
			if err := json.Unmarshal([]byte(literals[RelaysEnv]), &gotRelays); err != nil {
				t.Fatalf("$%s is not a relay list: %v (%q)", RelaysEnv, err, literals[RelaysEnv])
			}
			if len(gotRelays) != 2 || gotRelays[0].Name != "code-db" || gotRelays[1].Name != "web" {
				t.Errorf("$%s = %+v, want the two relays in name order", RelaysEnv, gotRelays)
			}
			if gotRelays[0].LocalPort != 5432 || gotRelays[1].LocalPort != 18080 {
				t.Errorf("$%s lost the planned local ports: %+v", RelaysEnv, gotRelays)
			}
			if strings.Contains(literals[RelaysEnv], ExposeTokenKey) {
				t.Errorf("$%s mentions the token key: %q", RelaysEnv, literals[RelaysEnv])
			}

			for _, r := range relayCases() {
				ref, ok := refs[r.TokenEnv()]
				if !ok {
					t.Fatalf("container does not receive $%s (literals %v, refs %v)", r.TokenEnv(), literals, refs)
				}
				if ref[0] != ExposeSecretName(r.Name) || ref[1] != ExposeTokenKey {
					t.Errorf("$%s is wired to %v, want {%s %s}", r.TokenEnv(), ref, ExposeSecretName(r.Name), ExposeTokenKey)
				}
				if _, literal := literals[r.TokenEnv()]; literal {
					t.Errorf("$%s is a literal value, not a secretKeyRef", r.TokenEnv())
				}
			}
		})
	}
}

// RED (S487.2): with no relays nothing changes at all — no wrapper, no
// relay variables, and the goldens of TestRenderLinuxGolden stand.
func TestRenderWithoutRelaysIsUnchanged(t *testing.T) {
	for name, in := range renderCases() {
		t.Run(name, func(t *testing.T) {
			objs, err := Render(mustGenerate(t, in), linuxClass())
			if err != nil {
				t.Fatalf("Render: %v", err)
			}
			c := containerOf(t, objs[0])
			if cmd := commandOf(t, c); cmd[0] != "bashy" || (len(cmd) > 2 && cmd[1] == "dks" && cmd[2] == "relay") {
				t.Errorf("command %q is wrapped without any relay", cmd)
			}
			literals, refs := envOf(t, c)
			if _, ok := literals[RelaysEnv]; ok {
				t.Errorf("$%s is set without any relay", RelaysEnv)
			}
			for n := range refs {
				if strings.HasPrefix(n, RelayTokenEnvPrefix) {
					t.Errorf("env %s is wired without any relay", n)
				}
			}
		})
	}
}

// RED (S487.2): a run Env: line may not shadow a relay-owned variable, or a
// pod could be handed a token-shaped literal in place of its secretKeyRef.
func TestRenderRelayVariablesAreNotShadowedByRunEnv(t *testing.T) {
	in := renderCases()["one-shot"]
	in.Relays = relayCases()
	in.Env = map[string]string{
		RelaysEnv:                    "[]",
		RelayTokenEnvName("web"):     "not-a-token",
		RelayTokenEnvName("code-db"): "not-a-token",
		"LANG":                       "C.UTF-8",
	}
	objs, err := Render(mustGenerate(t, in), linuxClass())
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	literals, refs := envOf(t, containerOf(t, objs[0]))
	if literals[RelaysEnv] == "[]" {
		t.Errorf("$%s was overwritten by a run Env: line", RelaysEnv)
	}
	for _, r := range relayCases() {
		if got := literals[r.TokenEnv()]; got != "" {
			t.Errorf("$%s is the literal %q from a run Env: line", r.TokenEnv(), got)
		}
		if _, ok := refs[r.TokenEnv()]; !ok {
			t.Errorf("$%s lost its secretKeyRef", r.TokenEnv())
		}
	}
	if literals["LANG"] != "C.UTF-8" {
		t.Errorf("an ordinary run variable was dropped: %v", literals)
	}
}

// RED (S487.2): an unusable relay list refuses the manifest rather than
// leaving the pod with a listener pointed somewhere unexpected.
func TestRelayListValidation(t *testing.T) {
	ok := Relay{Name: "web", Mode: RelayModeHTTP, Upstream: "https://sat.example.com/app/web/", LocalPort: 18080}
	for _, tc := range []struct {
		name  string
		relay Relay
	}{
		{"bad-name", Relay{Name: "Web", Mode: RelayModeHTTP, Upstream: ok.Upstream, LocalPort: 18080}},
		{"bad-mode", Relay{Name: "web", Mode: "socks", Upstream: ok.Upstream, LocalPort: 18080}},
		{"bad-upstream", Relay{Name: "web", Mode: RelayModeHTTP, Upstream: "ftp://x/", LocalPort: 18080}},
		{"no-port", Relay{Name: "web", Mode: RelayModeHTTP, Upstream: ok.Upstream}},
		{"bad-expiry", Relay{Name: "web", Mode: RelayModeHTTP, Upstream: ok.Upstream, LocalPort: 18080, Expires: "soon"}},
	} {
		if err := tc.relay.Validate(); err == nil {
			t.Errorf("%s: Validate accepted %+v", tc.name, tc.relay)
		}
		in := renderCases()["one-shot"]
		in.Relays = []Relay{tc.relay}
		if _, err := Generate(in); err == nil {
			t.Errorf("%s: Generate accepted %+v", tc.name, tc.relay)
		}
	}
	if err := ValidateRelays([]Relay{ok, ok}); err == nil {
		t.Error("ValidateRelays accepted one name twice")
	}
	other := ok
	other.Name = "other"
	if err := ValidateRelays([]Relay{ok, other}); err == nil {
		t.Error("ValidateRelays accepted two relays on one port")
	}
}
