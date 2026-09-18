// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package execlog

import (
	"sort"
	"strconv"
	"time"
)

// Pitfall is a candidate durable claim: this command fails here, repeatedly,
// and no more recent run of it has worked.
//
// It is a CANDIDATE. This package decides what the evidence supports; whether
// the claim is admitted, and to which store, is the caller's decision behind
// the scrubber. Keeping those separate is what stops a recorder from becoming
// an author.
type Pitfall struct {
	Template  string `json:"template"`
	Cmd       string `json:"cmd"`
	Dimension string `json:"dimension,omitempty"`
	ExitClass string `json:"exit_class"`

	// The evidence, all of it, so a reader can disagree with the threshold
	// rather than only with the verdict.
	Failures  int `json:"failures"`
	Episodes  int `json:"episodes"`
	Days      int `json:"days"`
	Successes int `json:"successes"`

	FirstSeen time.Time `json:"first_seen"`
	LastSeen  time.Time `json:"last_seen"`
	// LastSuccess is zero when the command has never worked here. It is carried
	// even for admitted pitfalls so a reader can see a claim is about a command
	// that used to work.
	LastSuccess time.Time `json:"last_success,omitempty"`
}

// PromoteOpts is the evidence bar a candidate must clear.
//
// The defaults are not tuning knobs; each one closes a specific way of being
// confidently wrong, and PromoteDefaults documents which.
type PromoteOpts struct {
	MinEpisodes int
	MinDays     int
	Now         time.Time
}

// PromoteDefaults is the bar, and the reasoning for each number.
//
//   - MinEpisodes 3, counted in DISTINCT EPISODES rather than in failures. An
//     agent retrying the same doomed command eleven times in one session is ONE
//     observation, not eleven, and counting executions would let a single bad
//     afternoon mint a permanent claim.
//   - MinDays 2. The spacing effect, and the cheapest guard there is against a
//     transient — a broken VPN, a full disk, a service restarting — being
//     written down as a property of the command.
func PromoteDefaults() PromoteOpts {
	return PromoteOpts{MinEpisodes: 3, MinDays: 2}
}

// Promote folds the corpus into the pitfalls its evidence supports.
//
// Four rules decide admission, and each exists because of a specific way this
// could otherwise lie:
//
//  1. A benign exit is not a failure. grep finding nothing, test being false,
//     diff differing — these are negative ANSWERS. In a real corpus they are
//     40% of every exit-1, and admitting them yields the claim "grep is
//     unreliable on this machine", which is both false and confident.
//  2. An unobserved exit is not evidence. It may never have run.
//  3. An OPAQUE command is excluded. `make test` fails in a child this process
//     never saw, so attributing the failure to `make test` attributes it to the
//     only thing that was NOT measured.
//  4. A more recent success closes the claim. That is supersession on positive
//     evidence, the same discipline the fact store uses: the store self-corrects
//     when something works, never when something fails, because a failure does
//     not say which of its parts was at fault.
func Promote(root string, opt PromoteOpts) ([]Pitfall, Coverage, error) {
	if opt.MinEpisodes <= 0 {
		opt.MinEpisodes = PromoteDefaults().MinEpisodes
	}
	if opt.MinDays <= 0 {
		opt.MinDays = PromoteDefaults().MinDays
	}

	recs, cov, err := Read(root, Query{})
	if err != nil {
		return nil, cov, err
	}

	type acc struct {
		p           Pitfall
		episodes    map[string]bool
		days        map[string]bool
		dimensions  map[string]int
		lastSuccess time.Time
	}
	byTemplate := map[string]*acc{}

	get := func(r Record) *acc {
		a, ok := byTemplate[r.Template]
		if !ok {
			a = &acc{
				p:          Pitfall{Template: r.Template, Cmd: r.Cmd},
				episodes:   map[string]bool{},
				days:       map[string]bool{},
				dimensions: map[string]int{},
			}
			byTemplate[r.Template] = a
		}
		return a
	}

	for _, r := range recs {
		if r.Template == "" || !r.Observed || r.Exit == nil {
			continue // rule 2
		}
		a := get(r)

		if *r.Exit == 0 {
			a.p.Successes++
			if r.At.After(a.lastSuccess) {
				a.lastSuccess = r.At
			}
			continue
		}
		if r.Benign || r.Opaque {
			continue // rules 1 and 3
		}

		a.p.Failures++
		a.episodes[r.Episode] = true
		a.days[r.At.UTC().Format("2006-01-02")] = true
		if r.Dimension != "" {
			a.dimensions[r.Dimension]++
		}
		if a.p.FirstSeen.IsZero() || r.At.Before(a.p.FirstSeen) {
			a.p.FirstSeen = r.At
		}
		if r.At.After(a.p.LastSeen) {
			a.p.LastSeen = r.At
			a.p.ExitClass = exitClass(*r.Exit)
		}
	}

	var out []Pitfall
	for _, a := range byTemplate {
		a.p.Episodes = len(a.episodes)
		a.p.Days = len(a.days)
		a.p.LastSuccess = a.lastSuccess
		a.p.Dimension = topDimension(a.dimensions)

		if a.p.Failures == 0 ||
			a.p.Episodes < opt.MinEpisodes ||
			a.p.Days < opt.MinDays {
			continue
		}
		// Rule 4 — supersession on positive evidence.
		if !a.lastSuccess.IsZero() && a.lastSuccess.After(a.p.LastSeen) {
			continue
		}
		out = append(out, a.p)
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].Episodes != out[j].Episodes {
			return out[i].Episodes > out[j].Episodes
		}
		return out[i].Template < out[j].Template
	})
	return out, cov, nil
}

// exitClass reduces an exit status to its meaning.
//
// The raw integer is a per-tool convention — `1` means "no match" to grep and
// "tests failed" to go — so a claim keyed on the number would merge two
// unrelated outcomes. Only the statuses with a shell-wide meaning get their own
// class.
func exitClass(status int) string {
	switch {
	case status == 126:
		return "not-executable"
	case status == 127:
		return "not-found"
	case status > 128 && status < 160:
		return "signal:" + strconv.Itoa(status-128)
	default:
		return "generic"
	}
}

// topDimension returns the advisor diagnosis seen most often, or "" when none
// was ever produced.
//
// Empty is reported rather than guessed. "The advisor could not classify this"
// is a real answer, and inventing a plausible dimension is exactly the
// confabulation the whole design is arranged to avoid.
func topDimension(counts map[string]int) string {
	best, bestN := "", 0
	for d, n := range counts {
		if n > bestN || (n == bestN && d < best) {
			best, bestN = d, n
		}
	}
	return best
}
