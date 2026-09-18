package commscli

// The atlas is this repo's manifest of the verbs a host mounts — the closest
// thing the coreutils side has to a record of "reachable from the CLI". The
// inbox/notify gap was invisible precisely along this axis: the constructors
// shipped, no atlas verb entry existed, and no test connected the two. These
// tests draw that line explicitly, in both directions.

import (
	"testing"

	"github.com/qiangli/yoke/pkg/atlas"
)

// Every S80 verb this audit proves behaviorally must also be declared in the
// atlas. A verb that behaves but is not in the manifest is exactly one host
// refactor away from becoming unreachable without any test noticing.
func TestS80VerbsAreDeclaredInTheAtlas(t *testing.T) {
	for _, v := range []string{"whois", "agent", "mb", "bus", "weave", "inbox", "notify"} {
		if _, ok := atlas.Lookup(v); !ok {
			t.Errorf("verb %q is audited as CLI-reachable but has no atlas entry — the manifest and the surface have drifted", v)
		}
	}
}

// KNOWN DEFECT, pinned (S80 follow-up). pkg/bus exports NewInboxCmd and
// NewNotifyCmd, and audit_test.go proves both behave from a built binary —
// but neither verb has an atlas entry, and the shipped bashy host does not
// mount them (`bashy inbox` → exit 127 on the host this audit was written
// on). They are reachable-in-principle, unreachable-in-fact: the precise
// state this audit exists to detect.
//
