package handoff

import (
	"os"

	"github.com/qiangli/yoke/pkg/secrets"
)

// Handoff artifacts are member-facing egress: a record is a FILE by design
// ("scp it, mesh it, paste it in an issue"), the store directory is
// world-readable, and every transition pushes a brief onto the host event
// log that meet streams to session members. A vault value that reaches any
// of them has left the owner.
//
// redactText masks the vault-projected values present in this process's
// environment. Values, not shapes: a legitimate patch line or brief that
// merely looks credential-shaped is left alone, the same rule the bus and
// feed egress points apply.
func redactText(s string) string {
	if s == "" {
		return s
	}
	return string(secrets.EnvValueRedactor(os.Environ()).Redact([]byte(s)))
}

// redactRecord masks every free-text field of a record in place: the
// continuity brief, the next action, the blockers, the captured diff and the
// untracked files carried by content. Redacting the DIFF can break the patch
// as a patch — that is deliberate and visible: the masked marker names what
// was removed, and a value that must not leave the owner was never safe to
// hand to a successor host either.
func redactRecord(r *Record) {
	if r == nil {
		return
	}
	r.Continuity = redactText(r.Continuity)
	r.NextAction = redactText(r.NextAction)
	for i, b := range r.Blockers {
		r.Blockers[i] = redactText(b)
	}
	r.Work.Diff = redactText(r.Work.Diff)
	for i, u := range r.Work.Untracked {
		r.Work.Untracked[i].Content = redactText(u.Content)
	}
}
