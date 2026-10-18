package lenscmd

import (
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/bus"
)

// A locator in a message body, written by <name>@<host>, is followed through
// the peer transport on the SENDER's host: the ref bus.MessageLocators emits
// is the exact operand lens reads.
func TestLocatorMessageRefFollowsOnSourceHost(t *testing.T) {
	cases := []struct {
		name, from, body string
		want             string // substring of stdout; "" with wantErr set
		wantErr          string
		wantCalls        bool
	}{
		{"sender host is a peer host", "alice@mac-1.example", "see dir:src for it", "a.txt", "", true},
		{"explicit alias scope", "alice@other", "see dir:src@mac", "a.txt", "", true},
		{"unknown sender host", "alice@nowhere", "see dir:src", "", "unknown peer", false},
		{"ambiguous alias", "alice@x", "see dir:src@dup", "", "ambiguous peer", false},
		{"offline sender host", "alice@gone.example", "see dir:src", "", "offline", false},
		{"absolute path is never guessed locally", "alice@mac-1.example", "see dir:/etc", "", "host-relative", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fh := hostFixture(t, hostPeers)
			refs := bus.MessageLocators(tc.body, tc.from)
			if len(refs) != 1 || !refs[0].CrossHost {
				t.Fatalf("refs = %+v, want one cross-host ref", refs)
			}
			out, errOut, code := runTool(t, t.TempDir(), "", "-t", "graph", refs[0].Ref)
			if tc.wantErr != "" {
				if code == 0 || !strings.Contains(errOut, tc.wantErr) {
					t.Fatalf("code %d stderr %q, want %q", code, errOut, tc.wantErr)
				}
			} else if code != 0 || !strings.Contains(out, tc.want) {
				t.Fatalf("code %d stderr %q out %q", code, errOut, out)
			}
			if tc.wantCalls != (len(fh.calls) > 0) {
				t.Fatalf("transport calls = %v", fh.calls)
			}
		})
	}
}

func TestLocatorMessageOperandFollowsPost(t *testing.T) {
	cases := []struct {
		name, from, body, want, wantErr string
		wantCalls                       bool
	}{
		{"peer host sender", "alice@mac-1.example", "see dir:src for it", "a.txt", "", true},
		{"explicit alias", "alice@other", "see dir:src@mac", "a.txt", "", true},
		{"unknown host", "alice@nowhere", "see dir:src", "", "unknown peer", false},
		{"ambiguous host alias", "alice@x", "see dir:src@dup", "", "ambiguous peer", false},
		{"two locators refused", "alice@mac-1.example", "dir:src and dir:other", "", "ambiguous, message names 2", false},
		{"no locator", "alice@mac-1.example", "just prose /etc/passwd", "", "no followable locator", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("BASHY_MB_DIR", t.TempDir())
			t.Setenv("BASHY_ROOM_DIR", t.TempDir())
			fh := hostFixture(t, hostPeers)
			if _, err := bus.PostMessageSeq(bus.Post{From: tc.from, Topic: "mb", Body: tc.body}); err != nil {
				t.Fatal(err)
			}
			out, errOut, code := runTool(t, t.TempDir(), "", "-t", "graph", "mb:1")
			if tc.wantErr != "" {
				if code == 0 || !strings.Contains(errOut, tc.wantErr) {
					t.Fatalf("code %d stderr %q, want %q", code, errOut, tc.wantErr)
				}
			} else if code != 0 || !strings.Contains(out, tc.want) {
				t.Fatalf("code %d stderr %q out %q", code, errOut, out)
			}
			if tc.wantCalls != (len(fh.calls) > 0) {
				t.Fatalf("transport calls = %v", fh.calls)
			}
		})
	}
	if _, _, code := runTool(t, t.TempDir(), "", "-t", "graph", "mb:99"); code == 0 {
		t.Fatal("missing post must fail")
	}
}
