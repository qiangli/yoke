package chat

import (
	"bytes"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/capability"
)

// --capability OFFERS the routed tool's declared vendor command; it never
// switches the chat path. Resolved against the compiled-in baseline tools
// (claude plan, codex review are the measured seeds).
func TestOfferToolCommandForRoutedCapability(t *testing.T) {
	pinCatalog(t)
	for _, tc := range []struct {
		agent string
		c     capability.Capability
		want  string
	}{
		{"claude:opus", capability.CapPlanning, "bashy tool cmd run claude:plan -- TEXT"},
		{"codex:gpt-5", capability.CapCodeReview, "bashy tool cmd run codex:review -- TEXT"},
		{"claude:opus", capability.CapCoding, ""},
		{"no-such-tool:x", capability.CapPlanning, ""},
	} {
		var buf bytes.Buffer
		offerToolCommand(&buf, tc.agent, tc.c)
		got := strings.TrimSpace(buf.String())
		if tc.want == "" {
			if got != "" {
				t.Errorf("%s/%s: offered %q, want nothing", tc.agent, tc.c, got)
			}
			continue
		}
		if !strings.Contains(got, tc.want) {
			t.Errorf("%s/%s: offered %q, want it to contain %q", tc.agent, tc.c, got, tc.want)
		}
	}
}
