// Package recall is the third-party READ surface over this host's memory rings.
//
// It answers "what is known about X" for software we do not control. That framing
// is the whole design, and two consequences are load-bearing.
//
// # It must never read the fact store
//
// craft facts are entity-bound, host-local, 0600, and have NO export path — and
// that absence IS the enforcement (dhnt/docs/skill-graph-design.md §Invariant 3).
// recall's output is designed to be piped, logged, and consumed by other people's
// programs, so if it printed facts it would BECOME the export path the fact
// store's whole design exists to deny. Facts are therefore not a ring, not behind
// a flag, and not reachable with any future --all. A fold DERIVED from facts is
// fine: pkg/redact is the admission gate and craft promote already refuses a fold
// whose note names an identity.
//
// This is the easiest constraint in the system to erode, and the diff that breaks
// it looks reasonable — "just add --include-facts for debugging". There is no
// FoldStore-shaped accessor for facts in this package and there must not be one.
//
// # Per-ring caps, never cross-ring fusion
//
// Scores from different rings are not comparable: a kb page's BM25 score is
// computed over a corpus of prose lessons, a capability's over contracts. Fusing
// them was measured and rejected — reciprocal-rank fusion with an arm that had no
// signal dropped MRR from 0.594 to 0.417 at identical hit@3
// (dhnt/docs/memory-eval-plan.md §4b). So each ring contributes at most K hits
// ranked WITHIN itself, rings are emitted in precedence order, and every hit
// carries its ring label so the caller can re-rank with knowledge we do not have.
// K is per-ring for exactly this reason; a global cap would force the comparison
// this package refuses to make.
package recall

import (
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/qiangli/yoke/pkg/craft"
	"github.com/qiangli/yoke/pkg/kb"
)

// Version is the envelope version. Fields may be ADDED within a major version;
// they may never be repurposed or removed, because third parties parse them and
// cannot read our source to discover a change.
const Version = 1

// Ring names, in precedence order. The order matches the fleet registry's
// (embedded → shared → cloud → local) intent: a record closer to this host and
// this operator is emitted before an org-wide default.
const (
	RingAgent      = "agent"      // owner-only <agent-data>/kb pages
	RingRepo       = "repo"       // the current repository's committed docs/kb pages
	RingHost       = "host"       // ~/.bashy/kb pages
	RingCapability = "capability" // craft folds — what generally holds at a coordinate
)

// Rings returns the rings this build can read, in presentation precedence.
// Readers rank only inside their own ring; this order never fuses their scores.
func Rings() []string { return []string{RingAgent, RingRepo, RingHost, RingCapability} }

// Query is a recall request. The zero value is usable except for Text.
type Query struct {
	Text string

	// K caps hits PER RING, not globally. See the package doc.
	K int
	// Rings restricts the read to these rings. Empty reads all of them.
	Rings []string
	// Budget is a hard ceiling on rendered output, in tokens. 0 = no ceiling.
	Budget int
	// Resolution controls how much of each record is rendered.
	Resolution kb.Resolution
	// MinCoverage gates the EMPTY answer: a query no ring covers this much of
	// returns Abstained rather than padding with weak hits. 0 = always answer.
	MinCoverage float64
	// Since ignores records older than this. 0 = no age filter.
	Since time.Duration
	// Repo and OS filter scoped records. Empty = unfiltered.
	Repo, OS string
	// Forms restricts records by shape. Empty leaves form selection to the
	// caller (recall's historical behaviour); context defaults it to note,page.
	Forms []string
	// Files are task-local code cues exposed to injected readers such as
	// CodeRing. Page readers do not reinterpret them.
	Files []string
	// Episode admits checkpoint-tagged agent notes from exactly this episode.
	// Without it, checkpoint reconstructions are never presented as records.
	Episode string

	// Now is injectable so tests are not clock-dependent. Zero = time.Now().
	Now time.Time
}

func (q Query) now() time.Time {
	if q.Now.IsZero() {
		return time.Now()
	}
	return q.Now
}

func (q Query) k() int {
	if q.K <= 0 {
		return 3
	}
	return q.K
}

func (q Query) wants(ring string) bool {
	if len(q.Rings) == 0 {
		return true
	}
	for _, r := range q.Rings {
		if r == ring {
			return true
		}
	}
	return false
}

// SourceRef is an L0 pointer: where the record can be re-opened. A hit with no
// re-openable source is a RECONSTRUCTION, and a reconstruction may never be
// presented as a record — so this is part of the contract, not a convenience.
type SourceRef struct {
	URI    string `json:"uri"`
	Hash   string `json:"hash,omitempty"`
	Region string `json:"region,omitempty"`
}

// Hit is one recalled record, normalized across rings.
type Hit struct {
	Ring string `json:"ring"`
	Kind string `json:"kind"`
	ID   string `json:"id"`
	Form string `json:"form,omitempty"`

	Cue  string `json:"cue"`
	Gist string `json:"gist,omitempty"`

	Source []SourceRef `json:"source,omitempty"`

	Status     string `json:"status,omitempty"`
	Confidence string `json:"confidence,omitempty"`

	ObservedAt string `json:"observed_at,omitempty"`
	ValidFrom  string `json:"valid_from,omitempty"`

	Score float64  `json:"score"`
	Why   []string `json:"why,omitempty"`

	// Compose is the command that renders a capability, when the hit is one.
	Compose string `json:"compose,omitempty"`

	// Full is the record body used by the context resolution ladder. It is
	// internal to rendering so recall's established JSON stays token-lean.
	Full string `json:"-"`
	// Episode and Checkpoint enforce the agent-ring reconstruction boundary.
	Episode    string `json:"-"`
	Checkpoint bool   `json:"-"`
}

// Budget reports what the render cost against the ceiling.
type Budget struct {
	Tokens    int  `json:"tokens"`
	Spent     int  `json:"spent"`
	Truncated bool `json:"truncated"`
}

// Result is the envelope. `--json` emits exactly this.
type Result struct {
	RecallVersion int    `json:"recall_version"`
	Query         string `json:"query"`
	Budget        Budget `json:"budget"`
	// Abstained is true when MinCoverage was set and no ring covered enough of
	// the query. It is a FIRST-CLASS ANSWER: abstained with zero hits is still
	// exit 0, because "nothing is known" is information and a caller that
	// cannot distinguish it from "something broke" will proceed on a false
	// negative.
	Abstained bool  `json:"abstained"`
	Hits      []Hit `json:"hits"`
	// Errors names rings that EXISTED but could not be read. A non-empty
	// Errors is the only thing that makes recall exit 1; a missing ring is not
	// an error, it is a host that has not learned anything there yet.
	Errors []RingError `json:"errors,omitempty"`
}

// RingError is a ring that was present but unreadable.
type RingError struct {
	Ring string `json:"ring"`
	Err  string `json:"error"`
}

// Reader is one ring's read side. Rings are independent: one failing must never
// suppress another's answer, which is why each returns its own error.
type Reader interface {
	Ring() string
	// Forms names the record shapes this reader can provide. The context
	// command uses it to refuse unavailable forms before doing any reads.
	Forms() []string
	Recall(q Query) ([]Hit, error)
}

// HostRing reads ~/.bashy/kb pages.
type HostRing struct {
	Store *kb.Store
	// Path, when set, is checked before reading. Context sets it so a missing
	// requested substrate is an honest broken-ring result naming what opened;
	// direct Recall callers may leave it empty for the historical lazy store.
	Path string
}

func (HostRing) Ring() string    { return RingHost }
func (HostRing) Forms() []string { return []string{kb.FormNote, kb.FormPage} }

func (r HostRing) Recall(q Query) ([]Hit, error) {
	return recallPages(RingHost, r.Store, r.Path, q)
}

// CapabilityRing reads craft FOLDS — generalisable knowledge keyed on a
// coordinate. It deliberately has no access to the fact store; see the package
// doc. Folds are ranked by reusing kb's BM25 over a synthetic page per fold, so
// there is ONE ranker in the system rather than a second hand-tuned one.
type CapabilityRing struct {
	Folds *craft.FoldStore
	Path  string
}

func (CapabilityRing) Ring() string    { return RingCapability }
func (CapabilityRing) Forms() []string { return []string{kb.FormPage} }

func (r CapabilityRing) Recall(q Query) ([]Hit, error) {
	if r.Path != "" {
		if err := requireRingDir(r.Path); err != nil {
			return nil, err
		}
	}
	if len(q.Forms) > 0 && !slices.Contains(q.Forms, kb.FormPage) {
		return nil, nil
	}
	if r.Folds == nil {
		return nil, nil
	}
	now := q.now()
	var pages []*kb.Page
	byslug := map[string]craft.Fold{}
	for i, f := range r.Folds.All() {
		if !f.Live(now) {
			continue
		}
		if q.Since > 0 && now.Sub(f.ObservedAt) > q.Since {
			continue
		}
		slug := "fold-" + itoa(i)
		byslug[slug] = f
		// A fold rendered as a page so kb.Search ranks it: the note is the
		// routing surface (description weight 3), the capability its title.
		pages = append(pages, &kb.Page{
			Slug: slug, Type: "fold",
			Title:       f.Capability,
			Description: f.Note,
			Status:      kb.StatusValidated,
			Body:        f.Evidence,
		})
	}
	if len(pages) == 0 {
		return nil, nil
	}
	var out []Hit
	for _, h := range kb.Search(pages, kb.Query{
		Terms: kb.Terms(q.Text), K: q.k(), MinCoverage: q.MinCoverage,
	}) {
		f := byslug[h.Page.Slug]
		id := "craft:" + f.Coordinate
		if f.Capability != "" {
			id = "craft:" + f.Capability + "@" + f.Coordinate
		}
		hit := Hit{
			Ring: RingCapability, Kind: "fold", ID: id,
			Form: kb.FormPage,
			Cue:  f.Note, Gist: f.Evidence,
			Full:   f.Evidence,
			Source: []SourceRef{{URI: "file://" + r.Folds.Path(), Region: f.Coordinate}},
			Status: kb.StatusValidated, Confidence: "EXTRACTED",
			ObservedAt: rfc3339(f.ObservedAt), ValidFrom: rfc3339(f.ValidFrom),
			Score: h.Score,
			Why:   []string{"fold note", "coordinate " + f.Coordinate},
		}
		if f.Capability != "" {
			hit.Compose = "bashy craft compose " + f.Capability
		}
		out = append(out, hit)
	}
	return out, nil
}

// Recall fans across the rings and assembles the envelope.
//
// Ring independence is the invariant: a ring that fails is RECORDED and the rest
// still answer, because a single unreadable store must not turn a host's whole
// memory into an error. Only Result.Errors makes the caller exit non-zero.
func Recall(q Query, readers ...Reader) Result {
	res := Result{RecallVersion: Version, Query: q.Text}
	res.Budget.Tokens = q.Budget

	for _, rd := range readers {
		if !q.wants(rd.Ring()) {
			continue
		}
		hits, err := rd.Recall(q)
		if err != nil {
			res.Errors = append(res.Errors, RingError{Ring: rd.Ring(), Err: err.Error()})
			continue
		}
		// Within a ring: rank by score, cap at K. Ties break on ID so the
		// same store returns the same order twice — a caller building on an
		// unstable order has no reproducible behaviour.
		sort.SliceStable(hits, func(i, j int) bool {
			if hits[i].Score != hits[j].Score {
				return hits[i].Score > hits[j].Score
			}
			return hits[i].ID < hits[j].ID
		})
		if len(hits) > q.k() {
			hits = hits[:q.k()]
		}
		res.Hits = append(res.Hits, hits...)
	}

	if q.MinCoverage > 0 && len(res.Hits) == 0 {
		res.Abstained = true
	}
	res.Hits, res.Budget.Spent, res.Budget.Truncated = applyBudget(res.Hits, q.Budget)
	return res
}

// applyBudget truncates at a whole-hit boundary. A half-rendered record is worse
// than one fewer record: it can be quoted as if complete.
func applyBudget(hits []Hit, budget int) (kept []Hit, spent int, truncated bool) {
	if budget <= 0 {
		for _, h := range hits {
			spent += tokens(h)
		}
		return hits, spent, false
	}
	for _, h := range hits {
		c := tokens(h)
		if spent+c > budget {
			return kept, spent, true
		}
		kept = append(kept, h)
		spent += c
	}
	return kept, spent, false
}

// tokens estimates rendered cost. Deliberately a cheap word count rather than a
// real tokenizer: the budget is a guard rail, and importing a tokenizer here
// would put a model-shaped dependency on the irreducible floor.
func tokens(h Hit) int {
	n := len(strings.Fields(h.Cue)) + len(strings.Fields(h.Gist))
	return n + 8 // envelope overhead per hit
}

func withinSince(updated, created string, now time.Time, d time.Duration) bool {
	for _, s := range []string{updated, created} {
		if s == "" {
			continue
		}
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			return now.Sub(t) <= d
		}
	}
	// No parseable timestamp: keep it. Dropping records for having no date
	// would silently hide every page written before timestamps existed.
	return true
}

func rfc3339(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
