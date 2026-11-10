package feed

import (
	"os"

	"github.com/qiangli/yoke/pkg/secrets"
)

// Outbound channel text is member-facing egress leaving the host entirely:
// the adapters deliver Envelope.Text to third-party services (Discord,
// Slack, Telegram, Matrix) and the declared tool-command channels carry it
// on argv. A vault value in an outbound body would leave the owner on the
// first hop. redactOutboundText masks the vault-projected values present in
// this process's environment — values, not shapes, so ordinary message text
// that merely looks credential-shaped is delivered verbatim.
func redactOutboundText(s string) string {
	if s == "" {
		return s
	}
	return string(secrets.EnvValueRedactor(os.Environ()).Redact([]byte(s)))
}
