package meet

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/bus"
)

func TestPeerOSUsesBoundedOutpostStatus(t *testing.T) {
	old := peerStatusRunner
	t.Cleanup(func() { peerStatusRunner = old })
	rows := `[{"host":"win-1","alias":"desk","os":"windows"},{"host":"linux-1","os":"linux"},{"host":"other","alias":"ambiguous","os":"darwin"},{"host":"another","alias":"ambiguous","os":"windows"}]`
	peerStatusRunner = func(context.Context) ([]byte, error) { return []byte(rows), nil }
	for host, want := range map[string]string{"win-1": "windows", "desk": "windows", "linux-1": "linux", "ambiguous": "", "missing": ""} {
		if got := lookupPeerOS(host); got != want {
			t.Errorf("lookupPeerOS(%q) = %q, want %q", host, got, want)
		}
	}
	refs := bus.MessageLocators("see dir:src@desk", "a")
	if len(refs) != 1 || !strings.HasPrefix(peerReadLine(refs[0]), "&") {
		t.Fatalf("Windows peer command was not rendered as PowerShell: %+v", refs)
	}
	peerStatusRunner = func(context.Context) ([]byte, error) { return nil, errors.New("offline") }
	if got := lookupPeerOS("win-1"); got != "" {
		t.Fatalf("unavailable peer board guessed OS %q", got)
	}
}

func TestLocatorPeerReadLine(t *testing.T) {
	t.Setenv("BASHY_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	t.Setenv("BASHY_MB_DIR", t.TempDir())
	cases := []struct {
		name, os, text, speaker, want, wantContains string
	}{
		{name: "unknown os both labeled", speaker: "a@peerbox-zz", text: "see dir:/srv/x", wantContains: "[bash] lens dir:/srv/x@peerbox-zz | [pwsh] &"},
		{name: "remote posix", os: "linux", speaker: "a@peerbox-zz", text: "see dir:/srv/x", want: "lens dir:/srv/x@peerbox-zz"},
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
	if !strings.Contains(out, "read: [bash] lens ") || !strings.Contains(out, text) || events[0].Text != text {
		t.Fatalf("bad output: %s", out)
	}
}
