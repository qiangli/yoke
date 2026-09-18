// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package execlog

import (
	"time"

	"github.com/qiangli/yoke/pkg/redact"
)

// Schema tags every record. Bump on a breaking field change.
const Schema = "bashy-execlog-v1"

// CanonVer is the template canonicaliser's version.
//
// It is part of the learning key, so a bump re-addresses the whole corpus.
// That is not an oversight: raw argv is never stored, so old records CANNOT be
// re-canonicalised, and pretending a migration is possible would be worse than
// admitting a reset. Old templates keep their evidence and age out.
const CanonVer = 1

// maxArgv bounds a single record so one append stays one write(2).
//
// A single atomic append is what makes the lock-free O_APPEND model safe for
// many concurrent writers. An unbounded argv — `grep -f pat <5000 files>` — is
// exactly the case that would exceed the kernel's atomicity window and let two
// processes interleave mid-record. Exceeding the cap sets Truncated, because a
// silently clipped record reads as a complete one.
const maxArgv = 4 << 10

// Record is one dispatched command.
type Record struct {
	Schema   string `json:"schema"`
	CanonVer int    `json:"canon_ver"`
	Stage    string `json:"stage"` // always "episode" — the learning-record stage axis

	At time.Time `json:"at"`

	// The space-time thread. Seq is assigned at CREATION, not at flush, so a
	// reader can tell "88 records were lost when that process died" from "88
	// records never existed". A gap is countable; silence is not.
	Episode string `json:"episode,omitempty"`
	PID     int    `json:"pid,omitempty"`
	PPID    int    `json:"ppid,omitempty"`
	Seq     uint64 `json:"seq"`

	// What ran. Template is the learning key; Argv is the scrubbed evidence.
	Cmd      string   `json:"cmd"`
	Template string   `json:"template"`
	Argv     []string `json:"argv,omitempty"`
	Cwd      string   `json:"cwd,omitempty"`

	// Outcome. Exit is a POINTER on purpose: "the command ran and returned 0"
	// and "we could not observe an exit" are different claims, and defaulting
	// the second to 0 is how an abstain gets recorded as a pass.
	Exit       *int     `json:"exit"`
	Observed   bool     `json:"observed"`
	DurationMs int64    `json:"duration_ms"`
	Effects    []string `json:"effects,omitempty"`

	// Benign marks a non-zero exit that is a NEGATIVE ANSWER rather than a
	// failure: grep found nothing, test was false, diff differed. Counting
	// these as failures would teach that grep is unreliable here — measured at
	// 40% of all exit-1s in a real corpus.
	Benign bool `json:"benign,omitempty"`

	// Dimension is the advisor's probe-grounded diagnosis of WHY it failed
	// (cwd | network | compute | disk | state | loop), and Retryable whether
	// re-running as-is could ever work. Empty means unclassified — which is an
	// honest answer, and different from "nothing was wrong".
	Dimension string `json:"dimension,omitempty"`
	Retryable bool   `json:"retryable,omitempty"`

	// Where. Neither is ever part of a key — see the clock-probe invariant in
	// spacegraph; the same trap eats an index key here.
	Coordinate string `json:"coordinate,omitempty"`
	Place      string `json:"place,omitempty"`

	// Opaque marks a command whose real work happened in children this process
	// never saw. `make test` looks atomic and is not. Rendering it as a leaf
	// would report 12 seconds of unobserved work as one observed command.
	Opaque    bool `json:"opaque,omitempty"`
	Truncated bool `json:"truncated,omitempty"`

	Redaction Stamp `json:"redaction"`
}

// Stamp records that redaction ran, and how much it caught.
//
// The COUNT only. A findings list carrying the original values would be the
// exact leak the scrubber exists to prevent.
type Stamp struct {
	Scrubber string `json:"scrubber"`
	N        int    `json:"n"`
}

// Scrubbed is argv and cwd that have provably been through the scrubber.
//
// The fields are unexported and there is no literal constructor, so the only
// way to obtain one is Scrub. Append takes a Scrubbed rather than a []string
// for that single reason: it makes "forgot to redact" a compile error instead
// of a leak nobody notices until the file is read.
type Scrubbed struct {
	argv      []string
	cwd       string
	template  string
	n         int
	truncated bool
}

// Scrub runs the two redaction passes and canonicalises the template.
//
// Order is load-bearing. Credentials are masked FIRST (by value), identity is
// tagged SECOND. Reversed, the hostname inside `psql postgres://u:p@host/db`
// gets replaced by a tag, the URL no longer matches the credential pattern,
// and the password ships.
func Scrub(s *redact.Scrubber, argv []string, cwd string, opt TemplateOpts) Scrubbed {
	if len(argv) == 0 {
		return Scrubbed{}
	}

	// Pass 1 — credentials, by value.
	masked := maskSecrets(argv)

	// Pass 2 — foreign identity by shape. The scrubber knows THIS machine's
	// host, user and home; it cannot know `/Users/someone-else/...`, and a
	// store holding every command on the box will see plenty of those.
	masked = maskForeignHomes(masked)

	// Pass 3 — local identity, by tag. The tags are co-reference preserving, so
	// the stored evidence still reads as a coherent sentence.
	var out Scrubbed
	if s != nil {
		joined, findings := s.Scrub(joinNUL(masked))
		out.argv = splitNUL(joined)
		out.cwd = s.String(cwd)
		out.n = len(findings)
	} else {
		out.argv = masked
		out.cwd = cwd
	}

	// The template is rendered from a SCRUBBED argv, then de-tagged.
	//
	// Rendering it from the pre-scrub argv — which is what this did first —
	// walks identity straight into the node key: a bare hostname or IP is not a
	// path, a number or a version, so the classifier leaves it verbatim and the
	// leak lands in the one field designed to be shared. Scrub first, then
	// collapse the co-reference tags to bare classes, because a tag is derived
	// from the local hostname and would give two machines two different nodes
	// for the same command.
	//
	// The `user@host` shape is classified BEFORE that scrub, because the
	// scrubber cannot tell `alice@remote.host` from an email address and would
	// swallow the whole word as one tag. That word names two entities and a
	// relation — it is the single most load-bearing shape the space graph
	// reads — so collapsing it to <EMAIL> would lose exactly the structure this
	// package exists to capture.
	tmplArgv := preclassifyHostShapes(masked)
	if s != nil {
		tmplArgv = splitNUL(s.String(joinNUL(tmplArgv)))
	}
	out.template = detag(Template(tmplArgv, opt))

	if n := argvBytes(out.argv); n > maxArgv {
		out.argv = clampArgv(out.argv, maxArgv)
		out.truncated = true
	}
	return out
}

// Elided reports how many identifying values the scrubber replaced.
func (s Scrubbed) Elided() int { return s.n }

// Template reports the canonical, identity-free rendering.
func (s Scrubbed) Template() string { return s.template }

func argvBytes(argv []string) int {
	n := 0
	for _, a := range argv {
		n += len(a) + 1
	}
	return n
}

func clampArgv(argv []string, limit int) []string {
	out := make([]string, 0, len(argv))
	n := 0
	for _, a := range argv {
		if n+len(a)+1 > limit {
			break
		}
		out = append(out, a)
		n += len(a) + 1
	}
	return out
}

// joinNUL round-trips an argv through the scrubber's string API without
// letting a value that already contains the separator split a word in two.
func joinNUL(argv []string) string {
	out := make([]byte, 0, argvBytes(argv))
	for i, a := range argv {
		if i > 0 {
			out = append(out, 0)
		}
		out = append(out, sanitizeNUL(a)...)
	}
	return string(out)
}

func splitNUL(s string) []string {
	if s == "" {
		return nil
	}
	out := []string{}
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == 0 {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	return append(out, s[start:])
}

func sanitizeNUL(s string) string {
	for i := 0; i < len(s); i++ {
		if s[i] == 0 {
			b := []byte(s)
			for j := range b {
				if b[j] == 0 {
					b[j] = ' '
				}
			}
			return string(b)
		}
	}
	return s
}
