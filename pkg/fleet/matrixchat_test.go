package fleet

import (
	"regexp"
	"strings"
	"testing"
)

// The optional "Matrix chat" homeserver (Sprint 433): the tuwunel pin is a
// complete, permissively licensed, digest-verified recipe that never staffs
// as an agent CLI, and its service unit is safe by default. No network.
func TestMatrixChatHomeserverPin(t *testing.T) {
	c := baseline(t)
	tl, ok := c.Tool("tuwunel")
	if !ok {
		t.Fatal("baseline tool tuwunel not found")
	}
	if tl.IsCLI() || !tl.Hidden {
		t.Fatalf("tuwunel must be a hidden non-CLI tool, got kind=%q hidden=%v", tl.Kind, tl.Hidden)
	}
	cli, _ := c.Tools(false)
	for _, x := range cli {
		if x.Name == "tuwunel" {
			t.Fatal("tuwunel is listed as a fleet agent CLI")
		}
	}
	m := tl.CLI.Managed
	if m == nil {
		t.Fatal("tuwunel has no managed pin")
	}
	if err := m.Validate(tl.Name); err != nil {
		t.Fatal(err)
	}
	permissive := map[string]bool{"Apache-2.0": true, "MIT": true, "BSD-2-Clause": true, "BSD-3-Clause": true, "ISC": true}
	if !permissive[m.License] || m.LicenseURL == "" || m.Source == "" {
		t.Fatalf("license %q (%s) source %q: want a permissive SPDX id with its URL", m.License, m.LicenseURL, m.Source)
	}
	if len(m.Assets) == 0 {
		t.Fatal("no assets")
	}
	sha := regexp.MustCompile(`^[0-9a-f]{64}$`)
	for k, a := range m.Assets {
		if !sha.MatchString(a.SHA256) {
			t.Errorf("%s: sha256 %q", k, a.SHA256)
		}
		if !strings.HasPrefix(a.URL, "https://github.com/qiangli/tuwunel/releases/download/bashy-v1.9.4-2/") {
			t.Errorf("%s: url %q is not the bashy-signed release page", k, a.URL)
		}
	}
	if _, err := tl.BinmgrTool(); err != nil {
		t.Fatal(err)
	}
}

// No AGPL homeserver may enter the baseline: not as a tool, not in a URL.
func TestBaselineHasNoAGPLHomeserver(t *testing.T) {
	tools, _ := baseline(t).Tools(true)
	for _, tl := range tools {
		blob := strings.ToLower(tl.Name + " " + tl.CLI.Binary)
		if m := tl.CLI.Managed; m != nil {
			for _, a := range m.Assets {
				blob += " " + strings.ToLower(a.URL)
			}
		}
		for _, bad := range []string{"synapse", "dendrite"} {
			if strings.Contains(blob, bad) {
				t.Errorf("tool %s references %s (AGPL)", tl.Name, bad)
			}
		}
	}
	body, err := OptionalService("matrix-chat")
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"synapse", "dendrite"} {
		if strings.Contains(strings.ToLower(string(body)), bad) {
			t.Errorf("matrix-chat unit references %s (AGPL)", bad)
		}
	}
}

// The unit `bashy service add -f` installs: valid, disabled, loopback-only,
// unexposed, registration and federation off, no secrets, data under the
// bashy state dir, and clear of the cloudbox matrix tunnel's names.
func TestMatrixChatServiceUnitIsSafeByDefault(t *testing.T) {
	body, err := OptionalService("matrix-chat")
	if err != nil {
		t.Fatal(err)
	}
	s, err := ParseService("matrix-chat", body, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Validate(); err != nil {
		t.Fatal(err)
	}
	if s.Name != "matrix-chat" || s.Enabled {
		t.Fatalf("name=%q enabled=%v: want matrix-chat, disabled until the skill enables it", s.Name, s.Enabled)
	}
	for _, p := range s.Ports {
		if p.Bind != "" && p.Bind != "loopback" && p.Bind != "127.0.0.1" {
			t.Errorf("port %d binds %q, want loopback", p.Port, p.Bind)
		}
	}
	if s.Expose.App != nil || len(s.Expose.Mesh) != 0 {
		t.Error("matrix-chat must not be exposed by default")
	}
	if len(s.Secrets) != 0 {
		t.Error("matrix-chat declares secrets")
	}
	if s.Env["TUWUNEL_ALLOW_REGISTRATION"] != "false" || s.Env["TUWUNEL_ALLOW_FEDERATION"] != "false" {
		t.Errorf("env %v: registration and federation must be off", s.Env)
	}
	for k := range s.Env {
		if strings.HasPrefix(k, "MATRIX_") {
			t.Errorf("env %s collides with the cloudbox matrix tunnel", k)
		}
	}
	argv, err := s.ExpandExec("")
	if err != nil {
		t.Fatal(err)
	}
	script := strings.Join(argv, "\n")
	if !strings.Contains(script, "${BASHY_HOME:-$HOME/.bashy}/matrix-chat") {
		t.Errorf("data dir is not under the bashy state dir:\n%s", script)
	}
	if !strings.Contains(script, "127.0.0.1") {
		t.Errorf("expanded argv carries no loopback bind:\n%s", script)
	}
	if strings.Contains(strings.ToLower(script), "registration_token") || strings.Contains(script, "/matrix/") {
		t.Errorf("unit carries a token or a matrix-tunnel route:\n%s", script)
	}
}
