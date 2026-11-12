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
	if m.Version != "1.2.8-bashy.1" {
		t.Fatalf("version=%q, want 1.2.8-bashy.1", m.Version)
	}
	const release = "https://github.com/qiangli/klatch/releases/download/bashy-v1.2.8-1/"
	const identity = "https://github.com/qiangli/klatch/.github/workflows/bashy-build.yml@refs/tags/bashy-v1.2.8-1"
	if m.Sigstore.Identity != identity || m.Sigstore.Issuer != "https://token.actions.githubusercontent.com" {
		t.Fatalf("wrong signing identity: %+v", m.Sigstore)
	}
	assets := map[string]struct{ file, sha256 string }{
		"linux/amd64":   {"klatch-v1.2.8-linux-amd64.tar.gz", "e419a33b0f28137443c9c45d182d4ff4105ff75018d1857efeb3f76230dc6c10"},
		"linux/arm64":   {"klatch-v1.2.8-linux-arm64.tar.gz", "7ec31b33f28c5ba87ad6c5d1a930742429558603290244565277b9501005d6e3"},
		"darwin/amd64":  {"klatch-v1.2.8-darwin-amd64.tar.gz", "f828c32eb386943139a6ddc76bbcff29cc4616617f09420c769a782b2af6f33a"},
		"darwin/arm64":  {"klatch-v1.2.8-darwin-arm64.tar.gz", "e447f86308f96038b0e27643f813b6f9f9dcf56835c6fab4a2a340c5abda05e2"},
		"windows/amd64": {"klatch-v1.2.8-windows-amd64.tar.gz", "fe9b13879bc86d6394e80aa87b8efe7410ca3c1bb78e16b84d85869e5fdf26f2"},
	}
	for platform, want := range assets {
		a, ok := m.Assets[platform]
		member := "klatch"
		if strings.HasPrefix(platform, "windows/") {
			member = "klatch.exe"
		}
		if !ok || a.SHA256 != want.sha256 || a.Member != member || a.URL != release+want.file {
			t.Errorf("%s: invalid signed archive pin: %+v", platform, a)
		}
	}
	if len(m.Assets) != len(assets) {
		t.Errorf("assets=%d, want five platforms", len(m.Assets))
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
