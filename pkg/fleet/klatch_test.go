package fleet

import (
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
	if m.Version != "1.2.1-bashy.1" {
		t.Fatalf("version=%q, want 1.2.1-bashy.1", m.Version)
	}
	const release = "https://github.com/qiangli/klatch/releases/download/bashy-v1.2.1-1/"
	const identity = "https://github.com/qiangli/klatch/.github/workflows/bashy-build.yml@refs/tags/bashy-v1.2.1-1"
	if m.Sigstore.Identity != identity || m.Sigstore.Issuer != "https://token.actions.githubusercontent.com" {
		t.Fatalf("wrong signing identity: %+v", m.Sigstore)
	}
	assets := map[string]struct{ file, sha256 string }{
		"linux/amd64":  {"klatch-v1.2.1-linux-amd64.tar.gz", "413d987f172faa82bfbb2a8d443cbe843cd310cd6f276ba6800102802310c726"},
		"linux/arm64":  {"klatch-v1.2.1-linux-arm64.tar.gz", "bff7dfa16ef614bd014cbaee49421e31bcebab44e1200fa3a5af28c45c9bb5bb"},
		"darwin/amd64": {"klatch-v1.2.1-darwin-amd64.tar.gz", "d285020a6ecc4066efb22db5d9393bebb84cdb9ed53105fc45bddda0b7630459"},
		"darwin/arm64": {"klatch-v1.2.1-darwin-arm64.tar.gz", "94baaa72bb1f004906ceeadbf1310264232cd42596dcac9ab8f62703e8585a5d"},
	}
	for platform, want := range assets {
		a, ok := m.Assets[platform]
		if !ok || a.SHA256 != want.sha256 || a.Member != "klatch" || a.URL != release+want.file {
			t.Errorf("%s: invalid signed archive pin: %+v", platform, a)
		}
	}
	if len(m.Assets) != len(assets) {
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
