// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package execlog

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// EvidenceRef is an ADDRESS into the stream, not a copy of it.
//
// This is the load-bearing half of "kb stores everything but indexes into the
// other stores". A knowledge page holds the CLAIM; the ref is how you get from
// the claim back to the raw records that produced it. Copying the records into
// the page instead would put 30,000 lines a day into a wiki whose index has to
// stay small enough to read, and would duplicate a corpus that already has a
// retention policy.
//
// Wire form:
//
//	exec://2026-08-03..2026-08-05/ep-4f2a91c3#seq=41,58,77
//	exec://2026-08-03..2026-08-05#n=12
//
// The second form is what a claim spanning many sessions gets: a window and a
// count, with no per-record addresses, because listing three hundred sequence
// numbers is not a citation, it is a copy.
type EvidenceRef struct {
	From, To time.Time
	Episode  string
	Seqs     []uint64
	N        int
}

// ErrPruned reports that the evidence an address points at is gone.
//
// This is the FADE, and it is the correct behaviour rather than a failure: the
// stream has a retention policy and the claim outlives it. What must never
// happen is the claim quietly losing its provenance — a page that cannot say
// "the records behind me were deleted on the 22nd" is asserting something it
// can no longer support, which is the absence-of-evidence failure with a
// citation stapled to it.
var ErrPruned = errors.New("execlog: evidence pruned")

// String renders the address.
func (r EvidenceRef) String() string {
	if r.From.IsZero() {
		return ""
	}
	var b strings.Builder
	b.WriteString("exec://")
	b.WriteString(r.From.UTC().Format("2006-01-02"))
	if !r.To.IsZero() && !sameDay(r.From, r.To) {
		b.WriteString("..")
		b.WriteString(r.To.UTC().Format("2006-01-02"))
	}
	if r.Episode != "" {
		b.WriteString("/")
		b.WriteString(r.Episode)
	}
	switch {
	case len(r.Seqs) > 0 && len(r.Seqs) <= maxCitedSeqs:
		b.WriteString("#seq=")
		parts := make([]string, len(r.Seqs))
		for i, s := range r.Seqs {
			parts[i] = strconv.FormatUint(s, 10)
		}
		b.WriteString(strings.Join(parts, ","))
	case r.N > 0:
		fmt.Fprintf(&b, "#n=%d", r.N)
	}
	return b.String()
}

// maxCitedSeqs bounds how many records an address names individually. Past
// this, a window and a count is the honest citation; an exhaustive list is a
// copy of the corpus wearing a pointer's clothes.
const maxCitedSeqs = 12

// ParseEvidence reads an address back.
func ParseEvidence(s string) (EvidenceRef, error) {
	var r EvidenceRef
	rest, ok := strings.CutPrefix(strings.TrimSpace(s), "exec://")
	if !ok {
		return r, fmt.Errorf("execlog: not an evidence ref: %q", s)
	}
	if head, frag, hasFrag := strings.Cut(rest, "#"); hasFrag {
		rest = head
		switch {
		case strings.HasPrefix(frag, "seq="):
			for _, p := range strings.Split(strings.TrimPrefix(frag, "seq="), ",") {
				n, err := strconv.ParseUint(strings.TrimSpace(p), 10, 64)
				if err != nil {
					return r, fmt.Errorf("execlog: bad seq in %q", s)
				}
				r.Seqs = append(r.Seqs, n)
			}
		case strings.HasPrefix(frag, "n="):
			n, err := strconv.Atoi(strings.TrimPrefix(frag, "n="))
			if err != nil {
				return r, fmt.Errorf("execlog: bad count in %q", s)
			}
			r.N = n
		}
	}
	days, episode, _ := strings.Cut(rest, "/")
	r.Episode = episode

	from, to, hasRange := strings.Cut(days, "..")
	f, err := time.Parse("2006-01-02", from)
	if err != nil {
		return r, fmt.Errorf("execlog: bad date in %q", s)
	}
	r.From = f
	r.To = f
	if hasRange {
		t, err := time.Parse("2006-01-02", to)
		if err != nil {
			return r, fmt.Errorf("execlog: bad end date in %q", s)
		}
		r.To = t
	}
	return r, nil
}

// Resolve fetches the records an address points at.
//
// It returns ErrPruned when the window is gone — wrapped, so a caller can tell
// "the evidence was deleted" from "the store is broken". A claim whose evidence
// has faded is still a claim; it has simply stopped being auditable, and the
// reader is entitled to know which.
func (r EvidenceRef) Resolve(root string) ([]Record, Coverage, error) {
	recs, cov, err := Read(root, Query{Episode: r.Episode})
	if err != nil {
		return nil, cov, err
	}

	want := map[uint64]bool{}
	for _, s := range r.Seqs {
		want[s] = true
	}

	var out []Record
	for _, rec := range recs {
		if !inWindow(rec.At, r.From, r.To) {
			continue
		}
		if len(want) > 0 && !want[rec.Seq] {
			continue
		}
		out = append(out, rec)
	}
	if len(out) == 0 {
		return nil, cov, fmt.Errorf("%w: %s (window %s..%s)",
			ErrPruned, r.String(),
			r.From.Format("2006-01-02"), r.To.Format("2006-01-02"))
	}
	return out, cov, nil
}

// EvidenceFor builds the address for a set of records.
func EvidenceFor(recs []Record) EvidenceRef {
	var r EvidenceRef
	r.N = len(recs)
	episodes := map[string]bool{}
	for _, rec := range recs {
		if r.From.IsZero() || rec.At.Before(r.From) {
			r.From = rec.At
		}
		if rec.At.After(r.To) {
			r.To = rec.At
		}
		episodes[rec.Episode] = true
		r.Seqs = append(r.Seqs, rec.Seq)
	}
	// An episode is named only when there is exactly one; naming the first of
	// several would make the address resolve to a strict subset of what it
	// claims to cite.
	if len(episodes) == 1 {
		for e := range episodes {
			r.Episode = e
		}
	}
	sort.Slice(r.Seqs, func(i, j int) bool { return r.Seqs[i] < r.Seqs[j] })
	if len(r.Seqs) > maxCitedSeqs {
		r.Seqs = nil
	}
	return r
}

func inWindow(t, from, to time.Time) bool {
	if from.IsZero() {
		return true
	}
	day := t.UTC().Truncate(24 * time.Hour)
	return !day.Before(from.UTC().Truncate(24*time.Hour)) &&
		!day.After(to.UTC().Truncate(24*time.Hour))
}

func sameDay(a, b time.Time) bool {
	return a.UTC().Format("2006-01-02") == b.UTC().Format("2006-01-02")
}
