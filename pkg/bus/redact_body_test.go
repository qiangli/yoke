package bus

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// vaultEgressFixture installs a vault-projected name with a rendered value
// visible in this process's environment, so the cross-host redaction has an
// exact value it must mask. The value is deliberately not credential-shaped:
// only exact-value registration can catch it.
func vaultEgressFixture(t *testing.T, localName, value string) {
	t.Helper()
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	path := filepath.Join(xdg, "bashy", "secrets.map")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(localName+"=@vault-item\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(localName, value)
}

// The cross-host send is member-facing egress twice over: the relay envelope
// leaves the host, and the sender's outbox copy lands in the world-readable
// board store. Both must carry the masked body.
func TestSendRemoteRedactsVaultValues(t *testing.T) {
	isolate(t)
	t.Setenv("BASHY_HOME", t.TempDir())
	vaultEgressFixture(t, "DEPLOY_TOKEN", "sycamore-lantern-42")
	sent, _ := wireRemote(t, nil)

	body := "deploy with DEPLOY_TOKEN=sycamore-lantern-42 before the cutover"
	res, err := Send(SendRequest{From: "tester", To: "plinth@noviwin1", Body: body})
	if err != nil {
		t.Fatal(err)
	}
	if res.Kind != SendRemote {
		t.Fatalf("kind = %v, want remote", res.Kind)
	}
	if len(*sent) != 1 {
		t.Fatalf("relay saw %d messages, want 1", len(*sent))
	}
	if strings.Contains((*sent)[0].Body, "sycamore-lantern-42") {
		t.Fatalf("vault value left on the relay: %q", (*sent)[0].Body)
	}
	if !strings.Contains((*sent)[0].Body, "[redacted:DEPLOY_TOKEN]") {
		t.Fatalf("relay body missing marker: %q", (*sent)[0].Body)
	}
	posts, err := Posts()
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range posts {
		if p.ID == res.ID || p.Seq == res.Seq {
			if strings.Contains(p.Body, "sycamore-lantern-42") {
				t.Fatalf("vault value persisted to the board outbox: %q", p.Body)
			}
			if !strings.Contains(p.Body, "[redacted:DEPLOY_TOKEN]") {
				t.Fatalf("outbox copy missing marker: %q", p.Body)
			}
			return
		}
	}
	t.Fatalf("outbox copy of %s (seq %d) not found among %d posts", res.ID, res.Seq, len(posts))
}

// RedactCrossHostBody masks only known values: ordinary text — even
// credential-SHAPED text that is not a registered vault value — passes
// through, so a post's legitimate content is never rewritten.
func TestRedactCrossHostBodyValuesNotShapes(t *testing.T) {
	isolate(t)
	vaultEgressFixture(t, "DEPLOY_TOKEN", "sycamore-lantern-42")
	got := RedactCrossHostBody("token sycamore-lantern-42 and a bare sk-shaped-not-registered word")
	if got != "token [redacted:DEPLOY_TOKEN] and a bare sk-shaped-not-registered word" {
		t.Fatalf("RedactCrossHostBody = %q", got)
	}
}
