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
	if m.Version != "1.2.7-bashy.1" {
		t.Fatalf("version=%q, want 1.2.7-bashy.1", m.Version)
	}
	const release = "https://github.com/qiangli/klatch/releases/download/bashy-v1.2.7-1/"
	const identity = "https://github.com/qiangli/klatch/.github/workflows/bashy-build.yml@refs/tags/bashy-v1.2.7-1"
	if m.Sigstore.Identity != identity || m.Sigstore.Issuer != "https://token.actions.githubusercontent.com" {
		t.Fatalf("wrong signing identity: %+v", m.Sigstore)
	}
	assets := map[string]struct{ file, sha256 string }{
		"linux/amd64":   {"klatch-v1.2.7-linux-amd64.tar.gz", "da6f37c19bee9894e54f67bd61e1b4a88036642b661e8eb106d57c4da63bfdfd"},
		"linux/arm64":   {"klatch-v1.2.7-linux-arm64.tar.gz", "4903ee0415324452ec79bd6ccf18a835fcee96df7d775b323ea513b5e866603e"},
		"darwin/amd64":  {"klatch-v1.2.7-darwin-amd64.tar.gz", "5ac930f4426832bbc42ab82e74e680e10dc118a144400c6a72a94da6796438c8"},
		"darwin/arm64":  {"klatch-v1.2.7-darwin-arm64.tar.gz", "55fb9dfba25445bc17447a4cce93c60ce8f9edb1dc0d54958e4615cfbaba1b57"},
		"windows/amd64": {"klatch-v1.2.7-windows-amd64.tar.gz", "ea4f5ffdad987869d1767d69e6db24e9342b585bc2aedfaa673fe5e49430c565"},
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
