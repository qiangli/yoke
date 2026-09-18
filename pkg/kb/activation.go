package kb

import (
	"bufio"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Use history — the data a memory needs in order to decay.
//
// Nothing in this store previously recorded that a page had been USED. Pages
// carry provenance (who wrote it, when, from what evidence) and a status
// ladder, but no use count and no last-used time, so there was no way to rank
// by recency or frequency, no way to retire what nobody reads, and no way to
// measure whether the store gets better or worse as it grows. That last one is
// the reason this exists: the growth axis (average accuracy, backward transfer
// i.e. forgetting, forward transfer) is a task×time measurement, and it cannot
// be computed at all without a time series of use.
//
// Two design decisions, both load-bearing:
//
//  1. **Use history lives in an append-only LOG, never in the page.** Writing a
//     counter into a page on every read would rewrite the file on a read path,
//     churn the git history of the committed repo-scoped store, and turn a
//     lock-free reader into a writer. It would also put a clock-derived value
//     inside the record, which is the trap the skill graph's KeyProbes ratchet
//     already exists to prevent. Activation is DERIVED, on demand, by replaying
//     reads.jsonl.
//
//  2. **An OPEN is the signal, not an impression.** The obvious thing to record
//     is "this page appeared in a result list", and it is wrong: the ranker
//     would then be scoring its own past output, and every early mistake would
//     compound. What is recorded instead is what the CALLER did: opening a page
//     (`kb show`). That is the reader's judgement, not the ranker's, so the loop
//     is not closed on itself. Mutations were tried as a second use signal and
//     measured to REGRESS retrieval — see UseHistory.

// useRecord is one appended line in reads.jsonl.
type useRecord struct {
	Op      string    `json:"op"` // open
	Slug    string    `json:"slug"`
	Tool    string    `json:"tool,omitempty"`
	Episode string    `json:"episode,omitempty"`
	At      time.Time `json:"at"`
}

func (s *Store) readsPath() string { return filepath.Join(s.dir, "reads.jsonl") }

// RecordOpen appends an open. Best-effort and silent on failure, exactly like
// the journal: reading a page is the operation, the log is the trail, and a
// full disk must never make `kb show` fail.
func (s *Store) RecordOpen(slug string) {
	rec := useRecord{Op: "open", Slug: slug, Tool: ToolID(), Episode: EpisodeID(), At: time.Now().UTC()}
	b, err := json.Marshal(rec)
	if err != nil {
		return
	}
	f, err := os.OpenFile(s.readsPath(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(append(b, '\n'))
}

// Use is the derived use history of one page.
type Use struct {
	N     int         // how many times it was used
	Last  time.Time   // most recent use
	Times []time.Time // every use, oldest first — the input to base-level activation
}

// BaseLevel is ACT-R's base-level activation: B = ln Σ (now − t_k)^(−d).
//
// It is the standard model of how retrievability falls with disuse and rises
// with repetition, and it is arithmetic — no model, no training. d is the decay
// rate; 0.5 is the conventional default. A page used once long ago and a page
// used ten times this week are separated by this term and by nothing else in
// the ranker.
//
// Returns 0 (not −Inf) for an unused page so the term is neutral rather than
// disqualifying: never having been opened is not evidence of irrelevance,
// which is the same absence-of-evidence rule the rest of this stack obeys.
func (u Use) BaseLevel(now time.Time, d float64) float64 {
	if len(u.Times) == 0 {
		return 0
	}
	var sum float64
	for _, t := range u.Times {
		age := now.Sub(t).Hours()
		if age < 1.0/60 { // floor at one minute: t^-d explodes at t→0
			age = 1.0 / 60
		}
		sum += math.Pow(age, -d)
	}
	if sum <= 0 {
		return 0
	}
	return math.Log(sum)
}

// DefaultDecay is ACT-R's conventional d.
const DefaultDecay = 0.5

// UseHistory replays reads.jsonl into per-slug use history. The log is
// append-only and tolerant: a corrupt line is skipped rather than failing the
// read, mirroring the page-list and journal-replay behaviour.
//
// MUTATIONS ARE DELIBERATELY NOT USES, and this is a correction rather than an
// omission. The first implementation counted validate/update/supersede from
// journal.jsonl, on the reasoning that editing a page is a stronger signal of
// relevance than opening it. Measured on the real host store that arm scored
// hit@3 93% / MRR 0.857 against 100% / 0.929 with the term off — a REGRESSION.
// Two reasons, both obvious afterwards:
//
//   - The store's entire mutation history was 17 records across 17 distinct
//     pages, 14 of them `validate`. So the "use" term was very nearly the
//     indicator "has been validated" — which the ranker ALREADY applies as the
//     status ladder's 1.25x. It was double-counting one signal, not adding one.
//   - An edit is not evidence a page was useful. A page corrected three times is
//     a page that was WRONG three times. Opening it is the reader saying it
//     helped; editing it is the reader saying it did not.
//
// So: an open is a use. Nothing else is, until something else is measured to be.
// ObservedFrom is the first moment the reads log recorded anything — the start
// of the period in which "was this page opened?" is an answerable question.
// Zero when nothing has ever been recorded.
//
// This exists because of a measured near-miss. Retirement asks "has this page
// gone unopened for longer than the grace period?", and on a store where opens
// were never recorded the answer is yes for EVERY page: pointing the rule at
// the real 29-page host store retired all of it, taking hit@3 from 100% to 0%.
// The bug is not the rule, it is applying it outside the window where its
// evidence exists. A page created before the log started has no opens BECAUSE
// NOTHING WAS RECORDED, which is absence of evidence, not evidence of disuse —
// the same principle that makes an undated page unretirable and an unused page
// neutral rather than penalised.
func (s *Store) ObservedFrom() time.Time {
	var first time.Time
	f, err := os.Open(s.readsPath())
	if err != nil {
		return first
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var r useRecord
		if json.Unmarshal([]byte(line), &r) != nil || r.At.IsZero() {
			continue
		}
		if first.IsZero() || r.At.Before(first) {
			first = r.At
		}
	}
	return first
}

func (s *Store) UseHistory() map[string]*Use {
	out := map[string]*Use{}
	add := func(slug string, at time.Time) {
		if slug == "" {
			return
		}
		u := out[slug]
		if u == nil {
			u = &Use{}
			out[slug] = u
		}
		u.N++
		u.Times = append(u.Times, at)
		if at.After(u.Last) {
			u.Last = at
		}
	}
	scan := func(path string, fn func(line []byte)) {
		f, err := os.Open(path)
		if err != nil {
			return
		}
		defer f.Close()
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if line != "" {
				fn([]byte(line))
			}
		}
	}
	scan(s.readsPath(), func(line []byte) {
		var r useRecord
		if json.Unmarshal(line, &r) == nil {
			add(r.Slug, r.At)
		}
	})
	return out
}
