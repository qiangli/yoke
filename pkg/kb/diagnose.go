package kb

// Why an empty answer explains itself.
//
// Search returns bare nil when nothing clears the bar, and the caller learns
// only that the store had nothing to say. That silence is expensive and it is
// measured: on queries that mean the right thing in the wrong words, four
// independent lexical rankers score 0% — below the 13% chance floor — while
// returning the whole corpus scores 100%. The information is present; the
// RANKING is what fails. A ranker that fails silently spends the caller's next
// move on a guess.
//
// So the empty path returns a report instead: which query terms no page uses,
// which appear only in bodies rather than on any page's routing surface, and a
// sample of the vocabulary this corpus actually speaks. The caller — an agent
// with world knowledge the index does not have — reformulates from that.
//
// This is deliberately NOT a synonym engine. The measured finding is that
// synonymy needs external knowledge and is not a tuning problem: a character
// trigram scores exactly the chance floor on retrieval, buying morphology
// (resolve/resolution) and nothing on meaning (stall/wait share no characters).
// So trigrams do the did-you-mean half only, and the reader supplies meaning.
// The alternative on the table was vendoring a ~30 MB static embedding table;
// this costs no bytes, no daemon, and no calibration constant.
//
// Diagnose never ranks and Search never calls it. The ranker is measured and
// stays untouched.

import (
	"fmt"
	"sort"
	"strings"
)

const (
	// DefaultVocabCap bounds the vocabulary sample. The report is a routing
	// aid, not a second payload — a dump of every term would cost more than
	// the answer it is standing in for.
	DefaultVocabCap = 24

	// vocabMinUseful is the point below which a filtered vocabulary sample is
	// too thin to reformulate from, and an unfiltered one is worth more.
	vocabMinUseful = 8

	// nearCap and nearMinDice bound the did-you-mean list. Trigram overlap is
	// a spelling and morphology signal, so a near-miss is worth three
	// suggestions at most; below the threshold the suggestions are noise and
	// the vocabulary sample is the better guide.
	//
	// Both constants are measured, not chosen, and BOTH are required — either
	// alone admits a false suggestion. Manufacturing a synonym is worse than
	// offering none, because the reader cannot tell which it got.
	//
	//	                        dice   prefix
	//	container/containers    0.933    9     morphology
	//	conform/conformance     0.714    7     morphology
	//	test/testing            0.571    4     morphology
	//	config/configuration    0.533    6     morphology
	//	retry/retries           0.500    4     morphology
	//	resolve/resolution      0.462    5     morphology
	//	------------------------------------
	//	test/testament          0.444    4     caught by dice
	//	wait/trait              0.400    0     coincidence
	//	hang/range              0.400    0     coincidence
	//	stall/start             0.333    3     coincidence
	//	cache/catch             0.000    2     coincidence
	//	stalled/install         0.600    0     caught by prefix ONLY
	//
	// stalled/install is why the prefix rule exists: both contain "stall", so
	// trigram overlap alone rates them 0.600 — higher than every genuine pair
	// below container/containers. They share a substring and nothing else. A
	// real morphological variant extends a shared stem, so it shares a long
	// prefix; a coincidence does not.
	nearCap     = 3
	nearMinDice = 0.45
	// nearMinPrefix is capped by the shorter word, so a 3-rune stem like
	// "log" can still reach "logs".
	nearMinPrefix = 4
)

// TermReport is what the corpus knows about one query term.
type TermReport struct {
	Term string
	// Pages is document frequency over the eligible set, all fields.
	Pages int
	// CuePages is document frequency restricted to the routing surface
	// (title, description, tags). A term with Pages > 0 and CuePages == 0
	// appears only in bodies, which is why it ranks weakly: field weighting
	// gives a body occurrence 1/4 the weight of a title one.
	CuePages int
	// Near holds trigram-nearest cue-vocabulary terms, populated only when
	// Pages == 0. Spelling and morphology, never synonymy.
	Near []string
}

// Report explains a search against the set that was actually ranked.
type Report struct {
	// Total is every page handed to the query; Eligible is what survived the
	// superseded, scope, tag and retirement filters. Total > Eligible means a
	// filter removed pages, which is a different failure from a bad query.
	Total    int
	Eligible int
	// Terms is one entry per usable query term, in query order. Empty when the
	// query tokenized to nothing — every word a stopword or shorter than three
	// runes, which is its own diagnosis.
	Terms []TermReport
	// Vocab samples the corpus's own routing-surface vocabulary, highest
	// document frequency first.
	Vocab []string
}

// Matched reports whether every usable query term is carried by some page.
func (r Report) Matched() bool {
	if len(r.Terms) == 0 {
		return false
	}
	for _, t := range r.Terms {
		if t.Pages == 0 {
			return false
		}
	}
	return true
}

// Diagnose explains what the corpus does and does not carry for q. It applies
// the same eligibility filters as Search, so the report describes the set that
// was ranked rather than the store as a whole. It does not score, sort or
// return pages.
func Diagnose(pages []*Page, q Query) Report {
	elig := eligible(pages, q)
	rep := Report{Total: len(pages), Eligible: len(elig)}

	// Distinct token sets per page: the full bag for df, the routing surface
	// for cue df. pageTokens repeats tokens to express field weight, which is
	// right for BM25 and wrong for counting documents, so both are deduped.
	full := make([]map[string]bool, len(elig))
	cue := make([]map[string]bool, len(elig))
	cueDF := map[string]int{}
	for i, p := range elig {
		full[i] = distinct(pageTokens(p))
		cue[i] = distinct(cueTokens(p))
		for tok := range cue[i] {
			cueDF[tok]++
		}
	}

	for _, t := range queryTerms(q.Terms) {
		tr := TermReport{Term: t}
		for i := range elig {
			if full[i][t] {
				tr.Pages++
			}
			if cue[i][t] {
				tr.CuePages++
			}
		}
		if tr.Pages == 0 {
			tr.Near = nearest(t, cueDF)
		}
		rep.Terms = append(rep.Terms, tr)
	}

	rep.Vocab = vocabulary(elig, DefaultVocabCap)
	return rep
}

// vocabulary samples what this corpus is ABOUT, from the most deliberate
// source that carries enough terms.
//
// Ranking the whole routing surface by document frequency does not work, and
// the reason is worth keeping: df ranks BOILERPLATE first. Measured on a real
// 36-page store, the top of an all-fields list was "bashy, keywords, agent,
// run, conformance, coreutils, outpost, posix, auto, current, map, never,
// weave, work, art, cited, cross, dated, distilled, doing, generated" — the
// second entry is a literal field label, and art/cited/dated/distilled/doing/
// generated are one shared description template appearing on five pages. A
// reader reformulating toward "generated" has been actively misled.
//
// Tags are curated topical labels rather than prose, so the same store yields
// "bashy, conformance, posix, research, sota, cloudbox, coreutils, outpost,
// orchestration, release, shell, weave, agent, binmgr, ..." — every entry a
// real subject. Titles are the fallback for untagged corpora, and the full cue
// surface the last resort, because a noisy list still beats none.
func vocabulary(elig []*Page, limit int) []string {
	tags := func(p *Page) []string { return tokenize(strings.Join(p.Tags, " ")) }
	titleAndTags := func(p *Page) []string {
		return append(tokenize(p.Title), tags(p)...)
	}
	for _, src := range []func(*Page) []string{tags, titleAndTags, cueTokens} {
		if got := topTerms(docFreq(elig, src), limit, 2); len(got) >= vocabMinUseful {
			return got
		}
	}
	// Too small or too diverse for the df floor: a df=1 list beats nothing.
	return topTerms(docFreq(elig, cueTokens), limit, 1)
}

func docFreq(pages []*Page, tokens func(*Page) []string) map[string]int {
	df := map[string]int{}
	for _, p := range pages {
		for tok := range distinct(tokens(p)) {
			df[tok]++
		}
	}
	return df
}

// Text renders the report for a human or an agent reading a terminal. It stays
// terse on purpose: it is spent instead of an answer, not alongside one.
func (r Report) Text() string {
	var b strings.Builder
	if r.Eligible == r.Total {
		fmt.Fprintf(&b, "kb: %s searched", plural(r.Total, "page"))
	} else {
		fmt.Fprintf(&b, "kb: %d of %s in scope", r.Eligible, plural(r.Total, "page"))
	}
	if r.Total > 0 && r.Eligible == 0 {
		b.WriteString(" — every page was filtered out by scope, tags or status\n")
		return b.String()
	}
	b.WriteString("\n")

	if len(r.Terms) == 0 {
		b.WriteString("  no usable query terms — every word was a stopword or shorter than 3 characters\n")
		return b.String()
	}

	missing := false
	for _, t := range r.Terms {
		switch {
		case t.Pages == 0:
			missing = true
			fmt.Fprintf(&b, "  %q: no page uses it", t.Term)
			if len(t.Near) > 0 {
				fmt.Fprintf(&b, " (near: %s)", strings.Join(t.Near, ", "))
			}
			b.WriteString("\n")
		case t.CuePages == 0:
			fmt.Fprintf(&b, "  %q: %s, none in a title/description/tag\n", t.Term, plural(t.Pages, "page"))
		default:
			fmt.Fprintf(&b, "  %q: %s (%d in a title/description/tag)\n", t.Term, plural(t.Pages, "page"), t.CuePages)
		}
	}

	// The vocabulary sample earns its tokens only when a term missed. When
	// every term landed, the ranking is the problem and a word list is noise.
	if missing && len(r.Vocab) > 0 {
		fmt.Fprintf(&b, "  this kb talks about: %s\n", strings.Join(r.Vocab, ", "))
	}
	return b.String()
}

// cueTokens is the routing surface only — the fields a page is meant to be
// found by. Deliberately excludes the body: a term buried in prose is why a
// page ranks weakly, and collapsing the two would hide that.
func cueTokens(p *Page) []string {
	var out []string
	out = append(out, tokenize(p.Title)...)
	out = append(out, tokenize(p.Description)...)
	out = append(out, tokenize(strings.Join(p.Tags, " "))...)
	out = append(out, tokenize(strings.ReplaceAll(p.Slug, "-", " "))...)
	return out
}

func distinct(toks []string) map[string]bool {
	set := make(map[string]bool, len(toks))
	for _, t := range toks {
		set[t] = true
	}
	return set
}

// topTerms returns the highest-df terms carried by at least minDF pages, ties
// broken alphabetically so the sample is stable across runs.
func topTerms(df map[string]int, limit, minDF int) []string {
	terms := make([]string, 0, len(df))
	for t, n := range df {
		if n >= minDF {
			terms = append(terms, t)
		}
	}
	sort.Slice(terms, func(i, j int) bool {
		if df[terms[i]] != df[terms[j]] {
			return df[terms[i]] > df[terms[j]]
		}
		return terms[i] < terms[j]
	})
	if len(terms) > limit {
		terms = terms[:limit]
	}
	return terms
}

// nearest returns the cue terms most similar to term by trigram overlap.
func nearest(term string, df map[string]int) []string {
	a := trigrams(term)
	if len(a) == 0 {
		return nil
	}
	type scored struct {
		term string
		dice float64
	}
	var out []scored
	for cand := range df {
		if cand == term {
			continue
		}
		// Both gates, always. See nearMinDice/nearMinPrefix.
		if !sharesStem(term, cand) {
			continue
		}
		d := dice(a, trigrams(cand))
		if d >= nearMinDice {
			out = append(out, scored{cand, d})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].dice != out[j].dice {
			return out[i].dice > out[j].dice
		}
		return out[i].term < out[j].term
	})
	if len(out) > nearCap {
		out = out[:nearCap]
	}
	terms := make([]string, len(out))
	for i, s := range out {
		terms[i] = s.term
	}
	return terms
}

// sharesStem reports whether a and b share a long enough common prefix to be
// morphological variants rather than words that merely overlap. The bar is
// nearMinPrefix runes, capped by the shorter word so short stems still reach
// their plurals.
func sharesStem(a, b string) bool {
	ra, rb := []rune(a), []rune(b)
	need := nearMinPrefix
	if n := min(len(ra), len(rb)); n < need {
		need = n
	}
	if need == 0 {
		return false
	}
	for i := range need {
		if ra[i] != rb[i] {
			return false
		}
	}
	return true
}

func trigrams(s string) map[string]bool {
	r := []rune(s)
	set := map[string]bool{}
	if len(r) < 3 {
		if len(r) > 0 {
			set[string(r)] = true
		}
		return set
	}
	for i := 0; i+3 <= len(r); i++ {
		set[string(r[i:i+3])] = true
	}
	return set
}

// dice is the Sørensen–Dice coefficient: 2|A∩B| / (|A|+|B|).
func dice(a, b map[string]bool) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	shared := 0
	for t := range a {
		if b[t] {
			shared++
		}
	}
	return 2 * float64(shared) / float64(len(a)+len(b))
}

func plural(n int, noun string) string {
	if n == 1 {
		return fmt.Sprintf("1 %s", noun)
	}
	return fmt.Sprintf("%d %ss", n, noun)
}
