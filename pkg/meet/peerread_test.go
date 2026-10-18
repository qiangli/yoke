package meet

import (
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/bus"
)

func TestLocatorPeerReadLine(t *testing.T) {
	t.Setenv("BASHY_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	t.Setenv("BASHY_MB_DIR", t.TempDir())
	cases := []struct {
		name, os, text, speaker, want, wantContains string
	}{
		{name: "remote posix", speaker: "a@peerbox-zz", text: "see dir:/srv/x", want: "lens dir:/srv/x@peerbox-zz"},
		{name: "remote windows", os: "windows", speaker: "a@peerbox-zz", text: "see dir:/srv/x", wantContains: "lens"},
		{name: "explicit host scope", speaker: "a", text: "see dir:/srv/x@peerbox-zz", wantContains: "@peerbox-zz"},
		{name: "unscoped sender is local", speaker: "a", text: "see dir:/srv/x", want: ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			old := peerOSFor
			peerOSFor = func(string) string { return c.os }
			defer func() { peerOSFor = old }()
			refs := bus.MessageLocators(c.text, c.speaker)
			if len(refs) != 1 {
				t.Fatalf("refs = %v", refs)
			}
			got := peerReadLine(refs[0])
			if c.wantContains != "" {
				if !strings.Contains(got, c.wantContains) {
					t.Fatalf("got %q want contains %q", got, c.wantContains)
				}
				if c.os == "windows" && !strings.HasPrefix(got, "&") {
					t.Fatalf("windows peer not pwsh: %q", got)
				}
				return
			}
			if got != c.want {
				t.Fatalf("got %q want %q", got, c.want)
			}
		})
	}
}

func TestLocatorPeerReadInContributions(t *testing.T) {
	t.Setenv("BASHY_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	t.Setenv("BASHY_MB_DIR", t.TempDir())
	text := "see dir:/srv/x"
	events := []Event{{Round: 1, Speaker: "a@peerbox-zz", Kind: "message", Text: text}}
	var b strings.Builder
	writeContributions(&b, &State{ID: "m1"}, events, "")
	out := b.String()
	if !strings.Contains(out, "read: lens ") || !strings.Contains(out, text) || events[0].Text != text {
		t.Fatalf("bad output: %s", out)
	}
}
