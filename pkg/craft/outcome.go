package craft

// WHAT FAILED — the difference between evidence about a fact and evidence about
// nothing.
//
// A non-zero exit is not one thing. `ssh -p 2222 host make test` returning 1
// means the BUILD failed; the connection worked perfectly, and the port and
// login it used are now PROVEN. `ssh -p 2222 host make test` returning 255
// means ssh never got there, and those same arguments are suspect.
//
// Reading both as "a failure" costs twice:
//
//   - Evidence is thrown away. The first case demonstrated that the port and
//     login are right, and the store learned nothing from it, because recording
//     was gated on exit 0. Most real invocations run a command that can fail on
//     its own merits, so this is the common case, not the corner.
//   - Noise is emitted. The same case fired "this host is known here as …" at
//     someone whose host was never in question. Hints that arrive when nothing
//     is wrong are how people learn to ignore hints, and once they do, the
//     system is worse than one that never spoke.
//
// So the exit status is classified against what each tool's convention actually
// means, and a status this package cannot interpret stays Unknown rather than
// being assumed into either bucket.
//
// # What this deliberately does NOT do
//
// It does not attribute a transport failure to a PARTICULAR role. Knowing
// whether 255 means "wrong port" or "wrong login" requires the error text
// ("Connection refused" vs "Permission denied"), and capturing stderr would mean
// wrapping the stream every command writes to. That breaks isatty for the very
// commands that need it — ssh decides whether to prompt for a password that way
// — and this middleware's first rule is that it never changes an outcome.
//
// The asymmetry is worth recording for whoever picks that up: reaching an
// authentication prompt PROVES the port, so "Permission denied" is negative
// evidence for the login and POSITIVE evidence for the port, from one message.

// Verdict is what an exit status says about the invocation.
//
// Named Verdict rather than Outcome because craft.Outcome is already the
// absorption disposition in study.go — a different question about a different
// noun, and collapsing the two names would make both harder to read.
type Verdict int

const (
	// VerdictUnknown means this tool's exit convention is not declared. Neither
	// learning nor invalidating is safe; only a hint is.
	VerdictUnknown Verdict = iota
	// VerdictSuccess — everything worked.
	VerdictSuccess
	// VerdictTransport — the tool could not establish or authenticate the
	// session. The arguments are suspect.
	VerdictTransport
	// VerdictPayload — the session was established and the thing it carried
	// failed. The connection arguments are CONFIRMED.
	VerdictPayload
)

func (v Verdict) String() string {
	switch v {
	case VerdictSuccess:
		return "success"
	case VerdictTransport:
		return "transport"
	case VerdictPayload:
		return "payload"
	}
	return "unknown"
}

// Confirms reports whether the outcome is positive evidence for the connection
// arguments — the session provably worked.
func (v Verdict) Confirms() bool {
	return v == VerdictSuccess || v == VerdictPayload
}

// ExitModel is a tool's exit-status convention.
type ExitModel int

const (
	// ExitUnclassified — the convention is not known. Everything stays Unknown.
	ExitUnclassified ExitModel = iota
	// ExitToolOnly — the tool does its own work and runs nothing on the far
	// side, so every failure is the tool's own (scp, sftp, ssh-keyscan).
	ExitToolOnly
	// ExitPassThrough — the tool runs a command remotely and returns THAT
	// command's status, so any status outside TransportExits belongs to the
	// payload (ssh, psql).
	ExitPassThrough
)

// Classify says what a status means for a given command.
//
// An unknown binary, or one whose convention is not declared, yields
// VerdictUnknown. That is the honest answer and it is also the safe one: the
// caller may still offer a hint, but it must not record a fact or invalidate
// one on a status it cannot interpret.
func Classify(binary string, status int) Verdict {
	if status == 0 {
		return VerdictSuccess
	}
	spec, ok := SpecFor(binary)
	if !ok {
		return VerdictUnknown
	}
	if spec.TransportExits[status] {
		return VerdictTransport
	}
	switch spec.ExitModel {
	case ExitToolOnly:
		return VerdictTransport
	case ExitPassThrough:
		return VerdictPayload
	}
	return VerdictUnknown
}
