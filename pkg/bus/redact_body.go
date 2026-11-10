package bus

import (
	"os"

	"github.com/qiangli/yoke/pkg/secrets"
)

// RedactCrossHostBody masks vault-known secret values in text before it
// leaves this host on the cross-host relay (or lands in the sender's
// world-readable outboard copy of it).
//
// The cross-host mail path is member-facing egress: the relay carries the
// envelope to ANOTHER host, and the sender's board keeps a copy in
// posts.jsonl, which every reader on this host can read. A vault value that
// rides either surface has left the owner. Masking happens at the single
// choke every cross-host send passes through (sendRemote), so no caller can
// forget it.
//
// Values, not shapes: the Redactor masks the exact vault-projected values
// present in this process's environment (secrets.EnvValueRedactor). A
// credential-shaped string that is not a known value is not rewritten here —
// mis-masking a post's legitimate text is a correctness bug the value-based
// rule avoids.
func RedactCrossHostBody(body string) string {
	return string(secrets.EnvValueRedactor(os.Environ()).Redact([]byte(body)))
}
