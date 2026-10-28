package fleet

import (
	"regexp"
	"strings"
	"testing"
)

func TestKlatchManagedServicePin(t *testing.T) {
	c := baseline(t)
	tl, ok := c.Tool("klatch")
	if !ok || tl.IsCLI() || !tl.Hidden {
		t.Fatalf("klatch must be a hidden system tool: found=%v kind=%q hidden=%v", ok, tl.Kind, tl.Hidden)
	}
	m := tl.CLI.Managed
	if m == nil || !m.Service {
		t.Fatal("klatch service pin is missing")
	}
	if err := m.Validate(tl.Name); err != nil {
		t.Fatal(err)
	}
	if m.Unverified() || m.License != "Apache-2.0" || m.LicenseURL == "" {
		t.Fatal("klatch needs a signed, licensed service pin")
	}
	const release = "https://github.com/qiangli/klatch/releases/download/bashy-v1.2.0-3/"
	const identity = "https://github.com/qiangli/klatch/.github/workflows/bashy-build.yml@refs/tags/bashy-v1.2.0-3"
	if m.Sigstore.Identity != identity || m.Sigstore.Issuer != "https://token.actions.githubusercontent.com" {
		t.Fatalf("wrong signing identity: %+v", m.Sigstore)
	}
	sha := regexp.MustCompile(`^[0-9a-f]{64}$`)
	for _, platform := range []string{"linux/amd64", "linux/arm64", "darwin/amd64", "darwin/arm64"} {
		a, ok := m.Assets[platform]
		if !ok || !sha.MatchString(a.SHA256) || a.Member != "klatch" || !strings.HasPrefix(a.URL, release) || !strings.HasSuffix(a.URL, ".tar.gz") {
			t.Errorf("%s: invalid signed archive pin: %+v", platform, a)
		}
	}
	if len(m.Assets) != 4 {
		t.Errorf("assets=%d, want four platforms", len(m.Assets))
	}
	if _, err := tl.BinmgrTool(); err != nil {
		t.Fatal(err)
	}
}

func TestKlatchOptionalServiceUnit(t *testing.T) {
	body, err := OptionalService("klatch")
	if err != nil {
		t.Fatal(err)
	}
	s, err := ParseService("klatch", body, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Validate(); err != nil {
		t.Fatal(err)
	}
	if s.Name != "klatch" || s.Enabled || s.Tool != "klatch" {
		t.Fatalf("unsafe unit identity: name=%q enabled=%v tool=%q", s.Name, s.Enabled, s.Tool)
	}
	if len(s.Ports) != 1 || s.Ports[0].Port != 7100 || s.Ports[0].Bind != "127.0.0.1" {
		t.Fatalf("want only loopback port 7100: %+v", s.Ports)
	}
	if s.Expose.App != nil || len(s.Expose.Mesh) != 0 || len(s.Secrets) != 0 {
		t.Fatal("optional unit must have no exposure or secrets")
	}
	argv, err := s.ExpandExec("")
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(argv, "\n")
	for _, want := range []string{"${BASHY_HOME:-$HOME/.bashy}/klatch", "127.0.0.1:$5", "-s \"$data\"", "-w \"$web\"", "-t \"$transform\""} {
		if !strings.Contains(joined, want) {
			t.Errorf("expanded exec missing %q: %s", want, joined)
		}
	}
}
